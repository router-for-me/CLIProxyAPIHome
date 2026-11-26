package dynamic

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/tidwall/gjson"
)

func TestHandleAuthSchedulerContextAndSelectionID(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"wire-auth"})
	var picked pluginapi.SchedulerPickRequest
	runtime.CoreManager().SetPluginScheduler(dispatchPluginSchedulerFunc(func(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		picked = req
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "wire-auth"}, true, nil
	}))
	reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "wire-fp"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"scheduler_context":{"stream":true,"request_id":"request-1","trace_id":"trace-1","execution_session_id":"execution-1"},"headers":{"x-api-key":"dispatch-client-key"}}`})
	id := gjson.GetBytes(reply.BulkString, "selection_id").String()
	if id == "" || picked.Options.Metadata[pluginapi.SelectionIDMetadataKey] != id || !picked.Stream || picked.Options.Metadata["request_id"] != "request-1" || picked.Options.Metadata["trace_id"] != "trace-1" || picked.Options.Metadata["execution_session_id"] != "execution-1" {
		t.Fatalf("reply=%s request=%+v", reply.BulkString, picked)
	}
	if admitter.Calls() != 1 {
		t.Fatalf("admissions=%d, want 1", admitter.Calls())
	}
	for _, input := range []string{`{"stream":"true"}`, `[]`, `1`} {
		reply = handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "wire-fp"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","scheduler_context":` + input + `,"headers":{"x-api-key":"dispatch-client-key"}}`})
		if !gjson.GetBytes(reply.BulkString, "error").Exists() || admitter.Calls() != 1 {
			t.Fatalf("invalid context accepted: %s", reply.BulkString)
		}
	}
}

type continuityContractScheduler struct {
	requests []pluginapi.SchedulerContinuityRequest
}

func (*continuityContractScheduler) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{}, false, nil
}
func (s *continuityContractScheduler) EvaluateSchedulerContinuity(_ context.Context, req pluginapi.SchedulerContinuityRequest) (pluginapi.SchedulerContinuityResponse, bool, error) {
	s.requests = append(s.requests, req)
	return pluginapi.SchedulerContinuityResponse{Decision: pluginapi.SchedulerContinuityWait, PollIntervalMs: 17, WaitBudgetMs: 100}, true, nil
}

func TestSchedulerContinuityUsesAuthoritativeCandidatesWithoutAdmission(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"observe-auth"})
	auth, ok := runtime.CoreManager().GetByID("observe-auth")
	if !ok {
		t.Fatal("missing auth")
	}
	auth.Provider = "codex"
	deadline := time.Now().Add(time.Minute)
	auth.ModelStates = map[string]*coreauth.ModelState{"gpt": {Unavailable: true, NextRetryAfter: deadline, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: deadline}}}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt"}})
	if _, errUpdate := runtime.CoreManager().Update(t.Context(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	scheduler := &continuityContractScheduler{}
	runtime.CoreManager().SetPluginScheduler(scheduler)
	query := home.SchedulerContinuityQuery{Request: pluginapi.SchedulerContinuityRequest{
		Model: "gpt", Phase: "pre_admission", Stream: true,
		Options:    pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Api-Key": {"dispatch-client-key"}}, Metadata: map[string]any{"allowed_auth_ids": []string{"forged"}}},
		Candidates: []pluginapi.SchedulerContinuityCandidate{{ID: "forged"}},
	}}
	payload, errMarshal := json.Marshal(query)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	reply := handleSchedulerContinuity(t.Context(), protocolOneDispatchEnv(runtime, "observe-fp"), []string{"RPOP", string(payload)})
	if !gjson.GetBytes(reply.BulkString, "handled").Bool() || gjson.GetBytes(reply.BulkString, "response.decision").String() != "wait" || len(scheduler.requests) != 1 || admitter.Calls() != 0 {
		t.Fatalf("reply=%s observations=%d admissions=%d", reply.BulkString, len(scheduler.requests), admitter.Calls())
	}
	request := scheduler.requests[0]
	if len(request.Candidates) != 1 || request.Candidates[0].ID != auth.ID || !request.Candidates[0].InCooldown || !request.Stream {
		t.Fatalf("observation=%+v", request)
	}
	if ids, _ := request.Options.Metadata[coreauth.AllowedAuthIDsMetadataKey].([]string); len(ids) == 1 && ids[0] == "forged" {
		t.Fatal("caller permission metadata reached plugin")
	}
	query.Request.Options.Headers = map[string][]string{"X-Api-Key": {"invalid"}}
	payload, errMarshal = json.Marshal(query)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	reply = handleSchedulerContinuity(t.Context(), protocolOneDispatchEnv(runtime, "observe-fp"), []string{"RPOP", string(payload)})
	if !gjson.GetBytes(reply.BulkString, "error").Exists() || len(scheduler.requests) != 1 {
		t.Fatalf("unauthenticated observation reached plugin: %s", reply.BulkString)
	}
}
