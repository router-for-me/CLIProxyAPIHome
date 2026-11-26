package home

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	copilotTestX    = "claude-haiku-5.5"
	copilotTestY    = "gpt-5.3-codex"
	copilotTestBase = "https://api.individual.githubcopilot.com"
)

type copilotTestTransport func(*http.Request) (*http.Response, error)

func TestCopilotDispatchRechecksCurrentCatalog(t *testing.T) {
	const id = "upstream-only"
	auth := &coreauth.Auth{ID: t.Name(), Provider: "copilot", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "fixture", "base_url": copilotTestBase}}
	rt := &Runtime{copilotAccounts: map[string]copilotAccountModels{auth.ID: {
		identity: copilotIdentity(auth), ids: map[string]struct{}{id: {}}, expiresAt: time.Now().Add(time.Hour),
	}}}
	const route = "copilot/" + id
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "copilot", []*ModelInfo{{ID: route, SourceModelID: route, MetadataModelID: id}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	decision := &coreauth.DispatchDecision{Auth: auth, UpstreamModel: route}
	if got := rt.copilotDispatchModelInfo(decision, route, time.Now()); got != nil {
		t.Fatalf("stale registry entry dispatched outside the catalog: %+v", got)
	}
}

func TestCopilotPruneExpiredProjectedToken(t *testing.T) {
	for _, excluded := range []string{"", "*"} {
		t.Run("excluded="+excluded, func(t *testing.T) {
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			rt := &Runtime{cfg: &config.Config{}, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
			auth := &coreauth.Auth{ID: t.Name(), Provider: "copilot", Status: coreauth.StatusActive, RuntimeProjection: true,
				Attributes: map[string]string{"excluded_models": excluded},
				Metadata:   map[string]any{"base_url": copilotTestBase, "expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}
			rt.copilotAccounts[auth.ID] = copilotAccountModels{identity: copilotIdentity(auth),
				ids: map[string]struct{}{copilotTestX: {}}, expiresAt: time.Now().Add(time.Hour)}
			reg := registry.GetGlobalRegistry()
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			ctx := coreauth.WithSkipPersist(t.Context())
			rt.applyCoreAuthAddOrUpdate(ctx, auth)
			if len(reg.GetModelCandidatesForClient(auth.ID)) != 1 {
				t.Fatal("missing candidates before expiry")
			}
			auth.Metadata["expired"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
			if _, errUpdate := manager.Update(ctx, auth); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			rt.PruneCopilotModels()
			if len(reg.GetModelsForClient(auth.ID)) != 0 || reg.GetModelCandidatesForClient(auth.ID) != nil {
				t.Fatal("expired projected token retained routes or candidates")
			}
		})
	}
}

func (transport copilotTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func (transport copilotTestTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper {
	return transport
}

func TestCopilotPublishedAliasDispatch(t *testing.T) {
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"copilot": {
			{Name: "copilot/" + copilotTestY, Alias: "copilot/" + copilotTestX + "(high)", Fork: true, ForceMapping: true},
			{Name: "copilot/" + copilotTestY, Alias: "friendly", Fork: true, ForceMapping: true},
			{Name: "copilot/" + copilotTestY, Alias: "copilot/legacy-friendly", Fork: true, ForceMapping: true},
		},
	}}
	m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	m.SetConfig(cfg)
	m.SetOAuthModelAlias(cfg.OAuthModelAlias)
	rt := &Runtime{cfg: cfg, coreManager: m, copilotAccounts: make(map[string]copilotAccountModels)}
	a := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Prefix: "team", Provider: "copilot", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "mock", "base_url": copilotTestBase}}
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	rt.applyCoreAuthAddOrUpdate(t.Context(), a)
	m.SetRoundTripperProvider(copilotTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"data":[{"id":%q},{"id":%q}]}`, copilotTestX, copilotTestY)))}, nil
	}))
	rt.discoverCopilotModels(t.Context(), a.ID)
	for _, tc := range []struct{ request, model, responseAlias string }{
		{"copilot/" + copilotTestX + "(high)", "copilot/" + copilotTestX + "(high)", ""},
		{"copilot/" + copilotTestX + "(low)", "copilot/" + copilotTestX + "(low)", ""},
		{"friendly", "copilot/" + copilotTestY, "friendly"},
		{"team/friendly", "copilot/" + copilotTestY, "friendly"},
		{"team/friendly(high)", "copilot/" + copilotTestY + "(high)", "friendly"},
		{"copilot/legacy-friendly", "copilot/" + copilotTestY, "copilot/legacy-friendly"},
		{"team/copilot/legacy-friendly(high)", "copilot/" + copilotTestY + "(high)", "copilot/legacy-friendly"},
	} {
		t.Run(tc.request, func(t *testing.T) {
			got, err := rt.dispatchWithOptions(t.Context(), tc.request, coreauth.Options{}, DispatchConcurrencyContext{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Model != tc.model || got.ModelInfo == nil || got.ModelInfo.ID != coreauth.CanonicalModelID(tc.model) ||
				got.ForceMapping != (tc.responseAlias != "") || got.OriginalAlias != tc.responseAlias {
				t.Fatalf("dispatch model=%q, force=%t, alias=%q, info=%+v", got.Model, got.ForceMapping, got.OriginalAlias, got.ModelInfo)
			}
		})
	}
	if _, err := rt.dispatchWithOptions(t.Context(), "copilot/friendly", coreauth.Options{}, DispatchConcurrencyContext{}); err == nil {
		t.Fatal("bare alias unexpectedly generated a copilot/-prefixed public route")
	}
}

func TestCopilotAccountEligibilityRecheckedAfterRefresh(t *testing.T) {
	const alias = "copilot/friendly"
	for _, tc := range []struct {
		name     string
		selector coreauth.Selector
	}{
		{"scheduler", &coreauth.FillFirstSelector{}},
		{"candidate scan", &dispatchSpySelector{}},
	} {
		for _, accounts := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d_accounts", tc.name, accounts), func(t *testing.T) {
				cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
					"copilot": {{Name: "copilot/" + copilotTestY, Alias: alias, ForceMapping: true}},
				}}
				manager := coreauth.NewManager(nil, tc.selector, nil)
				manager.SetConfig(cfg)
				manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
				for i := range accounts {
					id := fmt.Sprintf("%s/%d", t.Name(), i)
					auth := &coreauth.Auth{ID: id, Index: id, Provider: "copilot", Status: coreauth.StatusActive,
						Metadata: map[string]any{"access_token": "fixture", "base_url": copilotTestBase}}
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					rt.copilotAccounts[id] = copilotAccountModels{identity: copilotIdentity(auth),
						ids: map[string]struct{}{copilotTestX: {}, copilotTestY: {}}, expiresAt: time.Now().Add(time.Hour)}
					rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
				}

				manager.SetRoundTripperProvider(copilotTestTransport(func(*http.Request) (*http.Response, error) {
					body := fmt.Sprintf(`{"data":[{"id":%q}]}`, copilotTestX)
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				}))
				var refreshed string
				manager.SetFullAuthResolver(&copilotLookupTestAdapter{lookup: func(ctx context.Context, id string) (*coreauth.Auth, error) {
					if refreshed == "" {
						refreshed = id
						rt.discoverCopilotModels(ctx, id)
					}
					auth, _ := manager.GetByID(id)
					return auth, nil
				}})
				got, errDispatch := rt.dispatchWithOptions(t.Context(), alias, coreauth.Options{}, DispatchConcurrencyContext{})
				if refreshed == "" || registry.GetGlobalRegistry().ClientSupportsModel(refreshed, alias) {
					t.Fatal("the selected account did not lose the alias during credential resolution")
				}
				if accounts == 1 {
					if errDispatch == nil || got != nil {
						t.Fatal("dispatched a model the account no longer has")
					}
					return
				}
				if errDispatch != nil || got == nil || got.AuthID == refreshed || got.Model != "copilot/"+copilotTestY ||
					!got.ForceMapping || got.OriginalAlias != alias || got.ModelInfo == nil || got.ModelInfo.ID != "copilot/"+copilotTestY {
					t.Fatalf("expected the remaining GPT alias: result=%+v error=%v", got, errDispatch)
				}
			})
		}
	}
}

func TestCopilotDiscoveryDoesNotBlockHealthyAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const refreshedBase = "https://api.business.githubcopilot.com"
		m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
		rt := &Runtime{cfg: &config.Config{}, coreManager: m, copilotAccounts: make(map[string]copilotAccountModels), copilotWake: make(chan struct{}, 1)}
		started := make(chan string, copilotDiscoveryWorkers+2)
		release := make(chan struct{})
		m.SetRoundTripperProvider(copilotTestTransport(func(req *http.Request) (*http.Response, error) {
			key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if key == "refreshed" && req.URL.String() != refreshedBase+"/models" {
				t.Error("queued probe sent the refreshed token to the old origin")
			}
			select {
			case started <- key:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			if key != "healthy" {
				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}
			body := fmt.Sprintf(`{"data":[{"id":%q}]}`, copilotTestX)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		add := func(key string) {
			id := t.Name() + "/" + key
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			rt.applyCoreAuthAddOrUpdate(t.Context(), &coreauth.Auth{ID: id, Index: id, Provider: "copilot", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": key, "base_url": copilotTestBase}})
		}
		add("slow")
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go rt.runCopilotDiscovery(ctx, done)
		defer func() { cancel(); <-done }()
		synctest.Wait()
		add("healthy")
		synctest.Wait()
		got, err := rt.dispatchWithOptions(ctx, "copilot/"+copilotTestX, coreauth.Options{}, DispatchConcurrencyContext{})
		if err != nil || got.AuthID != t.Name()+"/healthy" {
			t.Fatalf("healthy account was not published while the slow probe was blocked: %v", err)
		}
		for i := 1; i < copilotDiscoveryWorkers; i++ {
			add(fmt.Sprintf("queued-%d", i))
		}
		synctest.Wait()
		add("waiting")
		synctest.Wait()
		waiting, _ := m.GetByID(t.Name() + "/waiting")
		waiting.Metadata["base_url"] = refreshedBase
		waiting.Metadata["access_token"] = "refreshed"
		rt.applyCoreAuthAddOrUpdate(ctx, waiting)
		synctest.Wait()
		if got := len(started); got != copilotDiscoveryWorkers+1 {
			t.Fatalf("started %d probes, want one completed plus %d blocked", got, copilotDiscoveryWorkers)
		}
		close(release)
		synctest.Wait()
		if got := len(started); got != copilotDiscoveryWorkers+2 {
			t.Fatalf("started %d probes after release, want %d", got, copilotDiscoveryWorkers+2)
		}
		seen := make(map[string]bool)
		for len(started) > 0 {
			key := <-started
			if seen[key] {
				t.Fatalf("duplicate discovery for %s", key)
			}
			seen[key] = true
		}
		if !seen["refreshed"] || seen["waiting"] {
			t.Fatal("queued probe did not use the refreshed credential")
		}
	})
}

func TestCopilotDiscoveryFailureDiagnostics(t *testing.T) {
	logger := log.StandardLogger()
	previous := logger.Out
	for _, tc := range []struct {
		name         string
		status       int
		body, signal string
		stage        string
	}{
		{"network", 0, "", "connection_refused", "fetch"},
		{"wrapped SDK error", 0, "", "error_type=", "fetch"},
		{"invalid JSON", 200, "{", "error_type=", "fetch"},
		{"missing data", 200, `{}`, "error_type=", "fetch"},
		{"malformed model", 200, `{"data":[{"id":{}}]}`, "invalid_response_json", "decode"},
		{"unavailable", 503, "", "status=503", "fetch"},
		{"unauthorized", 401, "", "status=401", "fetch"},
		{"forbidden", 403, "", "status=403", "fetch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			rt := &Runtime{cfg: &config.Config{}, coreManager: m, copilotAccounts: make(map[string]copilotAccountModels)}
			a := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "copilot", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "secret-fixture", "base_url": copilotTestBase}}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			rt.applyCoreAuthAddOrUpdate(t.Context(), a)
			m.SetRoundTripperProvider(copilotTestTransport(func(*http.Request) (*http.Response, error) {
				if tc.name == "wrapped SDK error" {
					return nil, fmt.Errorf("SDK failure: %w", errors.New("secret-fixture"))
				}
				if tc.status == 0 {
					return nil, errors.New("connection refused: secret-fixture")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			var logs bytes.Buffer
			logger.SetOutput(&logs)
			t.Cleanup(func() { logger.SetOutput(previous) })
			rt.discoverCopilotModels(t.Context(), a.ID)
			logger.SetOutput(previous)
			output := logs.String()
			if !strings.Contains(output, a.ID) || !strings.Contains(output, tc.signal) ||
				!strings.Contains(output, "stage="+tc.stage) || strings.Contains(output, "secret-fixture") {
				t.Fatalf("unexpected discovery diagnostic: %s", output)
			}
		})
	}
}

func TestCopilotAccountDiscoveryAndDispatch(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"copilot": {{Name: "copilot/" + copilotTestX, Alias: "x-alias", Fork: true}},
		},
		OAuthExcludedModels: map[string][]string{"copilot": {"copilot/claude-sonnet-5"}},
	}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
	for _, account := range []string{"a", "b"} {
		id := t.Name() + "/" + account
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		priority := "1"
		if account == "a" {
			priority = "10"
		}
		rt.applyCoreAuthAddOrUpdate(t.Context(), &coreauth.Auth{
			ID: id, Index: id, Provider: "copilot", Status: coreauth.StatusActive, Prefix: "team",
			Attributes: map[string]string{"priority": priority},
			Metadata: map[string]any{
				"access_token": account, "refresh_token": "mock-long-term", "base_url": copilotTestBase,
			},
		})
	}
	a, _ := manager.GetByID(t.Name() + "/a")
	b, _ := manager.GetByID(t.Name() + "/b")
	statusA := http.StatusOK
	modelsA := fmt.Sprintf(`{"data":[
		{"id":%q, "supported_endpoints":["/chat/completions"], "capabilities":{
			"limits":{"max_context_window_tokens":32768, "max_prompt_tokens":28672, "max_output_tokens":4096},
			"supports":{"reasoning_effort":["low"], "vision":false}
		}},
		{"id":"claude-sonnet-5"}
	]}`, copilotTestX)
	var preset *registry.ModelInfo
	for _, model := range registry.GetCopilotModels() {
		if model.ID == copilotTestX {
			preset = model
			break
		}
	}
	if preset == nil {
		t.Fatalf("missing Copilot catalog entry for %s", copilotTestX)
	}
	var requests int
	manager.SetRoundTripperProvider(copilotTestTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.String() != copilotTestBase+"/models" {
			t.Error("unexpected discovery endpoint")
		}
		status := statusA
		body := modelsA
		if req.Header.Get("Authorization") == "Bearer b" {
			status = http.StatusOK
			body = fmt.Sprintf(`{"data":[{"id":%q,"capabilities":{"supports":{"reasoning_effort":12}}},{"id":%q},{"id":"upstream-only"}]}`, copilotTestX, copilotTestY)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}))
	dispatch := func(route string) (*DispatchResult, error) {
		return rt.dispatchWithOptions(t.Context(), route, coreauth.Options{}, DispatchConcurrencyContext{})
	}
	if _, err := dispatch("copilot/" + copilotTestX); err == nil {
		t.Fatal("an undiscovered account was eligible")
	}
	if candidates := registry.GetGlobalRegistry().GetModelCandidatesForClient(a.ID); candidates != nil {
		t.Fatalf("undiscovered candidates = %v, want nil", candidates)
	}
	rt.discoverCopilotModels(t.Context(), a.ID)
	rt.discoverCopilotModels(t.Context(), b.ID)
	if requests != 2 {
		t.Fatalf("discovery request count = %d, want 2", requests)
	}
	const sourceX = "copilot/" + copilotTestX
	const sourceY = "copilot/" + copilotTestY
	checkDispatch := func(route, wantID, wantModel string) {
		t.Helper()
		decision, err := dispatch(route)
		if err != nil {
			t.Fatalf("dispatch %s: %v", route, err)
		}
		if decision.AuthID != wantID || decision.Model != wantModel || decision.ModelInfo == nil || decision.ModelInfo.ID != wantModel {
			t.Fatalf("unexpected credential or model for %s: got auth=%q model=%q info=%+v, want auth=%q model=%q", route, decision.AuthID, decision.Model, decision.ModelInfo, wantID, wantModel)
		}
		info := decision.ModelInfo
		if wantModel == sourceX && (info.ContextLength != preset.ContextLength || info.InputTokenLimit != preset.InputTokenLimit ||
			info.OutputTokenLimit != preset.OutputTokenLimit || info.MaxCompletionTokens != preset.MaxCompletionTokens ||
			!reflect.DeepEqual(info.Thinking, preset.Thinking)) {
			t.Fatalf("%s on %s returned capabilities inconsistent with the catalog: %+v", route, wantID, info)
		}
	}
	checkDispatch(sourceY, b.ID, sourceY)
	checkDispatch("team/x-alias", a.ID, sourceX)
	for _, auth := range []*coreauth.Auth{a, b} {
		published := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, sourceX)
		if published == nil || published.SourceModelID != sourceX || published.MetadataModelID != copilotTestX || !slices.Equal(published.SupportedInputModalities, preset.SupportedInputModalities) {
			t.Fatalf("published metadata for %s differs from the catalog: %+v", auth.ID, published)
		}
		if want := preset.DisplayName + " (Copilot)"; published.DisplayName != want {
			t.Errorf("published display name for %s = %q, want %q", auth.ID, published.DisplayName, want)
		}
	}
	for _, hidden := range []string{"copilot/claude-sonnet-5", "copilot/upstream-only", "copilot/claude-fable-5", copilotTestX, "team/copilot/x-alias"} {
		if _, err := dispatch(hidden); err == nil {
			t.Fatalf("unexpected dispatch for %s", hidden)
		}
	}

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			payload := fmt.Sprintf(`{"auth_index":%q,"provider":"copilot","model":%q,"failed":true,"fail":{"status_code":%d}}`, a.ID, sourceX, status)
			if err := rt.RecordUsagePayload(t.Context(), payload); err != nil {
				t.Fatal(err)
			}
			current, _ := manager.GetByID(a.ID)
			state := current.ModelStates[sourceX]
			if state == nil || state.LastError == nil || state.LastError.HTTPStatus != status {
				t.Fatalf("failure was not recorded against canonical source %q: states=%+v", sourceX, current.ModelStates)
			}
			checkDispatch(sourceX, b.ID, sourceX)
			checkDispatch("team/x-alias", b.ID, sourceX)
			payload = fmt.Sprintf(`{"auth_index":%q,"provider":"copilot","model":%q}`, a.ID, sourceX)
			if err := rt.RecordUsagePayload(t.Context(), payload); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("retry keeps the last successful snapshot", func(t *testing.T) {
		snapshot := rt.copilotAccounts[a.ID]
		snapshot.nextProbeAt = time.Time{}
		rt.copilotAccounts[a.ID] = snapshot
		statusA = http.StatusServiceUnavailable
		before := requests
		rt.discoverCopilotModels(t.Context(), a.ID)
		rt.discoverCopilotModels(t.Context(), a.ID)
		if requests != before+1 || rt.copilotAccounts[a.ID].expiresAt != snapshot.expiresAt {
			t.Fatal("transient failure ignored the retry interval or extended the snapshot")
		}
		checkDispatch(sourceX, a.ID, sourceX)
	})

	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"confirmed empty", http.StatusOK, `{"data":[]}`},
		{"outside catalog", http.StatusOK, `{"data":[{"id":"upstream-only"}]}`},
		{"unauthorized", http.StatusUnauthorized, `{}`},
		{"permission denied", http.StatusForbidden, `{}`},
		{"expired snapshot", http.StatusServiceUnavailable, `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			statusA, modelsA = http.StatusOK, fmt.Sprintf(`{"data":[{"id":%q}]}`, copilotTestX)
			snapshot := rt.copilotAccounts[a.ID]
			snapshot.nextProbeAt = time.Time{}
			rt.copilotAccounts[a.ID] = snapshot
			rt.discoverCopilotModels(t.Context(), a.ID)
			checkDispatch(sourceX, a.ID, sourceX)

			statusA, modelsA = test.status, test.body
			snapshot = rt.copilotAccounts[a.ID]
			snapshot.nextProbeAt = time.Time{}
			if test.status == http.StatusServiceUnavailable {
				snapshot.expiresAt = time.Now().Add(-time.Second)
			}
			rt.copilotAccounts[a.ID] = snapshot
			if test.status == http.StatusServiceUnavailable {
				// Dispatch must reject expired permissions before the background prune runs.
				checkDispatch(sourceX, b.ID, sourceX)
			}
			rt.discoverCopilotModels(t.Context(), a.ID)
			checkDispatch(sourceX, b.ID, sourceX)
			if models := registry.GetGlobalRegistry().GetModelsForClient(a.ID); len(models) != 0 {
				t.Fatal("unavailable account still publishes models")
			}
			rt.PruneCopilotModels()
			candidates := registry.GetGlobalRegistry().GetModelCandidatesForClient(a.ID)
			if len(candidates) != 0 || (candidates != nil) != (test.status == http.StatusOK) {
				t.Fatalf("exclusion candidates = %#v for status %d", candidates, test.status)
			}
		})
	}
}

func TestCopilotModelDiscoveryAfterCredentialRebinding(t *testing.T) {
	for _, change := range []string{"replace provider", "delete credential"} {
		t.Run(change, func(t *testing.T) {
			ctx := coreauth.WithSkipPersist(t.Context())
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			rt := &Runtime{cfg: &config.Config{}, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels), copilotWake: make(chan struct{}, 1)}
			auth := &coreauth.Auth{ID: t.Name(), Provider: "copilot", StateVersion: 2,
				Metadata: map[string]any{"base_url": copilotTestBase, "account_id": "same-account", "access_token": "fixture"}}
			reg := registry.GetGlobalRegistry()
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			allowed := copilotTestX
			manager.SetRoundTripperProvider(copilotTestTransport(func(*http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"data":[{"id":%q}]}`, allowed)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			rt.applyCoreAuthAddOrUpdate(ctx, auth)
			select {
			case <-rt.copilotWake:
			default:
				t.Fatal("new credential did not wake model discovery")
			}
			rt.discoverCopilotModels(ctx, auth.ID)
			oldRoute := "copilot/" + copilotTestX
			if reg.GetModelInfoForClient(auth.ID, oldRoute) == nil {
				t.Fatal("first discovery did not publish the account model")
			}
			if change == "replace provider" {
				other := &coreauth.Auth{ID: auth.ID, Provider: "claude", StateVersion: 1}
				rt.applyCoreAuthAddOrUpdate(ctx, other)
				if reg.GetModelInfoForClient(auth.ID, oldRoute) == nil {
					t.Fatal("a rejected provider change retired the current account")
				}
				other.StateVersion = 3
				rt.applyCoreAuthAddOrUpdate(ctx, other)
				if len(reg.GetModelsForClient(auth.ID)) == 0 {
					t.Fatal("replacement provider did not publish its own models")
				}
			} else {
				rt.applyCoreAuthRemove(ctx, auth.ID)
			}
			if reg.GetModelInfoForClient(auth.ID, oldRoute) != nil {
				t.Fatal("old provider route remained registered")
			}
			auth.StateVersion = 4
			rt.applyCoreAuthAddOrUpdate(ctx, auth)
			if len(reg.GetModelsForClient(auth.ID)) != 0 {
				t.Fatal("reused credential ID published the previous discovery result")
			}
			select {
			case <-rt.copilotWake:
			default:
				t.Fatal("reused credential ID did not wake model discovery")
			}
			allowed = copilotTestY
			rt.discoverCopilotModels(ctx, auth.ID)
			if models := reg.GetModelsForClient(auth.ID); len(models) != 1 || models[0].ID != "copilot/"+allowed {
				t.Fatalf("models after rediscovery = %v, want only copilot/%s", models, allowed)
			}
		})
	}
}

func TestCopilotRefreshInvalidatesOldOrigin(t *testing.T) {
	const newBase = "https://api.business.githubcopilot.com"
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	cfg := &config.Config{}
	manager.SetConfig(cfg)
	rt := &Runtime{cfg: cfg, coreManager: manager, copilotAccounts: make(map[string]copilotAccountModels)}
	id := t.Name()
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	now := time.Now()
	auth := &coreauth.Auth{
		ID: id, Index: id, Provider: "copilot", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "old-token", "refresh_token": "mock-grant", "base_url": copilotTestBase, "expired": now.Add(4 * time.Minute).Format(time.RFC3339)},
	}
	if !manager.ShouldRefreshCredential(auth, now) || manager.ShouldRefreshCredential(auth, now.Add(-2*time.Minute)) {
		t.Fatal("unexpected refresh lead")
	}
	rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
	var oldModels, newModels, refreshes int
	manager.SetRoundTripperProvider(copilotTestTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.String() {
		case "https://api.github.com/copilot_internal/v2/token":
			refreshes++
			if req.Header.Get("Authorization") != "Bearer mock-grant" {
				t.Error("token exchange did not use the GitHub grant")
			}
			body = fmt.Sprintf(`{"token":"new-token","expires_at":%d,"endpoints":{"api":%q}}`, now.Add(time.Hour).Unix(), newBase)
		case copilotTestBase + "/models":
			oldModels++
			body = fmt.Sprintf(`{"data":[{"id":%q}]}`, copilotTestX)
		case newBase + "/models":
			newModels++
			if req.Header.Get("Authorization") != "Bearer new-token" {
				t.Error("new origin did not receive the refreshed token")
			}
			body = fmt.Sprintf(`{"data":[{"id":%q}]}`, copilotTestY)
		default:
			t.Error("unexpected request during discovery or refresh")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}))
	rt.discoverCopilotModels(t.Context(), auth.ID)
	payload, err := rt.RefreshNowLocal(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(payload, "auth.metadata.base_url").String() != newBase ||
		gjson.GetBytes(payload, "auth.metadata.access_token").String() != "new-token" ||
		gjson.GetBytes(payload, "auth.metadata.refresh_token").Exists() {
		t.Fatal("refresh payload did not contain a safe host/token pair")
	}
	updated, _ := manager.GetByID(id)
	if _, err := rt.dispatchWithOptions(t.Context(), "copilot/"+copilotTestX, coreauth.Options{}, DispatchConcurrencyContext{}); err == nil {
		t.Fatal("old origin's model remained eligible after refresh")
	}
	rt.discoverCopilotModels(t.Context(), updated.ID)
	rt.registerModelsForAuth(auth)
	decision, err := rt.dispatchWithOptions(t.Context(), "copilot/"+copilotTestY, coreauth.Options{}, DispatchConcurrencyContext{})
	if err != nil || decision == nil || copilotIdentity(decision.Auth).base != newBase || decision.AccessToken != "new-token" {
		t.Fatal("next dispatch did not use the new origin and token")
	}
	disabled := updated.Clone()
	disabled.Disabled = true
	disabled.Status = coreauth.StatusDisabled
	rt.applyCoreAuthAddOrUpdate(t.Context(), disabled)
	rt.applyCoreAuthAddOrUpdate(t.Context(), updated)
	if _, errDispatch := rt.dispatchWithOptions(t.Context(), "copilot/"+copilotTestY, coreauth.Options{}, DispatchConcurrencyContext{}); errDispatch == nil {
		t.Fatal("re-enabled account reused its previous model snapshot")
	}
	if oldModels != 1 || newModels != 1 || refreshes != 1 {
		t.Fatalf("calls to old models, new models, and refresh: %d, %d, %d", oldModels, newModels, refreshes)
	}
}
