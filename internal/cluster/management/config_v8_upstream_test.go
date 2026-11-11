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
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

func TestConfigV8SharedUpstreamDatabaseRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	router.PATCH("/v8/management/config", h.ConfigV8)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
	}
	request := func(method, path, body string, want int) string {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, "/v8/management/config/"+path, strings.NewReader(body)))
		if response.Code != want {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.Code, want, response.Body.String())
		}
		return response.Body.String()
	}
	request(http.MethodPut, "upstream/codex", `{"stream-bootstrap-buffering":true,"response-steering":true}`, http.StatusOK)
	request(http.MethodPut, "oauth/auth-auto-refresh-workers", "3", http.StatusOK)
	request(http.MethodPut, "oauth/providers/codex/header-defaults", `{"user-agent":"oauth-agent"}`, http.StatusOK)
	if got := request(http.MethodGet, "upstream/codex/stream-bootstrap-buffering", "", http.StatusOK); got != "true" {
		t.Fatalf("shared provider value was not persisted: %s", got)
	}
	cfg, _, errLoad := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if !cfg.Codex.StreamBootstrapBuffering || !cfg.Codex.ResponseSteering || cfg.AuthAutoRefreshWorkers != 3 || cfg.OAuthOnlyFields["codex.response-steering"] || !cfg.OAuthOnlyFields["codex-header-defaults.user-agent"] {
		t.Fatal("database projection changed shared values or OAuth-only header scope")
	}
	request(http.MethodPut, "upstream/codex/response-steering", "false", http.StatusOK)
	request(http.MethodPut, "oauth/providers/codex/response-steering", "true", http.StatusOK)
	if got := request(http.MethodGet, "upstream/codex/response-steering", "", http.StatusOK); got != "true" {
		t.Fatal("historical path did not update the shared field")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(`{"oauth":{"providers":{"codex":{"response-steering":false}}}}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("historical request body failed: %s", response.Body.String())
	}
	if got := request(http.MethodGet, "upstream/codex/response-steering", "", http.StatusOK); got != "false" {
		t.Fatal("historical request body did not replace the canonical value")
	}
	request(http.MethodDelete, "upstream/codex", "", http.StatusOK)
	cfg, _, errLoad = repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil || cfg.Codex.StreamBootstrapBuffering || cfg.Codex.ResponseSteering || cfg.CodexHeaderDefaults.UserAgent != "oauth-agent" {
		t.Fatalf("deleting shared settings changed unrelated OAuth headers: %v", errLoad)
	}
}

func TestConfigV8SensitiveWordsReplaceDatabaseRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
	}
	request := func(method, path, body string, want int) string {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, "/v8/management/config/"+path, strings.NewReader(body)))
		if response.Code != want {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.Code, want, response.Body.String())
		}
		return response.Body.String()
	}

	request(http.MethodPut, "oauth/providers/antigravity/sensitive-words-replace", `{"You are Codex, an agent based on GPT-5.":"You are a helpful coding assistant.","DropOnly":""}`, http.StatusOK)
	request(http.MethodPut, "oauth/providers/devin/sensitive-words-replace", `{"Claude Code":"Devin"}`, http.StatusOK)

	if got := request(http.MethodGet, "oauth/providers/antigravity/sensitive-words-replace", "", http.StatusOK); !strings.Contains(got, "You are a helpful coding assistant.") || !strings.Contains(got, `"DropOnly":""`) {
		t.Fatalf("GET antigravity sensitive-words-replace = %s", got)
	}

	cfg, _, errLoad := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.Antigravity.SensitiveWordsReplace["You are Codex, an agent based on GPT-5."] != "You are a helpful coding assistant." {
		t.Fatalf("Antigravity.SensitiveWordsReplace = %#v", cfg.Antigravity.SensitiveWordsReplace)
	}
	if val, ok := cfg.Antigravity.SensitiveWordsReplace["DropOnly"]; !ok || val != "" {
		t.Fatalf("Antigravity empty deletion lost: %#v", cfg.Antigravity.SensitiveWordsReplace)
	}
	if cfg.Devin.SensitiveWordsReplace["Claude Code"] != "Devin" {
		t.Fatalf("Devin.SensitiveWordsReplace = %#v", cfg.Devin.SensitiveWordsReplace)
	}
	if !cfg.OAuthOnlyFields["antigravity.sensitive-words-replace"] || !cfg.OAuthOnlyFields["devin.sensitive-words-replace"] {
		t.Fatalf("OAuthOnlyFields missing sensitive-words-replace: %#v", cfg.OAuthOnlyFields)
	}

	request(http.MethodDelete, "oauth/providers/antigravity/sensitive-words-replace/DropOnly", "", http.StatusOK)
	cfg2, _, errLoad2 := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad2 != nil {
		t.Fatal(errLoad2)
	}
	if _, exists := cfg2.Antigravity.SensitiveWordsReplace["DropOnly"]; exists {
		t.Fatalf("deleted key DropOnly persisted: %#v", cfg2.Antigravity.SensitiveWordsReplace)
	}

	request(http.MethodDelete, "oauth/providers/devin/sensitive-words-replace", "", http.StatusOK)
	request(http.MethodGet, "oauth/providers/devin/sensitive-words-replace", "", http.StatusNotFound)

	// Claude API-key cloak persistence, export, and downstream CPA dispatch projection.
	request(
		http.MethodPut,
		"api-keys/claude",
		`[{"name":"claude-team","keys":[{"api-key":"sk-ant-cloak-1","cloak":{"mode":"always","strict-mode":true,"sensitive-words":["API","proxy"],"sensitive-words-replace":{"Claude Code":"Claude","DropOnly":""},"cache-user-id":true}}]}]`,
		http.StatusOK,
	)
	claudeRead := request(http.MethodGet, "api-keys/claude", "", http.StatusOK)
	for _, fragment := range []string{`"Claude Code":"Claude"`, `"DropOnly":""`, `"strict-mode":true`, `"cache-user-id":true`} {
		if !strings.Contains(claudeRead, fragment) {
			t.Fatalf("GET api-keys/claude missing %s: %s", fragment, claudeRead)
		}
	}

	auths, errList := repo.ListAuths(context.Background())
	if errList != nil || len(auths) != 1 {
		t.Fatalf("ListAuths() = %d, err = %v", len(auths), errList)
	}
	dispatched := home.SanitizeAuthForDownstream(auths[0])
	if dispatched.Attributes["cloak_mode"] != "always" ||
		dispatched.Attributes["cloak_strict_mode"] != "true" ||
		dispatched.Attributes["cloak_sensitive_words"] != "API,proxy" ||
		dispatched.Attributes["cloak_cache_user_id"] != "true" {
		t.Fatalf("unexpected dispatched cloak attributes: %#v", dispatched.Attributes)
	}
	var attrRepl map[string]string
	if errUnmarshal := json.Unmarshal([]byte(dispatched.Attributes["cloak_sensitive_words_replace"]), &attrRepl); errUnmarshal != nil {
		t.Fatalf("unmarshal cloak_sensitive_words_replace attr: %v (raw=%q)", errUnmarshal, dispatched.Attributes["cloak_sensitive_words_replace"])
	}
	wantClaudeRepl := map[string]string{"Claude Code": "Claude", "DropOnly": ""}
	if !reflect.DeepEqual(attrRepl, wantClaudeRepl) {
		t.Fatalf("dispatched cloak_sensitive_words_replace = %#v, want %#v", attrRepl, wantClaudeRepl)
	}

	// Omitted cloak.mode must not synthesize cloak_mode="auto" (which would override
	// upstream.claude.disable-claude-cloak-mode=true), and sensitive-words containing
	// commas must remain lossless in metadata without comma-splitting in attributes.
	request(
		http.MethodPut,
		"api-keys/claude",
		`[{"name":"claude-team","keys":[{"api-key":"sk-ant-cloak-1","cloak":{"sensitive-words":["ACME, Inc.","proxy"],"sensitive-words-replace":{"Claude Code":"Claude"}}}]}]`,
		http.StatusOK,
	)
	authsOmittedMode, errListOmitted := repo.ListAuths(context.Background())
	if errListOmitted != nil || len(authsOmittedMode) != 1 {
		t.Fatalf("ListAuths() omitted mode = %d, err = %v", len(authsOmittedMode), errListOmitted)
	}
	dispatchedOmitted := home.SanitizeAuthForDownstream(authsOmittedMode[0])
	if gotMode, hasMode := dispatchedOmitted.Attributes["cloak_mode"]; hasMode {
		t.Fatalf("omitted cloak.mode unexpectedly set cloak_mode=%q", gotMode)
	}
	if gotWordsAttr, hasWordsAttr := dispatchedOmitted.Attributes["cloak_sensitive_words"]; hasWordsAttr {
		t.Fatalf("comma-containing sensitive-words must not use lossy CSV attribute, got %q", gotWordsAttr)
	}
	metaWords, okWords := dispatchedOmitted.Metadata["cloak_sensitive_words"].([]any)
	if !okWords || !reflect.DeepEqual(metaWords, []any{"ACME, Inc.", "proxy"}) {
		t.Fatalf("dispatched metadata cloak_sensitive_words = %#v, want [ACME, Inc., proxy]", dispatchedOmitted.Metadata["cloak_sensitive_words"])
	}

	// Clearing cloak on the Claude key removes persisted cloak options and dispatch attributes.
	request(http.MethodPut, "api-keys/claude", `[{"name":"claude-team","keys":[{"api-key":"sk-ant-cloak-1"}]}]`, http.StatusOK)
	authsAfter, errListAfter := repo.ListAuths(context.Background())
	if errListAfter != nil || len(authsAfter) != 1 {
		t.Fatalf("ListAuths() after clear = %d, err = %v", len(authsAfter), errListAfter)
	}
	if _, hasAttr := authsAfter[0].Attributes["cloak_sensitive_words_replace"]; hasAttr {
		t.Fatalf("cleared cloak left cloak_sensitive_words_replace attribute: %#v", authsAfter[0].Attributes)
	}
	claudeReadAfter := request(http.MethodGet, "api-keys/claude", "", http.StatusOK)
	if strings.Contains(claudeReadAfter, "sensitive-words-replace") {
		t.Fatalf("cleared cloak persisted in GET api-keys/claude: %s", claudeReadAfter)
	}
}
