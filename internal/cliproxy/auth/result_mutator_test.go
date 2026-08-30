package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// fakeMutatorStore is a Store with StateMutator support backed by one shared
// auth, simulating the cluster database row shared by multiple Home nodes.
type fakeMutatorStore struct {
	mu        sync.Mutex
	persisted *Auth
	mutateErr error
	mutations int
	attempts  int
	saves     int
}

type testTokenStorage struct{}

func (*testTokenStorage) SaveTokenToFile(string) error { return nil }

func (s *fakeMutatorStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *fakeMutatorStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if auth == nil {
		return "", nil
	}
	return auth.ID, nil
}

func (s *fakeMutatorStore) Delete(context.Context, string) error { return nil }

func (s *fakeMutatorStore) MutateAuthState(_ context.Context, id string, mutate func(auth *Auth) bool) (*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.mutateErr != nil {
		return nil, s.mutateErr
	}
	if s.persisted == nil || s.persisted.ID != id {
		return nil, fmt.Errorf("auth %s not found", id)
	}
	working := s.persisted.Clone()
	if mutate(working) {
		s.mutations++
		s.persisted = working
	}
	return s.persisted.Clone(), nil
}

func (s *fakeMutatorStore) persistedSnapshot() *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persisted.Clone()
}

func (s *fakeMutatorStore) mutationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutations
}

func (s *fakeMutatorStore) mutationAttemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *fakeMutatorStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// newHomeNodeManager registers the minimal auth view a Home node holds after
// loading the cluster index (no metadata, no cooldown state).
func newHomeNodeManager(t *testing.T, store *fakeMutatorStore, authID string) *Manager {
	t.Helper()
	manager := NewManager(store, nil, nil)
	minimal := &Auth{ID: authID, Index: authID, Provider: "codex"}
	if _, errRegister := manager.Register(context.Background(), minimal); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	return manager
}

func TestMarkResultQuotaEscalationIsAtomicAcrossManagers(t *testing.T) {
	const authID = "auth-cluster-quota"
	store := &fakeMutatorStore{
		persisted: &Auth{
			ID:       authID,
			Index:    authID,
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{"email": "user@example.com"},
		},
	}
	nodeA := newHomeNodeManager(t, store, authID)
	nodeB := newHomeNodeManager(t, store, authID)

	// Node A observes the first quota failure and opens the shared window.
	nodeA.MarkResult(context.Background(), quotaResult(authID, "gpt-5"))
	afterFirst := store.persistedSnapshot()
	firstState := afterFirst.ModelStates["gpt-5"]
	if firstState == nil || firstState.Quota.BackoffLevel != 1 {
		t.Fatalf("expected persisted BackoffLevel 1 after first failure, got %+v", firstState)
	}
	if store.mutationCount() != 1 {
		t.Fatalf("expected one persisted mutation, got %d", store.mutationCount())
	}

	// Node B reports a concurrent failure without any local cooldown state.
	// The persisted window is still open, so the ladder must not escalate.
	nodeB.MarkResult(context.Background(), quotaResult(authID, "gpt-5"))
	afterSecond := store.persistedSnapshot()
	secondState := afterSecond.ModelStates["gpt-5"]
	if secondState == nil || secondState.Quota.BackoffLevel != 1 {
		t.Fatalf("expected persisted BackoffLevel to stay 1 for cross-node in-window failure, got %+v", secondState)
	}
	if !secondState.Quota.NextRecoverAt.Equal(firstState.Quota.NextRecoverAt) {
		t.Fatalf("expected shared window to stay %v, got %v", firstState.Quota.NextRecoverAt, secondState.Quota.NextRecoverAt)
	}
	if store.mutationCount() != 1 {
		t.Fatalf("expected in-window failure to skip persistence, got %d mutations", store.mutationCount())
	}

	// Node B adopted the shared window into its local view.
	localB, ok := nodeB.GetByID(authID)
	if !ok || localB == nil || localB.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected node B to adopt persisted model state")
	}
	if !localB.ModelStates["gpt-5"].Quota.NextRecoverAt.Equal(firstState.Quota.NextRecoverAt) {
		t.Fatalf("expected node B local window %v, got %v", firstState.Quota.NextRecoverAt, localB.ModelStates["gpt-5"].Quota.NextRecoverAt)
	}

	// While the local window is open, further failures stay off the store.
	nodeB.MarkResult(context.Background(), quotaResult(authID, "gpt-5"))
	if store.mutationCount() != 1 {
		t.Fatalf("expected locally absorbed failure to skip the store, got %d mutations", store.mutationCount())
	}

	// A success on node B clears the shared state for the whole cluster.
	nodeB.MarkResult(context.Background(), Result{AuthID: authID, Provider: "codex", Model: "gpt-5", Success: true})
	cleared := store.persistedSnapshot()
	clearedState := cleared.ModelStates["gpt-5"]
	if clearedState == nil || clearedState.Unavailable || clearedState.Quota.Exceeded || clearedState.Quota.BackoffLevel != 0 {
		t.Fatalf("expected success to clear persisted model state, got %+v", clearedState)
	}
	if cleared.Status != StatusActive || cleared.Unavailable {
		t.Fatalf("expected success to clear persisted aggregate state, got status=%v unavailable=%v", cleared.Status, cleared.Unavailable)
	}
	if store.mutationCount() != 2 {
		t.Fatalf("expected success clear to persist once, got %d mutations", store.mutationCount())
	}

	if store.saves != 0 {
		t.Fatalf("expected no Save calls on the mutator path, got %d", store.saves)
	}
}

func TestMarkResultAntigravityFullRetryHintsReuseSharedResetWindow(t *testing.T) {
	const authID = "auth-cluster-reset-timestamp"
	const model = "gemini-3.7-flash-high"
	ctx := context.Background()
	seed := &Auth{ID: authID, Index: authID, Provider: "antigravity", Status: StatusActive}
	store := &fakeMutatorStore{persisted: seed.Clone()}
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(ctx), seed.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	retryAfter := 4 * time.Hour
	resetAt := time.Now().UTC().Add(retryAfter).Truncate(time.Millisecond)
	result := quotaResult(authID, model)
	result.Provider = "antigravity"
	result.RetryAfter = &retryAfter
	result.ResetAt = &resetAt
	manager.MarkResult(ctx, result)

	persisted := store.persistedSnapshot()
	state := persisted.ModelStates[model]
	if state == nil || !state.NextRetryAfter.Equal(resetAt) {
		t.Fatalf("persisted Antigravity reset state = %#v, want %v", state, resetAt)
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("Quota.NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want first exponential level 1", state.Quota.BackoffLevel)
	}
	if attempts := store.mutationAttemptCount(); attempts != 1 {
		t.Fatalf("mutation attempts after reset timestamp = %d, want 1", attempts)
	}

	manager.MarkResult(ctx, result)
	manager.flushResultPersistQueue()
	if attempts := store.mutationAttemptCount(); attempts != 1 {
		t.Fatalf("mutation attempts after repeated full retry hints = %d, want 1", attempts)
	}
	if saves := store.saveCount(); saves != 0 {
		t.Fatalf("Save() calls after repeated full retry hints = %d, want 0", saves)
	}
	if repeated := store.persistedSnapshot().ModelStates[model]; repeated == nil || !repeated.NextRetryAfter.Equal(resetAt) {
		t.Fatalf("repeated persisted Antigravity reset state = %#v, want unchanged deadline %v", repeated, resetAt)
	}
	local, ok := manager.GetByID(authID)
	if !ok || local == nil || local.ModelStates[model] == nil || !local.ModelStates[model].NextRetryAfter.Equal(resetAt) {
		t.Fatalf("repeated local Antigravity reset state = %#v, want unchanged deadline %v", local, resetAt)
	}
}

func TestMarkResultAntigravityRetryDelayExtendsOpenSharedWindow(t *testing.T) {
	const authID = "auth-cluster-retry-delay-extend"
	const model = "gemini-3.7-flash-high"
	ctx := context.Background()
	seed := &Auth{ID: authID, Index: authID, Provider: "antigravity", Status: StatusActive}
	store := &fakeMutatorStore{persisted: seed.Clone()}
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(ctx), seed.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	// First 429 has no retry hints -> exponential backoff establishes a short window (1s).
	manager.MarkResult(ctx, Result{
		AuthID:   authID,
		Provider: "antigravity",
		Model:    model,
		Success:  false,
		Error:    &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
	})
	firstSnapshot := store.persistedSnapshot().ModelStates[model]
	if firstSnapshot == nil || !firstSnapshot.Quota.Exceeded {
		t.Fatalf("first 429 failed to establish quota window: %#v", firstSnapshot)
	}

	// Second concurrent 429 arrives within the short window carrying a large retryDelay (2h).
	retryAfter := 2 * time.Hour
	beforeSecond := time.Now()
	manager.MarkResult(ctx, Result{
		AuthID:     authID,
		Provider:   "antigravity",
		Model:      model,
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
	})

	extended := store.persistedSnapshot().ModelStates[model]
	if extended == nil {
		t.Fatalf("extended state is missing")
	}
	if extended.NextRetryAfter.Before(beforeSecond.Add(retryAfter - time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want extended to about 2h after failure", extended.NextRetryAfter)
	}
	if !extended.Quota.NextRecoverAt.Equal(extended.NextRetryAfter) {
		t.Fatalf("Quota.NextRecoverAt = %v, want %v", extended.Quota.NextRecoverAt, extended.NextRetryAfter)
	}
	if extended.Quota.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want backoff level preserved across in-window extension", extended.Quota.BackoffLevel)
	}
	if attempts := store.mutationAttemptCount(); attempts != 2 {
		t.Fatalf("mutation attempts = %d, want 2", attempts)
	}

	// A third 429 with a shorter retryDelay (5s) should NOT shorten the 2h window.
	shortRetry := 5 * time.Second
	manager.MarkResult(ctx, Result{
		AuthID:     authID,
		Provider:   "antigravity",
		Model:      model,
		Success:    false,
		RetryAfter: &shortRetry,
		Error:      &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
	})
	notShortened := store.persistedSnapshot().ModelStates[model]
	if notShortened == nil || !notShortened.NextRetryAfter.Equal(extended.NextRetryAfter) {
		t.Fatalf("NextRetryAfter was shortened to %v, want retained %v", notShortened.NextRetryAfter, extended.NextRetryAfter)
	}
	if attempts := store.mutationAttemptCount(); attempts != 2 {
		t.Fatalf("mutation attempts after shorter hint = %d, want retained 2", attempts)
	}
}

func TestMarkResultDisabledQuotaSkipsRepeatedPersistence(t *testing.T) {
	const authID = "auth-disabled-quota-local-repeat"
	const model = "gpt-5"
	ctx := context.Background()
	seed := &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "preserved"},
	}
	store := &fakeMutatorStore{persisted: seed.Clone()}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&config.Config{DisableCooling: true})
	if _, errRegister := manager.Register(WithSkipPersist(ctx), seed.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(ctx, quotaResult(authID, model))
	if attempts := store.mutationAttemptCount(); attempts != 1 {
		t.Fatalf("mutation attempts after first 429 = %d, want 1", attempts)
	}
	if mutations := store.mutationCount(); mutations != 1 {
		t.Fatalf("changed mutations after first 429 = %d, want 1", mutations)
	}

	manager.MarkResult(ctx, quotaResult(authID, model))
	manager.flushResultPersistQueue()
	if attempts := store.mutationAttemptCount(); attempts != 1 {
		t.Fatalf("mutation attempts after repeated 429 = %d, want 1", attempts)
	}
	if saves := store.saveCount(); saves != 0 {
		t.Fatalf("Save() calls after repeated 429 = %d, want 0", saves)
	}

	local, ok := manager.GetByID(authID)
	if !ok || local == nil || local.ModelStates[model] == nil {
		t.Fatalf("GetByID() missing disabled quota state: %#v", local)
	}
	state := local.ModelStates[model]
	if state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("repeated disabled quota state = %#v, want dispatchable", state)
	}
	if got := store.persistedSnapshot().Metadata["access_token"]; got != "preserved" {
		t.Fatalf("persisted access_token = %v, want preserved", got)
	}
}

func TestResultRoutingAndTransitionUseCapturedDisableCooling(t *testing.T) {
	const model = "gpt-5"
	result := quotaResult("auth-config-snapshot", model)
	now := time.Now().UTC()

	t.Run("captured disabled state survives cooling enable", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		manager.SetConfig(&config.Config{DisableCooling: false})
		auth := &Auth{
			ID:       result.AuthID,
			Index:    result.AuthID,
			Provider: result.Provider,
			Status:   StatusError,
			ModelStates: map[string]*ModelState{
				model: {
					Status: StatusError,
					Quota:  QuotaState{Scope: quotaScopeModel, Reason: "quota"},
				},
			},
		}

		if needsGlobal := manager.resultNeedsGlobalTransition(auth, result, model, now, true); needsGlobal {
			t.Fatal("captured disable-cooling result unexpectedly required a repeated global transition")
		}
		manager.applyResultTransition(auth, result, model, now, true)
		state := auth.ModelStates[model]
		if state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
			t.Fatalf("captured disabled state = %#v, want dispatchable", state)
		}
	})

	t.Run("captured enabled state survives cooling disable", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		manager.SetConfig(&config.Config{DisableCooling: true})
		auth := &Auth{ID: result.AuthID, Index: result.AuthID, Provider: result.Provider, Status: StatusActive}

		if needsGlobal := manager.resultNeedsGlobalTransition(auth, result, model, now, false); !needsGlobal {
			t.Fatal("captured cooling-enabled result skipped its first global transition")
		}
		manager.applyResultTransition(auth, result, model, now, false)
		state := auth.ModelStates[model]
		if state == nil || !state.Unavailable || !state.Quota.Exceeded || !state.NextRetryAfter.After(now) || !state.Quota.NextRecoverAt.After(now) {
			t.Fatalf("captured cooling-enabled state = %#v, want active quota cooldown", state)
		}
	})
}

func TestDisabledQuotaStateAlreadyAppliedAcceptsOtherModelQuota(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		Status: StatusError,
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status: StatusError,
				Quota:  QuotaState{Scope: quotaScopeModel, Reason: "quota"},
			},
			"gpt-5-mini": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					Scope:         quotaScopeModel,
					Reason:        "quota",
					NextRecoverAt: now.Add(time.Minute),
					BackoffLevel:  2,
				},
			},
		},
	}
	auth.Quota = aggregateModelQuota(auth.ModelStates)

	if !disabledQuotaStateAlreadyApplied(auth, "gpt-5") {
		t.Fatal("disabled quota marker was rejected because another model owns the aggregate quota")
	}
	auth.Quota = QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: now.Add(2 * time.Minute), BackoffLevel: 3}
	if disabledQuotaStateAlreadyApplied(auth, "gpt-5") {
		t.Fatal("disabled quota marker accepted a stale aggregate quota")
	}
}

func TestMarkResultMutationFailureDoesNotApplyLocalPenalty(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			const authID = "auth-mutation-failure"
			store := &fakeMutatorStore{
				persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive},
				mutateErr: fmt.Errorf("database temporarily unavailable"),
			}
			manager := newHomeNodeManager(t, store, authID)

			manager.MarkResult(context.Background(), Result{
				AuthID:   authID,
				Provider: "codex",
				Model:    "gpt-5",
				Error:    &Error{Message: http.StatusText(statusCode), HTTPStatus: statusCode},
			})

			persisted := store.persistedSnapshot()
			if persisted.ModelStates["gpt-5"] != nil || store.mutationCount() != 0 {
				t.Fatalf("persisted state/mutations = %#v/%d, want unchanged", persisted.ModelStates["gpt-5"], store.mutationCount())
			}
			local, ok := manager.GetByID(authID)
			if !ok || local == nil {
				t.Fatal("GetByID() missing auth")
			}
			if state := local.ModelStates["gpt-5"]; state != nil && (state.Quota.Exceeded || state.Unavailable) {
				t.Fatalf("local state = %#v, database failure must not apply cooldown", state)
			}
		})
	}
}

func TestMarkResultUnauthorizedMutatesPersistedStateWithoutReplacingTokens(t *testing.T) {
	const authID = "auth-cluster-unauthorized"
	store := &fakeMutatorStore{
		persisted: &Auth{
			ID:       authID,
			Index:    authID,
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{
				"access_token":  "current-access-token",
				"refresh_token": "current-refresh-token",
			},
		},
	}
	node := newHomeNodeManager(t, store, authID)

	node.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    "gpt-5",
		Success:  false,
		Error:    &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized},
	})

	persisted := store.persistedSnapshot()
	if persisted.Disabled || persisted.Status == StatusDisabled {
		t.Fatalf("execution 401 disabled persisted auth: %#v", persisted)
	}
	if got := persisted.Metadata["access_token"]; got != "current-access-token" {
		t.Fatalf("access_token = %v, want current token preserved", got)
	}
	if got := persisted.Metadata["refresh_token"]; got != "current-refresh-token" {
		t.Fatalf("refresh_token = %v, want current token preserved", got)
	}
	state := persisted.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.Status != StatusError || state.NextRetryAfter.IsZero() {
		t.Fatalf("persisted unauthorized cooldown = %#v", state)
	}
	if store.mutationCount() != 1 || store.saves != 0 {
		t.Fatalf("persistence calls = mutations %d saves %d, want 1/0", store.mutationCount(), store.saves)
	}
}

func TestMarkResultAdoptsNewerPersistedCredentialSnapshot(t *testing.T) {
	const authID = "auth-cluster-newer-credential"
	const resultModel = "gpt-5"
	const localModel = "gpt-local-only"
	now := time.Now().UTC()
	store := &fakeMutatorStore{persisted: &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "codex",
		Label:        "authoritative-label",
		Status:       StatusActive,
		StateVersion: 11,
		Attributes:   map[string]string{"base_url": "https://authoritative.example"},
		Metadata:     map[string]any{"access_token": "rotated-access-token"},
	}}
	manager := NewManager(store, nil, nil)
	runtimeMarker := &struct{ name string }{name: "runtime"}
	storageMarker := &testTokenStorage{}
	localState := &ModelState{
		Status:         StatusError,
		StatusMessage:  "local transient failure",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Minute),
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      now.Add(time.Second),
	}
	local := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "codex",
		Label:        "stale-label",
		Status:       StatusActive,
		StateVersion: 10,
		Attributes:   map[string]string{"base_url": "https://stale.example"},
		Metadata:     map[string]any{"access_token": "stale-access-token"},
		ModelStates:  map[string]*ModelState{localModel: localState},
		Runtime:      runtimeMarker,
		Storage:      storageMarker,
	}
	if _, errRegister := manager.Register(context.Background(), local); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), quotaResult(authID, resultModel))

	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("GetByID() missing auth")
	}
	if current.StateVersion != 11 || current.Metadata["access_token"] != "rotated-access-token" {
		t.Fatalf("credential snapshot = version %d token %v, want version 11 with rotated token", current.StateVersion, current.Metadata["access_token"])
	}
	if current.Label != "authoritative-label" || current.Attributes["base_url"] != "https://authoritative.example" {
		t.Fatalf("credential fields = label %q attributes %#v, want authoritative snapshot", current.Label, current.Attributes)
	}
	if current.Runtime != runtimeMarker || current.Storage != storageMarker {
		t.Fatalf("runtime state = Runtime %#v Storage %#v, want local runtime objects preserved", current.Runtime, current.Storage)
	}
	if state := current.ModelStates[localModel]; state == nil || state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("local model state = %#v, want unpersisted execution state preserved", state)
	}
	if state := current.ModelStates[resultModel]; state == nil || !state.Quota.Exceeded {
		t.Fatalf("result model state = %#v, want persisted quota transition", state)
	}
}

func TestMarkResultIgnoresUnauthorizedFromOlderAccessToken(t *testing.T) {
	const authID = "auth-cluster-stale-unauthorized"
	store := &fakeMutatorStore{persisted: &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "new-access-token"},
	}}
	node := newHomeNodeManager(t, store, authID)
	oldTokenHash := AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "old-access-token"}})

	node.MarkResult(context.Background(), Result{
		AuthID:            authID,
		Provider:          "codex",
		Model:             "gpt-5",
		AccessTokenSHA256: oldTokenHash,
		Success:           false,
		Error:             &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized},
	})

	persisted := store.persistedSnapshot()
	if state := persisted.ModelStates["gpt-5"]; state != nil && state.Unavailable {
		t.Fatalf("late 401 from an older token changed current state: %#v", state)
	}
	local, ok := node.GetByID(authID)
	if !ok || local == nil || local.Metadata["access_token"] != "new-access-token" {
		t.Fatalf("reporting node did not adopt authoritative token: %#v", local)
	}
	if store.mutationCount() != 0 {
		t.Fatalf("late 401 persisted %d mutations, want 0", store.mutationCount())
	}
}

func TestMarkResultQuotaEscalatesFromPersistedLevelAfterExpiry(t *testing.T) {
	const authID = "auth-cluster-expired"
	expired := time.Now().Add(-time.Second)
	store := &fakeMutatorStore{
		persisted: &Auth{
			ID:       authID,
			Index:    authID,
			Provider: "codex",
			Status:   StatusError,
			Metadata: map[string]any{"email": "user@example.com"},
			ModelStates: map[string]*ModelState{
				"gpt-5": {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: expired,
					Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: expired, BackoffLevel: 3},
				},
			},
		},
	}
	// The reporting node has no local cooldown state at all: the persisted
	// ladder must still advance from level 3 to 4, not restart at 1.
	node := newHomeNodeManager(t, store, authID)
	node.MarkResult(context.Background(), quotaResult(authID, "gpt-5"))

	persisted := store.persistedSnapshot()
	state := persisted.ModelStates["gpt-5"]
	if state == nil || state.Quota.BackoffLevel != 4 {
		t.Fatalf("expected persisted BackoffLevel 4 after post-window failure, got %+v", state)
	}
	if !state.Quota.NextRecoverAt.After(time.Now()) {
		t.Fatalf("expected a fresh persisted window, got %v", state.Quota.NextRecoverAt)
	}
}

func TestAdoptPersistedStateKeepsLocalModelError(t *testing.T) {
	const authID = "auth-adopt-local-error"
	store := &fakeMutatorStore{
		persisted: &Auth{
			ID:       authID,
			Index:    authID,
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{"email": "user@example.com"},
		},
	}
	node := newHomeNodeManager(t, store, authID)

	// A 429 on model-b goes through the mutator and opens a shared window.
	node.MarkResult(context.Background(), quotaResult(authID, "model-b"))

	// A transient 500 on model-a stays node-local and is never persisted.
	node.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    "model-a",
		Success:  false,
		Error:    &Error{Message: "upstream exploded", HTTPStatus: http.StatusInternalServerError},
	})
	mid, _ := node.GetByID(authID)
	if mid == nil || mid.Status != StatusError || mid.ModelStates["model-a"] == nil {
		t.Fatalf("expected local error state after 500 on model-a, got %+v", mid)
	}
	if store.persistedSnapshot().ModelStates["model-a"] != nil {
		t.Fatalf("precondition broken: model-a 500 must stay node-local, but reached the row")
	}

	// model-b succeeds; the row clears to active because it never knew about
	// model-a's local error. The adopted state must not flip the local auth
	// to active while that model error remains (the local path keeps
	// StatusError via hasModelError).
	node.MarkResult(context.Background(), Result{AuthID: authID, Provider: "codex", Model: "model-b", Success: true})

	if row := store.persistedSnapshot(); row.Status != StatusActive {
		t.Fatalf("expected persisted row to clear to active, got %q", row.Status)
	}
	local, _ := node.GetByID(authID)
	if local == nil || local.ModelStates["model-a"] == nil || local.ModelStates["model-a"].LastError == nil {
		t.Fatalf("expected model-a local error state to survive, got %+v", local)
	}
	if state := local.ModelStates["model-b"]; state == nil || state.Unavailable || state.Quota.Exceeded {
		t.Fatalf("expected model-b local state to adopt the cleared row state, got %+v", state)
	}
	if local.Status != StatusError {
		t.Fatalf("expected local Status to stay StatusError while model-a error remains, got %q", local.Status)
	}
	if local.LastError == nil || local.LastError.Message != "upstream exploded" {
		t.Fatalf("expected local LastError to keep the model-a failure, got %+v", local.LastError)
	}
}

func TestAdoptPersistedStateClearsRefreshErrorAndRebuildsLocalModelError(t *testing.T) {
	const authID = "auth-clear-refresh-block"
	now := time.Now().UTC()
	retryAt := now.Add(5 * time.Minute)
	manager := NewManager(nil, nil, nil)
	local := &Auth{
		ID:                    authID,
		Index:                 authID,
		Provider:              "codex",
		Status:                StatusError,
		StatusMessage:         refreshTransientErrorMsg,
		Unavailable:           true,
		RuntimeRefreshBlocked: true,
		NextRetryAfter:        retryAt,
		LastError: &Error{
			Code:       refreshTransientErrorCode,
			Message:    refreshTransientErrorMsg,
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				StatusMessage:  "model-a failed",
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      &Error{Message: "local upstream failure", HTTPStatus: http.StatusBadGateway},
				UpdatedAt:      now,
			},
			"model-b": {
				Status:    StatusError,
				LastError: &Error{Message: "stale model-b error", HTTPStatus: http.StatusBadGateway},
			},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), local); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	persisted := &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
	}

	adopted, current := manager.adoptPersistedResultState(Result{AuthID: authID, Model: "model-b"}, "model-b", persisted, now)
	if !current || adopted == nil {
		t.Fatalf("adoptPersistedResultState() = %#v/%v, want current auth", adopted, current)
	}
	if !adopted.Unavailable || adopted.RuntimeRefreshBlocked || RefreshBlocksDispatch(adopted) {
		t.Fatalf("cleared persisted refresh block remained active: %#v", adopted)
	}
	if adopted.Status != StatusError || adopted.StatusMessage != "model-a failed" || adopted.LastError == nil || adopted.LastError.Message != "local upstream failure" {
		t.Fatalf("auth-level model error was not rebuilt from model-a: %#v", adopted)
	}
	if blocked, _, _ := isAuthBlockedForModel(adopted, "model-a", now); !blocked {
		t.Fatal("surviving model-a error was cleared with the credential refresh block")
	}
	if blocked, _, _ := isAuthBlockedForModel(adopted, "model-c", now); blocked {
		t.Fatal("cleared credential refresh block still blocked an unrelated model")
	}
	if state := adopted.ModelStates["model-b"]; state != nil {
		t.Fatalf("cleared persisted model-b state remained local: %#v", state)
	}
}

func TestMarkResultAdoptsPersistedRefreshBlock(t *testing.T) {
	const authID = "auth-adopt-refresh-block"
	const model = "gpt-5"
	now := time.Now().UTC()
	retryAt := now.Add(5 * time.Minute)
	refreshAt := now.Add(3 * time.Minute)
	const providerMessage = `codex refresh: Post "https://auth.openai.com/oauth/token": proxyconnect tcp: connection refused`
	store := &fakeMutatorStore{persisted: &Auth{
		ID:               authID,
		Index:            authID,
		Provider:         "codex",
		Status:           StatusError,
		StatusMessage:    providerMessage,
		Unavailable:      true,
		NextRefreshAfter: refreshAt,
		NextRetryAfter:   retryAt,
		LastError: &Error{
			Code:       refreshTransientErrorCode,
			Message:    providerMessage,
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
		LastRefreshError: &Error{
			Code:       refreshTransientErrorCode,
			Message:    refreshTransientErrorMsg,
			Diagnostic: "refresh proxy unavailable",
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
		Metadata: map[string]any{"access_token": "persisted-access-token"},
	}}
	manager := NewManager(store, nil, nil)
	minimal := &Auth{
		ID:                    authID,
		Index:                 authID,
		Provider:              "codex",
		Status:                StatusError,
		Unavailable:           true,
		RuntimeRefreshBlocked: true,
		NextRetryAfter:        retryAt,
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), minimal); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: "codex", Model: model, Success: true})

	local, ok := manager.GetByID(authID)
	if !ok || local == nil {
		t.Fatal("GetByID() missing auth")
	}
	if local.Status != StatusError || !local.Unavailable || !local.RuntimeRefreshBlocked || !local.NextRetryAfter.Equal(retryAt) {
		t.Fatalf("local refresh block = %#v, want persisted credential-level block", local)
	}
	if local.LastError == nil || local.LastError.Code != refreshTransientErrorCode || local.LastError.Message != providerMessage {
		t.Fatalf("local refresh error = %#v, want persisted provider diagnostic", local.LastError)
	}
	if local.LastRefreshError == nil || local.LastRefreshError.Diagnostic != "refresh proxy unavailable" || !local.NextRefreshAfter.Equal(refreshAt) {
		t.Fatalf("local refresh diagnostic/backoff = %#v/%v, want persisted diagnostic until %v", local.LastRefreshError, local.NextRefreshAfter, refreshAt)
	}
	if blocked, _, _ := isAuthBlockedForModel(local, model, retryAt.Add(time.Hour)); !blocked {
		t.Fatal("late execution success made refresh-failed auth dispatchable")
	}
}

// blockingMutatorStore blocks inside MutateAuthState so tests can assert the
// manager lock is not held during persisted state mutations.
type blockingMutatorStore struct {
	fakeMutatorStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingMutatorStore) MutateAuthState(ctx context.Context, id string, mutate func(auth *Auth) bool) (*Auth, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.fakeMutatorStore.MutateAuthState(ctx, id, mutate)
}

type committedMutatorStore struct {
	fakeMutatorStore
	committed chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *committedMutatorStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persisted != nil && s.persisted.ID == id {
		s.persisted = nil
	}
	return nil
}

func (s *committedMutatorStore) MutateAuthState(ctx context.Context, id string, mutate func(auth *Auth) bool) (*Auth, error) {
	persisted, errMutate := s.fakeMutatorStore.MutateAuthState(ctx, id, mutate)
	s.once.Do(func() { close(s.committed) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return persisted, errMutate
}

func TestMarkResultDoesNotRestoreDeletedAuthToScheduler(t *testing.T) {
	const authID = "auth-deleted-after-result-commit"
	store := &committedMutatorStore{
		fakeMutatorStore: fakeMutatorStore{
			persisted: &Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{"access_token": "stored-token"},
			},
		},
		committed: make(chan struct{}),
		release:   make(chan struct{}),
	}
	manager := newHomeNodeManager(t, &store.fakeMutatorStore, authID)
	manager.SetStore(store)

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.MarkResult(context.Background(), quotaResult(authID, "gpt-5"))
	}()

	select {
	case <-store.committed:
	case <-time.After(2 * time.Second):
		t.Fatal("persisted result mutation did not commit")
	}
	if errDelete := manager.Delete(context.Background(), authID); errDelete != nil {
		t.Fatalf("Delete() error = %v", errDelete)
	}
	close(store.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MarkResult did not finish after mutation release")
	}

	if current, ok := manager.GetByID(authID); ok || current != nil {
		t.Fatalf("GetByID() = %#v, %v, want deleted auth to stay absent", current, ok)
	}
	manager.scheduler.mu.Lock()
	_, scheduled := manager.scheduler.authProviders[authID]
	manager.scheduler.mu.Unlock()
	if scheduled {
		t.Fatal("deleted auth was restored to the scheduler by a late result mutation")
	}
}

func TestMarkResultDoesNotHoldManagerLockDuringStateMutation(t *testing.T) {
	const authID = "auth-cluster-lock"
	store := &blockingMutatorStore{
		fakeMutatorStore: fakeMutatorStore{
			persisted: &Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{"email": "user@example.com"},
			},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := newHomeNodeManager(t, &store.fakeMutatorStore, authID)
	manager.SetStore(store)

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.MarkResult(context.Background(), quotaResult(authID, "gpt-5"))
	}()

	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("MutateAuthState was never invoked")
	}

	lookup := make(chan struct{})
	go func() {
		defer close(lookup)
		manager.GetByID(authID)
	}()
	select {
	case <-lookup:
	case <-time.After(2 * time.Second):
		t.Fatalf("manager lock held while state mutation was in flight")
	}

	close(store.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("MarkResult did not finish after mutation was released")
	}
}

func TestMarkResultReconcilesCoolingDisabledDuringQuotaMutation(t *testing.T) {
	const authID = "auth-cluster-config-race"
	const model = "gpt-5"
	store := &blockingMutatorStore{
		fakeMutatorStore: fakeMutatorStore{
			persisted: &Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{"access_token": "preserved"},
			},
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := newHomeNodeManager(t, &store.fakeMutatorStore, authID)
	manager.SetStore(store)
	manager.SetConfig(&config.Config{DisableCooling: false})

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.MarkResult(context.Background(), quotaResult(authID, model))
	}()

	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota mutation was never invoked")
	}

	manager.SetConfig(&config.Config{DisableCooling: true})
	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("pre-commit ClearDisabledCooldownStates() error = %v", errClear)
	}
	close(store.release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("MarkResult did not finish after mutation release")
	}

	persisted := store.persistedSnapshot()
	state := persisted.ModelStates[model]
	if state == nil || state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("persisted state after config race = %#v, want dispatchable", state)
	}
	if got := persisted.Metadata["access_token"]; got != "preserved" {
		t.Fatalf("persisted access_token = %v, want preserved", got)
	}
	local, ok := manager.GetByID(authID)
	if !ok || local == nil || local.ModelStates[model] == nil || local.ModelStates[model].Unavailable || local.ModelStates[model].Quota.Exceeded {
		t.Fatalf("local state after config race = %#v, want dispatchable", local)
	}
	if attempts := store.mutationAttemptCount(); attempts != 2 {
		t.Fatalf("mutation attempts = %d, want quota transition plus compensating clear", attempts)
	}
}
