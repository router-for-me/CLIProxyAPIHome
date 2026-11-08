package home

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// usageFeedbackMaxClockSkew bounds how far a CPA completion may lead Home.
const usageFeedbackMaxClockSkew = 5 * time.Minute

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

	observedAt, valid := usagePayloadObservedAt(payload, time.Now())
	if !valid {
		// Acknowledge malformed timing without applying scheduler feedback, rather
		// than poisoning the fence or turning timing errors into ingestion retries.
		return nil
	}
	result := coreauth.NewUsageResultWithCode(authIndex, provider, model, statusCode, body, failCode)
	result.AccessTokenSHA256 = strings.TrimSpace(gjson.Get(payload, "access_token_sha256").String())
	result.ObservedAt = observedAt
	result.SkipRequestStats = skipRequestStats
	errMark := r.coreManager.MarkResult(ctx, result)
	if errors.Is(errMark, coreauth.ErrAuthNotFound) || errors.Is(errMark, coreauth.ErrFullAuthNotFound) {
		return nil
	}
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
	errRetry := r.coreManager.MarkResult(ctx, result)
	if errors.Is(errRetry, coreauth.ErrAuthNotFound) || errors.Is(errRetry, coreauth.ErrFullAuthNotFound) {
		return nil
	}
	return errRetry
}

// usagePayloadObservedAt validates the execution completion time. Accepted times
// are never clamped to delivery time, so their identity stays stable on redelivery.
// Legacy payloads without a timestamp remain untimed.
func usagePayloadObservedAt(payload string, now time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(gjson.Get(payload, "timestamp").String())
	if raw == "" {
		return time.Time{}, true
	}
	started, errParse := time.Parse(time.RFC3339Nano, raw)
	if errParse != nil {
		return time.Time{}, false
	}
	var latencyMS int64
	if latency := gjson.Get(payload, "latency_ms"); latency.Exists() {
		var errLatency error
		latencyMS, errLatency = strconv.ParseInt(latency.String(), 10, 64)
		if errLatency != nil || latencyMS < 0 || latencyMS > int64(1<<63-1)/int64(time.Millisecond) {
			return time.Time{}, false
		}
	}
	completed := started.Add(time.Duration(latencyMS) * time.Millisecond).UTC()
	if completed.After(now.Add(usageFeedbackMaxClockSkew)) {
		return time.Time{}, false
	}
	return completed, true
}
