package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

func testAntigravityGrantBinding(auth *Auth) string {
	refresh := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token"))
	access := strings.TrimSpace(metaStringValue(auth.Metadata, "access_token"))
	if refresh == "" && access == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(refresh + "\x00" + access))
	return "grant:" + hex.EncodeToString(digest[:8])
}

func TestRefreshAntigravityPreservesSignatureCacheGrantAcrossRotations(t *testing.T) {

	initial := &Auth{
		ID:       "ag-home-1",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"project_id":    "proj-1",
		},
	}
	initialGrant := testAntigravityGrantBinding(initial)

	// First refresh rotates both access_token and refresh_token.
	rt1 := refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`)),
			Request:    req,
		}, nil
	})
	refreshed1, errRefresh1 := refreshAntigravity(context.Background(), nil, initial.Clone(), rt1)
	if errRefresh1 != nil {
		t.Fatalf("first refreshAntigravity() error = %v", errRefresh1)
	}
	if got := metaStringValue(refreshed1.Metadata, "cpa_signature_cache_grant"); got != initialGrant {
		t.Fatalf("after first refresh cpa_signature_cache_grant = %q, want %q", got, initialGrant)
	}
	if got, want := metaStringValue(refreshed1.Metadata, "cpa_signature_cache_grant_binding"), testAntigravityGrantBinding(refreshed1); got != want {
		t.Fatalf("after first refresh cpa_signature_cache_grant_binding = %q, want %q", got, want)
	}

	// Second refresh keeps refresh_token and rotates access_token.
	rt2 := refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-3","expires_in":3600}`)),
			Request:    req,
		}, nil
	})
	refreshed2, errRefresh2 := refreshAntigravity(context.Background(), nil, refreshed1.Clone(), rt2)
	if errRefresh2 != nil {
		t.Fatalf("second refreshAntigravity() error = %v", errRefresh2)
	}
	if got := metaStringValue(refreshed2.Metadata, "cpa_signature_cache_grant"); got != initialGrant {
		t.Fatalf("after second refresh cpa_signature_cache_grant = %q, want %q", got, initialGrant)
	}
	if got, want := metaStringValue(refreshed2.Metadata, "cpa_signature_cache_grant_binding"), testAntigravityGrantBinding(refreshed2); got != want {
		t.Fatalf("after second refresh cpa_signature_cache_grant_binding = %q, want %q", got, want)
	}

	// External credential replacement (changed refresh_token without matching binding) must NOT inherit stale grant.
	replaced := refreshed2.Clone()
	replaced.Metadata["refresh_token"] = "rt-replaced"
	refreshedReplaced, errReplaced := refreshAntigravity(context.Background(), nil, replaced, rt2)
	if errReplaced != nil {
		t.Fatalf("replaced refreshAntigravity() error = %v", errReplaced)
	}
	if got := metaStringValue(refreshedReplaced.Metadata, "cpa_signature_cache_grant"); got == "" || got == initialGrant {
		t.Fatalf("refresh after external credential replacement cpa_signature_cache_grant = %q, want new grant != %q", got, initialGrant)
	}
}

func TestRefreshAntigravityAlignsSignatureGrantWithRegisteredEnumScopeResolver(t *testing.T) {
	prev := antigravityEnumScopeResolver.Load()
	t.Cleanup(func() {
		if prev == nil {
			RegisterAntigravityEnumScopeResolver(nil)
			return
		}
		RegisterAntigravityEnumScopeResolver(*prev)
	})
	RegisterAntigravityEnumScopeResolver(func(a *Auth) string {
		if a == nil {
			return ""
		}
		return "enum:" + a.ID + ":" + metaStringValue(a.Metadata, "refresh_token")
	})

	initial := &Auth{
		ID:       "ag-home-enum-1",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
		},
	}
	wantGrant := AntigravitySignatureGrantFromEnumScope("enum:ag-home-enum-1:rt-1")
	if got := AntigravitySignatureGrantIdentity(initial); got != wantGrant {
		t.Fatalf("initial AntigravitySignatureGrantIdentity() = %q, want %q", got, wantGrant)
	}

	rt := refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`)),
			Request:    req,
		}, nil
	})
	refreshed, errRefresh := refreshAntigravity(context.Background(), nil, initial.Clone(), rt)
	if errRefresh != nil {
		t.Fatalf("refreshAntigravity() error = %v", errRefresh)
	}
	if got := metaStringValue(refreshed.Metadata, AntigravitySignatureGrantScopeMetadataKey); got != wantGrant {
		t.Fatalf("refreshed grant = %q, want %q", got, wantGrant)
	}
	wantHomeBinding := AntigravitySignatureGrantHomeBinding("enum:ag-home-enum-1:rt-2")
	if got := metaStringValue(refreshed.Metadata, AntigravitySignatureGrantHomeBindingMetadataKey); got != wantHomeBinding {
		t.Fatalf("refreshed home binding = %q, want %q", got, wantHomeBinding)
	}

	// If an older Home node subsequently rotates access_token (leaving refresh_token=rt-2 unchanged
	// without updating cpa_signature_cache_grant_binding), home_binding still preserves wantGrant.
	oldHomeRotated := refreshed.Clone()
	oldHomeRotated.Metadata["access_token"] = "at-3-from-old-home"
	if got := AntigravitySignatureGrantIdentity(oldHomeRotated); got != wantGrant {
		t.Fatalf("after old-Home access_token rotation identity = %q, want %q", got, wantGrant)
	}

	// External replacement changing refresh_token invalidates both token binding and home_binding.
	replaced := refreshed.Clone()
	replaced.Metadata["refresh_token"] = "rt-replaced"
	if got := AntigravitySignatureGrantIdentity(replaced); got == wantGrant {
		t.Fatalf("external replacement reused prior grant %q", got)
	}
}
