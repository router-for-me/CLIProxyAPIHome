package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// TestSanitizeUsageQuotaHeadersPreservesClaudeRateLimitAllowlist is the
// sanitizer-level companion to the end-to-end test in
// internal/home/claude_usage_result_sanitize_path_test.go: a Claude
// payload's allowlisted Anthropic rate-limit headers must survive
// sanitizeUsageQuotaHeaders under "quota_headers", and "response_headers"
// must still be deleted unconditionally (it carries credential material
// for other providers, so the delete itself must never be scoped away).
func TestSanitizeUsageQuotaHeadersPreservesClaudeRateLimitAllowlist(t *testing.T) {
	payload := `{
		"provider": "claude",
		"response_headers": {
			"Anthropic-Ratelimit-Unified-Status": ["rejected"],
			"Anthropic-Ratelimit-Unified-5h-Status": ["rejected"],
			"Anthropic-Ratelimit-Unified-5h-Reset": ["1700000000"],
			"Anthropic-Ratelimit-Unified-7d-Status": ["allowed"],
			"Anthropic-Ratelimit-Unified-7d-Reset": ["1700100000"],
			"Anthropic-Ratelimit-Unified-7d_oi-Status": ["allowed"],
			"Anthropic-Ratelimit-Unified-7d_oi-Reset": ["1700200000"],
			"Anthropic-Ratelimit-Unified-Reset": ["1700300000"],
			"Retry-After": ["30"]
		}
	}`

	out, errSanitize := sanitizeUsageQuotaHeaders(payload)
	if errSanitize != nil {
		t.Fatalf("sanitizeUsageQuotaHeaders() error = %v", errSanitize)
	}
	if gjson.Get(out, "response_headers").Exists() {
		t.Fatalf("response_headers survived sanitizing: %s", out)
	}
	quotaHeaders := gjson.Get(out, "quota_headers")
	if !quotaHeaders.IsObject() {
		t.Fatalf("quota_headers missing or not an object: %s", out)
	}
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-Status":       "rejected",
		"Anthropic-Ratelimit-Unified-5h-Status":    "rejected",
		"Anthropic-Ratelimit-Unified-5h-Reset":     "1700000000",
		"Anthropic-Ratelimit-Unified-7d-Status":    "allowed",
		"Anthropic-Ratelimit-Unified-7d-Reset":     "1700100000",
		"Anthropic-Ratelimit-Unified-7d_oi-Status": "allowed",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":  "1700200000",
		"Anthropic-Ratelimit-Unified-Reset":        "1700300000",
		"Retry-After":                              "30",
	}
	for key, wantValue := range want {
		if got := quotaHeaders.Get(key).String(); got != wantValue {
			t.Fatalf("quota_headers[%q] = %q, want %q (full: %s)", key, got, wantValue, out)
		}
	}
	gotMap := quotaHeaders.Map()
	if len(gotMap) != len(want) {
		t.Fatalf("quota_headers has %d keys, want exactly %d (extra/missing keys): %s", len(gotMap), len(want), out)
	}
}

// TestSanitizeUsageQuotaHeadersRejectsNonAllowlistedClaudeHeader is the
// security-property negative test: a header NOT on the Claude rate-limit
// allowlist -- including one that looks like credential material -- must
// never survive sanitizeUsageQuotaHeaders for a Claude payload.
func TestSanitizeUsageQuotaHeadersRejectsNonAllowlistedClaudeHeader(t *testing.T) {
	payload := `{
		"provider": "claude",
		"response_headers": {
			"Anthropic-Ratelimit-Unified-5h-Status": ["rejected"],
			"Authorization": ["Bearer secret-token-must-not-survive"],
			"X-Something-Secret": ["also-must-not-survive"]
		}
	}`

	out, errSanitize := sanitizeUsageQuotaHeaders(payload)
	if errSanitize != nil {
		t.Fatalf("sanitizeUsageQuotaHeaders() error = %v", errSanitize)
	}
	if gjson.Get(out, "quota_headers.Authorization").Exists() {
		t.Fatalf("Authorization header survived sanitizing: %s", out)
	}
	if gjson.Get(out, "quota_headers.X-Something-Secret").Exists() {
		t.Fatalf("X-Something-Secret header survived sanitizing: %s", out)
	}
	if strings.Contains(out, "secret-token-must-not-survive") {
		t.Fatalf("secret value leaked into sanitized payload: %s", out)
	}
	// The allowlisted header alongside it must still have survived -- proves
	// the rejection is per-key, not a blanket failure that would trivially
	// satisfy the assertions above.
	if got := gjson.Get(out, "quota_headers.Anthropic-Ratelimit-Unified-5h-Status").String(); got != "rejected" {
		t.Fatalf("allowlisted sibling header did not survive: %s", out)
	}
}

// TestSanitizeUsageQuotaHeadersCodexBehaviorUnchanged locks the pre-existing
// codex extraction + quotaSnapshotWriteFromUsagePayload behavior byte-for-
// byte, and proves a Claude payload -- which now DOES populate
// quota_headers, this fix's whole point -- still cannot reach the
// codex-only quota-snapshot write path (the brief's safety-rail claim,
// proved here rather than assumed).
func TestSanitizeUsageQuotaHeadersCodexBehaviorUnchanged(t *testing.T) {
	codexPayload := `{
		"provider": "codex",
		"auth_index": "codex-auth-unchanged",
		"timestamp": "2026-07-16T01:00:00Z",
		"response_headers": {
			"X-Codex-Active-Limit": ["premium"],
			"X-Codex-Primary-Used-Percent": ["82"],
			"X-Codex-Primary-Window-Minutes": ["300"],
			"X-Codex-Primary-Reset-After-Seconds": ["600"],
			"X-Codex-Plan-Type": ["pro"],
			"X-Upstream-Request-Id": ["upstream-quota-unchanged"],
			"Authorization": ["Bearer must-not-persist"]
		}
	}`

	out, errSanitize := sanitizeUsageQuotaHeaders(codexPayload)
	if errSanitize != nil {
		t.Fatalf("sanitizeUsageQuotaHeaders() error = %v", errSanitize)
	}
	if gjson.Get(out, "quota_headers.Authorization").Exists() {
		t.Fatalf("codex Authorization header survived sanitizing: %s", out)
	}
	quotaHeadersMap := gjson.Get(out, "quota_headers").Map()
	wantCodexKeys := map[string]string{
		"X-Codex-Active-Limit":                "premium",
		"X-Codex-Primary-Used-Percent":        "82",
		"X-Codex-Primary-Window-Minutes":      "300",
		"X-Codex-Primary-Reset-After-Seconds": "600",
		"X-Codex-Plan-Type":                   "pro",
	}
	if len(quotaHeadersMap) != len(wantCodexKeys) {
		t.Fatalf("codex quota_headers has %d keys, want exactly %d: %s", len(quotaHeadersMap), len(wantCodexKeys), out)
	}
	for key, wantValue := range wantCodexKeys {
		if got := quotaHeadersMap[key].String(); got != wantValue {
			t.Fatalf("quota_headers[%q] = %q, want %q: %s", key, got, wantValue, out)
		}
	}

	write, ok := quotaSnapshotWriteFromUsagePayload(out, UsageRuntimeMetadata{}, time.Date(2026, 7, 16, 1, 10, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("quotaSnapshotWriteFromUsagePayload() ok = false for a codex payload, want true")
	}
	if write.CredentialID != "codex-auth-unchanged" {
		t.Fatalf("write.CredentialID = %q, want codex-auth-unchanged", write.CredentialID)
	}
	if len(write.Windows) != 1 {
		t.Fatalf("write.Windows = %+v, want exactly 1 window", write.Windows)
	}

	claudePayload := `{
		"provider": "claude",
		"response_headers": {
			"Anthropic-Ratelimit-Unified-5h-Status": ["rejected"],
			"Anthropic-Ratelimit-Unified-5h-Reset": ["1700000000"]
		}
	}`
	sanitizedClaude, errSanitizeClaude := sanitizeUsageQuotaHeaders(claudePayload)
	if errSanitizeClaude != nil {
		t.Fatalf("sanitizeUsageQuotaHeaders(claude) error = %v", errSanitizeClaude)
	}
	if !gjson.Get(sanitizedClaude, "quota_headers").IsObject() {
		t.Fatalf("claude quota_headers missing -- test setup invalid, would trivially pass the safety-rail assertion below: %s", sanitizedClaude)
	}
	_, claudeOK := quotaSnapshotWriteFromUsagePayload(sanitizedClaude, UsageRuntimeMetadata{}, time.Now().UTC())
	if claudeOK {
		t.Fatalf("quotaSnapshotWriteFromUsagePayload() ok = true for a claude payload, want false (codex-only gate must hold)")
	}
}
