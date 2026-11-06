package home

import (
	"context"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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
