package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

func markRefreshTestMaster(t *testing.T, repo *Repository, coordinator *Coordinator) {
	t.Helper()
	if repo == nil || coordinator == nil {
		t.Fatal("refresh test master is unavailable")
	}
	now := time.Now().UTC()
	if errCreate := repo.db.Create(&ClusterNodeRecord{
		IP:         coordinator.node.IP,
		Port:       coordinator.node.Port,
		IsMaster:   true,
		StartedAt:  coordinator.node.StartedAt,
		LastSeenAt: now,
	}).Error; errCreate != nil {
		t.Fatalf("create refresh test master: %v", errCreate)
	}
	coordinator.setMaster(true)
}

func TestNewRefreshControllerStoresForwardTLSConfig(t *testing.T) {
	t.Parallel()

	tlsConfig := &tls.Config{}
	controller := NewRefreshController(nil, nil, nil, tlsConfig)
	if controller.forwardTLSConfig != tlsConfig {
		t.Fatal("NewRefreshController did not store forward TLS config")
	}
}

func TestForwardRefreshToMasterRequiresTLSConfig(t *testing.T) {
	t.Parallel()

	_, errForward := ForwardRefreshToMaster(context.Background(), &ClusterNodeRecord{IP: "127.0.0.1", Port: 8327}, "auth-id", "secret", nil)
	if errForward == nil {
		t.Fatal("ForwardRefreshToMaster() error = nil, want TLS config error")
	}
	if !strings.Contains(errForward.Error(), "tls config is required") {
		t.Fatalf("ForwardRefreshToMaster() error = %v, want TLS config error", errForward)
	}
}

type refreshTestRoundTripper struct {
	body       string
	statusCode int
	calls      int
}

func (rt *refreshTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	statusCode := rt.statusCode
	if statusCode == 0 {
		statusCode = http.StatusBadRequest
	}
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(rt.body)),
	}, nil
}

type refreshRoundTripperFunc func(*http.Request) (*http.Response, error)

func (fn refreshRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type blockingRefreshRoundTripper struct {
	calls      atomic.Int32
	started    chan struct{}
	release    chan struct{}
	secondCall chan struct{}
}

func (rt *blockingRefreshRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	call := rt.calls.Add(1)
	if call == 1 {
		close(rt.started)
		<-rt.release
	} else {
		select {
		case rt.secondCall <- struct{}{}:
		default:
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access-token","refresh_token":"new-refresh-token","expires_in":3600}`)),
	}, nil
}

type refreshTestRoundTripperProvider struct {
	rt http.RoundTripper
}

func (p refreshTestRoundTripperProvider) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return p.rt
}

type refreshTestFailingIndexAdapter struct {
	*RuntimeAdapter
}

func (a *refreshTestFailingIndexAdapter) RefreshAuthIndex(context.Context, string) error {
	return errors.New("injected refresh index failure")
}

func newRefreshTestRepository(t *testing.T) *Repository {
	t.Helper()
	return openRefreshTestRepositoryAt(t, filepath.Join(t.TempDir(), "home.db"), true)
}

func openRefreshTestRepositoryAt(t *testing.T, path string, migrate bool) *Repository {
	t.Helper()
	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, path)
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if migrate {
		if errMigrate := AutoMigrate(db); errMigrate != nil {
			t.Fatalf("AutoMigrate() error = %v", errMigrate)
		}
	}
	return NewRepository(db)
}

func newRefreshTestRuntime(t *testing.T, repo *Repository, auth *coreauth.Auth, transport http.RoundTripper) *home.Runtime {
	t.Helper()
	runtime, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	runtime.SetClusterAdapter(NewRuntimeAdapter(repo, ""))
	runtime.CoreManager().SetRoundTripperProvider(refreshTestRoundTripperProvider{rt: transport})
	if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(context.Background()), auth.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	t.Cleanup(runtime.Stop)
	return runtime
}

func newInvalidGrantRefreshAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Index:    id,
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token":  "stored-access-secret",
			"refresh_token": "stored-refresh-secret",
		},
	}
}

func requireTerminalRefreshError(t *testing.T, errRefresh error) {
	t.Helper()
	if errRefresh == nil {
		t.Fatal("RefreshNow() error = nil, want terminal authentication error")
	}
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) {
		t.Fatalf("RefreshNow() error type = %T, want *auth.Error", errRefresh)
	}
	if authErr.Code != "authentication_error" || authErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("RefreshNow() error = %#v, want authentication_error/401", authErr)
	}
	if authErr.Upstream != nil && (authErr.Upstream.Status <= 0 || errRefresh.Error() != string(authErr.Upstream.Body)) {
		t.Fatalf("RefreshNow() upstream response = %#v, error=%q", authErr.Upstream, errRefresh.Error())
	}
}

func requireDisabledRefreshAuth(t *testing.T, auth *coreauth.Auth) {
	t.Helper()
	if auth == nil {
		t.Fatal("auth = nil, want disabled auth")
	}
	if !auth.Disabled || !auth.Unavailable || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("auth state = %#v, want disabled and unavailable", auth)
	}
	if auth.LastError == nil || auth.LastError.Code != "authentication_error" || auth.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("LastError = %#v, want authentication_error/401", auth.LastError)
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero", auth.NextRefreshAfter)
	}
	if auth.LastError.Upstream != nil && (auth.LastError.Upstream.Status <= 0 || auth.LastError.Error() != string(auth.LastError.Upstream.Body)) {
		t.Fatalf("LastError upstream response = %#v, error=%q", auth.LastError.Upstream, auth.LastError.Error())
	}
}

func TestReadRESPBulkRestoresStructuredRefreshError(t *testing.T) {
	t.Parallel()

	_, errRead := readRESPBulk(bufio.NewReader(strings.NewReader("-ERR authentication_error: credential unauthorized\r\n")))
	requireTerminalRefreshError(t, errRead)
}

func TestRESPErrorRoundTripPreservesUpstreamResponseExactly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{name: "json", body: []byte(`{"error":"invalid_request"}`)},
		{name: "text", body: []byte("provider unavailable")},
		{name: "multiline", body: []byte("first line\r\nsecond line\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wireError := &coreauth.Error{
				Code:       "refresh_temporarily_unavailable",
				Message:    "credential refresh temporarily unavailable",
				Retryable:  true,
				HTTPStatus: http.StatusServiceUnavailable,
				Upstream: &coreauth.UpstreamResponse{
					Status: http.StatusBadRequest,
					Body:   tt.body,
				},
			}
			wire := "-" + FormatRESPError(wireError) + "\r\n"
			_, errRead := readRESPBulk(bufio.NewReader(strings.NewReader(wire)))
			var authErr *coreauth.Error
			if !errors.As(errRead, &authErr) || authErr.Upstream == nil {
				t.Fatalf("readRESPBulk() error = %#v, want structured upstream response", errRead)
			}
			if authErr.Code != wireError.Code || authErr.HTTPStatus != wireError.HTTPStatus || authErr.Upstream.Status != http.StatusBadRequest || !bytes.Equal(authErr.Upstream.Body, tt.body) {
				t.Fatalf("round-trip auth error = %#v, want %#v", authErr, wireError)
			}
			if got := authErr.Error(); !bytes.Equal([]byte(got), tt.body) {
				t.Fatalf("round-trip Error() = %q, want %q", got, tt.body)
			}
		})
	}
}

func TestRefreshControllerForwardedAuthFailureFailsClosed(t *testing.T) {
	const authID = "antigravity-fail-closed"
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	runtime := newRefreshTestRuntime(t, repo, auth, &refreshTestRoundTripper{})
	controller := NewRefreshController(nil, runtime, repo, nil)

	errDisable := controller.applyForwardedRefreshFailureInMemory(context.Background(), authID, &coreauth.Error{
		Code:       "authentication_error",
		Message:    "credential unauthorized",
		HTTPStatus: http.StatusUnauthorized,
	})
	if errDisable != nil {
		t.Fatalf("applyForwardedRefreshFailureInMemory() error = %v", errDisable)
	}
	inMemory, ok := runtime.CoreManager().GetByID(authID)
	if !ok {
		t.Fatal("GetByID() did not find auth")
	}
	requireDisabledRefreshAuth(t, inMemory)
}

func TestRefreshControllerMasterFailureFailsClosedWhenIndexSyncFails(t *testing.T) {
	const authID = "antigravity-master-fail-closed"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`}
	runtime, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	adapter := &refreshTestFailingIndexAdapter{RuntimeAdapter: NewRuntimeAdapter(repo, "")}
	runtime.SetClusterAdapter(adapter)
	runtime.CoreManager().SetRoundTripperProvider(refreshTestRoundTripperProvider{rt: transport})
	if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth.Clone()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	t.Cleanup(runtime.Stop)

	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9300, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRefresh)
	inMemory, ok := runtime.CoreManager().GetByID(authID)
	if !ok {
		t.Fatal("GetByID() did not find auth")
	}
	requireDisabledRefreshAuth(t, inMemory)
}

func TestRefreshControllerMasterInvalidGrantPersistsDisabledAuth(t *testing.T) {
	const authID = "antigravity-master"
	const responseBody = `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{body: responseBody}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9301, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRefresh)
	_, errRepeated := controller.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRepeated)
	if errRefresh.Error() != responseBody || errRepeated.Error() != responseBody {
		t.Fatalf("terminal refresh bodies = first %q repeated %q, want %q", errRefresh.Error(), errRepeated.Error(), responseBody)
	}
	if transport.calls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1 after repeated terminal request", transport.calls)
	}

	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	requireDisabledRefreshAuth(t, persisted)
	inMemory, ok := runtime.CoreManager().GetByID(authID)
	if !ok {
		t.Fatal("GetByID() did not find auth")
	}
	requireDisabledRefreshAuth(t, inMemory)
	if runtime.CoreManager().ShouldRefreshCredential(inMemory, time.Now().UTC()) {
		t.Fatal("disabled auth remained eligible for auto-refresh")
	}

	replacement := persisted.Clone()
	replacement.Disabled = false
	replacement.Unavailable = false
	replacement.Status = coreauth.StatusActive
	replacement.StatusMessage = ""
	replacement.LastError = nil
	replacement.Metadata["access_token"] = "replacement-access-secret"
	replacement.Metadata["refresh_token"] = "replacement-refresh-secret"
	if _, errUpsert := repo.UpsertAuth(ctx, replacement, "reauthenticate"); errUpsert != nil {
		t.Fatalf("UpsertAuth(replacement) error = %v", errUpsert)
	}
	if errReload := runtime.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}
	restored, ok := runtime.CoreManager().GetByID(authID)
	if !ok || restored == nil {
		t.Fatal("GetByID() did not find replacement auth")
	}
	if restored.Disabled || restored.Unavailable || restored.Status != coreauth.StatusActive {
		t.Fatalf("replacement auth state = %#v, want active", restored)
	}
}

func TestRefreshControllerTransientFailureBlocksCredentialDispatch(t *testing.T) {
	const authID = "antigravity-transient"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	providerCalls := 0
	const responseBody = `{"error":"invalid_request","error_description":"Malformed request"}`
	transport := refreshRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9302, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNowObserved(ctx, authID, coreauth.AccessTokenSHA256(auth))
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != "refresh_temporarily_unavailable" || authErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("RefreshNowObserved() error = %#v, want transient 503", errRefresh)
	}
	if got := errRefresh.Error(); got != responseBody {
		t.Fatalf("RefreshNowObserved() error = %q, want exact upstream body %q", got, responseBody)
	}
	if authErr.Upstream == nil || authErr.Upstream.Status != http.StatusBadRequest || string(authErr.Upstream.Body) != responseBody {
		t.Fatalf("RefreshNowObserved() upstream = %#v, want 400/%q", authErr.Upstream, responseBody)
	}
	if _, errRepeated := controller.RefreshNowObserved(ctx, authID, coreauth.AccessTokenSHA256(auth)); errRepeated == nil || errRepeated.Error() != errRefresh.Error() {
		t.Fatalf("repeated refresh error = %v, want persisted diagnostic %v", errRepeated, errRefresh)
	}
	if providerCalls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1 during refresh backoff", providerCalls)
	}

	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Disabled || !persisted.Unavailable || persisted.Status != coreauth.StatusError {
		t.Fatalf("transient refresh state = %#v, want unavailable error state", persisted)
	}
	if persisted.NextRefreshAfter.IsZero() || !persisted.NextRetryAfter.Equal(persisted.NextRefreshAfter) {
		t.Fatalf("refresh/retry deadlines = %v/%v, want matching backoff", persisted.NextRefreshAfter, persisted.NextRetryAfter)
	}
	if persisted.LastError == nil || persisted.LastError.Code != "refresh_temporarily_unavailable" {
		t.Fatalf("LastError = %#v, want transient refresh error", persisted.LastError)
	}
	if persisted.LastError.Message != authErr.Message {
		t.Fatalf("persisted refresh error = %q, want %q", persisted.LastError.Message, authErr.Message)
	}
	if persisted.LastError.Upstream == nil || persisted.LastError.Upstream.Status != http.StatusBadRequest || string(persisted.LastError.Upstream.Body) != responseBody {
		t.Fatalf("persisted upstream response = %#v, want 400/%q", persisted.LastError.Upstream, responseBody)
	}
	inMemory, ok := runtime.CoreManager().GetByID(authID)
	if !ok || inMemory == nil || !inMemory.Unavailable || inMemory.Status != coreauth.StatusError {
		t.Fatalf("in-memory refresh state = %#v, want unavailable error state", inMemory)
	}
}

func TestMergeClusterRefreshOutcomeKeepsConcurrentDetailsAndBlocksDispatch(t *testing.T) {
	now := time.Now().UTC()
	base := &coreauth.Auth{ID: "auth-1", Provider: "antigravity", Status: coreauth.StatusActive}
	current := base.Clone()
	current.ModelStates = map[string]*coreauth.ModelState{
		"gemini-3.7-flash-high": {
			Status:        coreauth.StatusError,
			StatusMessage: "upstream unavailable",
			LastError:     &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusBadGateway},
		},
	}
	current.Quota = coreauth.QuotaState{Reason: "concurrent quota detail"}

	retryAt := now.Add(5 * time.Minute)
	refreshed := base.Clone()
	refreshed.Status = coreauth.StatusError
	refreshed.StatusMessage = "credential refresh temporarily unavailable"
	refreshed.Unavailable = true
	refreshed.NextRefreshAfter = retryAt
	refreshed.NextRetryAfter = retryAt
	refreshed.LastError = &coreauth.Error{
		Code:       "refresh_temporarily_unavailable",
		Message:    "credential refresh temporarily unavailable",
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
	refreshed.UpdatedAt = now

	merged := mergeClusterRefreshOutcome(current, base, refreshed, errors.New("provider refresh failed"), now)

	if merged == nil || !merged.Unavailable || merged.Status != coreauth.StatusError || !merged.NextRetryAfter.Equal(retryAt) || merged.LastError == nil || merged.LastError.Code != "refresh_temporarily_unavailable" {
		t.Fatalf("merged refresh state = %#v, want blocked refresh acquisition", merged)
	}
	if merged.ModelStates["gemini-3.7-flash-high"] == nil || merged.ModelStates["gemini-3.7-flash-high"].StatusMessage != "upstream unavailable" {
		t.Fatalf("concurrent model state was lost: %#v", merged.ModelStates)
	}
	if merged.Quota.Reason != "concurrent quota detail" {
		t.Fatalf("concurrent quota state was lost: %#v", merged.Quota)
	}
}

func TestMergeClusterRefreshOutcomeClearsTransientBlockWhenRefreshBecomesUnsupported(t *testing.T) {
	now := time.Now().UTC()
	staleRetryAt := now.Add(-time.Minute)
	base := &coreauth.Auth{
		ID:             "auth-1",
		Provider:       "custom",
		Status:         coreauth.StatusError,
		StatusMessage:  "previous refresh failure",
		Unavailable:    true,
		NextRetryAfter: staleRetryAt,
		LastError: &coreauth.Error{
			Code:       "refresh_temporarily_unavailable",
			Message:    "previous refresh failure",
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
	}

	tests := []struct {
		name          string
		mutateCurrent func(*coreauth.Auth)
		check         func(*testing.T, *coreauth.Auth)
	}{
		{
			name: "clears stale credential block",
			check: func(t *testing.T, merged *coreauth.Auth) {
				t.Helper()
				if merged.Status != coreauth.StatusActive || merged.StatusMessage != "" || merged.Unavailable || !merged.NextRetryAfter.IsZero() {
					t.Fatalf("merged availability = %#v, want active credential", merged)
				}
			},
		},
		{
			name: "preserves concurrent model cooldown",
			mutateCurrent: func(current *coreauth.Auth) {
				modelRetryAt := now.Add(10 * time.Minute)
				current.ModelStates = map[string]*coreauth.ModelState{
					"model-a": {
						Status:         coreauth.StatusError,
						StatusMessage:  "quota exceeded",
						Unavailable:    true,
						NextRetryAfter: modelRetryAt,
						LastError:      &coreauth.Error{Message: "quota exceeded", HTTPStatus: http.StatusTooManyRequests},
						Quota: coreauth.QuotaState{
							Exceeded:      true,
							Scope:         "model",
							Reason:        "quota",
							NextRecoverAt: modelRetryAt,
						},
					},
				}
			},
			check: func(t *testing.T, merged *coreauth.Auth) {
				t.Helper()
				state := merged.ModelStates["model-a"]
				if state == nil || !state.Unavailable || !state.Quota.Exceeded {
					t.Fatalf("concurrent model cooldown was lost: %#v", merged.ModelStates)
				}
				if merged.Status != coreauth.StatusError || !merged.Unavailable || !merged.NextRetryAfter.Equal(state.NextRetryAfter) {
					t.Fatalf("merged model availability = %#v, want model cooldown", merged)
				}
				if !merged.Quota.Exceeded || !merged.Quota.NextRecoverAt.Equal(state.Quota.NextRecoverAt) {
					t.Fatalf("concurrent quota state was lost: %#v", merged.Quota)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := base.Clone()
			if test.mutateCurrent != nil {
				test.mutateCurrent(current)
			}
			refreshed := base.Clone()
			coreauth.ApplyUnsupportedRefreshBackoff(refreshed, now)

			merged := mergeClusterRefreshOutcome(current, base, refreshed, coreauth.ErrRefreshUnsupported, now)

			if merged.LastError == nil || merged.LastError.Code != "refresh_unsupported" || coreauth.RefreshBlocksDispatch(merged) {
				t.Fatalf("merged refresh state = %#v, want unsupported refresh-only backoff", merged)
			}
			if !merged.NextRefreshAfter.Equal(refreshed.NextRefreshAfter) {
				t.Fatalf("NextRefreshAfter = %v, want %v", merged.NextRefreshAfter, refreshed.NextRefreshAfter)
			}
			test.check(t, merged)
		})
	}
}

func TestMergeClusterRefreshOutcomeDoesNotClobberConcurrentStateOnCancellation(t *testing.T) {
	now := time.Now().UTC()
	base := &coreauth.Auth{ID: "auth-1", Provider: "antigravity", Status: coreauth.StatusActive}
	current := base.Clone()
	current.Status = coreauth.StatusError
	current.StatusMessage = "concurrent upstream failure"
	current.Unavailable = true
	current.NextRetryAfter = now.Add(time.Minute)
	current.LastError = &coreauth.Error{Message: "concurrent upstream failure", HTTPStatus: http.StatusBadGateway}
	refreshed := base.Clone()
	refreshed.NextRefreshAfter = now.Add(5 * time.Minute)

	merged := mergeClusterRefreshOutcome(current, base, refreshed, context.Canceled, now)

	if merged.Status != current.Status || merged.StatusMessage != current.StatusMessage || merged.Unavailable != current.Unavailable || !merged.NextRetryAfter.Equal(current.NextRetryAfter) || merged.LastError == nil || merged.LastError.Message != current.LastError.Message {
		t.Fatalf("canceled refresh clobbered concurrent state: got %#v, want %#v", merged, current)
	}
}

func TestWithAuthRefreshLockSerializesSeparateSQLiteConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "shared.db")
	repoA := openRefreshTestRepositoryAt(t, path, true)
	repoB := openRefreshTestRepositoryAt(t, path, false)
	var busyTimeout int
	if errPragma := repoA.db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; errPragma != nil || busyTimeout < 30000 {
		t.Fatalf("SQLite busy_timeout = %d, %v; want at least 30000ms", busyTimeout, errPragma)
	}
	auth := newInvalidGrantRefreshAuth("sqlite-cross-connection")
	if _, errUpsert := repoA.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		_, errLock := repoA.WithAuthRefreshLock(ctx, auth.ID, func(_ *Repository, current *coreauth.Auth) (*coreauth.Auth, error) {
			close(firstEntered)
			<-releaseFirst
			return current, nil
		})
		results <- errLock
	}()
	select {
	case <-firstEntered:
	case <-ctx.Done():
		t.Fatalf("first SQLite refresh lock did not start: %v", ctx.Err())
	}
	go func() {
		_, errLock := repoB.WithAuthRefreshLock(ctx, auth.ID, func(_ *Repository, current *coreauth.Auth) (*coreauth.Auth, error) {
			close(secondEntered)
			return current, nil
		})
		results <- errLock
	}()

	select {
	case <-secondEntered:
		t.Fatal("second SQLite connection entered refresh while the first lock was held")
	case errLock := <-results:
		t.Fatalf("second SQLite refresh lock returned early: %v", errLock)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		select {
		case errLock := <-results:
			if errLock != nil {
				t.Fatalf("WithAuthRefreshLock() error = %v", errLock)
			}
		case <-ctx.Done():
			t.Fatalf("SQLite refresh locks did not finish: %v", ctx.Err())
		}
	}
}

func TestRefreshControllerSerializesConcurrentSQLiteRotations(t *testing.T) {
	const authID = "antigravity-concurrent"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &blockingRefreshRoundTripper{
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		secondCall: make(chan struct{}, 1),
	}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9305, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	observedHash := coreauth.AccessTokenSHA256(auth)

	results := make(chan error, 2)
	go func() {
		_, errRefresh := controller.RefreshNowObserved(ctx, authID, observedHash)
		results <- errRefresh
	}()
	select {
	case <-transport.started:
	case <-ctx.Done():
		t.Fatalf("first provider refresh did not start: %v", ctx.Err())
	}
	go func() {
		_, errRefresh := controller.RefreshNowObserved(ctx, authID, observedHash)
		results <- errRefresh
	}()

	select {
	case <-transport.secondCall:
		t.Fatal("concurrent SQLite refresh reached the provider before the first rotation committed")
	case <-time.After(100 * time.Millisecond):
	}
	close(transport.release)
	for range 2 {
		select {
		case errRefresh := <-results:
			if errRefresh != nil {
				t.Fatalf("RefreshNowObserved() error = %v", errRefresh)
			}
		case <-ctx.Done():
			t.Fatalf("concurrent refresh did not finish: %v", ctx.Err())
		}
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("provider refresh calls = %d, want 1", got)
	}
}

func TestRefreshControllerSkipsRotationAfterObservedTokenChanges(t *testing.T) {
	const authID = "antigravity-observed-token"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{
		statusCode: http.StatusOK,
		body:       `{"access_token":"new-access-token","refresh_token":"new-refresh-token","expires_in":3600,"token_type":"Bearer"}`,
	}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9303, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	observedHash := coreauth.AccessTokenSHA256(auth)

	if _, errRefresh := controller.RefreshNowObserved(ctx, authID, observedHash); errRefresh != nil {
		t.Fatalf("first RefreshNowObserved() error = %v", errRefresh)
	}
	if _, errRefresh := controller.RefreshNowObserved(ctx, authID, observedHash); errRefresh != nil {
		t.Fatalf("second RefreshNowObserved() error = %v", errRefresh)
	}
	if transport.calls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1", transport.calls)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if got := persisted.Metadata["access_token"]; got != "new-access-token" {
		t.Fatalf("persisted access token = %v, want new-access-token", got)
	}
}

func TestRefreshControllerHonorsCallerDeadline(t *testing.T) {
	const authID = "antigravity-deadline"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9304, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	deadlineCtx, cancelDeadline := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelDeadline()
	startedAt := time.Now()
	_, errRefresh := controller.RefreshNowObserved(deadlineCtx, authID, coreauth.AccessTokenSHA256(auth))
	if !errors.Is(errRefresh, context.DeadlineExceeded) {
		t.Fatalf("RefreshNowObserved() error = %v, want context deadline exceeded", errRefresh)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("refresh cancellation took %v, want under 1s", elapsed)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Disabled || persisted.Unavailable || persisted.Status != coreauth.StatusActive {
		t.Fatalf("timed-out refresh blocked credential: %#v", persisted)
	}
}

func TestRefreshControllerStandbySyncsSuccessfulMasterRefresh(t *testing.T) {
	const authID = "antigravity-forwarded-success"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	masterTransport := &refreshTestRoundTripper{
		statusCode: http.StatusOK,
		body:       `{"access_token":"new-access-token","refresh_token":"new-refresh-token","expires_in":3600}`,
	}
	masterRuntime := newRefreshTestRuntime(t, repo, auth, masterTransport)
	standbyRuntime := newRefreshTestRuntime(t, repo, auth, &refreshTestRoundTripper{})
	masterCoordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9403, Secret: "master-secret", StartedAt: time.Now().UTC().Add(-time.Minute)}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, masterCoordinator)
	masterController := NewRefreshController(masterCoordinator, masterRuntime, repo, nil)

	standbyCoordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.2", Port: 9404, Secret: "standby-secret"}, CoordinatorOptions{})
	standbyCoordinator.setMaster(false)
	standbyController := NewRefreshController(standbyCoordinator, standbyRuntime, repo, nil)
	standbyController.forwardRefresh = func(ctx context.Context, _ *ClusterNodeRecord, forwardedAuthID, _ string, observedAccessTokenSHA256 string, _ *tls.Config) ([]byte, error) {
		return masterController.RefreshNowObserved(ctx, forwardedAuthID, observedAccessTokenSHA256)
	}

	if _, errRefresh := standbyController.RefreshNowObserved(ctx, authID, coreauth.AccessTokenSHA256(auth)); errRefresh != nil {
		t.Fatalf("standby RefreshNowObserved() error = %v", errRefresh)
	}
	standbyAuth, ok := standbyRuntime.CoreManager().GetByID(authID)
	if !ok || standbyAuth == nil || standbyAuth.Metadata["access_token"] != "new-access-token" {
		t.Fatalf("standby auth cache = %#v, want refreshed token", standbyAuth)
	}
	if masterTransport.calls != 1 {
		t.Fatalf("master provider refresh calls = %d, want 1", masterTransport.calls)
	}
}

func TestRefreshControllerStandbyPreservesTerminalMasterError(t *testing.T) {
	const authID = "antigravity-forwarded"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	masterTransport := &refreshTestRoundTripper{body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`}
	standbyTransport := &refreshTestRoundTripper{body: masterTransport.body}
	masterRuntime := newRefreshTestRuntime(t, repo, auth, masterTransport)
	standbyRuntime := newRefreshTestRuntime(t, repo, auth, standbyTransport)

	masterIdentity := NodeIdentity{IP: "127.0.0.1", Port: 9401, Secret: "master-secret", StartedAt: time.Now().UTC().Add(-time.Minute)}
	masterCoordinator := NewCoordinator(repo, masterIdentity, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, masterCoordinator)
	masterController := NewRefreshController(masterCoordinator, masterRuntime, repo, nil)

	standbyCoordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.2", Port: 9402, Secret: "standby-secret"}, CoordinatorOptions{})
	standbyCoordinator.setMaster(false)
	standbyController := NewRefreshController(standbyCoordinator, standbyRuntime, repo, nil)
	forwardCalls := 0
	standbyController.forwardRefresh = func(ctx context.Context, _ *ClusterNodeRecord, forwardedAuthID, _ string, observedAccessTokenSHA256 string, _ *tls.Config) ([]byte, error) {
		forwardCalls++
		payload, errMasterRefresh := masterController.RefreshNowObserved(ctx, forwardedAuthID, observedAccessTokenSHA256)
		if errMasterRefresh == nil {
			return payload, nil
		}
		wire := "-" + FormatRESPError(errMasterRefresh) + "\r\n"
		return readRESPBulk(bufio.NewReader(strings.NewReader(wire)))
	}

	_, errRefresh := standbyController.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRefresh)
	if forwardCalls != 1 {
		t.Fatalf("forward calls = %d, want 1", forwardCalls)
	}
	if masterTransport.calls != 1 {
		t.Fatalf("master provider refresh calls = %d, want 1", masterTransport.calls)
	}
	if standbyTransport.calls != 0 {
		t.Fatalf("standby provider refresh calls = %d, want 0", standbyTransport.calls)
	}

	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	requireDisabledRefreshAuth(t, persisted)
	for name, runtime := range map[string]*home.Runtime{"master": masterRuntime, "standby": standbyRuntime} {
		inMemory, ok := runtime.CoreManager().GetByID(authID)
		if !ok {
			t.Fatalf("%s GetByID() did not find auth", name)
		}
		requireDisabledRefreshAuth(t, inMemory)
	}
}

func TestRefreshControllerStandbyTransientMasterErrorFailsClosedWhenSyncFails(t *testing.T) {
	const authID = "antigravity-forwarded-transient"
	const responseBody = "first line\r\nsecond line\n"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	masterTransport := &refreshTestRoundTripper{body: responseBody}
	standbyTransport := &refreshTestRoundTripper{body: responseBody}
	masterRuntime := newRefreshTestRuntime(t, repo, auth, masterTransport)
	standbyRuntime := newRefreshTestRuntime(t, repo, auth, standbyTransport)
	standbyRuntime.SetClusterAdapter(&refreshTestFailingIndexAdapter{RuntimeAdapter: NewRuntimeAdapter(repo, "")})

	masterCoordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9405, Secret: "master-secret", StartedAt: time.Now().UTC().Add(-time.Minute)}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, masterCoordinator)
	masterController := NewRefreshController(masterCoordinator, masterRuntime, repo, nil)

	standbyCoordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.2", Port: 9406, Secret: "standby-secret"}, CoordinatorOptions{})
	standbyCoordinator.setMaster(false)
	standbyController := NewRefreshController(standbyCoordinator, standbyRuntime, repo, nil)
	standbyController.forwardRefresh = func(ctx context.Context, _ *ClusterNodeRecord, forwardedAuthID, _ string, observedAccessTokenSHA256 string, _ *tls.Config) ([]byte, error) {
		payload, errMasterRefresh := masterController.RefreshNowObserved(ctx, forwardedAuthID, observedAccessTokenSHA256)
		if errMasterRefresh == nil {
			return payload, nil
		}
		wire := "-" + FormatRESPError(errMasterRefresh) + "\r\n"
		return readRESPBulk(bufio.NewReader(strings.NewReader(wire)))
	}

	_, errRefresh := standbyController.RefreshNowObserved(ctx, authID, coreauth.AccessTokenSHA256(auth))
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != "refresh_temporarily_unavailable" {
		t.Fatalf("standby refresh error = %#v, want transient refresh error", errRefresh)
	}
	if authErr.Upstream == nil || authErr.Upstream.Status != http.StatusBadRequest || string(authErr.Upstream.Body) != responseBody || errRefresh.Error() != responseBody {
		t.Fatalf("standby upstream response = %#v, error=%q", authErr.Upstream, errRefresh.Error())
	}
	if masterTransport.calls != 1 || standbyTransport.calls != 0 {
		t.Fatalf("provider refresh calls = master %d standby %d, want 1/0", masterTransport.calls, standbyTransport.calls)
	}

	persisted, _, errPersisted := repo.GetAuth(ctx, authID)
	if errPersisted != nil {
		t.Fatalf("GetAuth() error = %v", errPersisted)
	}
	if !coreauth.RefreshBlocksDispatch(persisted) {
		t.Fatalf("persisted auth was not refresh-blocked: %#v", persisted)
	}
	standbyAuth, ok := standbyRuntime.CoreManager().GetByID(authID)
	if !ok || standbyAuth == nil {
		t.Fatal("standby GetByID() did not find auth")
	}
	if standbyAuth.Disabled || !standbyAuth.Unavailable || !standbyAuth.RuntimeRefreshBlocked || standbyAuth.Status != coreauth.StatusError || !coreauth.RefreshBlocksDispatch(standbyAuth) {
		t.Fatalf("standby fail-closed state = %#v, want transient credential block", standbyAuth)
	}
	if standbyAuth.LastError == nil || standbyAuth.LastError.Code != "refresh_temporarily_unavailable" || standbyAuth.LastError.Upstream == nil || string(standbyAuth.LastError.Upstream.Body) != responseBody {
		t.Fatalf("standby LastError = %#v, want forwarded transient upstream response", standbyAuth.LastError)
	}
}
