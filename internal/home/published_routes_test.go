package home

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type publicationTestPlugin struct {
	static       []sdkpluginhost.ModelInfo
	result       sdkpluginhost.AuthModelResult
	beforeResult func()
}

func (p publicationTestPlugin) ModelsForAuth(context.Context, *cpaauth.Auth) sdkpluginhost.AuthModelResult {
	if p.beforeResult != nil {
		p.beforeResult()
	}
	return p.result
}

func (p publicationTestPlugin) ModelsForProvider(string) []sdkpluginhost.ModelInfo { return p.static }

func TestPluginModelAuthUpdatePublication(t *testing.T) {
	cfg := &config.Config{}
	cfg.ForceModelPrefix = true
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	rt := &Runtime{cfg: cfg, coreManager: manager}
	auth := &coreauth.Auth{ID: t.Name(), Provider: "claude", FileName: "fixture.json", StateVersion: 7, Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "fixture", "account_id": "first"}}
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	rt.applyCoreAuthAddOrUpdate(t.Context(), auth)

	update := &cpaauth.Auth{ID: "plugin-id", Prefix: "team", Status: cpaauth.StatusActive, Metadata: auth.Clone().Metadata}
	rt.publishModelsForAuth(auth, publicationTestPlugin{result: sdkpluginhost.AuthModelResult{
		Handled: true, Auth: update, Models: []sdkpluginhost.ModelInfo{{ID: copilotTestX}},
	}})
	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Prefix != "team" || current.Provider != "claude" || current.FileName != auth.FileName ||
		current.Attributes["auth_kind"] != "oauth" || current.Metadata["account_id"] != "first" {
		t.Fatal("plugin update lost the credential identity, defaults or changed fields")
	}
	models := reg.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "team/"+copilotTestX {
		t.Fatalf("expected only the updated route %q, got %v", "team/"+copilotTestX, models)
	}
}

func TestClaudeStaticPluginAliasCapabilities(t *testing.T) {
	const source = "plugin-model"
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"claude": {
			{Name: source, Alias: "friendly"}, {Name: source, Alias: "friendly(high)"},
		}},
		OAuthSettings: map[string][]config.OAuthModelSetting{"claude": {
			{Name: source, Alias: "friendly", MaxContextLength: 32000},
			{Name: source, Alias: "friendly(high)", MaxContextLength: 64000},
		}},
	}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	rt := &Runtime{cfg: cfg, coreManager: manager}
	auth := &coreauth.Auth{ID: t.Name(), Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"access_token": "fixture"}}
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	rt.publishModelsForAuth(auth, publicationTestPlugin{static: []sdkpluginhost.ModelInfo{{ID: source}}})
	manager.RefreshSchedulerEntry(auth.ID)
	if reg.ClientSupportsModel(auth.ID, source) {
		t.Fatal("non-forked static plugin model was still published under its original name")
	}
	for _, tc := range []struct {
		route string
		want  int
	}{
		{"friendly", 32000}, {"friendly(high)", 64000}, {"friendly(low)", 32000},
	} {
		got, errDispatch := rt.dispatchWithOptions(t.Context(), tc.route, coreauth.Options{}, DispatchConcurrencyContext{})
		if errDispatch != nil || got.ModelInfo == nil || got.ModelInfo.ID != source || got.ModelInfo.ContextLength != tc.want {
			t.Errorf("dispatch %q = (%+v, %v), want context %d", tc.route, got, errDispatch, tc.want)
		}
	}
}

func TestOpenAICompatibilityDoesNotAppendStaticPluginModels(t *testing.T) {
	for _, from := range []string{"metadata", "configured", "empty", "unconfigured"} {
		t.Run(from, func(t *testing.T) {
			cfg := &config.Config{}
			auth := &coreauth.Auth{ID: t.Name(), Provider: "custom", Status: coreauth.StatusActive,
				Attributes: map[string]string{"provider_key": "custom", "compat_name": "custom", "auth_kind": "apikey"}}
			switch from {
			case "metadata":
				auth.Metadata = map[string]any{homeConfigModelsMetadataKey: []map[string]any{{"id": "configured"}}}
			case "configured":
				cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "custom", Models: []config.OpenAICompatibilityModel{{Name: "configured"}}}}
			case "empty":
				cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "custom"}}
			}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			rt := &Runtime{cfg: cfg, coreManager: manager}
			if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			rt.publishModelsForAuth(auth, publicationTestPlugin{static: []sdkpluginhost.ModelInfo{{ID: "plugin-only"}}})
			manager.RefreshSchedulerEntry(auth.ID)
			if supported := reg.ClientSupportsModel(auth.ID, "configured"); supported != (from == "metadata" || from == "configured") {
				t.Fatalf("configured route availability = %t", supported)
			}
			if _, errDispatch := rt.dispatchWithOptions(t.Context(), "plugin-only", coreauth.Options{}, DispatchConcurrencyContext{}); errDispatch == nil {
				t.Fatal("dispatched an unconfigured static plugin model")
			}
		})
	}
}

func TestApplyModelPrefixesPreservesExplicitPublishedNames(t *testing.T) {
	for _, provider := range []string{"copilot", "devin"} {
		t.Run(provider, func(t *testing.T) {
			baseSource := provider + "/base"
			explicitSource := provider + "/explicit"
			models := []*ModelInfo{
				{ID: "chat", SourceModelID: baseSource, MetadataModelID: "base", ResponseModelAlias: "chat"},
				{ID: "team/chat", SourceModelID: explicitSource, MetadataModelID: "explicit", ResponseModelAlias: "team/chat"},
			}
			for _, tc := range []struct {
				name  string
				force bool
				want  map[string]struct{ source, alias string }
			}{
				{
					name:  "optional",
					force: false,
					want: map[string]struct{ source, alias string }{
						"chat":           {baseSource, "chat"},
						"team/chat":      {explicitSource, "team/chat"},
						"team/team/chat": {explicitSource, "team/chat"},
					},
				},
				{
					name:  "forced",
					force: true,
					want: map[string]struct{ source, alias string }{
						"team/chat":      {baseSource, "chat"},
						"team/team/chat": {explicitSource, "team/chat"},
					},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					published := applyModelPrefixes(models, "team", tc.force)
					if len(published) != len(tc.want) {
						t.Fatalf("published %d routes, want %d: %+v", len(published), len(tc.want), published)
					}
					for _, model := range published {
						want, ok := tc.want[model.ID]
						if !ok {
							t.Fatalf("unexpected route %q", model.ID)
						}
						if model.SourceModelID != want.source || model.ResponseModelAlias != want.alias {
							t.Errorf("route %q = (source=%q, alias=%q), want (%q, %q)", model.ID, model.SourceModelID, model.ResponseModelAlias, want.source, want.alias)
						}
					}
				})
			}
		})
	}
}

func TestPublishedRoutesAcceptanceCases(t *testing.T) {
	t.Run("copilot acceptance case 1", func(t *testing.T) {
		const native = copilotTestY
		const source = "copilot/" + native
		for _, forcePrefix := range []bool{false, true} {
			t.Run(fmt.Sprintf("force_prefix=%t", forcePrefix), func(t *testing.T) {
				cfg := &config.Config{
					OAuthModelAlias: map[string][]config.OAuthModelAlias{
						"copilot": {{Name: source, Alias: "friendly", Fork: true, ForceMapping: true}},
					},
				}
				cfg.ForceModelPrefix = forcePrefix
				manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
				manager.SetConfig(cfg)
				manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
				auth := &coreauth.Auth{
					ID: t.Name(), Index: t.Name(), Provider: "copilot", Prefix: "team", Status: coreauth.StatusActive,
					Metadata: map[string]any{"access_token": "mock", "base_url": copilotTestBase},
				}
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				rt.copilotAccounts[auth.ID] = copilotAccountModels{
					identity:  copilotIdentity(auth),
					ids:       map[string]struct{}{native: {}},
					expiresAt: time.Now().Add(time.Hour),
				}
				rt.applyCoreAuthAddOrUpdate(t.Context(), auth)

				got, err := rt.dispatchWithOptions(t.Context(), "team/friendly(high)", coreauth.Options{}, DispatchConcurrencyContext{})
				if err != nil {
					t.Fatalf("dispatch team/friendly(high): %v", err)
				}
				if got.Model != source+"(high)" || got.ModelInfo == nil || got.ModelInfo.ID != source || !got.ForceMapping || got.OriginalAlias != "friendly" {
					t.Fatalf("got model=%q info=%+v force=%t alias=%q", got.Model, got.ModelInfo, got.ForceMapping, got.OriginalAlias)
				}
				if forcePrefix {
					if _, errUnprefixed := rt.dispatchWithOptions(t.Context(), "friendly(high)", coreauth.Options{}, DispatchConcurrencyContext{}); errUnprefixed == nil {
						t.Fatal("unprefixed friendly succeeded with force-model-prefix=true")
					}
				} else {
					unprefixed, errUnprefixed := rt.dispatchWithOptions(t.Context(), "friendly(high)", coreauth.Options{}, DispatchConcurrencyContext{})
					if errUnprefixed != nil || unprefixed.Model != source+"(high)" || unprefixed.OriginalAlias != "friendly" {
						t.Fatalf("unprefixed friendly = %+v, err = %v", unprefixed, errUnprefixed)
					}
				}
				if _, errLegacy := rt.dispatchWithOptions(t.Context(), "copilot/friendly", coreauth.Options{}, DispatchConcurrencyContext{}); errLegacy == nil {
					t.Fatal("copilot/friendly should not be auto-generated for alias=friendly")
				}
			})
		}
	})

	t.Run("devin acceptance case 2 and alias collision", func(t *testing.T) {
		const swe2Source = "devin/swe-2"
		const flashSource = "devin/swe-2-flash"
		var excluded []string
		for _, model := range registry.GetDevinModels() {
			if model.ID != "swe-2" && model.ID != "swe-2-flash" {
				excluded = append(excluded, "devin/"+model.ID)
			}
		}

		type routeExpectation struct {
			route        string
			wantModel    string
			wantInfoID   string
			wantAlias    string
			wantForce    bool
			wantRejected bool
		}
		tests := []struct {
			name        string
			forcePrefix bool
			withChat    bool
			wantListed  []string
			checks      []routeExpectation
		}{
			{
				name:        "optional prefix single alias",
				forcePrefix: false,
				withChat:    false,
				wantListed:  []string{"team/chat", "team/team/chat", flashSource, "team/" + flashSource},
				checks: []routeExpectation{
					{route: "team/chat(high)", wantModel: swe2Source + "(high)", wantInfoID: swe2Source, wantAlias: "team/chat", wantForce: true},
					{route: "team/team/chat(high)", wantModel: swe2Source + "(high)", wantInfoID: swe2Source, wantAlias: "team/chat", wantForce: true},
					{route: "chat", wantRejected: true},
					{route: swe2Source, wantRejected: true},
				},
			},
			{
				name:        "forced prefix single alias",
				forcePrefix: true,
				withChat:    false,
				wantListed:  []string{"team/team/chat", "team/" + flashSource},
				checks: []routeExpectation{
					{route: "team/team/chat(high)", wantModel: swe2Source + "(high)", wantInfoID: swe2Source, wantAlias: "team/chat", wantForce: true},
					{route: "team/chat(high)", wantRejected: true},
					{route: "chat", wantRejected: true},
					{route: swe2Source, wantRejected: true},
				},
			},
			{
				name:        "optional prefix collision explicit team/chat wins",
				forcePrefix: false,
				withChat:    true,
				wantListed:  []string{"chat", "team/chat", "team/team/chat"},
				checks: []routeExpectation{
					{route: "chat(high)", wantModel: flashSource + "(high)", wantInfoID: flashSource, wantAlias: "chat", wantForce: true},
					{route: "team/chat(high)", wantModel: swe2Source + "(high)", wantInfoID: swe2Source, wantAlias: "team/chat", wantForce: true},
					{route: "team/team/chat(high)", wantModel: swe2Source + "(high)", wantInfoID: swe2Source, wantAlias: "team/chat", wantForce: true},
				},
			},
			{
				name:        "forced prefix collision both prefixed routes published",
				forcePrefix: true,
				withChat:    true,
				wantListed:  []string{"team/chat", "team/team/chat"},
				checks: []routeExpectation{
					{route: "chat(high)", wantRejected: true},
					{route: "team/chat(high)", wantModel: flashSource + "(high)", wantInfoID: flashSource, wantAlias: "chat", wantForce: true},
					{route: "team/team/chat(high)", wantModel: swe2Source + "(high)", wantInfoID: swe2Source, wantAlias: "team/chat", wantForce: true},
				},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				aliases := []config.OAuthModelAlias{
					{Name: swe2Source, Alias: "team/chat", ForceMapping: true},
				}
				if tc.withChat {
					aliases = append([]config.OAuthModelAlias{
						{Name: flashSource, Alias: "chat", ForceMapping: true},
					}, aliases...)
				}
				cfg := &config.Config{
					OAuthExcludedModels: map[string][]string{"devin": excluded},
					OAuthModelAlias:     map[string][]config.OAuthModelAlias{"devin": aliases},
				}
				cfg.ForceModelPrefix = tc.forcePrefix
				manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
				manager.SetConfig(cfg)
				manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				rt := &Runtime{cfg: cfg, coreManager: manager}
				auth := &coreauth.Auth{
					ID: t.Name(), Index: t.Name(), Provider: "devin", Prefix: "team", Status: coreauth.StatusActive,
					Attributes: map[string]string{"auth_kind": "oauth", "api_key": "mock-token"},
				}
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				rt.applyCoreAuthAddOrUpdate(t.Context(), auth)

				registered := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
				var listed []string
				for _, model := range registered {
					listed = append(listed, model.ID)
				}
				slices.Sort(listed)
				wantListed := slices.Clone(tc.wantListed)
				slices.Sort(wantListed)
				if !slices.Equal(listed, wantListed) {
					t.Fatalf("published routes = %v, want %v", listed, wantListed)
				}

				for _, check := range tc.checks {
					got, err := rt.dispatchWithOptions(t.Context(), check.route, coreauth.Options{}, DispatchConcurrencyContext{})
					if check.wantRejected {
						if err == nil {
							t.Errorf("route %q unexpectedly dispatched: %+v", check.route, got)
						}
						continue
					}
					if err != nil {
						t.Errorf("route %q failed: %v", check.route, err)
						continue
					}
					if got.Model != check.wantModel || got.ModelInfo == nil || got.ModelInfo.ID != check.wantInfoID ||
						got.ForceMapping != check.wantForce || got.OriginalAlias != check.wantAlias {
						t.Errorf("route %q = (model=%q info=%+v force=%t alias=%q), want (%q, %q, %t, %q)",
							check.route, got.Model, got.ModelInfo, got.ForceMapping, got.OriginalAlias,
							check.wantModel, check.wantInfoID, check.wantForce, check.wantAlias)
					}
				}
			})
		}
	})
}

func TestPublishedRoutesExclusionsAndExplicitAliases(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		excluded  []string
		aliases   []config.OAuthModelAlias
		wantRoute string
		wantSrc   string
		rejected  []string
	}{
		{
			name:      "copilot exclusion before alias and verbatim short alias",
			provider:  "copilot",
			excluded:  []string{"copilot/" + copilotTestX},
			aliases:   []config.OAuthModelAlias{{Name: "copilot/" + copilotTestY, Alias: "friendly"}, {Name: "copilot/" + copilotTestX, Alias: "hidden"}},
			wantRoute: "friendly",
			wantSrc:   "copilot/" + copilotTestY,
			rejected:  []string{"copilot/friendly", "hidden", "copilot/" + copilotTestX, "copilot/" + copilotTestY},
		},
		{
			name:      "copilot explicit qualified alias",
			provider:  "copilot",
			excluded:  []string{"copilot/" + copilotTestX},
			aliases:   []config.OAuthModelAlias{{Name: "copilot/" + copilotTestY, Alias: "copilot/friendly"}},
			wantRoute: "copilot/friendly",
			wantSrc:   "copilot/" + copilotTestY,
			rejected:  []string{"friendly", "copilot/" + copilotTestX, "copilot/" + copilotTestY},
		},
		{
			name:      "devin exclusion before alias and verbatim short alias",
			provider:  "devin",
			excluded:  []string{"devin/swe-2-flash"},
			aliases:   []config.OAuthModelAlias{{Name: "devin/swe-2", Alias: "fast"}, {Name: "devin/swe-2-flash", Alias: "hidden"}},
			wantRoute: "fast",
			wantSrc:   "devin/swe-2",
			rejected:  []string{"devin/fast", "hidden", "devin/swe-2-flash", "devin/swe-2"},
		},
		{
			name:      "bare alias name does not match qualified source",
			provider:  "copilot",
			aliases:   []config.OAuthModelAlias{{Name: copilotTestY, Alias: "unmatched"}},
			wantRoute: "copilot/" + copilotTestY,
			wantSrc:   "copilot/" + copilotTestY,
			rejected:  []string{"unmatched", "copilot/unmatched"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				OAuthExcludedModels: map[string][]string{tc.provider: tc.excluded},
				OAuthModelAlias:     map[string][]config.OAuthModelAlias{tc.provider: tc.aliases},
			}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
			rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
			auth := &coreauth.Auth{
				ID: t.Name(), Index: t.Name(), Provider: tc.provider, Status: coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "api_key": "mock-token"},
				Metadata:   map[string]any{"access_token": "mock", "base_url": copilotTestBase},
			}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if tc.provider == "copilot" {
				rt.copilotAccounts[auth.ID] = copilotAccountModels{
					identity:  copilotIdentity(auth),
					ids:       map[string]struct{}{copilotTestX: {}, copilotTestY: {}},
					expiresAt: time.Now().Add(time.Hour),
				}
			}
			rt.applyCoreAuthAddOrUpdate(t.Context(), auth)

			got, err := rt.dispatchWithOptions(t.Context(), tc.wantRoute, coreauth.Options{}, DispatchConcurrencyContext{})
			if err != nil || got.Model != tc.wantSrc || got.ModelInfo == nil || got.ModelInfo.ID != tc.wantSrc {
				t.Fatalf("dispatch %q = (%+v, %v), want source %q", tc.wantRoute, got, err, tc.wantSrc)
			}
			for _, rejected := range tc.rejected {
				if _, errRej := rt.dispatchWithOptions(t.Context(), rejected, coreauth.Options{}, DispatchConcurrencyContext{}); errRej == nil {
					t.Errorf("rejected route %q unexpectedly succeeded", rejected)
				}
			}
		})
	}
}

func TestPublishedRoutesOAuthSettings(t *testing.T) {
	for _, provider := range []struct {
		name   string
		native string
		alias  string
	}{
		{name: "copilot", native: copilotTestY, alias: "fast"},
		{name: "devin", native: "swe-2", alias: "team/chat"},
	} {
		t.Run(provider.name, func(t *testing.T) {
			source := provider.name + "/" + provider.native
			cfg := &config.Config{
				OAuthModelAlias: map[string][]config.OAuthModelAlias{
					provider.name: {{Name: source, Alias: provider.alias, Fork: true, ForceMapping: true}},
				},
			}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
			rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
			auth := &coreauth.Auth{
				ID: t.Name(), Index: t.Name(), Provider: provider.name, Prefix: "team", Status: coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "api_key": "mock-token"},
				Metadata:   map[string]any{"access_token": "mock", "base_url": copilotTestBase},
			}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if provider.name == "copilot" {
				rt.copilotAccounts[auth.ID] = copilotAccountModels{
					identity:  copilotIdentity(auth),
					ids:       map[string]struct{}{provider.native: {}},
					expiresAt: time.Now().Add(time.Hour),
				}
			}
			rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
			defaultInfo := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, source)
			if defaultInfo == nil || defaultInfo.ContextLength <= 0 {
				t.Fatalf("missing default catalog metadata for %s", source)
			}
			defaultContext := defaultInfo.ContextLength

			for _, tc := range []struct {
				name                  string
				settings              []config.OAuthModelSetting
				sourceSize, aliasSize int
			}{
				{name: "default", sourceSize: defaultContext, aliasSize: defaultContext},
				{name: "bare catalog id", settings: []config.OAuthModelSetting{{Name: provider.native, MaxContextLength: 131072}}, sourceSize: 131072, aliasSize: 131072},
				{name: "qualified source id", settings: []config.OAuthModelSetting{{Name: source, MaxContextLength: 131072}}, sourceSize: 131072, aliasSize: 131072},
				{
					name: "alias override wins over source",
					settings: []config.OAuthModelSetting{
						{Name: source, MaxContextLength: 131072},
						{Name: source, Alias: provider.alias, MaxContextLength: 524288},
					},
					sourceSize: 131072,
					aliasSize:  524288,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cfg.OAuthSettings = map[string][]config.OAuthModelSetting{provider.name: tc.settings}
					rt.registerModelsForAuth(auth)
					manager.RefreshSchedulerEntry(auth.ID)
					for _, route := range []struct {
						id       string
						wantSize int
					}{
						{source, tc.sourceSize},
						{"team/" + source, tc.sourceSize},
						{provider.alias, tc.aliasSize},
						{"team/" + provider.alias, tc.aliasSize},
					} {
						info := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, route.id)
						if info == nil || info.SourceModelID != source || info.MetadataModelID != provider.native || info.ContextLength != route.wantSize {
							t.Fatalf("route %q catalog info = %+v, want source=%q meta=%q context=%d", route.id, info, source, provider.native, route.wantSize)
						}
						encoded, errMarshal := json.Marshal(info)
						if errMarshal != nil {
							t.Fatal(errMarshal)
						}
						for _, internalField := range []string{"SourceModelID", "source_model_id", "MetadataModelID", "metadata_model_id", "ResponseModelAlias", "response_model_alias"} {
							if strings.Contains(string(encoded), internalField) {
								t.Fatalf("internal field %q leaked into serialized model info: %s", internalField, encoded)
							}
						}
						got, errDispatch := rt.dispatchWithOptions(t.Context(), route.id+"(high)", coreauth.Options{}, DispatchConcurrencyContext{})
						if errDispatch != nil || got.Model != source+"(high)" || got.ModelInfo == nil || got.ModelInfo.ID != source || got.ModelInfo.ContextLength != route.wantSize {
							t.Fatalf("dispatch %q = (%+v, %v), want source=%q context=%d", route.id, got, errDispatch, source, route.wantSize)
						}
					}
				})
			}
		})
	}
}

func TestPublishedRoutesSharedCooldownAndRebinding(t *testing.T) {
	for _, tc := range []struct {
		provider   string
		nativeA    string
		nativeB    string
		alias      string
		prefix     string
		reusedName string
	}{
		{provider: "copilot", nativeA: copilotTestX, nativeB: copilotTestY, alias: "friendly", prefix: "team", reusedName: "copilot/" + copilotTestX},
		{provider: "devin", nativeA: "swe-2-flash", nativeB: "swe-2", alias: "team/chat", prefix: "team", reusedName: "devin/swe-2-flash"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			sourceA := tc.provider + "/" + tc.nativeA
			sourceB := tc.provider + "/" + tc.nativeB
			cfg := &config.Config{
				OAuthModelAlias: map[string][]config.OAuthModelAlias{
					tc.provider: {{Name: sourceA, Alias: tc.alias, Fork: true, ForceMapping: true}},
				},
			}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
			rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
			auth := &coreauth.Auth{
				ID: t.Name(), Index: t.Name(), Provider: tc.provider, Prefix: tc.prefix, Status: coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "api_key": "mock-token"},
				Metadata:   map[string]any{"access_token": "mock", "base_url": copilotTestBase},
			}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if tc.provider == "copilot" {
				rt.copilotAccounts[auth.ID] = copilotAccountModels{
					identity:  copilotIdentity(auth),
					ids:       map[string]struct{}{tc.nativeA: {}, tc.nativeB: {}},
					expiresAt: time.Now().Add(time.Hour),
				}
			}
			rt.applyCoreAuthAddOrUpdate(t.Context(), auth)

			allSourceARoutes := []string{sourceA, tc.prefix + "/" + sourceA, tc.alias, tc.prefix + "/" + tc.alias, tc.prefix + "/" + tc.alias + "(high)"}

			// 1. Canonical 429 usage report cools source A across all aliases, prefixes, and thinking suffixes.
			payload429 := fmt.Sprintf(`{"auth_index":%q,"provider":%q,"model":%q,"failed":true,"fail":{"status_code":429}}`, auth.ID, tc.provider, sourceA)
			if errRecord := rt.RecordUsagePayload(t.Context(), payload429); errRecord != nil {
				t.Fatal(errRecord)
			}
			for _, route := range allSourceARoutes {
				if _, errDispatch := rt.dispatchWithOptions(t.Context(), route, coreauth.Options{}, DispatchConcurrencyContext{}); errDispatch == nil {
					t.Fatalf("429 on %q did not cool route %q", sourceA, route)
				}
			}
			// Unrelated source B remains healthy.
			if got, errDispatch := rt.dispatchWithOptions(t.Context(), sourceB, coreauth.Options{}, DispatchConcurrencyContext{}); errDispatch != nil || got.Model != sourceB {
				t.Fatalf("source B was unexpectedly cooled: got=%+v err=%v", got, errDispatch)
			}

			// 2. Rebinding alias (and even reusing the excluded source A name) to healthy source B must not inherit source A's 429.
			cfg.OAuthExcludedModels = map[string][]string{tc.provider: {sourceA}}
			cfg.OAuthModelAlias[tc.provider] = []config.OAuthModelAlias{
				{Name: sourceB, Alias: tc.alias, Fork: true, ForceMapping: true},
				{Name: sourceB, Alias: tc.reusedName, Fork: true, ForceMapping: true},
			}
			manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
			rt.registerModelsForAuth(auth)
			manager.ReconcileRegistryModelStates(t.Context(), auth.ID)
			manager.RefreshSchedulerEntry(auth.ID)

			for _, rebound := range []struct{ route, wantModel, wantAlias string }{
				{tc.alias + "(high)", sourceB + "(high)", tc.alias},
				{tc.prefix + "/" + tc.alias + "(low)", sourceB + "(low)", tc.alias},
				{tc.reusedName + "(high)", sourceB + "(high)", tc.reusedName},
			} {
				got, errDispatch := rt.dispatchWithOptions(t.Context(), rebound.route, coreauth.Options{}, DispatchConcurrencyContext{})
				if errDispatch != nil || got.Model != rebound.wantModel || got.OriginalAlias != rebound.wantAlias {
					t.Fatalf("rebound route %q blocked or misrouted: got=%+v err=%v", rebound.route, got, errDispatch)
				}
			}
		})
	}
}

func TestDevinLegacyRegistrationWithoutSourceModelID(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"devin": {{Name: "custom-devin-upstream", Alias: "custom-devin-alias", ForceMapping: true}},
		},
	}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	auth := &coreauth.Auth{
		ID: t.Name(), Index: t.Name(), Provider: "devin", Prefix: "team", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth", "api_key": "mock-token"},
	}
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	// Register a route directly without SourceModelID (mimicking plugin/legacy registration).
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "devin", []*registry.ModelInfo{
		{ID: "team/custom-devin-alias", Type: "devin"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.RefreshSchedulerEntry(auth.ID)

	if got := manager.ResolveExecutionModel(auth, "team/custom-devin-alias(high)"); got != "custom-devin-upstream(high)" {
		t.Fatalf("legacy Devin execution model = %q, want custom-devin-upstream(high)", got)
	}
	decision, errDispatch := manager.Dispatch(t.Context(), []string{"devin"}, "team/custom-devin-alias(high)", coreauth.Options{})
	if errDispatch != nil || decision == nil || decision.UpstreamModel != "custom-devin-upstream(high)" || !decision.ForceMapping || decision.OriginalAlias != "custom-devin-alias" {
		t.Fatalf("legacy Devin dispatch = (%+v, %v), want custom-devin-upstream(high) with force-mapping", decision, errDispatch)
	}
}

func TestCopilotTokenExpiryConsistentBetweenListingAndDispatch(t *testing.T) {
	for _, accounts := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d_accounts", accounts), func(t *testing.T) {
			cfg := &config.Config{}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
			now := time.Now()
			for i := range accounts {
				id := fmt.Sprintf("%s/%d", t.Name(), i)
				priority := "1"
				expiry := now.Add(time.Hour)
				if i == 0 {
					priority = "10"
					expiry = now.Add(-time.Minute)
				}
				auth := &coreauth.Auth{
					ID: id, Index: id, Provider: "copilot", Status: coreauth.StatusActive,
					Attributes: map[string]string{"priority": priority},
					Metadata: map[string]any{
						"access_token": "tok",
						"base_url":     copilotTestBase,
						"expired":      expiry.UTC().Format(time.RFC3339),
					},
				}
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				rt.copilotAccounts[id] = copilotAccountModels{
					identity:  copilotIdentity(auth),
					ids:       map[string]struct{}{copilotTestY: {}},
					expiresAt: now.Add(time.Hour),
				}
				rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
			}

			// Expired token must not publish models even when snapshot itself has not expired.
			if models := registry.GetGlobalRegistry().GetModelsForClient(fmt.Sprintf("%s/0", t.Name())); len(models) != 0 {
				t.Fatalf("expired Copilot token still published models: %+v", models)
			}
			got, errDispatch := rt.dispatchWithOptions(t.Context(), "copilot/"+copilotTestY, coreauth.Options{}, DispatchConcurrencyContext{})
			if accounts == 1 {
				if errDispatch == nil || got != nil {
					t.Fatalf("expired single account dispatched: %+v", got)
				}
				return
			}
			wantID := fmt.Sprintf("%s/1", t.Name())
			if errDispatch != nil || got == nil || got.AuthID != wantID || got.Model != "copilot/"+copilotTestY {
				t.Fatalf("dispatch = (%+v, %v), want healthy account %q", got, errDispatch, wantID)
			}
		})
	}
}

func TestCopilotIgnoresPluginModels(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	rt := &Runtime{cfg: &config.Config{}, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
	auth := &coreauth.Auth{
		ID: t.Name(), Index: t.Name(), Provider: "copilot", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "fixture", "base_url": copilotTestBase},
	}
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	rt.copilotAccounts[auth.ID] = copilotAccountModels{
		identity: copilotIdentity(auth), expiresAt: time.Now().Add(time.Hour),
		ids: map[string]struct{}{copilotTestX: {}, "plugin-only": {}},
	}
	rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
	rt.publishModelsForAuth(auth, publicationTestPlugin{
		static: []sdkpluginhost.ModelInfo{{ID: "plugin-only"}, {ID: copilotTestX + "(high)"}},
		result: sdkpluginhost.AuthModelResult{
			Handled: true, Provider: "copilot",
			Models: []sdkpluginhost.ModelInfo{{ID: "plugin-only"}, {ID: copilotTestX + "(high)"}},
		},
	})
	models := reg.GetModelsForClient(auth.ID)
	if len(models) != 1 || models[0].ID != "copilot/"+copilotTestX {
		t.Fatalf("copilot models = %+v, want only catalog intersection [copilot/%s]", models, copilotTestX)
	}
}

func TestPublishedRoutesLegacyUsageCooldownAndRebindingIsolation(t *testing.T) {
	for _, tc := range []struct {
		provider string
		nativeA  string
		nativeB  string
	}{
		{provider: "copilot", nativeA: copilotTestX, nativeB: copilotTestY},
		{provider: "devin", nativeA: "swe-2-flash", nativeB: "swe-2"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			sourceA := tc.provider + "/" + tc.nativeA
			sourceB := tc.provider + "/" + tc.nativeB
			cfg := &config.Config{
				OAuthModelAlias: map[string][]config.OAuthModelAlias{
					tc.provider: {{Name: sourceA, Alias: "friendly", Fork: true, ForceMapping: true}},
				},
			}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
			rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
			auth := &coreauth.Auth{
				ID: t.Name(), Index: t.Name(), Provider: tc.provider, Status: coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "api_key": "mock-token"},
				Metadata:   map[string]any{"access_token": "mock", "base_url": copilotTestBase},
			}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if tc.provider == "copilot" {
				rt.copilotAccounts[auth.ID] = copilotAccountModels{
					identity:  copilotIdentity(auth),
					ids:       map[string]struct{}{tc.nativeA: {}, tc.nativeB: {}},
					expiresAt: time.Now().Add(time.Hour),
				}
			}
			rt.applyCoreAuthAddOrUpdate(t.Context(), auth)

			// Legacy unqualified usage feedback cools the canonical source and its aliases.
			payload := fmt.Sprintf(`{"auth_index":%q,"provider":%q,"model":%q,"failed":true,"fail":{"status_code":429}}`, auth.ID, tc.provider, tc.nativeA)
			if errRecord := rt.RecordUsagePayload(t.Context(), payload); errRecord != nil {
				t.Fatal(errRecord)
			}
			for _, route := range []string{sourceA, "friendly"} {
				if _, errDispatch := rt.dispatchWithOptions(t.Context(), route, coreauth.Options{}, DispatchConcurrencyContext{}); errDispatch == nil {
					t.Fatalf("legacy 429 on %q did not cool route %q", tc.nativeA, route)
				}
			}

			// Rebinding an alias (including reusing the bare legacy name) to sourceB must stay isolated.
			cfg.OAuthExcludedModels = map[string][]string{tc.provider: {sourceA}}
			cfg.OAuthModelAlias[tc.provider] = []config.OAuthModelAlias{
				{Name: sourceB, Alias: "friendly", Fork: true, ForceMapping: true},
				{Name: sourceB, Alias: tc.nativeA, Fork: true, ForceMapping: true},
			}
			manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
			rt.registerModelsForAuth(auth)
			manager.ReconcileRegistryModelStates(t.Context(), auth.ID)
			manager.RefreshSchedulerEntry(auth.ID)

			for _, route := range []string{"friendly", tc.nativeA} {
				got, errDispatch := rt.dispatchWithOptions(t.Context(), route, coreauth.Options{}, DispatchConcurrencyContext{})
				if errDispatch != nil || got.Model != sourceB || got.OriginalAlias != route {
					t.Fatalf("rebound route %q = (%+v, %v), want %q", route, got, errDispatch, sourceB)
				}
			}
		})
	}
}
