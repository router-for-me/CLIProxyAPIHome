package home

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestSanitizeAuthForDownstreamRemovesRefreshLease(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "leased-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"__home_refresh_lease": "internal-lease-id",
			"route":                "retained",
		},
	}

	sanitized := SanitizeAuthForDownstream(auth)
	if sanitized == nil {
		t.Fatal("SanitizeAuthForDownstream() = nil")
	}
	if _, exists := sanitized.Attributes["__home_refresh_lease"]; exists {
		t.Fatalf("sanitized auth retained internal refresh lease: %#v", sanitized.Attributes)
	}
	if sanitized.Attributes["route"] != "retained" {
		t.Fatalf("sanitized route attribute = %q, want retained", sanitized.Attributes["route"])
	}
	if auth.Attributes["__home_refresh_lease"] != "internal-lease-id" {
		t.Fatal("SanitizeAuthForDownstream() mutated the source auth")
	}
}
