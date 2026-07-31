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
