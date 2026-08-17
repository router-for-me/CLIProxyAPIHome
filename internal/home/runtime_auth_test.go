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
)

type failingCooldownMutationStore struct {
	err      error
	attempts int
}

type failingAuthIndexClusterAdapter struct {
	err error
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
	return nil, nil
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
