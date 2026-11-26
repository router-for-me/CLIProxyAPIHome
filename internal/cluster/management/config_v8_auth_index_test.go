package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gopkg.in/yaml.v3"
)

func newConfigV8AuthIndexRouter(t *testing.T) (*cluster.Repository, func(method, path, body string, status int) string) {
	t.Helper()
	db, cleanup := openManagementLogTestDB(t)
	t.Cleanup(cleanup)
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
		router.Handle(method, "/v8/management/config", h.ConfigV8)
	}
	router.GET("/v8/management/config.yaml", h.ConfigV8)
	router.PUT("/v8/management/config.yaml", h.ConfigV8)
	router.POST("/v8/management/requests/api-call", h.APICall)
	return repo, func(method, path, body string, status int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
		}
		return rec.Body.String()
	}
}

func TestConfigV8AuthIndexesCoverAllNativeProviders(t *testing.T) {
	repo, request := newConfigV8AuthIndexRouter(t)
	providers := map[string]string{
		"gemini": "gemini", "interactions": "gemini-interactions", "vertex": "vertex",
		"codex": "codex", "claude": "claude", "xai": "xai", "meta": "meta",
	}
	for family := range providers {
		request("PUT", "/v8/management/config/api-keys/"+family,
			`[{"base-url":"https://example.test","keys":[{"api-key":"shared-test-key"}]}]`, http.StatusOK)
	}
	auths, errAuths := repo.ListAuths(t.Context())
	if errAuths != nil || len(auths) != len(providers) {
		t.Fatalf("credentials = %d, error = %v", len(auths), errAuths)
	}
	idsByProvider := make(map[string]string, len(auths))
	for _, auth := range auths {
		idsByProvider[auth.Provider] = auth.ID
	}
	var tree map[string][]struct {
		Keys []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
		} `json:"keys"`
	}
	read := request("GET", "/v8/management/config/api-keys", "", http.StatusOK)
	if errDecode := json.Unmarshal([]byte(read), &tree); errDecode != nil {
		t.Fatal(errDecode)
	}
	for family, provider := range providers {
		groups := tree[family]
		if len(groups) != 1 || len(groups[0].Keys) != 1 {
			t.Errorf("%s projected groups = %+v", family, groups)
			continue
		}
		key := groups[0].Keys[0]
		if expected := idsByProvider[provider]; expected == "" || key.ID != expected || key.AuthIndex != expected {
			t.Errorf("%s key = %+v, want credential %q", family, key, expected)
		}
	}
}

func TestConfigV8AuthIndexWorksForStoredCredentialAPICall(t *testing.T) {
	_, request := newConfigV8AuthIndexRouter(t)
	authorization := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if _, errWrite := w.Write([]byte(`{"data":[{"id":"test-model"}]}`)); errWrite != nil {
			t.Errorf("write model catalog: %v", errWrite)
		}
	}))
	t.Cleanup(upstream.Close)
	request("PUT", "/v8/management/config/api-keys/codex",
		`[{"base-url":"https://example.test","keys":[{"api-key":"first-test-key"},{"api-key":"second-test-key"}]}]`, http.StatusOK)
	var groups []struct {
		Keys []struct {
			APIKey    string `json:"api-key"`
			AuthIndex string `json:"auth_index"`
		} `json:"keys"`
	}
	if errDecode := json.Unmarshal([]byte(request("GET", "/v8/management/config/api-keys/codex", "", http.StatusOK)), &groups); errDecode != nil {
		t.Fatal(errDecode)
	}
	authIndex := ""
	for _, group := range groups {
		for _, key := range group.Keys {
			if key.APIKey == "second-test-key" {
				authIndex = key.AuthIndex
			}
		}
	}
	if authIndex == "" {
		t.Fatal("saved key has no auth_index")
	}
	body, errMarshal := json.Marshal(map[string]any{
		"auth_index": authIndex, "method": "GET", "url": upstream.URL + "/models",
		"header": map[string]string{"Authorization": "Bearer $TOKEN$"},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var response apiCallResponse
	if errDecode := json.Unmarshal([]byte(request("POST", "/v8/management/requests/api-call", string(body), http.StatusOK)), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Body, "test-model") {
		t.Fatalf("model fetch response = %+v", response)
	}
	select {
	case got := <-authorization:
		if got != "Bearer second-test-key" {
			t.Fatalf("api-call selected the wrong stored credential: %q", got)
		}
	default:
		t.Fatal("model catalog was not fetched")
	}
}

func TestConfigV8AuthIndexRoundTripAcrossWriteScopes(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		for _, path := range []string{"/v8/management/config", "/v8/management/config/api-keys", "/v8/management/config/api-keys/codex"} {
			t.Run(method+path, func(t *testing.T) {
				repo, request := newConfigV8AuthIndexRouter(t)
				request("PUT", "/v8/management/config/api-keys/codex",
					`[{"base-url":"https://example.test","auth_index":"ignored-group","keys":[{"api-key":"test-key","auth-index":"ignored-key"}],"headers":{"auth_index":"keep-header"}}]`, http.StatusOK)
				before, errBefore := repo.ListAuths(t.Context())
				if errBefore != nil || len(before) != 1 {
					t.Fatalf("credentials = %d, error = %v", len(before), errBefore)
				}
				read := request("GET", path, "", http.StatusOK)
				request(method, path, read, http.StatusOK)
				after, errAfter := repo.ListAuths(t.Context())
				if errAfter != nil || len(after) != 1 || after[0].ID != before[0].ID {
					t.Fatalf("read/write changed credential identity: %+v, %v", after, errAfter)
				}
				options, _ := after[0].Metadata["credential_options"].(map[string]any)
				for _, field := range configV8AuthIndexFields {
					if _, stored := options[field]; stored {
						t.Errorf("read-only %s became credential configuration", field)
					}
				}
				result := request("GET", "/v8/management/config/api-keys/codex", "", http.StatusOK)
				if !strings.Contains(result, `"auth_index":"keep-header"`) || !strings.Contains(result, `"auth_index":"`+before[0].ID+`"`) {
					t.Fatalf("round trip lost the header or credential index: %s", result)
				}
			})
		}
	}
}

func TestConfigV8AuthIndexProjectionOverridesStaleFields(t *testing.T) {
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal([]byte(`api-keys: {codex: [{keys: [{id: saved-id, auth_index: stale-id, auth-index: another-stale-id}]}]}`), &document); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	root := document.Content[0]
	injectConfigV8APIKeyAuthIndexes(root)
	key := configV8Node(root, []string{"api-keys", "codex"}).Content[0]
	key = configV8Node(key, []string{"keys"}).Content[0]
	if index := configV8Node(key, []string{"auth_index"}); index == nil || index.Value != "saved-id" {
		t.Fatalf("projection trusted a stale auth index: %+v", index)
	}
	if configV8Node(key, []string{"auth-index"}) != nil {
		t.Fatal("projection retained a stale alternate spelling")
	}
}

func TestConfigV8AuthIndexYAMLWriteOmitsRuntimeFields(t *testing.T) {
	_, request := newConfigV8AuthIndexRouter(t)
	request("PUT", "/v8/management/config/api-keys/codex",
		`[{"base-url":"https://example.test","keys":[{"api-key":"test-key"}]}]`, http.StatusOK)
	read := request("GET", "/v8/management/config", "", http.StatusOK)
	var root map[string]any
	if errDecode := json.Unmarshal([]byte(read), &root); errDecode != nil {
		t.Fatal(errDecode)
	}
	data, errMarshal := yaml.Marshal(root)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if !strings.Contains(string(data), "auth_index:") {
		t.Fatal("JSON projection did not include an auth index")
	}
	request("PUT", "/v8/management/config.yaml", string(data), http.StatusOK)
	if exported := request("GET", "/v8/management/config.yaml", "", http.StatusOK); strings.Contains(exported, "auth_index:") {
		t.Fatalf("uploaded runtime field was exported as configuration: %s", exported)
	}
}

func TestConfigV8APIKeysExposeAuthIndex(t *testing.T) {
	repo, request := newConfigV8AuthIndexRouter(t)
	request("PUT", "/v8/management/config/api-keys/codex",
		`[{"base-url":"https://example.test","headers":{"auth-index":"kept"},"keys":[{"api-key":"key-one"},{"api-key":"key-two"}]}]`, 200)
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 2 {
		t.Fatalf("credentials: count=%d err=%v", len(auths), errAuths)
	}

	read := request("GET", "/v8/management/config/api-keys/codex", "", 200)
	// Home projects one group per credential, so the two keys come back as two
	// groups of one key each.
	var groups []map[string]any
	if errDecode := json.Unmarshal([]byte(read), &groups); errDecode != nil {
		t.Fatalf("decode groups: %v: %s", errDecode, read)
	}
	keyCount := 0
	for _, group := range groups {
		if _, onGroup := group["auth_index"]; onGroup {
			t.Fatalf("a group with keys carries auth_index: %v", group)
		}
		keys, _ := group["keys"].([]any)
		for _, raw := range keys {
			keyCount++
			key, _ := raw.(map[string]any)
			id, _ := key["id"].(string)
			if id == "" || key["auth_index"] != id {
				t.Fatalf("key auth_index = %v, want its id %q", key["auth_index"], id)
			}
		}
	}
	if keyCount != 2 {
		t.Fatalf("keys = %d, want 2: %s", keyCount, read)
	}

	// The whole-config read carries it too; the YAML export is configuration only.
	if whole := request("GET", "/v8/management/config", "", 200); !strings.Contains(whole, `"auth_index"`) {
		t.Fatalf("full config read lacks auth_index: %s", whole)
	}
	if exported := request("GET", "/v8/management/config.yaml", "", 200); strings.Contains(exported, "auth_index") {
		t.Fatalf("yaml export leaks auth_index: %s", exported)
	}

	// Sending a read straight back is accepted and stores nothing new.
	request("PUT", "/v8/management/config/api-keys/codex", read, 200)
	request("PUT", "/v8/management/config/api-keys/codex", strings.ReplaceAll(read, `"auth_index"`, `"auth-index"`), 200)
	again := request("GET", "/v8/management/config/api-keys/codex", "", 200)
	if strings.Count(again, `"auth_index"`) != 2 || strings.Contains(again, `"auth-index":"codex`) {
		t.Fatalf("round trip duplicated or stored auth_index: %s", again)
	}
	if !strings.Contains(again, `"auth-index":"kept"`) {
		t.Fatalf("a header named auth-index was dropped: %s", again)
	}
	auths, errAuths = repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 2 {
		t.Fatalf("round trip changed credentials: count=%d err=%v", len(auths), errAuths)
	}
	for _, auth := range auths {
		options, _ := auth.Metadata["credential_options"].(map[string]any)
		for _, field := range configV8AuthIndexFields {
			if _, stored := options[field]; stored {
				t.Fatalf("credential %s stored %s as configuration", auth.ID, field)
			}
		}
	}
}

func TestConfigV8KeylessCompatibilityGroupExposesAuthIndex(t *testing.T) {
	_, request := newConfigV8AuthIndexRouter(t)
	request("PUT", "/v8/management/config/api-keys/openai-compatibility",
		`[{"name":"keyless","base-url":"https://keyless.example.test/v1","keys":[]},{"name":"keyed","base-url":"https://keyed.example.test/v1","keys":[{"api-key":"key-one"}]}]`, 200)
	read := request("GET", "/v8/management/config/api-keys/openai-compatibility", "", 200)
	var groups []map[string]any
	if errDecode := json.Unmarshal([]byte(read), &groups); errDecode != nil {
		t.Fatalf("decode: %v: %s", errDecode, read)
	}
	byName := map[string]map[string]any{}
	for _, group := range groups {
		name, _ := group["name"].(string)
		byName[name] = group
	}
	keyless := byName["keyless"]
	if keyless == nil {
		t.Fatalf("keyless group missing: %s", read)
	}
	if id, _ := keyless["id"].(string); id == "" || keyless["auth_index"] != id {
		t.Fatalf("keyless group auth_index = %v, id = %v", keyless["auth_index"], keyless["id"])
	}
	keyed := byName["keyed"]
	keys, _ := keyed["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keyed group keys = %v", keyed["keys"])
	}
	key, _ := keys[0].(map[string]any)
	if id, _ := key["id"].(string); id == "" || key["auth_index"] != id {
		t.Fatalf("compat key auth_index = %v, id = %v", key["auth_index"], key["id"])
	}
	request("PUT", "/v8/management/config/api-keys/openai-compatibility", read, 200)
}
