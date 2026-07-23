package quota

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const (
	quotaLowRemainingRatio             = 0.20
	codexMaxResetCreditsAvailableCount = 1_000_000
)

type codexUsagePayload struct {
	PlanType              string                     `json:"plan_type"`
	RateLimit             *codexRateLimit            `json:"rate_limit"`
	CodeReviewRateLimit   *codexRateLimit            `json:"code_review_rate_limit"`
	AdditionalLimits      []codexAdditionalRateLimit `json:"additional_rate_limits"`
	RateLimitResetCredits *codexResetCreditsCount    `json:"rate_limit_reset_credits"`
}

type codexAdditionalRateLimit struct {
	LimitName      string          `json:"limit_name"`
	MeteredFeature string          `json:"metered_feature"`
	RateLimit      *codexRateLimit `json:"rate_limit"`
}

type codexResetCreditsCount struct {
	AvailableCount *int `json:"available_count"`
}

type codexResetCreditsPayload struct {
	Credits        *[]codexResetCreditPayload `json:"credits"`
	AvailableCount *int                       `json:"available_count"`
}

type codexResetCreditPayload struct {
	ID        string  `json:"id"`
	ResetType string  `json:"reset_type"`
	Status    string  `json:"status"`
	GrantedAt string  `json:"granted_at"`
	ExpiresAt *string `json:"expires_at"`
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

type codexUsageResult struct {
	windows                    []cluster.QuotaWindow
	plan                       *cluster.QuotaPlan
	resetCreditsAvailableCount *int
}

func parseCodexUsage(body []byte, observedAt time.Time) (codexUsageResult, error) {
	var payload codexUsagePayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return codexUsageResult{}, fmt.Errorf("decode codex quota response: %w", errDecode)
	}
	windows := make([]cluster.QuotaWindow, 0, 6)
	windows = appendCodexProbeRateLimit(windows, "", nil, "account", nil, payload.RateLimit, 0, observedAt)
	windows = appendCodexProbeRateLimit(
		windows,
		"code-review",
		quotaStringPtr("Code Review"),
		"model",
		quotaStringPtr(codexLimitScopeID("code-review")),
		payload.CodeReviewRateLimit,
		10,
		observedAt,
	)
	additional := append([]codexAdditionalRateLimit(nil), payload.AdditionalLimits...)
	sort.SliceStable(additional, func(i, j int) bool {
		return codexAdditionalLimitIdentity(additional[i]) < codexAdditionalLimitIdentity(additional[j])
	})
	seenAdditionalLimits := make(map[string]struct{}, len(additional))
	for index, limit := range additional {
		identity := codexAdditionalLimitIdentity(limit)
		if identity == "" {
			continue
		}
		scopeID := codexLimitScopeID(identity)
		if _, exists := seenAdditionalLimits[scopeID]; exists {
			return codexUsageResult{}, fmt.Errorf("decode codex quota response: duplicate additional limit %q", identity)
		}
		seenAdditionalLimits[scopeID] = struct{}{}
		label := firstCodexString(limit.LimitName, limit.MeteredFeature)
		windows = appendCodexProbeRateLimit(
			windows,
			identity,
			quotaStringPtr(label),
			"model",
			quotaStringPtr(scopeID),
			limit.RateLimit,
			20+index*10,
			observedAt,
		)
	}
	var resetCreditsAvailableCount *int
	if payload.RateLimitResetCredits != nil && payload.RateLimitResetCredits.AvailableCount != nil {
		count := *payload.RateLimitResetCredits.AvailableCount
		if count < 0 || count > codexMaxResetCreditsAvailableCount {
			return codexUsageResult{}, fmt.Errorf("decode codex quota response: reset-credit available_count is out of range")
		}
		resetCreditsAvailableCount = &count
	}
	return codexUsageResult{
		windows:                    windows,
		plan:                       codexPlan(payload.PlanType),
		resetCreditsAvailableCount: resetCreditsAvailableCount,
	}, nil
}

func parseCodexUsageWindows(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, error) {
	result, errParse := parseCodexUsage(body, observedAt)
	return result.windows, errParse
}

func appendCodexProbeRateLimit(windows []cluster.QuotaWindow, limitIdentity string, label *string, scope string, scopeID *string, limit *codexRateLimit, priority int, observedAt time.Time) []cluster.QuotaWindow {
	if limit == nil {
		return windows
	}
	seenDurations := make(map[int64]struct{}, 2)
	for index, input := range []*codexUsageWindow{limit.PrimaryWindow, limit.SecondaryWindow} {
		if input == nil || input.LimitWindowSeconds <= 0 {
			continue
		}
		id := codexWindowID(limitIdentity, input.LimitWindowSeconds)
		if window, ok := codexProbeWindow(id, label, scope, scopeID, input, priority+index, observedAt); ok {
			if _, exists := seenDurations[input.LimitWindowSeconds]; exists {
				continue
			}
			seenDurations[input.LimitWindowSeconds] = struct{}{}
			windows = append(windows, window)
		}
	}
	return windows
}

func codexProbeWindow(id string, label *string, scope string, scopeID *string, input *codexUsageWindow, priority int, observedAt time.Time) (cluster.QuotaWindow, bool) {
	if input == nil || input.UsedPercent == nil || input.LimitWindowSeconds <= 0 || math.IsNaN(*input.UsedPercent) || math.IsInf(*input.UsedPercent, 0) || *input.UsedPercent < 0 {
		return cluster.QuotaWindow{}, false
	}
	usedRatio := math.Max(0, math.Min(1, *input.UsedPercent/100))
	remainingRatio := 1 - usedRatio
	used, remaining, limit := usedRatio*100, remainingRatio*100, float64(100)
	periodUnit, periodValue := quotaProbePeriod(input.LimitWindowSeconds)
	window := cluster.QuotaWindow{
		ID: id, Label: label, Scope: scope, ScopeID: scopeID, Mode: "rolling", Status: quotaProbeStatus(remainingRatio), Unit: "percentage",
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

func codexWindowIDSuffix(seconds int64) string {
	unit, value := quotaProbePeriod(seconds)
	if value != nil && unit != "unknown" {
		return quotaIDSlug(strconv.FormatFloat(*value, 'f', -1, 64) + "-" + unit)
	}
	return strconv.FormatInt(seconds, 10) + "-second"
}

func codexWindowID(limitIdentity string, seconds int64) string {
	prefix := "codex"
	if key := codexLimitKey(limitIdentity); key != "" {
		prefix += "-" + key
	}
	return prefix + "-" + codexWindowIDSuffix(seconds)
}

func codexLimitKey(value string) string {
	key := quotaIDSlug(value)
	if key == "codex" {
		return ""
	}
	return strings.TrimPrefix(key, "codex-")
}

func codexLimitScopeID(value string) string {
	key := codexLimitKey(value)
	if key == "" {
		return "codex"
	}
	return "codex_" + strings.ReplaceAll(key, "-", "_")
}

func codexAdditionalLimitIdentity(limit codexAdditionalRateLimit) string {
	return strings.TrimSpace(limit.MeteredFeature)
}

func firstCodexString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func codexPlan(planType string) *cluster.QuotaPlan {
	planType = strings.TrimSpace(planType)
	if planType == "" {
		return nil
	}
	normalized := strings.ToLower(planType)
	switch normalized {
	case "pro":
		return &cluster.QuotaPlan{Name: "Pro 20x", Premium: true}
	case "prolite", "pro-lite", "pro_lite":
		return &cluster.QuotaPlan{Name: "Pro 5x", Premium: true}
	case "plus":
		return &cluster.QuotaPlan{Name: "Plus"}
	case "team":
		return &cluster.QuotaPlan{Name: "Team"}
	case "free":
		return &cluster.QuotaPlan{Name: "Free"}
	default:
		return &cluster.QuotaPlan{Name: planType}
	}
}

func parseCodexResetCredits(body []byte, observedAt time.Time, fallbackAvailableCount *int) (*cluster.QuotaResetCredits, error) {
	var payload codexResetCreditsPayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode codex reset credits response: %w", errDecode)
	}
	if payload.AvailableCount == nil && payload.Credits == nil {
		return nil, fmt.Errorf("decode codex reset credits response: expected credits or available_count")
	}
	credits := make([]cluster.QuotaResetCredit, 0)
	seenIDs := make(map[string]struct{})
	if payload.Credits != nil {
		for _, record := range *payload.Credits {
			if strings.TrimSpace(record.ResetType) != "codex_rate_limits" || !strings.EqualFold(strings.TrimSpace(record.Status), "available") {
				continue
			}
			if len(credits) >= 100 {
				return nil, fmt.Errorf("decode codex reset credits response: too many available credits")
			}
			id := strings.TrimSpace(record.ID)
			if id == "" {
				return nil, fmt.Errorf("decode codex reset credits response: available credit id is required")
			}
			if _, exists := seenIDs[id]; exists {
				return nil, fmt.Errorf("decode codex reset credits response: duplicate credit id %q", id)
			}
			seenIDs[id] = struct{}{}
			grantedAt, errGrantedAt := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.GrantedAt))
			if errGrantedAt != nil {
				return nil, fmt.Errorf("decode codex reset credits response: credit %q has invalid granted_at", id)
			}
			var expiresAt *time.Time
			if record.ExpiresAt != nil && strings.TrimSpace(*record.ExpiresAt) != "" {
				parsedExpiresAt, errExpiresAt := time.Parse(time.RFC3339Nano, strings.TrimSpace(*record.ExpiresAt))
				if errExpiresAt != nil {
					return nil, fmt.Errorf("decode codex reset credits response: credit %q has invalid expires_at", id)
				}
				parsedExpiresAt = parsedExpiresAt.UTC()
				if !parsedExpiresAt.After(observedAt.UTC()) {
					continue
				}
				expiresAt = &parsedExpiresAt
			}
			credits = append(credits, cluster.QuotaResetCredit{
				ID: id, Status: "available", GrantedAt: grantedAt.UTC(), ExpiresAt: expiresAt,
			})
		}
	}
	sort.SliceStable(credits, func(i, j int) bool {
		leftExpiry := credits[i].ExpiresAt
		rightExpiry := credits[j].ExpiresAt
		if leftExpiry == nil || rightExpiry == nil {
			if leftExpiry != nil {
				return true
			}
			if rightExpiry != nil {
				return false
			}
		} else if !leftExpiry.Equal(*rightExpiry) {
			return leftExpiry.Before(*rightExpiry)
		}
		return credits[i].ID < credits[j].ID
	})
	var availableCount *int
	if payload.AvailableCount != nil {
		if *payload.AvailableCount < 0 || *payload.AvailableCount > codexMaxResetCreditsAvailableCount {
			return nil, fmt.Errorf("decode codex reset credits response: available_count is out of range")
		}
		count := *payload.AvailableCount
		availableCount = &count
	}
	if availableCount == nil && fallbackAvailableCount != nil {
		fallback := *fallbackAvailableCount
		availableCount = &fallback
	}
	if availableCount == nil || *availableCount < len(credits) {
		count := len(credits)
		availableCount = &count
	}
	return &cluster.QuotaResetCredits{
		AvailableCount: availableCount,
		ObservedAt:     observedAt.UTC(),
		Credits:        credits,
	}, nil
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
