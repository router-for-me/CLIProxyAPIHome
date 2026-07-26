package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	codexQuotaHeaderPrefix        = "X-Codex-"
	quotaHeaderValueMaxLength     = 4096
	quotaHeaderSnapshotFreshness  = 30 * time.Minute
	quotaMaxFutureObservationSkew = 5 * time.Minute
	quotaLowRemainingRatio        = 0.20
)

func sanitizeUsageQuotaHeaders(payload string) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(gjson.Get(payload, "provider").String()))
	filtered := make(map[string]string)
	collect := func(result gjson.Result) {
		if provider != "codex" || !result.IsObject() {
			return
		}
		for rawKey, rawValue := range result.Map() {
			key := http.CanonicalHeaderKey(strings.TrimSpace(rawKey))
			value := quotaHeaderResultValue(rawValue)
			if isCodexQuotaHeaderKey(key) && value != "" && len(value) <= quotaHeaderValueMaxLength {
				filtered[key] = value
			}
		}
	}
	collect(gjson.Get(payload, "quota_headers"))
	responseHeaders := gjson.Get(payload, "response_headers")
	collect(responseHeaders)
	upstreamRequestID := quotaResponseHeaderRequestID(responseHeaders)

	out, errDelete := sjson.Delete(payload, "quota_headers")
	if errDelete != nil {
		return "", errDelete
	}
	out, errDelete = sjson.Delete(out, "response_headers")
	if errDelete != nil {
		return "", errDelete
	}
	out, errSanitizeRequestIDs := sanitizeUsageUpstreamRequestIDs(out)
	if errSanitizeRequestIDs != nil {
		return "", errSanitizeRequestIDs
	}
	if strings.TrimSpace(gjson.Get(out, "upstream_request_id").String()) == "" && upstreamRequestID != "" {
		out, errDelete = sjson.Set(out, "upstream_request_id", upstreamRequestID)
		if errDelete != nil {
			return "", errDelete
		}
	}
	if len(filtered) == 0 {
		return out, nil
	}
	out, errSet := sjson.Set(out, "quota_headers", filtered)
	if errSet != nil {
		return "", errSet
	}
	return out, nil
}

func sanitizeUsageUpstreamRequestIDs(payload string) (string, error) {
	out := payload
	for _, path := range []string{"upstream_request_id", "upstream.request_id", "response.request_id", "response.id"} {
		value := gjson.Get(out, path)
		if !value.Exists() {
			continue
		}
		safeValue := SafeQuotaRequestID(value.String())
		var errUpdate error
		if safeValue == "" {
			out, errUpdate = sjson.Delete(out, path)
		} else {
			out, errUpdate = sjson.Set(out, path, safeValue)
		}
		if errUpdate != nil {
			return "", errUpdate
		}
	}
	return out, nil
}

func quotaHeaderResultValue(value gjson.Result) string {
	if value.IsArray() {
		for _, candidate := range value.Array() {
			if trimmed := strings.TrimSpace(candidate.String()); trimmed != "" {
				return trimmed
			}
		}
		return ""
	}
	return strings.TrimSpace(value.String())
}

func quotaResponseHeaderRequestID(headers gjson.Result) string {
	if !headers.IsObject() {
		return ""
	}
	for rawKey, rawValue := range headers.Map() {
		key := http.CanonicalHeaderKey(strings.TrimSpace(rawKey))
		switch key {
		case "X-Upstream-Request-Id", "X-Request-Id", "Openai-Request-Id":
			if value := SafeQuotaRequestID(quotaHeaderResultValue(rawValue)); value != "" {
				return value
			}
		}
	}
	return ""
}

func upsertQuotaFromUsagePayloadTx(ctx context.Context, tx *gorm.DB, payload string, metadata UsageRuntimeMetadata, receivedAt time.Time) error {
	input, ok := quotaSnapshotWriteFromUsagePayload(payload, metadata, receivedAt)
	if !ok {
		return nil
	}
	record, found, errResolve := resolveQuotaObservationCredential(ctx, tx, input.CredentialID)
	if errResolve != nil {
		return errResolve
	}
	if !found || normalizeQuotaProviderID(record.Provider) != "codex" {
		return nil
	}
	auth, errAuth := quotaAuthFromRecord(&record)
	if errAuth != nil {
		return errAuth
	}
	credentialType := quotaCredentialType(auth)
	if credentialType != "oauth" && credentialType != "file_auth" {
		return nil
	}
	reportedType := normalizeUsageObservabilityCredentialType(gjson.Get(payload, "auth_type").String())
	if reportedType != "unknown" && reportedType != credentialType {
		return nil
	}
	input.CredentialID = record.UUID
	_, errUpsert := upsertQuotaSnapshotDB(ctx, tx, input)
	return errUpsert
}

func resolveQuotaObservationCredential(ctx context.Context, tx *gorm.DB, authIndex string) (AuthRecord, bool, error) {
	if tx == nil || strings.TrimSpace(authIndex) == "" {
		return AuthRecord{}, false, nil
	}
	for _, column := range []string{"uuid", "index", "id"} {
		var record AuthRecord
		errFind := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(map[string]any{column: strings.TrimSpace(authIndex)}).Order("uuid ASC").First(&record).Error
		if errFind == nil {
			return record, true, nil
		}
		if !errors.Is(errFind, gorm.ErrRecordNotFound) {
			return AuthRecord{}, false, errFind
		}
	}
	return AuthRecord{}, false, nil
}

func quotaSnapshotWriteFromUsagePayload(payload string, metadata UsageRuntimeMetadata, receivedAt time.Time) (QuotaSnapshotWrite, bool) {
	provider := strings.ToLower(strings.TrimSpace(gjson.Get(payload, "provider").String()))
	credentialID := strings.TrimSpace(gjson.Get(payload, "auth_index").String())
	if provider != "codex" || credentialID == "" {
		return QuotaSnapshotWrite{}, false
	}
	headerResult := gjson.Get(payload, "quota_headers")
	if !headerResult.IsObject() {
		return QuotaSnapshotWrite{}, false
	}
	headers := make(http.Header)
	for key, value := range headerResult.Map() {
		headers.Set(http.CanonicalHeaderKey(key), value.String())
	}
	observedAt, errTime := time.Parse(time.RFC3339Nano, strings.TrimSpace(gjson.Get(payload, "timestamp").String()))
	if errTime != nil {
		return QuotaSnapshotWrite{}, false
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	} else {
		receivedAt = receivedAt.UTC()
	}
	observedAt = observedAt.UTC()
	if observedAt.After(receivedAt.Add(quotaMaxFutureObservationSkew)) {
		observedAt = receivedAt
	}
	windows := parseCodexQuotaHeaderWindows(headers, observedAt)
	if len(windows) == 0 {
		return QuotaSnapshotWrite{}, false
	}
	status := aggregateQuotaWindowStatus(windows)
	collectionStatus := "partial"
	expiresAt := observedAt.UTC().Add(quotaHeaderSnapshotFreshness)
	maxAcceptedObservedAt := receivedAt.Add(quotaMaxFutureObservationSkew)
	plan := codexQuotaPlan(firstQuotaHeaderValue(headers, "X-Codex-Plan-Type"))
	homeID := strings.TrimSpace(metadata.HomeIP)
	if metadata.HomePort > 0 {
		homeID = fmt.Sprintf("%s:%d", homeID, metadata.HomePort)
	}
	if homeID == ":0" {
		homeID = ""
	}
	runtime := &QuotaRuntime{
		HomeID:       homeID,
		HomeLabel:    homeID,
		CPANodeID:    firstNonEmptyQuotaString(metadata.CPANodeID, metadata.CPAIP),
		CPANodeLabel: firstNonEmptyQuotaString(metadata.CPALabel, metadata.CPANodeID, metadata.CPAIP),
	}
	return QuotaSnapshotWrite{
		CredentialID: credentialID, QuotaStatus: status, CollectionStatus: collectionStatus, Source: "response_header",
		ObservedAt: &observedAt, ReceivedAt: &receivedAt, MaxAcceptedObservedAt: &maxAcceptedObservedAt, ExpiresAt: &expiresAt, LastAttemptAt: &observedAt, LastSuccessAt: &observedAt,
		NextProbeAt: &observedAt, Runtime: runtime, Plan: plan, ParserVersion: quotaSnapshotSchemaVersion,
		CollectorVersion: quotaSnapshotSchemaVersion, ClearProbeLease: false, ReplaceWindows: false, Windows: windows,
	}, true
}

type codexHeaderLimitFamily struct {
	groupName string
	limitKey  string
}

func parseCodexQuotaHeaderWindows(headers http.Header, observedAt time.Time) []QuotaWindow {
	headers = canonicalQuotaHeaders(headers)
	groupNames := make(map[string]struct{})
	for key := range headers {
		if groupName := codexQuotaHeaderGroupName(key); groupName != "" {
			groupNames[groupName] = struct{}{}
		}
	}
	families := make([]codexHeaderLimitFamily, 0, len(groupNames))
	for groupName := range groupNames {
		families = append(families, codexHeaderLimitFamily{groupName: groupName, limitKey: codexQuotaLimitKey(groupName)})
	}
	sort.Slice(families, func(i, j int) bool {
		if families[i].limitKey != families[j].limitKey {
			return families[i].limitKey < families[j].limitKey
		}
		return families[i].groupName < families[j].groupName
	})

	activeLimitKey, activeScope, activeScopeID, activeLimitOK := codexActiveQuotaLimitFamily(firstQuotaHeaderValue(headers, "X-Codex-Active-Limit"))
	activePriority := 0
	var activeLabel *string
	if activeLimitOK && activeLimitKey != "" {
		activePriority = 20 + len(families)*10
	}
	if activeLimitOK {
		for index, family := range families {
			if family.limitKey != activeLimitKey {
				continue
			}
			if activeLimitKey != "" {
				activePriority = 20 + index*10
			}
			if value := firstQuotaHeaderValue(headers, codexQuotaHeaderPrefix+family.groupName+"-Limit-Name"); value != "" {
				activeLabel = &value
			}
			break
		}
	}

	windows := make([]QuotaWindow, 0, 4)
	if activeLimitOK {
		windows = appendCodexHeaderLimitWindows(windows, headers, codexQuotaHeaderPrefix, activeLimitKey, activeLabel, activeScope, activeScopeID, activePriority, observedAt)
	}
	for index, family := range families {
		prefix := codexQuotaHeaderPrefix + family.groupName + "-"
		var label *string
		if value := firstQuotaHeaderValue(headers, prefix+"Limit-Name"); value != "" {
			label = &value
		}
		var scopeID *string
		if family.limitKey != "" {
			value := "codex_" + strings.ReplaceAll(family.limitKey, "-", "_")
			scopeID = &value
		}
		scope := "model"
		if family.limitKey == "" {
			scope = "account"
		}
		familyWindows := appendCodexHeaderLimitWindows(nil, headers, prefix, family.limitKey, label, scope, scopeID, 20+index*10, observedAt)
		if activeLimitOK && family.limitKey == activeLimitKey {
			windows = mergeCodexActiveHeaderWindows(windows, familyWindows)
			continue
		}
		windows = append(windows, familyWindows...)
	}
	return windows
}

func codexActiveQuotaLimitFamily(value string) (string, string, *string, bool) {
	value = strings.TrimSpace(value)
	if !validCodexQuotaLimitIdentifier(value) {
		return "", "", nil, false
	}
	switch strings.ToLower(value) {
	case "codex", "premium":
		return "", "account", nil, true
	}
	limitKey := codexQuotaLimitKey(value)
	if limitKey == "" {
		return "", "", nil, false
	}
	scopeIDValue := "codex_" + strings.ReplaceAll(limitKey, "-", "_")
	return limitKey, "model", &scopeIDValue, true
}

func validCodexQuotaLimitIdentifier(value string) bool {
	if value == "" || len(value) > quotaWindowTextMaxLength {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func mergeCodexActiveHeaderWindows(windows []QuotaWindow, familyWindows []QuotaWindow) []QuotaWindow {
	for _, familyWindow := range familyWindows {
		merged := false
		for index := range windows {
			window := &windows[index]
			if window.ID != familyWindow.ID || window.WindowSeconds == nil || familyWindow.WindowSeconds == nil || *window.WindowSeconds != *familyWindow.WindowSeconds {
				continue
			}
			if window.Label == nil && familyWindow.Label != nil {
				window.Label = familyWindow.Label
			}
			merged = true
			break
		}
		if !merged {
			windows = append(windows, familyWindow)
		}
	}
	return windows
}

func codexQuotaHeaderGroupName(key string) string {
	if !strings.HasPrefix(key, codexQuotaHeaderPrefix) {
		return ""
	}
	body := strings.TrimPrefix(key, codexQuotaHeaderPrefix)
	for _, suffix := range []string{
		"-Limit-Name",
		"-Primary-Used-Percent",
		"-Primary-Window-Minutes",
		"-Primary-Reset-At",
		"-Primary-Reset-After-Seconds",
		"-Secondary-Used-Percent",
		"-Secondary-Window-Minutes",
		"-Secondary-Reset-At",
		"-Secondary-Reset-After-Seconds",
	} {
		if strings.HasSuffix(body, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(body, suffix))
		}
	}
	return ""
}

func codexQuotaPlan(planType string) *QuotaPlan {
	planType = strings.TrimSpace(planType)
	if planType == "" {
		return nil
	}
	normalized := strings.ToLower(planType)
	switch normalized {
	case "pro":
		return &QuotaPlan{Name: "Pro 20x", Premium: true}
	case "prolite", "pro-lite", "pro_lite":
		return &QuotaPlan{Name: "Pro 5x", Premium: true}
	case "plus":
		return &QuotaPlan{Name: "Plus"}
	case "team":
		return &QuotaPlan{Name: "Team"}
	case "free":
		return &QuotaPlan{Name: "Free"}
	default:
		return &QuotaPlan{Name: planType}
	}
}

func appendCodexHeaderLimitWindows(windows []QuotaWindow, headers http.Header, prefix string, limitKey string, label *string, scope string, scopeID *string, priority int, observedAt time.Time) []QuotaWindow {
	seenDurations := make(map[int64]struct{}, 2)
	for index, windowName := range []string{"Primary-", "Secondary-"} {
		window, ok := codexHeaderWindow(headers, prefix+windowName, label, scope, scopeID, priority+index, observedAt)
		if !ok || window.WindowSeconds == nil {
			continue
		}
		if _, exists := seenDurations[*window.WindowSeconds]; exists {
			continue
		}
		seenDurations[*window.WindowSeconds] = struct{}{}
		window.ID = codexQuotaWindowID(limitKey, *window.WindowSeconds)
		windows = append(windows, window)
	}
	return windows
}

func codexHeaderWindow(headers http.Header, prefix string, label *string, scope string, scopeID *string, priority int, observedAt time.Time) (QuotaWindow, bool) {
	usedPercent, okUsed := quotaFloatHeader(headers, prefix+"Used-Percent")
	windowMinutes, okWindow := quotaIntHeader(headers, prefix+"Window-Minutes")
	if !okUsed || !okWindow || windowMinutes <= 0 || math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) {
		return QuotaWindow{}, false
	}
	usedRatio := math.Max(0, math.Min(1, usedPercent/100))
	remainingRatio := 1 - usedRatio
	used, remaining, limit := usedRatio*100, remainingRatio*100, float64(100)
	windowSeconds := windowMinutes * 60
	resetAt, okReset := codexQuotaResetAt(headers, prefix, observedAt)
	if !okReset {
		return QuotaWindow{}, false
	}
	periodUnit, periodValue := quotaPeriodFromSeconds(windowSeconds)
	status := quotaStatusFromRemainingRatio(remainingRatio)
	return QuotaWindow{
		Label: label, Scope: scope, ScopeID: scopeID, Mode: "rolling", Status: status, Unit: "percentage",
		Used: &used, Remaining: &remaining, Limit: &limit, UsedRatio: &usedRatio, RemainingRatio: &remainingRatio,
		ResetAt: &resetAt, WindowSeconds: &windowSeconds, PeriodUnit: periodUnit, PeriodValue: periodValue,
		Source: "response_header", ObservedAt: observedAt.UTC(), Priority: priority,
	}, true
}

func codexQuotaLimitKey(value string) string {
	key := quotaSlug(value)
	if key == "codex" || key == "premium" {
		return ""
	}
	return strings.TrimPrefix(key, "codex-")
}

func codexQuotaWindowID(limitKey string, seconds int64) string {
	prefix := "codex"
	if key := codexQuotaLimitKey(limitKey); key != "" {
		prefix += "-" + key
	}
	return prefix + "-" + codexQuotaWindowIDSuffix(seconds)
}

func codexQuotaWindowIDSuffix(seconds int64) string {
	unit, value := quotaPeriodFromSeconds(seconds)
	if value != nil && unit != "unknown" {
		return quotaSlug(strconv.FormatFloat(*value, 'f', -1, 64) + "-" + unit)
	}
	return strconv.FormatInt(seconds, 10) + "-second"
}

func codexQuotaResetAt(headers http.Header, prefix string, observedAt time.Time) (time.Time, bool) {
	if unixValue, ok := quotaIntHeader(headers, prefix+"Reset-At"); ok && unixValue > 0 {
		return time.Unix(unixValue, 0).UTC(), true
	}
	if seconds, ok := quotaIntHeader(headers, prefix+"Reset-After-Seconds"); ok && seconds >= 0 {
		return observedAt.UTC().Add(time.Duration(seconds) * time.Second), true
	}
	return time.Time{}, false
}

func quotaPeriodFromSeconds(seconds int64) (string, *float64) {
	var unit string
	var value float64
	switch {
	case seconds == 30*24*60*60 || seconds == 2628000:
		unit, value = "month", 1
	case seconds%(7*24*60*60) == 0:
		unit, value = "week", float64(seconds/(7*24*60*60))
	case seconds%(24*60*60) == 0:
		unit, value = "day", float64(seconds/(24*60*60))
	case seconds%(60*60) == 0:
		unit, value = "hour", float64(seconds/(60*60))
	case seconds%60 == 0:
		unit, value = "minute", float64(seconds/60)
	default:
		return "unknown", nil
	}
	return unit, &value
}

func aggregateQuotaWindowStatus(windows []QuotaWindow) string {
	status := "healthy"
	for _, window := range windows {
		if window.Status == "exhausted" {
			return "exhausted"
		}
		if window.Status == "low" {
			status = "low"
		}
	}
	return status
}

func quotaStatusFromRemainingRatio(remaining float64) string {
	if remaining <= 0 {
		return "exhausted"
	}
	if remaining <= quotaLowRemainingRatio {
		return "low"
	}
	return "healthy"
}

func canonicalQuotaHeaders(headers http.Header) http.Header {
	canonical := make(http.Header, len(headers))
	for key, values := range headers {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		for _, value := range values {
			if canonicalKey != "" && strings.TrimSpace(value) != "" {
				canonical.Add(canonicalKey, strings.TrimSpace(value))
			}
		}
	}
	return canonical
}

func isCodexQuotaHeaderKey(key string) bool {
	if key == "X-Codex-Plan-Type" || key == "X-Codex-Active-Limit" {
		return true
	}
	if !strings.HasPrefix(key, codexQuotaHeaderPrefix) {
		return false
	}
	for _, suffix := range []string{"-Limit-Name", "-Used-Percent", "-Window-Minutes", "-Reset-At", "-Reset-After-Seconds"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

func firstQuotaHeaderValue(headers http.Header, key string) string {
	for _, value := range headers.Values(key) {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func quotaFloatHeader(headers http.Header, key string) (float64, bool) {
	value := firstQuotaHeaderValue(headers, key)
	parsed, errParse := strconv.ParseFloat(value, 64)
	return parsed, value != "" && errParse == nil && parsed >= 0
}

func quotaIntHeader(headers http.Header, key string) (int64, bool) {
	value := firstQuotaHeaderValue(headers, key)
	parsed, errParse := strconv.ParseInt(value, 10, 64)
	return parsed, value != "" && errParse == nil
}

func quotaSlug(value string) string {
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
