package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type markResultBlockingStore struct {
	block       atomic.Bool
	saveCalls   atomic.Int32
	startOnce   sync.Once
	saveStarted chan struct{}
	unblock     chan struct{}
}

func newMarkResultBlockingStore() *markResultBlockingStore {
	return &markResultBlockingStore{
		saveStarted: make(chan struct{}),
		unblock:     make(chan struct{}),
	}
}

func (s *markResultBlockingStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *markResultBlockingStore) Save(ctx context.Context, auth *Auth) (string, error) {
	s.saveCalls.Add(1)
	if s.block.Load() {
		s.startOnce.Do(func() {
			close(s.saveStarted)
		})
		select {
		case <-s.unblock:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if auth == nil {
		return "", nil
	}
	return auth.ID, nil
}

func (s *markResultBlockingStore) Delete(context.Context, string) error {
	return nil
}

func TestMarkResultDoesNotHoldManagerLockWhilePersisting(t *testing.T) {
	store := newMarkResultBlockingStore()
	manager := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "gemini",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	store.block.Store(true)
	done := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID:  "auth-1",
			Model:   "gemini-3.1-pro-preview",
			Success: false,
			Error: &Error{
				Message:    "quota exhausted",
				HTTPStatus: http.StatusTooManyRequests,
			},
		})
		close(done)
	}()

	select {
	case <-store.saveStarted:
	case <-time.After(time.Second):
		close(store.unblock)
		<-done
		t.Fatal("MarkResult() did not reach store Save")
	}

	readDone := make(chan struct{})
	go func() {
		if got, ok := manager.GetByID("auth-1"); !ok || got == nil {
			t.Errorf("GetByID() = %#v, %v; want auth", got, ok)
		}
		close(readDone)
	}()

	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		close(store.unblock)
		<-done
		t.Fatal("GetByID() blocked while MarkResult() was persisting")
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		close(store.unblock)
		<-done
		t.Fatal("MarkResult() blocked while persisting")
	}

	close(store.unblock)
}

func TestMarkResultUnauthorizedUsesRecoverableCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-401",
		Index:    "auth-401",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "expired"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:  auth.ID,
		Model:   "gpt-5",
		Success: false,
		Error: &Error{
			Message:    "expired access token",
			HTTPStatus: http.StatusUnauthorized,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("execution 401 permanently disabled auth: %#v", updated)
	}
	state := updated.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.Status != StatusError {
		t.Fatalf("401 model state = %#v, want recoverable unavailable error", state)
	}
	if state.NextRetryAfter.Before(before.Add(unauthorizedRetryBackoff - time.Second)) {
		t.Fatalf("NextRetryAfter = %v, want about %v after failure", state.NextRetryAfter, unauthorizedRetryBackoff)
	}
	if blocked, reason, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); !blocked || reason == blockReasonDisabled {
		t.Fatalf("isAuthBlockedForModel() = blocked %v reason %v, want non-disabled cooldown", blocked, reason)
	}
}

func TestAuthRefreshBackoffBlocksEveryModel(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:             "auth-refresh-backoff",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  refreshTransientErrorMsg,
		Unavailable:    true,
		NextRetryAfter: now.Add(refreshFailureBackoff),
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked || reason != blockReasonOther || !next.Equal(auth.NextRetryAfter) {
		t.Fatalf("isAuthBlockedForModel() = %v, %v, %v; want true, other, %v", blocked, reason, next, auth.NextRetryAfter)
	}
}

func TestModelCooldownDoesNotBlockUnrelatedModel(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auth-multi-model",
		Provider: "codex",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired"},
			},
		},
	}
	updateAggregatedAvailability(auth, now)

	if blocked, _, _ := isAuthBlockedForModel(auth, "model-b", now); blocked {
		t.Fatal("model-b was blocked by model-a cooldown")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "model-a", now); !blocked {
		t.Fatal("model-a was not blocked by its own cooldown")
	}
}

func TestAuthScopedCooldownStillBlocksModels(t *testing.T) {
	now := time.Now().UTC()
	retryAt := now.Add(30 * time.Minute)
	auth := &Auth{
		ID:             "auth-scope-cooldown",
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: retryAt,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: retryAt},
		ModelStates: map[string]*ModelState{
			"model-a": {Status: StatusActive},
		},
	}

	manager := NewManager(nil, nil, nil)
	manager.applyResultTransition(auth, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "model-a",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "late model failure"},
	}, "model-a", now)

	if blocked, reason, next := isAuthBlockedForModel(auth, "model-b", now); !blocked || reason != blockReasonCooldown || !next.Equal(retryAt) {
		t.Fatalf("auth-scoped cooldown = blocked %v reason %v next %v, want true/cooldown/%v", blocked, reason, next, retryAt)
	}
	if !auth.Quota.Exceeded || !auth.NextRetryAfter.Equal(retryAt) {
		t.Fatalf("model result cleared auth-scoped cooldown: %#v", auth)
	}
}

func TestAuthScopedQuotaRemainsDistinctFromModelQuota(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{ID: "auth-over-model", Provider: "codex", Status: StatusActive}
	manager := NewManager(nil, nil, nil)

	manager.applyResultTransition(auth, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "model-a",
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
	}, "model-a", now)
	manager.applyResultTransition(auth, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "account quota"},
	}, "", now.Add(time.Second))
	accountRetry := auth.NextRetryAfter

	manager.applyResultTransition(auth, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "model-a",
		Success:  true,
	}, "model-a", now.Add(2*time.Second))

	if authCooldownScope(auth) != cooldownScopeAuth {
		t.Fatalf("cooldown scope = %q, want auth", authCooldownScope(auth))
	}
	if blocked, reason, next := isAuthBlockedForModel(auth, "model-b", now.Add(2*time.Second)); !blocked || reason != blockReasonCooldown || !next.Equal(accountRetry) {
		t.Fatalf("auth quota after model success = blocked %v reason %v next %v, want true/cooldown/%v", blocked, reason, next, accountRetry)
	}
}

func TestModelResultsDoNotShortenRefreshBackoff(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	retryAt := now.Add(refreshFailureBackoff)
	auth := &Auth{
		ID:               "auth-refresh-result",
		Index:            "auth-refresh-result",
		Provider:         "codex",
		Status:           StatusError,
		Unavailable:      true,
		NextRefreshAfter: retryAt,
		NextRetryAfter:   retryAt,
		LastError:        &Error{Code: refreshTransientErrorCode, Message: refreshTransientErrorMsg, Retryable: true},
		Metadata:         map[string]any{"access_token": "expired"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "Home refresh unavailable"},
	})
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: auth.Provider, Model: "gpt-5", Success: true})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.LastError == nil || updated.LastError.Code != refreshTransientErrorCode {
		t.Fatalf("refresh error was overwritten: %#v", updated.LastError)
	}
	if !updated.Unavailable || !updated.NextRetryAfter.Equal(retryAt) {
		t.Fatalf("refresh backoff = unavailable %v retry %v, want true/%v", updated.Unavailable, updated.NextRetryAfter, retryAt)
	}
}

func TestAuthScopedResultsDoNotShortenRefreshBackoff(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	retryAt := now.Add(refreshFailureBackoff)
	auth := &Auth{
		ID:               "auth-refresh-result-without-model",
		Index:            "auth-refresh-result-without-model",
		Provider:         "codex",
		Status:           StatusError,
		StatusMessage:    refreshTransientErrorMsg,
		Unavailable:      true,
		NextRefreshAfter: retryAt,
		NextRetryAfter:   retryAt,
		LastError:        &Error{Code: refreshTransientErrorCode, Message: refreshTransientErrorMsg, Retryable: true},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "late upstream failure"},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.LastError == nil || updated.LastError.Code != refreshTransientErrorCode {
		t.Fatalf("refresh error was overwritten: %#v", updated.LastError)
	}
	if !updated.Unavailable || !updated.NextRetryAfter.Equal(retryAt) {
		t.Fatalf("refresh backoff = unavailable %v retry %v, want true/%v", updated.Unavailable, updated.NextRetryAfter, retryAt)
	}
}

func TestUnauthorizedTransitionDoesNotPermanentlySuspendRegistryModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-401-transition", Provider: "codex", Status: StatusActive}
	transition := manager.applyResultTransition(auth, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired access token"},
	}, "gpt-5", time.Now())

	if transition.shouldSuspendModel {
		t.Fatalf("401 transition requested permanent registry suspension: %#v", transition)
	}
}
