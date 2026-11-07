package home

import (
	"context"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// RecordUsagePayload counts a usage event and applies its scheduler feedback.
func (r *Runtime) RecordUsagePayload(ctx context.Context, payload string) error {
	return r.recordUsagePayload(ctx, payload, false)
}

// ApplyUsagePayloadFeedback repairs scheduler feedback without counting an
// uncommitted or already counted delivery as a new request.
func (r *Runtime) ApplyUsagePayloadFeedback(ctx context.Context, payload string) error {
	return r.recordUsagePayload(ctx, payload, true)
}

func (r *Runtime) recordUsagePayload(ctx context.Context, payload string, skipRequestStats bool) error {
	// Validate input data before converting it into runtime state.
	if r == nil || r.coreManager == nil {
		return nil
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || !gjson.Valid(payload) {
		return fmt.Errorf("invalid usage json")
	}

	if feedback := coreauth.UsageFeedbackFromContext(ctx); feedback != nil && feedback.ScopeKey != "" {
		// Recovered feedback is authoritative even when retry routing fields are absent.
		result := feedback.Result
		result.SkipRequestStats = skipRequestStats
		return r.coreManager.MarkResult(ctx, result)
	}

	authIndex := strings.TrimSpace(gjson.Get(payload, "auth_index").String())
	if authIndex == "" {
		return nil
	}

	provider := strings.TrimSpace(gjson.Get(payload, "provider").String())
	model := strings.TrimSpace(gjson.Get(payload, "model").String())
	if coreauth.CanonicalModelID(model) == "" {
		return nil
	}

	statusCode := int(gjson.Get(payload, "fail.status_code").Int())
	if statusCode <= 0 {
		if gjson.Get(payload, "failed").Bool() {
			statusCode = 500
		} else {
			statusCode = 200
		}
	}
	body := gjson.Get(payload, "fail.body").String()
	failCode := strings.TrimSpace(gjson.Get(payload, "fail.code").String())

	result := coreauth.NewUsageResultWithCode(authIndex, provider, model, statusCode, body, failCode)
	result.AccessTokenSHA256 = strings.TrimSpace(gjson.Get(payload, "access_token_sha256").String())
	result.SkipRequestStats = skipRequestStats
	return r.coreManager.MarkResult(ctx, result)
}
