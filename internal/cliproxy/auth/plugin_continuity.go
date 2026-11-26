package auth

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// EvaluateSchedulerContinuity invokes the configured scheduler's optional observation hook.
func (m *Manager) EvaluateSchedulerContinuity(ctx context.Context, req pluginapi.SchedulerContinuityRequest) (pluginapi.SchedulerContinuityResponse, bool, error) {
	if errContext := ctx.Err(); errContext != nil {
		return pluginapi.SchedulerContinuityResponse{}, false, errContext
	}
	m.mu.RLock()
	scheduler := m.pluginScheduler
	m.mu.RUnlock()
	continuity, ok := scheduler.(interface {
		EvaluateSchedulerContinuity(context.Context, pluginapi.SchedulerContinuityRequest) (pluginapi.SchedulerContinuityResponse, bool, error)
	})
	if !ok {
		return pluginapi.SchedulerContinuityResponse{}, false, nil
	}
	return continuity.EvaluateSchedulerContinuity(ctx, req)
}

// CodexContinuityCandidates observes model/request eligibility without selecting
// credentials, reserving concurrency, or changing session bindings.
func (m *Manager) CodexContinuityCandidates(ctx context.Context, model string, opts Options, applyRetryRound bool) ([]pluginapi.SchedulerContinuityCandidate, error) {
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	policy, validPolicy := NormalizeCredentialPolicy(credentialPolicyFromOptions(opts))
	if !validPolicy {
		return nil, &Error{Code: "unsupported_credential_policy", Message: "unsupported credential policy"}
	}
	modelKey := canonicalModelKey(model)
	if !modelAllowedByID(modelKey, allowedModelIDsFromOptions(opts)) {
		return nil, nil
	}
	allowed := allowedAuthIDsFromOptions(opts)
	excluded := excludedAuthIDsFromOptions(opts)
	// Match dispatch and CPA: configured weighted selection excludes zero weight.
	m.mu.RLock()
	weighted := isWeightedSelector(m.selector)
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth != nil && strings.EqualFold(auth.Provider, "codex") && !auth.Disabled && auth.Status != StatusDisabled &&
			authAllowedByID(auth.ID, allowed) && credentialPolicyAllows(policy, auth) {
			if _, skip := excluded[auth.ID]; !skip {
				auths = append(auths, auth.Clone())
			}
		}
	}
	m.mu.RUnlock()
	defaultRetry := 0
	if cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config); cfg != nil {
		defaultRetry = cfg.RequestRetry
	}
	now := time.Now()
	out := make([]pluginapi.SchedulerContinuityCandidate, 0, len(auths))
	for _, auth := range auths {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		if weighted && authWeight(auth) <= 0 || isUnauthorizedAuthState(auth) || RefreshBlocksDispatch(auth) {
			continue
		}
		if applyRetryRound && !requestRetryRoundAllowed(auth, requestRetryRoundFromOptions(opts), defaultRetry) {
			continue
		}
		if !registry.GetGlobalRegistry().ClientSupportsModel(auth.ID, modelKey) {
			continue
		}
		resolved := m.resolveDispatchModel(auth, model)
		if resolved.Model == "" {
			continue
		}
		effective := m.effectiveAvailabilityAuth(auth, now)
		blockedModel, blocked, reason, next := dispatchModelBlockStatus(effective, resolved.Key, model, now)
		credentialQuota := effective.Quota.Exceeded && (len(effective.ModelStates) == 0 || effective.Quota.Reason == "credential_quota")
		quota := credentialQuota
		if credentialQuota && effective.Unavailable && effective.NextRetryAfter.After(now) && effective.LastError != nil && statusCodeFromResult(effective.LastError) != 429 {
			continue
		}
		if state := effective.ModelStates[canonicalModelKey(blockedModel)]; state != nil {
			if isUnauthorizedModelState(state) || state.Unavailable && state.NextRetryAfter.After(now) && state.LastError != nil && statusCodeFromResult(state.LastError) != 429 {
				continue
			}
			quota = quota || state.Quota.Exceeded
		}
		if blocked && (reason != blockReasonCooldown || !quota) {
			continue
		}
		item := pluginapi.SchedulerContinuityCandidate{
			ID: auth.ID, Provider: "codex", Priority: authPriority(auth), Status: string(auth.Status),
			InCooldown: blocked, Attributes: schedulerSafeAttributes(auth.Attributes),
		}
		if blocked {
			item.CooldownReason = "quota"
			if !next.IsZero() {
				deadline := next.UTC()
				item.CooldownUntil = &deadline
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
