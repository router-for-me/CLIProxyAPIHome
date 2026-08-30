package cluster

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func newQuotaTestRepository(t *testing.T) *Repository {
	t.Helper()
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home-test.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite returned error: %v", errOpen)
	}
	t.Cleanup(func() {
		if sqlDB, errDB := db.DB(); errDB == nil {
			if errClose := sqlDB.Close(); errClose != nil {
				t.Logf("close sqlite: %v", errClose)
			}
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate returned error: %v", errMigrate)
	}
	return NewRepository(db)
}

type blockingStateVersionStore struct {
	*RuntimeAdapter
	saveStarted chan *coreauth.Auth
	releaseSave chan struct{}
	saveDone    chan struct{}
	blockOnce   sync.Once
}

func (s *blockingStateVersionStore) SaveWithStateVersion(ctx context.Context, auth *coreauth.Auth) (string, int64, error) {
	blocked := false
	s.blockOnce.Do(func() {
		blocked = true
		s.saveStarted <- auth.Clone()
		<-s.releaseSave
	})
	if blocked {
		defer close(s.saveDone)
	}
	return s.RuntimeAdapter.SaveWithStateVersion(ctx, auth)
}

// newQuotaTestNode builds the manager view a Home node holds after loading the
// cluster index: the adapter as store and a minimal auth without cooldown state.
func newQuotaTestNode(t *testing.T, repo *Repository, authID string) *coreauth.Manager {
	t.Helper()
	adapter := NewRuntimeAdapter(repo, "127.0.0.1")
	manager := coreauth.NewManager(adapter, nil, nil)
	t.Cleanup(manager.Shutdown)
	minimal := &coreauth.Auth{ID: authID, Index: authID, Provider: "codex"}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), minimal); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	return manager
}

func clusterQuotaResult(authID, model string) coreauth.Result {
	return coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &coreauth.Error{
			Message:    "quota",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
	}
}

func TestClusterQuotaBackoffEscalatesOncePerWindowAcrossNodes(t *testing.T) {
	const authID = "auth-cluster-window"
	const model = "gpt-5"
	repo := newQuotaTestRepository(t)
	ctx := context.Background()

	// Seed the shared row with an expired level-5 window so the first failure
	// opens a wide fresh window and the test stays timing-independent.
	expired := time.Now().Add(-time.Minute)
	seed := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusError,
		Metadata: map[string]any{"email": "user@example.com"},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: expired,
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: expired, BackoffLevel: 5},
			},
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth returned error: %v", errUpsert)
	}

	nodeA := newQuotaTestNode(t, repo, authID)
	nodeB := newQuotaTestNode(t, repo, authID)

	// Node A reports the first failure after expiry: the persisted ladder
	// advances from 5 to 6 and opens a 64s window.
	nodeA.MarkResult(ctx, clusterQuotaResult(authID, model))
	afterFirst, firstRecord, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth returned error: %v", errGet)
	}
	firstState := afterFirst.ModelStates[model]
	if firstState == nil || firstState.Quota.BackoffLevel != 6 {
		t.Fatalf("expected persisted BackoffLevel 6 after post-window failure, got %+v", firstState)
	}
	if !firstState.Quota.NextRecoverAt.After(time.Now()) {
		t.Fatalf("expected open persisted window, got %v", firstState.Quota.NextRecoverAt)
	}

	// Node B holds no local cooldown state and reports a concurrent failure.
	// The persisted window is still open, so the ladder must not escalate and
	// the row must not be rewritten.
	nodeB.MarkResult(ctx, clusterQuotaResult(authID, model))
	afterSecond, secondRecord, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth returned error: %v", errGet)
	}
	secondState := afterSecond.ModelStates[model]
	if secondState == nil || secondState.Quota.BackoffLevel != 6 {
		t.Fatalf("expected persisted BackoffLevel to stay 6 for cross-node in-window failure, got %+v", secondState)
	}
	if !secondState.Quota.NextRecoverAt.Equal(firstState.Quota.NextRecoverAt) {
		t.Fatalf("expected shared window to stay %v, got %v", firstState.Quota.NextRecoverAt, secondState.Quota.NextRecoverAt)
	}
	if secondRecord.Version != firstRecord.Version {
		t.Fatalf("expected in-window failure to skip the row write, version went %d -> %d", firstRecord.Version, secondRecord.Version)
	}

	// Node B adopted the shared window into its local scheduler view.
	localB, ok := nodeB.GetByID(authID)
	if !ok || localB == nil || localB.ModelStates[model] == nil {
		t.Fatalf("expected node B to adopt persisted model state")
	}
	if !localB.ModelStates[model].Quota.NextRecoverAt.Equal(firstState.Quota.NextRecoverAt) {
		t.Fatalf("expected node B local window %v, got %v", firstState.Quota.NextRecoverAt, localB.ModelStates[model].Quota.NextRecoverAt)
	}

	// Force the shared window to expire, then a third node escalates exactly
	// one level from the persisted ladder.
	pastWindow := time.Now().Add(-time.Second)
	if _, _, _, errMutate := repo.MutateAuth(ctx, authID, "update", func(auth *coreauth.Auth) bool {
		state := auth.ModelStates[model]
		if state == nil {
			t.Fatalf("expected persisted model state while expiring window")
		}
		state.NextRetryAfter = pastWindow
		state.Quota.NextRecoverAt = pastWindow
		return true
	}); errMutate != nil {
		t.Fatalf("MutateAuth returned error: %v", errMutate)
	}

	nodeC := newQuotaTestNode(t, repo, authID)
	nodeC.MarkResult(ctx, clusterQuotaResult(authID, model))
	afterThird, _, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth returned error: %v", errGet)
	}
	thirdState := afterThird.ModelStates[model]
	if thirdState == nil || thirdState.Quota.BackoffLevel != 7 {
		t.Fatalf("expected persisted BackoffLevel 7 after expiry, got %+v", thirdState)
	}

	// A success on any node clears the shared cooldown for the whole cluster.
	nodeC.MarkResult(ctx, coreauth.Result{AuthID: authID, Provider: "codex", Model: model, Success: true})
	cleared, _, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth returned error: %v", errGet)
	}
	clearedState := cleared.ModelStates[model]
	if clearedState == nil || clearedState.Unavailable || clearedState.Quota.Exceeded || clearedState.Quota.BackoffLevel != 0 {
		t.Fatalf("expected success to clear persisted model state, got %+v", clearedState)
	}
	if cleared.Unavailable || cleared.Status != coreauth.StatusActive {
		t.Fatalf("expected success to clear persisted aggregate state, got status=%v unavailable=%v", cleared.Status, cleared.Unavailable)
	}
}

func TestClusterClearQuotaCooldownPersistsAndPublishesAuthEvent(t *testing.T) {
	const authID = "auth-cluster-clear"
	const clearedModel = "gpt-a"
	const preservedModel = "gpt-b"
	repo := newQuotaTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	clearedAt := now.Add(10 * time.Minute)
	preservedAt := now.Add(20 * time.Minute)
	seed := &coreauth.Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         coreauth.StatusError,
		Unavailable:    true,
		NextRetryAfter: clearedAt,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: clearedAt, BackoffLevel: 2},
		Metadata:       map[string]any{"type": "codex"},
		ModelStates: map[string]*coreauth.ModelState{
			clearedModel: {
				Status:         coreauth.StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: clearedAt,
				LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: clearedAt, BackoffLevel: 2},
			},
			preservedModel: {
				Status:         coreauth.StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: preservedAt,
				LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: preservedAt, BackoffLevel: 4},
			},
		},
	}
	seedRecord, errUpsert := repo.UpsertAuth(ctx, seed, "register")
	if errUpsert != nil {
		t.Fatalf("UpsertAuth returned error: %v", errUpsert)
	}
	manager := newQuotaTestNode(t, repo, authID)

	result, errClear := manager.ClearQuotaCooldown(ctx, authID, clearedModel)
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown returned error: %v", errClear)
	}
	if !result.Cleared || len(result.ClearedModels) != 1 || result.ClearedModels[0] != clearedModel {
		t.Fatalf("ClearQuotaCooldown result = %#v", result)
	}

	persisted, record, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth returned error: %v", errGet)
	}
	if record.Version != seedRecord.Version+1 {
		t.Fatalf("auth version = %d, want %d", record.Version, seedRecord.Version+1)
	}
	if state := persisted.ModelStates[clearedModel]; state == nil || state.Quota.Exceeded || state.Unavailable || !state.NextRetryAfter.IsZero() || state.QuotaResetAt.IsZero() {
		t.Fatalf("cleared model state = %#v", state)
	}
	if state := persisted.ModelStates[preservedModel]; state == nil || !state.Quota.Exceeded || !state.Unavailable {
		t.Fatalf("preserved model state = %#v", state)
	}

	var event ClusterEventRecord
	if errEvent := repo.db.Where("scope = ? AND entity_uuid = ? AND version = ?", "auth", authID, record.Version).Order("id DESC").First(&event).Error; errEvent != nil {
		t.Fatalf("load auth event: %v", errEvent)
	}
	if event.Op != "update" {
		t.Fatalf("event op = %q, want update", event.Op)
	}

	peerAdapter := NewRuntimeAdapter(repo, "127.0.0.2")
	if errApply := peerAdapter.ApplyEvent(ctx, event); errApply != nil {
		t.Fatalf("ApplyEvent returned error: %v", errApply)
	}
	peerAuths := peerAdapter.ListMinimalAuths()
	if len(peerAuths) != 1 || peerAuths[0].ModelStates[clearedModel] == nil || peerAuths[0].ModelStates[clearedModel].Quota.Exceeded || peerAuths[0].ModelStates[clearedModel].QuotaResetAt.IsZero() {
		t.Fatalf("peer auth index = %#v, want cleared model state", peerAuths)
	}
}

func TestClusterAuthIndexCarriesCooldownState(t *testing.T) {
	const authID = "auth-cluster-index"
	const model = "gpt-5"
	repo := newQuotaTestRepository(t)
	ctx := context.Background()

	recover := time.Now().Add(10 * time.Minute).Round(0)
	seed := &coreauth.Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         coreauth.StatusError,
		Unavailable:    true,
		NextRetryAfter: recover,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: recover, BackoffLevel: 4},
		Metadata:       map[string]any{"email": "user@example.com", "disable_cooling": true},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: recover,
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: recover, BackoffLevel: 4},
			},
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth returned error: %v", errUpsert)
	}

	adapter := NewRuntimeAdapter(repo, "127.0.0.1")
	if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
		t.Fatalf("LoadIndex returned error: %v", errLoad)
	}
	minimals := adapter.ListMinimalAuths()
	if len(minimals) != 1 {
		t.Fatalf("expected one minimal auth, got %d", len(minimals))
	}
	minimal := minimals[0]
	if minimal.RuntimeRefreshBlocked {
		t.Fatal("quota cooldown was projected as a credential refresh block")
	}
	if minimal.RuntimeDisableCooling == nil || !*minimal.RuntimeDisableCooling {
		t.Fatal("expected minimal auth to preserve disable-cooling override")
	}
	if !minimal.NextRetryAfter.Equal(recover) {
		t.Fatalf("expected minimal auth NextRetryAfter %v, got %v", recover, minimal.NextRetryAfter)
	}
	if !minimal.Quota.Exceeded || minimal.Quota.BackoffLevel != 4 {
		t.Fatalf("expected minimal auth quota state, got %+v", minimal.Quota)
	}
	state := minimal.ModelStates[model]
	if state == nil || !state.Unavailable || state.Quota.BackoffLevel != 4 {
		t.Fatalf("expected minimal auth model state, got %+v", state)
	}
	if !state.Quota.NextRecoverAt.Equal(recover) {
		t.Fatalf("expected minimal model window %v, got %v", recover, state.Quota.NextRecoverAt)
	}
}

func TestClusterAuthIndexCarriesRefreshBlockWithoutProviderDiagnostic(t *testing.T) {
	const authID = "auth-cluster-refresh-block"
	repo := newQuotaTestRepository(t)
	ctx := context.Background()
	retryAt := time.Now().UTC().Add(5 * time.Minute).Round(0)
	seed := &coreauth.Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "antigravity",
		Status:         coreauth.StatusError,
		StatusMessage:  `antigravity refresh: upstream response contains provider-secret`,
		Unavailable:    true,
		NextRetryAfter: retryAt,
		LastError: &coreauth.Error{
			Code:       "refresh_temporarily_unavailable",
			Message:    `antigravity refresh: upstream response contains provider-secret`,
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
		Metadata: map[string]any{
			"access_token":  "access-secret",
			"refresh_token": "refresh-secret",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	peerAdapter := NewRuntimeAdapter(repo, "127.0.0.2")
	if errLoad := peerAdapter.LoadIndex(ctx); errLoad != nil {
		t.Fatalf("LoadIndex() error = %v", errLoad)
	}
	minimals := peerAdapter.ListMinimalAuths()
	if len(minimals) != 1 || minimals[0] == nil {
		t.Fatalf("ListMinimalAuths() = %#v, want one auth", minimals)
	}
	minimal := minimals[0]
	if !minimal.Unavailable || !minimal.RuntimeRefreshBlocked || !minimal.NextRetryAfter.Equal(retryAt) {
		t.Fatalf("minimal refresh block = %#v, want unavailable until %v", minimal, retryAt)
	}
	if minimal.LastError != nil || minimal.StatusMessage != "" {
		t.Fatalf("minimal projection copied provider diagnostic: status=%q error=%#v", minimal.StatusMessage, minimal.LastError)
	}
	if _, ok := minimal.Metadata["access_token"]; ok {
		t.Fatalf("minimal projection copied provider metadata: %#v", minimal.Metadata)
	}
}

func TestClusterAuthIndexDoesNotProjectUnsupportedRefreshBackoffAsRefreshBlock(t *testing.T) {
	const authID = "auth-cluster-refresh-unsupported"
	const model = "blocked-model"
	repo := newQuotaTestRepository(t)
	ctx := context.Background()
	retryAt := time.Now().UTC().Add(5 * time.Minute).Round(0)
	seed := &coreauth.Auth{
		ID:               authID,
		Index:            authID,
		Provider:         "custom",
		Status:           coreauth.StatusError,
		Unavailable:      true,
		NextRefreshAfter: retryAt,
		NextRetryAfter:   retryAt,
		LastError: &coreauth.Error{
			Code:       "refresh_unsupported",
			Message:    "credential does not support refresh",
			HTTPStatus: http.StatusServiceUnavailable,
		},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: retryAt,
			},
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	peerAdapter := NewRuntimeAdapter(repo, "127.0.0.2")
	if errLoad := peerAdapter.LoadIndex(ctx); errLoad != nil {
		t.Fatalf("LoadIndex() error = %v", errLoad)
	}
	minimals := peerAdapter.ListMinimalAuths()
	if len(minimals) != 1 || minimals[0] == nil {
		t.Fatalf("ListMinimalAuths() = %#v, want one auth", minimals)
	}
	minimal := minimals[0]
	if minimal.RuntimeRefreshBlocked || coreauth.RefreshBlocksDispatch(minimal) {
		t.Fatalf("unsupported refresh backoff was projected as a credential-level block: %#v", minimal)
	}
	if state := minimal.ModelStates[model]; state == nil || !state.Unavailable {
		t.Fatalf("model cooldown was lost from minimal projection: %#v", minimal.ModelStates)
	}
}

func TestClusterAuthIndexProjectsRequestRetryWithoutSecrets(t *testing.T) {
	for _, requestRetry := range []int{0, 2} {
		t.Run(fmt.Sprintf("request-retry-%d", requestRetry), func(t *testing.T) {
			authID := fmt.Sprintf("auth-cluster-request-retry-%d", requestRetry)
			repo := newQuotaTestRepository(t)
			ctx := context.Background()
			seed := &coreauth.Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"access_token":  "must-not-be-projected",
					"request_retry": requestRetry,
				},
			}
			if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
				t.Fatalf("UpsertAuth() error = %v", errUpsert)
			}

			adapter := NewRuntimeAdapter(repo, "127.0.0.1")
			if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
				t.Fatalf("LoadIndex() error = %v", errLoad)
			}
			minimals := adapter.ListMinimalAuths()
			if len(minimals) != 1 || minimals[0] == nil {
				t.Fatalf("ListMinimalAuths() = %#v, want one auth", minimals)
			}
			minimal := minimals[0]
			if got, ok := minimal.RequestRetryOverride(); !ok || got != requestRetry {
				t.Fatalf("minimal request-retry override = (%d, %t), want (%d, true)", got, ok, requestRetry)
			}
			if _, exists := minimal.Metadata["access_token"]; exists {
				t.Fatalf("minimal metadata exposed access_token: %#v", minimal.Metadata)
			}
		})
	}
}

func TestClusterMinimalAuthPreservesDisableCoolingForTransientResults(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{name: "request-timeout", status: http.StatusRequestTimeout},
		{name: "payment-required", status: http.StatusPaymentRequired},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "not-found", status: http.StatusNotFound},
		{name: "internal-server-error", status: http.StatusInternalServerError},
		{name: "bad-gateway", status: http.StatusBadGateway},
		{name: "service-unavailable", status: http.StatusServiceUnavailable},
		{name: "gateway-timeout", status: http.StatusGatewayTimeout},
	}

	for _, tc := range statuses {
		t.Run(tc.name, func(t *testing.T) {
			const authID = "auth-cluster-disable-cooling"
			const model = "gpt-5"
			repo := newQuotaTestRepository(t)
			ctx := context.Background()
			seed := &coreauth.Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"disable_cooling": true},
			}
			if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
				t.Fatalf("UpsertAuth returned error: %v", errUpsert)
			}

			adapter := NewRuntimeAdapter(repo, "127.0.0.1")
			if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
				t.Fatalf("LoadIndex returned error: %v", errLoad)
			}
			minimals := adapter.ListMinimalAuths()
			if len(minimals) != 1 || minimals[0].RuntimeDisableCooling == nil || !*minimals[0].RuntimeDisableCooling {
				t.Fatalf("minimal auth = %#v, want runtime disable-cooling", minimals)
			}

			manager := coreauth.NewManager(adapter, nil, nil)
			t.Cleanup(manager.Shutdown)
			if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), minimals[0]); errRegister != nil {
				t.Fatalf("Register returned error: %v", errRegister)
			}
			manager.MarkResult(ctx, coreauth.Result{
				AuthID:   authID,
				Provider: "codex",
				Model:    model,
				Error: &coreauth.Error{
					Message:    "upstream failure",
					Retryable:  true,
					HTTPStatus: tc.status,
				},
			})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatalf("GetByID(%s) missing auth after MarkResult", authID)
			}
			state := updated.ModelStates[model]
			if state == nil || !state.Unavailable {
				t.Fatalf("model state = %#v, want unavailable failure state", state)
			}
			if !state.NextRetryAfter.IsZero() {
				t.Fatalf("NextRetryAfter = %v, want zero with disable-cooling", state.NextRetryAfter)
			}
		})
	}
}

func TestClusterDisabledCoolingFencesQueuedRequestErrorSnapshots(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusRequestTimeout,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			const authID = "auth-cluster-result-save-fence"
			const model = "gpt-5"
			ctx := context.Background()
			repo := newQuotaTestRepository(t)
			seed := &coreauth.Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{
					"access_token":              "preserved",
					homeConfigModelsMetadataKey: []map[string]any{{"name": model}},
				},
			}
			seedRecord, errUpsert := repo.UpsertAuth(ctx, seed, "register")
			if errUpsert != nil {
				t.Fatalf("UpsertAuth() error = %v", errUpsert)
			}

			adapter := NewRuntimeAdapter(repo, "127.0.0.1")
			if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
				t.Fatalf("LoadIndex() error = %v", errLoad)
			}
			minimals := adapter.ListMinimalAuths()
			if len(minimals) != 1 || minimals[0] == nil {
				t.Fatalf("ListMinimalAuths() = %#v, want one auth", minimals)
			}
			minimal := minimals[0]
			if minimal.Metadata == nil || minimal.Metadata[homeConfigModelsMetadataKey] == nil {
				t.Fatalf("minimal metadata = %#v, want Home model projection", minimal.Metadata)
			}
			if _, exists := minimal.Metadata["access_token"]; exists {
				t.Fatalf("minimal metadata exposed access_token: %#v", minimal.Metadata)
			}

			store := &blockingStateVersionStore{
				RuntimeAdapter: adapter,
				saveStarted:    make(chan *coreauth.Auth, 1),
				releaseSave:    make(chan struct{}),
				saveDone:       make(chan struct{}),
			}
			manager := coreauth.NewManager(store, nil, nil)
			t.Cleanup(manager.Shutdown)
			manager.SetConfig(&appconfig.Config{DisableCooling: false})
			if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), minimal); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			manager.MarkResult(ctx, coreauth.Result{
				AuthID:   authID,
				Provider: "codex",
				Model:    model,
				Error: &coreauth.Error{
					Message:    "upstream request failed",
					Retryable:  true,
					HTTPStatus: statusCode,
				},
			})

			var staleSnapshot *coreauth.Auth
			select {
			case staleSnapshot = <-store.saveStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("queued result snapshot did not reach SaveWithStateVersion")
			}
			released := false
			t.Cleanup(func() {
				if !released {
					close(store.releaseSave)
				}
				select {
				case <-store.saveDone:
				case <-time.After(2 * time.Second):
					t.Error("queued result save did not finish during cleanup")
				}
			})
			staleState := staleSnapshot.ModelStates[model]
			if staleSnapshot.StateVersion != seedRecord.Version || staleState == nil || staleState.NextRetryAfter.IsZero() {
				t.Fatalf("queued snapshot version/state = %d/%#v, want version %d with cooldown", staleSnapshot.StateVersion, staleState, seedRecord.Version)
			}

			manager.SetConfig(&appconfig.Config{DisableCooling: true})
			manager.MarkResult(coreauth.WithSkipPersist(ctx), coreauth.Result{
				AuthID:   authID,
				Provider: "codex",
				Model:    model,
				Error: &coreauth.Error{
					Message:    "upstream request failed after cooling was disabled",
					Retryable:  true,
					HTTPStatus: statusCode,
				},
			})
			localBeforeFence, okLocal := manager.GetByID(authID)
			if !okLocal || localBeforeFence == nil || localBeforeFence.ModelStates[model] == nil || !localBeforeFence.ModelStates[model].NextRetryAfter.IsZero() {
				t.Fatalf("local state before fence = %#v/%v, want a clean disabled-cooling result", localBeforeFence, okLocal)
			}
			if errFence := manager.FenceDisabledCooldownStates(ctx); errFence != nil {
				t.Fatalf("FenceDisabledCooldownStates() error = %v", errFence)
			}
			cleared, clearedRecord, errGet := repo.GetAuth(ctx, authID)
			if errGet != nil {
				t.Fatalf("GetAuth() after clear error = %v", errGet)
			}
			if clearedRecord.Version <= seedRecord.Version {
				t.Fatalf("clear version = %d, want fence newer than queued version %d", clearedRecord.Version, seedRecord.Version)
			}
			if len(cleared.ModelStates) != 0 || cleared.Unavailable || !cleared.NextRetryAfter.IsZero() {
				t.Fatalf("persisted state after clear = %#v, want clean auth", cleared)
			}

			close(store.releaseSave)
			released = true
			select {
			case <-store.saveDone:
			case <-time.After(2 * time.Second):
				t.Fatal("queued result save did not finish after release")
			}

			persisted, persistedRecord, errGet := repo.GetAuth(ctx, authID)
			if errGet != nil {
				t.Fatalf("GetAuth() after stale save error = %v", errGet)
			}
			if persistedRecord.Version != clearedRecord.Version {
				t.Fatalf("stale save changed version %d -> %d", clearedRecord.Version, persistedRecord.Version)
			}
			if len(persisted.ModelStates) != 0 || persisted.Unavailable || !persisted.NextRetryAfter.IsZero() {
				t.Fatalf("stale save restored cooldown: %#v", persisted)
			}
			if got := persisted.Metadata["access_token"]; got != "preserved" {
				t.Fatalf("access_token after stale save = %v, want preserved", got)
			}
			local, ok := manager.GetByID(authID)
			if !ok || local == nil || local.ModelStates[model] == nil || !local.ModelStates[model].NextRetryAfter.IsZero() {
				t.Fatalf("local state after stale save = %#v/%v, want cooldown cleared", local, ok)
			}
		})
	}
}

func TestClusterMinimalAuthUsesFullAuthForQuotaResultWithCoolingOverride(t *testing.T) {
	tests := []struct {
		name              string
		idSuffix          string
		credentialSet     bool
		credentialDisable bool
		globalDisable     bool
		wantCooldown      bool
	}{
		{name: "credential disable override", idSuffix: "credential-disabled", credentialSet: true, credentialDisable: true},
		{name: "credential enable overrides global", idSuffix: "credential-enabled", credentialSet: true, globalDisable: true, wantCooldown: true},
		{name: "global setting", idSuffix: "global", globalDisable: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const model = "gpt-5"
			authID := "auth-cluster-disabled-quota-" + tc.idSuffix
			repo := newQuotaTestRepository(t)
			ctx := context.Background()
			metadata := map[string]any{"access_token": "preserved"}
			if tc.credentialSet {
				metadata["disable_cooling"] = tc.credentialDisable
			}
			seed := &coreauth.Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Metadata: metadata,
			}
			if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
				t.Fatalf("UpsertAuth returned error: %v", errUpsert)
			}

			adapter := NewRuntimeAdapter(repo, "127.0.0.1")
			if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
				t.Fatalf("LoadIndex returned error: %v", errLoad)
			}
			minimals := adapter.ListMinimalAuths()
			if len(minimals) != 1 {
				t.Fatalf("minimal auths = %#v, want one auth", minimals)
			}
			minimal := minimals[0]
			if tc.credentialSet {
				if minimal.RuntimeDisableCooling == nil || *minimal.RuntimeDisableCooling != tc.credentialDisable {
					t.Fatalf("runtime override = %#v, want %t", minimal.RuntimeDisableCooling, tc.credentialDisable)
				}
			} else if minimal.RuntimeDisableCooling != nil {
				t.Fatalf("runtime override = %#v, want nil", minimal.RuntimeDisableCooling)
			}
			manager := coreauth.NewManager(adapter, nil, nil)
			t.Cleanup(manager.Shutdown)
			if tc.globalDisable {
				manager.SetConfig(&appconfig.Config{DisableCooling: true})
			}
			if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), minimal); errRegister != nil {
				t.Fatalf("Register returned error: %v", errRegister)
			}
			manager.MarkResult(ctx, coreauth.Result{
				AuthID:   authID,
				Provider: "codex",
				Model:    model,
				Error: &coreauth.Error{
					Message:    "quota exhausted",
					Retryable:  true,
					HTTPStatus: http.StatusTooManyRequests,
				},
			})

			persisted, _, errGet := repo.GetAuth(ctx, authID)
			if errGet != nil {
				t.Fatalf("GetAuth returned error: %v", errGet)
			}
			if got := persisted.Metadata["access_token"]; got != "preserved" {
				t.Fatalf("access_token = %v, want full auth metadata preserved", got)
			}
			state := persisted.ModelStates[model]
			if tc.wantCooldown {
				if state == nil || !state.Unavailable || !state.Quota.Exceeded || state.NextRetryAfter.IsZero() || state.Quota.NextRecoverAt.IsZero() {
					t.Fatalf("persisted quota state = %#v, want active model cooldown", state)
				}
			} else if state == nil || state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
				t.Fatalf("persisted quota state = %#v, want no active model cooldown", state)
			}
		})
	}
}
