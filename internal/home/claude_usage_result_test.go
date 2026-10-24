package home

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// TestRecordUsagePayloadHonoursClaudeUnified5hHeaderReset is the A5-i /
// A5-iii test: a Claude 429 carrying a real "response_headers" block (the
// exact canonical-cased, JSON-array-wrapped wire shape the node sends --
// see internal/redisqueue/plugin.go's requestDetail.ResponseHeaders on the
// CPA side) with a 5h reset ~4h out must honour that reset, NOT re-arm the
// model-scoped 1s->30m exponential ladder.
func TestRecordUsagePayloadHonoursClaudeUnified5hHeaderReset(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-claude-5h-auth",
		Index:    "usage-claude-5h-index",
		Provider: "claude",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	reset := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)
	rt.RecordUsagePayload(context.Background(), fmt.Sprintf(`{
        "auth_index": "usage-claude-5h-index",
        "provider": "claude",
        "model": "claude-opus-4-1",
        "failed": true,
        "fail": {
            "status_code": 429,
            "body": ""
        },
        "response_headers": {
            "Anthropic-Ratelimit-Unified-Status": ["rejected"],
            "Anthropic-Ratelimit-Unified-5h-Status": ["rejected"],
            "Anthropic-Ratelimit-Unified-5h-Reset": [%q],
            "Anthropic-Ratelimit-Unified-7d-Status": ["allowed"]
        }
    }`, strconv.FormatInt(reset.Unix(), 10)))

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["claude-opus-4-1"]
	if state == nil {
		t.Fatalf("ModelStates[claude-opus-4-1] missing after usage payload: %#v", got.ModelStates)
	}
	if !state.NextRetryAfter.Equal(reset) {
		t.Fatalf("ModelStates[claude-opus-4-1].NextRetryAfter = %v, want the header reset %v (NOT the exponential ladder)", state.NextRetryAfter, reset)
	}
	if delay := state.NextRetryAfter.Sub(time.Now()); delay < 3*time.Hour {
		t.Fatalf("NextRetryAfter delay = %v, looks like the 1s exponential floor re-armed instead of the 4h header reset", delay)
	}
}

// TestRecordUsagePayloadClaudeCredentialScopedRejectionBlocksSiblingModel is
// the A5-ii test: a Claude 429 explicitly rejected on the shared 5h window
// must block every ModelState on the credential, not just the model that
// failed, and the aggregated auth.Unavailable must follow.
func TestRecordUsagePayloadClaudeCredentialScopedRejectionBlocksSiblingModel(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-claude-credential-auth",
		Index:    "usage-claude-credential-index",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{
			// A sibling model that has never failed and must be blocked by
			// the fan-out even though the 429 lands on claude-opus.
			"claude-3-7-sonnet": {Status: coreauth.StatusActive},
		},
	}
	rt := newUsageResultTestRuntime(t, auth)

	reset := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)
	rt.RecordUsagePayload(context.Background(), fmt.Sprintf(`{
        "auth_index": "usage-claude-credential-index",
        "provider": "claude",
        "model": "claude-opus-4-1",
        "failed": true,
        "fail": {
            "status_code": 429,
            "body": ""
        },
        "response_headers": {
            "Anthropic-Ratelimit-Unified-5h-Status": ["rejected"],
            "Anthropic-Ratelimit-Unified-5h-Reset": [%q]
        }
    }`, strconv.FormatInt(reset.Unix(), 10)))

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}

	failedState := got.ModelStates["claude-opus-4-1"]
	if failedState == nil || !failedState.Unavailable || !failedState.NextRetryAfter.Equal(reset) {
		t.Fatalf("failed model state = %#v, want Unavailable at %v", failedState, reset)
	}

	siblingState := got.ModelStates["claude-3-7-sonnet"]
	if siblingState == nil || !siblingState.Unavailable {
		t.Fatalf("sibling model state = %#v, want Unavailable (credential-scoped fan-out did not cover it)", siblingState)
	}
	if !siblingState.NextRetryAfter.Equal(reset) {
		t.Fatalf("sibling NextRetryAfter = %v, want %v", siblingState.NextRetryAfter, reset)
	}

	if !got.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true once every ModelState is blocked")
	}
	if !got.NextRetryAfter.Equal(reset) {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", got.NextRetryAfter, reset)
	}
}

// TestRecordUsagePayloadClaudeFableOnlyRejectionStaysModelScoped is the
// negative case: a Fable-only 7d_oi rejection, with both shared windows
// explicitly allowed, must NOT fan out to sibling models.
func TestRecordUsagePayloadClaudeFableOnlyRejectionStaysModelScoped(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-claude-fable-only-auth",
		Index:    "usage-claude-fable-only-index",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{
			"claude-3-7-sonnet": {Status: coreauth.StatusActive},
		},
	}
	rt := newUsageResultTestRuntime(t, auth)

	reset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	rt.RecordUsagePayload(context.Background(), fmt.Sprintf(`{
        "auth_index": "usage-claude-fable-only-index",
        "provider": "claude",
        "model": "claude-opus-4-1",
        "failed": true,
        "fail": {
            "status_code": 429,
            "body": ""
        },
        "response_headers": {
            "Anthropic-Ratelimit-Unified-Status": ["rejected"],
            "Anthropic-Ratelimit-Unified-5h-Status": ["allowed"],
            "Anthropic-Ratelimit-Unified-7d-Status": ["allowed_warning"],
            "Anthropic-Ratelimit-Unified-7d_oi-Status": ["rejected"],
            "Anthropic-Ratelimit-Unified-7d_oi-Reset": [%q]
        }
    }`, strconv.FormatInt(reset.Unix(), 10)))

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}

	siblingState := got.ModelStates["claude-3-7-sonnet"]
	if siblingState == nil || siblingState.Unavailable {
		t.Fatalf("sibling model state = %#v, want untouched (fable-only rejection is model-scoped)", siblingState)
	}
	if got.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false (only one model blocked)")
	}
}

// TestParseResponseHeadersUnwrapsJSONArrayValues is the A1 wire-shape trap
// test: response_headers values are ALWAYS JSON arrays
// (encoding/json's shape for http.Header/map[string][]string), even for a
// single value -- a naive .String() read on the array would silently return
// the array's raw text ("[\"rejected\"]") instead of "rejected".
func TestParseResponseHeadersUnwrapsJSONArrayValues(t *testing.T) {
	payload := `{
        "response_headers": {
            "Anthropic-Ratelimit-Unified-Status": ["rejected"],
            "Retry-After": ["30"]
        }
    }`
	headers := parseResponseHeaders(payload)
	if headers == nil {
		t.Fatalf("parseResponseHeaders() = nil, want a populated http.Header")
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("headers.Get(Anthropic-Ratelimit-Unified-Status) = %q, want %q (not the raw array text)", got, "rejected")
	}
	if got := headers.Get("Retry-After"); got != "30" {
		t.Fatalf("headers.Get(Retry-After) = %q, want %q", got, "30")
	}
}

// TestParseResponseHeadersCanonicalizesNonCanonicalCasing proves headers
// arrive usable even if a producer sent lowercase or otherwise
// non-canonical JSON keys -- http.Header.Add canonicalizes on insertion.
func TestParseResponseHeadersCanonicalizesNonCanonicalCasing(t *testing.T) {
	payload := `{"response_headers": {"anthropic-ratelimit-unified-5h-status": ["rejected"]}}`
	headers := parseResponseHeaders(payload)
	if headers == nil {
		t.Fatalf("parseResponseHeaders() = nil, want a populated http.Header")
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Status"); got != "rejected" {
		t.Fatalf("headers.Get() = %q, want %q (canonical lookup over non-canonical input casing)", got, "rejected")
	}
}

// TestParseResponseHeadersAbsentFieldReturnsNil confirms the seam degrades
// gracefully when a payload has no response_headers at all (e.g. a
// non-Claude provider, or an older node build).
func TestParseResponseHeadersAbsentFieldReturnsNil(t *testing.T) {
	if got := parseResponseHeaders(`{"auth_index":"x"}`); got != nil {
		t.Fatalf("parseResponseHeaders() = %#v, want nil", got)
	}
	if got := parseResponseHeaders(`{"response_headers": {}}`); got != nil {
		t.Fatalf("parseResponseHeaders() = %#v, want nil for an empty object", got)
	}
	if got := parseResponseHeaders(`{"response_headers": "not-an-object"}`); got != nil {
		t.Fatalf("parseResponseHeaders() = %#v, want nil for a non-object value", got)
	}
}

// TestParseResponseHeadersReadsFlatQuotaHeadersShape is the A5/ingest-fix
// wire-shape test: "quota_headers" is what actually survives
// sanitizeUsageQuotaHeaders (internal/cluster/quota_ingestion.go) for a
// recognized provider -- a FLAT map[string]string (its own `filtered`
// map), a different JSON shape from "response_headers"'s
// map[string][]string. Before this fix parseResponseHeaders only read
// "response_headers", so a real production payload -- which after
// sanitizing carries "quota_headers", not "response_headers" -- parsed to
// no headers at all.
func TestParseResponseHeadersReadsFlatQuotaHeadersShape(t *testing.T) {
	payload := `{
		"quota_headers": {
			"Anthropic-Ratelimit-Unified-Status": "rejected",
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
			"Anthropic-Ratelimit-Unified-5h-Reset": "1700000000"
		}
	}`
	headers := parseResponseHeaders(payload)
	if headers == nil {
		t.Fatalf("parseResponseHeaders() = nil, want a populated http.Header")
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("headers.Get(Anthropic-Ratelimit-Unified-Status) = %q, want %q", got, "rejected")
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Reset"); got != "1700000000" {
		t.Fatalf("headers.Get(Anthropic-Ratelimit-Unified-5h-Reset) = %q, want %q", got, "1700000000")
	}
}

// TestParseResponseHeadersQuotaHeadersAloneIsSufficient proves a payload
// carrying ONLY quota_headers (the real production shape post-sanitizing --
// response_headers is always deleted by sanitizeUsageQuotaHeaders) still
// yields usable headers, not nil.
func TestParseResponseHeadersQuotaHeadersAloneIsSufficient(t *testing.T) {
	payload := `{"quota_headers": {"Retry-After": "30"}}`
	headers := parseResponseHeaders(payload)
	if headers == nil {
		t.Fatalf("parseResponseHeaders() = nil, want a populated http.Header from quota_headers alone")
	}
	if got := headers.Get("Retry-After"); got != "30" {
		t.Fatalf("headers.Get(Retry-After) = %q, want %q", got, "30")
	}
}

// TestParseResponseHeadersResponseHeadersWinsOnCollision proves the merge
// precedence: when the same header key appears in both response_headers
// and quota_headers, response_headers' value wins.
func TestParseResponseHeadersResponseHeadersWinsOnCollision(t *testing.T) {
	payload := `{
		"response_headers": {"Retry-After": ["response-headers-value"]},
		"quota_headers": {"Retry-After": "quota-headers-value"}
	}`
	headers := parseResponseHeaders(payload)
	if headers == nil {
		t.Fatalf("parseResponseHeaders() = nil, want a populated http.Header")
	}
	if got := headers.Get("Retry-After"); got != "response-headers-value" {
		t.Fatalf("headers.Get(Retry-After) = %q, want %q (response_headers must win on collision)", got, "response-headers-value")
	}
	if len(headers.Values("Retry-After")) != 1 {
		t.Fatalf("headers.Values(Retry-After) = %v, want exactly 1 value (quota_headers' colliding value must not also be appended)", headers.Values("Retry-After"))
	}
}

// TestParseResponseHeadersEmptyQuotaHeadersObjectReturnsNil confirms the
// empty-object degrade applies to quota_headers too, mirroring
// TestParseResponseHeadersAbsentFieldReturnsNil's coverage of
// response_headers.
func TestParseResponseHeadersEmptyQuotaHeadersObjectReturnsNil(t *testing.T) {
	if got := parseResponseHeaders(`{"quota_headers": {}}`); got != nil {
		t.Fatalf("parseResponseHeaders() = %#v, want nil for an empty quota_headers object", got)
	}
}
