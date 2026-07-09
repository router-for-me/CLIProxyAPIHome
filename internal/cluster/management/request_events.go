package management

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

const requestEventsExportDefaultLimit = usageExportDefaultLimit

func (h *Handler) ListRequestEvents(c *gin.Context) {
	query, errParse := parseUsageRecordHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()

	result, errRecords := h.repo.ListUsageObservabilityRecords(ctx, usageObservabilityRecordQueryFromHTTP(query))
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "request_events_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		items = append(items, h.requestEventResponse(&result.Records[index]))
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  items,
		"total":  result.Total,
		"limit":  query.Limit,
		"offset": query.Offset,
		"sort":   query.Sort,
	})
}

func (h *Handler) GetRequestEvent(c *gin.Context) {
	id := requestEventUsageIDFromParam(c.Param("id"))
	includePayload := parseUsageBool(c.Query("include_payload"))
	includeLogs := parseUsageBool(c.Query("include_logs"))

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.GetUsageObservabilityRecord(ctx, id)
	if errRecord != nil {
		if errors.Is(errRecord, gorm.ErrRecordNotFound) {
			respondError(c, http.StatusNotFound, "request_event_not_found", errRecord)
			return
		}
		respondError(c, http.StatusInternalServerError, "request_event_load_failed", errRecord)
		return
	}

	var payloadSummary any
	if includePayload {
		summary, errSummary := h.repo.GetUsageObservabilityPayloadSummary(ctx, id)
		if errSummary != nil {
			respondError(c, http.StatusInternalServerError, "request_event_payload_summary_load_failed", errSummary)
			return
		}
		payloadSummary = requestEventPayloadSummaryResponse(summary)
	}
	logExcerpt := []string{}
	if includeLogs {
		logExcerpt = h.usageRequestLogExcerpt(record)
	}
	c.JSON(http.StatusOK, gin.H{
		"event":           h.requestEventResponse(record),
		"payload_summary": payloadSummary,
		"log_excerpt":     logExcerpt,
	})
}

func (h *Handler) ExportRequestEvents(c *gin.Context) {
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "jsonl" {
		respondError(c, http.StatusBadRequest, "invalid_format", fmt.Errorf("format must be csv or jsonl"))
		return
	}
	query, errParse := parseUsageRecordHTTPQueryWithoutPagination(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	query.Limit = requestEventsExportDefaultLimit
	query.Offset = 0

	ctx, cancel := h.requestContext(c)
	defer cancel()
	repoQuery := usageObservabilityRecordQueryFromHTTP(query)
	repoQuery.MaxLimit = requestEventsExportDefaultLimit
	result, errRecords := h.repo.ListUsageObservabilityRecords(ctx, repoQuery)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "request_events_export_load_failed", errRecords)
		return
	}

	filename := "request-events." + format
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if format == "jsonl" {
		c.Header("Content-Type", "application/x-ndjson")
		c.Status(http.StatusOK)
		encoder := json.NewEncoder(c.Writer)
		for index := range result.Records {
			if errEncode := encoder.Encode(h.requestEventExportRecordMap(&result.Records[index])); errEncode != nil {
				return
			}
		}
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Status(http.StatusOK)
	writer := csv.NewWriter(c.Writer)
	_ = writer.Write(requestEventExportCSVHeader())
	for index := range result.Records {
		_ = writer.Write(h.requestEventExportCSVRow(&result.Records[index]))
	}
	writer.Flush()
}

func (h *Handler) GetRequestEventFilterOptions(c *gin.Context) {
	query, errParse := parseUsageRecordHTTPQueryWithoutPagination(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	options, errOptions := h.repo.UsageObservabilityFilterOptions(ctx, usageObservabilityRecordQueryFromHTTP(query))
	if errOptions != nil {
		respondError(c, http.StatusInternalServerError, "request_event_filter_options_load_failed", errOptions)
		return
	}
	c.JSON(http.StatusOK, requestEventFilterOptionsResponse(options))
}

func (h *Handler) requestEventResponse(record *cluster.UsageObservabilityRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	recordCopy := *record
	available, _, _ := h.usageRequestLogAvailability(recordCopy.Runtime.HomeIP, recordCopy.RequestID)
	recordCopy.Runtime.RequestLogAvailable = available
	return requestEventResponse(&recordCopy)
}

func requestEventResponse(record *cluster.UsageObservabilityRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	statusCode := requestEventHTTPStatusCode(record)
	upstreamStatusCode := requestEventUpstreamStatusCode(record)
	return gin.H{
		"id":                   "evt_" + record.ID,
		"timestamp":            record.Timestamp.UTC().Format(time.RFC3339Nano),
		"request_id":           record.RequestID,
		"upstream_request_id":  emptyStringAsNil(record.UpstreamRequestID),
		"event_type":           requestEventType(record),
		"status":               record.Status,
		"failed":               record.Failed,
		"status_code":          statusCode,
		"upstream_status_code": upstreamStatusCode,
		"provider":             record.Provider,
		"model":                record.Model,
		"original_model":       record.OriginalModel,
		"model_alias":          requestEventModelAlias(record),
		"endpoint":             record.Endpoint,
		"source":               emptyStringAsNil(record.Source),
		"executor_type":        record.ExecutorType,
		"service_tier":         emptyStringAsNil(record.ServiceTier),
		"reasoning_effort":     emptyStringAsNil(record.ReasoningEffort),
		"runtime":              requestEventRuntimeResponse(record),
		"credential":           requestEventCredentialResponse(record),
		"client":               requestEventClientResponse(record),
		"error":                requestEventErrorResponse(record),
		"tokens":               requestEventTokensResponse(record),
		"performance":          requestEventPerformanceResponse(record),
		"billing":              requestEventBillingResponse(record),
		"related":              requestEventRelatedResponse(record),
	}
}

func requestEventType(record *cluster.UsageObservabilityRecord) string {
	if record == nil {
		return "completion"
	}
	if eventType := strings.TrimSpace(record.EventType); eventType != "" {
		return eventType
	}
	return "completion"
}

func requestEventHTTPStatusCode(record *cluster.UsageObservabilityRecord) any {
	if record == nil {
		return nil
	}
	if record.StatusCode > 0 {
		return record.StatusCode
	}
	if !record.Failed {
		return http.StatusOK
	}
	return nil
}

func requestEventUpstreamStatusCode(record *cluster.UsageObservabilityRecord) any {
	if record == nil {
		return nil
	}
	if record.UpstreamStatusCode > 0 {
		return record.UpstreamStatusCode
	}
	if record.Error == nil || record.Error.UpstreamStatusCode <= 0 {
		return requestEventHTTPStatusCode(record)
	}
	return record.Error.UpstreamStatusCode
}

func requestEventModelAlias(record *cluster.UsageObservabilityRecord) any {
	if record == nil {
		return nil
	}
	alias := strings.TrimSpace(record.OriginalModel)
	if alias == "" || alias == strings.TrimSpace(record.Model) {
		return nil
	}
	return alias
}

func requestEventRuntimeResponse(record *cluster.UsageObservabilityRecord) gin.H {
	homeIP := strings.TrimSpace(record.Runtime.HomeIP)
	homeID := any(nil)
	if homeIP != "" {
		homeID = homeIP
		if record.Runtime.HomePort > 0 {
			homeID = fmt.Sprintf("%s:%d", homeIP, record.Runtime.HomePort)
		}
	}
	cpaLabel := strings.TrimSpace(record.Runtime.CPALabel)
	if cpaLabel == "" && strings.TrimSpace(record.Runtime.CPAIP) != "" {
		cpaLabel = record.Runtime.CPAIP
		if record.Runtime.CPAPort > 0 {
			cpaLabel = fmt.Sprintf("%s:%d", record.Runtime.CPAIP, record.Runtime.CPAPort)
		}
	}
	return gin.H{
		"home_ip":     emptyStringAsNil(homeIP),
		"home_port":   optionalPositiveIntValue(record.Runtime.HomePort),
		"home_id":     homeID,
		"cpa_node_id": emptyStringAsNil(record.Runtime.CPANodeID),
		"cpa_ip":      emptyStringAsNil(record.Runtime.CPAIP),
		"cpa_port":    optionalPositiveIntValue(record.Runtime.CPAPort),
		"cpa_label":   emptyStringAsNil(cpaLabel),
	}
}

func requestEventCredentialResponse(record *cluster.UsageObservabilityRecord) gin.H {
	return gin.H{
		"credential_type": record.Credential.CredentialType,
		"credential_id":   record.Credential.CredentialID,
		"auth_index":      record.Credential.AuthIndex,
		"provider":        record.Credential.Provider,
		"label":           record.Credential.Label,
		"source":          record.Credential.Source,
		"api_key_preview": emptyStringAsNil(record.Credential.APIKeyPreview),
	}
}

func requestEventClientResponse(record *cluster.UsageObservabilityRecord) gin.H {
	return gin.H{
		"user_id":           optionalUintValue(record.Client.UserID),
		"username":          record.Client.Username,
		"client_key_id":     optionalUintValue(record.Client.APIKeyID),
		"client_key_label":  record.Client.APIKeyLabel,
		"client_key_masked": record.Client.APIKeyMasked,
		"client_ip":         emptyStringAsNil(record.Client.ClientIP),
	}
}

func requestEventErrorResponse(record *cluster.UsageObservabilityRecord) gin.H {
	var statusCode any
	var upstreamStatusCode any
	var reason any
	var message any
	var bodyPreview any
	if record != nil && record.Error != nil {
		if record.Error.StatusCode > 0 {
			statusCode = record.Error.StatusCode
		}
		if record.Error.UpstreamStatusCode > 0 {
			upstreamStatusCode = record.Error.UpstreamStatusCode
		}
		reason = emptyStringAsNil(record.Error.Reason)
		message = emptyStringAsNil(record.Error.Message)
		bodyPreview = emptyStringAsNil(record.Error.BodyPreview)
	}
	return gin.H{
		"status_code":          statusCode,
		"upstream_status_code": upstreamStatusCode,
		"reason":               reason,
		"message":              message,
		"body_preview":         bodyPreview,
	}
}

func requestEventTokensResponse(record *cluster.UsageObservabilityRecord) gin.H {
	return gin.H{
		"input_tokens":          record.Tokens.InputTokens,
		"output_tokens":         record.Tokens.OutputTokens,
		"reasoning_tokens":      record.Tokens.ReasoningTokens,
		"cached_tokens":         record.Tokens.CachedTokens,
		"cache_read_tokens":     record.Tokens.CacheReadTokens,
		"cache_creation_tokens": record.Tokens.CacheCreationTokens,
		"total_tokens":          record.Tokens.TotalTokens,
	}
}

func requestEventPerformanceResponse(record *cluster.UsageObservabilityRecord) gin.H {
	return gin.H{
		"latency_ms": record.Performance.LatencyMS,
		"ttft_ms":    optionalInt64Value(record.Performance.TTFTMS),
		"tps":        optionalFloat64Value(record.Performance.TPS),
	}
}

func requestEventBillingResponse(record *cluster.UsageObservabilityRecord) gin.H {
	return gin.H{
		"amount":             optionalFloat64Value(record.Billing.Amount),
		"currency":           emptyStringAsNil(record.Billing.Currency),
		"charge_id":          emptyStringAsNil(record.Billing.ChargeID),
		"matched_price_rule": emptyStringAsNil(record.Billing.MatchedPriceRule),
	}
}

func requestEventRelatedResponse(record *cluster.UsageObservabilityRecord) gin.H {
	return gin.H{
		"usage_record_id": record.ID,
		"request_log": gin.H{
			"request_id":   record.RequestID,
			"home_ip":      record.Runtime.HomeIP,
			"available":    record.Runtime.RequestLogAvailable,
			"download_url": requestEventRequestLogDownloadURL(record),
		},
	}
}

func requestEventRequestLogDownloadURL(record *cluster.UsageObservabilityRecord) any {
	if record == nil || !record.Runtime.RequestLogAvailable || strings.TrimSpace(record.RequestID) == "" {
		return nil
	}
	value := "/request-log-by-id/" + url.PathEscape(record.RequestID)
	if homeIP := strings.TrimSpace(record.Runtime.HomeIP); homeIP != "" {
		value += "?home_ip=" + url.QueryEscape(homeIP)
	}
	return value
}

func requestEventUsageIDFromParam(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "evt_")
	return value
}

func requestEventPayloadSummaryResponse(summary *cluster.UsageObservabilityPayloadSummary) gin.H {
	payload := usagePayloadSummaryResponse(summary)
	payload["body_preview"] = nil
	return payload
}

func (h *Handler) requestEventExportRecordMap(record *cluster.UsageObservabilityRecord) map[string]any {
	item := h.requestEventResponse(record)
	out := flattenRequestEventExportMap(item)
	return out
}

func flattenRequestEventExportMap(item gin.H) map[string]any {
	out := map[string]any{}
	for _, key := range requestEventExportCSVHeader() {
		out[key] = requestEventFlattenedValue(item, key)
	}
	return out
}

func requestEventFlattenedValue(item gin.H, key string) any {
	switch key {
	case "home_ip", "home_port", "home_id", "cpa_node_id", "cpa_ip", "cpa_port", "cpa_label":
		return nestedMapValue(item, "runtime", key)
	case "credential_type", "credential_id", "auth_index", "credential_provider", "credential_label", "credential_source", "credential_api_key_preview":
		credentialKey := strings.TrimPrefix(key, "credential_")
		if key == "credential_type" || key == "credential_id" {
			credentialKey = key
		}
		return nestedMapValue(item, "credential", credentialKey)
	case "user_id", "username", "client_key_id", "client_key_label", "client_key_masked", "client_ip":
		return nestedMapValue(item, "client", key)
	case "input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens":
		return nestedMapValue(item, "tokens", key)
	case "latency_ms", "ttft_ms", "tps":
		return nestedMapValue(item, "performance", key)
	case "amount", "currency", "charge_id", "matched_price_rule":
		return nestedMapValue(item, "billing", key)
	case "error_status_code":
		return nestedMapValue(item, "error", "status_code")
	case "error_upstream_status_code":
		return nestedMapValue(item, "error", "upstream_status_code")
	case "error_reason", "error_message", "error_body_preview":
		return nestedMapValue(item, "error", strings.TrimPrefix(key, "error_"))
	case "usage_record_id":
		return nestedMapValue(item, "related", "usage_record_id")
	case "request_log_available":
		return nestedMapValuePath(item, "related", "request_log", "available")
	default:
		return item[key]
	}
}

func nestedMapValue(item gin.H, section string, key string) any {
	return nestedMapValuePath(item, section, key)
}

func nestedMapValuePath(item gin.H, path ...string) any {
	var current any = item
	for _, key := range path {
		typed, ok := current.(gin.H)
		if !ok {
			if fallback, okMap := current.(map[string]any); okMap {
				current = fallback[key]
				continue
			}
			return nil
		}
		current = typed[key]
	}
	return current
}

func requestEventExportCSVHeader() []string {
	return []string{
		"id", "timestamp", "request_id", "upstream_request_id", "event_type", "status", "failed", "status_code", "upstream_status_code",
		"provider", "model", "original_model", "model_alias", "endpoint", "source", "executor_type", "service_tier", "reasoning_effort",
		"home_ip", "home_port", "home_id", "cpa_node_id", "cpa_ip", "cpa_port", "cpa_label",
		"credential_type", "credential_id", "auth_index", "credential_provider", "credential_label", "credential_source", "credential_api_key_preview",
		"user_id", "username", "client_key_id", "client_key_label", "client_key_masked", "client_ip",
		"input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens",
		"latency_ms", "ttft_ms", "tps", "amount", "currency", "charge_id", "matched_price_rule",
		"error_status_code", "error_upstream_status_code", "error_reason", "error_message", "error_body_preview",
		"usage_record_id", "request_log_available",
	}
}

func (h *Handler) requestEventExportCSVRow(record *cluster.UsageObservabilityRecord) []string {
	row := h.requestEventExportRecordMap(record)
	out := make([]string, 0, len(requestEventExportCSVHeader()))
	for _, key := range requestEventExportCSVHeader() {
		out = append(out, usageExportCSVValue(row[key]))
	}
	return out
}

func requestEventFilterOptionsResponse(options cluster.UsageObservabilityFilterOptions) gin.H {
	statusCodes := make([]string, 0, len(options.StatusCodes))
	for _, statusCode := range options.StatusCodes {
		if statusCode <= 0 {
			continue
		}
		statusCodes = append(statusCodes, fmt.Sprint(statusCode))
	}
	return gin.H{
		"event_types":  options.EventTypes,
		"providers":    options.Providers,
		"models":       options.Models,
		"home_ips":     options.HomeIPs,
		"cpa_nodes":    options.CPANodes,
		"status_codes": statusCodes,
	}
}
