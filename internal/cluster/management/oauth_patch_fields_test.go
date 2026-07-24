package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestApplyOAuthFieldPatchArbitraryFields(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "codex-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
		Metadata: map[string]any{
			"type":       "codex",
			"websockets": true,
		},
	}
	fields := mustRawFields(t, `{"abc":true,"nested.cde":true,"fgh":{"ijk":true},"websockets":false}`)

	changed, errPatch := applyOAuthFieldPatch(auth, fields)
	if errPatch != nil {
		t.Fatalf("applyOAuthFieldPatch returned error: %v", errPatch)
	}
	if !changed {
		t.Fatalf("applyOAuthFieldPatch changed = false, want true")
	}
	if got := auth.Metadata["abc"]; got != true {
		t.Fatalf("metadata.abc = %#v, want true", got)
	}
	nested, ok := auth.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.nested = %#v, want object", auth.Metadata["nested"])
	}
	if got := nested["cde"]; got != true {
		t.Fatalf("metadata.nested.cde = %#v, want true", got)
	}
	fgh, ok := auth.Metadata["fgh"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.fgh = %#v, want object", auth.Metadata["fgh"])
	}
	if got := fgh["ijk"]; got != true {
		t.Fatalf("metadata.fgh.ijk = %#v, want true", got)
	}
	if got, ok := auth.Metadata["websockets"].(bool); !ok || got {
		t.Fatalf("metadata.websockets = %#v, want false", auth.Metadata["websockets"])
	}
	if got := auth.Attributes["websockets"]; got != "false" {
		t.Fatalf("attributes.websockets = %q, want false", got)
	}
}

func TestAuthFileEntryIncludesEditableMetadata(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Prefix:   "team-a",
		ProxyURL: "socks5://127.0.0.1:1080",
		Attributes: map[string]string{
			"priority":   "10",
			"note":       "primary",
			"websockets": "true",
		},
		Metadata: map[string]any{
			"type":       "codex",
			"prefix":     "legacy-team",
			"proxy_url":  "socks5://legacy.example:1080",
			"websockets": false,
		},
	}

	entry := authFileEntry(auth)
	if got := entry["prefix"]; got != "team-a" {
		t.Fatalf("prefix = %#v, want team-a", got)
	}
	if got := entry["proxy_url"]; got != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy_url = %#v, want stored proxy URL", got)
	}
	if got := entry["priority"]; got != 10 {
		t.Fatalf("priority = %#v, want 10", got)
	}
	if got := entry["note"]; got != "primary" {
		t.Fatalf("note = %#v, want primary", got)
	}
	if got := entry["websockets"]; got != true {
		t.Fatalf("websockets = %#v, want true", got)
	}
}

func TestAuthFileEntryUsesLegacyEditableMetadataFallbacks(t *testing.T) {
	for _, test := range []struct {
		name     string
		proxyKey string
	}{
		{name: "underscore proxy key", proxyKey: "proxy_url"},
		{name: "hyphenated proxy key", proxyKey: "proxy-url"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]any{
				"type":       "codex",
				"prefix":     "legacy-team",
				"websockets": true,
			}
			metadata[test.proxyKey] = "socks5://legacy.example:1080"

			entry := authFileEntry(&coreauth.Auth{
				ID:       "legacy-codex-auth",
				Provider: "codex",
				Metadata: metadata,
			})
			if got := entry["prefix"]; got != "legacy-team" {
				t.Fatalf("prefix = %#v, want legacy-team", got)
			}
			if got := entry["proxy_url"]; got != "socks5://legacy.example:1080" {
				t.Fatalf("proxy_url = %#v, want legacy proxy URL", got)
			}
			if got := entry["websockets"]; got != true {
				t.Fatalf("websockets = %#v, want true", got)
			}
		})
	}
}

func TestPatchAuthFileFieldsRoundTripsEditableMetadata(t *testing.T) {
	handler, engine, _, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-metadata")
	engine.GET("/auth-files", handler.ListAuthFiles)

	patchResponse := httptest.NewRecorder()
	patchRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-metadata",
		"prefix":"team-a",
		"proxy-url":"socks5://127.0.0.1:1080",
		"priority":10,
		"note":"primary",
		"websockets":true
	}`))
	patchRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchResponse, patchRequest)
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchResponse.Code, patchResponse.Body.String())
	}

	listResponse := httptest.NewRecorder()
	engine.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/auth-files", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var payload struct {
		Files []struct {
			ID         string `json:"id"`
			Prefix     string `json:"prefix"`
			ProxyURL   string `json:"proxy_url"`
			Priority   int    `json:"priority"`
			Note       string `json:"note"`
			Websockets bool   `json:"websockets"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(listResponse.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files = %#v, want one item", payload.Files)
	}
	file := payload.Files[0]
	if file.ID != "oauth-metadata" || file.Prefix != "team-a" || file.ProxyURL != "socks5://127.0.0.1:1080" || file.Priority != 10 || file.Note != "primary" || !file.Websockets {
		t.Fatalf("file = %#v, want persisted editable metadata", file)
	}

	persisted, _, errAuth := handler.repo.GetAuth(t.Context(), "oauth-metadata")
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Prefix != "team-a" || persisted.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("persisted auth = %#v, want canonical prefix and proxy URL", persisted)
	}
	if persisted.Attributes["priority"] != "10" || persisted.Attributes["note"] != "primary" || persisted.Attributes["websockets"] != "true" {
		t.Fatalf("persisted attributes = %#v, want editable metadata attributes", persisted.Attributes)
	}
}

func TestAuthFileEntryIncludesExplicitEmptyMetadataProjection(t *testing.T) {
	entry := authFileEntry(&coreauth.Auth{
		ID:         "claude-auth",
		Provider:   "claude",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"type": "claude"},
	})

	for _, key := range []string{"prefix", "proxy_url", "websockets"} {
		if _, ok := entry[key]; !ok {
			t.Fatalf("entry does not contain %q: %#v", key, entry)
		}
	}
	if got := entry["prefix"]; got != "" {
		t.Fatalf("prefix = %#v, want empty string", got)
	}
	if got := entry["proxy_url"]; got != "" {
		t.Fatalf("proxy_url = %#v, want empty string", got)
	}
	if got := entry["websockets"]; got != false {
		t.Fatalf("websockets = %#v, want false", got)
	}
}

func mustRawFields(t *testing.T, payload string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(payload), &fields); errUnmarshal != nil {
		t.Fatalf("failed to unmarshal raw fields: %v", errUnmarshal)
	}
	return fields
}
