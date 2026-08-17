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
