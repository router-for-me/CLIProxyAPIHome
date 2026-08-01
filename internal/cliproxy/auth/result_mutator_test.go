package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// fakeMutatorStore is a Store with StateMutator support backed by one shared
// auth, simulating the cluster database row shared by multiple Home nodes.
type fakeMutatorStore struct {
	mu           sync.Mutex
	persisted    *Auth
	mutations    int
	saves        int
	beforeReturn func()
	returnLatest bool
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
	if s.persisted == nil || s.persisted.ID != id {
		s.mu.Unlock()
		return nil, fmt.Errorf("auth %s not found", id)
	}
	working := s.persisted.Clone()
	if mutate(working) {
		s.mutations++
		if working.StateVersion > 0 {
			working.StateVersion++
		}
		s.persisted = working
	}
	result := s.persisted.Clone()
	beforeReturn := s.beforeReturn
	s.beforeReturn = nil
	s.mu.Unlock()
	if beforeReturn != nil {
		beforeReturn()
	}
	if s.returnLatest {
		s.mu.Lock()
		result = s.persisted.Clone()
		s.mu.Unlock()
	}
	return result, nil
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

func TestMarkResultIgnoresSuccessFromOlderAccessToken(t *testing.T) {
	for _, model := range []string{"", "gpt-5"} {
		name := "auth-scoped"
		if model != "" {
			name = "model-scoped"
		}
		t.Run(name, func(t *testing.T) {
			const authID = "auth-cluster-stale-success"
			current := &Auth{
				ID:       authID,
				Index:    authID,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{"access_token": "new-access-token"},
			}
			if model != "" {
				current.ModelStates = map[string]*ModelState{
					model:     {Status: StatusActive},
					"model-b": {Status: StatusActive},
				}
			}
			store := &fakeMutatorStore{persisted: current}
			node := newHomeNodeManager(t, store, authID)
			currentHash := AccessTokenSHA256(current)

			node.MarkResult(context.Background(), Result{
				AuthID:            authID,
				Provider:          "codex",
				Model:             model,
				AccessTokenSHA256: currentHash,
				Success:           false,
				Error:             &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized},
			})
			if store.mutationCount() != 1 {
				t.Fatalf("current-token 401 mutations = %d, want 1", store.mutationCount())
			}
			beforeSuccess := store.persistedSnapshot()
			if !authUnauthorizedCooldownOpen(beforeSuccess, time.Now()) {
				t.Fatalf("current-token 401 did not open auth cooldown: %#v", beforeSuccess)
			}

			oldHash := AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "old-access-token"}})
			node.MarkResult(context.Background(), Result{
				AuthID:            authID,
				Provider:          "codex",
				Model:             model,
				AccessTokenSHA256: oldHash,
				Success:           true,
			})

			afterSuccess := store.persistedSnapshot()
			if !authUnauthorizedCooldownOpen(afterSuccess, time.Now()) {
				t.Fatalf("older-token success cleared persisted cooldown: %#v", afterSuccess)
			}
			if store.mutationCount() != 1 {
				t.Fatalf("older-token success persisted %d mutations, want 1 total", store.mutationCount())
			}
			local, ok := node.GetByID(authID)
			if !ok || local == nil || !authUnauthorizedCooldownOpen(local, time.Now()) {
				t.Fatalf("older-token success cleared local cooldown: %#v", local)
			}
			if blocked, _, _ := isAuthBlockedForModel(local, "model-b", time.Now()); !blocked {
				t.Fatal("older-token success made current credential dispatchable")
			}
		})
	}
}

func TestMarkResultDoesNotPersistTokenVersionedSuccessFromCleanStaleNode(t *testing.T) {
	const authID = "auth-cluster-clean-stale-success"
	now := time.Now()
	store := &fakeMutatorStore{persisted: &Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "unauthorized",
		Unavailable:    true,
		LastError:      &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized},
		NextRetryAfter: now.Add(unauthorizedRetryBackoff),
		Metadata:       map[string]any{"access_token": "new-access-token"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				StatusMessage:  "expired access token",
				Unavailable:    true,
				LastError:      &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized},
				NextRetryAfter: now.Add(unauthorizedRetryBackoff),
			},
		},
	}}
	manager := NewManager(store, nil, nil)
	local := &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "old-access-token"},
	}
	if _, errRegister := manager.Register(context.Background(), local); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	savesBefore := store.saves

	manager.MarkResult(context.Background(), Result{
		AuthID:            authID,
		Provider:          "codex",
		Model:             "gpt-5",
		AccessTokenSHA256: AccessTokenSHA256(local),
		Success:           true,
	})

	persisted := store.persistedSnapshot()
	if !authUnauthorizedCooldownOpen(persisted, time.Now()) {
		t.Fatalf("clean stale node cleared authoritative cooldown: %#v", persisted)
	}
	if got := persisted.Metadata["access_token"]; got != "new-access-token" {
		t.Fatalf("authoritative access token = %v, want new-access-token", got)
	}
	if store.mutationCount() != 0 || store.saves != savesBefore {
		t.Fatalf("clean stale success persistence = mutations %d saves %d, want 0/%d", store.mutationCount(), store.saves, savesBefore)
	}
	localAfter, _ := manager.GetByID(authID)
	if localAfter == nil || len(localAfter.ModelStates) != 0 {
		t.Fatalf("clean stale success changed local availability state: %#v", localAfter)
	}
}

func TestMarkResultDoesNotAdoptOlderMutationSnapshot(t *testing.T) {
	const authID = "auth-cluster-out-of-order-adoption"
	now := time.Now()
	initial := &Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Minute),
		StateVersion:   1,
		Metadata:       map[string]any{"access_token": "same-access-token"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{Message: "temporary failure", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
	store := &fakeMutatorStore{persisted: initial.Clone()}
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(context.Background(), initial.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	store.mu.Lock()
	store.beforeReturn = func() {
		newer := initial.Clone()
		newer.StateVersion = 3
		newer.Disabled = true
		newer.Status = StatusDisabled
		newer.StatusMessage = "disabled by operator"
		store.mu.Lock()
		store.persisted = newer.Clone()
		store.mu.Unlock()
		manager.mu.Lock()
		manager.auths[authID] = newer
		manager.indexAuth[authID] = newer
		manager.mu.Unlock()
	}
	store.mu.Unlock()

	manager.MarkResult(context.Background(), Result{
		AuthID:            authID,
		Provider:          "codex",
		Model:             "gpt-5",
		AccessTokenSHA256: AccessTokenSHA256(initial),
		Success:           true,
	})

	local, ok := manager.GetByID(authID)
	if !ok || local == nil {
		t.Fatal("local auth not found")
	}
	if local.StateVersion != 3 || !local.Disabled || local.Status != StatusDisabled {
		t.Fatalf("older mutation snapshot replaced newer local state: %#v", local)
	}
	if state := local.ModelStates["gpt-5"]; state == nil || !state.Unavailable {
		t.Fatalf("older success cleared newer model state: %#v", state)
	}
}

func TestMarkResultSuppressesEffectsFromSupersededMutation(t *testing.T) {
	const (
		authID  = "auth-cluster-superseded-effects"
		modelID = "model-superseded-effects"
	)
	now := time.Now()
	initial := &Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Minute),
		StateVersion:   1,
		Metadata:       map[string]any{"access_token": "same-access-token"},
		ModelStates: map[string]*ModelState{
			modelID: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{Message: "temporary failure", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
	store := &fakeMutatorStore{persisted: initial.Clone(), returnLatest: true}
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(context.Background(), initial.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID}})
	modelRegistry.SuspendClientModel(authID, modelID, "temporary_failure")
	t.Cleanup(func() { modelRegistry.RegisterClient(authID, "codex", nil) })

	store.mu.Lock()
	store.beforeReturn = func() {
		newer := initial.Clone()
		newer.StateVersion = 3
		newer.LastError = &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized}
		newer.StatusMessage = "expired access token"
		newer.ModelStates[modelID].LastError = cloneError(newer.LastError)
		newer.ModelStates[modelID].StatusMessage = newer.LastError.Message
		store.mu.Lock()
		store.persisted = newer
		store.mu.Unlock()
	}
	store.mu.Unlock()

	manager.MarkResult(context.Background(), Result{
		AuthID:            authID,
		Provider:          "codex",
		Model:             modelID,
		AccessTokenSHA256: AccessTokenSHA256(initial),
		Success:           true,
	})

	local, ok := manager.GetByID(authID)
	if !ok || local == nil || local.StateVersion != 3 {
		t.Fatalf("manager did not adopt newest snapshot: %#v", local)
	}
	if state := local.ModelStates[modelID]; state == nil || !state.Unavailable || state.LastError == nil || state.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("newest model cooldown was not retained: %#v", state)
	}
	for _, model := range modelRegistry.GetAvailableModelDefinitions() {
		if model != nil && model.ID == modelID {
			t.Fatal("superseded success resumed the model registry")
		}
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
