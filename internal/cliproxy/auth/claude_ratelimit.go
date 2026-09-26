package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// claudeAllowedRateLimitStatus reports whether a unified rate-limit window
// status value indicates the window is not currently rejecting requests.
func claudeAllowedRateLimitStatus(status string) bool {
	return status == "allowed" || status == "allowed_warning"
}

// claudeFableOnlyRejection reports whether only the Fable-specific 7d_oi
// window is rejected while both shared windows (5h, 7d) are explicitly
// allowed.
func claudeFableOnlyRejection(status5h, status7d, status7dOI string) bool {
	return claudeAllowedRateLimitStatus(status5h) && claudeAllowedRateLimitStatus(status7d) && status7dOI == "rejected"
}

// claudeHeadersIndicateUnifiedRateLimitRejection reports whether response
// headers explicitly declare an Anthropic shared 5h or 7d rate-limit
// rejection -- i.e. a credential-scoped rejection, not one limited to a
// single model. A Fable-only 7d_oi rejection stays model-scoped when both
// shared windows are explicitly allowed (status "allowed"/"allowed_warning").
//
// Mirrors CPA's own
// internal/runtime/executor/helps/claude_ratelimit.go:ClaudeHeadersIndicateUnifiedRateLimitRejection.
func claudeHeadersIndicateUnifiedRateLimitRejection(headers http.Header) bool {
	if headers == nil {
		return false
	}
	unifiedStatus := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-Status")))
	status5h := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-5h-Status")))
	if status5h == "rejected" {
		return true
	}
	status7d := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d-Status")))
	if status7d == "rejected" {
		return true
	}
	if unifiedStatus != "rejected" {
		return false
	}
	status7dOI := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d_oi-Status")))
	return !claudeFableOnlyRejection(status5h, status7d, status7dOI)
}

// parseClaudeRateLimitResetAt inspects Anthropic response headers for shared
// and Fable-specific unified rate-limit windows plus the standard
// Retry-After header, returning the latest applicable absolute deadline
// across every explicitly rejected window. Returns nil when no valid future
// reset information is present, so the caller falls back to generic
// exponential backoff -- same as CPA.
//
// This function itself is deterministic: it takes `now` as an explicit
// parameter rather than reading the clock internally, so its own unit tests
// can pin a fixed instant. Its only caller today (parseUsageRetryHints's
// "claude" case, in result.go) passes real time.Now().UTC() at the call
// site -- same as every other provider's 429 path -- so that determinism is
// local to this function's own tests, not a claim that Claude 429 handling
// avoids wall-clock time end-to-end.
//
// Deliberately does NOT add CPA's crypto/rand 1-30s fuzz grace period
// either: injecting real randomness here would remove even this function's
// own test determinism (upstream's AGENTS.md asks for controllable clocks
// over wall-clock reads in TTL/expiration-style unit tests). If a fuzz
// window is wanted later, its randomness source must be injectable the same
// way `now` already is.
//
// Mirrors CPA's own
// internal/runtime/executor/helps/claude_ratelimit.go:parseClaudeRateLimitResetWithFuzz,
// minus the fuzz, and returns an absolute time.Time instead of a duration so
// it maps directly onto Home's Result.ResetAt field.
func parseClaudeRateLimitResetAt(headers http.Header, now time.Time) *time.Time {
	if headers == nil {
		return nil
	}
	unifiedStatus := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-Status")))
	status5h := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-5h-Status")))
	status7d := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d-Status")))
	status7dOI := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d_oi-Status")))
	fableOnlyRejection := claudeFableOnlyRejection(status5h, status7d, status7dOI)

	var candidateDeadlines []time.Time

	// 1. Retry-After header (relative seconds or an HTTP-date).
	if raw := getHeaderCaseInsensitive(headers, "Retry-After"); raw != "" {
		if t, ok := parseClaudeRetryAfterHeader(raw, now); ok && t.After(now) {
			candidateDeadlines = append(candidateDeadlines, t)
		}
	}
	// 2. 5-hour window reset (only when that window is rejected).
	if status5h == "rejected" {
		if raw := getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-5h-Reset"); raw != "" {
			if t, ok := parseClaudeUnixOrTimestamp(raw); ok && t.After(now) {
				candidateDeadlines = append(candidateDeadlines, t)
			}
		}
	}
	// 3. 7-day window reset (only when that window is rejected).
	if status7d == "rejected" {
		if raw := getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d-Reset"); raw != "" {
			if t, ok := parseClaudeUnixOrTimestamp(raw); ok && t.After(now) {
				candidateDeadlines = append(candidateDeadlines, t)
			}
		}
	}
	// 4. Fable-specific 7-day window reset (only when rejected and not a
	// Fable-only rejection -- a pure Fable-only rejection is model-scoped
	// and is handled by the ordinary per-model backoff ladder instead).
	if status7dOI == "rejected" && !fableOnlyRejection {
		if raw := getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-7d_oi-Reset"); raw != "" {
			if t, ok := parseClaudeUnixOrTimestamp(raw); ok && t.After(now) {
				candidateDeadlines = append(candidateDeadlines, t)
			}
		}
	}
	// 5. Generic unified reset header, when any window looks rejected (or the
	// status headers are entirely absent but neither shared window is
	// explicitly allowed).
	unifiedRejected := !fableOnlyRejection && (unifiedStatus == "rejected" || status5h == "rejected" || status7d == "rejected" || status7dOI == "rejected" ||
		(unifiedStatus == "" && !claudeAllowedRateLimitStatus(status5h) && !claudeAllowedRateLimitStatus(status7d)))
	if unifiedRejected {
		if raw := getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-Reset"); raw != "" {
			if t, ok := parseClaudeUnixOrTimestamp(raw); ok && t.After(now) {
				candidateDeadlines = append(candidateDeadlines, t)
			}
		}
	}

	if len(candidateDeadlines) == 0 {
		return nil
	}
	var latestDeadline time.Time
	for _, deadline := range candidateDeadlines {
		if deadline.After(latestDeadline) {
			latestDeadline = deadline
		}
	}
	if latestDeadline.IsZero() || !latestDeadline.After(now) {
		return nil
	}
	latestDeadline = latestDeadline.UTC()
	return &latestDeadline
}

// getHeaderCaseInsensitive returns the first value for a header, trying an
// exact (canonical-form) lookup first and falling back to a case-insensitive
// linear scan. A real net/http response canonicalizes header keys, so the
// exact lookup is expected to succeed in production; the fallback mirrors
// CPA's own defensive posture rather than assuming that always holds.
func getHeaderCaseInsensitive(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if v := headers.Get(name); v != "" {
		return v
	}
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		if strings.EqualFold(key, name) {
			return values[0]
		}
	}
	return ""
}

// parseClaudeUnixOrTimestamp parses a header value as float seconds since the
// Unix epoch, then RFC3339, then any format http.ParseTime accepts.
func parseClaudeUnixOrTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Unix(0, int64(seconds*float64(time.Second))).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// parseClaudeRetryAfterHeader parses a Retry-After header as either seconds
// (a relative duration added to now) or an HTTP-date.
func parseClaudeRetryAfterHeader(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return now.Add(time.Duration(seconds * float64(time.Second))).UTC(), true
	}
	if t, err := http.ParseTime(raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
