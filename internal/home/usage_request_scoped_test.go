package home

import (
	"context"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestRecordUsagePayloadRequestScopedPreservesAvailability(t *testing.T) {
	for _, existingCooldown := range []bool{false, true} {
		name := "available"
		if existingCooldown {
			name = "existing_cooldown"
		}
		t.Run(name, func(t *testing.T) {
			auth := &coreauth.Auth{ID: "request-scoped-auth", Index: "request-scoped-index", Provider: "codex", Status: coreauth.StatusActive}
			if existingCooldown {
				next := time.Now().Add(time.Hour)
				auth.Unavailable = true
				auth.Status = coreauth.StatusError
				auth.NextRetryAfter = next
				auth.ModelStates = map[string]*coreauth.ModelState{
					"gpt-6-sol": {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: next, StatusMessage: "previous quota failure"},
				}
			}
			rt := newUsageResultTestRuntime(t, auth)
			before, _ := rt.coreManager.GetByID(auth.ID)
			// Use a model-support message to verify that the explicit request scope
			// takes precedence over heuristic classification by error text.
			rt.RecordUsagePayload(context.Background(), `{
				"auth_index":"request-scoped-index","provider":"codex","model":"gpt-6-sol","failed":true,
				"fail":{"status_code":400,"body":"model is not supported for this request","code":"request_scoped"}
			}`)
			after, ok := rt.coreManager.GetByID(auth.ID)
			if !ok {
				t.Fatal("credential disappeared after request-scoped usage")
			}
			if !reflect.DeepEqual(before.ModelStates, after.ModelStates) || before.Unavailable != after.Unavailable || before.Status != after.Status || !before.NextRetryAfter.Equal(after.NextRetryAfter) || !reflect.DeepEqual(before.LastError, after.LastError) || !reflect.DeepEqual(before.Quota, after.Quota) {
				t.Fatalf("request-scoped usage changed availability: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestRecordUsagePayloadModelSupportErrorStillCoolsModel(t *testing.T) {
	auth := &coreauth.Auth{ID: "model-support-auth", Index: "model-support-index", Provider: "codex", Status: coreauth.StatusActive}
	rt := newUsageResultTestRuntime(t, auth)
	rt.RecordUsagePayload(context.Background(), `{
		"auth_index":"model-support-index","provider":"codex","model":"gpt-6-sol","failed":true,
		"fail":{"status_code":400,"body":"model is not supported for this account"}
	}`)
	after, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || after.ModelStates["gpt-6-sol"] == nil || !after.ModelStates["gpt-6-sol"].NextRetryAfter.After(time.Now()) {
		t.Fatalf("unclassified upstream model support error did not cool the model: %#v", after)
	}
}

func TestRecordUsagePayloadForceCooldownOverridesDisableCoolingAndStatus400(t *testing.T) {
	disableCooling := true
	for _, payload := range []string{
		`{"auth_index":"force-cool-index","provider":"codex","model":"gpt-6-sol","failed":true,"fail":{"status_code":400,"body":"custom cooldown match","code":"force_cooldown"}}`,
		`{"auth_index":"force-cool-index","provider":"codex","model":"gpt-6-sol","failed":true,"fail":{"status_code":429,"body":"custom cooldown match","code":"force_cooldown"}}`,
		`{"auth_index":"force-cool-index","provider":"codex","model":"gpt-6-sol","failed":true,"fail":{"status_code":503,"body":"custom cooldown match","code":"force_cooldown"}}`,
	} {
		auth := &coreauth.Auth{
			ID:                    "force-cool-auth",
			Index:                 "force-cool-index",
			Provider:              "codex",
			Status:                coreauth.StatusActive,
			RuntimeDisableCooling: &disableCooling,
		}
		rt := newUsageResultTestRuntime(t, auth)
		rt.RecordUsagePayload(context.Background(), payload)
		after, ok := rt.coreManager.GetByID(auth.ID)
		if !ok || after.ModelStates["gpt-6-sol"] == nil || !after.ModelStates["gpt-6-sol"].Unavailable || !after.ModelStates["gpt-6-sol"].NextRetryAfter.After(time.Now()) {
			t.Fatalf("force_cooldown usage did not cool the model when cooling was disabled: payload=%s after=%#v", payload, after)
		}
	}
}

func TestRecordUsagePayloadMatchesAuthRequestScopedRulesWhenFailCodeOmitted(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "rule-fallback-auth",
		Index:    "rule-fallback-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"request_scoped_errors": []any{
				map[string]any{
					"status": 404,
					"match":  []any{"custom_not_found"},
					"action": "stop",
				},
			},
		},
	}
	rt := newUsageResultTestRuntime(t, auth)
	rt.RecordUsagePayload(context.Background(), `{
		"auth_index":"rule-fallback-index","provider":"codex","model":"gpt-6-sol","failed":true,
		"fail":{"status_code":404,"body":"upstream returned custom_not_found for this route"}
	}`)
	after, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || after.Unavailable || len(after.ModelStates) != 0 {
		t.Fatalf("expected Home-side request-scoped rule match to skip 404 cooldown even when fail.code is empty, got %#v", after)
	}
}

func TestRecordUsagePayloadForceCooldownPreservedWhenClearingSiblingDisabledCooldown(t *testing.T) {
	disableCooling := true
	auth := &coreauth.Auth{
		ID:                    "force-cool-sibling-auth",
		Index:                 "force-cool-sibling-index",
		Provider:              "codex",
		Status:                coreauth.StatusActive,
		RuntimeDisableCooling: &disableCooling,
		ModelStates: map[string]*coreauth.ModelState{
			"model-normal": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(10 * time.Minute),
				LastError:      &coreauth.Error{HTTPStatus: 503, Message: "ordinary 503"},
			},
		},
	}
	rt := newUsageResultTestRuntime(t, auth)
	rt.RecordUsagePayload(context.Background(), `{"auth_index":"force-cool-sibling-index","provider":"codex","model":"model-forced","failed":true,"fail":{"status_code":429,"body":"forced rate limit","code":"force_cooldown"}}`)
	after, ok := rt.coreManager.GetByID(auth.ID)
	if !ok || after.ModelStates["model-forced"] == nil || !after.ModelStates["model-forced"].Unavailable || !after.ModelStates["model-forced"].NextRetryAfter.After(time.Now()) {
		t.Fatalf("model-forced lost its forced 429 cooldown when sibling disabled cooldown was cleared: %#v", after)
	}
	if after.ModelStates["model-normal"] != nil && after.ModelStates["model-normal"].Unavailable {
		t.Fatalf("expected sibling model-normal disabled cooldown to be cleared, got %#v", after.ModelStates["model-normal"])
	}

	// Verify that explicit force_cooldown also overrides the built-in 404 store=false heuristic.
	rt.RecordUsagePayload(context.Background(), `{"auth_index":"force-cool-sibling-index","provider":"codex","model":"model-404-forced","failed":true,"fail":{"status_code":404,"body":"item with id msg_123 not found: items are not persisted when `+"`store`"+` is set to false","code":"force_cooldown"}}`)
	after404, _ := rt.coreManager.GetByID(auth.ID)
	if after404.ModelStates["model-404-forced"] == nil || !after404.ModelStates["model-404-forced"].Unavailable || !after404.ModelStates["model-404-forced"].NextRetryAfter.After(time.Now()) {
		t.Fatalf("expected force_cooldown to override 404 store=false heuristic, got %#v", after404.ModelStates["model-404-forced"])
	}
}

func TestRecordUsagePayloadOAuthGlobalRulesMatchOnRuntimeProjection(t *testing.T) {
	// OAuth credential whose kind is inferred from OAuth token metadata on the
	// full record, projected to a minimal runtime Auth via cluster index.
	full := &coreauth.Auth{
		ID:       "oauth-projection-auth",
		Index:    "oauth-projection-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token":  "tok-1",
			"refresh_token": "ref-1",
		},
	}
	if full.AuthKind() != coreauth.AuthKindOAuth {
		t.Fatalf("expected full auth kind %q, got %q", coreauth.AuthKindOAuth, full.AuthKind())
	}
	projected := &coreauth.Auth{
		ID:                full.ID,
		Index:             full.Index,
		Provider:          full.Provider,
		Status:            full.Status,
		RuntimeProjection: true,
		Attributes: map[string]string{
			"auth_kind": full.AuthKind(),
		},
	}
	rt := newUsageResultTestRuntime(t, projected)
	cfg := &config.Config{
		OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{
			"codex": {
				{
					Status: 404,
					Match:  []string{"items are not persisted when `store` is set to false"},
					Action: "stop-and-cooldown",
				},
			},
		},
	}
	rt.coreManager.SetConfig(cfg)

	rt.RecordUsagePayload(context.Background(), `{"auth_index":"oauth-projection-index","provider":"codex","model":"gpt-6-sol","failed":true,"fail":{"status_code":404,"body":"item with id msg_123 not found: items are not persisted when `+"`store`"+` is set to false"}}`)
	after, ok := rt.coreManager.GetByID(projected.ID)
	if !ok || after.ModelStates["gpt-6-sol"] == nil || !after.ModelStates["gpt-6-sol"].Unavailable || !after.ModelStates["gpt-6-sol"].NextRetryAfter.After(time.Now()) {
		t.Fatalf("expected OAuth global stop-and-cooldown rule on minimal projection to cool the model, got %#v", after)
	}
}
