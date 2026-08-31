package home

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func newUsageResultTestRuntime(t *testing.T, auth *coreauth.Auth) *Runtime {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
	}
	return &Runtime{coreManager: manager}
}

func TestRecordUsagePayloadUsesModelAsUpstreamKey(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-model-auth",
		Index:    "usage-model-index",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-model-index",
		"provider": "gemini",
		"model": "upstream-model",
		"alias": "alias-model",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "quota exhausted"
		}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["upstream-model"]
	if state == nil {
		t.Fatalf("ModelStates[upstream-model] missing after usage payload: %#v", got.ModelStates)
	}
	if _, exists := got.ModelStates["alias-model"]; exists {
		t.Fatalf("ModelStates contains alias-model, want upstream model only: %#v", got.ModelStates)
	}
	if !state.Unavailable || !state.Quota.Exceeded || state.NextRetryAfter.IsZero() {
		t.Fatalf("ModelStates[upstream-model] = %#v, want 429 cooldown state", state)
	}
}

func TestRecordUsagePayloadUsesAntigravityQuotaResetTimestamp(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-reset-timestamp-auth",
		Index:    "usage-reset-timestamp-index",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	resetAt := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Millisecond)
	body := fmt.Sprintf(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"quotaResetTimeStamp":%q}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]}}`, resetAt.Format(time.RFC3339Nano))
	encodedBody, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatalf("json.Marshal(error body) error = %v", errMarshal)
	}
	rt.RecordUsagePayload(context.Background(), fmt.Sprintf(`{
		"auth_index": "usage-reset-timestamp-index",
		"provider": "antigravity",
		"model": "gemini-3.7-flash-high",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": %s
		}
	}`, encodedBody))

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gemini-3.7-flash-high"]
	if state == nil {
		t.Fatalf("ModelStates[gemini-3.7-flash-high] missing after usage payload: %#v", got.ModelStates)
	}
	if !state.NextRetryAfter.Equal(resetAt) {
		t.Fatalf("ModelStates[gemini-3.7-flash-high].NextRetryAfter = %v, want %v", state.NextRetryAfter, resetAt)
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("Quota.NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("Quota.BackoffLevel = %d, want first exponential level", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadUsesRetryDelayWhenAntigravityResetTimestampMissing(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-missing-reset-timestamp-auth",
		Index:    "usage-missing-reset-timestamp-index",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	before := time.Now()
	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-missing-reset-timestamp-index",
		"provider": "antigravity",
		"model": "gemini-3.7-flash-high",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "{\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.ErrorInfo\",\"reason\":\"QUOTA_EXHAUSTED\"},{\"@type\":\"type.googleapis.com/google.rpc.RetryInfo\",\"retryDelay\":\"45m\"}]}}"
		}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gemini-3.7-flash-high"]
	if state == nil {
		t.Fatalf("ModelStates[gemini-3.7-flash-high] missing after usage payload: %#v", got.ModelStates)
	}
	delay := state.NextRetryAfter.Sub(before)
	if delay < 45*time.Minute || delay > 46*time.Minute {
		t.Fatalf("ModelStates[gemini-3.7-flash-high].NextRetryAfter delay = %v, want retryDelay", delay)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("Quota.BackoffLevel = %d, want first exponential level", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadUsesCodexQuotaResetTimestamp(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-codex-reset-auth",
		Index:    "usage-codex-reset-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	resetAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	body := fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":%d,"resets_in_seconds":30}}`, resetAt.Unix())
	encodedBody, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatalf("json.Marshal(error body) error = %v", errMarshal)
	}
	rt.RecordUsagePayload(context.Background(), fmt.Sprintf(`{
		"auth_index": "usage-codex-reset-index",
		"provider": "codex",
		"model": "gpt-5",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": %s
		}
	}`, encodedBody))

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gpt-5"]
	if state == nil {
		t.Fatalf("ModelStates[gpt-5] missing after usage payload: %#v", got.ModelStates)
	}
	if !state.NextRetryAfter.Equal(resetAt) {
		t.Fatalf("ModelStates[gpt-5].NextRetryAfter = %v, want %v", state.NextRetryAfter, resetAt)
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("Quota.NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("Quota.BackoffLevel = %d, want first exponential level", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadUsesRetryDelayWhenCodexResetTimestampMissing(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-codex-relative-auth",
		Index:    "usage-codex-relative-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	before := time.Now()
	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-codex-relative-index",
		"provider": "codex",
		"model": "gpt-5",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "{\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"The usage limit has been reached\",\"resets_in_seconds\":600}}"
		}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gpt-5"]
	if state == nil {
		t.Fatalf("ModelStates[gpt-5] missing after usage payload: %#v", got.ModelStates)
	}
	delay := state.NextRetryAfter.Sub(before)
	if delay < 9*time.Minute || delay > 11*time.Minute {
		t.Fatalf("ModelStates[gpt-5].NextRetryAfter delay = %v, want ~10m", delay)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("Quota.BackoffLevel = %d, want first exponential level", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadTransientCodexRateLimitKeepsExponentialBackoff(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-codex-transient-auth",
		Index:    "usage-codex-transient-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	before := time.Now()
	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-codex-transient-index",
		"provider": "codex",
		"model": "gpt-5",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "{\"error\":{\"type\":\"rate_limit_error\",\"resets_in_seconds\":999999}}"
		}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gpt-5"]
	if state == nil {
		t.Fatalf("ModelStates[gpt-5] missing after usage payload: %#v", got.ModelStates)
	}
	delay := state.NextRetryAfter.Sub(before)
	if delay < 500*time.Millisecond || delay > 3*time.Second {
		t.Fatalf("ModelStates[gpt-5].NextRetryAfter delay = %v, want first exponential backoff (1s)", delay)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("Quota.BackoffLevel = %d, want first exponential level", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadCodexHintsExceedingMaxHorizonClampedToThirtyMinutes(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-codex-horizon-auth",
		Index:    "usage-codex-horizon-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	// resets_at > 30m (e.g. 72 hours), and resets_in_seconds > 30m (e.g. 72 hours)
	farFuture := time.Now().UTC().Add(72 * time.Hour)
	before := time.Now()
	rt.RecordUsagePayload(context.Background(), fmt.Sprintf(`{
		"auth_index": "usage-codex-horizon-index",
		"provider": "codex",
		"model": "gpt-5",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "{\"error\":{\"type\":\"usage_limit_reached\",\"resets_at\":%d,\"resets_in_seconds\":259200}}"
		}
	}`, farFuture.Unix()))

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gpt-5"]
	if state == nil {
		t.Fatalf("ModelStates[gpt-5] missing after usage payload: %#v", got.ModelStates)
	}
	delay := state.NextRetryAfter.Sub(before)
	if delay < 29*time.Minute || delay > 31*time.Minute {
		t.Fatalf("ModelStates[gpt-5].NextRetryAfter delay = %v, want clamped 30m horizon", delay)
	}
	if state.Quota.BackoffLevel != 1 {
		t.Fatalf("Quota.BackoffLevel = %d, want first exponential level", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadIgnoresUnauthorizedFromOlderToken(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-stale-auth",
		Index:    "usage-stale-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "current-access-token"},
	}
	rt := newUsageResultTestRuntime(t, auth)
	oldHash := coreauth.AccessTokenSHA256(&coreauth.Auth{Metadata: map[string]any{"access_token": "old-access-token"}})

	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-stale-index",
		"provider": "codex",
		"model": "gpt-5",
		"access_token_sha256": "`+oldHash+`",
		"failed": true,
		"fail": {"status_code": 401, "body": "expired access token"}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	if state := got.ModelStates["gpt-5"]; state != nil && state.Unavailable {
		t.Fatalf("late 401 from an older token changed current state: %#v", state)
	}
}

func TestRecordUsagePayloadIgnoresMissingModel(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-missing-model-auth",
		Index:    "usage-missing-model-index",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-missing-model-index",
		"provider": "gemini",
		"failed": true,
		"fail": {"status_code": 429, "body": "quota exhausted"}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth", auth.ID)
	}
	if len(got.ModelStates) != 0 || got.Status != coreauth.StatusActive || got.Unavailable || got.Quota.Exceeded {
		t.Fatalf("auth changed for model-less result: %#v", got)
	}
}

func TestRecordUsagePayloadIgnoresAliasWithoutModel(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-alias-auth",
		Index:    "usage-alias-index",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-alias-index",
		"provider": "gemini",
		"model": "",
		"alias": "alias-model",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "quota exhausted"
		}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	if len(got.ModelStates) != 0 || got.Status != coreauth.StatusActive || got.Unavailable || got.Quota.Exceeded {
		t.Fatalf("alias-only result changed auth: %#v", got)
	}
}
