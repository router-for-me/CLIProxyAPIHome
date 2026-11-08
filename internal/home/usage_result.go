package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// RecordUsagePayload counts a usage event and applies its scheduler feedback.
func (r *Runtime) RecordUsagePayload(ctx context.Context, payload string) error {
	return r.recordUsagePayload(ctx, payload, false)
}

// ApplyUsagePayloadFeedback applies scheduler feedback for a redelivered usage
// event without counting it as a new request. The result state machine fences
// replays by event time, so applying an already-processed event is a no-op.
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
	result.ObservedAt = usagePayloadObservedAt(payload)
	result.SkipRequestStats = skipRequestStats
	errMark := r.coreManager.MarkResult(ctx, result)
	if !errors.Is(errMark, coreauth.ErrAuthNotLoaded) {
		return errMark
	}
	// The credential can exist in the database before this Home has applied
	// the cluster event that introduces it. Load the authoritative record once;
	// a credential that no longer exists keeps the delivery acknowledged.
	if r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return nil
	}
	auth, errLoad := r.clusterAdapter.GetFullAuth(ctx, authIndex)
	if errors.Is(errLoad, coreauth.ErrFullAuthNotFound) || (errLoad == nil && auth == nil) {
		return nil
	}
	if errLoad != nil {
		return errLoad
	}
	r.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(ctx), auth)
	return r.coreManager.MarkResult(ctx, result)
}

// usagePayloadObservedAt returns the completion time of the reported execution,
// or zero when the payload carries no parsable timestamp.
func usagePayloadObservedAt(payload string) time.Time {
	raw := strings.TrimSpace(gjson.Get(payload, "timestamp").String())
	if raw == "" {
		return time.Time{}
	}
	started, errParse := time.Parse(time.RFC3339Nano, raw)
	if errParse != nil {
		return time.Time{}
	}
	return started.Add(time.Duration(gjson.Get(payload, "latency_ms").Int()) * time.Millisecond).UTC()
}
