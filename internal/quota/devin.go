package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func (c *Collector) probeDevin(ctx context.Context, auth *coreauth.Auth) (probeResult, *probeError) {
	token := quotaMetadataString(auth.Metadata, "api_key", "session_token", "access_token", "accessToken")
	if token == "" {
		for _, key := range []string{"api_key", "session_token", "token"} {
			if token = strings.TrimSpace(auth.Attributes[key]); token != "" {
				break
			}
		}
	}
	if token == "" {
		return probeResult{}, &probeError{code: "AUTH_TOKEN_UNAVAILABLE", message: "Credential session token is unavailable.", retryable: true}
	}
	body, errMarshal := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideName": "chisel", "ideVersion": "3000.10.21", "apiKey": token,
			"locale": "en", "os": "darwin", "extensionVersion": "3000.10.21", "clientName": "chisel",
		},
	})
	if errMarshal != nil {
		return probeResult{}, &probeError{code: "PROBE_REQUEST_INVALID", message: "Quota probe request could not be created.", retryable: false}
	}
	headers := http.Header{"Content-Type": []string{"application/json"}, "Connect-Protocol-Version": []string{"1"}}
	payload, _, errRequest := c.probeHTTPRequest(ctx, auth, http.MethodPost, c.options.DevinQuotaURL, body, headers)
	if errRequest != nil {
		return probeResult{}, errRequest
	}
	now := c.options.Now().UTC()
	windows, plan, errParse := parseDevinQuota(payload, now)
	amounts := windows[:0]
	for _, window := range windows {
		if window.RemainingRatio != nil {
			amounts = append(amounts, window)
		}
	}
	windows = amounts
	if errParse != nil || len(windows) == 0 {
		return probeResult{}, &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Devin quota response did not contain usable daily or weekly quota.", retryable: true}
	}
	result := probeResult{windows: windows, plan: plan, replaceWindows: len(windows) == 2, replacePlan: plan != nil, boundWindowLifetimes: true}
	if len(windows) < 2 {
		result.partial = true
		result.collectionError = probeCollectionError(&probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Devin quota response did not contain both daily and weekly remaining amounts.", retryable: true}, now)
	}
	return result, nil
}

func parseDevinQuota(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, *cluster.QuotaPlan, error) {
	var payload struct {
		UserStatus struct {
			PlanStatus struct {
				PlanInfo      json.RawMessage `json:"planInfo"`
				PlanEnd       json.RawMessage `json:"planEnd"`
				Daily         flexFloat       `json:"dailyQuotaRemainingPercent"`
				Weekly        flexFloat       `json:"weeklyQuotaRemainingPercent"`
				DailyResetAt  flexFloat       `json:"dailyQuotaResetAtUnix"`
				WeeklyResetAt flexFloat       `json:"weeklyQuotaResetAtUnix"`
			} `json:"planStatus"`
		} `json:"userStatus"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, nil, fmt.Errorf("decode devin quota response: %w", errDecode)
	}
	status := payload.UserStatus.PlanStatus
	windows := make([]cluster.QuotaWindow, 0, 2)
	for index, bucket := range []struct {
		id, label, period string
		remaining, reset  flexFloat
	}{
		{"devin-daily", "Daily Limit", "day", status.Daily, status.DailyResetAt},
		{"devin-weekly", "Weekly Limit", "week", status.Weekly, status.WeeklyResetAt},
	} {
		remaining := flexFloatValue(bucket.remaining)
		if remaining != nil && (*remaining < 0 || *remaining > 100) {
			remaining = nil
		}
		var resetAt *time.Time
		if seconds := flexFloatValue(bucket.reset); seconds != nil && *seconds > 0 && *seconds <= 253402300799 && math.Trunc(*seconds) == *seconds {
			reset := time.Unix(int64(*seconds), 0).UTC()
			resetAt = &reset
		}
		if resetAt != nil && !resetAt.After(observedAt) {
			continue
		}
		if remaining == nil && resetAt == nil {
			continue
		}
		window := cluster.QuotaWindow{
			ID: bucket.id, Label: quotaStringPtr(bucket.label), Scope: "account", Mode: "fixed", Status: "unknown",
			Unit: "percentage", Limit: quotaFloatPtr(100), Remaining: remaining, ResetAt: resetAt,
			PeriodUnit: bucket.period, PeriodValue: quotaFloatPtr(1), Source: "active_probe", ObservedAt: observedAt, Priority: index,
		}
		normalizeWindowValues(&window)
		windows = append(windows, window)
	}
	// Optional subscription metadata must not invalidate otherwise usable quota.
	var planInfo struct {
		PlanName string `json:"planName"`
	}
	_ = json.Unmarshal(status.PlanInfo, &planInfo)
	var planEnd string
	_ = json.Unmarshal(status.PlanEnd, &planEnd)
	var plan *cluster.QuotaPlan
	if name := strings.TrimSpace(planInfo.PlanName); name != "" {
		switch strings.ToLower(name) {
		case "free":
			name = "Free"
		case "pro":
			name = "Pro"
		case "teams":
			name = "Teams"
		case "enterprise":
			name = "Enterprise"
		}
		expiresAt := parseProviderTime(planEnd)
		if expiresAt != nil && !expiresAt.After(time.Unix(0, 0)) {
			expiresAt = nil
		}
		plan = &cluster.QuotaPlan{Name: name, ExpiresAt: expiresAt}
	}
	return windows, plan, nil
}
