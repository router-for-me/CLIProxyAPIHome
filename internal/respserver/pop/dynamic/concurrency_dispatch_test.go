package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

type concurrencyDispatchAdmitter struct {
	mu               sync.Mutex
	errors           map[string]error
	results          map[string]home.ConcurrencyAdmissionResult
	requests         []home.ConcurrencyAdmissionRequest
	calls            int
	defaultAccounted bool
}

func (a *concurrencyDispatchAdmitter) AdmitCredentialConcurrency(_ context.Context, req home.ConcurrencyAdmissionRequest) (home.ConcurrencyAdmissionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.requests = append(a.requests, req)
	if errResult := a.errors[req.CredentialID]; errResult != nil {
		return home.ConcurrencyAdmissionResult{}, errResult
	}
	if result, okResult := a.results[req.CredentialID]; okResult {
		return result, nil
	}
	return home.ConcurrencyAdmissionResult{Accounted: a.defaultAccounted, CredentialID: req.CredentialID, Model: req.Model}, nil
}

func (a *concurrencyDispatchAdmitter) SetResult(credentialID string, errResult error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errors[credentialID] = errResult
}

func (a *concurrencyDispatchAdmitter) SetAdmissionResult(credentialID string, result home.ConcurrencyAdmissionResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results[credentialID] = result
}

func (a *concurrencyDispatchAdmitter) Requests() []home.ConcurrencyAdmissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]home.ConcurrencyAdmissionRequest(nil), a.requests...)
}

func (a *concurrencyDispatchAdmitter) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func concurrencyError(errorType string) error {
	return &cluster.ConcurrencyAdmissionError{Type: errorType}
}

func newConcurrencyDispatchRuntime(t *testing.T, credentialIDs []string) (*home.Runtime, *concurrencyDispatchAdmitter) {
	t.Helper()
	rt := newAuthValidateRuntime(t, context.Background(), "dispatch-client-key", "deleted-dispatch-client-key")
	rt.CoreManager().SetFullAuthResolver(nil)
	rt.CoreManager().SetSelector(coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.FillFirstSelector{}}))
	admitter := &concurrencyDispatchAdmitter{errors: make(map[string]error), results: make(map[string]home.ConcurrencyAdmissionResult), defaultAccounted: true}
	rt.SetConcurrencyAdmitter(admitter)
	for _, credentialID := range credentialIDs {
		registry.GetGlobalRegistry().RegisterClient(credentialID, "openai", []*registry.ModelInfo{{ID: "gpt"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credentialID) })
		if _, errRegister := rt.CoreManager().Register(context.Background(), &coreauth.Auth{ID: credentialID, Provider: "openai"}); errRegister != nil {
			t.Fatalf("Register(%q) error = %v", credentialID, errRegister)
		}
	}
	return rt, admitter
}

func protocolOneDispatchEnv(runtime *home.Runtime, fingerprint string) dispatch.Env {
	return dispatch.Env{Runtime: runtime, ConnectionLifetime: cluster.ConnectionLifetime{
		Fingerprint: fingerprint,
		ConnectedAt: time.Now().UTC(),
		Controlled:  true,
	}}
}

func TestConcurrencyDispatchFixture(t *testing.T) {
	raw, errRead := os.ReadFile("../../testdata/concurrency_dispatch_accounted.json")
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}

	var want any
	if errUnmarshal := json.Unmarshal(raw, &want); errUnmarshal != nil {
		t.Fatalf("Unmarshal fixture error = %v", errUnmarshal)
	}
	var response struct {
		Concurrency dispatchConcurrencyResponse `json:"concurrency"`
	}
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("Unmarshal response error = %v", errUnmarshal)
	}
	if !response.Concurrency.Accounted || response.Concurrency.CredentialID != "cred-1" || response.Concurrency.Model != "gpt" {
		t.Fatalf("concurrency = %#v", response.Concurrency)
	}

	_, accounted, errPrepare := prepareDispatchResponse(&home.DispatchResult{
		Model:    "gpt",
		Provider: "codex",
		Auth: &coreauth.Auth{
			ID:       "cred-1",
			Index:    "cred-1",
			Provider: "codex",
			Status:   coreauth.StatusActive,
		},
		Concurrency: home.ConcurrencyAdmissionResult{Accounted: true, CredentialID: "cred-1", Model: "gpt"},
	}, "user-key")
	if errPrepare != nil {
		t.Fatalf("prepareDispatchResponse() error = %v", errPrepare)
	}
	var got any
	if errUnmarshal := json.Unmarshal(accounted, &got); errUnmarshal != nil {
		t.Fatalf("Unmarshal dispatch response error = %v", errUnmarshal)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatch response = %s, want %s", accounted, raw)
	}

	busyRaw, errRead := os.ReadFile("../../testdata/concurrency_dispatch_busy.json")
	if errRead != nil {
		t.Fatalf("ReadFile busy fixture error = %v", errRead)
	}
	var busyWant any
	if errUnmarshal := json.Unmarshal(busyRaw, &busyWant); errUnmarshal != nil {
		t.Fatalf("Unmarshal busy fixture error = %v", errUnmarshal)
	}
	var busyGot any
	busyResponse := buildDispatchErrorJSON(nil, &cluster.ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded", RetryAfterMS: 750})
	if errUnmarshal := json.Unmarshal([]byte(busyResponse), &busyGot); errUnmarshal != nil {
		t.Fatalf("Unmarshal busy response error = %v", errUnmarshal)
	}
	if !reflect.DeepEqual(busyGot, busyWant) {
		t.Fatalf("busy response = %s, want %s", busyResponse, busyRaw)
	}
}

func TestPrepareDispatchResponseIncludesForceMapping(t *testing.T) {
	result := &home.DispatchResult{
		Model:         "gemini-3-flash-agent",
		Provider:      "antigravity",
		ForceMapping:  true,
		OriginalAlias: "gemini-3.5-flash",
		Auth: &coreauth.Auth{
			ID:       "antigravity-auth",
			Index:    "antigravity-auth",
			Provider: "antigravity",
			Status:   coreauth.StatusActive,
		},
		Concurrency: home.ConcurrencyAdmissionResult{
			Accounted:    true,
			CredentialID: "antigravity-auth",
			Model:        "gemini-3-flash-agent",
		},
	}

	unaccounted, accounted, errPrepare := prepareDispatchResponse(result, "user-key")
	if errPrepare != nil {
		t.Fatalf("prepareDispatchResponse() error = %v", errPrepare)
	}
	for name, payload := range map[string][]byte{"unaccounted": unaccounted, "accounted": accounted} {
		if !gjson.GetBytes(payload, "force_mapping").Bool() {
			t.Fatalf("%s response = %s, want force_mapping true", name, payload)
		}
		if got := gjson.GetBytes(payload, "original_alias").String(); got != "gemini-3.5-flash" {
			t.Fatalf("%s original_alias = %q, want gemini-3.5-flash", name, got)
		}
	}
}

func TestHandleAuthSkipsSaturatedAffinityCandidate(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	admitter.SetResult("cred-a", concurrencyError("credential_concurrency_exceeded"))
	env := protocolOneDispatchEnv(runtime, "fp-a")
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	payload := string(reply.BulkString)
	if gjson.Get(payload, "auth.id").String() != "cred-b" {
		t.Fatalf("reply = %s", payload)
	}
	if !gjson.Get(payload, "concurrency.accounted").Bool() {
		t.Fatalf("reply = %s", payload)
	}
	if gjson.Get(payload, "concurrency.credential_id").String() != "cred-b" || gjson.Get(payload, "concurrency.model").String() != "gpt" {
		t.Fatalf("reply = %s", payload)
	}
}

func TestHandleAuthReturnsTypedBusyResponseWhenAllCandidatesAreSaturated(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	admitter.SetResult("cred-a", concurrencyError("credential_concurrency_exceeded"))
	admitter.SetResult("cred-b", concurrencyError("credential_concurrency_exceeded"))
	reply := handleAuth(context.Background(), protocolOneDispatchEnv(runtime, "fp-a"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	payload := string(reply.BulkString)
	if gjson.Get(payload, "error.type").String() != "credential_concurrency_exceeded" || !gjson.Get(payload, "error.retryable").Bool() || gjson.Get(payload, "error.retry_after_ms").Int() <= 0 {
		t.Fatalf("reply = %s", payload)
	}
}

func TestAuthValidateNeverCallsConcurrencyAdmitter(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	_ = handleAuthValidate(context.Background(), env, []string{"RPOP", "auth-validate", `{"type":"auth-validate"}`})
	if admitter.Calls() != 0 {
		t.Fatalf("admission calls = %d", admitter.Calls())
	}
}

func TestHandleAuthPreflightFailureDoesNotAdmit(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	env.Conn = &dispatch.ConnEnv{PrepareDispatchReply: func() error { return errors.New("preflight failed") }}
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if admitter.Calls() != 0 {
		t.Fatalf("admission calls = %d, want 0", admitter.Calls())
	}
	if gjson.Get(string(reply.BulkString), "error.message").String() != "preflight failed" {
		t.Fatalf("reply = %s", reply.BulkString)
	}
}

func TestHandleAuthPostAccountedReplyFailureIsFenced(t *testing.T) {
	runtime, _ := newConcurrencyDispatchRuntime(t, []string{"cred-a"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	env.Conn = &dispatch.ConnEnv{AccountedReplyFailure: func() error { return errors.New("tuple injection failed") }}
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if !reply.AccountedAdmission || reply.PreWriteError == nil {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestHandleAuthReturnsCPACompatibleCanonicalReleaseTupleForSuffix(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt(high)","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	payload := string(reply.BulkString)
	if gjson.Get(payload, "concurrency.credential_id").String() != "cred-a" || gjson.Get(payload, "concurrency.model").String() != "gpt" {
		t.Fatalf("reply = %s", payload)
	}
	requests := admitter.Requests()
	if len(requests) != 1 || requests[0].Model != "gpt" {
		t.Fatalf("admission requests = %#v", requests)
	}
}

func TestHandleAuthFencesAccountedAdmissionResultMismatch(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a"})
	admitter.SetAdmissionResult("cred-a", home.ConcurrencyAdmissionResult{Accounted: true, CredentialID: "other", Model: "other"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if !reply.AccountedAdmission || reply.PreWriteError == nil {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestHandleAuthSkipsProtocolRequiredCandidateWithoutAffinity(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	admitter.SetResult("cred-a", concurrencyError("concurrency_protocol_required"))
	admitter.SetAdmissionResult("cred-b", home.ConcurrencyAdmissionResult{CredentialID: "cred-b", Model: "gpt"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	env.ConnectionLifetime.Controlled = false
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key"}}`})
	if gjson.Get(string(reply.BulkString), "auth.id").String() != "cred-b" || gjson.Get(string(reply.BulkString), "concurrency.accounted").Exists() {
		t.Fatalf("reply = %s", reply.BulkString)
	}
}

func TestHandleAuthSkipsProtocolRequiredAffinityCandidate(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	env := protocolOneDispatchEnv(runtime, "fp-a")
	first := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if gjson.Get(string(first.BulkString), "auth.id").String() != "cred-a" {
		t.Fatalf("initial reply = %s", first.BulkString)
	}
	admitter.SetResult("cred-a", concurrencyError("concurrency_protocol_required"))
	admitter.SetAdmissionResult("cred-b", home.ConcurrencyAdmissionResult{CredentialID: "cred-b", Model: "gpt"})
	env.ConnectionLifetime.Controlled = false
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","headers":{"x-api-key":"dispatch-client-key"}}`})
	if gjson.Get(string(reply.BulkString), "auth.id").String() != "cred-b" || gjson.Get(string(reply.BulkString), "concurrency.accounted").Exists() {
		t.Fatalf("reply = %s", reply.BulkString)
	}
}

func TestHandleAuthReturnsProtocolRequiredWhenAllCandidatesRequireProtocol(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	admitter.SetResult("cred-a", concurrencyError("concurrency_protocol_required"))
	admitter.SetResult("cred-b", concurrencyError("concurrency_protocol_required"))
	env := protocolOneDispatchEnv(runtime, "fp-a")
	env.ConnectionLifetime.Controlled = false
	reply := handleAuth(context.Background(), env, []string{"RPOP", "auth", `{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key"}}`})
	if gjson.Get(string(reply.BulkString), "error.type").String() != "concurrency_protocol_required" {
		t.Fatalf("reply = %s", reply.BulkString)
	}
}

func TestConcurrencyRetryAfterMSUsesRuntimeConfigBounds(t *testing.T) {
	runtime, _ := newConcurrencyDispatchRuntime(t, []string{"cred-a"})
	cfg := runtime.Config()
	cfg.CredentialConcurrency.BusyRetryMin = "17ms"
	cfg.CredentialConcurrency.BusyRetryMax = "17ms"
	if errApply := runtime.ApplyConfigFromCluster(context.Background(), cfg); errApply != nil {
		t.Fatal(errApply)
	}
	if got := concurrencyRetryAfterMS(runtime, 0); got != 17 {
		t.Fatalf("retry after = %d, want 17", got)
	}
	cfg = runtime.Config()
	cfg.CredentialConcurrency.BusyRetryMin = "1ms"
	cfg.CredentialConcurrency.BusyRetryMax = "1ms"
	if errApply := runtime.ApplyConfigFromCluster(context.Background(), cfg); errApply != nil {
		t.Fatal(errApply)
	}
	if got := concurrencyRetryAfterMS(runtime, 0); got != 1 {
		t.Fatalf("retry after = %d, want 1", got)
	}
	cfg = runtime.Config()
	cfg.CredentialConcurrency.BusyRetryMin = "21ms"
	cfg.CredentialConcurrency.BusyRetryMax = "23ms"
	if errApply := runtime.ApplyConfigFromCluster(context.Background(), cfg); errApply != nil {
		t.Fatal(errApply)
	}
	for index := 0; index < 100; index++ {
		if got := concurrencyRetryAfterMS(runtime, 0); got < 21 || got > 23 {
			t.Fatalf("retry after = %d, want [21, 23]", got)
		}
	}
}
