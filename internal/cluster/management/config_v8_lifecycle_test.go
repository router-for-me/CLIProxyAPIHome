package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gopkg.in/yaml.v3"
)

// executionLifecycleView compares the actual CPA payload, not private source trees
// or timestamps. Identity and advertised model capabilities must also survive.
type executionLifecycleView struct {
	ID, Provider, Model, APIKey, BaseURL, ProxyURL string
	Pool                                           *cpaauth.ProxyConnectionPoolOverride
	Options                                        any
	VertexOpenAPI, Interactions                    string
	ModelInfo                                      *home.DispatchModelInfo
}

func executionLifecycleDispatch(t *testing.T, repo *cluster.Repository) map[string]executionLifecycleView {
	t.Helper()
	cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(t.Context())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	runtime, errRuntime := home.NewRuntime(cfg)
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	defer runtime.Stop()
	runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "lifecycle-test"))
	if errReload := runtime.ReloadAuths(t.Context()); errReload != nil {
		t.Fatal(errReload)
	}
	auths, errAuths := repo.ListAuths(t.Context())
	if errAuths != nil {
		t.Fatal(errAuths)
	}
	views := make(map[string]executionLifecycleView, len(auths))
	for _, auth := range auths {
		models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
		route := ""
		for _, model := range models {
			if strings.HasPrefix(model.ID, "lifecycle-") {
				route = model.ID
				break
			}
		}
		if route == "" {
			t.Fatalf("credential %s lost its configured model route", auth.ID)
		}
		decision, errDispatch := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), route, nil, "", "", nil, auth.ID, home.DispatchConcurrencyContext{})
		if errDispatch != nil || decision == nil {
			t.Fatalf("dispatch %s: %v", auth.ID, errDispatch)
		}
		sanitized, errSanitize := home.SanitizeAuthForDownstream(decision.Auth, cfg)
		if errSanitize != nil {
			t.Fatal(errSanitize)
		}
		encoded, errMarshal := json.Marshal(sanitized)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		var cpa cpaauth.Auth
		if errDecode := json.Unmarshal(encoded, &cpa); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, private := range []string{"home_config", "home_v8_group", "home_config_models", "refresh_token", "service_account"} {
			if _, exists := cpa.Metadata[private]; exists {
				t.Fatalf("private field %s reached CPA", private)
			}
		}
		views[auth.ID] = executionLifecycleView{
			ID: cpa.ID, Provider: decision.Provider, Model: decision.Model, APIKey: decision.APIKey, BaseURL: decision.BaseURL, ProxyURL: cpa.ProxyURL,
			Pool: cpa.ProxyConnectionPool, Options: cpa.Metadata["credential_options"], VertexOpenAPI: cpa.Attributes["vertex_model_openapi"], Interactions: cpa.Attributes["interactions"], ModelInfo: decision.ModelInfo,
		}
	}
	wire, errWire := runtime.ReadConfigYAMLContext(t.Context())
	if errWire != nil {
		t.Fatal(errWire)
	}
	var root map[string]any
	if errDecode := yaml.Unmarshal(wire, &root); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, private := range []string{"remote-management", "api-keys", "auth-dir", "tls", "gemini-api-key", "interactions-api-key", "codex-api-key", "xai-api-key", "meta-api-key", "claude-api-key", "vertex-api-key", "openai-compatibility"} {
		if _, exists := root[private]; exists {
			t.Fatalf("private config %s reached CPA", private)
		}
	}
	if _, errCPA := cpaconfig.ParseConfigBytes(wire); errCPA != nil {
		t.Fatal(errCPA)
	}
	return views
}

func executionLifecycleVersions(t *testing.T, repo *cluster.Repository) map[string]int64 {
	t.Helper()
	auths, errList := repo.ListAuths(t.Context())
	if errList != nil {
		t.Fatal(errList)
	}
	versions := make(map[string]int64, len(auths))
	for _, auth := range auths {
		_, record, errGet := repo.GetAuth(t.Context(), auth.ID)
		if errGet != nil {
			t.Fatal(errGet)
		}
		versions[auth.ID] = record.Version
	}
	return versions
}

func TestConfigV8ExecutionLifecycleAllProviders(t *testing.T) {
	repo, request := newConfigV8AuthIndexRouter(t)
	families := []string{"gemini", "interactions", "vertex", "codex", "xai", "meta", "claude", "openai-compatibility"}
	groups := make(map[string]any, len(families))
	for _, family := range families {
		var keys []any
		for index, pool := range []any{"omitted", nil, map[string]any{}, map[string]any{"enabled": false}, map[string]any{"enabled": nil, "max-idle-conns-per-host": 0}} {
			key := map[string]any{"api-key": fmt.Sprintf("%s-key-%d", family, index)}
			if pool != "omitted" {
				key["connection-pool"] = pool
			}
			if family == "claude" {
				key["cloak"] = map[string]any{"mode": "never", "cache-user-id": false, "sensitive-patterns-replace": []any{map[string]any{"pattern": "^private", "replacement": ""}}}
			}
			keys = append(keys, key)
		}
		model := map[string]any{"name": "lifecycle-upstream-" + family, "alias": "lifecycle-" + family}
		if family == "vertex" {
			model["openapi"] = false
		}
		group := map[string]any{
			"name": "lifecycle-" + family, "base-url": "https://example.test/v1", "models": []any{model}, "keys": keys,
			"connection-pool": map[string]any{"enabled": true, "idle-conn-timeout": "123s", "max-idle-conns-per-host": 7},
		}
		if family == "vertex" {
			group["interactions"] = true
		}
		groups[family] = []any{group}
	}
	root := map[string]any{
		"config-version": 8, "api-keys": groups,
		"server":     map[string]any{"github-token": "test-only-token", "response-compression": map[string]any{"enabled": false, "min-bytes": 0}, "tls": map[string]any{"enable": false, "cert": "home-only-cert", "key": "home-only-key"}},
		"management": map[string]any{"secret-key": "home-only-test-secret"},
		"client":     map[string]any{"codex": map[string]any{"enable-apply-patch": false, "web-search-result-cache": map[string]any{"enabled": false, "ttl": "24h", "limit": 0}}},
		"upstream":   map[string]any{"claude": map[string]any{"header-defaults": map[string]any{"native-entrypoints": []any{}, "inject-diagnostics": nil}}},
	}
	input := filepath.Join(t.TempDir(), "config.yaml")
	data, errMarshal := yaml.Marshal(root)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(input, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if _, errImport := cluster.ImportLocalState(t.Context(), cluster.ImportOptions{Repository: repo, ConfigPath: input}); errImport != nil {
		t.Fatal(errImport)
	}
	// The editing view must preserve the two source levels as well as the effective dispatch options.
	for _, family := range families {
		var records []map[string]any
		if errDecode := json.Unmarshal([]byte(request("GET", "/v8/management/config/api-keys/"+family, "", http.StatusOK)), &records); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(records) != 1 || records[0]["name"] != "lifecycle-"+family {
			t.Fatalf("%s did not retain its authored group", family)
		}
		members := records[0]["keys"].([]any)
		if len(members) != 5 {
			t.Fatalf("%s returned %d group members", family, len(members))
		}
		for _, member := range members {
			key := member.(map[string]any)
			secret := key["api-key"].(string)
			pool, exists := key["connection-pool"]
			want := []any{nil, nil, map[string]any{}, map[string]any{"enabled": false}, map[string]any{"enabled": nil, "max-idle-conns-per-host": float64(0)}}
			index := int(secret[len(secret)-1] - '0')
			if exists != (index != 0) || !reflect.DeepEqual(pool, want[index]) {
				t.Fatalf("%s key %d lost its connection pool scope: %#v", family, index, pool)
			}
		}
	}
	initial := executionLifecycleDispatch(t, repo)
	if len(initial) != len(families)*5 {
		t.Fatalf("credentials = %d", len(initial))
	}
	for _, view := range initial {
		pool := view.Pool
		if pool == nil || pool.Enabled == nil || pool.IdleConnTimeout != "123s" || pool.MaxIdleConnsPerHost == nil {
			t.Fatalf("lost inherited pool for %s", view.ID)
		}
		index := view.APIKey[len(view.APIKey)-1]
		wantIdleCount := 7
		if index == '4' {
			wantIdleCount = 0
		}
		if *pool.Enabled != (index != '3') || *pool.MaxIdleConnsPerHost != wantIdleCount {
			t.Fatalf("incorrect effective pool for %s: %+v", view.APIKey, pool)
		}
	}
	versions := executionLifecycleVersions(t, repo)
	whole := request("GET", "/v8/management/config", "", http.StatusOK)
	request("PUT", "/v8/management/config", whole, http.StatusOK)
	yamlRead := request("GET", "/v8/management/config.yaml", "", http.StatusOK)
	request("PUT", "/v8/management/config.yaml", yamlRead, http.StatusOK)
	request("PATCH", "/v8/management/config/client/codex", `{"enable-apply-patch":true}`, http.StatusOK)
	if got := executionLifecycleVersions(t, repo); !reflect.DeepEqual(versions, got) {
		t.Fatal("reading/writing config or changing an unrelated field rewrote credentials")
	}
	if got := executionLifecycleDispatch(t, repo); !reflect.DeepEqual(initial, got) {
		t.Fatal("read/modify changed CPA credential behavior")
	}
	// Reordering is a no-op; editing one key must retain IDs and all sibling options.
	for _, family := range families {
		path := "/v8/management/config/api-keys/" + family
		var familyGroups []map[string]any
		if errDecode := json.Unmarshal([]byte(request("GET", path, "", http.StatusOK)), &familyGroups); errDecode != nil {
			t.Fatal(errDecode)
		}
		slices.Reverse(familyGroups)
		var editedID string
		for _, group := range familyGroups {
			for _, raw := range group["keys"].([]any) {
				key := raw.(map[string]any)
				if key["api-key"] == family+"-key-0" {
					key["connection-pool"] = map[string]any{"enabled": false, "idle-conn-timeout": "0s", "max-idle-conns-per-host": 0}
					editedID = key["id"].(string)
				}
			}
		}
		encoded, errEncode := json.Marshal(familyGroups)
		if errEncode != nil {
			t.Fatal(errEncode)
		}
		before := executionLifecycleVersions(t, repo)
		request("PUT", path, string(encoded), http.StatusOK)
		after := executionLifecycleVersions(t, repo)
		if len(after) != len(before) || editedID == "" {
			t.Fatal("credential identities changed")
		}
		for id, version := range before {
			if (id == editedID && after[id] <= version) || (id != editedID && after[id] != version) {
				t.Fatalf("editing %s changed unexpected credential %s", editedID, id)
			}
		}
	}
	modified := executionLifecycleDispatch(t, repo)
	for id, view := range modified {
		want := initial[id]
		if view.APIKey[len(view.APIKey)-1] == '0' {
			if view.Pool.Enabled == nil || *view.Pool.Enabled || view.Pool.IdleConnTimeout != "0s" || *view.Pool.MaxIdleConnsPerHost != 0 {
				t.Fatalf("edit did not reach dispatch for %s", id)
			}
			want.Pool = view.Pool
			options := want.Options.(map[string]any)
			options["connection-pool"] = view.Options.(map[string]any)["connection-pool"]
		}
		if !reflect.DeepEqual(want, view) {
			t.Fatalf("edit changed unrelated execution fields for %s", id)
		}
	}
	// Invalid input must roll back both the config tree and authoritative auth rows.
	beforeConfig := request("GET", "/v8/management/config", "", http.StatusOK)
	beforeVersions := executionLifecycleVersions(t, repo)
	for _, invalid := range []struct{ path, body string }{
		{"routing/session-affinity-rebind-on", `"invalid"`},
		{"client/codex", `{"enable-apply-patch":"invalid"}`},
		{"api-keys/vertex", `[{"keys":[{"api-key":"bad","connection-pool":{"enabled":"invalid"}}]}]`},
		{"api-keys/claude", `[{"keys":[{"api-key":"bad","cloak":{"sensitive-patterns-replace":"invalid"}}]}]`},
	} {
		request("PUT", "/v8/management/config/"+invalid.path, invalid.body, http.StatusBadRequest)
	}
	if got := request("GET", "/v8/management/config", "", http.StatusOK); got != beforeConfig {
		t.Fatal("rejected mutation changed configuration")
	}
	if !reflect.DeepEqual(beforeVersions, executionLifecycleVersions(t, repo)) {
		t.Fatal("rejected mutation changed credential versions")
	}
	for cycle := range 2 {
		output := t.TempDir()
		if _, errExport := cluster.ExportLocalState(t.Context(), cluster.ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
			t.Fatal(errExport)
		}
		next, nextRequest := newConfigV8AuthIndexRouter(t)
		if _, errImport := cluster.ImportLocalState(t.Context(), cluster.ImportOptions{Repository: next, ConfigPath: filepath.Join(output, "config.yaml")}); errImport != nil {
			t.Fatal(errImport)
		}
		if got := executionLifecycleDispatch(t, next); !reflect.DeepEqual(modified, got) {
			t.Fatalf("export/reimport cycle %d changed dispatch behavior", cycle)
		}
		for _, path := range []string{"server", "client", "upstream"} {
			if request("GET", "/v8/management/config/"+path, "", http.StatusOK) != nextRequest("GET", "/v8/management/config/"+path, "", http.StatusOK) {
				t.Fatalf("cycle %d changed explicit empty/false/zero/null configuration in %s", cycle, path)
			}
		}
		repo, request = next, nextRequest
	}
	// Removing an option is different from false/null: no stale runtime
	// projection or private source may recreate it on the next dispatch/import.
	for _, family := range families {
		path := "/v8/management/config/api-keys/" + family
		var familyGroups []map[string]any
		if errDecode := json.Unmarshal([]byte(request("GET", path, "", http.StatusOK)), &familyGroups); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, group := range familyGroups {
			delete(group, "connection-pool")
			for _, raw := range group["keys"].([]any) {
				delete(raw.(map[string]any), "connection-pool")
			}
		}
		encoded, errEncode := json.Marshal(familyGroups)
		if errEncode != nil {
			t.Fatal(errEncode)
		}
		request("PUT", path, string(encoded), http.StatusOK)
	}
	cleared := executionLifecycleDispatch(t, repo)
	for id, view := range cleared {
		want := modified[id]
		want.Pool = nil
		delete(want.Options.(map[string]any), "connection-pool")
		if !reflect.DeepEqual(want, view) {
			t.Fatalf("removing a pool retained stale options or changed other fields for %s", id)
		}
	}
	request("DELETE", "/v8/management/config/api-keys/meta", "", http.StatusOK)
	remaining := executionLifecycleDispatch(t, repo)
	if len(remaining) != len(cleared)-5 {
		t.Fatal("family deletion removed the wrong number of credentials")
	}
	for id, view := range remaining {
		if view.Provider == "meta" || !reflect.DeepEqual(cleared[id], view) {
			t.Fatal("family deletion retained removed credentials or changed other families")
		}
	}
	output := t.TempDir()
	if _, errExport := cluster.ExportLocalState(t.Context(), cluster.ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	next, _ := newConfigV8AuthIndexRouter(t)
	if _, errImport := cluster.ImportLocalState(t.Context(), cluster.ImportOptions{Repository: next, ConfigPath: filepath.Join(output, "config.yaml")}); errImport != nil {
		t.Fatal(errImport)
	}
	if got := executionLifecycleDispatch(t, next); !reflect.DeepEqual(remaining, got) {
		t.Fatal("reimport recreated deleted credentials or removed pool settings")
	}
}

func TestConfigV8ExecutionCompatibilityInheritanceRoundTrip(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			repo, request := newConfigV8AuthIndexRouter(t)
			groups := []map[string]any{
				{"name": "gateway", "base-url": "https://example.test", "connection-pool": map[string]any{"enabled": true}, "keys": []any{map[string]any{"id": "11111111-1111-4111-8111-111111111111", "api-key": "first", "connection-pool": map[string]any{"enabled": false}}}},
				{"name": "gateway", "base-url": "https://example.test", "connection-pool": map[string]any{"enabled": false}, "keys": []any{map[string]any{"id": "22222222-2222-4222-8222-222222222222", "api-key": "second"}}},
				{"name": "gateway", "base-url": "https://example.test", "keys": []any{map[string]any{"id": "33333333-3333-4333-8333-333333333333", "api-key": "third", "connection-pool": map[string]any{"enabled": false}}}},
			}
			if reverse {
				slices.Reverse(groups)
			}
			for _, group := range groups {
				group["models"] = []any{map[string]any{"name": "lifecycle-compat", "alias": "lifecycle-compat"}}
			}
			encoded, errEncode := json.Marshal(groups)
			if errEncode != nil {
				t.Fatal(errEncode)
			}
			path := "/v8/management/config/api-keys/openai-compatibility"
			request("PUT", path, string(encoded), http.StatusOK)
			before := executionLifecycleDispatch(t, repo)
			read := request("GET", path, "", http.StatusOK)
			// A GET/PUT and export must preserve each key's inheritance source.
			request("PUT", path, read, http.StatusOK)
			output := t.TempDir()
			if _, errExport := cluster.ExportLocalState(t.Context(), cluster.ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
				t.Fatal(errExport)
			}
			next, _ := newConfigV8AuthIndexRouter(t)
			if _, errImport := cluster.ImportLocalState(t.Context(), cluster.ImportOptions{Repository: next, ConfigPath: filepath.Join(output, "config.yaml")}); errImport != nil {
				t.Fatal(errImport)
			}
			if after := executionLifecycleDispatch(t, next); !reflect.DeepEqual(before, after) {
				t.Fatalf("group merging changed inherited execution options: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestConfigV8PoolScopeWriteExportAndCredentialLifecycle(t *testing.T) {
	for _, family := range []string{"codex", "openai-compatibility"} {
		t.Run(family, func(t *testing.T) {
			repo, request := newConfigV8AuthIndexRouter(t)
			path := "/v8/management/config/api-keys/" + family
			firstID := "11111111-1111-4111-8111-111111111111"
			secondID := "22222222-2222-4222-8222-222222222222"
			first := map[string]any{"id": firstID, "api-key": "first"}
			second := map[string]any{"id": secondID, "api-key": "second", "connection-pool": nil}
			group := map[string]any{"name": "group", "base-url": "https://example.test", "keys": []any{first, second}, "connection-pool": map[string]any{}}
			write := func() {
				t.Helper()
				body, errMarshal := json.Marshal([]any{group})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				request("PUT", path, string(body), http.StatusOK)
			}
			read := func(body string) (map[string]any, map[string]map[string]any) {
				t.Helper()
				var groups []map[string]any
				if errDecode := json.Unmarshal([]byte(body), &groups); errDecode != nil {
					t.Fatal(errDecode)
				}
				if len(groups) != 1 {
					t.Fatalf("expected one group, got %d", len(groups))
				}
				byID := make(map[string]map[string]any)
				for _, raw := range groups[0]["keys"].([]any) {
					key := raw.(map[string]any)
					byID[key["id"].(string)] = key
				}
				return groups[0], byID
			}
			write()
			request("PUT", "/v8/management/config/access/api-keys", `["client-access"]`, http.StatusOK)
			// Moving an empty pool from the group to the individual keys changes only its source.
			if family == "codex" {
				auths, errAuths := repo.ListAuths(t.Context())
				if errAuths != nil {
					t.Fatal(errAuths)
				}
				for _, auth := range auths {
					auth.Metadata["runtime-marker"] = auth.ID
					if _, errUpsert := repo.UpsertAuth(t.Context(), auth, "upsert"); errUpsert != nil {
						t.Fatal(errUpsert)
					}
				}
			}
			delete(group, "connection-pool")
			first["connection-pool"], second["connection-pool"] = map[string]any{}, map[string]any{}
			write()
			got, keys := read(request("GET", path, "", http.StatusOK))
			if family == "codex" {
				auths, errAuths := repo.ListAuths(t.Context())
				if errAuths != nil {
					t.Fatal(errAuths)
				}
				for _, auth := range auths {
					if auth.Metadata["runtime-marker"] != auth.ID {
						t.Fatal("scope-only edit reset live credential state")
					}
				}
			}
			if _, exists := got["connection-pool"]; exists || !reflect.DeepEqual(keys[firstID]["connection-pool"], map[string]any{}) || !reflect.DeepEqual(keys[secondID]["connection-pool"], map[string]any{}) {
				t.Fatal("a scope-only edit was discarded")
			}
			group["connection-pool"] = map[string]any{"idle-conn-timeout": "45s", "max-idle-conns-per-host": 4}
			first["connection-pool"] = map[string]any{"enabled": nil, "max-idle-conns-per-host": 0}
			second["connection-pool"] = map[string]any{"enabled": false}
			write()
			before := request("GET", path, "", http.StatusOK)
			output := t.TempDir()
			if _, errExport := cluster.ExportLocalState(t.Context(), cluster.ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
				t.Fatal(errExport)
			}
			fresh, next := newConfigV8AuthIndexRouter(t)
			if _, errImport := cluster.ImportLocalState(t.Context(), cluster.ImportOptions{Repository: fresh, ConfigPath: filepath.Join(output, "config.yaml")}); errImport != nil {
				t.Fatal(errImport)
			}
			beforeGroup, beforeKeys := read(before)
			afterGroup, afterKeys := read(next("GET", path, "", http.StatusOK))
			if !reflect.DeepEqual(beforeGroup["connection-pool"], afterGroup["connection-pool"]) || !reflect.DeepEqual(beforeKeys, afterKeys) {
				t.Fatal("export/import changed the credential scope or identity")
			}
			if data := next("GET", "/v8/management/config/access/api-keys", "", http.StatusOK); !strings.Contains(data, "client-access") {
				t.Fatal("lost the independent client access keys")
			}
			auths, errAuths := fresh.ListAuths(t.Context())
			if errAuths != nil {
				t.Fatal(errAuths)
			}
			for _, auth := range auths {
				if auth.ID == firstID {
					auth.Attributes["api_key"] = "rotated"
					if _, errUpsert := fresh.UpsertAuth(t.Context(), auth, "upsert"); errUpsert != nil {
						t.Fatal(errUpsert)
					}
				}
			}
			if errDelete := fresh.SoftDeleteAuth(t.Context(), secondID); errDelete != nil {
				t.Fatal(errDelete)
			}
			_, live := read(next("GET", path, "", http.StatusOK))
			if len(live) != 1 || live[firstID]["api-key"] != "rotated" || !reflect.DeepEqual(live[firstID]["connection-pool"], beforeKeys[firstID]["connection-pool"]) {
				t.Fatal("group source did not respect live credential rotation or deletion")
			}
		})
	}
}

func TestConfigV8PoolScopesKeepDistinctIdenticalGroups(t *testing.T) {
	for _, family := range []string{"claude", "openai-compatibility"} {
		t.Run(family, func(t *testing.T) {
			repo, request := newConfigV8AuthIndexRouter(t)
			path := "/v8/management/config/api-keys/" + family
			group := func(key string) map[string]any {
				return map[string]any{"name": "duplicate", "base-url": "https://example.test", "connection-pool": map[string]any{}, "keys": []any{map[string]any{"api-key": key, "connection-pool": nil}}}
			}
			body, errMarshal := json.Marshal([]any{group("first"), group("second")})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			request("PUT", path, string(body), http.StatusOK)
			saved := request("GET", path, "", http.StatusOK)
			request("PUT", path, saved, http.StatusOK)
			var groups []map[string]any
			if errDecode := json.Unmarshal([]byte(request("GET", path, "", http.StatusOK)), &groups); errDecode != nil {
				t.Fatal(errDecode)
			}
			if len(groups) != 2 {
				t.Fatalf("distinct submitted groups collapsed into %d groups", len(groups))
			}
			for _, item := range groups {
				if len(item["keys"].([]any)) != 1 {
					t.Fatal("key migrated between groups")
				}
			}
			// Independently authored groups also remain distinct if a live edit makes their endpoints equal.
			groups[0]["base-url"] = "https://alternate.test"
			updated, errEncode := json.Marshal(groups)
			if errEncode != nil {
				t.Fatal(errEncode)
			}
			request("PUT", path, string(updated), http.StatusOK)
			auths, errAuths := repo.ListAuths(t.Context())
			if errAuths != nil {
				t.Fatal(errAuths)
			}
			for _, auth := range auths {
				if auth.Attributes["base_url"] == "https://alternate.test" {
					auth.Attributes["base_url"] = "https://example.test"
					if _, errUpsert := repo.UpsertAuth(t.Context(), auth, "upsert"); errUpsert != nil {
						t.Fatal(errUpsert)
					}
				}
			}
			if errDecode := json.Unmarshal([]byte(request("GET", path, "", http.StatusOK)), &groups); errDecode != nil {
				t.Fatal(errDecode)
			}
			if len(groups) != 2 || groups[0]["base-url"] != groups[1]["base-url"] {
				t.Fatal("live endpoint change merged two source groups")
			}
		})
	}
}
