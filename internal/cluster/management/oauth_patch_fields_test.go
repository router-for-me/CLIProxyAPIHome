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

func TestApplyOAuthFieldPatchCanonicalPrecedenceOverLegacyAliases(t *testing.T) {
	for _, tc := range []struct {
		name      string
		patch     string
		wantRetry int
		wantProxy string
	}{
		{
			name:      "canonical request_retry wins over legacy alias",
			patch:     `{"request-retry":2,"request_retry":3,"proxy_url":"http://127.0.0.1:8080","proxy-url":"http://127.0.0.1:8080"}`,
			wantRetry: 3,
			wantProxy: "http://127.0.0.1:8080",
		},
		{
			name:      "canonical request_retry wins when alias has whitespace",
			patch:     `{" request-retry ":2,"request_retry":4,"proxy-url":"http://127.0.0.1:8080","proxy_url":"http://127.0.0.1:8080"}`,
			wantRetry: 4,
			wantProxy: "http://127.0.0.1:8080",
		},
		{
			name:      "canonical request_retry wins regardless of map iteration order",
			patch:     `{"request_retry":5,"request-retry":1}`,
			wantRetry: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &coreauth.Auth{
				ID:         "codex-auth",
				Provider:   "codex",
				Attributes: map[string]string{"priority": "10"},
				Metadata:   map[string]any{"type": "codex", "request_retry": 1},
			}
			changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, tc.patch))
			if errPatch != nil || !changed {
				t.Fatalf("applyOAuthFieldPatch(%s) = (%t, %v), want successful patch", tc.patch, changed, errPatch)
			}
			if got, ok := auth.RequestRetryOverride(); !ok || got != tc.wantRetry {
				t.Fatalf("request retry override = (%d, %t), want (%d, true)", got, ok, tc.wantRetry)
			}
			if tc.wantProxy != "" {
				if auth.ProxyURL != tc.wantProxy {
					t.Fatalf("auth.ProxyURL = %q, want %q", auth.ProxyURL, tc.wantProxy)
				}
				if _, exists := auth.Metadata["proxy-url"]; exists {
					t.Fatalf("legacy proxy-url remains in metadata: %#v", auth.Metadata)
				}
			}
		})
	}
}

func TestApplyOAuthFieldPatchRejectsConflictingOrOverlappingPaths(t *testing.T) {
	patches := []string{
		`{"request-retry":2," request-retry ":3}`,
		`{"request_retry":2," request_retry ":3}`,
		`{"request_retry":2,"request-retry.child":3}`,
		`{"proxy_url":"http://proxy","proxy_url.child":"value"}`,
	}
	for _, patch := range patches {
		auth := &coreauth.Auth{
			ID:         "codex-auth",
			Provider:   "codex",
			Attributes: map[string]string{"priority": "10"},
			Metadata:   map[string]any{"type": "codex", "request_retry": 1},
		}
		changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, patch))
		if errPatch == nil || changed {
			t.Fatalf("applyOAuthFieldPatch(%s) = (%t, %v), want unchanged conflict/overlap error", patch, changed, errPatch)
		}
		if got, ok := auth.RequestRetryOverride(); !ok || got != 1 {
			t.Fatalf("request retry override after rejected patch %s = (%d, %t), want (1, true)", patch, got, ok)
		}
		if auth.Attributes["priority"] != "10" {
			t.Fatalf("attributes changed after rejected patch %s: %#v", patch, auth.Attributes)
		}
	}
}

func TestApplyOAuthFieldPatchRejectsInvalidRequestRetryAtomically(t *testing.T) {
	patches := []string{
		`{"priority":20,"request-retry":"2"}`,
		`{"priority":20,"request_retry":true}`,
		`{"priority":20,"request-retry":1.5}`,
		`{"priority":20,"request-retry":1e2}`,
		`{"priority":20,"request-retry":{}}`,
		`{"priority":20,"request_retry.child":2}`,
	}
	for _, patch := range patches {
		auth := &coreauth.Auth{
			ID:         "codex-auth",
			Provider:   "codex",
			Attributes: map[string]string{"priority": "10"},
			Metadata:   map[string]any{"type": "codex", "priority": 10, "request_retry": 1},
		}
		changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, patch))
		if errPatch == nil || changed {
			t.Fatalf("applyOAuthFieldPatch(%s) = (%t, %v), want unchanged validation error", patch, changed, errPatch)
		}
		if got, ok := auth.RequestRetryOverride(); !ok || got != 1 {
			t.Fatalf("request retry override after rejected patch %s = (%d, %t), want (1, true)", patch, got, ok)
		}
		if got := auth.Metadata["priority"]; got != 10 || auth.Attributes["priority"] != "10" {
			t.Fatalf("priority changed after rejected patch %s: metadata=%#v attributes=%#v", patch, auth.Metadata, auth.Attributes)
		}
	}
}

func TestApplyOAuthFieldPatchNegativeRequestRetryClearsOverride(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "request_retry": 1, "request-retry": 2},
	}
	changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, `{"request-retry":-1}`))
	if errPatch != nil || !changed {
		t.Fatalf("applyOAuthFieldPatch() = (%t, %v), want successful clear", changed, errPatch)
	}
	if got, ok := auth.RequestRetryOverride(); ok || got != 0 {
		t.Fatalf("request retry override after negative patch = (%d, %t), want inherited", got, ok)
	}
	if _, exists := auth.Metadata["request_retry"]; exists {
		t.Fatalf("canonical request_retry remains after clear: %#v", auth.Metadata)
	}
	if _, exists := auth.Metadata["request-retry"]; exists {
		t.Fatalf("legacy request-retry remains after clear: %#v", auth.Metadata)
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

func TestPatchAuthFileFieldsWithBothProxyUrlAliases(t *testing.T) {
	handler, engine, _, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-proxy-both")
	engine.GET("/auth-files", handler.ListAuthFiles)

	patchResponse := httptest.NewRecorder()
	patchRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-proxy-both",
		"prefix":"team-b",
		"proxy_url":"socks5://127.0.0.1:1080",
		"proxy-url":"socks5://127.0.0.1:1080",
		"priority":5,
		"note":"secondary"
	}`))
	patchRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchResponse, patchRequest)
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchResponse.Code, patchResponse.Body.String())
	}

	persisted, _, errAuth := handler.repo.GetAuth(t.Context(), "oauth-proxy-both")
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Prefix != "team-b" || persisted.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("persisted auth = %#v, want canonical prefix and proxy URL", persisted)
	}
	if _, exists := persisted.Metadata["proxy-url"]; exists {
		t.Fatalf("legacy proxy-url remains in metadata: %#v", persisted.Metadata)
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

func TestAuthFileEntryEmitsDisableCoolingOverride(t *testing.T) {
	t.Run("emits when override is true", func(t *testing.T) {
		entry := authFileEntry(&coreauth.Auth{
			ID:       "codex-auth",
			Provider: "codex",
			Metadata: map[string]any{"type": "codex", "disable_cooling": true},
		})
		if got, ok := entry["disable-cooling"]; !ok || got != true {
			t.Fatalf("disable-cooling = %#v (present=%t), want true", entry["disable-cooling"], ok)
		}
	})
	t.Run("omits when override is unset", func(t *testing.T) {
		entry := authFileEntry(&coreauth.Auth{
			ID:       "codex-auth",
			Provider: "codex",
			Metadata: map[string]any{"type": "codex"},
		})
		if _, ok := entry["disable-cooling"]; ok {
			t.Fatalf("disable-cooling present = %#v, want absent", entry["disable-cooling"])
		}
	})
	t.Run("emits when override is explicitly false", func(t *testing.T) {
		entry := authFileEntry(&coreauth.Auth{
			ID:       "codex-auth",
			Provider: "codex",
			Metadata: map[string]any{"type": "codex", "disable_cooling": false},
		})
		if got, ok := entry["disable-cooling"]; !ok || got != false {
			t.Fatalf("disable-cooling = %#v (present=%t), want false", entry["disable-cooling"], ok)
		}
	})
}

func TestAuthFileEntryEmitsRequestRetryOverride(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     int
		present  bool
	}{
		{name: "canonical", metadata: map[string]any{"type": "codex", "request_retry": 2}, want: 2, present: true},
		{name: "legacy", metadata: map[string]any{"type": "codex", "request-retry": 0}, present: true},
		{name: "unset", metadata: map[string]any{"type": "codex"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := authFileEntry(&coreauth.Auth{ID: "codex-auth", Provider: "codex", Metadata: test.metadata})
			got, present := entry["request-retry"]
			if present != test.present {
				t.Fatalf("request-retry present = %t, want %t (entry=%#v)", present, test.present, entry)
			}
			if present && got != test.want {
				t.Fatalf("request-retry = %#v, want %d", got, test.want)
			}
		})
	}
}

func TestPatchAuthFileFieldsRoundTripsRequestRetry(t *testing.T) {
	handler, engine, _, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-retry")
	engine.GET("/auth-files", handler.ListAuthFiles)

	patchResponse := httptest.NewRecorder()
	patchRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-retry",
		"request-retry":2
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
			ID           string `json:"id"`
			RequestRetry *int   `json:"request-retry"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(listResponse.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	if len(payload.Files) != 1 || payload.Files[0].ID != "oauth-retry" || payload.Files[0].RequestRetry == nil || *payload.Files[0].RequestRetry != 2 {
		t.Fatalf("files = %#v, want request-retry 2", payload.Files)
	}

	persisted, _, errAuth := handler.repo.GetAuth(t.Context(), "oauth-retry")
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if got, ok := persisted.RequestRetryOverride(); !ok || got != 2 {
		t.Fatalf("persisted request retry override = (%d, %t), want (2, true)", got, ok)
	}
	if _, exists := persisted.Metadata["request-retry"]; exists {
		t.Fatalf("legacy request-retry metadata remains after hyphenated PATCH: %#v", persisted.Metadata)
	}

	clearResponse := httptest.NewRecorder()
	clearRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-retry",
		"request_retry":null
	}`))
	clearRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(clearResponse, clearRequest)
	if clearResponse.Code != http.StatusOK {
		t.Fatalf("PATCH clear status = %d body=%s", clearResponse.Code, clearResponse.Body.String())
	}

	persisted, _, errAuth = handler.repo.GetAuth(t.Context(), "oauth-retry")
	if errAuth != nil {
		t.Fatalf("GetAuth(after clear) error = %v", errAuth)
	}
	if got, ok := persisted.RequestRetryOverride(); ok || got != 0 {
		t.Fatalf("persisted request retry override after clear = (%d, %t), want inherited", got, ok)
	}
	listResponse = httptest.NewRecorder()
	engine.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/auth-files", nil))
	payload.Files = nil
	if errDecode := json.Unmarshal(listResponse.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode GET after clear: %v", errDecode)
	}
	if len(payload.Files) != 1 || payload.Files[0].RequestRetry != nil {
		t.Fatalf("files after clear = %#v, want omitted request-retry", payload.Files)
	}
}

func TestPatchAuthFileFieldsRoundTripsDisableCooling(t *testing.T) {
	handler, engine, _, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-cooling")
	engine.GET("/auth-files", handler.ListAuthFiles)

	patchResponse := httptest.NewRecorder()
	patchRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-cooling",
		"disable_cooling":true
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
			ID             string `json:"id"`
			DisableCooling *bool  `json:"disable-cooling"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(listResponse.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files = %#v, want one item", payload.Files)
	}
	if file := payload.Files[0]; file.ID != "oauth-cooling" || file.DisableCooling == nil || !*file.DisableCooling {
		t.Fatalf("file = %#v, want disable-cooling round-tripped to true", file)
	}

	persisted, _, errAuth := handler.repo.GetAuth(t.Context(), "oauth-cooling")
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if disabled := persisted.DisableCoolingOverride(); disabled == nil || !*disabled {
		t.Fatalf("persisted override = %#v, want true", disabled)
	}

	patchFalseResponse := httptest.NewRecorder()
	patchFalseRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-cooling",
		"disable_cooling":false
	}`))
	patchFalseRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchFalseResponse, patchFalseRequest)
	if patchFalseResponse.Code != http.StatusOK {
		t.Fatalf("PATCH false status = %d body=%s", patchFalseResponse.Code, patchFalseResponse.Body.String())
	}

	listFalseResponse := httptest.NewRecorder()
	engine.ServeHTTP(listFalseResponse, httptest.NewRequest(http.MethodGet, "/auth-files", nil))
	if errDecode := json.Unmarshal(listFalseResponse.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode false GET response: %v", errDecode)
	}
	if len(payload.Files) != 1 || payload.Files[0].DisableCooling == nil || *payload.Files[0].DisableCooling {
		t.Fatalf("files = %#v, want explicit disable-cooling false", payload.Files)
	}
	persisted, _, errAuth = handler.repo.GetAuth(t.Context(), "oauth-cooling")
	if errAuth != nil {
		t.Fatalf("GetAuth() after false patch error = %v", errAuth)
	}
	if disabled := persisted.DisableCoolingOverride(); disabled == nil || *disabled {
		t.Fatalf("persisted override = %#v, want false", disabled)
	}

	patchHyphenResponse := httptest.NewRecorder()
	patchHyphenRequest := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", strings.NewReader(`{
		"id":"oauth-cooling",
		"disable-cooling":true
	}`))
	patchHyphenRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchHyphenResponse, patchHyphenRequest)
	if patchHyphenResponse.Code != http.StatusOK {
		t.Fatalf("PATCH hyphenated true status = %d body=%s", patchHyphenResponse.Code, patchHyphenResponse.Body.String())
	}
	persisted, _, errAuth = handler.repo.GetAuth(t.Context(), "oauth-cooling")
	if errAuth != nil {
		t.Fatalf("GetAuth() after hyphenated true patch error = %v", errAuth)
	}
	if disabled := persisted.DisableCoolingOverride(); disabled == nil || !*disabled {
		t.Fatalf("persisted override after hyphenated patch = %#v, want true", disabled)
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
