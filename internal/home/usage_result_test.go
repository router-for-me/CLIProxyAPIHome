package home

import (
	"context"
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

func TestRecordUsagePayloadUsesRetryDelayFromFailBody(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "usage-retry-delay-auth",
		Index:    "usage-retry-delay-index",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
	}
	rt := newUsageResultTestRuntime(t, auth)

	before := time.Now()
	rt.RecordUsagePayload(context.Background(), `{
		"auth_index": "usage-retry-delay-index",
		"provider": "gemini",
		"model": "gemini-3-pro-preview",
		"failed": true,
		"fail": {
			"status_code": 429,
			"body": "{\"error\":{\"code\":429,\"message\":\"Individual quota reached. Contact your administrator to enable overages. Resets in 45m7s.\",\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.ErrorInfo\",\"reason\":\"QUOTA_EXHAUSTED\",\"metadata\":{\"quotaResetDelay\":\"2707.893713377s\",\"quotaResetTimeStamp\":\"2026-06-23T17:45:04Z\"}},{\"@type\":\"type.googleapis.com/google.rpc.RetryInfo\",\"retryDelay\":\"2707.893713377s\"}]}}"
		}
	}`)

	got, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) missing auth after usage payload", auth.ID)
	}
	state := got.ModelStates["gemini-3-pro-preview"]
	if state == nil {
		t.Fatalf("ModelStates[gemini-3-pro-preview] missing after usage payload: %#v", got.ModelStates)
	}
	delay := state.NextRetryAfter.Sub(before)
	if delay < 45*time.Minute || delay > 46*time.Minute {
		t.Fatalf("ModelStates[gemini-3-pro-preview].NextRetryAfter delay = %v, want about 45m7s", delay)
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("Quota.NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
	if state.Quota.BackoffLevel != 0 {
		t.Fatalf("Quota.BackoffLevel = %d, want 0 when explicit retry delay is used", state.Quota.BackoffLevel)
	}
}

func TestRecordUsagePayloadFallsBackToAliasWhenModelEmpty(t *testing.T) {
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
	state := got.ModelStates["alias-model"]
	if state == nil {
		t.Fatalf("ModelStates[alias-model] missing after usage payload: %#v", got.ModelStates)
	}
	if !state.Unavailable || !state.Quota.Exceeded || state.NextRetryAfter.IsZero() {
		t.Fatalf("ModelStates[alias-model] = %#v, want 429 cooldown state", state)
	}
}
