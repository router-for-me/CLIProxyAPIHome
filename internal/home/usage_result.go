package home

import (
	"context"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// RecordUsagePayload applies downstream usage status to the scheduler auth state.
func (r *Runtime) RecordUsagePayload(ctx context.Context, payload string) {
	// Validate input data before converting it into runtime state.
	if r == nil || r.coreManager == nil {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || !gjson.Valid(payload) {
		return
	}

	authIndex := strings.TrimSpace(gjson.Get(payload, "auth_index").String())
	if authIndex == "" {
		return
	}

	provider := strings.TrimSpace(gjson.Get(payload, "provider").String())
	model := strings.TrimSpace(gjson.Get(payload, "model").String())
	if model == "" {
		model = strings.TrimSpace(gjson.Get(payload, "alias").String())
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

	r.coreManager.MarkResult(ctx, coreauth.NewUsageResult(authIndex, provider, model, statusCode, body))
}
