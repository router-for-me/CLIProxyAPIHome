package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
)

func TestCodexUnknownRefreshOutcomeIsPersistableAndNotTerminal(t *testing.T) {
	auth := &Auth{ID: t.Name(), Provider: "codex", Status: StatusActive, Metadata: map[string]any{
		"access_token": "working-access", "refresh_token": t.Name(), "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	errOutcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "transport", context.DeadlineExceeded)
	errRefresh := ApplyRefreshFailureState(auth, builtInRefreshError("codex", "provider_refresh", errOutcome), time.Now())
	if errRefresh.Code != ErrorCodeRefreshOutcomeUnknown || errRefresh.Retryable || auth.Disabled {
		t.Fatalf("unknown outcome became a rejected grant: %#v", errRefresh)
	}
	if codexauth.RefreshOutcomeUnknown(auth.Metadata, false) == nil || auth.Metadata["access_token"] != "working-access" {
		t.Fatal("unknown outcome lost its grant identity or existing access token")
	}
	if !isRefreshAcquisitionError(errRefresh) || isTerminalRefreshAuthError(errOutcome) {
		t.Fatal("unknown outcome has the wrong recovery classification")
	}
}

func TestCodexPersistedUnknownOutcomeStopsRefreshUntilGrantChanges(t *testing.T) {
	var calls atomic.Int32
	originalTransport := http.DefaultTransport
	http.DefaultTransport = refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
			Body: io.NopCloser(strings.NewReader(`{"access_token":"recovered","refresh_token":"next","expires_in":3600}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	auth := &Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{
		"access_token": "previous", "refresh_token": t.Name(),
	}}
	codexauth.RecordRefreshOutcomeUnknown(auth.Metadata, codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "response_read", io.ErrUnexpectedEOF))
	manager := NewManager(nil, nil, nil)
	_, errRefresh := manager.RefreshAuthCredential(context.Background(), auth.Clone())
	if errRefresh == nil || calls.Load() != 0 {
		t.Fatalf("persisted unknown grant was replayed: err=%v calls=%d", errRefresh, calls.Load())
	}
	auth.Metadata["refresh_token"] = t.Name() + "-replacement"
	updated, errRefresh := manager.RefreshAuthCredential(context.Background(), auth.Clone())
	if errRefresh != nil || calls.Load() != 1 || updated.Metadata["access_token"] != "recovered" {
		t.Fatalf("replacement did not recover: err=%v calls=%d", errRefresh, calls.Load())
	}
}

func TestCodexUnknownOutcomeStateDoesNotInheritCauseRejection(t *testing.T) {
	refusal := &Error{Code: "invalid_grant", Message: "401 unauthorized", HTTPStatus: http.StatusUnauthorized}
	for _, cause := range []error{refusal, errors.Join(refusal, errors.New("transport diagnostic"))} {
		outcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "transport", cause)
		auth := &Auth{Provider: "codex", Status: StatusActive, Metadata: map[string]any{"refresh_token": t.Name()}}
		failure := ApplyRefreshFailureState(auth, builtInRefreshError("codex", "provider_refresh", outcome), time.Now())
		if failure.Code != ErrorCodeRefreshOutcomeUnknown || failure.Retryable || auth.Disabled || failure.Upstream != nil {
			t.Fatalf("unknown outcome lost its recovery classification: %#v", failure)
		}
	}
}

type codexOutcomePersistenceStore struct {
	deadlinePresent bool
	canceledAtSave  bool
	saved           bool
}

func (*codexOutcomePersistenceStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (*codexOutcomePersistenceStore) Delete(context.Context, string) error  { return nil }
func (s *codexOutcomePersistenceStore) Save(ctx context.Context, auth *Auth) (string, error) {
	_, s.deadlinePresent = ctx.Deadline()
	s.canceledAtSave = ctx.Err() != nil
	s.saved = true
	return auth.ID, nil
}

func TestCodexCanceledUnknownOutcomePersistenceRemainsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = refreshTestRoundTripper(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, io.ErrUnexpectedEOF
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	store := &codexOutcomePersistenceStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: StatusActive,
		Metadata: map[string]any{"access_token": "previous", "refresh_token": t.Name()},
	}
	if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	_, errRefresh := manager.RefreshNowObserved(ctx, auth.Index, "")
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != ErrorCodeRefreshOutcomeUnknown {
		t.Fatalf("unknown outcome was lost after cancellation: %v", errRefresh)
	}
	if !store.saved || store.canceledAtSave || !store.deadlinePresent {
		t.Fatalf("result persistence must ignore caller cancellation and retain a deadline: saved=%t canceled=%t deadline=%t", store.saved, store.canceledAtSave, store.deadlinePresent)
	}
}
