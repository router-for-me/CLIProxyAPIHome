package cluster

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestEnsurePluginAuthIdentityAssignsStableUUID(t *testing.T) {
	first := &coreauth.Auth{ID: "copilot-octo-user.json", Provider: "copilot"}
	EnsurePluginAuthIdentity(first)
	if !isValidUUID(first.ID) {
		t.Fatalf("ID = %q, want a UUID", first.ID)
	}
	if first.Index != first.ID {
		t.Fatalf("Index = %q, want it to equal ID %q", first.Index, first.ID)
	}
	if first.FileName != "copilot-octo-user.json" {
		t.Fatalf("FileName = %q, want the plugin identifier kept", first.FileName)
	}
	if _, err := AuthToRecord(first); err != nil {
		t.Fatalf("AuthToRecord() error = %v, want the auth to be storable", err)
	}

	again := &coreauth.Auth{ID: "copilot-octo-user.json", Provider: "Copilot"}
	EnsurePluginAuthIdentity(again)
	if again.ID != first.ID {
		t.Fatalf("re-login ID = %q, want the same UUID %q", again.ID, first.ID)
	}

	other := &coreauth.Auth{ID: "copilot-second-user.json", Provider: "copilot"}
	EnsurePluginAuthIdentity(other)
	if other.ID == first.ID {
		t.Fatal("a different account produced the same UUID")
	}
	sameIDOtherProvider := &coreauth.Auth{ID: "copilot-octo-user.json", Provider: "other"}
	EnsurePluginAuthIdentity(sameIDOtherProvider)
	if sameIDOtherProvider.ID == first.ID {
		t.Fatal("a different provider produced the same UUID")
	}
}

func TestEnsurePluginAuthIdentityKeepsExistingUUID(t *testing.T) {
	const existing = "4d810cfd-47c8-420d-8687-05f68428caba"
	auth := &coreauth.Auth{ID: existing, Provider: "copilot", FileName: "copilot-octo-user.json"}
	EnsurePluginAuthIdentity(auth)
	if auth.ID != existing || auth.Index != existing {
		t.Fatalf("ID/Index = %q/%q, want the existing UUID kept", auth.ID, auth.Index)
	}
}

func TestEnsurePluginAuthIdentityUsesFileNameWhenIDMissing(t *testing.T) {
	auth := &coreauth.Auth{Provider: "copilot", FileName: "copilot-octo-user.json"}
	EnsurePluginAuthIdentity(auth)
	withID := &coreauth.Auth{ID: "copilot-octo-user.json", Provider: "copilot"}
	EnsurePluginAuthIdentity(withID)
	if auth.ID != withID.ID || auth.Index != auth.ID {
		t.Fatalf("ID = %q, want %q derived from the file name", auth.ID, withID.ID)
	}
}

func TestEnsurePluginAuthIdentityIgnoresEmptyAuth(t *testing.T) {
	EnsurePluginAuthIdentity(nil)
	auth := &coreauth.Auth{Provider: "copilot"}
	EnsurePluginAuthIdentity(auth)
	if auth.ID != "" || auth.Index != "" {
		t.Fatalf("ID/Index = %q/%q, want untouched when there is no identifier", auth.ID, auth.Index)
	}
}
