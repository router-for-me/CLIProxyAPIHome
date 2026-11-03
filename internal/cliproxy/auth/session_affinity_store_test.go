package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestSessionAffinitySharedStoreAcrossSelectors(t *testing.T) {
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	nodeA := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store})
	nodeB := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store})
	auths := []*Auth{{ID: "a", Status: StatusActive}, {ID: "b", Status: StatusActive}}
	steps := []struct {
		selector *SessionAffinitySelector
		session  string
		parent   string
		want     string
	}{
		{nodeB, "warmup", "", "a"},
		{nodeA, "parent", "", "a"},
		{nodeB, "parent", "", "a"},
		{nodeB, "child", "parent", "a"},
		{nodeA, "child", "", "a"},
	}
	for _, step := range steps {
		opts := Options{Headers: http.Header{"X-Session-ID": {step.session}, "X-Parent-Session-ID": {step.parent}}}
		picked, errPick := step.selector.Pick(t.Context(), "openai", "gpt", opts, auths)
		if errPick != nil || picked == nil || picked.ID != step.want {
			t.Fatalf("session %s: picked=%v err=%v, want %s", step.session, picked, errPick, step.want)
		}
	}
}

type firstBindingBarrierSelector struct {
	RoundRobinSelector
	arrived chan struct{}
	release chan struct{}
}

func (s *firstBindingBarrierSelector) Pick(ctx context.Context, provider, model string, opts Options, auths []*Auth) (*Auth, error) {
	select {
	case s.arrived <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.RoundRobinSelector.Pick(ctx, provider, model, opts, auths)
}

func TestSessionAffinityConcurrentFirstBinding(t *testing.T) {
	ctx, cancelCtx := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCtx()
	fallback := &firstBindingBarrierSelector{arrived: make(chan struct{}, 2), release: make(chan struct{})}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback})
	t.Cleanup(selector.Stop)
	auths := []*Auth{{ID: "a", Status: StatusActive}, {ID: "b", Status: StatusActive}}
	opts := Options{Headers: http.Header{"X-Session-ID": {"concurrent"}}}
	type pickResult struct {
		auth *Auth
		err  error
	}
	results := make(chan pickResult, 2)
	for range 2 {
		go func() {
			picked, errPick := selector.Pick(ctx, "openai", "gpt", opts, auths)
			results <- pickResult{picked, errPick}
		}()
	}
	for range 2 {
		select {
		case <-fallback.arrived:
		case <-ctx.Done():
			t.Fatal("requests did not reach the first-binding barrier")
		}
	}
	close(fallback.release)
	var expected string
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil || result.auth == nil {
				t.Fatalf("pick=%v err=%v", result.auth, result.err)
			}
			if expected == "" {
				expected = result.auth.ID
			}
			if result.auth.ID != expected {
				t.Fatalf("concurrent requests returned different credentials: %s and %s", expected, result.auth.ID)
			}
		case <-ctx.Done():
			t.Fatal("requests did not finish")
		}
	}
}

func TestSessionAffinityDeferredCommitAndCandidateFiltering(t *testing.T) {
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: &FillFirstSelector{}})
	selection := &SessionAffinitySelection{}
	opts := Options{Headers: http.Header{"X-Session-ID": {"deferred"}}, SessionAffinity: selection}
	authA := &Auth{ID: "a", Status: StatusActive}
	authB := &Auth{ID: "b", Status: StatusActive}
	picked, errPick := selector.Pick(t.Context(), "openai", "gpt", opts, []*Auth{authA, authB})
	if errPick != nil || picked.ID != "a" || selection.Commit == nil {
		t.Fatalf("pick=%v selection=%#v err=%v", picked, selection, errPick)
	}
	binding, errLoad := store.LoadSessionBinding(t.Context(), selection.Commit.Key)
	if errLoad != nil || binding.Revision != "" {
		t.Fatalf("selection wrote before admission: binding=%#v err=%v", binding, errLoad)
	}
	if swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), *selection.Commit); errSwap != nil || !swapped {
		t.Fatalf("commit=%t err=%v", swapped, errSwap)
	}
	// A binding must never bypass the current permission, weight, retry, or
	// availability candidate view supplied by the manager.
	picked, errPick = selector.Pick(t.Context(), "openai", "gpt", opts, []*Auth{authB})
	if errPick != nil || picked.ID != "b" {
		t.Fatalf("filtered pick=%v err=%v, want b", picked, errPick)
	}
	binding, errLoad = store.LoadSessionBinding(t.Context(), selection.Commit.Key)
	if errLoad != nil || binding.AuthID != "a" {
		t.Fatalf("uncommitted failover replaced binding: %#v err=%v", binding, errLoad)
	}
}

func TestSessionBindingKeyIsFramedAndModelScoped(t *testing.T) {
	keys := []string{
		sessionBindingKey("a::b", "c", "d"),
		sessionBindingKey("a", "b::c", "d"),
		sessionBindingKey("a", "b", "c::d"),
		sessionBindingKey("a", "b", "other-model"),
	}
	seen := make(map[string]bool)
	for _, key := range keys {
		if seen[key] || len(key) != 64 {
			t.Fatalf("invalid or colliding key %q", key)
		}
		seen[key] = true
	}
}

type failingSessionBindingStore struct {
	SessionAffinityStore
	err error
}

func (s *failingSessionBindingStore) LoadSessionBinding(context.Context, string) (SessionBinding, error) {
	return SessionBinding{}, s.err
}

func TestSessionAffinityStoreFailureDoesNotFallBackLocally(t *testing.T) {
	failure := errors.New("database unavailable")
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: &failingSessionBindingStore{err: failure}})
	picked, errPick := selector.Pick(t.Context(), "openai", "gpt", Options{Headers: http.Header{"X-Session-ID": {"s"}}}, []*Auth{{ID: "a"}})
	if picked != nil || !errors.Is(errPick, failure) {
		t.Fatalf("pick=%v err=%v, want store failure", picked, errPick)
	}
}

func TestSessionCacheExpiredBindingRejectsStaleClaims(t *testing.T) {
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	key := sessionBindingKey("openai", "expired", "gpt")
	store.Set(key, "a")
	store.mu.Lock()
	entry := store.entries[key]
	entry.expiresAt = time.Now().Add(-time.Hour)
	store.entries[key] = entry
	store.mu.Unlock()
	commit := SessionAffinityCommit{Key: key, ExpectedRevision: entry.revision, ExpiredAt: entry.expiresAt, AuthID: "a", TTL: time.Hour}
	if swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("expired claim=%t err=%v", swapped, errSwap)
	}
	commit.AuthID = "b"
	if swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("stale competing claim=%t err=%v", swapped, errSwap)
	}
	current, errLoad := store.LoadSessionBinding(t.Context(), key)
	if errLoad != nil || current.Revision == entry.revision {
		t.Fatalf("expired claim reused revision: %#v err=%v", current, errLoad)
	}
	// An expiration-based claim also loses to a same-revision lease renewal.
	commit.ExpectedRevision = current.Revision
	if swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("stale expiration-based claim=%t err=%v", swapped, errSwap)
	}
}

func TestSessionCacheConditionalBindingSurvivesConcurrentRenewals(t *testing.T) {
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	key := sessionBindingKey("openai", "s", "gpt")
	if swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), SessionAffinityCommit{Key: key, AuthID: "a", TTL: time.Hour}); errSwap != nil || !swapped {
		t.Fatalf("initial commit=%t err=%v", swapped, errSwap)
	}
	binding, errLoad := store.LoadSessionBinding(t.Context(), key)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), SessionAffinityCommit{Key: key, ExpectedRevision: binding.Revision, AuthID: "a", TTL: time.Hour})
			if errSwap != nil || !swapped {
				t.Errorf("concurrent renewal=%t err=%v", swapped, errSwap)
			}
		})
	}
	wait.Wait()
}
