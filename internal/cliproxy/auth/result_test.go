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
	if blocked, reason, next := isAuthBlockedForModel(updated, "gpt-5-mini", time.Now()); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("unrelated model blocked/reason/next = %v/%v/%v", blocked, reason, next)
	}
}

func TestMarkResultIgnoresMissingCanonicalModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-missing-model", Index: "auth-missing-model", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:  auth.ID,
		Model:   " ",
		Success: false,
		Error:   &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
	})
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Success: true})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Success != 0 || updated.Failed != 0 || updated.Status != StatusActive || updated.Unavailable || updated.Quota.Exceeded || len(updated.ModelStates) != 0 {
		t.Fatalf("model-less result changed auth: %#v", updated)
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
