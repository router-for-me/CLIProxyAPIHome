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

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	}, "user-key", nil)
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

	unaccounted, accounted, errPrepare := prepareDispatchResponse(result, "user-key", nil)
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

func TestPrepareDispatchResponseIncludesModelInfo(t *testing.T) {
	result := &home.DispatchResult{
		Model:    "gemini-3.8-flash-high",
		Provider: "antigravity",
		ModelInfo: &home.DispatchModelInfo{
			ID:                         "gemini-3.8-flash-high",
			Type:                       "gemini",
			ContextLength:              1048576,
			SupportConfigurationUpdate: true,
			Thinking: &registry.ThinkingSupport{
				Levels: []string{"low", "medium", "high"},
			},
		},
		Auth: &coreauth.Auth{
			ID:       "antigravity-model-info-auth",
			Index:    "antigravity-model-info-auth",
			Provider: "antigravity",
			Status:   coreauth.StatusActive,
		},
	}

	unaccounted, accounted, errPrepare := prepareDispatchResponse(result, "user-key", nil)
	if errPrepare != nil {
		t.Fatalf("prepareDispatchResponse() error = %v", errPrepare)
	}
	for name, payload := range map[string][]byte{"unaccounted": unaccounted, "accounted": accounted} {
		if got := gjson.GetBytes(payload, "model_info.context_length").Int(); got != 1048576 {
			t.Fatalf("%s model context_length = %d, want 1048576; payload=%s", name, got, payload)
		}
		if got := gjson.GetBytes(payload, "model_info.thinking.levels.#").Int(); got != 3 {
			t.Fatalf("%s thinking level count = %d, want 3; payload=%s", name, got, payload)
		}
		if got := gjson.GetBytes(payload, "model_info.thinking.levels.2").String(); got != "high" {
			t.Fatalf("%s highest thinking level = %q, want high; payload=%s", name, got, payload)
		}
		if got := gjson.GetBytes(payload, "model_info.support_configuration_update"); !got.Exists() || !got.Bool() {
			t.Fatalf("%s dispatch response omitted selected configuration update capability: %s", name, payload)
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

type dispatchPluginSchedulerFunc func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)

func (f dispatchPluginSchedulerFunc) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return f(req)
}

func TestHandleAuthReturnsPluginRejectionAfterBusyCandidate(t *testing.T) {
	const busy, other = "plugin-busy-a", "plugin-busy-b"
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{busy, other})
	admitter.SetResult(busy, concurrencyError("credential_concurrency_exceeded"))
	runtime.CoreManager().SetPluginScheduler(dispatchPluginSchedulerFunc(func(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		for _, candidate := range req.Candidates {
			if candidate.ID == busy {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: busy}, true, nil
			}
		}
		return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: " denied ", RejectReason: " plugin refused selection "}, true, nil
	}))
	reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "fp-a"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	payload := string(reply.BulkString)
	if gjson.Get(payload, "error.type").String() != "scheduler_rejected" || gjson.Get(payload, "error.code").String() != "denied" ||
		gjson.Get(payload, "error.message").String() != "plugin refused selection" || gjson.Get(payload, "error.retryable").Bool() || reply.AccountedAdmission {
		t.Fatalf("reply = %s, want unaccounted, non-retryable plugin rejection", payload)
	}
	if got := admitter.Requests(); len(got) != 1 || got[0].CredentialID != busy {
		t.Fatalf("admissions = %+v, want only the busy credential", got)
	}
}

func TestHandleAuthPreservesCooldownAfterPluginExcludesLastReadyCredential(t *testing.T) {
	const ready, cooling = "plugin-ready", "plugin-cooling"
	for _, delegated := range []bool{false, true} {
		name := "fallback"
		if delegated {
			name = "delegation"
		}
		t.Run(name, func(t *testing.T) {
			runtime, admitter := newConcurrencyDispatchRuntime(t, []string{ready})
			registry.GetGlobalRegistry().RegisterClient(cooling, "openai", []*registry.ModelInfo{{ID: "gpt"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(cooling) })
			nextRetry := time.Now().Add(time.Minute)
			if _, errRegister := runtime.CoreManager().Register(t.Context(), &coreauth.Auth{
				ID: cooling, Provider: "openai", Status: coreauth.StatusActive,
				Metadata: map[string]any{"request_retry": 2},
				ModelStates: map[string]*coreauth.ModelState{"gpt": {
					Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: nextRetry,
					Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
				}},
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			runtime.CoreManager().SetPluginScheduler(dispatchPluginSchedulerFunc(func(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				if len(req.Candidates) != 1 || req.Candidates[0].ID != ready {
					t.Fatalf("plugin candidates = %+v, want only %s", req.Candidates, ready)
				}
				resp := pluginapi.SchedulerPickResponse{ExcludeCandidates: []string{ready}, Handled: delegated}
				if delegated {
					resp.DelegateBuiltin = pluginapi.SchedulerBuiltinFillFirst
				}
				return resp, delegated, nil
			}))
			reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "fp-a"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
			payload := string(reply.BulkString)
			retryAfter := gjson.Get(payload, "error.retry_after_ms").Int()
			if gjson.Get(payload, "error.type").String() != "model_cooldown" || retryAfter <= 0 || retryAfter > 60_000 ||
				gjson.Get(payload, "error.request_retry").Int() != 2 || admitter.Calls() != 0 {
				t.Fatalf("reply = %s, admissions = %d; want a cooldown response without admission", payload, admitter.Calls())
			}
		})
	}
}

func TestHandleAuthDelegatedPluginRebindsAfterConcurrencyExclusion(t *testing.T) {
	const bound, other = "plugin-rebind-a", "plugin-rebind-b"
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{bound, other})
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.FillFirstSelector{}, RebindOn: "any",
	})
	t.Cleanup(selector.Stop)
	runtime.CoreManager().SetSelector(selector)
	env := protocolOneDispatchEnv(runtime, "fp-affinity")
	args := []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`}
	checkPick := func(want string) {
		t.Helper()
		reply := handleAuth(t.Context(), env, args)
		if got := gjson.GetBytes(reply.BulkString, "auth.id").String(); got != want {
			t.Fatalf("reply = %s, want %s", reply.BulkString, want)
		}
	}
	checkPick(bound)
	admitter.SetResult(bound, concurrencyError("credential_concurrency_exceeded"))
	runtime.CoreManager().SetPluginScheduler(dispatchPluginSchedulerFunc(func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst}, true, nil
	}))
	checkPick(other)

	// The new binding survives even when the old credential becomes eligible again.
	admitter.SetResult(bound, nil)
	runtime.CoreManager().SetPluginScheduler(nil)
	checkPick(other)
}

func TestHandleAuthNeverRebindPreservesBoundCredentialSaturation(t *testing.T) {
	for _, delegate := range []bool{false, true} {
		name := "default"
		if delegate {
			name = "delegated"
		}
		for _, cooling := range []bool{false, true} {
			alt := "ready"
			if cooling {
				alt = "cooling"
			}
			t.Run(name+"/"+alt, func(t *testing.T) {
				checkNeverRebindSaturation(t, delegate, cooling)
			})
		}
	}
}

func checkNeverRebindSaturation(t *testing.T, delegate, cooling bool) {
	t.Helper()
	const bound, other = "plugin-affinity-a", "plugin-affinity-b"
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{bound, other})
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.FillFirstSelector{}, RebindOn: "never",
	})
	t.Cleanup(selector.Stop)
	runtime.CoreManager().SetSelector(selector)
	env := protocolOneDispatchEnv(runtime, "fp-affinity")
	args := []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`}
	first := handleAuth(t.Context(), env, args)
	if got := gjson.GetBytes(first.BulkString, "auth.id").String(); got != bound {
		t.Fatalf("initial reply = %s, want binding to %s", first.BulkString, bound)
	}

	if cooling {
		auth, ok := runtime.CoreManager().GetByID(other)
		if !ok {
			t.Fatalf("missing alternative credential %s", other)
		}
		nextRetry := time.Now().Add(time.Minute)
		auth.ModelStates = map[string]*coreauth.ModelState{"gpt": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: nextRetry,
			Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
		}}
		if _, errUpdate := runtime.CoreManager().Update(t.Context(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
	admitter.SetResult(bound, &cluster.ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded", RetryAfterMS: 5250})
	if delegate {
		runtime.CoreManager().SetPluginScheduler(dispatchPluginSchedulerFunc(func(pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
			return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst}, true, nil
		}))
	}
	reply := handleAuth(t.Context(), env, args)
	if gjson.GetBytes(reply.BulkString, "error.type").String() != "credential_concurrency_exceeded" ||
		gjson.GetBytes(reply.BulkString, "error.retry_after_ms").Int() != 5250 || reply.AccountedAdmission {
		t.Fatalf("reply = %s, want the original saturation and retry time", reply.BulkString)
	}
	if got := admitter.Requests(); len(got) != 2 || got[1].CredentialID != bound {
		t.Fatalf("admissions = %+v, want no admission for %s", got, other)
	}
	admitter.SetResult(bound, nil)
	runtime.CoreManager().SetPluginScheduler(nil)
	reply = handleAuth(t.Context(), env, args)
	if got := gjson.GetBytes(reply.BulkString, "auth.id").String(); got != bound {
		t.Fatalf("after capacity recovers, reply = %s; want original binding %s", reply.BulkString, bound)
	}
}

func TestHandleAuthNeverRebindPreservesBoundCredentialProtocolError(t *testing.T) {
	const bound, other = "protocol-bound-a", "protocol-other-b"
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{bound, other})
	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.FillFirstSelector{}, RebindOn: "never",
	})
	t.Cleanup(selector.Stop)
	runtime.CoreManager().SetSelector(selector)
	env := protocolOneDispatchEnv(runtime, "fp-protocol-bound")
	capableArgs := []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`}
	reply := handleAuth(t.Context(), env, capableArgs)
	if got := gjson.GetBytes(reply.BulkString, "auth.id").String(); got != bound {
		t.Fatalf("initial reply = %s, want binding to %s", reply.BulkString, bound)
	}

	admitter.SetResult(bound, concurrencyError("concurrency_protocol_required"))
	legacyArgs := []string{"RPOP", "auth", `{"type":"auth","model":"gpt","session_id":"sticky","headers":{"x-api-key":"dispatch-client-key"}}`}
	reply = handleAuth(t.Context(), env, legacyArgs)
	if gjson.GetBytes(reply.BulkString, "error.type").String() != "concurrency_protocol_required" || reply.AccountedAdmission {
		t.Fatalf("reply = %s, want the bound credential's protocol admission error", reply.BulkString)
	}
	if got := admitter.Requests(); len(got) != 2 || got[0].ProtocolVersion != 1 || got[1].ProtocolVersion != 0 || got[1].CredentialID != bound {
		t.Fatalf("admissions = %+v, want protocol 1 then 0 for %s only", got, bound)
	}

	// Restoring protocol support must reuse the original binding, not the alternative.
	admitter.SetResult(bound, nil)
	reply = handleAuth(t.Context(), env, capableArgs)
	if got := gjson.GetBytes(reply.BulkString, "auth.id").String(); got != bound {
		t.Fatalf("restored reply = %s, want original binding %s", reply.BulkString, bound)
	}
}

func TestHandleAuthPreservesPluginCooldownAfterProtocolRequired(t *testing.T) {
	const blocked, excluded, cooling = "plugin-protocol-a", "plugin-protocol-b", "plugin-protocol-c"
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{blocked, excluded})
	admitter.SetResult(blocked, concurrencyError("concurrency_protocol_required"))
	registry.GetGlobalRegistry().RegisterClient(cooling, "openai", []*registry.ModelInfo{{ID: "gpt"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(cooling) })
	nextRetry := time.Now().Add(time.Minute)
	if _, errRegister := runtime.CoreManager().Register(t.Context(), &coreauth.Auth{
		ID: cooling, Provider: "openai", Status: coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{"gpt": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: nextRetry,
			Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
		}},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	runtime.CoreManager().SetPluginScheduler(dispatchPluginSchedulerFunc(func(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		for _, candidate := range req.Candidates {
			if candidate.ID == blocked {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: blocked}, true, nil
			}
		}
		return pluginapi.SchedulerPickResponse{ExcludeCandidates: []string{excluded}}, false, nil
	}))
	reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "fp-protocol"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key"}}`})
	if gjson.GetBytes(reply.BulkString, "error.type").String() != "model_cooldown" || gjson.GetBytes(reply.BulkString, "error.retry_after_ms").Int() <= 0 {
		t.Fatalf("reply = %s, want cooldown instead of protocol error", reply.BulkString)
	}
	if got := admitter.Requests(); len(got) != 1 || got[0].CredentialID != blocked {
		t.Fatalf("admissions = %+v, want only %s", got, blocked)
	}
}

func TestHandleAuthRetryBudgetIncludesTemporarilyBusyCredential(t *testing.T) {
	for _, builtin := range []bool{false, true} {
		name := "session"
		if builtin {
			name = "builtin"
		}
		t.Run(name, func(t *testing.T) {
			const busy, fallback = "budget-a", "budget-b"
			runtime, admitter := newConcurrencyDispatchRuntime(t, []string{busy, fallback})
			if builtin {
				runtime.CoreManager().SetSelector(&coreauth.FillFirstSelector{})
			}
			for id, limit := range map[string]int{busy: 2, fallback: 0} {
				auth, ok := runtime.CoreManager().GetByID(id)
				if !ok {
					t.Fatalf("missing %s", id)
				}
				auth.Metadata = map[string]any{"request_retry": limit}
				if _, errUpdate := runtime.CoreManager().Update(t.Context(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			args := []string{"RPOP", "auth", `{"type":"auth","model":"gpt","retry_round":0,"concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`}
			env := protocolOneDispatchEnv(runtime, "budget-fp")
			for _, tc := range []struct {
				errorType string
				wantRetry int64
			}{
				{"credential_concurrency_exceeded", 2},
				{"credential_model_concurrency_exceeded", 2},
			} {
				admitter.SetResult(busy, &cluster.ConcurrencyAdmissionError{Type: tc.errorType, RetryAfterMS: 250})
				reply := handleAuth(t.Context(), env, args)
				if gjson.GetBytes(reply.BulkString, "auth.id").String() != fallback || gjson.GetBytes(reply.BulkString, "request_retry").Int() != tc.wantRetry {
					t.Fatalf("%s: reply = %s; want fallback credential and retry budget %d", tc.errorType, reply.BulkString, tc.wantRetry)
				}
			}
		})
	}
}

func TestHandleAuthChoosesEarlierConcurrencyRecoveryOverCooldown(t *testing.T) {
	for _, builtin := range []bool{false, true} {
		for _, tc := range []struct {
			name, admissionType, wantType string
			retryMS                       int64
			busyMin, busyMax              string
			wantMinMS, wantMaxMS          int64
		}{
			{"capacity first", "credential_concurrency_exceeded", "credential_concurrency_exceeded", 250, "", "", 250, 250},
			{"model capacity first", "credential_model_concurrency_exceeded", "credential_model_concurrency_exceeded", 250, "", "", 250, 250},
			{"default capacity hint", "credential_concurrency_exceeded", "credential_concurrency_exceeded", 0, "", "", 250, 1000},
			{"default model capacity hint", "credential_model_concurrency_exceeded", "credential_model_concurrency_exceeded", 0, "", "", 250, 1000},
			{"configured capacity hint", "credential_concurrency_exceeded", "credential_concurrency_exceeded", 0, "17ms", "17ms", 17, 17},
			{"configured model capacity hint", "credential_model_concurrency_exceeded", "credential_model_concurrency_exceeded", 0, "21ms", "23ms", 21, 23},
			{"explicit hint overrides default", "credential_concurrency_exceeded", "credential_concurrency_exceeded", 250, "2m", "2m", 250, 250},
			{"cooldown first", "credential_concurrency_exceeded", "model_cooldown", 120_000, "", "", 1, 60_000},
			{"cooldown before configured hint", "credential_concurrency_exceeded", "model_cooldown", 0, "2m", "2m", 1, 60_000},
			{"incompatible protocol", "concurrency_protocol_required", "model_cooldown", 0, "", "", 1, 60_000},
		} {
			path := "session/"
			if builtin {
				path = "builtin/"
			}
			t.Run(path+tc.name, func(t *testing.T) {
				const busy, cooling = "recovery-a", "recovery-b"
				runtime, admitter := newConcurrencyDispatchRuntime(t, []string{busy, cooling})
				if tc.busyMin != "" {
					cfg := runtime.Config()
					cfg.CredentialConcurrency.BusyRetryMin = tc.busyMin
					cfg.CredentialConcurrency.BusyRetryMax = tc.busyMax
					if errApply := runtime.ApplyConfigFromCluster(t.Context(), cfg); errApply != nil {
						t.Fatal(errApply)
					}
					// Config reload replaces the manually registered fixture credentials.
					for _, id := range []string{busy, cooling} {
						registry.GetGlobalRegistry().RegisterClient(id, "openai", []*registry.ModelInfo{{ID: "gpt"}})
						if _, errRegister := runtime.CoreManager().Register(t.Context(), &coreauth.Auth{ID: id, Provider: "openai"}); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
				}
				if builtin {
					runtime.CoreManager().SetSelector(&coreauth.FillFirstSelector{})
				}
				auth, ok := runtime.CoreManager().GetByID(cooling)
				if !ok {
					t.Fatal("missing cooling credential")
				}
				deadline := time.Now().Add(time.Minute)
				auth.ModelStates = map[string]*coreauth.ModelState{"gpt": {
					Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: deadline,
					Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: deadline},
				}}
				if _, errUpdate := runtime.CoreManager().Update(t.Context(), auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				admitter.SetResult(busy, &cluster.ConcurrencyAdmissionError{Type: tc.admissionType, RetryAfterMS: tc.retryMS})
				reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "recovery-fp"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
				if gjson.GetBytes(reply.BulkString, "error.type").String() != tc.wantType || reply.AccountedAdmission {
					t.Fatalf("reply = %s, want %s without admission", reply.BulkString, tc.wantType)
				}
				retryMS := gjson.GetBytes(reply.BulkString, "error.retry_after_ms").Int()
				if retryMS < tc.wantMinMS || retryMS > tc.wantMaxMS {
					t.Fatalf("reply = %s, unexpected retry timing", reply.BulkString)
				}
				if got := admitter.Requests(); len(got) != 1 || got[0].CredentialID != busy {
					t.Fatalf("admissions = %+v, want only %s", got, busy)
				}
				if tc.retryMS == 0 && tc.wantType != "model_cooldown" {
					_, errDispatch := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", nil, "", "", nil, "", home.DispatchConcurrencyContext{ProtocolVersion: 1})
					var timed interface{ RetryAfter() *time.Duration }
					if !errors.As(errDispatch, &timed) || timed.RetryAfter() == nil {
						t.Fatalf("error = %v, want effective retry hint", errDispatch)
					}
					// Serialization must reuse the sampled hint, even without a runtime config.
					serialized := buildDispatchErrorJSON(nil, errDispatch)
					if got := gjson.Get(serialized, "error.retry_after_ms").Int(); got != timed.RetryAfter().Milliseconds() {
						t.Fatalf("serialized = %s, want the exact sampled hint %v", serialized, *timed.RetryAfter())
					}
					var original *cluster.ConcurrencyAdmissionError
					if !errors.As(errDispatch, &original) || original.RetryAfterMS != 0 {
						t.Fatalf("error = %v, want unchanged underlying zero-hint admission error", errDispatch)
					}
				}
			})
		}
	}
}

func TestHandleAuthProtocolErrorNotHiddenByEarlierCapacityError(t *testing.T) {
	const busy, incompatible = "precedence-a", "precedence-b"
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{busy, incompatible})
	admitter.SetResult(busy, &cluster.ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded", RetryAfterMS: 250})
	admitter.SetResult(incompatible, concurrencyError("concurrency_protocol_required"))
	reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "precedence-fp"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key"}}`})
	if gjson.GetBytes(reply.BulkString, "error.type").String() != "concurrency_protocol_required" ||
		gjson.GetBytes(reply.BulkString, "error.retry_after_ms").Exists() || reply.AccountedAdmission {
		t.Fatalf("reply = %s, want protocol incompatibility without capacity timing", reply.BulkString)
	}
	if got := admitter.Requests(); len(got) != 2 || got[0].CredentialID != busy || got[1].CredentialID != incompatible {
		t.Fatalf("admissions = %+v, want busy then incompatible", got)
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
	if got := runtime.ConcurrencyRetryAfterMS(0); got != 17 {
		t.Fatalf("retry after = %d, want 17", got)
	}
	cfg = runtime.Config()
	cfg.CredentialConcurrency.BusyRetryMin = "1ms"
	cfg.CredentialConcurrency.BusyRetryMax = "1ms"
	if errApply := runtime.ApplyConfigFromCluster(context.Background(), cfg); errApply != nil {
		t.Fatal(errApply)
	}
	if got := runtime.ConcurrencyRetryAfterMS(0); got != 1 {
		t.Fatalf("retry after = %d, want 1", got)
	}
	cfg = runtime.Config()
	cfg.CredentialConcurrency.BusyRetryMin = "21ms"
	cfg.CredentialConcurrency.BusyRetryMax = "23ms"
	if errApply := runtime.ApplyConfigFromCluster(context.Background(), cfg); errApply != nil {
		t.Fatal(errApply)
	}
	for index := 0; index < 100; index++ {
		if got := runtime.ConcurrencyRetryAfterMS(0); got < 21 || got > 23 {
			t.Fatalf("retry after = %d, want [21, 23]", got)
		}
	}
}
