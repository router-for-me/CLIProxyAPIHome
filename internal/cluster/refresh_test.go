package cluster

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

	_, errForward := ForwardRefreshToMaster(context.Background(), &ClusterNodeRecord{IP: "127.0.0.1", Port: 8327}, "auth-id", "secret", time.Time{}, "", nil)
	if errForward == nil {
		t.Fatal("ForwardRefreshToMaster() error = nil, want TLS config error")
	}
	if !strings.Contains(errForward.Error(), "tls config is required") {
		t.Fatalf("ForwardRefreshToMaster() error = %v, want TLS config error", errForward)
	}
}

type refreshTestRoundTripper struct {
	statusCode int
	body       string
	err        error
	calls      int
	hook       func()
}

func (rt *refreshTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	if rt.hook != nil {
		rt.hook()
	}
	if rt.err != nil {
		return nil, rt.err
	}
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
	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
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
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
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

func TestRefreshControllerSkipsRefreshForNewerStoredCredential(t *testing.T) {
	const authID = "antigravity-already-refreshed"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	auth.Metadata["access_token"] = "new-access-token"
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{body: `{"error":"invalid_grant"}`}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9301, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	observedDigest := sha256.Sum256([]byte("old-access-token"))
	payload, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, hex.EncodeToString(observedDigest[:]))
	if errRefresh != nil {
		t.Fatalf("RefreshNowObserved() error = %v", errRefresh)
	}
	if len(payload) == 0 {
		t.Fatal("RefreshNowObserved() returned empty auth payload")
	}
	if transport.calls != 0 {
		t.Fatalf("provider refresh calls = %d, want 0", transport.calls)
	}
}

func TestRefreshControllerUnsupportedCredentialReturnsErrorAndBacksOff(t *testing.T) {
	const authID = "codex-refresh-unsupported"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	recoverAt := time.Now().UTC().Add(30 * time.Minute)
	auth := &coreauth.Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         coreauth.StatusError,
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: recoverAt},
		Metadata:       map[string]any{"access_token": "access-only"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	runtime := newRefreshTestRuntime(t, repo, auth, &refreshTestRoundTripper{})
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9304, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, coreauth.AccessTokenSHA256(auth))
	if !errors.Is(errRefresh, coreauth.ErrRefreshUnsupported) {
		t.Fatalf("RefreshNowObserved() error = %v, want ErrRefreshUnsupported", errRefresh)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if !persisted.NextRefreshAfter.After(time.Now()) {
		t.Fatalf("unsupported refresh NextRefreshAfter = %v, want future", persisted.NextRefreshAfter)
	}
	if !persisted.Unavailable || !persisted.NextRetryAfter.Equal(recoverAt) || !persisted.Quota.Exceeded {
		t.Fatalf("unsupported refresh cleared auth quota cooldown: %#v", persisted)
	}
}

func TestContextRefreshLockHonorsCancellation(t *testing.T) {
	lock := newContextRefreshLock()
	if errLock := lock.Lock(context.Background()); errLock != nil {
		t.Fatalf("first Lock() error = %v", errLock)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if errLock := lock.Lock(ctx); !errors.Is(errLock, context.Canceled) {
		t.Fatalf("second Lock() error = %v, want context.Canceled", errLock)
	}
	lock.Unlock()
}

func TestRefreshControllerBlocksDispatchWhileProviderRefreshIsInFlight(t *testing.T) {
	const authID = "antigravity-refresh-in-flight"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	transport := &refreshTestRoundTripper{
		statusCode: http.StatusOK,
		body:       `{"access_token":"fresh-access-token","expires_in":3600}`,
		hook: func() {
			close(started)
			<-release
		},
	}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9305, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	refreshDone := make(chan error, 1)
	go func() {
		_, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, coreauth.AccessTokenSHA256(auth))
		refreshDone <- errRefresh
	}()
	<-started

	inMemory, ok := runtime.CoreManager().GetByID(authID)
	if !ok || inMemory == nil || !coreauth.RefreshBackoffOpen(inMemory, time.Now().UTC()) {
		close(release)
		<-refreshDone
		t.Fatalf("in-flight refresh state = %#v, want dispatch-blocking refresh state", inMemory)
	}
	decision, errDispatch := runtime.CoreManager().Dispatch(ctx, []string{"antigravity"}, "model-a", coreauth.Options{})
	if errDispatch == nil || decision != nil {
		close(release)
		<-refreshDone
		t.Fatalf("Dispatch() = decision %#v error %v, want credential blocked", decision, errDispatch)
	}

	close(release)
	if errRefresh := <-refreshDone; errRefresh != nil {
		t.Fatalf("RefreshNowObserved() error = %v", errRefresh)
	}
}

func TestRefreshControllerDoesNotRetryCredentialDisabledDuringOAuth(t *testing.T) {
	const authID = "antigravity-disabled-during-refresh"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	var disableErr error
	transport := &refreshTestRoundTripper{
		statusCode: http.StatusOK,
		body:       `{"access_token":"fresh-access-token","expires_in":3600}`,
		hook: func() {
			_, _, _, disableErr = repo.MutateAuth(ctx, authID, "disable", func(current *coreauth.Auth) bool {
				current.Disabled = true
				current.Unavailable = true
				current.Status = coreauth.StatusDisabled
				return true
			})
		},
	}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9306, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, coreauth.AccessTokenSHA256(auth))
	if disableErr != nil {
		t.Fatalf("disable during OAuth failed: %v", disableErr)
	}
	requireTerminalRefreshError(t, errRefresh)
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if !persisted.Disabled || persisted.Status != coreauth.StatusDisabled {
		t.Fatalf("disabled auth state = %#v", persisted)
	}
}

func TestRefreshControllerCancellationRestoresPreClaimState(t *testing.T) {
	const authID = "antigravity-canceled-refresh"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{err: context.Canceled}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9307, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, coreauth.AccessTokenSHA256(auth))
	if !errors.Is(errRefresh, context.Canceled) {
		t.Fatalf("RefreshNowObserved() error = %v, want context.Canceled", errRefresh)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Unavailable || !persisted.NextRetryAfter.IsZero() || !persisted.NextRefreshAfter.IsZero() || persisted.Attributes[refreshLeaseAttribute] != "" {
		t.Fatalf("canceled refresh retained claim state: %#v", persisted)
	}
}

func TestRefreshControllerDeadlineRestoresPreClaimState(t *testing.T) {
	const authID = "antigravity-deadline-refresh"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{err: context.DeadlineExceeded}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9308, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	_, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, coreauth.AccessTokenSHA256(auth))
	if !errors.Is(errRefresh, context.DeadlineExceeded) {
		t.Fatalf("RefreshNowObserved() error = %v, want context.DeadlineExceeded", errRefresh)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Unavailable || !persisted.NextRetryAfter.IsZero() || !persisted.NextRefreshAfter.IsZero() || persisted.Attributes[refreshLeaseAttribute] != "" {
		t.Fatalf("deadline refresh retained claim state: %#v", persisted)
	}
}

func TestRefreshControllerDoesNotHoldDatabaseTransactionDuringOAuth(t *testing.T) {
	const authID = "antigravity-transaction-scope"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	var mutateErr error
	transport := &refreshTestRoundTripper{
		statusCode: http.StatusOK,
		body:       `{"access_token":"fresh-access-token","expires_in":3600}`,
	}
	transport.hook = func() {
		_, _, _, mutateErr = repo.MutateAuth(ctx, authID, "concurrent-update", func(current *coreauth.Auth) bool {
			current.Label = "updated-during-oauth"
			current.Metadata["management_note"] = "preserve-me"
			return true
		})
	}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9303, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	if _, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, coreauth.AccessTokenSHA256(auth)); errRefresh != nil {
		t.Fatalf("RefreshNowObserved() error = %v", errRefresh)
	}
	if mutateErr != nil {
		t.Fatalf("concurrent mutation during OAuth failed: %v", mutateErr)
	}
	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Label != "updated-during-oauth" || persisted.Metadata["management_note"] != "preserve-me" {
		t.Fatalf("concurrent update was overwritten: label=%q metadata=%#v", persisted.Label, persisted.Metadata)
	}
	if persisted.Metadata["access_token"] != "fresh-access-token" {
		t.Fatalf("refreshed access token = %v, want fresh-access-token", persisted.Metadata["access_token"])
	}
}

func TestMergeClusterRefreshOutcomePreservesIneffectiveBackoff(t *testing.T) {
	now := time.Now().UTC()
	base := newInvalidGrantRefreshAuth("ineffective-backoff")
	current := base.Clone()
	refreshed := base.Clone()
	refreshed.Metadata["access_token"] = "same-access-token"
	refreshed.NextRefreshAfter = now.Add(30 * time.Second)

	merged := mergeClusterRefreshOutcome(current, base, refreshed, nil, now)
	if merged == nil || !merged.NextRefreshAfter.Equal(refreshed.NextRefreshAfter) {
		t.Fatalf("merged NextRefreshAfter = %v, want %v", merged.NextRefreshAfter, refreshed.NextRefreshAfter)
	}
}

func TestRefreshControllerTransientBackoffDeduplicatesStaleRefreshes(t *testing.T) {
	const authID = "antigravity-transient-backoff"
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	transport := &refreshTestRoundTripper{body: `{"error":"temporary","detail":"upstream-secret"}`}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9302, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	observedHash := coreauth.AccessTokenSHA256(auth)

	for attempt := 0; attempt < 2; attempt++ {
		_, errRefresh := controller.RefreshNowObserved(ctx, authID, time.Time{}, observedHash)
		if errRefresh == nil {
			t.Fatalf("attempt %d error = nil, want transient refresh error", attempt+1)
		}
		if strings.Contains(errRefresh.Error(), "upstream-secret") {
			t.Fatalf("attempt %d leaked provider response: %v", attempt+1, errRefresh)
		}
	}
	if transport.calls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1 during backoff", transport.calls)
	}

	persisted, _, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Disabled || !coreauth.RefreshBackoffOpen(persisted, time.Now().UTC()) {
		t.Fatalf("persisted transient refresh state = %#v", persisted)
	}
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
	if transport.calls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1", transport.calls)
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
	standbyController.forwardRefresh = func(ctx context.Context, _ *ClusterNodeRecord, forwardedAuthID, _ string, observedRefreshAt time.Time, observedAccessTokenSHA256 string, _ *tls.Config) ([]byte, error) {
		forwardCalls++
		payload, errMasterRefresh := masterController.RefreshNowObserved(ctx, forwardedAuthID, observedRefreshAt, observedAccessTokenSHA256)
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
