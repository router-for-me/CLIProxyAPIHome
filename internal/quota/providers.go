package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func (c *Collector) probeClaude(ctx context.Context, auth *coreauth.Auth) (probeResult, *probeError) {
	headers := http.Header{"Content-Type": []string{"application/json"}, "Anthropic-Beta": []string{"oauth-2025-04-20"}}
	body, _, errUsage := c.probeRequest(ctx, auth, http.MethodGet, c.options.ClaudeUsageURL, nil, headers)
	if errUsage != nil {
		return probeResult{}, errUsage
	}
	windows, errParse := parseClaudeUsageWindows(body, c.options.Now().UTC())
	if errParse != nil || len(windows) == 0 {
		return probeResult{}, &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Claude quota response did not contain usable windows.", retryable: true}
	}
	result := probeResult{windows: windows, replaceWindows: true}
	profileBody, _, errProfile := c.probeRequest(ctx, auth, http.MethodGet, c.options.ClaudeProfileURL, nil, headers)
	if errProfile != nil {
		result.partial = true
		result.collectionError = probeCollectionError(errProfile, c.options.Now().UTC())
		return result, nil
	}
	if !validClaudeProfile(profileBody) {
		profileError := &probeError{code: "PROFILE_RESPONSE_INVALID", message: "Claude profile response could not be parsed.", retryable: true}
		result.partial = true
		result.collectionError = probeCollectionError(profileError, c.options.Now().UTC())
	}
	return result, nil
}

func (c *Collector) probeAntigravity(ctx context.Context, auth *coreauth.Auth) ([]cluster.QuotaWindow, *probeError) {
	projectID := quotaMetadataString(auth.Metadata, "project_id", "projectId", "project")
	if projectID == "" {
		return nil, &probeError{code: "PROJECT_ID_UNAVAILABLE", message: "Credential project ID is unavailable.", retryable: false}
	}
	body, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		return nil, &probeError{code: "PROBE_REQUEST_INVALID", message: "Antigravity quota request could not be created.", retryable: false}
	}
	headers := http.Header{"Content-Type": []string{"application/json"}, "User-Agent": []string{antigravityUserAgent}}
	probeCtx, cancelProbe := context.WithTimeout(ctx, c.options.ProbeTimeout)
	defer cancelProbe()
	var lastError *probeError
	for _, targetURL := range c.options.AntigravityURLs {
		payload, _, errRequest := c.probeRequest(probeCtx, auth, http.MethodPost, targetURL, body, headers)
		if errRequest != nil {
			lastError = errRequest
			if errRequest.code == "UPSTREAM_AUTH_REJECTED" || probeCtx.Err() != nil {
				break
			}
			continue
		}
		windows, errParse := parseAntigravityWindows(payload, c.options.Now().UTC())
		if errParse == nil && antigravityQuotaGroupsComplete(windows) {
			return windows, nil
		}
		lastError = &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Antigravity quota response did not contain both usable weekly quota buckets.", retryable: true}
	}
	if lastError == nil {
		lastError = &probeError{code: "PROVIDER_UNAVAILABLE", message: "Antigravity quota collector has no configured endpoint.", retryable: false}
	}
	return nil, lastError
}

func (c *Collector) probeKimi(ctx context.Context, auth *coreauth.Auth) ([]cluster.QuotaWindow, *probeError) {
	payload, _, errRequest := c.probeRequest(ctx, auth, http.MethodGet, c.options.KimiUsageURL, nil, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	windows, errParse := parseKimiUsageWindows(payload, c.options.Now().UTC())
	if errParse != nil || len(windows) == 0 {
		return nil, &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "Kimi quota response did not contain usable windows.", retryable: true}
	}
	return windows, nil
}

func (c *Collector) probeXAI(ctx context.Context, auth *coreauth.Auth) ([]cluster.QuotaWindow, *cluster.QuotaPlan, *probeError) {
	headers := http.Header{
		"Accept":               []string{"*/*"},
		"User-Agent":           []string{xaiGrokUserAgent},
		xaiTokenAuthHeader:     []string{xaiTokenAuthValue},
		xaiClientVersionHeader: []string{xaiClientVersionValue},
	}
	if userID := quotaMetadataString(auth.Metadata, "sub", "subject", "user_id", "userId"); userID != "" {
		headers.Set("x-userid", userID)
	}
	payload, _, errRequest := c.probeRequest(ctx, auth, http.MethodGet, c.options.XAIBillingURL, nil, headers)
	if errRequest != nil {
		return nil, nil, errRequest
	}
	windows, plan, errParse := parseXAIUsageWindows(payload, c.options.Now().UTC())
	if errParse != nil || len(windows) == 0 {
		return nil, nil, &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: "xAI billing response did not contain usable quota.", retryable: true}
	}
	return windows, plan, nil
}

func probeCollectionError(failure *probeError, occurredAt time.Time) *cluster.QuotaCollectionError {
	if failure == nil {
		return nil
	}
	result := &cluster.QuotaCollectionError{Code: failure.code, Message: failure.message, Retryable: failure.retryable, OccurredAt: &occurredAt}
	if failure.statusCode > 0 {
		result.UpstreamStatusCode = &failure.statusCode
	}
	if failure.requestID != "" {
		result.RequestID = &failure.requestID
	}
	return result
}

type claudeUsagePayload struct {
	FiveHour          *claudeUsageWindow `json:"five_hour"`
	SevenDay          *claudeUsageWindow `json:"seven_day"`
	SevenDayOAuthApps *claudeUsageWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus      *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet    *claudeUsageWindow `json:"seven_day_sonnet"`
	SevenDayCowork    *claudeUsageWindow `json:"seven_day_cowork"`
	IguanaNecktie     *claudeUsageWindow `json:"iguana_necktie"`
	ExtraUsage        *claudeExtraUsage  `json:"extra_usage"`
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeExtraUsage struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

func parseClaudeUsageWindows(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, error) {
	var payload claudeUsagePayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode claude quota response: %w", errDecode)
	}
	windows := make([]cluster.QuotaWindow, 0, 8)
	windows = appendClaudeWindow(windows, "claude-five-hour", nil, "account", nil, payload.FiveHour, "hour", 5, 0, observedAt)
	windows = appendClaudeWindow(windows, "claude-seven-day", nil, "account", nil, payload.SevenDay, "week", 1, 1, observedAt)
	windows = appendClaudeWindow(windows, "claude-seven-day-oauth-apps", quotaStringPtr("OAuth Apps"), "account", nil, payload.SevenDayOAuthApps, "week", 1, 2, observedAt)
	windows = appendClaudeWindow(windows, "claude-seven-day-opus", quotaStringPtr("Opus"), "model", quotaStringPtr("opus"), payload.SevenDayOpus, "week", 1, 3, observedAt)
	windows = appendClaudeWindow(windows, "claude-seven-day-sonnet", quotaStringPtr("Sonnet"), "model", quotaStringPtr("sonnet"), payload.SevenDaySonnet, "week", 1, 4, observedAt)
	windows = appendClaudeWindow(windows, "claude-seven-day-cowork", quotaStringPtr("Cowork"), "account", nil, payload.SevenDayCowork, "week", 1, 5, observedAt)
	windows = appendClaudeWindow(windows, "claude-iguana-necktie", quotaStringPtr("Iguana Necktie"), "account", nil, payload.IguanaNecktie, "unknown", 0, 6, observedAt)
	if extra, ok := claudeExtraUsageWindow(payload.ExtraUsage, observedAt); ok {
		windows = append(windows, extra)
	}
	return windows, nil
}

func appendClaudeWindow(windows []cluster.QuotaWindow, id string, label *string, scope string, scopeID *string, input *claudeUsageWindow, periodUnit string, periodValue float64, priority int, observedAt time.Time) []cluster.QuotaWindow {
	if input == nil || input.Utilization == nil || math.IsNaN(*input.Utilization) || math.IsInf(*input.Utilization, 0) || *input.Utilization < 0 {
		return windows
	}
	usedRatio := normalizedProviderRatio(*input.Utilization)
	remainingRatio := 1 - usedRatio
	used, remaining, limit := usedRatio*100, remainingRatio*100, float64(100)
	window := cluster.QuotaWindow{ID: id, Label: label, Scope: scope, ScopeID: scopeID, Mode: "rolling", Status: quotaProbeStatus(remainingRatio), Unit: "percentage", Used: &used, Remaining: &remaining, Limit: &limit, UsedRatio: &usedRatio, RemainingRatio: &remainingRatio, PeriodUnit: periodUnit, Source: "active_probe", ObservedAt: observedAt, Priority: priority}
	if periodUnit != "unknown" {
		window.PeriodValue = quotaFloatPtr(periodValue)
		window.WindowSeconds = periodSeconds(periodUnit, periodValue)
	}
	window.ResetAt = parseProviderTime(input.ResetsAt)
	return append(windows, window)
}

func claudeExtraUsageWindow(input *claudeExtraUsage, observedAt time.Time) (cluster.QuotaWindow, bool) {
	if input == nil || (!input.IsEnabled && input.MonthlyLimit == nil && input.UsedCredits == nil && input.Utilization == nil) {
		return cluster.QuotaWindow{}, false
	}
	window := cluster.QuotaWindow{ID: "claude-extra-usage", Label: quotaStringPtr("Extra Usage"), Scope: "account", Mode: "fixed", Status: "unknown", Unit: "credits", PeriodUnit: "month", PeriodValue: quotaFloatPtr(1), Source: "active_probe", ObservedAt: observedAt, Priority: 20}
	window.Used = nonNegativeFloat(input.UsedCredits)
	window.Limit = nonNegativeFloat(input.MonthlyLimit)
	if window.Limit != nil && *window.Limit > 0 {
		remaining := math.Max(0, *window.Limit-quotaFloatValue(window.Used))
		window.Remaining = &remaining
		usedRatio := math.Max(0, math.Min(1, quotaFloatValue(window.Used)/(*window.Limit)))
		remainingRatio := 1 - usedRatio
		window.UsedRatio, window.RemainingRatio = &usedRatio, &remainingRatio
		window.Status = quotaProbeStatus(remainingRatio)
	} else if input.Utilization != nil {
		usedRatio := normalizedProviderRatio(*input.Utilization)
		remainingRatio := 1 - usedRatio
		window.UsedRatio, window.RemainingRatio = &usedRatio, &remainingRatio
		window.Status = quotaProbeStatus(remainingRatio)
	}
	return window, true
}

func validClaudeProfile(body []byte) bool {
	var payload struct {
		Account      map[string]any `json:"account"`
		Organization map[string]any `json:"organization"`
	}
	return json.Unmarshal(body, &payload) == nil && (payload.Account != nil || payload.Organization != nil)
}

type antigravityPayload struct {
	Groups []json.RawMessage `json:"groups"`
}

type antigravityQuotaGroup struct {
	DisplayName string            `json:"displayName"`
	Buckets     []json.RawMessage `json:"buckets"`
}

type antigravityQuotaBucket struct {
	BucketID          string            `json:"bucketId"`
	Disabled          bool              `json:"disabled"`
	RemainingFraction flexQuotaFraction `json:"remainingFraction"`
	ResetTime         string            `json:"resetTime"`
}

type antigravityBucketSpec struct {
	scopeID       string
	periodUnit    string
	periodValue   float64
	windowSeconds int64
	priority      int
}

func antigravityQuotaBucketSpec(bucketID string) (antigravityBucketSpec, bool) {
	switch strings.TrimSpace(bucketID) {
	case "gemini-5h":
		return antigravityBucketSpec{scopeID: "gemini", periodUnit: "hour", periodValue: 5, windowSeconds: 5 * 60 * 60, priority: 0}, true
	case "3p-5h":
		return antigravityBucketSpec{scopeID: "third-party", periodUnit: "hour", periodValue: 5, windowSeconds: 5 * 60 * 60, priority: 1}, true
	case "gemini-weekly":
		return antigravityBucketSpec{scopeID: "gemini", periodUnit: "week", periodValue: 1, windowSeconds: 7 * 24 * 60 * 60, priority: 2}, true
	case "3p-weekly":
		return antigravityBucketSpec{scopeID: "third-party", periodUnit: "week", periodValue: 1, windowSeconds: 7 * 24 * 60 * 60, priority: 3}, true
	default:
		return antigravityBucketSpec{}, false
	}
}

func antigravityQuotaGroupsComplete(windows []cluster.QuotaWindow) bool {
	var geminiWeekly, thirdPartyWeekly bool
	for _, window := range windows {
		switch strings.TrimSpace(window.ID) {
		case "antigravity-gemini-weekly":
			geminiWeekly = true
		case "antigravity-3p-weekly":
			thirdPartyWeekly = true
		}
	}
	return geminiWeekly && thirdPartyWeekly
}

func parseAntigravityWindows(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, error) {
	var payload antigravityPayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode antigravity quota response: %w", errDecode)
	}
	windows := make([]cluster.QuotaWindow, 0, 4)
	seenBuckets := make(map[string]struct{}, 4)
	for _, rawGroup := range payload.Groups {
		var group antigravityQuotaGroup
		if errGroup := json.Unmarshal(rawGroup, &group); errGroup != nil {
			continue
		}
		groupLabel := strings.TrimSpace(group.DisplayName)
		var label *string
		if groupLabel != "" {
			label = quotaStringPtr(groupLabel)
		}
		for _, rawBucket := range group.Buckets {
			var bucket antigravityQuotaBucket
			if errBucket := json.Unmarshal(rawBucket, &bucket); errBucket != nil {
				continue
			}
			bucketID := strings.TrimSpace(bucket.BucketID)
			spec, supported := antigravityQuotaBucketSpec(bucketID)
			if !supported || bucket.Disabled || !bucket.RemainingFraction.set {
				continue
			}
			if _, exists := seenBuckets[bucketID]; exists {
				continue
			}
			seenBuckets[bucketID] = struct{}{}
			remainingRatio := bucket.RemainingFraction.value
			usedRatio := 1 - remainingRatio
			used, remaining, limit := usedRatio*100, remainingRatio*100, float64(100)
			window := cluster.QuotaWindow{
				ID: "antigravity-" + bucketID, Label: label, Scope: "model", ScopeID: quotaStringPtr(spec.scopeID),
				Mode: "rolling", Status: quotaProbeStatus(remainingRatio), Unit: "percentage",
				Used: &used, Remaining: &remaining, Limit: &limit, UsedRatio: &usedRatio, RemainingRatio: &remainingRatio,
				ResetAt: parseProviderTime(bucket.ResetTime), WindowSeconds: quotaInt64Ptr(spec.windowSeconds),
				PeriodUnit: spec.periodUnit, PeriodValue: quotaFloatPtr(spec.periodValue), Source: "active_probe",
				ObservedAt: observedAt, Priority: spec.priority,
			}
			normalizeWindowValues(&window)
			windows = append(windows, window)
		}
	}
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].Priority != windows[j].Priority {
			return windows[i].Priority < windows[j].Priority
		}
		return windows[i].ID < windows[j].ID
	})
	return windows, nil
}

// flexQuotaFraction accepts finite JSON numbers, numeric strings, and percentage strings.
// Invalid values stay unset so one malformed bucket cannot reject the full response.
type flexQuotaFraction struct {
	value float64
	set   bool
}

func (f *flexQuotaFraction) UnmarshalJSON(data []byte) error {
	*f = flexQuotaFraction{}
	raw, ok := flexibleNumberText(data)
	if !ok {
		return nil
	}
	percentage := strings.HasSuffix(raw, "%")
	if percentage {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	if percentage {
		value /= 100
	}
	if value < 0 || value > 1 {
		return nil
	}
	f.value = value
	f.set = true
	return nil
}

// flexFloat tolerates providers that encode finite numeric fields as JSON strings.
// Unparseable values leave the field unset instead of failing the whole payload.
type flexFloat struct {
	value float64
	set   bool
}

func (f *flexFloat) UnmarshalJSON(data []byte) error {
	*f = flexFloat{}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var wrapped struct {
			Val json.RawMessage `json:"val"`
		}
		if errDecode := json.Unmarshal(trimmed, &wrapped); errDecode != nil {
			return nil
		}
		if len(wrapped.Val) == 0 {
			return nil
		}
		data = wrapped.Val
	}
	raw, ok := flexibleNumberText(data)
	if !ok {
		return nil
	}
	value, errParse := strconv.ParseFloat(raw, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	f.value = value
	f.set = true
	return nil
}

func flexFloatValue(value flexFloat) *float64 {
	if !value.set || math.IsNaN(value.value) || math.IsInf(value.value, 0) {
		return nil
	}
	converted := value.value
	return &converted
}

// flexInt64 tolerates integer fields encoded as either JSON numbers or strings.
type flexInt64 struct {
	value int64
	set   bool
}

func (f *flexInt64) UnmarshalJSON(data []byte) error {
	*f = flexInt64{}
	raw, ok := flexibleNumberText(data)
	if !ok {
		return nil
	}
	value, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil {
		return nil
	}
	f.value = value
	f.set = true
	return nil
}

func flexibleNumberText(data []byte) (string, bool) {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		return "", false
	}
	if strings.HasPrefix(raw, "\"") {
		unquoted, errUnquote := strconv.Unquote(raw)
		if errUnquote != nil {
			return "", false
		}
		raw = strings.TrimSpace(unquoted)
	}
	return raw, raw != ""
}

type kimiUsagePayload struct {
	Usage  *kimiUsageDetail `json:"usage"`
	Limits []kimiLimitItem  `json:"limits"`
}

type kimiUsageDetail struct {
	Used           flexFloat `json:"used"`
	Limit          flexFloat `json:"limit"`
	Remaining      flexFloat `json:"remaining"`
	Name           string    `json:"name"`
	Title          string    `json:"title"`
	Duration       flexInt64 `json:"duration"`
	TimeUnit       string    `json:"timeUnit"`
	ResetAtSnake   string    `json:"reset_at"`
	ResetAt        string    `json:"resetAt"`
	ResetTimeSnake string    `json:"reset_time"`
	ResetTime      string    `json:"resetTime"`
	ResetInSnake   flexFloat `json:"reset_in"`
	ResetIn        flexFloat `json:"resetIn"`
	TTL            flexFloat `json:"ttl"`
}

type kimiLimitItem struct {
	Name   string           `json:"name"`
	Title  string           `json:"title"`
	Scope  string           `json:"scope"`
	Detail *kimiUsageDetail `json:"detail"`
	Window *struct {
		Duration flexInt64 `json:"duration"`
		TimeUnit string    `json:"timeUnit"`
	} `json:"window"`
	Used           flexFloat `json:"used"`
	Limit          flexFloat `json:"limit"`
	Remaining      flexFloat `json:"remaining"`
	Duration       flexInt64 `json:"duration"`
	TimeUnit       string    `json:"timeUnit"`
	ResetAtSnake   string    `json:"reset_at"`
	ResetAt        string    `json:"resetAt"`
	ResetTimeSnake string    `json:"reset_time"`
	ResetTime      string    `json:"resetTime"`
	ResetInSnake   flexFloat `json:"reset_in"`
	ResetIn        flexFloat `json:"resetIn"`
	TTL            flexFloat `json:"ttl"`
}

func parseKimiUsageWindows(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, error) {
	var payload kimiUsagePayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode kimi quota response: %w", errDecode)
	}
	windows := make([]cluster.QuotaWindow, 0, len(payload.Limits)+1)
	if window, ok := kimiSummaryWindow(payload.Usage, observedAt); ok {
		// Keep the aggregate summary in detail responses without allowing it to
		// displace provider-specific duration limits from primary_windows.
		window.Priority = len(payload.Limits) + 1
		windows = append(windows, window)
	}
	for index, limit := range payload.Limits {
		window, ok := kimiLimitWindow(limit, index, observedAt)
		if ok {
			window.Priority = index
			windows = append(windows, window)
		}
	}
	return windows, nil
}

func kimiLimitWindow(input kimiLimitItem, index int, observedAt time.Time) (cluster.QuotaWindow, bool) {
	used := firstFloat(flexFloatValue(input.Used), detailFloat(input.Detail, "used"))
	limit := firstFloat(flexFloatValue(input.Limit), detailFloat(input.Detail, "limit"))
	remaining := firstFloat(flexFloatValue(input.Remaining), detailFloat(input.Detail, "remaining"))
	if used == nil && limit == nil && remaining == nil {
		return cluster.QuotaWindow{}, false
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = fmt.Sprintf("%d", index+1)
	}
	label := firstNonEmptyString(input.Title, input.Name, "Limit")
	window := cluster.QuotaWindow{ID: "kimi-limit-" + quotaIDSlug(name), Label: &label, Scope: normalizeProviderScope(input.Scope), Mode: "rolling", Status: "unknown", Unit: "requests", Used: nonNegativeFloat(used), Remaining: nonNegativeFloat(remaining), Limit: nonNegativeFloat(limit), PeriodUnit: "unknown", Source: "active_probe", ObservedAt: observedAt}
	duration, unit := kimiLimitPeriod(input)
	window.PeriodUnit, window.PeriodValue, window.WindowSeconds = structuredPeriod(duration, unit)
	window.ResetAt = firstProviderTime(input.ResetAtSnake, input.ResetAt, input.ResetTimeSnake, input.ResetTime)
	if window.ResetAt == nil {
		window.ResetAt = kimiUsageResetAt(input.Detail)
	}
	resetIn := firstNonNegativeFloat(flexFloatValue(input.ResetInSnake), flexFloatValue(input.ResetIn), flexFloatValue(input.TTL), kimiUsageResetSeconds(input.Detail))
	if window.ResetAt == nil && resetIn != nil {
		resetAt := observedAt.Add(time.Duration(*resetIn) * time.Second)
		window.ResetAt = &resetAt
	}
	normalizeWindowValues(&window)
	return window, true
}

func kimiSummaryWindow(input *kimiUsageDetail, observedAt time.Time) (cluster.QuotaWindow, bool) {
	if input == nil {
		return cluster.QuotaWindow{}, false
	}
	used, limit, remaining := flexFloatValue(input.Used), flexFloatValue(input.Limit), flexFloatValue(input.Remaining)
	if used == nil && limit == nil && remaining == nil {
		return cluster.QuotaWindow{}, false
	}
	label := firstNonEmptyString(input.Title, "Usage")
	window := cluster.QuotaWindow{ID: "kimi-usage", Label: &label, Scope: "account", Mode: "balance", Status: "unknown", Unit: "requests", Used: nonNegativeFloat(used), Remaining: nonNegativeFloat(remaining), Limit: nonNegativeFloat(limit), PeriodUnit: "unknown", Source: "active_probe", ObservedAt: observedAt}
	window.ResetAt = kimiUsageResetAt(input)
	resetIn := kimiUsageResetSeconds(input)
	if window.ResetAt == nil && resetIn != nil {
		resetAt := observedAt.Add(time.Duration(*resetIn) * time.Second)
		window.ResetAt = &resetAt
	}
	normalizeWindowValues(&window)
	return window, true
}

type xaiBillingPayload struct {
	Config *xaiBillingConfig `json:"config"`
}

type xaiBillingConfig struct {
	MonthlyLimit      flexFloat `json:"monthlyLimit"`
	MonthlyLimitSnake flexFloat `json:"monthly_limit"`
	Used              flexFloat `json:"used"`
	OnDemandCap       flexFloat `json:"onDemandCap"`
	OnDemandCapSnake  flexFloat `json:"on_demand_cap"`
	OnDemandUsed      flexFloat `json:"onDemandUsed"`
	OnDemandUsedSnake flexFloat `json:"on_demand_used"`
	BillingPeriodEnd  string    `json:"billingPeriodEnd"`
	BillingEndSnake   string    `json:"billing_period_end"`
}

const (
	xaiSuperGrokMonthlyLimitCents      = 15_000
	xaiSuperGrokHeavyMonthlyLimitCents = 150_000
)

func xaiPlanFromMonthlyLimit(monthlyLimitCents *float64) *cluster.QuotaPlan {
	if monthlyLimitCents == nil {
		return nil
	}
	switch *monthlyLimitCents {
	case xaiSuperGrokMonthlyLimitCents:
		return &cluster.QuotaPlan{Name: "SuperGrok", Premium: false}
	case xaiSuperGrokHeavyMonthlyLimitCents:
		return &cluster.QuotaPlan{Name: "SuperGrok Heavy", Premium: true}
	default:
		return nil
	}
}

func parseXAIUsageWindows(body []byte, observedAt time.Time) ([]cluster.QuotaWindow, *cluster.QuotaPlan, error) {
	var payload xaiBillingPayload
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		return nil, nil, fmt.Errorf("decode xai billing response: %w", errDecode)
	}
	if payload.Config == nil {
		return nil, nil, nil
	}
	config := payload.Config
	monthlyLimitCents := firstFloat(flexFloatValue(config.MonthlyLimit), flexFloatValue(config.MonthlyLimitSnake))
	usedCents := flexFloatValue(config.Used)
	onDemandCapCents := firstFloat(flexFloatValue(config.OnDemandCap), flexFloatValue(config.OnDemandCapSnake))
	onDemandUsedCents := firstFloat(flexFloatValue(config.OnDemandUsed), flexFloatValue(config.OnDemandUsedSnake))
	billingPeriodEnd := firstNonEmptyString(config.BillingPeriodEnd, config.BillingEndSnake)

	windows := make([]cluster.QuotaWindow, 0, 2)
	if monthlyLimitCents != nil {
		limit := math.Max(0, *monthlyLimitCents/100)
		var used *float64
		if usedCents != nil {
			value := math.Min(limit, math.Max(0, *usedCents/100))
			used = &value
		}
		window := cluster.QuotaWindow{ID: "xai-monthly-spend", Label: quotaStringPtr("Monthly Spend"), Scope: "account", Mode: "fixed", Status: "unknown", Unit: "currency", Currency: quotaStringPtr("USD"), Used: used, Limit: &limit, PeriodUnit: "month", PeriodValue: quotaFloatPtr(1), Source: "active_probe", ObservedAt: observedAt}
		window.ResetAt = parseProviderTime(billingPeriodEnd)
		normalizeWindowValues(&window)
		windows = append(windows, window)
	}

	if onDemandCapCents != nil && *onDemandCapCents > 0 {
		limit := math.Max(0, *onDemandCapCents/100)
		var used *float64
		if onDemandUsedCents != nil {
			value := math.Max(0, *onDemandUsedCents/100)
			used = &value
		} else if usedCents != nil && monthlyLimitCents != nil {
			value := math.Max(0, (*usedCents-*monthlyLimitCents)/100)
			used = &value
		}
		if used != nil {
			value := math.Min(limit, *used)
			used = &value
		}
		onDemandWindow := cluster.QuotaWindow{ID: "xai-on-demand", Label: quotaStringPtr("On-Demand"), Scope: "account", Mode: "balance", Status: "unknown", Unit: "currency", Currency: quotaStringPtr("USD"), Used: used, Limit: &limit, PeriodUnit: "month", PeriodValue: quotaFloatPtr(1), Source: "active_probe", ObservedAt: observedAt, Priority: 10}
		onDemandWindow.ResetAt = parseProviderTime(billingPeriodEnd)
		normalizeWindowValues(&onDemandWindow)
		windows = append(windows, onDemandWindow)
	}

	return windows, xaiPlanFromMonthlyLimit(monthlyLimitCents), nil
}

func normalizedProviderRatio(value float64) float64 {
	if value > 1 {
		value /= 100
	}
	return math.Max(0, math.Min(1, value))
}

func normalizeWindowValues(window *cluster.QuotaWindow) {
	cluster.NormalizeQuotaWindowValues(window)
}

func structuredPeriod(duration int64, rawUnit string) (string, *float64, *int64) {
	if duration <= 0 {
		return "unknown", nil, nil
	}
	unit := strings.ToLower(strings.TrimSpace(rawUnit))
	unit = strings.TrimPrefix(unit, "time_unit_")
	value := float64(duration)
	var seconds int64
	var ok bool
	switch unit {
	case "minute", "minutes", "m":
		unit = "minute"
		seconds, ok = checkedPeriodSeconds(duration, 60)
	case "hour", "hours", "h":
		unit = "hour"
		seconds, ok = checkedPeriodSeconds(duration, 60*60)
	case "day", "days", "d":
		unit = "day"
		seconds, ok = checkedPeriodSeconds(duration, 24*60*60)
	case "week", "weeks", "w":
		unit = "week"
		seconds, ok = checkedPeriodSeconds(duration, 7*24*60*60)
	case "month", "months":
		unit, seconds = "month", 0
		ok = true
	default:
		return "unknown", nil, nil
	}
	if !ok {
		return "unknown", nil, nil
	}
	if seconds > 0 {
		return unit, &value, &seconds
	}
	return unit, &value, nil
}

func checkedPeriodSeconds(duration, multiplier int64) (int64, bool) {
	if duration <= 0 || multiplier <= 0 || duration > math.MaxInt64/multiplier {
		return 0, false
	}
	return duration * multiplier, true
}

func periodSeconds(unit string, value float64) *int64 {
	if value <= 0 || math.Trunc(value) != value {
		return nil
	}
	integer := int64(value)
	switch unit {
	case "minute":
		return quotaInt64Ptr(integer * 60)
	case "hour":
		return quotaInt64Ptr(integer * 60 * 60)
	case "day":
		return quotaInt64Ptr(integer * 24 * 60 * 60)
	case "week":
		return quotaInt64Ptr(integer * 7 * 24 * 60 * 60)
	default:
		return nil
	}
}

func parseProviderTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

func firstProviderTime(values ...string) *time.Time {
	for _, value := range values {
		if parsed := parseProviderTime(value); parsed != nil {
			return parsed
		}
	}
	return nil
}

func normalizeProviderScope(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "project":
		return "project"
	case "model":
		return "model"
	case "organization", "org":
		return "organization"
	case "account", "request", "requests", "user":
		return "account"
	default:
		return "unknown"
	}
}

func nonNegativeFloat(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	normalized := math.Max(0, *value)
	return &normalized
}

func firstFloat(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstNonNegativeFloat(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 {
			return value
		}
	}
	return nil
}

func firstPositiveFlexInt64(values ...flexInt64) int64 {
	for _, value := range values {
		if value.set && value.value > 0 {
			return value.value
		}
	}
	return 0
}

func kimiLimitPeriod(input kimiLimitItem) (int64, string) {
	var windowDuration flexInt64
	var windowUnit string
	if input.Window != nil {
		windowDuration = input.Window.Duration
		windowUnit = input.Window.TimeUnit
	}
	var detailDuration flexInt64
	var detailUnit string
	if input.Detail != nil {
		detailDuration = input.Detail.Duration
		detailUnit = input.Detail.TimeUnit
	}
	return firstPositiveFlexInt64(windowDuration, input.Duration, detailDuration), firstNonEmptyString(windowUnit, input.TimeUnit, detailUnit)
}

func kimiUsageResetAt(input *kimiUsageDetail) *time.Time {
	if input == nil {
		return nil
	}
	return firstProviderTime(input.ResetAtSnake, input.ResetAt, input.ResetTimeSnake, input.ResetTime)
}

func kimiUsageResetSeconds(input *kimiUsageDetail) *float64 {
	if input == nil {
		return nil
	}
	return firstNonNegativeFloat(flexFloatValue(input.ResetInSnake), flexFloatValue(input.ResetIn), flexFloatValue(input.TTL))
}

func detailFloat(detail *kimiUsageDetail, field string) *float64 {
	if detail == nil {
		return nil
	}
	switch field {
	case "used":
		return flexFloatValue(detail.Used)
	case "limit":
		return flexFloatValue(detail.Limit)
	case "remaining":
		return flexFloatValue(detail.Remaining)
	default:
		return nil
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func quotaFloatValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func quotaInt64Ptr(value int64) *int64 { return &value }
