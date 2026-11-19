package cluster

import (
	"context"
	"errors"
	"regexp"
	goruntime "runtime"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestADrainWaitsForAClaimedRefreshToWriteBack(t *testing.T) {
	t.Cleanup(func() { refreshDraining.Store(false) })
	const authID = "antigravity-draining"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	transport := &blockingRefreshRoundTripper{started: make(chan struct{}), release: make(chan struct{}), secondCall: make(chan struct{}, 1)}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9313, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	result := make(chan error, 1)
	go func() {
		_, errRefresh := controller.RefreshNow(ctx, authID)
		result <- errRefresh
	}()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider refresh did not start")
	}
	if got := RefreshesInFlight(); got != 1 {
		t.Fatalf("RefreshesInFlight() = %d, want 1", got)
	}
	drained := make(chan bool, 1)
	go func() { drained <- DrainRefreshes(5 * time.Second) }()
	select {
	case <-drained:
		t.Fatal("drain returned while a claimed refresh was still out")
	case <-time.After(100 * time.Millisecond):
	}
	close(transport.release)
	if !<-drained {
		t.Fatal("drain timed out after the refresh finished")
	}
	if errRefresh := <-result; errRefresh != nil {
		t.Fatalf("RefreshNow() error = %v", errRefresh)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if got := persisted.Metadata["refresh_token"]; got != "new-refresh-token" {
		t.Fatalf("persisted refresh token = %v, want the rotated one written back before the drain ended", got)
	}
	if got := RefreshesInFlight(); got != 0 {
		t.Fatalf("RefreshesInFlight() = %d after the drain, want 0", got)
	}
}

func TestADrainingHomeStartsNoNewRefresh(t *testing.T) {
	t.Cleanup(func() { refreshDraining.Store(false) })
	const authID = "antigravity-refused"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	transport := &refreshTestRoundTripper{statusCode: 200, body: `{"access_token":"a","refresh_token":"b","expires_in":3600}`}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9314, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	if !DrainRefreshes(time.Second) {
		t.Fatal("drain with nothing in flight timed out")
	}
	_, errRefresh := controller.RefreshNow(ctx, authID)
	if !errors.Is(errRefresh, errRefreshDraining) || !errors.Is(errRefresh, context.Canceled) {
		t.Fatalf("RefreshNow() while draining = %v, want a refusal that reads as cancelled", errRefresh)
	}
	if transport.calls != 0 {
		t.Fatalf("provider refresh calls = %d while draining, want 0", transport.calls)
	}
}

func TestADrainGivesUpAtItsBound(t *testing.T) {
	t.Cleanup(func() { refreshDraining.Store(false) })
	done := holdRefresh()
	if DrainRefreshes(50 * time.Millisecond) {
		t.Fatal("drain reported done with a refresh still held")
	}
	done()
	done()
	if !DrainRefreshes(time.Second) || RefreshesInFlight() != 0 {
		t.Fatalf("drain after release: in flight %d, want 0", RefreshesInFlight())
	}
}

func TestRefreshRejectionsMapOntoClosedReasonsAndReferences(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New(`{"error":"refresh_token_reused","error_description":"Your refresh token has already been used"}`), "refresh_token_reused"},
		{&coreauth.Error{Code: "authentication_error", Message: "invalid_grant: token revoked"}, "invalid_grant"},
		{errors.New("token was REUSED by another client"), "refresh_token_reused"},
		{errors.New("401 person@example.com"), "unauthorized"},
		{nil, "unauthorized"},
	} {
		if got := terminalRefreshReason(tc.err); got != tc.want {
			t.Errorf("terminalRefreshReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	accountRef := regexp.MustCompile(`^(?:[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}|sha256:[a-f0-9]{64})$`)
	const uuid = "11111111-2222-4333-8444-555555555555"
	if got := credentialReference(uuid); got != uuid {
		t.Errorf("credentialReference(uuid) = %q, want it unchanged", got)
	}
	for _, id := range []string{"claude-person@example.com.json", "", " 11111111-2222-4333-8444-555555555555x"} {
		if got := credentialReference(id); !accountRef.MatchString(got) || got == id {
			t.Errorf("credentialReference(%q) = %q, want an opaque reference", id, got)
		}
	}
}

func TestARefreshWaitingOnItsLockStartsNoProviderCallOnceADrainBegan(t *testing.T) {
	t.Cleanup(func() { refreshDraining.Store(false) })
	const authID = "antigravity-lock-waiter"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	transport := &blockingRefreshRoundTripper{started: make(chan struct{}), release: make(chan struct{}), secondCall: make(chan struct{}, 1)}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9315, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	first := make(chan error, 1)
	go func() {
		_, errRefresh := controller.RefreshNow(ctx, authID)
		first <- errRefresh
	}()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider refresh did not start")
	}
	second := make(chan error, 1)
	go func() {
		_, errRefresh := controller.RefreshNow(ctx, authID)
		second <- errRefresh
	}()
	// The second refresh is counted (so it passed the entry check) and now waits on the lock.
	for deadline := time.Now().Add(5 * time.Second); RefreshesInFlight() < 2; {
		if time.Now().After(deadline) {
			t.Fatal("second refresh never started")
		}
		goruntime.Gosched()
	}
	drained := make(chan bool, 1)
	go func() { drained <- DrainRefreshes(5 * time.Second) }()
	for !refreshDraining.Load() {
		goruntime.Gosched()
	}
	close(transport.release)
	if errRefresh := <-first; errRefresh != nil {
		t.Fatalf("first RefreshNow() error = %v", errRefresh)
	}
	if errRefresh := <-second; !errors.Is(errRefresh, errRefreshDraining) {
		t.Fatalf("second RefreshNow() after the drain began = %v, want the draining refusal", errRefresh)
	}
	if !<-drained {
		t.Fatal("drain timed out")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("provider refresh calls = %d, want 1: the lock waiter started a call during the drain", got)
	}
}
