package management

import (
	"encoding/json"
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
		Metadata: map[string]any{"type": "codex"},
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
