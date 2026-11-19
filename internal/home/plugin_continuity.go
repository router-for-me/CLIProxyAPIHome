package home

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// EvaluateSchedulerContinuity exposes the Home-loaded plugin's optional hook to the manager.
func (r *Runtime) EvaluateSchedulerContinuity(ctx context.Context, req pluginapi.SchedulerContinuityRequest) (pluginapi.SchedulerContinuityResponse, bool, error) {
	if r == nil || r.pluginHost == nil {
		return pluginapi.SchedulerContinuityResponse{}, false, nil
	}
	return r.pluginHost.EvaluateSchedulerContinuity(ctx, req)
}

// SchedulerContinuityQuery is a read-only observation, never a dispatch/admission.
type SchedulerContinuityQuery struct {
	Request                   pluginapi.SchedulerContinuityRequest `json:"request"`
	CredentialPolicy          string                               `json:"credential_policy,omitempty"`
	RetryRound                int                                  `json:"retry_round"`
	ApplyRetryRoundExclusions bool                                 `json:"apply_retry_round_exclusions"`
	ExcludedAuthIDs           []string                             `json:"excluded_auth_ids,omitempty"`
	PinnedAuthID              string                               `json:"pinned_auth_id,omitempty"`
}

// ObserveSchedulerContinuity replaces all caller-provided candidates and permission
// metadata with the same authoritative API-key/model/pin restrictions as dispatch.
func (r *Runtime) ObserveSchedulerContinuity(ctx context.Context, query SchedulerContinuityQuery, apiKey string) (pluginapi.SchedulerContinuityResponse, bool, error) {
	empty := pluginapi.SchedulerContinuityResponse{}
	if r == nil || r.coreManager == nil {
		return empty, false, fmt.Errorf("home runtime: core manager is nil")
	}
	req := query.Request
	if req.Phase != "pre_admission" && req.Phase != "pre_output_429" || query.RetryRound < 0 || req.Attempt < 0 || req.ElapsedMs < 0 {
		return empty, false, &coreauth.Error{Code: "invalid_request", Message: "invalid scheduler continuity context"}
	}
	model := strings.TrimSpace(req.Model)
	if req.RequestedModel == "" {
		req.RequestedModel = model
	}
	allowed, models, errAllowed := r.allowedDispatchIDsForAPIKey(ctx, apiKey, model)
	if errAllowed != nil {
		return empty, false, errAllowed
	}
	metadata := map[string]any{
		coreauth.CredentialPolicyMetadataKey:  query.CredentialPolicy,
		coreauth.RequestRetryRoundMetadataKey: query.RetryRound,
		coreauth.ExcludedAuthIDsMetadataKey:   query.ExcludedAuthIDs,
		"request_id":                          req.RequestID, "trace_id": req.TraceID,
		"execution_session_id": req.ExecutionSessionID, "canonical_session_id": req.CanonicalSessionID,
		coreauth.RequestedModelMetadataKey: req.RequestedModel,
	}
	if allowed != nil {
		metadata[coreauth.AllowedAuthIDsMetadataKey] = allowed
	}
	if models != nil {
		metadata[coreauth.AllowedModelIDsMetadataKey] = models
	}
	if query.PinnedAuthID != "" {
		metadata[coreauth.PinnedAuthIDMetadataKey] = query.PinnedAuthID
	}
	opts := coreauth.Options{Headers: http.Header(req.Options.Headers), Stream: req.Stream, Metadata: metadata}
	candidates, errCandidates := r.coreManager.CodexContinuityCandidates(ctx, model, opts, query.ApplyRetryRoundExclusions)
	if errCandidates != nil {
		return empty, false, errCandidates
	}
	if len(candidates) == 0 {
		return empty, false, nil
	}
	req.Provider, req.Providers = "codex", []string{"codex"}
	req.Model, req.Candidates = model, candidates
	req.Plugin = pluginapi.Metadata{}
	req.Options = pluginapi.SchedulerOptions{Headers: opts.Headers.Clone(), Metadata: metadata}
	response, handled, errEvaluate := r.coreManager.EvaluateSchedulerContinuity(ctx, req)
	if errEvaluate != nil || !handled {
		// CPA disables continuity on callback failure or decline. An unhandled
		// wire response is reserved for the local empty-candidate path above.
		return pluginapi.SchedulerContinuityResponse{Decision: pluginapi.SchedulerContinuityDisabled}, true, nil
	}
	return response, true, nil
}
