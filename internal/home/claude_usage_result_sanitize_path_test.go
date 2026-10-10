package home_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

// TestRecordUsagePayloadHonoursClaudeHeadersThroughRealSanitizePath is the
// end-to-end test that would have caught the defect this fix closes: a
// Claude 429 payload carrying real-shaped Anthropic rate-limit headers,
// pushed through cluster.SanitizeUsagePayloadSecrets FIRST and only then
// into Runtime.RecordUsagePayload -- the exact order
// internal/respserver/push/usage.go's handleUsage uses in production
// (sanitize, reassign payload, THEN RecordUsagePayload(ctx, payload)).
//
// Before this fix, sanitizeUsageQuotaHeaders
// (internal/cluster/quota_ingestion.go) unconditionally deleted
// "response_headers" for every provider and only ever re-attached a
// filtered subset under "quota_headers" for provider=="codex" -- so a
// Claude payload's rate-limit headers never survived to reach
// RecordUsagePayload, and this exact assertion (blocked until the real
// multi-hour reset, not the 1s exponential floor) would have failed.
//
// This file is `package home_test` (external test package), not `package
// home`, because internal/cluster imports internal/home in production code
// (refresh.go, runtime.go) -- an internal `package home` test file
// importing internal/cluster is a genuine import cycle, verified via `go
// vet` during development, not assumed.
func TestRecordUsagePayloadHonoursClaudeHeadersThroughRealSanitizePath(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-claude-sanitize-path-auth",
		Index:    "usage-claude-sanitize-path-index",
		Provider: "claude",
		Status:   coreauth.StatusActive,
	}
	rt := home.NewUsageResultTestRuntime(t, auth)

	reset := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)
	rawPayload := fmt.Sprintf(`{
		"auth_index": "usage-claude-sanitize-path-index",
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
			"Anthropic-Ratelimit-Unified-7d-Status": ["allowed"],
			"Authorization": ["Bearer must-not-survive-sanitizing"]
		}
	}`, strconv.FormatInt(reset.Unix(), 10))

	sanitized, errSanitize := cluster.SanitizeUsagePayloadSecrets(rawPayload)
	if errSanitize != nil {
		t.Fatalf("SanitizeUsagePayloadSecrets() error = %v", errSanitize)
	}

	rt.RecordUsagePayload(context.Background(), sanitized)

	got, ok := rt.CoreManager().GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["claude-opus-4-1"]
	if state == nil {
		t.Fatalf("ModelStates[claude-opus-4-1] missing after usage payload: %#v", got.ModelStates)
	}
	if !state.NextRetryAfter.Equal(reset) {
		t.Fatalf("ModelStates[claude-opus-4-1].NextRetryAfter = %v, want the header reset %v (through the real sanitize path)", state.NextRetryAfter, reset)
	}
	if delay := state.NextRetryAfter.Sub(time.Now()); delay < 3*time.Hour {
		t.Fatalf("NextRetryAfter delay = %v, looks like the 1s exponential floor re-armed instead of the 4h header reset", delay)
	}
}
