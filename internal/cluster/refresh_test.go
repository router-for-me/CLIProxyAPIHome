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
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

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
	body  string
	calls int
}

func (rt *refreshTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	return &http.Response{
		StatusCode: http.StatusBadRequest,
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
	coordinator.setMaster(true)
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
	coordinator.setMaster(true)
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
	masterCoordinator.setMaster(true)
	masterController := NewRefreshController(masterCoordinator, masterRuntime, repo, nil)

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("repository database: %v", errDB)
	}
	if errCreate := db.Create(&ClusterNodeRecord{
		IP:         masterIdentity.IP,
		Port:       masterIdentity.Port,
		IsMaster:   true,
		StartedAt:  masterIdentity.StartedAt,
		LastSeenAt: time.Now().UTC(),
	}).Error; errCreate != nil {
		t.Fatalf("create master node: %v", errCreate)
	}

	standbyCoordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.2", Port: 9402, Secret: "standby-secret"}, CoordinatorOptions{})
	standbyCoordinator.setMaster(false)
	standbyController := NewRefreshController(standbyCoordinator, standbyRuntime, repo, nil)
	forwardCalls := 0
	standbyController.forwardRefresh = func(ctx context.Context, _ *ClusterNodeRecord, forwardedAuthID, _ string, _ *tls.Config) ([]byte, error) {
		forwardCalls++
		payload, errMasterRefresh := masterController.RefreshNow(ctx, forwardedAuthID)
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
