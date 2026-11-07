package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCodexForwardedUnknownRefreshBlocksWithoutRejectingGrant(t *testing.T) {
	repo := newRefreshTestRepository(t)
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "previous", "refresh_token": "previous-refresh"},
	}
	runtime := newRefreshTestRuntime(t, repo, auth, &refreshTestRoundTripper{})
	controller := NewRefreshController(nil, runtime, repo, nil)
	errApply := controller.applyForwardedRefreshFailureInMemory(context.Background(), auth.ID, &coreauth.Error{
		Code: coreauth.ErrorCodeRefreshOutcomeUnknown, HTTPStatus: http.StatusServiceUnavailable,
	})
	if errApply != nil {
		t.Fatal(errApply)
	}
	current, ok := runtime.CoreManager().GetByID(auth.ID)
	if !ok || current.Disabled || !current.RuntimeRefreshBlocked || current.LastRefreshError == nil ||
		current.LastRefreshError.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || current.LastRefreshError.Retryable {
		t.Fatal("forwarded unknown outcome was lost or mislabeled as rejection")
	}
}

func TestCodexUnknownRefreshBackoffPreservesRecoveryError(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "previous", "refresh_token": t.Name()},
	}
	outcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "response_read", io.ErrUnexpectedEOF)
	coreauth.ApplyRefreshFailureState(auth, outcome, time.Now())
	auth.NextRefreshAfter = time.Now().Add(time.Hour)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	var calls atomic.Int32
	transport := refreshRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected refresh during backoff")
	})
	originalTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	controller := NewRefreshController(nil, runtime, repo, nil)
	_, errRefresh := controller.refreshLocalWithLock(ctx, auth.ID, "")
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || authErr.Retryable || calls.Load() != 0 {
		t.Fatalf("backoff lost the unknown outcome: err=%v calls=%d", errRefresh, calls.Load())
	}
}
