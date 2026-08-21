package dynamic

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func TestHandleAuthExcludesCredentialsFromCurrentRetryRound(t *testing.T) {
	runtime := newAuthValidateRuntime(t, context.Background(), "dispatch-client-key", "deleted-dispatch-client-key")
	runtime.CoreManager().SetFullAuthResolver(nil)
	runtime.CoreManager().SetSelector(&coreauth.RoundRobinSelector{})
	runtime.CoreManager().SetConfig(&appconfig.Config{RequestRetry: 0})
	for _, authID := range []string{"retry-auth-a", "retry-auth-b"} {
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		retryLimit := 0
		if authID == "retry-auth-b" {
			retryLimit = 2
		}
		if _, errRegister := runtime.CoreManager().Register(context.Background(), &coreauth.Auth{
			ID:         authID,
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"api_key": authID},
			Metadata:   map[string]any{"request_retry": retryLimit},
		}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", authID, errRegister)
		}
	}
	aggregate := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","excluded_auth_ids":["retry-auth-b"],"headers":{"x-api-key":"dispatch-client-key"}}`})
	aggregatePayload := string(aggregate.BulkString)
	if authID := gjson.Get(aggregatePayload, "auth.id").String(); authID != "retry-auth-a" {
		t.Fatalf("aggregate dispatch selected %q, want retry-auth-a; body=%s", authID, aggregatePayload)
	}
	if got := gjson.Get(aggregatePayload, "request_retry"); !got.Exists() || got.Int() != 2 {
		t.Fatalf("aggregate dispatch request_retry = %s, want 2; body=%s", got.Raw, aggregatePayload)
	}

	legacyRetry := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","count":2,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if got := gjson.Get(string(legacyRetry.BulkString), "error.type").String(); got != "request_retry_exceeded" {
		t.Fatalf("legacy count=2 error type = %q, want request_retry_exceeded; body=%s", got, string(legacyRetry.BulkString))
	}
	for _, malformedExclusions := range []string{"null", `{}`, `[1,"retry-auth-a"]`} {
		payload := `{"type":"auth","model":"gpt","count":2,"excluded_auth_ids":` + malformedExclusions + `,"headers":{"x-api-key":"dispatch-client-key"}}`
		reply := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", payload})
		if got := gjson.Get(string(reply.BulkString), "error.type").String(); got != "error" {
			t.Fatalf("malformed excluded_auth_ids=%s error type = %q, want error; body=%s", malformedExclusions, got, string(reply.BulkString))
		}
	}

	emptyExclusions := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","count":2,"excluded_auth_ids":[],"headers":{"x-api-key":"dispatch-client-key"}}`})
	if authID := gjson.Get(string(emptyExclusions.BulkString), "auth.id").String(); authID == "" {
		t.Fatalf("new retry contract with empty exclusions did not dispatch an auth: %s", string(emptyExclusions.BulkString))
	}

	pinned := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","excluded_auth_ids":[],"pinned_auth_id":"retry-auth-b","headers":{"x-api-key":"dispatch-client-key"}}`})
	if authID := gjson.Get(string(pinned.BulkString), "auth.id").String(); authID != "retry-auth-b" {
		t.Fatalf("pinned dispatch selected %q, want retry-auth-b; body=%s", authID, string(pinned.BulkString))
	}

	malformedPinned := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","excluded_auth_ids":[],"pinned_auth_id":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if got := gjson.Get(string(malformedPinned.BulkString), "error.type").String(); got == "" {
		t.Fatalf("malformed pinned_auth_id did not return a structured error: %s", string(malformedPinned.BulkString))
	}

	first := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key"}}`})
	firstID := gjson.Get(string(first.BulkString), "auth.id").String()
	if firstID == "" {
		t.Fatalf("first dispatch omitted auth id: %s", string(first.BulkString))
	}
	secondPayload := `{"type":"auth","model":"gpt","count":2,"excluded_auth_ids":["` + firstID + `"],"headers":{"x-api-key":"dispatch-client-key"}}`
	second := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", secondPayload})
	secondID := gjson.Get(string(second.BulkString), "auth.id").String()
	if secondID == "" || secondID == firstID {
		t.Fatalf("second dispatch selected %q after excluding %q: %s", secondID, firstID, string(second.BulkString))
	}
}

func TestHandleAuthRetryRoundProtocolValidation(t *testing.T) {
	runtime := newAuthValidateRuntime(t, context.Background(), "dispatch-client-key", "deleted-dispatch-client-key")
	runtime.CoreManager().SetFullAuthResolver(nil)
	runtime.CoreManager().SetSelector(&coreauth.RoundRobinSelector{})
	runtime.CoreManager().SetConfig(&appconfig.Config{RequestRetry: 3})
	const authID = "retry-round-wire-auth"
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	if _, errRegister := runtime.CoreManager().Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": authID},
		Metadata:   map[string]any{"request_retry": 3},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	validPayloads := []string{
		`{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key"}}`,
		`{"type":"auth","model":"gpt","retry_round":0,"headers":{"x-api-key":"dispatch-client-key"}}`,
		`{"type":"auth","model":"gpt","retry_round":1,"headers":{"x-api-key":"dispatch-client-key"}}`,
	}
	for _, payload := range validPayloads {
		reply := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", payload})
		if authIDGot := gjson.Get(string(reply.BulkString), "auth.id").String(); authIDGot != authID {
			t.Fatalf("payload %s selected auth %q, want %q; body=%s", payload, authIDGot, authID, string(reply.BulkString))
		}
	}
	for _, raw := range []string{"-1", "1.5", `"1"`, "null", "true"} {
		payload := `{"type":"auth","model":"gpt","retry_round":` + raw + `,"headers":{"x-api-key":"dispatch-client-key"}}`
		reply := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", payload})
		if got := gjson.Get(string(reply.BulkString), "error.type").String(); got != "error" {
			t.Fatalf("retry_round=%s error type = %q, want error; body=%s", raw, got, string(reply.BulkString))
		}
	}
}

func TestBuildDispatchErrorJSONPreservesModelCooldownRetryAfter(t *testing.T) {
	tests := []struct {
		name         string
		retryAfter   time.Duration
		wantMillisec int64
	}{
		{name: "whole milliseconds", retryAfter: 1500 * time.Millisecond, wantMillisec: 1500},
		{name: "fractional milliseconds", retryAfter: 1500 * time.Microsecond, wantMillisec: 2},
		{name: "positive sub-millisecond", retryAfter: 500 * time.Microsecond, wantMillisec: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := buildDispatchErrorJSON(nil, dispatchRetryAfterTestError{retryAfter: tc.retryAfter})
			if got := gjson.Get(payload, "error.type").String(); got != "model_cooldown" {
				t.Fatalf("error.type = %q, want model_cooldown; body=%s", got, payload)
			}
			if !gjson.Get(payload, "error.retryable").Bool() {
				t.Fatalf("error.retryable = false, want true; body=%s", payload)
			}
			if got := gjson.Get(payload, "error.retry_after_ms").Int(); got != tc.wantMillisec {
				t.Fatalf("error.retry_after_ms = %d, want %d; body=%s", got, tc.wantMillisec, payload)
			}
		})
	}
}

func TestHandleAuthCooldownIncludesCredentialRequestRetryLimit(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "retry-home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	nextRetry := time.Now().Add(time.Second)
	repo := cluster.NewRepository(db)
	if _, errUpsert := repo.UpsertAuth(ctx, &coreauth.Auth{
		ID:       "retry-cooling-auth",
		Index:    "retry-cooling-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"request_retry": 2, "access_token": "must-not-be-projected"},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: nextRetry,
				Quota:          coreauth.QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
			},
		},
	}, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	adapter := cluster.NewRuntimeAdapter(repo, "192.0.2.10")
	if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
		t.Fatalf("LoadIndex() error = %v", errLoad)
	}
	minimals := adapter.ListMinimalAuths()
	if len(minimals) != 1 || minimals[0] == nil {
		t.Fatalf("ListMinimalAuths() = %#v, want one auth", minimals)
	}
	minimal := minimals[0]
	if _, exists := minimal.Metadata["access_token"]; exists {
		t.Fatalf("minimal metadata exposed access_token: %#v", minimal.Metadata)
	}

	runtime := newAuthValidateRuntime(t, ctx, "dispatch-client-key", "deleted-dispatch-client-key")
	runtime.CoreManager().SetFullAuthResolver(nil)
	runtime.CoreManager().SetConfig(&appconfig.Config{RequestRetry: 0})
	registry.GetGlobalRegistry().RegisterClient("retry-cooling-auth", "codex", []*registry.ModelInfo{{ID: "gpt"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("retry-cooling-auth") })
	if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(ctx), minimal); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	reply := handleAuth(ctx, dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","excluded_auth_ids":[],"headers":{"x-api-key":"dispatch-client-key"}}`})
	payload := string(reply.BulkString)
	if got := gjson.Get(payload, "error.type").String(); got != "model_cooldown" {
		t.Fatalf("error.type = %q, want model_cooldown; body=%s", got, payload)
	}
	if got := gjson.Get(payload, "error.request_retry").Int(); got != 2 {
		t.Fatalf("error.request_retry = %d, want 2; body=%s", got, payload)
	}
	if got := gjson.Get(payload, "error.retry_after_ms").Int(); got <= 0 {
		t.Fatalf("error.retry_after_ms = %d, want positive; body=%s", got, payload)
	}
}

type dispatchRetryAfterTestError struct {
	retryAfter time.Duration
}

func (dispatchRetryAfterTestError) Error() string { return "model cooldown" }

func (dispatchRetryAfterTestError) ErrorCode() string { return "model_cooldown" }

func (dispatchRetryAfterTestError) ErrorMessage() string { return "all credentials are cooling down" }

func (e dispatchRetryAfterTestError) RetryAfter() *time.Duration {
	value := e.retryAfter
	return &value
}
