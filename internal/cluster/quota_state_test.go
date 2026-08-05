package cluster

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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

// newQuotaTestNode builds the manager view a Home node holds after loading the
// cluster index: the adapter as store and a minimal auth without cooldown state.
func newQuotaTestNode(t *testing.T, repo *Repository, authID string) *coreauth.Manager {
	t.Helper()
	adapter := NewRuntimeAdapter(repo, "127.0.0.1")
	manager := coreauth.NewManager(adapter, nil, nil)
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
		Metadata:       map[string]any{"email": "user@example.com"},
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
