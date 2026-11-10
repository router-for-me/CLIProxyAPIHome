package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigV8DatabaseRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
	}
	router.GET("/v8/management/config", h.ConfigV8)
	router.PATCH("/v8/management/config", h.ConfigV8)
	router.PUT("/v0/management/request-retry", h.PutRequestRetry)
	router.GET("/v0/management/config", h.GetConfig)
	router.GET("/v0/management/codex-api-key", h.GetCodexKeys)
	router.PUT("/v0/management/antigravity", h.PutConfigRoot("/antigravity"))
	router.DELETE("/v0/management/antigravity", h.DeleteConfigRoot("/antigravity"))
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
		}
		return rec
	}
	request("PUT", "/v8/management/config/routing/retry/request-retry", "0", 200)
	request("PUT", "/v0/management/request-retry", `{"value":2}`, 200)
	if got := request("GET", "/v8/management/config/routing/retry/request-retry", "", 200).Body.String(); got != "2" {
		t.Fatalf("legacy write invisible to v8: %s", got)
	}
	request("PUT", "/v8/management/config/api-keys/codex", `[{"name":"team","base-url":"https://example.test","request-retry":3,"disable-cooling":true,"keys":[{"api-key":"fixture-key","request-retry":0,"disable-cooling":false,"weight":2,"disable-codex-cloaking":false}]}]`, 200)
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 1 {
		t.Fatalf("credential reconciliation: count=%d err=%v", len(auths), errAuths)
	}
	id := auths[0].ID
	if auths[0].Attributes["weight"] != "2" || auths[0].Attributes["codex_disable_cloaking"] != "false" {
		t.Fatal("credential execution options were dropped")
	}
	for _, path := range []string{"/v0/management/codex-api-key", "/v8/management/config/api-keys/codex"} {
		body := request("GET", path, "", 200).Body.String()
		for _, fragment := range []string{`"request-retry":0`, `"disable-cooling":false`, `"weight":2`, `"disable-codex-cloaking":false`, id} {
			if !strings.Contains(body, fragment) {
				t.Fatalf("%s lost %s: %s", path, fragment, body)
			}
		}
	}
	request("PUT", "/v8/management/config/oauth/providers/antigravity", `{"sensitive-words":["before"]}`, 200)
	request("PUT", "/v0/management/antigravity", `{"sensitive-words":123}`, 422)
	request("PUT", "/v0/management/antigravity", `{"sensitive-words":["after"]}`, 200)
	if got := request("GET", "/v8/management/config/oauth/providers/antigravity/sensitive-words", "", 200).Body.String(); got != `["after"]` {
		t.Fatalf("v0 scoped update = %s", got)
	}
	cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errConfig != nil || !cfg.OAuthOnlyFields["antigravity.sensitive-words"] {
		t.Fatalf("scope lost: %v", errConfig)
	}
	request("DELETE", "/v0/management/antigravity", "", 200)
	request("GET", "/v8/management/config/oauth/providers/antigravity/sensitive-words", "", 404)

	before, errBefore := repo.LoadConfigSnapshot(context.Background())
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	for _, body := range []string{`{"debug":true}`, `{"routing":{"retry":{"typo":1}}}`, `{"credentials":{"concurrency":{"lifecycle-config-revision":999}}}`} {
		request("PATCH", "/v8/management/config", body, 400)
	}
	after, errAfter := repo.LoadConfigSnapshot(context.Background())
	if errAfter != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected writes changed snapshot: %v", errAfter)
	}
	request("DELETE", "/v8/management/config/api-keys/codex", "", 200)
	auths, errAuths = repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 0 {
		t.Fatalf("deleted credential remains: %v", errAuths)
	}
	request("PUT", "/v8/management/config/api-keys/openai-compatibility", `[{"name":"compatible","base-url":"https://example.test/v1","support-prompt-cache-key":true,"keys":[{"api-key":"key-one","weight":2},{"api-key":"key-two","weight":3}]}]`, 200)
	compatible := request("GET", "/v8/management/config/api-keys/openai-compatibility", "", 200).Body.String()
	for _, fragment := range []string{`"weight":2`, `"weight":3`, `"support-prompt-cache-key":true`} {
		if !strings.Contains(compatible, fragment) {
			t.Fatalf("compatibility group lost %s: %s", fragment, compatible)
		}
	}
	request("PUT", "/v8/management/config/management/secret-key", `"v8-test-secret"`, 200)
	snapshot, errSnapshot := repo.LoadConfigSnapshot(context.Background())
	if errSnapshot != nil || strings.Contains(string(snapshot["remote-management"]), "v8-test-secret") {
		t.Fatalf("management secret was not hashed at the persistence boundary: %v", errSnapshot)
	}
}

func TestConfigV8TURNSecretsAndReadOnlyGET(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/config", h.ConfigV8)
	engine.PATCH("/config", h.ConfigV8)
	engine.GET("/config.yaml", h.ConfigV8)
	payload := `{"oauth":{"providers":{"codex":{"live-media-relay":{"ice-servers":[{"urls":["turn:example.test:3478"],"username":"fixture-user","credential":"fixture-secret"}]}}}}}`
	write := httptest.NewRecorder()
	engine.ServeHTTP(write, httptest.NewRequest("PATCH", "/config", strings.NewReader(payload)))
	if write.Code != 200 {
		t.Fatalf("write: %d %s", write.Code, write.Body.String())
	}
	before, _ := repo.LoadConfigSnapshot(context.Background())
	read := httptest.NewRecorder()
	engine.ServeHTTP(read, httptest.NewRequest("GET", "/config", nil))
	if read.Code != 200 || strings.Contains(read.Body.String(), "fixture-secret") || strings.Contains(read.Body.String(), "fixture-user") {
		t.Fatal("JSON leaked TURN secrets or failed")
	}
	if !json.Valid(read.Body.Bytes()) {
		t.Fatal("invalid JSON")
	}
	after, _ := repo.LoadConfigSnapshot(context.Background())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("GET mutated persisted configuration")
	}
	write = httptest.NewRecorder()
	engine.ServeHTTP(write, httptest.NewRequest("PATCH", "/config", strings.NewReader(read.Body.String())))
	if write.Code != 200 {
		t.Fatalf("round trip: %d %s", write.Code, write.Body.String())
	}
	read = httptest.NewRecorder()
	engine.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/config.yaml", nil))
	if read.Code != 200 || !strings.Contains(read.Body.String(), "fixture-secret") {
		t.Fatal("redacted JSON round trip lost TURN credentials")
	}
}

func TestConfigV8OAuthModelAliasRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	router.GET("/v8/management/config/*path", h.ConfigV8)
	router.PUT("/v0/management/oauth-model-alias", h.PutOAuthModelAlias)
	router.PATCH("/v0/management/oauth-model-alias", h.PatchOAuthModelAlias)
	router.GET("/v0/management/oauth-model-alias", h.GetOAuthModelAlias)
	request := func(t *testing.T, method, path, body string) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	want := map[string][]appconfig.OAuthModelAlias{
		"codex": {
			{Name: "upstream", Alias: "friendly", DisplayName: "Friendly Model", Fork: true, ForceMapping: true},
			{Name: "legacy-upstream", Alias: "legacy-alias"},
		},
	}
	const aliases = `[{"name":"upstream","alias":"friendly","display-name":" Friendly Model ","fork":true,"force-mapping":true},{"name":"legacy-upstream","alias":"legacy-alias"}]`
	for _, write := range []struct {
		name, method, path, body string
	}{
		{"v8-put", http.MethodPut, "/v8/management/config/oauth/model-alias", `{"codex":` + aliases + `}`},
		{"v0-put", http.MethodPut, "/v0/management/oauth-model-alias", `{"codex":` + aliases + `}`},
		{"v0-patch", http.MethodPatch, "/v0/management/oauth-model-alias", `{"channel":"codex","aliases":` + aliases + `}`},
	} {
		t.Run(write.name, func(t *testing.T) {
			request(t, write.method, write.path, write.body)
			cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(context.Background())
			if errConfig != nil {
				t.Fatal(errConfig)
			}
			if !reflect.DeepEqual(cfg.OAuthModelAlias, want) {
				t.Errorf("runtime aliases = %#v, want %#v", cfg.OAuthModelAlias, want)
			}
			var legacy struct {
				Aliases map[string][]appconfig.OAuthModelAlias `json:"oauth-model-alias"`
			}
			if errDecode := json.Unmarshal(request(t, http.MethodGet, "/v0/management/oauth-model-alias", ""), &legacy); errDecode != nil {
				t.Fatal(errDecode)
			}
			if !reflect.DeepEqual(legacy.Aliases, want) {
				t.Errorf("v0 aliases = %#v, want %#v", legacy.Aliases, want)
			}
			data, errMarshal := json.Marshal(legacy.Aliases)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			request(t, http.MethodPut, "/v0/management/oauth-model-alias", string(data))
			var current map[string][]appconfig.OAuthModelAlias
			if errDecode := json.Unmarshal(request(t, http.MethodGet, "/v8/management/config/oauth/model-alias", ""), &current); errDecode != nil {
				t.Fatal(errDecode)
			}
			if !reflect.DeepEqual(current, want) {
				t.Errorf("v0 round trip changed persisted aliases: %#v, want %#v", current, want)
			}
		})
	}
}

func TestConfigV8CleansUnknownLegacySectionsOnWrite(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	ctx := context.Background()
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, cluster.DefaultHeartbeatTimeout()); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	seed := map[string]any{
		"home":           map[string]any{"enabled": true, "host": "ignored.example"},
		"former-feature": map[string]any{"items": []any{"first", "second"}, "token": "fixture-secret"},
		"proxy-url":      "old",
		"tls":            map[string]any{},
		"user-email":     map[string]any{},
	}
	if errSeed := repo.ReplaceConfigSnapshot(ctx, seed); errSeed != nil {
		t.Fatal(errSeed)
	}
	// Seed retired fields directly to represent a database written by older Home versions.
	for key, value := range map[string]any{
		"codex":   map[string]any{"identity-confuse": true},
		"routing": map[string]any{"claude-code-session-affinity": true},
	} {
		if errSeed := repo.UpsertConfigValue(ctx, key, value); errSeed != nil {
			t.Fatal(errSeed)
		}
	}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
		router.Handle(method, "/v8/management/config", h.ConfigV8)
	}
	router.GET("/v8/management/config.yaml", h.ConfigV8)
	router.PUT("/v8/management/config.yaml", h.ConfigV8)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
	}
	router.PUT("/v0/management/request-retry", h.PutRequestRetry)
	request := func(method, path, body string, status int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		if rec.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	before, errBefore := repo.LoadConfigSnapshot(ctx)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	jsonView := request(http.MethodGet, "/v8/management/config", "", http.StatusOK)
	if strings.Contains(jsonView, "former-feature") || strings.Contains(jsonView, "fixture-secret") {
		t.Fatal("unknown sections remained active in the JSON view")
	}
	for _, removed := range []string{"identity-confuse", "claude-code-session-affinity"} {
		if strings.Contains(jsonView, removed) {
			t.Fatalf("retired field %s remained in the v8 view", removed)
		}
	}
	if got := request(http.MethodGet, "/v8/management/config/routing/session-affinity", "", http.StatusOK); got != "true" {
		t.Fatalf("legacy affinity was lost: %s", got)
	}
	request(http.MethodGet, "/v8/management/config/former-feature", "", http.StatusNotFound)
	request(http.MethodDelete, "/v8/management/config/former-feature", "", http.StatusNotFound)
	if yamlView := request(http.MethodGet, "/v8/management/config.yaml", "", http.StatusOK); strings.Contains(yamlView, "former-feature") || strings.Contains(yamlView, "fixture-secret") {
		t.Fatal("YAML view retained ignored values as comments")
	}
	for _, body := range []string{`{"home":{"enabled":true}}`, `{"unknown-setting":true}`, `{"server":{"unknown-field":true}}`, `{"server":{"port":"invalid"}}`, `{"oauth":{"providers":{"codex":{"identity-confuse":true}}}}`, `{"routing":{"claude-code-session-affinity":false}}`} {
		request(http.MethodPatch, "/v8/management/config", body, http.StatusBadRequest)
	}
	afterRejected, errRejected := repo.LoadConfigSnapshot(ctx)
	if errRejected != nil || !reflect.DeepEqual(before, afterRejected) {
		t.Fatalf("read or rejected edit changed the database: %v", errRejected)
	}
	for _, edit := range []struct{ method, path, body string }{
		{http.MethodPut, "/v8/management/config/requests/proxy-url", `"direct"`},
		{http.MethodPut, "/v8/management/config/server/tls", `{"enable":false,"cert":"fixture-cert"}`},
		{http.MethodPut, "/v0/management/request-retry", `{"value":2}`},
		{http.MethodDelete, "/v8/management/config/requests/proxy-url", ""},
	} {
		request(edit.method, edit.path, edit.body, http.StatusOK)
		yamlView := request(http.MethodGet, "/v8/management/config.yaml", "", http.StatusOK)
		var doc yaml.Node
		if errDecode := yaml.Unmarshal([]byte(yamlView), &doc); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, key := range []string{"home", "former-feature"} {
			if configV8Node(doc.Content[0], []string{key}) != nil || strings.Contains(yamlView, key+":") {
				t.Fatalf("%s retained ignored section %s", edit.path, key)
			}
		}
		request(http.MethodPut, "/v8/management/config.yaml", yamlView, http.StatusOK)
	}
	snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	for _, key := range []string{"home", "former-feature"} {
		if _, exists := snapshot[key]; exists {
			t.Fatalf("ignored section %s remained in the database after a successful write", key)
		}
	}
	if strings.Contains(string(snapshot["codex"]), "identity-confuse") || strings.Contains(string(snapshot["routing"]), "claude-code-session-affinity") {
		t.Fatal("retired fields remained persisted after a successful write")
	}
	if _, exists := snapshot["proxy-url"]; exists {
		t.Fatal("deleted known setting was restored")
	}
	if got := request(http.MethodGet, "/v8/management/config/server/tls/cert", "", http.StatusOK); got != `"fixture-cert"` {
		t.Fatalf("known empty section edit was overwritten: %s", got)
	}
	if got := request(http.MethodGet, "/v8/management/config/routing/retry/request-retry", "", http.StatusOK); got != "2" {
		t.Fatalf("v0 edit was lost: %s", got)
	}
}
