package home

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func testDownstreamGrantBinding(auth *coreauth.Auth) string {
	var refresh, access string
	if auth != nil && auth.Metadata != nil {
		refresh, _ = auth.Metadata["refresh_token"].(string)
		access, _ = auth.Metadata["access_token"].(string)
	}
	refresh = strings.TrimSpace(refresh)
	access = strings.TrimSpace(access)
	if refresh == "" && access == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(refresh + "\x00" + access))
	return "grant:" + hex.EncodeToString(digest[:8])
}

func TestSanitizeAuthForDownstreamProjectsStableAntigravitySignatureGrant(t *testing.T) {
	t.Parallel()

	initial := &coreauth.Auth{
		ID:       "ag-home-sanitize",
		Index:    "ag-home-sanitize",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"project_id":    "proj-1",
		},
	}

	dispatch1, errSanitize1 := SanitizeAuthForDownstream(initial, nil)
	if errSanitize1 != nil {
		t.Fatalf("SanitizeAuthForDownstream(initial) error = %v", errSanitize1)
	}
	if dispatch1.Metadata["refresh_token"] != nil {
		t.Fatal("SanitizeAuthForDownstream() did not remove refresh_token")
	}
	grant1, _ := dispatch1.Metadata["cpa_signature_cache_grant"].(string)
	binding1, _ := dispatch1.Metadata["cpa_signature_cache_grant_binding"].(string)
	if grant1 == "" || binding1 == "" {
		t.Fatalf("dispatch1 missing projected signature grant metadata: grant=%q binding=%q", grant1, binding1)
	}
	enumScope1 := dispatch1.Attributes[antigravityModelEnumScopeAttribute]
	wantGrant1 := coreauth.AntigravitySignatureGrantFromEnumScope(enumScope1)
	if grant1 != wantGrant1 {
		t.Fatalf("dispatch1 grant = %q, want enum-aligned %q (rolling upgrade parity with old Home)", grant1, wantGrant1)
	}
	if wantBinding1 := testDownstreamGrantBinding(dispatch1); binding1 != wantBinding1 {
		t.Fatalf("dispatch1 binding = %q, want %q", binding1, wantBinding1)
	}

	// Re-sanitizing an already-sanitized auth must be idempotent.
	dispatch1Again, errAgain := SanitizeAuthForDownstream(dispatch1, nil)
	if errAgain != nil {
		t.Fatalf("SanitizeAuthForDownstream(dispatch1) error = %v", errAgain)
	}
	if got, _ := dispatch1Again.Metadata["cpa_signature_cache_grant"].(string); got != grant1 {
		t.Fatalf("idempotent SanitizeAuthForDownstream() grant = %q, want %q", got, grant1)
	}

	// Routine access_token rotation performed by an older Home node (without updating
	// cpa_signature_cache_grant_binding) must still yield the exact same grant on a new Home node.
	rotatedByOldHome := initial.Clone()
	rotatedByOldHome.Metadata["access_token"] = "at-old-home-rotated"
	dispatchOldToNew, errOldToNew := SanitizeAuthForDownstream(rotatedByOldHome, nil)
	if errOldToNew != nil {
		t.Fatalf("SanitizeAuthForDownstream(rotatedByOldHome) error = %v", errOldToNew)
	}
	if got, _ := dispatchOldToNew.Metadata["cpa_signature_cache_grant"].(string); got != grant1 {
		t.Fatalf("old->new Home rotation grant = %q, want %q", got, grant1)
	}

	// Simulate a verified refresh in Home rotating access_token and refresh_token while retaining grant + home binding.
	refreshed := initial.Clone()
	refreshed.Metadata["cpa_signature_cache_grant"] = coreauth.AntigravitySignatureGrantIdentity(initial)
	refreshed.Metadata["access_token"] = "at-2"
	refreshed.Metadata["refresh_token"] = "rt-2"
	refreshed.Metadata["cpa_signature_cache_grant_binding"] = testDownstreamGrantBinding(refreshed)
	enumScopeAfterRT := antigravityModelEnumScope(refreshed)
	if enumScopeAfterRT == enumScope1 {
		t.Fatal("expected refresh_token rotation to change antigravity_model_enum_scope")
	}
	refreshed.Metadata["cpa_signature_cache_grant_home_binding"] = coreauth.AntigravitySignatureGrantHomeBinding(enumScopeAfterRT)

	dispatch2, errSanitize2 := SanitizeAuthForDownstream(refreshed, nil)
	if errSanitize2 != nil {
		t.Fatalf("SanitizeAuthForDownstream(refreshed) error = %v", errSanitize2)
	}
	grant2, _ := dispatch2.Metadata["cpa_signature_cache_grant"].(string)
	binding2, _ := dispatch2.Metadata["cpa_signature_cache_grant_binding"].(string)
	if grant2 != grant1 {
		t.Fatalf("dispatch2 grant = %q, want %q", grant2, grant1)
	}
	if wantBinding2 := testDownstreamGrantBinding(dispatch2); binding2 != wantBinding2 {
		t.Fatalf("dispatch2 binding = %q, want %q", binding2, wantBinding2)
	}

	// BuildRefreshPayload must also carry the same projected signature grant.
	rawRefresh, errPayload := BuildRefreshPayload(refreshed, nil)
	if errPayload != nil {
		t.Fatalf("BuildRefreshPayload() error = %v", errPayload)
	}
	var env struct {
		Auth coreauth.Auth `json:"auth"`
	}
	if errUnmarshal := json.Unmarshal(rawRefresh, &env); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal(BuildRefreshPayload) error = %v", errUnmarshal)
	}
	if got, _ := env.Auth.Metadata["cpa_signature_cache_grant"].(string); got != grant1 {
		t.Fatalf("BuildRefreshPayload() grant = %q, want %q", got, grant1)
	}
	if got, want := env.Auth.Metadata["cpa_signature_cache_grant_binding"], testDownstreamGrantBinding(&env.Auth); got != want {
		t.Fatalf("BuildRefreshPayload() binding = %v, want %q", got, want)
	}

	// External credential replacement (stale binding mismatch) must produce a new downstream grant.
	replaced := refreshed.Clone()
	replaced.Metadata["refresh_token"] = "rt-other-account"
	replaced.Metadata["access_token"] = "at-other-account"
	dispatchReplaced, errReplaced := SanitizeAuthForDownstream(replaced, nil)
	if errReplaced != nil {
		t.Fatalf("SanitizeAuthForDownstream(replaced) error = %v", errReplaced)
	}
	if got, _ := dispatchReplaced.Metadata["cpa_signature_cache_grant"].(string); got == "" || got == grant1 {
		t.Fatalf("replaced credential downstream grant = %q, want distinct non-empty grant (old=%q)", got, grant1)
	}

	// After an older Home node rotates access_token on the rt-2 row without updating
	// cpa_signature_cache_grant_binding, new Home still preserves grant1 via home_binding.
	oldHomeAfterRT := refreshed.Clone()
	oldHomeAfterRT.Metadata["access_token"] = "at-3-old-home"
	dispatchAfterOldHome, errAfterOld := SanitizeAuthForDownstream(oldHomeAfterRT, nil)
	if errAfterOld != nil {
		t.Fatalf("SanitizeAuthForDownstream(oldHomeAfterRT) error = %v", errAfterOld)
	}
	if got, _ := dispatchAfterOldHome.Metadata["cpa_signature_cache_grant"].(string); got != grant1 {
		t.Fatalf("post-rt-rotation old->new Home grant = %q, want %q", got, grant1)
	}
}
