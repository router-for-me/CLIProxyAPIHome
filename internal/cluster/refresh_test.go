package cluster

import (
	"bufio"
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
	for _, secret := range []string{"response-access-secret", "stored-access-secret", "stored-refresh-secret", "invalid_grant"} {
		if strings.Contains(errRefresh.Error(), secret) {
			t.Fatalf("RefreshNow() error leaked provider data %q: %v", secret, errRefresh)
		}
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
	if strings.Contains(auth.LastError.Error(), "secret") || strings.Contains(auth.LastError.Error(), "invalid_grant") {
		t.Fatalf("LastError leaked provider data: %v", auth.LastError)
	}
}

func TestReadRESPBulkRestoresStructuredRefreshError(t *testing.T) {
	t.Parallel()

	_, errRead := readRESPBulk(bufio.NewReader(strings.NewReader("-ERR authentication_error: credential unauthorized\r\n")))
	requireTerminalRefreshError(t, errRead)
}

func TestRefreshControllerForwardedAuthFailureFailsClosed(t *testing.T) {
	const authID = "antigravity-fail-closed"
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	runtime := newRefreshTestRuntime(t, repo, auth, &refreshTestRoundTripper{})
	controller := NewRefreshController(nil, runtime, repo, nil)

	errDisable := controller.disableForwardedAuthInMemory(context.Background(), authID, &coreauth.Error{
		Code:       "authentication_error",
		Message:    "credential unauthorized",
		HTTPStatus: http.StatusUnauthorized,
	})
	if errDisable != nil {
		t.Fatalf("disableForwardedAuthInMemory() error = %v", errDisable)
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
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked.","access_token":"response-access-secret"}`}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9301, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRefresh)
	_, errRepeated := controller.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRepeated)
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

func TestRefreshControllerTransientFailureKeepsCredentialDispatchable(t *testing.T) {
	const authID = "antigravity-transient"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	providerCalls := 0
	transport := refreshRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return nil, errors.New("proxy unavailable")
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
	if _, errRepeated := controller.RefreshNowObserved(ctx, authID, coreauth.AccessTokenSHA256(auth)); errRepeated == nil {
		t.Fatal("repeated refresh during backoff returned nil error")
	}
	if providerCalls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1 during refresh backoff", providerCalls)
	}

	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Disabled || persisted.Unavailable || persisted.Status != coreauth.StatusActive {
		t.Fatalf("transient refresh blocked credential dispatch: %#v", persisted)
	}
	if persisted.NextRefreshAfter.IsZero() || !persisted.NextRetryAfter.IsZero() {
		t.Fatalf("refresh/retry deadlines = %v/%v, want refresh-only backoff", persisted.NextRefreshAfter, persisted.NextRetryAfter)
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

	masterTransport := &refreshTestRoundTripper{body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked.","access_token":"response-access-secret"}`}
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
		wire := "-ERR " + errMasterRefresh.Error() + "\r\n"
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
