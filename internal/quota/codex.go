package quota

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const quotaLowRemainingRatio = 0.20

type codexUsagePayload struct {
	RateLimit           *codexRateLimit            `json:"rate_limit"`
	CodeReviewRateLimit *codexRateLimit            `json:"code_review_rate_limit"`
	AdditionalLimits    []codexAdditionalRateLimit `json:"additional_rate_limits"`
}

type codexAdditionalRateLimit struct {
	LimitName string          `json:"limit_name"`
	RateLimit *codexRateLimit `json:"rate_limit"`
}

type codexRateLimit struct {
	PrimaryWindow   *codexUsageWindow `json:"primary_window"`
	SecondaryWindow *codexUsageWindow `json:"secondary_window"`
}

type codexUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

func parseCodexUsageWindows(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, error) {
	var payload codexUsagePayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode codex quota response: %w", errDecode)
	}
	windows := make([]cluster.QuotaWindow, 0, 6)
	windows = appendCodexProbeRateLimit(windows, "codex", nil, payload.RateLimit, 0, observedAt)
	windows = appendCodexProbeRateLimit(windows, "codex-code-review", quotaStringPtr("Code Review"), payload.CodeReviewRateLimit, 2, observedAt)
	additional := append([]codexAdditionalRateLimit(nil), payload.AdditionalLimits...)
	sort.SliceStable(additional, func(i, j int) bool { return additional[i].LimitName < additional[j].LimitName })
	for index, limit := range additional {
		label := strings.TrimSpace(limit.LimitName)
		if label == "" {
			continue
		}
		windows = appendCodexProbeRateLimit(windows, "codex-"+quotaIDSlug(label), &label, limit.RateLimit, 10+index*2, observedAt)
	}
	return windows, nil
}

func appendCodexProbeRateLimit(windows []cluster.QuotaWindow, prefix string, label *string, limit *codexRateLimit, priority int, observedAt time.Time) []cluster.QuotaWindow {
	if limit == nil {
		return windows
	}
	if limit.PrimaryWindow != nil {
		if window, ok := codexProbeWindow(prefix+"-primary", label, limit.PrimaryWindow, priority, observedAt); ok {
			windows = append(windows, window)
		}
	}
	if limit.SecondaryWindow != nil {
		if window, ok := codexProbeWindow(prefix+"-secondary", label, limit.SecondaryWindow, priority+1, observedAt); ok {
			windows = append(windows, window)
		}
	}
	return windows
}

func codexProbeWindow(id string, label *string, input *codexUsageWindow, priority int, observedAt time.Time) (cluster.QuotaWindow, bool) {
	if input == nil || input.UsedPercent == nil || input.LimitWindowSeconds <= 0 || math.IsNaN(*input.UsedPercent) || math.IsInf(*input.UsedPercent, 0) || *input.UsedPercent < 0 {
		return cluster.QuotaWindow{}, false
	}
	usedRatio := math.Max(0, math.Min(1, *input.UsedPercent/100))
	remainingRatio := 1 - usedRatio
	used, remaining, limit := usedRatio*100, remainingRatio*100, float64(100)
	periodUnit, periodValue := quotaProbePeriod(input.LimitWindowSeconds)
	window := cluster.QuotaWindow{
		ID: id, Label: label, Scope: "account", Mode: "rolling", Status: quotaProbeStatus(remainingRatio), Unit: "percentage",
		Used: &used, Remaining: &remaining, Limit: &limit, UsedRatio: &usedRatio, RemainingRatio: &remainingRatio,
		WindowSeconds: &input.LimitWindowSeconds, PeriodUnit: periodUnit, PeriodValue: periodValue,
		Source: "active_probe", ObservedAt: observedAt.UTC(), Priority: priority,
	}
	if input.ResetAt != nil && *input.ResetAt > 0 {
		resetAt := time.Unix(*input.ResetAt, 0).UTC()
		window.ResetAt = &resetAt
	} else if input.ResetAfterSeconds != nil && *input.ResetAfterSeconds >= 0 {
		resetAt := observedAt.UTC().Add(time.Duration(*input.ResetAfterSeconds) * time.Second)
		window.ResetAt = &resetAt
	}
	return window, true
}

func quotaProbePeriod(seconds int64) (string, *float64) {
	switch seconds {
	case 30 * 24 * 60 * 60, 2628000:
		return "month", quotaFloatPtr(1)
	}
	if seconds%(7*24*60*60) == 0 {
		return "week", quotaFloatPtr(float64(seconds / (7 * 24 * 60 * 60)))
	}
	if seconds%(24*60*60) == 0 {
		return "day", quotaFloatPtr(float64(seconds / (24 * 60 * 60)))
	}
	if seconds%(60*60) == 0 {
		return "hour", quotaFloatPtr(float64(seconds / (60 * 60)))
	}
	if seconds%60 == 0 {
		return "minute", quotaFloatPtr(float64(seconds / 60))
	}
	return "unknown", nil
}

func quotaProbeStatus(remaining float64) string {
	if remaining <= 0 {
		return "exhausted"
	}
	if remaining <= quotaLowRemainingRatio {
		return "low"
	}
	return "healthy"
}

func quotaWindowAggregateStatus(windows []cluster.QuotaWindow) string {
	status := "unknown"
	for _, window := range windows {
		switch window.Status {
		case "exhausted":
			return "exhausted"
		case "low":
			status = "low"
		case "error":
			if status != "low" {
				status = "error"
			}
		case "healthy":
			if status == "unknown" {
				status = "healthy"
			}
		}
	}
	return status
}

func quotaIDSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
			lastDash = false
		} else if builder.Len() > 0 && !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func quotaFloatPtr(value float64) *float64 { return &value }
func quotaStringPtr(value string) *string  { return &value }
