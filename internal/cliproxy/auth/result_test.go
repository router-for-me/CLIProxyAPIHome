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

func TestMarkResultUnauthorizedBlocksEveryModelForCurrentToken(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-401-multi-model",
		Index:    "auth-401-multi-model",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "invalid-current-token"},
		ModelStates: map[string]*ModelState{
			"model-a": {Status: StatusActive},
			"model-b": {Status: StatusActive},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:            auth.ID,
		Model:             "model-a",
		AccessTokenSHA256: AccessTokenSHA256(auth),
		Success:           false,
		Error:             &Error{Message: "invalid access token", HTTPStatus: http.StatusUnauthorized},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("execution 401 permanently disabled auth: %#v", updated)
	}
	if !authUnauthorizedCooldownOpen(updated, time.Now()) {
		t.Fatalf("auth-wide unauthorized cooldown is not open: %#v", updated)
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, "model-b", time.Now()); !blocked || reason == blockReasonDisabled || next.IsZero() {
		t.Fatalf("model-b block = %v, %v, %v; want recoverable auth-wide cooldown", blocked, reason, next)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:  auth.ID,
		Model:   "model-b",
		Success: false,
		Error:   &Error{Message: "temporary upstream failure", HTTPStatus: http.StatusInternalServerError},
	})
	updated, _ = manager.GetByID(auth.ID)
	if !authUnauthorizedCooldownOpen(updated, time.Now()) {
		t.Fatalf("later model failure cleared auth-wide unauthorized cooldown: %#v", updated)
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, "model-c", time.Now()); !blocked {
		t.Fatal("model-c remained dispatchable after current-token 401")
	}
}

func TestAuthRefreshBackoffDoesNotBlockModels(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:               "auth-refresh-backoff",
		Provider:         "codex",
		Status:           StatusActive,
		NextRefreshAfter: now.Add(refreshFailureBackoff),
	}

	refreshRetryAt := auth.NextRefreshAfter
	NewManager(nil, nil, nil).applyResultTransition(auth, Result{AuthID: auth.ID, Model: "gpt-5", Success: true}, "gpt-5", now)
	if !auth.NextRefreshAfter.Equal(refreshRetryAt) || !RefreshRetryBackoffOpen(auth, now) {
		t.Fatalf("successful usage shortened refresh backoff: got %v, want %v", auth.NextRefreshAfter, refreshRetryAt)
	}
	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("isAuthBlockedForModel() = %v, %v, %v; want dispatchable", blocked, reason, next)
	}
}
