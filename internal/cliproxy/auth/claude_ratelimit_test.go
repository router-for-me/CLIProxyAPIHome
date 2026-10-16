package auth

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func claudeHeaders(pairs map[string]string) http.Header {
	headers := make(http.Header)
	for k, v := range pairs {
		headers.Set(k, v)
	}
	return headers
}

func TestClaudeHeadersIndicateUnifiedRateLimitRejection(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{
			name:    "nil headers",
			headers: nil,
			want:    false,
		},
		{
			name:    "no headers set",
			headers: claudeHeaders(nil),
			want:    false,
		},
		{
			name: "5h rejected is credential scope",
			headers: claudeHeaders(map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
			}),
			want: true,
		},
		{
			name: "7d rejected is credential scope",
			headers: claudeHeaders(map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
			}),
			want: true,
		},
		{
			name: "fable-only 7d_oi rejection with both shared windows allowed is NOT credential scope",
			headers: claudeHeaders(map[string]string{
				"Anthropic-Ratelimit-Unified-Status":       "rejected",
				"Anthropic-Ratelimit-Unified-5h-Status":    "allowed",
				"Anthropic-Ratelimit-Unified-7d-Status":    "allowed_warning",
				"Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected",
			}),
			want: false,
		},
		{
			name: "unified rejected with 7d_oi also rejected and shared windows NOT explicitly allowed is credential scope",
			headers: claudeHeaders(map[string]string{
				"Anthropic-Ratelimit-Unified-Status":       "rejected",
				"Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected",
			}),
			want: true,
		},
		{
			name: "unified allowed, no window rejected",
			headers: claudeHeaders(map[string]string{
				"Anthropic-Ratelimit-Unified-Status": "allowed",
			}),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeHeadersIndicateUnifiedRateLimitRejection(tc.headers); got != tc.want {
				t.Fatalf("claudeHeadersIndicateUnifiedRateLimitRejection() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseClaudeRateLimitResetAtHonoursExplicitResetHeaders(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

	t.Run("5h reset honoured", func(t *testing.T) {
		reset := now.Add(4 * time.Hour)
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
			"Anthropic-Ratelimit-Unified-5h-Reset":  strconv.FormatInt(reset.Unix(), 10),
		})
		got := parseClaudeRateLimitResetAt(headers, now)
		if got == nil || !got.Equal(reset) {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want %v", got, reset)
		}
	})

	t.Run("7d reset honoured", func(t *testing.T) {
		reset := now.Add(6 * 24 * time.Hour)
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
			"Anthropic-Ratelimit-Unified-7d-Reset":  strconv.FormatInt(reset.Unix(), 10),
		})
		got := parseClaudeRateLimitResetAt(headers, now)
		if got == nil || !got.Equal(reset) {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want %v", got, reset)
		}
	})

	t.Run("latest deadline wins across multiple rejected windows", func(t *testing.T) {
		earlier := now.Add(4 * time.Hour)
		later := now.Add(6 * 24 * time.Hour)
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
			"Anthropic-Ratelimit-Unified-5h-Reset":  strconv.FormatInt(earlier.Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
			"Anthropic-Ratelimit-Unified-7d-Reset":  strconv.FormatInt(later.Unix(), 10),
		})
		got := parseClaudeRateLimitResetAt(headers, now)
		if got == nil || !got.Equal(later) {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want latest deadline %v", got, later)
		}
	})

	t.Run("Retry-After seconds honoured", func(t *testing.T) {
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-Status": "rejected",
			"Retry-After":                        "30",
		})
		got := parseClaudeRateLimitResetAt(headers, now)
		want := now.Add(30 * time.Second)
		if got == nil || !got.Equal(want) {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want %v", got, want)
		}
	})

	t.Run("fable-only 7d_oi reset is excluded when shared windows are allowed", func(t *testing.T) {
		reset := now.Add(2 * time.Hour)
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-Status":       "rejected",
			"Anthropic-Ratelimit-Unified-5h-Status":    "allowed",
			"Anthropic-Ratelimit-Unified-7d-Status":    "allowed_warning",
			"Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected",
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":  strconv.FormatInt(reset.Unix(), 10),
		})
		got := parseClaudeRateLimitResetAt(headers, now)
		if got != nil {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want nil (fable-only reset excluded)", got)
		}
	})

	t.Run("no headers returns nil", func(t *testing.T) {
		if got := parseClaudeRateLimitResetAt(nil, now); got != nil {
			t.Fatalf("parseClaudeRateLimitResetAt(nil, ...) = %v, want nil", got)
		}
	})

	t.Run("malformed reset value is ignored", func(t *testing.T) {
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
			"Anthropic-Ratelimit-Unified-5h-Reset":  "not-a-timestamp",
		})
		if got := parseClaudeRateLimitResetAt(headers, now); got != nil {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want nil for malformed reset", got)
		}
	})

	t.Run("past reset is ignored", func(t *testing.T) {
		past := now.Add(-time.Hour)
		headers := claudeHeaders(map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
			"Anthropic-Ratelimit-Unified-5h-Reset":  strconv.FormatInt(past.Unix(), 10),
		})
		if got := parseClaudeRateLimitResetAt(headers, now); got != nil {
			t.Fatalf("parseClaudeRateLimitResetAt() = %v, want nil for a past reset", got)
		}
	})
}

// TestGetHeaderCaseInsensitiveFallsBackToLinearScan proves the wire-shape and
// casing trap called out in the workstream brief: a caller must not assume
// canonical casing, and must take a single value out of the array-wrapped
// form http.Header always uses.
func TestGetHeaderCaseInsensitiveFallsBackToLinearScan(t *testing.T) {
	headers := http.Header{
		// Deliberately non-canonical casing, added directly to the map
		// (bypassing Set/Add's own canonicalization) to exercise the
		// fallback linear scan.
		"anthropic-ratelimit-unified-5h-status": []string{"rejected"},
	}
	got := getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-5h-Status")
	if got != "rejected" {
		t.Fatalf("getHeaderCaseInsensitive() = %q, want %q", got, "rejected")
	}
}

// TestParseUsageRetryHintsClaudeCase exercises the "claude" branch of
// parseUsageRetryHints end-to-end: a credential-scoped signal plus a 4h-out
// reset must be honoured, and body content must be irrelevant.
func TestParseUsageRetryHintsClaudeCase(t *testing.T) {
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
		"Anthropic-Ratelimit-Unified-5h-Reset":  strconv.FormatInt(time.Now().Add(4*time.Hour).Unix(), 10),
	})
	retryAfter, resetAt, credentialScope := parseUsageRetryHints("claude", "not json at all", http.StatusTooManyRequests, headers)
	if retryAfter != nil {
		t.Fatalf("retryAfter = %v, want nil (claude case never sets RetryAfter)", retryAfter)
	}
	if resetAt == nil {
		t.Fatalf("resetAt = nil, want a resolved deadline")
	}
	if !credentialScope {
		t.Fatalf("credentialScope = false, want true for an explicit 5h rejection")
	}

	// Non-429 status never parses headers, regardless of provider.
	if _, _, cs := parseUsageRetryHints("claude", "", http.StatusOK, headers); cs {
		t.Fatalf("parseUsageRetryHints() credentialScope = true for a non-429 status")
	}

	// Absent/empty headers falls back to nil, nil, false so the caller uses
	// generic backoff.
	if ra, rs, cs := parseUsageRetryHints("claude", "", http.StatusTooManyRequests, nil); ra != nil || rs != nil || cs {
		t.Fatalf("parseUsageRetryHints() with nil headers = (%v, %v, %v), want (nil, nil, false)", ra, rs, cs)
	}
}
