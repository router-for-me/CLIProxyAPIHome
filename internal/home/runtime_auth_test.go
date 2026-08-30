package home

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type failingCooldownMutationStore struct {
	err      error
	attempts int
}

type runtimeAuthTestStorage struct{}

func (*runtimeAuthTestStorage) SaveTokenToFile(string) error { return nil }

type failingAuthIndexClusterAdapter struct {
	err             error
	fullAuth        *coreauth.Auth
	fullAuthErr     error
	observedVersion int64
	observedActive  bool
}

func (a *failingAuthIndexClusterAdapter) Enabled() bool {
	return true
}

func (a *failingAuthIndexClusterAdapter) LoadAuthIndex(context.Context) error {
	return a.err
}

func (a *failingAuthIndexClusterAdapter) ListMinimalAuths() []*coreauth.Auth {
	return nil
}

func (a *failingAuthIndexClusterAdapter) GetFullAuth(context.Context, string) (*coreauth.Auth, error) {
	if a.fullAuthErr != nil {
		return nil, a.fullAuthErr
	}
	if a.fullAuth == nil {
		return nil, nil
	}
	return a.fullAuth.Clone(), nil
}

func (a *failingAuthIndexClusterAdapter) ObservedAuthState(string) (int64, bool) {
	return a.observedVersion, a.observedActive
}

func (a *failingAuthIndexClusterAdapter) LoadConfigYAML(context.Context) ([]byte, error) {
	return nil, nil
}

func (s *failingCooldownMutationStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *failingCooldownMutationStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	return auth.ID, nil
}

func (s *failingCooldownMutationStore) Delete(context.Context, string) error {
	return nil
}

func (s *failingCooldownMutationStore) MutateAuthState(context.Context, string, func(*coreauth.Auth) bool) (*coreauth.Auth, error) {
	s.attempts++
	return nil, s.err
}

func TestRuntimeAuthenticateRequestFailClosedWithoutAccessManager(t *testing.T) {
	rt := &Runtime{}
	_, authErr := rt.authenticateRequest(context.Background(), http.Header{})
	if !access.IsAuthErrorCode(authErr, access.AuthErrorCodeNoCredentials) {
		t.Fatalf("authenticateRequest() error = %v, want no credentials", authErr)
	}
}

func TestRuntimeUpdateAuthInMemoryUsesNewerClusterSnapshot(t *testing.T) {
	t.Parallel()

	const authID = "newer-cluster-auth"
	manager := coreauth.NewManager(nil, nil, nil)
	runtimeMarker := &struct{ name string }{name: "runtime"}
	storageMarker := &runtimeAuthTestStorage{}
	current := &coreauth.Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       coreauth.StatusActive,
		StateVersion: 10,
		Runtime:      runtimeMarker,
		Storage:      storageMarker,
		Success:      3,
		Failed:       2,
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	newer := current.Clone()
	newer.StateVersion = 11
	newer.Status = coreauth.StatusError
	newer.Unavailable = true
	newer.LastError = &coreauth.Error{Message: "credential unauthorized", HTTPStatus: http.StatusUnauthorized}
	newer.Runtime = nil
	newer.Storage = nil
	newer.Success = 0
	newer.Failed = 0
	runtime := &Runtime{
		coreManager:    manager,
		clusterAdapter: &failingAuthIndexClusterAdapter{fullAuth: newer},
	}

	updated, errUpdate := runtime.UpdateAuthInMemory(context.Background(), current)
	if errUpdate != nil {
		t.Fatalf("UpdateAuthInMemory() error = %v", errUpdate)
	}
	if updated.StateVersion != 11 || !updated.Unavailable || updated.LastError == nil || updated.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("UpdateAuthInMemory() = %#v, want version 11 unauthorized state", updated)
	}
	if updated.Runtime != runtimeMarker || updated.Storage != storageMarker || updated.Success != current.Success || updated.Failed != current.Failed {
		t.Fatalf("UpdateAuthInMemory() runtime state = Runtime %#v Storage %#v Success/Failed %d/%d, want local runtime state preserved", updated.Runtime, updated.Storage, updated.Success, updated.Failed)
	}
	inMemory, ok := manager.GetByID(authID)
	if !ok || inMemory == nil || inMemory.StateVersion != 11 || !inMemory.Unavailable || inMemory.LastError == nil || inMemory.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("GetByID() = %#v, want version 11 unauthorized state", inMemory)
	}
	if inMemory.Runtime != runtimeMarker || inMemory.Storage != storageMarker || inMemory.Success != current.Success || inMemory.Failed != current.Failed {
		t.Fatalf("GetByID() runtime state = Runtime %#v Storage %#v Success/Failed %d/%d, want local runtime state preserved", inMemory.Runtime, inMemory.Storage, inMemory.Success, inMemory.Failed)
	}
}

func TestRuntimeUpdateAuthInMemoryPreservesLocalModelState(t *testing.T) {
	t.Parallel()

	const authID = "newer-cluster-auth-with-local-model-state"
	const model = "gemini-3-pro"
	now := time.Now().UTC()
	manager := coreauth.NewManager(nil, nil, nil)
	current := &coreauth.Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       coreauth.StatusError,
		StateVersion: 10,
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:         coreauth.StatusError,
				StatusMessage:  "upstream unavailable",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				UpdatedAt:      now,
				LastError:      &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	newer := current.Clone()
	newer.StateVersion = 11
	newer.Status = coreauth.StatusActive
	newer.ModelStates = nil
	runtime := &Runtime{
		coreManager:    manager,
		clusterAdapter: &failingAuthIndexClusterAdapter{fullAuth: newer},
	}

	updated, errUpdate := runtime.UpdateAuthInMemory(context.Background(), current)
	if errUpdate != nil {
		t.Fatalf("UpdateAuthInMemory() error = %v", errUpdate)
	}
	state := updated.ModelStates[model]
	if updated.StateVersion != newer.StateVersion || state == nil || state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("UpdateAuthInMemory() = %#v, want version %d with local model state", updated, newer.StateVersion)
	}
}

func TestRuntimeUpdateAuthInMemoryRejectsSnapshotOlderThanObservedVersion(t *testing.T) {
	t.Parallel()

	const authID = "stale-cluster-auth"
	manager := coreauth.NewManager(nil, nil, nil)
	current := &coreauth.Auth{ID: authID, Index: authID, Provider: "antigravity", Status: coreauth.StatusActive, StateVersion: 10}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	errRead := errors.New("database temporarily unavailable")
	runtime := &Runtime{
		coreManager: manager,
		clusterAdapter: &failingAuthIndexClusterAdapter{
			fullAuthErr:     errRead,
			observedVersion: 11,
			observedActive:  true,
		},
	}

	updated, errUpdate := runtime.UpdateAuthInMemory(context.Background(), current.Clone())
	if !errors.Is(errUpdate, errRead) || updated != nil {
		t.Fatalf("UpdateAuthInMemory() = %#v, %v, want nil and %v", updated, errUpdate, errRead)
	}
	inMemory, ok := manager.GetByID(authID)
	if !ok || inMemory == nil || inMemory.StateVersion != current.StateVersion {
		t.Fatalf("GetByID() = %#v, want unchanged version %d", inMemory, current.StateVersion)
	}
}

func TestRuntimeUpdateAuthInMemoryAllowsObservedFailClosedSnapshot(t *testing.T) {
	t.Parallel()

	const authID = "fail-closed-cluster-auth"
	manager := coreauth.NewManager(nil, nil, nil)
	current := &coreauth.Auth{ID: authID, Index: authID, Provider: "antigravity", Status: coreauth.StatusActive, StateVersion: 9}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	failClosed := current.Clone()
	failClosed.StateVersion = 10
	failClosed.Status = coreauth.StatusError
	failClosed.Unavailable = true
	failClosed.LastError = &coreauth.Error{Code: "refresh_temporarily_unavailable", Message: "credential refresh temporarily unavailable", HTTPStatus: http.StatusServiceUnavailable}
	runtime := &Runtime{
		coreManager: manager,
		clusterAdapter: &failingAuthIndexClusterAdapter{
			fullAuthErr:     errors.New("database temporarily unavailable"),
			observedVersion: failClosed.StateVersion,
			observedActive:  true,
		},
	}

	updated, errUpdate := runtime.UpdateAuthInMemory(context.Background(), failClosed)
	if errUpdate != nil {
		t.Fatalf("UpdateAuthInMemory() error = %v", errUpdate)
	}
	if updated == nil || updated.StateVersion != failClosed.StateVersion || !updated.Unavailable || updated.LastError == nil {
		t.Fatalf("UpdateAuthInMemory() = %#v, want fail-closed version %d", updated, failClosed.StateVersion)
	}
}

func TestApplyCoreAuthAddOrUpdateRegistersModelsFromAcceptedSnapshot(t *testing.T) {
	const authID = "accepted-snapshot-model-registry"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager := coreauth.NewManager(nil, nil, nil)
	current := &coreauth.Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "claude",
		Status:       coreauth.StatusActive,
		StateVersion: 11,
		Metadata: map[string]any{
			"home_config_models": []map[string]any{{"id": "accepted-model"}},
		},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	runtime := &Runtime{coreManager: manager, cfg: &config.Config{}}
	stale := current.Clone()
	stale.StateVersion = 10
	stale.Metadata["home_config_models"] = []map[string]any{{"id": "stale-model"}}

	runtime.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(context.Background()), stale)

	if !modelRegistry.ClientSupportsModel(authID, "accepted-model") {
		t.Fatal("accepted model was not registered")
	}
	if modelRegistry.ClientSupportsModel(authID, "stale-model") {
		t.Fatal("rejected stale model was registered")
	}
}

func TestSanitizeAuthForDownstreamRemovesRefreshDiagnostic(t *testing.T) {
	t.Parallel()

	auth := &coreauth.Auth{
		ID:       "refresh-diagnostic-auth",
		Provider: "antigravity",
		LastError: &coreauth.Error{
			Code:       "refresh_temporarily_unavailable",
			Diagnostic: "safe refresh diagnostic",
			Upstream:   &coreauth.UpstreamResponse{Status: http.StatusBadGateway, Body: []byte(`{"error":"proxy detail"}`)},
		},
		LastRefreshError: &coreauth.Error{Code: "refresh_temporarily_unavailable", Diagnostic: "safe refresh diagnostic"},
	}
	sanitized := SanitizeAuthForDownstream(auth)
	if sanitized == nil || sanitized.LastRefreshError != nil || sanitized.LastError != nil {
		t.Fatalf("SanitizeAuthForDownstream() = %#v, want refresh diagnostic removed", sanitized)
	}
	if auth.LastRefreshError == nil || auth.LastError == nil || auth.LastError.Upstream == nil {
		t.Fatal("SanitizeAuthForDownstream() mutated the source auth")
	}

	executionErr := &coreauth.Error{Code: "model_failed", Diagnostic: "execution diagnostic", HTTPStatus: http.StatusBadGateway}
	sanitizedExecution := SanitizeAuthForDownstream(&coreauth.Auth{ID: "execution-error-auth", LastError: executionErr})
	if sanitizedExecution == nil || sanitizedExecution.LastError == nil || sanitizedExecution.LastError.Code != executionErr.Code {
		t.Fatalf("SanitizeAuthForDownstream() removed ordinary execution error: %#v", sanitizedExecution)
	}
}

func TestRuntimeDisabledCooldownClearFailureDoesNotFailConfigApplication(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{DisableCooling: true}
	store := &failingCooldownMutationStore{err: errors.New("database temporarily unavailable")}
	manager := coreauth.NewManager(store, nil, nil)
	manager.SetConfig(cfg)
	retryAt := time.Now().UTC().Add(time.Minute)
	const authID = "auth-cooldown-clear-failure"
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusError,
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	rt := &Runtime{cfg: cfg, coreManager: manager}

	if errClear := rt.clearDisabledCooldowns(ctx); errClear != nil {
		t.Fatalf("clearDisabledCooldowns() error = %v, want best-effort success", errClear)
	}
	if store.attempts != 1 {
		t.Fatalf("cooldown clear attempts = %d, want 1", store.attempts)
	}
	got, ok := manager.GetByID(authID)
	if !ok || got == nil || got.ModelStates["gpt-5"] == nil || !got.ModelStates["gpt-5"].NextRetryAfter.IsZero() || got.ModelStates["gpt-5"].Unavailable {
		t.Fatalf("auth after failed persistence = %#v, want local cooldown cleared", got)
	}
}

func TestRuntimeDoesNotReenableCoolingBeforeRevisionFenceSucceeds(t *testing.T) {
	ctx := context.Background()
	oldCfg := &config.Config{DisableCooling: true}
	store := &failingCooldownMutationStore{err: errors.New("database temporarily unavailable")}
	manager := coreauth.NewManager(store, nil, nil)
	manager.SetConfig(oldCfg)
	const authID = "auth-cooling-reenable-fence"
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusError,
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: time.Now().UTC().Add(time.Minute),
				LastError:      &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	rt := &Runtime{cfg: oldCfg, coreManager: manager}
	if errClear := manager.ClearDisabledCooldownStates(ctx); errClear == nil {
		t.Fatal("ClearDisabledCooldownStates() error = nil, want initial cleanup failure")
	}

	errApply := rt.applyConfigAndReloadAuths(ctx, &config.Config{DisableCooling: false})
	if errApply == nil {
		t.Fatal("applyConfigAndReloadAuths() error = nil, want failed revision fence")
	}
	if store.attempts != 2 {
		t.Fatalf("cooldown cleanup attempts = %d, want initial clear plus re-enable fence", store.attempts)
	}
	if cfg := rt.Config(); cfg == nil || !cfg.DisableCooling {
		t.Fatalf("runtime config = %#v, want cooling to remain disabled", cfg)
	}
	manager.MarkResult(coreauth.WithSkipPersist(ctx), coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    "gpt-5",
		Error:    &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
	})
	got, ok := manager.GetByID(authID)
	if !ok || got == nil || got.ModelStates["gpt-5"] == nil || !got.ModelStates["gpt-5"].NextRetryAfter.IsZero() {
		t.Fatalf("manager state after failed re-enable = %#v, want disable-cooling policy retained", got)
	}
}

func TestRuntimeConfigPreflightFailureKeepsPreviousCoolingPolicy(t *testing.T) {
	ctx := context.Background()
	oldCfg := &config.Config{DisableCooling: true}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(oldCfg)
	const authID = "auth-config-preflight-rollback"
	const model = "gpt-5"
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	rt := &Runtime{cfg: oldCfg, coreManager: manager}
	nextCfg := &config.Config{
		DisableCooling: false,
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     t.TempDir(),
			Configs: map[string]config.PluginInstanceConfig{
				"sample": homePluginConfigFromYAML(t, `
enabled: true
load-in-home: true
store:
  id: sample
`),
			},
		},
	}

	if errApply := rt.applyConfigAndReloadAuths(ctx, nextCfg); errApply == nil {
		t.Fatal("applyConfigAndReloadAuths() error = nil, want plugin preflight failure")
	}
	if cfg := rt.Config(); cfg != oldCfg || !cfg.DisableCooling {
		t.Fatalf("runtime config = %#v, want previous disable-cooling config", cfg)
	}
	manager.MarkResult(coreauth.WithSkipPersist(ctx), coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Error:    &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
	})
	got, ok := manager.GetByID(authID)
	if !ok || got == nil || got.ModelStates[model] == nil || !got.ModelStates[model].NextRetryAfter.IsZero() {
		t.Fatalf("manager state after failed config preflight = %#v, want previous disable-cooling policy", got)
	}
}

func TestRuntimeAuthReloadFailureRollsBackCoolingPolicy(t *testing.T) {
	ctx := context.Background()
	oldCfg := &config.Config{DisableCooling: true}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(oldCfg)
	const authID = "auth-config-reload-rollback"
	const model = "gpt-5"
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	errLoad := errors.New("auth index unavailable")
	rt := &Runtime{
		cfg:            oldCfg,
		coreManager:    manager,
		clusterAdapter: &failingAuthIndexClusterAdapter{err: errLoad},
	}

	errApply := rt.applyConfigAndReloadAuths(ctx, &config.Config{DisableCooling: false})
	if !errors.Is(errApply, errLoad) {
		t.Fatalf("applyConfigAndReloadAuths() error = %v, want %v", errApply, errLoad)
	}
	if cfg := rt.Config(); cfg != oldCfg || !cfg.DisableCooling {
		t.Fatalf("runtime config = %#v, want previous disable-cooling config", cfg)
	}
	manager.MarkResult(coreauth.WithSkipPersist(ctx), coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Error:    &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
	})
	got, ok := manager.GetByID(authID)
	if !ok || got == nil || got.ModelStates[model] == nil || !got.ModelStates[model].NextRetryAfter.IsZero() {
		t.Fatalf("manager state after failed auth reload = %#v, want previous disable-cooling policy", got)
	}
}
