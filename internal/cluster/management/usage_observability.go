package management

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

const (
	usageExportDefaultLimit          = 10000
	usageSummaryDefaultWindowSeconds = 24 * 60 * 60
)

func (h *Handler) GetCapabilities(c *gin.Context) {
	usageTokenBreakdownV2 := false
	if h != nil && h.repo != nil {
		ctx, cancel := h.requestContext(c)
		usageTokenBreakdownV2, _ = h.repo.UsageTokenBreakdownV2Ready(ctx)
		cancel()
	}
	c.JSON(http.StatusOK, gin.H{
		"capabilities": gin.H{
			"quota_snapshots":                      true,
			"quota_snapshot_details":               true,
			"quota_recollect":                      h.quotaRecollect != nil,
			"usage":                                true,
			"usage_overview":                       true,
			"usage_records":                        true,
			"usage_record_details":                 true,
			"usage_aggregates":                     true,
			"usage_export":                         true,
			"usage_provider_health":                true,
			"usage_credential_health":              true,
			"usage_realtime":                       true,
			"usage_token_breakdown_v2":             usageTokenBreakdownV2,
			"request_log_index":                    true,
			"request_events":                       true,
			"request_event_details":                true,
			"request_event_export":                 true,
			"request_event_filters":                true,
			"request_events_details":               true,
			"request_events_export":                true,
			"request_events_filters":               true,
			"requestEvents":                        true,
			"requestEventDetails":                  true,
			"requestEventExport":                   true,
			"requestEventFilters":                  true,
			"requestEventsDetails":                 true,
			"requestEventsExport":                  true,
			"requestEventsFilters":                 true,
			"oauth_usage":                          true,
			"logs":                                 true,
			"request_error_logs":                   true,
			"model_channel_bindings":               true,
			"topology":                             true,
			"users":                                true,
			"access_groups":                        true,
			"user_period_limits":                   true,
			"credential_in_flight_snapshots":       true,
			"credential_in_flight_snapshot_cursor": true,
			"credential_concurrency_limits_v2":     true,
			"credential_cooldown_reset":            true,
		},
		"server_info": gin.H{
			"home_version":    buildinfo.Version,
			"home_commit":     buildinfo.Commit,
			"home_build_date": buildinfo.BuildDate,
		},
	})
}

func (h *Handler) GetUsageOverview(c *gin.Context) {
	query, errParse := parseUsageOverviewHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	overview, errOverview := h.repo.UsageObservabilityOverview(ctx, usageObservabilityOverviewQueryFromHTTP(query))
	if errOverview != nil {
		if errors.Is(errOverview, cluster.ErrUsageObservabilityTooManyTrendBuckets) {
			respondUsageHTTPError(c, newUsageHTTPError("invalid_interval_range", "%v", errOverview))
			return
		}
		respondError(c, http.StatusInternalServerError, "usage_overview_load_failed", errOverview)
		return
	}
	c.JSON(http.StatusOK, usageOverviewResponse(overview))
}

func (h *Handler) ListUsageRecords(c *gin.Context) {
	query, errParse := parseUsageRecordHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	result, errRecords := h.repo.ListUsageObservabilityRecords(ctx, usageObservabilityRecordQueryFromHTTP(query))
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "usage_records_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		items = append(items, h.usageRecordSummaryResponse(&result.Records[index]))
	}
	c.JSON(http.StatusOK, gin.H{
		"items":           items,
		"total":           result.Total,
		"limit":           query.Limit,
		"offset":          query.Offset,
		"sort":            query.Sort,
		"sortable_fields": usageRecordSortableFields(),
	})
}

func (h *Handler) GetUsageRecord(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	includePayload := parseUsageBool(c.Query("include_payload"))
	includeLogs := parseUsageBool(c.Query("include_logs"))

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.GetUsageObservabilityRecord(ctx, id)
	if errRecord != nil {
		if errors.Is(errRecord, gorm.ErrRecordNotFound) {
			respondError(c, http.StatusNotFound, "usage_record_not_found", errRecord)
			return
		}
		respondError(c, http.StatusInternalServerError, "usage_record_load_failed", errRecord)
		return
	}

	var payloadSummary any
	if includePayload {
		summary, errSummary := h.repo.GetUsageObservabilityPayloadSummary(ctx, id)
		if errSummary != nil {
			respondError(c, http.StatusInternalServerError, "usage_payload_summary_load_failed", errSummary)
			return
		}
		payloadSummary = usagePayloadSummaryResponse(summary)
	}
	logExcerpt := []string{}
	if includeLogs {
		logExcerpt = h.usageRequestLogExcerpt(record)
	}
	recordCopy := *record
	available, _, _ := h.usageRequestLogAvailability(recordCopy.Runtime.HomeIP, recordCopy.Runtime.HomePort, recordCopy.RequestID)
	recordCopy.Runtime.RequestLogAvailable = available
	c.JSON(http.StatusOK, gin.H{
		"record":          usageRecordSummaryResponse(&recordCopy),
		"payload_summary": payloadSummary,
		"log_excerpt":     logExcerpt,
		"related": gin.H{
			"charge_id": emptyStringAsNil(record.Billing.ChargeID),
			"request_log": gin.H{
				"request_id":   recordCopy.RequestID,
				"home_ip":      recordCopy.Runtime.HomeIP,
				"home_port":    optionalPositiveIntValue(recordCopy.Runtime.HomePort),
				"available":    recordCopy.Runtime.RequestLogAvailable,
				"download_url": requestLogDownloadURL(&recordCopy),
			},
		},
	})
}

func (h *Handler) ListUsageAggregates(c *gin.Context) {
	query, errParse := parseUsageAggregateHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	result, errAggregates := h.repo.ListUsageObservabilityAggregates(ctx, usageObservabilityAggregateQueryFromHTTP(query))
	if errAggregates != nil {
		respondError(c, http.StatusInternalServerError, "usage_aggregates_load_failed", errAggregates)
		return
	}
	items := make([]gin.H, 0, len(result.Items))
	for index := range result.Items {
		items = append(items, usageAggregateItemResponse(&result.Items[index]))
	}
	c.JSON(http.StatusOK, gin.H{
		"group_by":         query.GroupBy,
		"metric":           query.Metric,
		"direction":        query.Direction,
		"items":            items,
		"total":            result.Total,
		"limit":            query.Limit,
		"offset":           query.Offset,
		"sortable_metrics": usageAggregateSortableMetrics(),
	})
}

func (h *Handler) ExportUsageRecords(c *gin.Context) {
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "jsonl" {
		respondError(c, http.StatusBadRequest, "invalid_format", fmt.Errorf("format must be csv or jsonl"))
		return
	}
	query, errParse := parseUsageRecordHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	exportLimit, errLimit := parseUsageLimit(c.Query("limit"), usageExportDefaultLimit, usageExportDefaultLimit)
	if errLimit != nil {
		respondUsageHTTPError(c, errLimit)
		return
	}
	query.Limit = exportLimit

	ctx, cancel := h.requestContext(c)
	defer cancel()
	repoQuery := usageObservabilityRecordQueryFromHTTP(query)
	repoQuery.MaxLimit = usageExportDefaultLimit
	result, errRecords := h.repo.ListUsageObservabilityRecords(ctx, repoQuery)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "usage_export_load_failed", errRecords)
		return
	}

	filename := "usage-records." + format
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if format == "jsonl" {
		c.Header("Content-Type", "application/x-ndjson")
		c.Status(http.StatusOK)
		encoder := json.NewEncoder(c.Writer)
		for index := range result.Records {
			if errEncode := encoder.Encode(h.usageExportRecordMap(&result.Records[index])); errEncode != nil {
				return
			}
		}
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Status(http.StatusOK)
	writer := csv.NewWriter(c.Writer)
	_ = writer.Write(usageExportCSVHeader())
	for index := range result.Records {
		_ = writer.Write(h.usageExportCSVRow(&result.Records[index]))
	}
	writer.Flush()
}

func (h *Handler) GetUsageRealtime(c *gin.Context) {
	query, errParse := parseUsageRealtimeHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	snapshot, errSnapshot := h.repo.UsageObservabilityRealtime(ctx, cluster.UsageObservabilityRealtimeQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
		GroupBy:        query.GroupBy,
		BucketSeconds:  query.BucketSeconds,
	})
	if errSnapshot != nil {
		respondError(c, http.StatusInternalServerError, "usage_realtime_aggregate_failed", errSnapshot)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"window_seconds":       query.WindowSeconds,
		"bucket_seconds":       query.BucketSeconds,
		"updated_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"group_by":             query.GroupBy,
		"velocity":             usageRealtimeVelocityPointResponse(snapshot.Velocity),
		"latency_distribution": usageLatencyDistributionBucketResponse(snapshot.LatencyDistribution),
		"current_usage":        usageAggregateItemsResponse(snapshot.CurrentUsage),
	})
}

func (h *Handler) GetUsageProviderHealth(c *gin.Context) {
	h.getUsageHealth(c, "provider")
}

func (h *Handler) GetUsageCredentialHealth(c *gin.Context) {
	h.getUsageHealth(c, "credential")
}

func (h *Handler) ListRequestLogs(c *gin.Context) {
	query, errParse := parseUsageRecordHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	search := strings.TrimSpace(query.Search)
	responseLimit := query.Limit
	responseOffset := query.Offset
	fileSearch := requestLogSearchNeedsFileScan(search)
	if search != "" {
		query.Search = ""
		if fileSearch {
			query.Limit = usageExportDefaultLimit
			query.Offset = 0
		}
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	repoQuery := usageObservabilityRecordQueryFromHTTP(query)
	if search != "" {
		if fileSearch {
			repoQuery.MaxLimit = usageExportDefaultLimit
		} else {
			repoQuery.RequestLogSearch = search
		}
	}
	result, errRecords := h.repo.ListUsageObservabilityRecords(ctx, repoQuery)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "request_logs_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		item := h.requestLogIndexItemResponse(&result.Records[index])
		if fileSearch && !requestLogIndexItemMatchesSearch(item, search) {
			continue
		}
		items = append(items, item)
	}
	total := result.Total
	if fileSearch {
		total = int64(len(items))
		start := responseOffset
		if start > len(items) {
			start = len(items)
		}
		end := start + responseLimit
		if end > len(items) {
			end = len(items)
		}
		items = items[start:end]
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  items,
		"total":  total,
		"limit":  responseLimit,
		"offset": responseOffset,
	})
}

func (h *Handler) getUsageHealth(c *gin.Context, subject string) {
	query, errParse := parseUsageHealthHTTPQuery(c)
	if errParse != nil {
		respondUsageHTTPError(c, errParse)
		return
	}
	groupBy := "provider"
	if subject == "credential" {
		groupBy = "credential"
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	result, errAggregates := h.repo.ListUsageObservabilityAggregates(ctx, cluster.UsageObservabilityAggregateQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
		GroupBy:        groupBy,
		Metric:         "failed_count",
		Direction:      "desc",
		Limit:          100,
	})
	if errAggregates != nil {
		respondError(c, http.StatusInternalServerError, "usage_health_load_failed", errAggregates)
		return
	}
	details, errDetails := h.repo.UsageObservabilityHealthDetails(ctx, cluster.UsageObservabilityRecordQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
	}, subject)
	if errDetails != nil {
		respondError(c, http.StatusInternalServerError, "usage_health_records_load_failed", errDetails)
		return
	}
	items := make([]gin.H, 0, len(result.Items))
	for index := range result.Items {
		items = append(items, usageHealthItemResponse(&result.Items[index], subject, details))
	}
	c.JSON(http.StatusOK, gin.H{
		"subject":        subject,
		"window_seconds": query.WindowSeconds,
		"items":          items,
	})
}

type usageHTTPError struct {
	Code    string
	Message string
}

func (e *usageHTTPError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return e.Code
}

type usageRecordHTTPQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	Timezone       string
	Status         string
	StatusCode     *int
	RequestID      string
	User           string
	UserID         *uint
	ClientKey      string
	ClientKeyID    *uint
	CredentialID   string
	AuthIndex      string
	ExecutorType   string
	EventType      string
	CPANode        string
	MinLatencyMS   *int64
	MaxLatencyMS   *int64
	MinAmount      *float64
	MaxAmount      *float64
	Search         string
	Limit          int
	Offset         int
	Sort           string
}

type usageAggregateHTTPQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	Timezone       string
	GroupBy        string
	Metric         string
	Direction      string
	Limit          int
	Offset         int
}

type usageOverviewHTTPQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	Timezone       string
	Interval       string
}

type usageRealtimeHTTPQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	Timezone       string
	WindowSeconds  int
	BucketSeconds  int
	GroupBy        string
}

type usageHealthHTTPQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	Timezone       string
	WindowSeconds  int
}

func parseUsageRecordHTTPQuery(c *gin.Context) (usageRecordHTTPQuery, *usageHTTPError) {
	return parseUsageRecordHTTPQueryWithPagination(c, true)
}

func parseUsageRecordHTTPQueryWithoutPagination(c *gin.Context) (usageRecordHTTPQuery, *usageHTTPError) {
	return parseUsageRecordHTTPQueryWithPagination(c, false)
}

func parseUsageRecordHTTPQueryWithPagination(c *gin.Context, includePagination bool) (usageRecordHTTPQuery, *usageHTTPError) {
	query := usageRecordHTTPQuery{}
	if c == nil {
		return query, newUsageHTTPError("invalid_request", "request is unavailable")
	}

	location, timezone, errTimezone := parseUsageTimezone(c.Query("timezone"))
	if errTimezone != nil {
		return query, errTimezone
	}
	from, to, errTimeRange := parseUsageTimeRange(c.Query("from"), c.Query("to"), location)
	if errTimeRange != nil {
		return query, errTimeRange
	}
	query.From = from
	query.To = to
	query.Timezone = timezone

	limit := cluster.UsageObservabilityDefaultRecordLimit
	offset := 0
	if includePagination {
		parsedLimit, errLimit := parseUsageLimit(c.Query("limit"), cluster.UsageObservabilityDefaultRecordLimit, cluster.UsageObservabilityMaxRecordLimit)
		if errLimit != nil {
			return query, errLimit
		}
		parsedOffset, errOffset := parseUsageOffset(c.Query("offset"))
		if errOffset != nil {
			return query, errOffset
		}
		limit = parsedLimit
		offset = parsedOffset
	}
	sortValue, errSort := parseUsageSort(c.Query("sort"))
	if errSort != nil {
		return query, errSort
	}
	minLatency, errMinLatency := optionalNonNegativeInt64(c.Query("min_latency_ms"), "min_latency_ms")
	if errMinLatency != nil {
		return query, errMinLatency
	}
	maxLatency, errMaxLatency := optionalNonNegativeInt64(c.Query("max_latency_ms"), "max_latency_ms")
	if errMaxLatency != nil {
		return query, errMaxLatency
	}
	if minLatency != nil && maxLatency != nil && *minLatency > *maxLatency {
		return query, newUsageHTTPError("invalid_range", "min_latency_ms must not exceed max_latency_ms")
	}
	minAmount, errMinAmount := optionalNonNegativeFloat64(c.Query("min_amount"), "min_amount")
	if errMinAmount != nil {
		return query, errMinAmount
	}
	maxAmount, errMaxAmount := optionalNonNegativeFloat64(c.Query("max_amount"), "max_amount")
	if errMaxAmount != nil {
		return query, errMaxAmount
	}
	if minAmount != nil && maxAmount != nil && *minAmount > *maxAmount {
		return query, newUsageHTTPError("invalid_range", "min_amount must not exceed max_amount")
	}

	query.Provider = strings.TrimSpace(c.Query("provider"))
	query.Model = strings.TrimSpace(c.Query("model"))
	query.HomeIP = firstNonEmptyQuery(c, "home_ip", "home-ip")
	query.Endpoint = strings.TrimSpace(c.Query("endpoint"))
	credentialType, errCredentialType := parseUsageCredentialType(c.Query("credential_type"))
	if errCredentialType != nil {
		return query, errCredentialType
	}
	query.CredentialType = credentialType
	query.Status = strings.TrimSpace(c.Query("status"))
	statusCode, errStatusCode := optionalPositiveInt(c.Query("status_code"), "status_code")
	if errStatusCode != nil {
		return query, errStatusCode
	}
	query.StatusCode = statusCode
	query.RequestID = firstNonEmptyQuery(c, "request_id", "request-id")
	query.User = strings.TrimSpace(c.Query("user"))
	userID, errUserID := optionalUint(c.Query("user_id"), "user_id")
	if errUserID != nil {
		return query, errUserID
	}
	query.ClientKey = strings.TrimSpace(c.Query("client_key"))
	clientKeyID, errClientKeyID := optionalUint(firstNonEmptyQuery(c, "client_key_id", "client-key-id"), "client_key_id")
	if errClientKeyID != nil {
		return query, errClientKeyID
	}
	query.UserID = userID
	query.ClientKeyID = clientKeyID
	query.CredentialID = strings.TrimSpace(c.Query("credential_id"))
	query.AuthIndex = strings.TrimSpace(c.Query("auth_index"))
	query.ExecutorType = strings.TrimSpace(c.Query("executor_type"))
	query.EventType = strings.TrimSpace(c.Query("event_type"))
	query.CPANode = strings.TrimSpace(c.Query("cpa_node"))
	query.MinLatencyMS = minLatency
	query.MaxLatencyMS = maxLatency
	query.MinAmount = minAmount
	query.MaxAmount = maxAmount
	query.Search = strings.TrimSpace(c.Query("search"))
	query.Limit = limit
	query.Offset = offset
	query.Sort = sortValue
	return query, nil
}

func parseUsageAggregateHTTPQuery(c *gin.Context) (usageAggregateHTTPQuery, *usageHTTPError) {
	query := usageAggregateHTTPQuery{}
	if c == nil {
		return query, newUsageHTTPError("invalid_request", "request is unavailable")
	}

	location, timezone, errTimezone := parseUsageTimezone(c.Query("timezone"))
	if errTimezone != nil {
		return query, errTimezone
	}
	from, to, errTimeRange := parseUsageTimeRange(c.Query("from"), c.Query("to"), location)
	if errTimeRange != nil {
		return query, errTimeRange
	}
	applyDefaultUsageSummaryWindow(&from, &to)
	groupBy, errGroupBy := parseUsageGroupBy(c.Query("group_by"))
	if errGroupBy != nil {
		return query, errGroupBy
	}
	metric, errMetric := parseUsageMetric(c.Query("metric"))
	if errMetric != nil {
		return query, errMetric
	}
	direction, errDirection := parseUsageDirection(c.Query("direction"))
	if errDirection != nil {
		return query, errDirection
	}
	limit, errLimit := parseUsageLimit(c.Query("limit"), cluster.UsageObservabilityDefaultGroupLimit, cluster.UsageObservabilityMaxGroupLimit)
	if errLimit != nil {
		return query, errLimit
	}
	offset, errOffset := parseUsageOffset(c.Query("offset"))
	if errOffset != nil {
		return query, errOffset
	}

	query.From = from
	query.To = to
	query.Timezone = timezone
	query.Provider = strings.TrimSpace(c.Query("provider"))
	query.Model = strings.TrimSpace(c.Query("model"))
	query.HomeIP = firstNonEmptyQuery(c, "home_ip", "home-ip")
	query.Endpoint = strings.TrimSpace(c.Query("endpoint"))
	credentialType, errCredentialType := parseUsageCredentialType(c.Query("credential_type"))
	if errCredentialType != nil {
		return query, errCredentialType
	}
	query.CredentialType = credentialType
	query.GroupBy = groupBy
	query.Metric = metric
	query.Direction = direction
	query.Limit = limit
	query.Offset = offset
	return query, nil
}

func parseUsageOverviewHTTPQuery(c *gin.Context) (usageOverviewHTTPQuery, *usageHTTPError) {
	query := usageOverviewHTTPQuery{}
	if c == nil {
		return query, newUsageHTTPError("invalid_request", "request is unavailable")
	}
	location, timezone, errTimezone := parseUsageTimezone(c.Query("timezone"))
	if errTimezone != nil {
		return query, errTimezone
	}
	from, to, errTimeRange := parseUsageTimeRange(c.Query("from"), c.Query("to"), location)
	if errTimeRange != nil {
		return query, errTimeRange
	}
	applyDefaultUsageSummaryWindow(&from, &to)
	query.From = from
	query.To = to
	query.Provider = strings.TrimSpace(c.Query("provider"))
	query.Model = strings.TrimSpace(c.Query("model"))
	query.HomeIP = firstNonEmptyQuery(c, "home_ip", "home-ip")
	query.Endpoint = strings.TrimSpace(c.Query("endpoint"))
	credentialType, errCredentialType := parseUsageCredentialType(c.Query("credential_type"))
	if errCredentialType != nil {
		return query, errCredentialType
	}
	query.CredentialType = credentialType
	query.Timezone = timezone
	interval, errInterval := parseUsageInterval(c.Query("interval"))
	if errInterval != nil {
		return query, errInterval
	}
	query.Interval = interval
	return query, nil
}

func parseUsageRealtimeHTTPQuery(c *gin.Context) (usageRealtimeHTTPQuery, *usageHTTPError) {
	query := usageRealtimeHTTPQuery{WindowSeconds: 900, BucketSeconds: 60, GroupBy: "model"}
	if c == nil {
		return query, newUsageHTTPError("invalid_request", "request is unavailable")
	}
	timezone, errWindow := parseUsageWindowQuery(c, query.WindowSeconds, &query.From, &query.To, &query.WindowSeconds)
	if errWindow != nil {
		return query, errWindow
	}
	query.Timezone = timezone
	bucketSeconds, errBucket := parseUsagePositiveInt(c.Query("bucket_seconds"), query.BucketSeconds, "bucket_seconds")
	if errBucket != nil {
		return query, errBucket
	}
	if bucketSeconds > query.WindowSeconds {
		bucketSeconds = query.WindowSeconds
	}
	query.BucketSeconds = bucketSeconds
	groupBy := strings.TrimSpace(c.Query("group_by"))
	if groupBy != "" {
		switch groupBy {
		case "model", "provider", "client_key", "credential":
			query.GroupBy = groupBy
		default:
			return query, newUsageHTTPError("invalid_group_by", "unsupported realtime group_by %q", groupBy)
		}
	}
	query.Provider = strings.TrimSpace(c.Query("provider"))
	query.Model = strings.TrimSpace(c.Query("model"))
	query.HomeIP = firstNonEmptyQuery(c, "home_ip", "home-ip")
	query.Endpoint = strings.TrimSpace(c.Query("endpoint"))
	credentialType, errCredentialType := parseUsageCredentialType(c.Query("credential_type"))
	if errCredentialType != nil {
		return query, errCredentialType
	}
	query.CredentialType = credentialType
	return query, nil
}

func parseUsageHealthHTTPQuery(c *gin.Context) (usageHealthHTTPQuery, *usageHTTPError) {
	query := usageHealthHTTPQuery{WindowSeconds: 300}
	if c == nil {
		return query, newUsageHTTPError("invalid_request", "request is unavailable")
	}
	timezone, errWindow := parseUsageWindowQuery(c, query.WindowSeconds, &query.From, &query.To, &query.WindowSeconds)
	if errWindow != nil {
		return query, errWindow
	}
	query.Timezone = timezone
	query.Provider = strings.TrimSpace(c.Query("provider"))
	query.Model = strings.TrimSpace(c.Query("model"))
	query.HomeIP = firstNonEmptyQuery(c, "home_ip", "home-ip")
	query.Endpoint = strings.TrimSpace(c.Query("endpoint"))
	credentialType, errCredentialType := parseUsageCredentialType(c.Query("credential_type"))
	if errCredentialType != nil {
		return query, errCredentialType
	}
	query.CredentialType = credentialType
	return query, nil
}

func parseUsageWindowQuery(c *gin.Context, defaultWindow int, from **time.Time, to **time.Time, windowSeconds *int) (string, *usageHTTPError) {
	location, timezone, errTimezone := parseUsageTimezone(c.Query("timezone"))
	if errTimezone != nil {
		return "", errTimezone
	}
	parsedWindow, errWindow := parseUsagePositiveInt(c.Query("window_seconds"), defaultWindow, "window_seconds")
	if errWindow != nil {
		return "", errWindow
	}
	*windowSeconds = parsedWindow
	parsedFrom, parsedTo, errTimeRange := parseUsageTimeRange(c.Query("from"), c.Query("to"), location)
	if errTimeRange != nil {
		return "", errTimeRange
	}
	now := time.Now().UTC()
	if parsedTo == nil {
		parsedTo = &now
	}
	if parsedFrom == nil {
		start := parsedTo.Add(-time.Duration(parsedWindow) * time.Second)
		parsedFrom = &start
	}
	*from = parsedFrom
	*to = parsedTo
	return timezone, nil
}

func applyDefaultUsageSummaryWindow(from **time.Time, to **time.Time) {
	if from == nil || to == nil {
		return
	}
	if *from != nil && *to != nil {
		return
	}
	if *to == nil {
		now := time.Now().UTC()
		*to = &now
	}
	if *from == nil {
		start := (*to).Add(-time.Duration(usageSummaryDefaultWindowSeconds) * time.Second)
		*from = &start
	}
}

func parseUsagePositiveInt(raw string, defaultValue int, field string) (int, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultValue, nil
	}
	parsed, errParse := strconv.Atoi(value)
	if errParse != nil || parsed <= 0 {
		return 0, newUsageHTTPError("invalid_range", "%s must be a positive integer", field)
	}
	return parsed, nil
}

func respondUsageHTTPError(c *gin.Context, errValue *usageHTTPError) {
	if errValue == nil {
		return
	}
	respondError(c, http.StatusBadRequest, errValue.Code, errValue)
}

func newUsageHTTPError(code string, format string, args ...any) *usageHTTPError {
	message := strings.TrimSpace(format)
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	return &usageHTTPError{Code: strings.TrimSpace(code), Message: message}
}

func parseUsageLimit(raw string, defaultLimit int, maxLimit int) (int, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultLimit, nil
	}
	limit, errAtoi := strconv.Atoi(value)
	if errAtoi != nil || limit <= 0 {
		return 0, newUsageHTTPError("invalid_limit", "limit must be a positive integer")
	}
	if limit > maxLimit {
		return maxLimit, nil
	}
	return limit, nil
}

func parseUsageOffset(raw string) (int, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	offset, errAtoi := strconv.Atoi(value)
	if errAtoi != nil || offset < 0 {
		return 0, newUsageHTTPError("invalid_offset", "offset must be a non-negative integer")
	}
	return offset, nil
}

func parseUsageSort(raw string) (string, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "timestamp_desc", nil
	}
	for _, allowed := range []string{"timestamp_desc", "timestamp_asc", "tokens_desc", "tokens_asc", "cost_desc", "cost_asc", "latency_desc", "latency_asc", "failed_first"} {
		if value == allowed {
			return value, nil
		}
	}
	return "", newUsageHTTPError("invalid_sort", "unsupported usage sort %q", value)
}

func parseUsageGroupBy(raw string) (string, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", newUsageHTTPError("invalid_group_by", "group_by is required")
	}
	for _, allowed := range []string{"user", "client_key", "credential", "provider", "model", "endpoint", "home_ip", "executor_type", "status_code"} {
		if value == allowed {
			return value, nil
		}
	}
	return "", newUsageHTTPError("invalid_group_by", "unsupported usage group_by %q", value)
}

func parseUsageMetric(raw string) (string, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "request_count", nil
	}
	for _, allowed := range usageAggregateSortableMetrics() {
		if value == allowed {
			return value, nil
		}
	}
	return "", newUsageHTTPError("invalid_metric", "unsupported usage metric %q", value)
}

func parseUsageDirection(raw string) (string, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "desc", nil
	}
	if value == "desc" || value == "asc" {
		return value, nil
	}
	return "", newUsageHTTPError("invalid_direction", "direction must be desc or asc")
}

func parseUsageInterval(raw string) (string, *usageHTTPError) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "auto", nil
	}
	for _, allowed := range []string{"minute", "hour", "day", "week", "auto"} {
		if value == allowed {
			return value, nil
		}
	}
	return "", newUsageHTTPError("invalid_interval", "unsupported usage interval %q", value)
}

func parseUsageCredentialType(raw string) (string, *usageHTTPError) {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "":
		return "", nil
	case "provider_api_key", "api_key", "apikey":
		return "provider_api_key", nil
	case "oauth", "file_auth", "vertex", "unknown":
		return value, nil
	default:
		return "", newUsageHTTPError("invalid_credential_type", "unsupported credential_type %q", strings.TrimSpace(raw))
	}
}

func parseUsageTimezone(raw string) (*time.Location, string, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.UTC, "UTC", nil
	}
	location, errLoad := time.LoadLocation(value)
	if errLoad != nil {
		return nil, "", newUsageHTTPError("invalid_timezone", "timezone is not supported")
	}
	return location, value, nil
}

func parseUsageTimeRange(rawFrom string, rawTo string, location *time.Location) (*time.Time, *time.Time, *usageHTTPError) {
	if location == nil {
		location = time.UTC
	}
	from, errFrom := optionalUsageTime(rawFrom, false, location)
	if errFrom != nil {
		return nil, nil, errFrom
	}
	to, errTo := optionalUsageTime(rawTo, true, location)
	if errTo != nil {
		return nil, nil, errTo
	}
	if from != nil && to != nil && from.After(*to) {
		return nil, nil, newUsageHTTPError("invalid_time_range", "from must not be after to")
	}
	return from, to, nil
}

func optionalUsageTime(raw string, exclusiveDayEnd bool, location *time.Location) (*time.Time, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if location == nil {
		location = time.UTC
	}
	if unix, errUnix := strconv.ParseInt(value, 10, 64); errUnix == nil {
		if unix < 0 {
			return nil, newUsageHTTPError("invalid_time_range", "timestamp must be non-negative")
		}
		parsed := time.Unix(unix, 0).UTC()
		return &parsed, nil
	}
	if parsedDay, errDay := time.ParseInLocation("2006-01-02", value, location); errDay == nil {
		parsed := parsedDay
		if exclusiveDayEnd {
			parsed = parsed.AddDate(0, 0, 1)
		}
		parsed = parsed.UTC()
		return &parsed, nil
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil {
		return nil, newUsageHTTPError("invalid_time_range", "timestamp must be YYYY-MM-DD, Unix seconds, or RFC3339")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func optionalPositiveInt(raw string, field string) (*int, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	parsed, errParse := strconv.Atoi(value)
	if errParse != nil || parsed <= 0 {
		return nil, newUsageHTTPError("invalid_range", "%s must be a positive integer", field)
	}
	return &parsed, nil
}

func optionalNonNegativeInt64(raw string, field string) (*int64, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	parsed, errParse := strconv.ParseInt(value, 10, 64)
	if errParse != nil || parsed < 0 {
		return nil, newUsageHTTPError("invalid_range", "%s must be a non-negative integer", field)
	}
	return &parsed, nil
}

func optionalNonNegativeFloat64(raw string, field string) (*float64, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	parsed, errParse := strconv.ParseFloat(value, 64)
	if errParse != nil || parsed < 0 {
		return nil, newUsageHTTPError("invalid_range", "%s must be a non-negative number", field)
	}
	return &parsed, nil
}

func optionalUint(raw string, field string) (*uint, *usageHTTPError) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	parsed, errParse := strconv.ParseUint(value, 10, 64)
	if errParse != nil {
		return nil, newUsageHTTPError("invalid_range", "%s must be a non-negative integer", field)
	}
	out := uint(parsed)
	return &out, nil
}

func usageRecordSortableFields() []string {
	return []string{"timestamp", "total_tokens", "total_amount", "latency_ms"}
}

func usageAggregateSortableMetrics() []string {
	return []string{"request_count", "total_tokens", "total_amount", "failed_count", "avg_latency_ms", "p95_latency_ms"}
}

func parseUsageBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func usageObservabilityRecordQueryFromHTTP(query usageRecordHTTPQuery) cluster.UsageObservabilityRecordQuery {
	return cluster.UsageObservabilityRecordQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
		Status:         query.Status,
		StatusCode:     query.StatusCode,
		RequestID:      query.RequestID,
		User:           query.User,
		UserID:         query.UserID,
		ClientKey:      query.ClientKey,
		ClientKeyID:    query.ClientKeyID,
		CredentialID:   query.CredentialID,
		AuthIndex:      query.AuthIndex,
		ExecutorType:   query.ExecutorType,
		EventType:      query.EventType,
		CPANode:        query.CPANode,
		MinLatencyMS:   query.MinLatencyMS,
		MaxLatencyMS:   query.MaxLatencyMS,
		MinAmount:      query.MinAmount,
		MaxAmount:      query.MaxAmount,
		Search:         query.Search,
		Limit:          query.Limit,
		Offset:         query.Offset,
		Sort:           query.Sort,
	}
}

func usageObservabilityAggregateQueryFromHTTP(query usageAggregateHTTPQuery) cluster.UsageObservabilityAggregateQuery {
	return cluster.UsageObservabilityAggregateQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
		GroupBy:        query.GroupBy,
		Metric:         query.Metric,
		Direction:      query.Direction,
		Limit:          query.Limit,
		Offset:         query.Offset,
	}
}

func usageObservabilityOverviewQueryFromHTTP(query usageOverviewHTTPQuery) cluster.UsageObservabilityOverviewQuery {
	return cluster.UsageObservabilityOverviewQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
		Timezone:       query.Timezone,
		Interval:       query.Interval,
	}
}

func usageOverviewResponse(overview cluster.UsageObservabilityOverview) gin.H {
	return gin.H{
		"range": gin.H{
			"from":     overview.Range.From,
			"to":       overview.Range.To,
			"timezone": overview.Range.Timezone,
			"interval": overview.Range.Interval,
		},
		"live": gin.H{
			"window_seconds": overview.Live.WindowSeconds,
			"rpm":            overview.Live.RPM,
			"tpm":            overview.Live.TPM,
			"error_rate":     overview.Live.ErrorRate,
			"success_rate":   overview.Live.SuccessRate,
			"p50_latency_ms": optionalFloat64Value(overview.Live.P50LatencyMS),
			"p95_latency_ms": optionalFloat64Value(overview.Live.P95LatencyMS),
		},
		"totals":           usageOverviewTotalsResponse(overview.Totals),
		"trend":            usageTrendResponse(overview.Trend),
		"cost_breakdown":   usageCostBreakdownResponse(overview.CostBreakdown),
		"model_efficiency": usageAggregateItemsResponse(overview.ModelEfficiency),
		"top":              usageTopGroupsResponse(overview.Top),
		"activity":         usageActivityResponse(overview.Activity),
	}
}

func usageOverviewTotalsResponse(totals cluster.UsageObservabilityTotals) gin.H {
	return gin.H{
		"request_count":              totals.RequestCount,
		"success_count":              totals.SuccessCount,
		"failed_count":               totals.FailedCount,
		"error_rate":                 totals.ErrorRate,
		"success_rate":               totals.SuccessRate,
		"input_tokens":               totals.InputTokens,
		"output_tokens":              totals.OutputTokens,
		"reasoning_tokens":           totals.ReasoningTokens,
		"cached_tokens":              totals.CachedTokens,
		"cache_read_tokens":          totals.CacheReadTokens,
		"cache_creation_tokens":      totals.CacheCreationTokens,
		"total_tokens":               totals.TotalTokens,
		"token_breakdown":            usageTokenBreakdownResponse(totals.TokenBreakdown),
		"total_amount":               optionalFloat64Value(totals.TotalAmount),
		"currency":                   emptyStringAsNil(totals.Currency),
		"blended_cost_per_1m_tokens": optionalFloat64Value(totals.BlendedCostPer1M),
		"avg_latency_ms":             optionalFloat64Value(totals.AvgLatencyMS),
		"p50_latency_ms":             optionalFloat64Value(totals.P50LatencyMS),
		"p95_latency_ms":             optionalFloat64Value(totals.P95LatencyMS),
		"avg_ttft_ms":                optionalFloat64Value(totals.AvgTTFTMS),
		"active_user_count":          totals.ActiveUserCount,
		"active_client_key_count":    totals.ActiveClientKeyCount,
		"active_credential_count":    totals.ActiveCredentialCount,
		"active_model_count":         totals.ActiveModelCount,
	}
}

func usageTrendResponse(points []cluster.UsageObservabilityTrendPoint) []gin.H {
	out := make([]gin.H, 0, len(points))
	for _, point := range points {
		out = append(out, gin.H{
			"bucket_start":          point.BucketStart.UTC().Format(time.RFC3339Nano),
			"bucket_end":            point.BucketEnd.UTC().Format(time.RFC3339Nano),
			"request_count":         point.RequestCount,
			"success_count":         point.SuccessCount,
			"failed_count":          point.FailedCount,
			"input_tokens":          point.InputTokens,
			"output_tokens":         point.OutputTokens,
			"reasoning_tokens":      point.ReasoningTokens,
			"cached_tokens":         point.CachedTokens,
			"cache_read_tokens":     point.CacheReadTokens,
			"cache_creation_tokens": point.CacheCreationTokens,
			"total_tokens":          point.TotalTokens,
			"token_breakdown":       usageTokenBreakdownResponse(point.TokenBreakdown),
			"total_amount":          optionalFloat64Value(point.TotalAmount),
			"avg_latency_ms":        optionalFloat64Value(point.AvgLatencyMS),
			"p95_latency_ms":        optionalFloat64Value(point.P95LatencyMS),
		})
	}
	return out
}

func usageCostBreakdownResponse(items []cluster.UsageObservabilityCostBreakdownItem) []gin.H {
	out := make([]gin.H, 0, len(items))
	for _, item := range items {
		out = append(out, gin.H{
			"category":      item.Category,
			"amount":        item.Amount,
			"percentage":    item.Percentage,
			"tokens":        item.Tokens,
			"billing_basis": item.BillingBasis,
		})
	}
	return out
}

func usageTopGroupsResponse(top cluster.UsageObservabilityTopGroups) gin.H {
	return gin.H{
		"users":       usageAggregateItemsResponse(top.Users),
		"client_keys": usageAggregateItemsResponse(top.ClientKeys),
		"credentials": usageAggregateItemsResponse(top.Credentials),
		"providers":   usageAggregateItemsResponse(top.Providers),
		"models":      usageAggregateItemsResponse(top.Models),
		"endpoints":   usageAggregateItemsResponse(top.Endpoints),
		"errors":      usageAggregateItemsResponse(top.Errors),
	}
}

func usageActivityResponse(points []cluster.UsageObservabilityActivityPoint) []gin.H {
	out := make([]gin.H, 0, len(points))
	for _, point := range points {
		out = append(out, gin.H{
			"bucket_start":  point.BucketStart.UTC().Format(time.RFC3339Nano),
			"bucket_end":    point.BucketEnd.UTC().Format(time.RFC3339Nano),
			"request_count": point.RequestCount,
			"success_count": point.SuccessCount,
			"failed_count":  point.FailedCount,
			"success_rate":  point.SuccessRate,
			"error_rate":    point.ErrorRate,
			"status":        point.Status,
		})
	}
	return out
}

func usageAggregateItemsResponse(items []cluster.UsageObservabilityAggregateItem) []gin.H {
	out := make([]gin.H, 0, len(items))
	for index := range items {
		out = append(out, usageAggregateItemResponse(&items[index]))
	}
	return out
}

func usageRealtimeVelocityPointResponse(points []cluster.UsageObservabilityRealtimeVelocityPoint) []gin.H {
	out := make([]gin.H, 0, len(points))
	for index := range points {
		out = append(out, gin.H{
			"bucket_start": points[index].BucketStart.UTC().Format(time.RFC3339Nano),
			"bucket_end":   points[index].BucketEnd.UTC().Format(time.RFC3339Nano),
			"rpm":          points[index].RPM,
			"tpm":          points[index].TPM,
			"error_rate":   points[index].ErrorRate,
		})
	}
	return out
}

func usageLatencyDistributionBucketResponse(buckets []cluster.UsageObservabilityLatencyDistributionBucket) []gin.H {
	out := make([]gin.H, 0, len(buckets))
	for index := range buckets {
		out = append(out, gin.H{"bucket": buckets[index].Bucket, "request_count": buckets[index].RequestCount})
	}
	return out
}

func usageHealthItemResponse(item *cluster.UsageObservabilityAggregateItem, subject string, details map[string]cluster.UsageObservabilityHealthDetail) gin.H {
	if item == nil {
		return gin.H{}
	}
	metadata := item.Metadata
	provider := ""
	credentialType := any(nil)
	if value, ok := metadata["provider"].(string); ok {
		provider = value
	}
	if subject == "provider" && strings.TrimSpace(provider) == "" {
		provider = item.ID
	}
	if subject == "credential" {
		credentialType = metadata["credential_type"]
	}
	status := usageHealthStatus(item.ErrorRate, item.RequestCount)
	if subject == "credential" {
		if metadataStatus, ok := metadata["status"].(string); ok {
			switch strings.TrimSpace(metadataStatus) {
			case "disabled", "unavailable":
				status = strings.TrimSpace(metadataStatus)
			}
		}
	}
	var lastErrorAt any
	var lastErrorStatus any
	var lastErrorMessage any
	if detail, ok := details[item.ID]; ok {
		if detail.LastErrorAt != nil {
			lastErrorAt = detail.LastErrorAt.UTC().Format(time.RFC3339Nano)
		}
		if detail.LastErrorStatus > 0 {
			lastErrorStatus = detail.LastErrorStatus
		}
		lastErrorMessage = emptyStringAsNil(detail.LastErrorMessage)
	}
	nextRetryAt := any(nil)
	if value, ok := metadata["next_retry_at"].(string); ok {
		nextRetryAt = emptyStringAsNil(value)
	}
	if nextRetryAt == nil {
		if detail, ok := details[item.ID]; ok && detail.NextRetryAt != nil {
			nextRetryAt = detail.NextRetryAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return gin.H{
		"id":                   item.ID,
		"label":                item.Label,
		"status":               status,
		"provider":             emptyStringAsNil(provider),
		"credential_type":      credentialType,
		"recent_success_count": item.SuccessCount,
		"recent_failed_count":  item.FailedCount,
		"recent_error_rate":    item.ErrorRate,
		"last_error_at":        lastErrorAt,
		"last_error_status":    lastErrorStatus,
		"last_error_message":   lastErrorMessage,
		"next_retry_at":        nextRetryAt,
		"avg_latency_ms":       optionalFloat64Value(item.AvgLatencyMS),
		"p95_latency_ms":       optionalFloat64Value(item.P95LatencyMS),
	}
}

func usageHealthStatus(errorRate float64, requestCount int64) string {
	if requestCount == 0 {
		return "unknown"
	}
	if errorRate >= 0.50 {
		return "unavailable"
	}
	if errorRate >= 0.05 {
		return "degraded"
	}
	return "healthy"
}

func (h *Handler) requestLogIndexItemResponse(record *cluster.UsageObservabilityRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	requestID := strings.TrimSpace(record.RequestID)
	homeIP := strings.TrimSpace(record.Runtime.HomeIP)
	homePort := record.Runtime.HomePort
	available, fileName, sizeBytes := h.usageRequestLogAvailability(homeIP, homePort, requestID)
	var downloadURL any
	if available {
		value := "/request-log-by-id/" + url.PathEscape(requestID)
		query := url.Values{}
		if homeIP != "" {
			query.Set("home_ip", homeIP)
		}
		if homePort > 0 {
			query.Set("home_port", strconv.Itoa(homePort))
		}
		if len(query) > 0 {
			value += "?" + query.Encode()
		}
		downloadURL = value
	}
	return gin.H{
		"id":           record.ID,
		"request_id":   requestID,
		"timestamp":    record.Timestamp.UTC().Format(time.RFC3339Nano),
		"home_ip":      emptyStringAsNil(homeIP),
		"home_port":    optionalPositiveIntValue(homePort),
		"file_name":    fileName,
		"size_bytes":   sizeBytes,
		"available":    available,
		"provider":     emptyStringAsNil(record.Provider),
		"model":        emptyStringAsNil(record.Model),
		"status":       record.Status,
		"download_url": downloadURL,
	}
}

func requestLogIndexItemMatchesSearch(item gin.H, search string) bool {
	needle := strings.ToLower(strings.TrimSpace(search))
	if needle == "" {
		return true
	}
	for _, key := range []string{"request_id", "file_name", "model", "provider", "status"} {
		value := strings.ToLower(strings.TrimSpace(requestLogIndexStringValue(item[key])))
		if value != "" && strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func requestLogSearchNeedsFileScan(search string) bool {
	value := strings.TrimSpace(search)
	if value == "" {
		return false
	}
	if strings.Contains(strings.ToLower(value), ".log") {
		return true
	}
	if len(value) < 8 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func requestLogIndexStringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func (h *Handler) usageRequestLogAvailability(homeIP string, homePort int, requestID string) (bool, any, any) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false, nil, nil
	}
	if h.requestLogTargetIsRemote(homeIP, homePort) {
		return h.remoteRequestLogDownloadAvailable(homeIP), nil, nil
	}
	name, path, errFind := findRequestLogFile(homeLogDirectory, requestID)
	if errFind != nil {
		return false, nil, nil
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		return false, nil, nil
	}
	return true, name, info.Size()
}

func (h *Handler) remoteRequestLogDownloadAvailable(homeIP string) bool {
	return h != nil && h.repo != nil && h.forwardTLSConfig != nil && strings.TrimSpace(homeIP) != ""
}

func (h *Handler) usageRequestLogExcerpt(record *cluster.UsageObservabilityRecord) []string {
	if record == nil {
		return []string{}
	}
	if h.requestLogTargetIsRemote(record.Runtime.HomeIP, record.Runtime.HomePort) {
		return []string{}
	}
	_, path, errFind := findRequestLogFile(homeLogDirectory, record.RequestID)
	if errFind != nil {
		return []string{}
	}
	content, errRead := os.ReadFile(path)
	if errRead != nil {
		return []string{}
	}
	rawLines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	lines := make([]string, 0, 20)
	for _, rawLine := range rawLines {
		if len(lines) >= 20 {
			break
		}
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, redactUsageRequestLogLine(line))
	}
	return lines
}

func redactUsageRequestLogLine(line string) string {
	normalized := strings.ToLower(line)
	for _, marker := range []string{"authorization", "access_token", "refresh_token", "api_key", "cookie", "set-cookie", "bearer "} {
		if strings.Contains(normalized, marker) {
			return "[redacted]"
		}
	}
	return line
}

func (h *Handler) usageRecordSummaryResponse(record *cluster.UsageObservabilityRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	recordCopy := *record
	available, _, _ := h.usageRequestLogAvailability(recordCopy.Runtime.HomeIP, recordCopy.Runtime.HomePort, recordCopy.RequestID)
	recordCopy.Runtime.RequestLogAvailable = available
	return usageRecordSummaryResponse(&recordCopy)
}

func usageRecordSummaryResponse(record *cluster.UsageObservabilityRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	var errorValue any
	if record.Error != nil {
		errorValue = gin.H{
			"status_code":  record.Error.StatusCode,
			"message":      record.Error.Message,
			"body_preview": record.Error.BodyPreview,
		}
	}
	return gin.H{
		"id":                   record.ID,
		"usage_id":             record.UsageID,
		"timestamp":            record.Timestamp.UTC().Format(time.RFC3339Nano),
		"request_id":           record.RequestID,
		"upstream_request_id":  emptyStringAsNil(record.UpstreamRequestID),
		"event_type":           emptyStringAsNil(record.EventType),
		"status":               record.Status,
		"failed":               record.Failed,
		"status_code":          record.StatusCode,
		"upstream_status_code": optionalPositiveIntValue(record.UpstreamStatusCode),
		"source":               emptyStringAsNil(record.Source),
		"provider":             record.Provider,
		"model":                record.Model,
		"original_model":       record.OriginalModel,
		"endpoint":             record.Endpoint,
		"service_tier":         emptyStringAsNil(record.ServiceTier),
		"reasoning_effort":     emptyStringAsNil(record.ReasoningEffort),
		"executor_type":        record.ExecutorType,
		"tokens": gin.H{
			"input_tokens":          record.Tokens.InputTokens,
			"output_tokens":         record.Tokens.OutputTokens,
			"reasoning_tokens":      record.Tokens.ReasoningTokens,
			"cached_tokens":         record.Tokens.CachedTokens,
			"cache_read_tokens":     record.Tokens.CacheReadTokens,
			"cache_creation_tokens": record.Tokens.CacheCreationTokens,
			"total_tokens":          record.Tokens.TotalTokens,
		},
		"token_breakdown": usageTokenBreakdownResponse(record.Tokens.TokenBreakdown),
		"performance": gin.H{
			"latency_ms": record.Performance.LatencyMS,
			"ttft_ms":    optionalInt64Value(record.Performance.TTFTMS),
			"tps":        optionalFloat64Value(record.Performance.TPS),
		},
		"client": gin.H{
			"api_key_id":     optionalUintValue(record.Client.APIKeyID),
			"api_key_label":  record.Client.APIKeyLabel,
			"api_key_masked": record.Client.APIKeyMasked,
			"user_id":        optionalUintValue(record.Client.UserID),
			"username":       record.Client.Username,
			"client_ip":      emptyStringAsNil(record.Client.ClientIP),
		},
		"credential": gin.H{
			"credential_type": record.Credential.CredentialType,
			"credential_id":   record.Credential.CredentialID,
			"auth_index":      record.Credential.AuthIndex,
			"provider":        record.Credential.Provider,
			"label":           record.Credential.Label,
			"source":          record.Credential.Source,
			"status":          record.Credential.Status,
			"api_key_preview": emptyStringAsNil(record.Credential.APIKeyPreview),
		},
		"billing": gin.H{
			"charge_id":          emptyStringAsNil(record.Billing.ChargeID),
			"amount":             optionalFloat64Value(record.Billing.Amount),
			"currency":           emptyStringAsNil(record.Billing.Currency),
			"billing_basis":      record.Billing.BillingBasis,
			"matched_price_rule": emptyStringAsNil(record.Billing.MatchedPriceRule),
			"balance_before":     optionalFloat64Value(record.Billing.BalanceBefore),
			"balance_after":      optionalFloat64Value(record.Billing.BalanceAfter),
		},
		"runtime": gin.H{
			"home_ip":               record.Runtime.HomeIP,
			"home_port":             optionalPositiveIntValue(record.Runtime.HomePort),
			"cpa_node_id":           emptyStringAsNil(record.Runtime.CPANodeID),
			"cpa_ip":                emptyStringAsNil(record.Runtime.CPAIP),
			"cpa_port":              optionalPositiveIntValue(record.Runtime.CPAPort),
			"cpa_label":             emptyStringAsNil(record.Runtime.CPALabel),
			"request_log_available": record.Runtime.RequestLogAvailable,
			"log_home_ip_required":  record.Runtime.LogHomeIPRequired,
		},
		"error": errorValue,
	}
}

func usageAggregateItemResponse(item *cluster.UsageObservabilityAggregateItem) gin.H {
	if item == nil {
		return gin.H{}
	}
	return gin.H{
		"id":                    item.ID,
		"label":                 item.Label,
		"metadata":              item.Metadata,
		"request_count":         item.RequestCount,
		"success_count":         item.SuccessCount,
		"failed_count":          item.FailedCount,
		"success_rate":          item.SuccessRate,
		"error_rate":            item.ErrorRate,
		"input_tokens":          item.InputTokens,
		"output_tokens":         item.OutputTokens,
		"reasoning_tokens":      item.ReasoningTokens,
		"cached_tokens":         item.CachedTokens,
		"cache_read_tokens":     item.CacheReadTokens,
		"cache_creation_tokens": item.CacheCreationTokens,
		"cache_rate":            item.CacheRate,
		"total_tokens":          item.TotalTokens,
		"token_breakdown":       usageTokenBreakdownResponse(item.TokenBreakdown),
		"total_amount":          optionalFloat64Value(item.TotalAmount),
		"currency":              emptyStringAsNil(item.Currency),
		"avg_latency_ms":        optionalFloat64Value(item.AvgLatencyMS),
		"p95_latency_ms":        optionalFloat64Value(item.P95LatencyMS),
		"last_used_at":          optionalTimeValue(item.LastUsedAt),
		"comparison":            nil,
		"quota_windows":         []any{},
	}
}

func usageTokenBreakdownResponse(breakdown cluster.UsageTokenBreakdown) gin.H {
	return gin.H{
		"schema_version": breakdown.SchemaVersion,
		"quality":        breakdown.Quality,
		"total_tokens":   breakdown.TotalTokens,
		"input": gin.H{
			"total_tokens":       breakdown.Input.TotalTokens,
			"uncached_tokens":    breakdown.Input.UncachedTokens,
			"cache_read_tokens":  breakdown.Input.CacheReadTokens,
			"cache_write_tokens": breakdown.Input.CacheWriteTokens,
		},
		"output": gin.H{
			"total_tokens":         breakdown.Output.TotalTokens,
			"non_reasoning_tokens": breakdown.Output.NonReasoningTokens,
			"reasoning_tokens":     breakdown.Output.ReasoningTokens,
		},
		"unclassified_tokens": breakdown.UnclassifiedTokens,
	}
}

func usagePayloadSummaryResponse(summary *cluster.UsageObservabilityPayloadSummary) gin.H {
	if summary == nil {
		return gin.H{}
	}
	return gin.H{
		"method":        optionalStringValue(summary.Method),
		"stream":        optionalBoolValue(summary.Stream),
		"message_count": optionalIntValue(summary.MessageCount),
		"tool_count":    optionalIntValue(summary.ToolCount),
	}
}

func (h *Handler) usageExportRecordMap(record *cluster.UsageObservabilityRecord) map[string]any {
	if record == nil {
		return map[string]any{}
	}
	recordCopy := *record
	available, _, _ := h.usageRequestLogAvailability(recordCopy.Runtime.HomeIP, recordCopy.Runtime.HomePort, recordCopy.RequestID)
	recordCopy.Runtime.RequestLogAvailable = available
	return usageExportRecordMap(&recordCopy)
}

func usageExportRecordMap(record *cluster.UsageObservabilityRecord) map[string]any {
	if record == nil {
		return map[string]any{}
	}
	var errorStatusCode any
	var errorMessage any
	var errorBodyPreview any
	if record.Error != nil {
		if record.Error.StatusCode > 0 {
			errorStatusCode = record.Error.StatusCode
		}
		errorMessage = emptyStringAsNil(record.Error.Message)
		errorBodyPreview = emptyStringAsNil(record.Error.BodyPreview)
	}
	tokenBreakdown := record.Tokens.TokenBreakdown
	return map[string]any{
		"id":                                    record.ID,
		"usage_id":                              record.UsageID,
		"timestamp":                             record.Timestamp.UTC().Format(time.RFC3339Nano),
		"request_id":                            record.RequestID,
		"upstream_request_id":                   emptyStringAsNil(record.UpstreamRequestID),
		"event_type":                            emptyStringAsNil(record.EventType),
		"status":                                record.Status,
		"failed":                                record.Failed,
		"status_code":                           record.StatusCode,
		"upstream_status_code":                  optionalPositiveIntValue(record.UpstreamStatusCode),
		"source":                                emptyStringAsNil(record.Source),
		"provider":                              record.Provider,
		"model":                                 record.Model,
		"original_model":                        record.OriginalModel,
		"endpoint":                              record.Endpoint,
		"service_tier":                          emptyStringAsNil(record.ServiceTier),
		"reasoning_effort":                      emptyStringAsNil(record.ReasoningEffort),
		"executor_type":                         record.ExecutorType,
		"input_tokens":                          record.Tokens.InputTokens,
		"output_tokens":                         record.Tokens.OutputTokens,
		"reasoning_tokens":                      record.Tokens.ReasoningTokens,
		"cached_tokens":                         record.Tokens.CachedTokens,
		"cache_read_tokens":                     record.Tokens.CacheReadTokens,
		"cache_creation_tokens":                 record.Tokens.CacheCreationTokens,
		"total_tokens":                          record.Tokens.TotalTokens,
		"token_breakdown_schema_version":        tokenBreakdown.SchemaVersion,
		"token_breakdown_quality":               tokenBreakdown.Quality,
		"token_breakdown_total_tokens":          tokenBreakdown.TotalTokens,
		"token_breakdown_input_total_tokens":    tokenBreakdown.Input.TotalTokens,
		"token_breakdown_input_uncached_tokens": tokenBreakdown.Input.UncachedTokens,
		"token_breakdown_input_cache_read_tokens":     tokenBreakdown.Input.CacheReadTokens,
		"token_breakdown_input_cache_write_tokens":    tokenBreakdown.Input.CacheWriteTokens,
		"token_breakdown_output_total_tokens":         tokenBreakdown.Output.TotalTokens,
		"token_breakdown_output_non_reasoning_tokens": tokenBreakdown.Output.NonReasoningTokens,
		"token_breakdown_output_reasoning_tokens":     tokenBreakdown.Output.ReasoningTokens,
		"token_breakdown_unclassified_tokens":         tokenBreakdown.UnclassifiedTokens,
		"latency_ms":                                  record.Performance.LatencyMS,
		"ttft_ms":                                     optionalInt64Value(record.Performance.TTFTMS),
		"tps":                                         optionalFloat64Value(record.Performance.TPS),
		"api_key_id":                                  optionalUintValue(record.Client.APIKeyID),
		"api_key_label":                               record.Client.APIKeyLabel,
		"api_key_masked":                              record.Client.APIKeyMasked,
		"user_id":                                     optionalUintValue(record.Client.UserID),
		"username":                                    record.Client.Username,
		"client_ip":                                   emptyStringAsNil(record.Client.ClientIP),
		"credential_id":                               record.Credential.CredentialID,
		"credential_type":                             record.Credential.CredentialType,
		"auth_index":                                  record.Credential.AuthIndex,
		"credential_provider":                         record.Credential.Provider,
		"credential_label":                            record.Credential.Label,
		"credential_source":                           record.Credential.Source,
		"credential_status":                           record.Credential.Status,
		"credential_api_key_preview":                  emptyStringAsNil(record.Credential.APIKeyPreview),
		"charge_id":                                   emptyStringAsNil(record.Billing.ChargeID),
		"amount":                                      optionalFloat64Value(record.Billing.Amount),
		"currency":                                    emptyStringAsNil(record.Billing.Currency),
		"billing_basis":                               record.Billing.BillingBasis,
		"matched_price_rule":                          emptyStringAsNil(record.Billing.MatchedPriceRule),
		"balance_before":                              optionalFloat64Value(record.Billing.BalanceBefore),
		"balance_after":                               optionalFloat64Value(record.Billing.BalanceAfter),
		"home_ip":                                     record.Runtime.HomeIP,
		"home_port":                                   optionalPositiveIntValue(record.Runtime.HomePort),
		"cpa_node_id":                                 emptyStringAsNil(record.Runtime.CPANodeID),
		"cpa_ip":                                      emptyStringAsNil(record.Runtime.CPAIP),
		"cpa_port":                                    optionalPositiveIntValue(record.Runtime.CPAPort),
		"cpa_label":                                   emptyStringAsNil(record.Runtime.CPALabel),
		"request_log_available":                       record.Runtime.RequestLogAvailable,
		"log_home_ip_required":                        record.Runtime.LogHomeIPRequired,
		"error_status_code":                           errorStatusCode,
		"error_message":                               errorMessage,
		"error_body_preview":                          errorBodyPreview,
	}
}

func usageExportCSVHeader() []string {
	return []string{
		"id", "usage_id", "timestamp", "request_id", "upstream_request_id", "event_type", "status", "failed", "status_code", "upstream_status_code",
		"source", "provider", "model", "original_model", "endpoint", "service_tier", "reasoning_effort", "executor_type",
		"input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens", "total_tokens",
		"token_breakdown_schema_version", "token_breakdown_quality", "token_breakdown_total_tokens",
		"token_breakdown_input_total_tokens", "token_breakdown_input_uncached_tokens", "token_breakdown_input_cache_read_tokens", "token_breakdown_input_cache_write_tokens",
		"token_breakdown_output_total_tokens", "token_breakdown_output_non_reasoning_tokens", "token_breakdown_output_reasoning_tokens", "token_breakdown_unclassified_tokens",
		"latency_ms", "ttft_ms", "tps", "api_key_id", "api_key_label", "api_key_masked", "user_id", "username", "client_ip",
		"credential_id", "credential_type", "auth_index", "credential_provider", "credential_label", "credential_source", "credential_status", "credential_api_key_preview",
		"charge_id", "amount", "currency", "billing_basis", "matched_price_rule", "balance_before", "balance_after",
		"home_ip", "home_port", "cpa_node_id", "cpa_ip", "cpa_port", "cpa_label", "request_log_available", "log_home_ip_required", "error_status_code", "error_message", "error_body_preview",
	}
}

func (h *Handler) usageExportCSVRow(record *cluster.UsageObservabilityRecord) []string {
	row := h.usageExportRecordMap(record)
	out := make([]string, 0, len(usageExportCSVHeader()))
	for _, key := range usageExportCSVHeader() {
		out = append(out, usageExportCSVValue(row[key]))
	}
	return out
}

func usageExportCSVValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func optionalUintValue(value *uint) any {
	if value == nil || *value == 0 {
		return nil
	}
	return *value
}

func optionalInt64Value(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalIntValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalPositiveIntValue(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func optionalStringValue(value *string) any {
	if value == nil {
		return nil
	}
	if strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

func optionalBoolValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalFloat64Value(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalTimeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func emptyStringAsNil(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func firstNonEmptyQueryValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
