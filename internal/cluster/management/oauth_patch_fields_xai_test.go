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

func TestApplyOAuthFieldPatchXaiUsingAPI(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Metadata: map[string]any{
			"type": "xai",
		},
	}
	entry := authFileEntry(auth)
	if got := entry["using_api"]; got != false {
		t.Fatalf("default using_api = %#v, want false", got)
	}

	changed, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, `{"using_api":true}`))
	if errPatch != nil {
		t.Fatalf("applyOAuthFieldPatch returned error: %v", errPatch)
	}
	if !changed {
		t.Fatalf("applyOAuthFieldPatch changed = false, want true")
	}
	if got, ok := auth.Metadata["using_api"].(bool); !ok || !got {
		t.Fatalf("metadata.using_api = %#v, want true", auth.Metadata["using_api"])
	}
	if got := auth.Attributes["using_api"]; got != "true" {
		t.Fatalf("attributes.using_api = %q, want true", got)
	}
	entry = authFileEntry(auth)
	if got := entry["using_api"]; got != true {
		t.Fatalf("patched using_api = %#v, want true", got)
	}
}
