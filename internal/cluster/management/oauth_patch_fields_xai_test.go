package management

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// xAI OAuth auth files share the same generic websockets patch path as Codex:
// PATCH /auth-files/fields persists metadata.websockets and syncs the
// effective attributes.websockets flag without provider gating.
func TestApplyOAuthFieldPatchXaiWebsockets(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Metadata: map[string]any{
			"type": "xai",
		},
	}
	fields := mustRawFields(t, `{"websockets":true}`)

	changed, errPatch := applyOAuthFieldPatch(auth, fields)
	if errPatch != nil {
		t.Fatalf("applyOAuthFieldPatch returned error: %v", errPatch)
	}
	if !changed {
		t.Fatalf("applyOAuthFieldPatch changed = false, want true")
	}
	if got, ok := auth.Metadata["websockets"].(bool); !ok || !got {
		t.Fatalf("metadata.websockets = %#v, want true", auth.Metadata["websockets"])
	}
	if got := auth.Attributes["websockets"]; got != "true" {
		t.Fatalf("attributes.websockets = %q, want true", got)
	}
}
