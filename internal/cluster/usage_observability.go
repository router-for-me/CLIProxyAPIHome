package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	UsageObservabilityDefaultRecordLimit = 50
	UsageObservabilityMaxRecordLimit     = 200
	UsageObservabilityDefaultGroupLimit  = 20
	UsageObservabilityMaxGroupLimit      = 100

	UsageObservabilityCurrencyCredits     = "credits"
	UsageObservabilityBillingBasisCharge  = "billing_charge"
	UsageObservabilityBillingBasisUnknown = "unknown"
)

var usageObservabilitySecretPattern = regexp.MustCompile(`(?i)(authorization|access_token|refresh_token|api_key|client_secret|cookie|set-cookie|bearer|secret)(["']?\s*[:=]\s*["']?|\s+)[^"'\s,;\]}]+`)

type UsageObservabilityRecordQuery struct {
	From             *time.Time
	To               *time.Time
	Provider         string
	Model            string
	HomeIP           string
	Endpoint         string
	CredentialType   string
	Status           string
	StatusCode       *int
	RequestID        string
	User             string
	UserID           *uint
	ClientKey        string
	ClientKeyID      *uint
	CredentialID     string
	AuthIndex        string
	ExecutorType     string
	EventType        string
	CPANode          string
	MinLatencyMS     *int64
	MaxLatencyMS     *int64
	MinAmount        *float64
	MaxAmount        *float64
	Search           string
	RequestLogSearch string
	Limit            int
	Offset           int
	Sort             string
	MaxLimit         int
}

type UsageObservabilityRecordListResult struct {
	Records []UsageObservabilityRecord
	Total   int64
}

type UsageObservabilityFilterOptions struct {
	EventTypes  []string
	Providers   []string
	Models      []string
	HomeIPs     []string
	CPANodes    []string
	StatusCodes []int
}

type UsageObservabilityAggregateQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	GroupBy        string
	Metric         string
	Direction      string
	Limit          int
	Offset         int
}

type UsageObservabilityAggregateResult struct {
	Items []UsageObservabilityAggregateItem
	Total int64
}

type UsageObservabilityOverviewQuery struct {
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

type UsageObservabilityOverview struct {
	Range           UsageObservabilityOverviewRange
	Live            UsageObservabilityLiveSummary
	Totals          UsageObservabilityTotals
	Trend           []UsageObservabilityTrendPoint
	CostBreakdown   []UsageObservabilityCostBreakdownItem
	ModelEfficiency []UsageObservabilityAggregateItem
	Top             UsageObservabilityTopGroups
	Activity        []UsageObservabilityActivityPoint
}

type UsageObservabilityOverviewRange struct {
	From     string
	To       string
	Timezone string
	Interval string
}

type UsageObservabilityLiveSummary struct {
	WindowSeconds int
	RPM           float64
	TPM           float64
	ErrorRate     float64
	SuccessRate   float64
	P50LatencyMS  *float64
	P95LatencyMS  *float64
}

type UsageObservabilityTotals struct {
	RequestCount          int64
	SuccessCount          int64
	FailedCount           int64
	ErrorRate             float64
	SuccessRate           float64
	InputTokens           int64
	OutputTokens          int64
	ReasoningTokens       int64
	CachedTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	TotalTokens           int64
	TotalAmount           *float64
	Currency              string
	BlendedCostPer1M      *float64
	AvgLatencyMS          *float64
	P50LatencyMS          *float64
	P95LatencyMS          *float64
	AvgTTFTMS             *float64
	ActiveUserCount       int64
	ActiveClientKeyCount  int64
	ActiveCredentialCount int64
	ActiveModelCount      int64
}

type UsageObservabilityTrendPoint struct {
	BucketStart         time.Time
	BucketEnd           time.Time
	RequestCount        int64
	SuccessCount        int64
	FailedCount         int64
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
	TotalAmount         *float64
	AvgLatencyMS        *float64
	P95LatencyMS        *float64
}

type UsageObservabilityCostBreakdownItem struct {
	Category     string
	Amount       float64
	Percentage   float64
	Tokens       int64
	BillingBasis string
}

type UsageObservabilityTopGroups struct {
	Users       []UsageObservabilityAggregateItem
	ClientKeys  []UsageObservabilityAggregateItem
	Credentials []UsageObservabilityAggregateItem
	Providers   []UsageObservabilityAggregateItem
	Models      []UsageObservabilityAggregateItem
	Endpoints   []UsageObservabilityAggregateItem
	Errors      []UsageObservabilityAggregateItem
}

type UsageObservabilityActivityPoint struct {
	BucketStart  time.Time
	BucketEnd    time.Time
	RequestCount int64
	SuccessCount int64
	FailedCount  int64
	SuccessRate  float64
	ErrorRate    float64
	Status       string
}

type UsageObservabilityAggregateItem struct {
	ID                  string
	Label               string
	Metadata            map[string]any
	RequestCount        int64
	SuccessCount        int64
	FailedCount         int64
	SuccessRate         float64
	ErrorRate           float64
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	CacheRate           float64
	TotalTokens         int64
	TotalAmount         *float64
	Currency            string
	AvgLatencyMS        *float64
	P95LatencyMS        *float64
	LastUsedAt          *time.Time
}

type UsageObservabilityRecord struct {
	ID                 string
	UsageID            uint
	Timestamp          time.Time
	RequestID          string
	UpstreamRequestID  string
	EventType          string
	Status             string
	Failed             bool
	StatusCode         int
	UpstreamStatusCode int
	Source             string
	Provider           string
	Model              string
	OriginalModel      string
	Endpoint           string
	ServiceTier        string
	ReasoningEffort    string
	ExecutorType       string
	Tokens             UsageObservabilityTokens
	Performance        UsageObservabilityPerformance
	Client             UsageObservabilityClient
	Credential         UsageObservabilityCredential
	Billing            UsageObservabilityBilling
	Runtime            UsageObservabilityRuntime
	Error              *UsageObservabilityError
}

type UsageObservabilityTokens struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

type UsageObservabilityPerformance struct {
	LatencyMS int64
	TTFTMS    *int64
	TPS       *float64
}

type UsageObservabilityClient struct {
	APIKeyID     *uint
	APIKeyLabel  string
	APIKeyMasked string
	UserID       *uint
	Username     string
	ClientIP     string
}

type UsageObservabilityCredential struct {
	CredentialType string
	CredentialID   string
	AuthIndex      string
	Provider       string
	Label          string
	Source         string
	Status         string
	APIKeyPreview  string
	NextRetryAt    *time.Time
}

type UsageObservabilityBilling struct {
	ChargeID         string
	Amount           *float64
	Currency         string
	BillingBasis     string
	MatchedPriceRule string
	BalanceBefore    *float64
	BalanceAfter     *float64
}

type UsageObservabilityRuntime struct {
	HomeIP              string
	HomePort            int
	CPANodeID           string
	CPAIP               string
	CPAPort             int
	CPALabel            string
	RequestLogAvailable bool
	LogHomeIPRequired   bool
}

type UsageObservabilityError struct {
	StatusCode         int
	UpstreamStatusCode int
	Reason             string
	Message            string
	BodyPreview        string
}

type UsageObservabilityPayloadSummary struct {
	Method       *string
	Stream       *bool
	MessageCount *int
	ToolCount    *int
}

type usageObservabilityRecordRow struct {
	UsageID              uint            `gorm:"column:usage_id"`
	Timestamp            time.Time       `gorm:"column:timestamp"`
	LatencyMS            int64           `gorm:"column:latency_ms"`
	TTFTMS               int64           `gorm:"column:ttft_ms"`
	InputTokens          int64           `gorm:"column:input_tokens"`
	OutputTokens         int64           `gorm:"column:output_tokens"`
	ReasoningTokens      int64           `gorm:"column:reasoning_tokens"`
	CachedTokens         int64           `gorm:"column:cached_tokens"`
	CacheReadTokens      int64           `gorm:"column:cache_read_tokens"`
	CacheCreationTokens  int64           `gorm:"column:cache_creation_tokens"`
	TotalTokens          int64           `gorm:"column:total_tokens"`
	Failed               bool            `gorm:"column:failed"`
	FailStatusCode       int             `gorm:"column:fail_status_code"`
	FailBody             string          `gorm:"column:fail_body"`
	Source               string          `gorm:"column:source"`
	Provider             string          `gorm:"column:provider"`
	ExecutorType         string          `gorm:"column:executor_type"`
	Model                string          `gorm:"column:model"`
	Alias                string          `gorm:"column:alias"`
	ReasoningEffort      string          `gorm:"column:reasoning_effort"`
	ServiceTier          string          `gorm:"column:service_tier"`
	Endpoint             string          `gorm:"column:endpoint"`
	AuthType             string          `gorm:"column:auth_type"`
	RawAPIKey            string          `gorm:"column:raw_api_key"`
	RequestID            string          `gorm:"column:request_id"`
	UpstreamRequestID    string          `gorm:"column:upstream_request_id"`
	EventType            string          `gorm:"column:event_type"`
	UpstreamStatusCode   int             `gorm:"column:upstream_status_code"`
	HomeIP               string          `gorm:"column:home_ip"`
	HomePort             int             `gorm:"column:home_port"`
	CPANodeID            string          `gorm:"column:cpa_node_id"`
	CPAIP                string          `gorm:"column:cpa_ip"`
	CPAPort              int             `gorm:"column:cpa_port"`
	CPALabel             string          `gorm:"column:cpa_label"`
	PayloadJSON          JSONB           `gorm:"column:payload_json"`
	ClientAPIKeyID       *uint           `gorm:"column:client_api_key_id"`
	ClientAPIKeyLabel    string          `gorm:"column:client_api_key_label"`
	ClientAPIKeyMasked   string          `gorm:"column:client_api_key_masked"`
	ClientUserID         *uint           `gorm:"column:client_user_id"`
	Username             string          `gorm:"column:username"`
	ChargeID             string          `gorm:"column:charge_id"`
	Amount               sql.NullFloat64 `gorm:"column:amount"`
	MatchedPriceRule     string          `gorm:"column:matched_price_rule"`
	BalanceBefore        sql.NullFloat64 `gorm:"column:balance_before"`
	BalanceAfter         sql.NullFloat64 `gorm:"column:balance_after"`
	UsageAuthIndex       string          `gorm:"column:usage_auth_index"`
	AuthUUID             string          `gorm:"column:auth_uuid"`
	AuthJSON             JSONB           `gorm:"column:auth_json"`
	AuthID               string          `gorm:"column:auth_id"`
	AuthIndex            string          `gorm:"column:auth_index"`
	AuthProvider         string          `gorm:"column:auth_provider"`
	AuthLabel            string          `gorm:"column:auth_label"`
	AuthStatus           string          `gorm:"column:auth_status"`
	AuthDisabled         bool            `gorm:"column:auth_disabled"`
	AuthUnavailable      bool            `gorm:"column:auth_unavailable"`
	AuthNextRefreshAfter sql.NullTime    `gorm:"column:auth_next_refresh_after"`
	AuthNextRetryAfter   sql.NullTime    `gorm:"column:auth_next_retry_after"`
}

type usageObservabilityAggregateRow struct {
	AggregateID                string          `gorm:"column:aggregate_id"`
	AggregateLabel             string          `gorm:"column:aggregate_label"`
	MetadataUserID             sql.NullInt64   `gorm:"column:metadata_user_id"`
	MetadataUsername           string          `gorm:"column:metadata_username"`
	MetadataAPIKeyID           sql.NullInt64   `gorm:"column:metadata_api_key_id"`
	MetadataAPIKeyLabel        string          `gorm:"column:metadata_api_key_label"`
	MetadataAPIKeyMasked       string          `gorm:"column:metadata_api_key_masked"`
	MetadataCredentialType     string          `gorm:"column:metadata_credential_type"`
	MetadataCredentialID       string          `gorm:"column:metadata_credential_id"`
	MetadataAuthIndex          string          `gorm:"column:metadata_auth_index"`
	MetadataCredentialProvider string          `gorm:"column:metadata_credential_provider"`
	MetadataCredentialLabel    string          `gorm:"column:metadata_credential_label"`
	MetadataAuthUUID           string          `gorm:"column:metadata_auth_uuid"`
	MetadataAuthNextRetryAt    string          `gorm:"column:metadata_auth_next_retry_at"`
	MetadataAuthStatus         string          `gorm:"column:metadata_auth_status"`
	MetadataAuthDisabled       int64           `gorm:"column:metadata_auth_disabled"`
	MetadataAuthUnavailable    int64           `gorm:"column:metadata_auth_unavailable"`
	MetadataProvider           string          `gorm:"column:metadata_provider"`
	MetadataStatus             string          `gorm:"column:metadata_status"`
	RequestCount               int64           `gorm:"column:request_count"`
	SuccessCount               int64           `gorm:"column:success_count"`
	FailedCount                int64           `gorm:"column:failed_count"`
	InputTokens                int64           `gorm:"column:input_tokens"`
	OutputTokens               int64           `gorm:"column:output_tokens"`
	ReasoningTokens            int64           `gorm:"column:reasoning_tokens"`
	CachedTokens               int64           `gorm:"column:cached_tokens"`
	CacheReadTokens            int64           `gorm:"column:cache_read_tokens"`
	CacheCreationTokens        int64           `gorm:"column:cache_creation_tokens"`
	TotalTokens                int64           `gorm:"column:total_tokens"`
	TotalAmount                sql.NullFloat64 `gorm:"column:total_amount"`
	AvgLatencyMS               sql.NullFloat64 `gorm:"column:avg_latency_ms"`
	P95LatencyMS               sql.NullFloat64 `gorm:"column:p95_latency_ms"`
	LastUsedAt                 string          `gorm:"column:last_used_at"`
}

type UsageObservabilityRealtimeQuery struct {
	From           *time.Time
	To             *time.Time
	Provider       string
	Model          string
	HomeIP         string
	Endpoint       string
	CredentialType string
	GroupBy        string
	BucketSeconds  int
}

type UsageObservabilityRealtimeSnapshot struct {
	Velocity            []UsageObservabilityRealtimeVelocityPoint
	LatencyDistribution []UsageObservabilityLatencyDistributionBucket
	CurrentUsage        []UsageObservabilityAggregateItem
}

type UsageObservabilityRealtimeVelocityPoint struct {
	BucketStart time.Time
	BucketEnd   time.Time
	RPM         float64
	TPM         float64
	ErrorRate   float64
}

type UsageObservabilityLatencyDistributionBucket struct {
	Bucket       string
	RequestCount int64
}

type UsageObservabilityHealthDetail struct {
	LastErrorAt      *time.Time
	LastErrorStatus  int
	LastErrorMessage string
	NextRetryAt      *time.Time
}

type usageObservabilityAggregateAccumulator struct {
	Item          UsageObservabilityAggregateItem
	LatencyValues []int64
	LatencyTotal  int64
	AmountTotal   float64
	AmountValid   bool
}

type usageObservabilityOverviewBounds struct {
	MinTimestamp sql.NullString `gorm:"column:min_timestamp"`
	MaxTimestamp sql.NullString `gorm:"column:max_timestamp"`
}

type usageObservabilityTotalsRow struct {
	RequestCount          int64           `gorm:"column:request_count"`
	SuccessCount          sql.NullInt64   `gorm:"column:success_count"`
	FailedCount           sql.NullInt64   `gorm:"column:failed_count"`
	InputTokens           sql.NullInt64   `gorm:"column:input_tokens"`
	OutputTokens          sql.NullInt64   `gorm:"column:output_tokens"`
	ReasoningTokens       sql.NullInt64   `gorm:"column:reasoning_tokens"`
	CachedTokens          sql.NullInt64   `gorm:"column:cached_tokens"`
	CacheReadTokens       sql.NullInt64   `gorm:"column:cache_read_tokens"`
	CacheCreationTokens   sql.NullInt64   `gorm:"column:cache_creation_tokens"`
	TotalTokens           sql.NullInt64   `gorm:"column:total_tokens"`
	TotalAmount           sql.NullFloat64 `gorm:"column:total_amount"`
	AvgLatencyMS          sql.NullFloat64 `gorm:"column:avg_latency_ms"`
	AvgTTFTMS             sql.NullFloat64 `gorm:"column:avg_ttft_ms"`
	ActiveUserCount       int64           `gorm:"column:active_user_count"`
	ActiveClientKeyCount  int64           `gorm:"column:active_client_key_count"`
	ActiveCredentialCount int64           `gorm:"column:active_credential_count"`
	ActiveModelCount      int64           `gorm:"column:active_model_count"`
	MinTimestamp          sql.NullString  `gorm:"column:min_timestamp"`
	MaxTimestamp          sql.NullString  `gorm:"column:max_timestamp"`
}

type usageObservabilityLatencyPercentileRow struct {
	P50LatencyMS sql.NullFloat64 `gorm:"column:p50_latency_ms"`
	P95LatencyMS sql.NullFloat64 `gorm:"column:p95_latency_ms"`
}

type usageObservabilityTrendBucketRow struct {
	BucketUnix          int64           `gorm:"column:bucket_unix"`
	RequestCount        int64           `gorm:"column:request_count"`
	SuccessCount        sql.NullInt64   `gorm:"column:success_count"`
	FailedCount         sql.NullInt64   `gorm:"column:failed_count"`
	InputTokens         sql.NullInt64   `gorm:"column:input_tokens"`
	OutputTokens        sql.NullInt64   `gorm:"column:output_tokens"`
	ReasoningTokens     sql.NullInt64   `gorm:"column:reasoning_tokens"`
	CachedTokens        sql.NullInt64   `gorm:"column:cached_tokens"`
	CacheReadTokens     sql.NullInt64   `gorm:"column:cache_read_tokens"`
	CacheCreationTokens sql.NullInt64   `gorm:"column:cache_creation_tokens"`
	TotalTokens         sql.NullInt64   `gorm:"column:total_tokens"`
	TotalAmount         sql.NullFloat64 `gorm:"column:total_amount"`
	AvgLatencyMS        sql.NullFloat64 `gorm:"column:avg_latency_ms"`
}

type usageObservabilityTrendPercentileRow struct {
	BucketUnix   int64           `gorm:"column:bucket_unix"`
	P95LatencyMS sql.NullFloat64 `gorm:"column:p95_latency_ms"`
}

type usageObservabilityRealtimeVelocityRow struct {
	BucketUnix   int64         `gorm:"column:bucket_unix"`
	RequestCount int64         `gorm:"column:request_count"`
	FailedCount  sql.NullInt64 `gorm:"column:failed_count"`
	TotalTokens  sql.NullInt64 `gorm:"column:total_tokens"`
}

type usageObservabilityLatencyDistributionRow struct {
	Bucket       string `gorm:"column:bucket"`
	RequestCount int64  `gorm:"column:request_count"`
}

type usageObservabilityHealthLastErrorRow struct {
	SubjectID      string    `gorm:"column:subject_id"`
	UsageID        uint      `gorm:"column:usage_id"`
	Timestamp      time.Time `gorm:"column:timestamp"`
	FailStatusCode int       `gorm:"column:fail_status_code"`
}

type usageObservabilityHealthLastErrorBodyRow struct {
	UsageID  uint   `gorm:"column:usage_id"`
	FailBody string `gorm:"column:fail_body"`
}

type usageObservabilityHealthNextRetryRow struct {
	SubjectID   string `gorm:"column:subject_id"`
	NextRetryAt string `gorm:"column:next_retry_at"`
}

func (r *Repository) ListUsageObservabilityRecords(ctx context.Context, query UsageObservabilityRecordQuery) (UsageObservabilityRecordListResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageObservabilityRecordListResult{}, errDB
	}
	maxLimit := query.MaxLimit
	if maxLimit <= 0 {
		maxLimit = UsageObservabilityMaxRecordLimit
	}
	query.Limit, query.Offset = normalizeUsageObservabilityPagination(query.Limit, query.Offset, UsageObservabilityDefaultRecordLimit, maxLimit)

	scope := usageObservabilityRecordScope(db.WithContext(contextOrBackground(ctx)).Table("usage"), query)
	var total int64
	if errCount := scope.Session(&gorm.Session{}).Select(`COUNT(DISTINCT "usage"."id")`).Scan(&total).Error; errCount != nil {
		return UsageObservabilityRecordListResult{}, errCount
	}

	var rows []usageObservabilityRecordRow
	if errFind := scope.Session(&gorm.Session{}).
		Select(usageObservabilityRecordSelect()).
		Order(usageObservabilityRecordOrder(query.Sort)).
		Limit(query.Limit).
		Offset(query.Offset).
		Scan(&rows).Error; errFind != nil {
		return UsageObservabilityRecordListResult{}, errFind
	}

	records := make([]UsageObservabilityRecord, 0, len(rows))
	for index := range rows {
		records = append(records, usageObservabilityRecordFromRow(&rows[index]))
	}
	return UsageObservabilityRecordListResult{Records: records, Total: total}, nil
}

func (r *Repository) GetUsageObservabilityRecord(ctx context.Context, id string) (*UsageObservabilityRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	id = strings.TrimSpace(id)
	usageID, errParse := strconv.ParseUint(id, 10, 64)
	if errParse != nil || usageID == 0 {
		return nil, gorm.ErrRecordNotFound
	}

	var rows []usageObservabilityRecordRow
	scope := usageObservabilityRecordScope(db.WithContext(contextOrBackground(ctx)).Table("usage"), UsageObservabilityRecordQuery{}).
		Where(`"usage"."id" = ?`, uint(usageID)).
		Limit(1)
	if errFind := scope.Session(&gorm.Session{}).Select(usageObservabilityRecordSelect()).Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	record := usageObservabilityRecordFromRow(&rows[0])
	return &record, nil
}

func (r *Repository) UsageObservabilityFilterOptions(ctx context.Context, query UsageObservabilityRecordQuery) (UsageObservabilityFilterOptions, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageObservabilityFilterOptions{}, errDB
	}
	ctx = contextOrBackground(ctx)
	options := UsageObservabilityFilterOptions{}
	var errOptions error
	if options.EventTypes, errOptions = usageObservabilityDistinctStrings(ctx, db, query, `"usage"."event_type"`); errOptions != nil {
		return UsageObservabilityFilterOptions{}, errOptions
	}
	if options.Providers, errOptions = usageObservabilityDistinctStrings(ctx, db, query, `"usage"."provider"`); errOptions != nil {
		return UsageObservabilityFilterOptions{}, errOptions
	}
	if options.Models, errOptions = usageObservabilityDistinctStrings(ctx, db, query, `"usage"."model"`); errOptions != nil {
		return UsageObservabilityFilterOptions{}, errOptions
	}
	if options.HomeIPs, errOptions = usageObservabilityDistinctStrings(ctx, db, query, `"usage"."home_ip"`); errOptions != nil {
		return UsageObservabilityFilterOptions{}, errOptions
	}
	if options.CPANodes, errOptions = usageObservabilityDistinctStrings(ctx, db, query, `COALESCE(NULLIF("usage"."cpa_label", ''), NULLIF("usage"."cpa_node_id", ''), NULLIF("usage"."cpa_ip", ''))`); errOptions != nil {
		return UsageObservabilityFilterOptions{}, errOptions
	}
	if options.StatusCodes, errOptions = usageObservabilityDistinctStatusCodes(ctx, db, query); errOptions != nil {
		return UsageObservabilityFilterOptions{}, errOptions
	}
	return options, nil
}

func (r *Repository) GetUsageObservabilityPayloadSummary(ctx context.Context, id string) (*UsageObservabilityPayloadSummary, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	id = strings.TrimSpace(id)
	usageID, errParse := strconv.ParseUint(id, 10, 64)
	if errParse != nil || usageID == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	record := &UsageRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Select("id", "payload").First(record, "id = ?", uint(usageID)).Error; errFirst != nil {
		return nil, errFirst
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal([]byte(record.PayloadJSON), &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	summary := usageObservabilityPayloadSummary(payload)
	return &summary, nil
}

func (r *Repository) ListUsageObservabilityAggregates(ctx context.Context, query UsageObservabilityAggregateQuery) (UsageObservabilityAggregateResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageObservabilityAggregateResult{}, errDB
	}
	groupBy := strings.TrimSpace(query.GroupBy)
	if groupBy == "" {
		return UsageObservabilityAggregateResult{}, fmt.Errorf("group_by is required")
	}
	query.Limit, query.Offset = normalizeUsageObservabilityPagination(query.Limit, query.Offset, UsageObservabilityDefaultGroupLimit, UsageObservabilityMaxGroupLimit)
	recordQuery := UsageObservabilityRecordQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
	}

	baseForCount := usageObservabilityAggregateBaseQuery(db.WithContext(contextOrBackground(ctx)), recordQuery, groupBy)
	groupedForCount := db.WithContext(contextOrBackground(ctx)).
		Table("(?) AS scoped", baseForCount).
		Select("scoped.aggregate_id").
		Group("scoped.aggregate_id")
	var total int64
	if errCount := db.WithContext(contextOrBackground(ctx)).Table("(?) AS grouped", groupedForCount).Count(&total).Error; errCount != nil {
		return UsageObservabilityAggregateResult{}, errCount
	}

	includeP95ForSort := strings.TrimSpace(query.Metric) == "p95_latency_ms"
	baseForItems := usageObservabilityAggregateBaseQuery(db.WithContext(contextOrBackground(ctx)), recordQuery, groupBy)
	itemsQuery := db.WithContext(contextOrBackground(ctx)).
		Table("(?) AS scoped", baseForItems)
	if includeP95ForSort {
		p95Query := usageObservabilityAggregateP95Query(db.WithContext(contextOrBackground(ctx)), recordQuery, groupBy)
		itemsQuery = itemsQuery.Joins("LEFT JOIN (?) AS p95 ON p95.aggregate_id = scoped.aggregate_id", p95Query)
	}
	var rows []usageObservabilityAggregateRow
	if errFind := itemsQuery.
		Select(usageObservabilityAggregateSQLSelect(includeP95ForSort)).
		Group("scoped.aggregate_id").
		Order(usageObservabilityAggregateSQLOrder(query.Metric, query.Direction)).
		Limit(query.Limit).
		Offset(query.Offset).
		Scan(&rows).Error; errFind != nil {
		return UsageObservabilityAggregateResult{}, errFind
	}
	if !includeP95ForSort && len(rows) > 0 {
		p95Values, errP95 := usageObservabilityAggregateP95Values(db.WithContext(contextOrBackground(ctx)), recordQuery, groupBy, usageObservabilityAggregateRowIDs(rows))
		if errP95 != nil {
			return UsageObservabilityAggregateResult{}, errP95
		}
		for index := range rows {
			if value, ok := p95Values[rows[index].AggregateID]; ok {
				rows[index].P95LatencyMS = value
			}
		}
	}

	items := make([]UsageObservabilityAggregateItem, 0, len(rows))
	for index := range rows {
		items = append(items, usageObservabilityAggregateItemFromRow(&rows[index], groupBy))
	}
	return UsageObservabilityAggregateResult{Items: items, Total: total}, nil
}

func (r *Repository) UsageObservabilityOverview(ctx context.Context, query UsageObservabilityOverviewQuery) (UsageObservabilityOverview, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageObservabilityOverview{}, errDB
	}
	db = db.WithContext(contextOrBackground(ctx))
	recordQuery := usageObservabilityOverviewRecordQuery(query)
	totals, bounds, errTotals := usageObservabilityTotalsSQL(db, recordQuery)
	if errTotals != nil {
		return UsageObservabilityOverview{}, errTotals
	}

	interval := usageObservabilityOverviewIntervalFromBounds(query.Interval, query.From, query.To, bounds)
	location := usageObservabilityLocation(query.Timezone)
	overview := UsageObservabilityOverview{
		Range: UsageObservabilityOverviewRange{
			From:     usageObservabilityRangeTimeFromBounds(query.From, bounds, true),
			To:       usageObservabilityRangeTimeFromBounds(query.To, bounds, false),
			Timezone: firstNonEmptyUsageObservabilityString(query.Timezone, "UTC"),
			Interval: interval,
		},
		Totals:          totals,
		CostBreakdown:   []UsageObservabilityCostBreakdownItem{},
		ModelEfficiency: []UsageObservabilityAggregateItem{},
	}
	live, errLive := usageObservabilityLiveSQL(db, recordQuery, 300)
	if errLive != nil {
		return UsageObservabilityOverview{}, errLive
	}
	overview.Live = live
	trend, errTrend := usageObservabilityTrendSQL(db, recordQuery, interval, location, bounds)
	if errTrend != nil {
		return UsageObservabilityOverview{}, errTrend
	}
	overview.Trend = trend
	overview.Activity = usageObservabilityActivity(overview.Trend)
	modelEfficiency, errModelEfficiency := usageObservabilityAggregateItemsSQL(db, recordQuery, "model", "total_tokens", "desc", 10)
	if errModelEfficiency != nil {
		return UsageObservabilityOverview{}, errModelEfficiency
	}
	overview.ModelEfficiency = modelEfficiency
	top, errTop := usageObservabilityTopSQL(db, recordQuery)
	if errTop != nil {
		return UsageObservabilityOverview{}, errTop
	}
	overview.Top = top
	return overview, nil
}

func (r *Repository) UsageObservabilityRealtime(ctx context.Context, query UsageObservabilityRealtimeQuery) (UsageObservabilityRealtimeSnapshot, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageObservabilityRealtimeSnapshot{}, errDB
	}
	db = db.WithContext(contextOrBackground(ctx))
	recordQuery := UsageObservabilityRecordQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
	}
	velocity, errVelocity := usageObservabilityRealtimeVelocitySQL(db, recordQuery, query.BucketSeconds)
	if errVelocity != nil {
		return UsageObservabilityRealtimeSnapshot{}, errVelocity
	}
	latencyDistribution, errLatencyDistribution := usageObservabilityLatencyDistributionSQL(db, recordQuery)
	if errLatencyDistribution != nil {
		return UsageObservabilityRealtimeSnapshot{}, errLatencyDistribution
	}
	currentUsage, errCurrentUsage := usageObservabilityAggregateItemsSQL(db, recordQuery, query.GroupBy, "request_count", "desc", 20)
	if errCurrentUsage != nil {
		return UsageObservabilityRealtimeSnapshot{}, errCurrentUsage
	}
	return UsageObservabilityRealtimeSnapshot{Velocity: velocity, LatencyDistribution: latencyDistribution, CurrentUsage: currentUsage}, nil
}

func (r *Repository) UsageObservabilityHealthDetails(ctx context.Context, query UsageObservabilityRecordQuery, subject string) (map[string]UsageObservabilityHealthDetail, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	db = db.WithContext(contextOrBackground(ctx))
	details := map[string]UsageObservabilityHealthDetail{}
	lastErrors, errLastErrors := usageObservabilityHealthLastErrorsSQL(db, query, subject)
	if errLastErrors != nil {
		return nil, errLastErrors
	}
	for key, detail := range lastErrors {
		details[key] = detail
	}
	nextRetries, errNextRetries := usageObservabilityHealthNextRetriesSQL(db, query, subject)
	if errNextRetries != nil {
		return nil, errNextRetries
	}
	for key, nextRetryAt := range nextRetries {
		detail := details[key]
		nextRetryValue := nextRetryAt.UTC()
		detail.NextRetryAt = &nextRetryValue
		details[key] = detail
	}
	return details, nil
}

func usageObservabilityOverviewRecordQuery(query UsageObservabilityOverviewQuery) UsageObservabilityRecordQuery {
	return UsageObservabilityRecordQuery{
		From:           query.From,
		To:             query.To,
		Provider:       query.Provider,
		Model:          query.Model,
		HomeIP:         query.HomeIP,
		Endpoint:       query.Endpoint,
		CredentialType: query.CredentialType,
	}
}

func usageObservabilityTotalsSQL(db *gorm.DB, query UsageObservabilityRecordQuery) (UsageObservabilityTotals, usageObservabilityOverviewBounds, error) {
	var row usageObservabilityTotalsRow
	scope := usageObservabilityRecordScope(db.Table("usage"), query)
	if errScan := scope.Session(&gorm.Session{}).Select(usageObservabilityTotalsSQLSelect()).Scan(&row).Error; errScan != nil {
		return UsageObservabilityTotals{}, usageObservabilityOverviewBounds{}, errScan
	}
	totals := usageObservabilityTotalsFromRow(&row)
	percentiles, errPercentiles := usageObservabilityLatencyPercentilesSQL(db, query)
	if errPercentiles != nil {
		return UsageObservabilityTotals{}, usageObservabilityOverviewBounds{}, errPercentiles
	}
	if percentiles.P50LatencyMS.Valid {
		p50 := percentiles.P50LatencyMS.Float64
		totals.P50LatencyMS = &p50
	}
	if percentiles.P95LatencyMS.Valid {
		p95 := percentiles.P95LatencyMS.Float64
		totals.P95LatencyMS = &p95
	}
	bounds := usageObservabilityOverviewBounds{MinTimestamp: row.MinTimestamp, MaxTimestamp: row.MaxTimestamp}
	return totals, bounds, nil
}

func usageObservabilityTotalsSQLSelect() string {
	credentialExpr := `NULLIF(` + usageObservabilitySQLCredentialID() + `, '')`
	return fmt.Sprintf(`
		COUNT(DISTINCT "usage"."id") AS request_count,
		SUM(CASE WHEN "usage"."failed" THEN 0 ELSE 1 END) AS success_count,
		SUM(CASE WHEN "usage"."failed" THEN 1 ELSE 0 END) AS failed_count,
		SUM("usage"."input_tokens") AS input_tokens,
		SUM("usage"."output_tokens") AS output_tokens,
		SUM("usage"."reasoning_tokens") AS reasoning_tokens,
		SUM("usage"."cached_tokens") AS cached_tokens,
		SUM("usage"."cache_read_tokens") AS cache_read_tokens,
		SUM("usage"."cache_creation_tokens") AS cache_creation_tokens,
		SUM("usage"."total_tokens") AS total_tokens,
		SUM("billing_charge"."amount") AS total_amount,
		AVG(CASE WHEN "usage"."latency_ms" >= 0 THEN "usage"."latency_ms" END) AS avg_latency_ms,
		AVG(CASE WHEN "usage"."ttft_ms" > 0 THEN "usage"."ttft_ms" END) AS avg_ttft_ms,
		COUNT(DISTINCT COALESCE("billing_charge"."user_id", "api_key"."user_id")) AS active_user_count,
		COUNT(DISTINCT COALESCE("billing_charge"."api_key_id", "api_key"."id")) AS active_client_key_count,
		COUNT(DISTINCT %s) AS active_credential_count,
		COUNT(DISTINCT NULLIF("usage"."model", '')) AS active_model_count,
		MIN("usage"."timestamp") AS min_timestamp,
		MAX("usage"."timestamp") AS max_timestamp`, credentialExpr)
}

func usageObservabilityTotalsFromRow(row *usageObservabilityTotalsRow) UsageObservabilityTotals {
	if row == nil {
		return UsageObservabilityTotals{}
	}
	totals := UsageObservabilityTotals{
		RequestCount:          row.RequestCount,
		SuccessCount:          optionalSQLInt64Value(row.SuccessCount),
		FailedCount:           optionalSQLInt64Value(row.FailedCount),
		InputTokens:           optionalSQLInt64Value(row.InputTokens),
		OutputTokens:          optionalSQLInt64Value(row.OutputTokens),
		ReasoningTokens:       optionalSQLInt64Value(row.ReasoningTokens),
		CachedTokens:          optionalSQLInt64Value(row.CachedTokens),
		CacheReadTokens:       optionalSQLInt64Value(row.CacheReadTokens),
		CacheCreationTokens:   optionalSQLInt64Value(row.CacheCreationTokens),
		TotalTokens:           optionalSQLInt64Value(row.TotalTokens),
		ActiveUserCount:       row.ActiveUserCount,
		ActiveClientKeyCount:  row.ActiveClientKeyCount,
		ActiveCredentialCount: row.ActiveCredentialCount,
		ActiveModelCount:      row.ActiveModelCount,
	}
	if totals.RequestCount > 0 {
		totals.SuccessRate = float64(totals.SuccessCount) / float64(totals.RequestCount)
		totals.ErrorRate = float64(totals.FailedCount) / float64(totals.RequestCount)
	}
	if row.TotalAmount.Valid {
		amount := row.TotalAmount.Float64
		totals.TotalAmount = &amount
		totals.Currency = UsageObservabilityCurrencyCredits
	}
	if row.AvgLatencyMS.Valid {
		avg := row.AvgLatencyMS.Float64
		totals.AvgLatencyMS = &avg
	}
	if row.AvgTTFTMS.Valid {
		avgTTFT := row.AvgTTFTMS.Float64
		totals.AvgTTFTMS = &avgTTFT
	}
	if totals.TotalAmount != nil && totals.TotalTokens > 0 {
		blended := *totals.TotalAmount * 1000000 / float64(totals.TotalTokens)
		totals.BlendedCostPer1M = &blended
	}
	return totals
}

func optionalSQLInt64Value(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func usageObservabilityLatencyPercentilesSQL(db *gorm.DB, query UsageObservabilityRecordQuery) (usageObservabilityLatencyPercentileRow, error) {
	base := usageObservabilityNarrowUsageScope(db.Table("usage"), query).
		Select(`"usage"."latency_ms" AS latency_ms`).
		Where(`"usage"."latency_ms" >= ?`, 0)
	ranked := db.Table("(?) AS scoped_latency", base).
		Select(`
			scoped_latency.latency_ms AS latency_ms,
			COUNT(*) OVER () AS latency_count,
			ROW_NUMBER() OVER (ORDER BY scoped_latency.latency_ms ASC) AS latency_rank`)
	var row usageObservabilityLatencyPercentileRow
	if errScan := db.Table("(?) AS ranked_latency", ranked).
		Select(`
			MIN(CASE WHEN ranked_latency.latency_rank * 100 >= ranked_latency.latency_count * 50 THEN ranked_latency.latency_ms END) AS p50_latency_ms,
			MIN(CASE WHEN ranked_latency.latency_rank * 100 >= ranked_latency.latency_count * 95 THEN ranked_latency.latency_ms END) AS p95_latency_ms`).
		Scan(&row).Error; errScan != nil {
		return usageObservabilityLatencyPercentileRow{}, errScan
	}
	return row, nil
}

func usageObservabilityLiveSQL(db *gorm.DB, query UsageObservabilityRecordQuery, windowSeconds int) (UsageObservabilityLiveSummary, error) {
	if windowSeconds <= 0 {
		windowSeconds = 300
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowSeconds) * time.Second)
	liveQuery := query
	if liveQuery.From == nil || liveQuery.From.Before(cutoff) {
		liveQuery.From = &cutoff
	}
	totals, _, errTotals := usageObservabilityTotalsSQL(db, liveQuery)
	if errTotals != nil {
		return UsageObservabilityLiveSummary{}, errTotals
	}
	live := UsageObservabilityLiveSummary{
		WindowSeconds: windowSeconds,
		ErrorRate:     totals.ErrorRate,
		SuccessRate:   totals.SuccessRate,
		P50LatencyMS:  totals.P50LatencyMS,
		P95LatencyMS:  totals.P95LatencyMS,
	}
	minutes := float64(windowSeconds) / 60
	if minutes > 0 {
		live.RPM = float64(totals.RequestCount) / minutes
		live.TPM = float64(totals.TotalTokens) / minutes
	}
	return live, nil
}

func usageObservabilityTrendSQL(db *gorm.DB, query UsageObservabilityRecordQuery, interval string, location *time.Location, bounds usageObservabilityOverviewBounds) ([]UsageObservabilityTrendPoint, error) {
	bucketExpr, bucketArgs := usageObservabilityTrendBucketUnixSQL(db, interval, location, query, bounds)
	baseSelect := fmt.Sprintf(`
		%s AS bucket_unix,
		"usage"."latency_ms" AS latency_ms,
		"usage"."input_tokens" AS input_tokens,
		"usage"."output_tokens" AS output_tokens,
		"usage"."reasoning_tokens" AS reasoning_tokens,
		"usage"."cached_tokens" AS cached_tokens,
		"usage"."cache_read_tokens" AS cache_read_tokens,
		"usage"."cache_creation_tokens" AS cache_creation_tokens,
		"usage"."total_tokens" AS total_tokens,
		"usage"."failed" AS failed,
		"billing_charge"."amount" AS amount`, bucketExpr)
	base := usageObservabilityUsageBillingScope(db.Table("usage"), query).Select(baseSelect, bucketArgs...)
	var rows []usageObservabilityTrendBucketRow
	if errFind := db.Table("(?) AS trend_source", base).
		Select(`
			trend_source.bucket_unix AS bucket_unix,
			COUNT(*) AS request_count,
			SUM(CASE WHEN trend_source.failed THEN 0 ELSE 1 END) AS success_count,
			SUM(CASE WHEN trend_source.failed THEN 1 ELSE 0 END) AS failed_count,
			SUM(trend_source.input_tokens) AS input_tokens,
			SUM(trend_source.output_tokens) AS output_tokens,
			SUM(trend_source.reasoning_tokens) AS reasoning_tokens,
			SUM(trend_source.cached_tokens) AS cached_tokens,
			SUM(trend_source.cache_read_tokens) AS cache_read_tokens,
			SUM(trend_source.cache_creation_tokens) AS cache_creation_tokens,
			SUM(trend_source.total_tokens) AS total_tokens,
			SUM(trend_source.amount) AS total_amount,
			AVG(CASE WHEN trend_source.latency_ms >= 0 THEN trend_source.latency_ms END) AS avg_latency_ms`).
		Group("trend_source.bucket_unix").
		Order("trend_source.bucket_unix ASC").
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}

	percentiles, errPercentiles := usageObservabilityTrendP95SQL(db, query, bucketExpr, bucketArgs)
	if errPercentiles != nil {
		return nil, errPercentiles
	}
	return usageObservabilityTrendFromBucketRows(rows, percentiles, interval, location), nil
}

func usageObservabilityTrendP95SQL(db *gorm.DB, query UsageObservabilityRecordQuery, bucketExpr string, bucketArgs []any) (map[int64]sql.NullFloat64, error) {
	baseSelect := fmt.Sprintf(`
		%s AS bucket_unix,
		"usage"."latency_ms" AS latency_ms`, bucketExpr)
	base := usageObservabilityNarrowUsageScope(db.Table("usage"), query).
		Select(baseSelect, bucketArgs...).
		Where(`"usage"."latency_ms" >= ?`, 0)
	ranked := db.Table("(?) AS trend_latency", base).
		Select(`
			trend_latency.bucket_unix AS bucket_unix,
			trend_latency.latency_ms AS latency_ms,
			COUNT(*) OVER (PARTITION BY trend_latency.bucket_unix) AS latency_count,
			ROW_NUMBER() OVER (PARTITION BY trend_latency.bucket_unix ORDER BY trend_latency.latency_ms ASC) AS latency_rank`)
	var rows []usageObservabilityTrendPercentileRow
	if errScan := db.Table("(?) AS ranked_latency", ranked).
		Select(`
			ranked_latency.bucket_unix AS bucket_unix,
			MIN(ranked_latency.latency_ms) AS p95_latency_ms`).
		Where("ranked_latency.latency_rank * 100 >= ranked_latency.latency_count * 95").
		Group("ranked_latency.bucket_unix").
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	percentiles := make(map[int64]sql.NullFloat64, len(rows))
	for index := range rows {
		percentiles[rows[index].BucketUnix] = rows[index].P95LatencyMS
	}
	return percentiles, nil
}

func usageObservabilityTrendFromBucketRows(rows []usageObservabilityTrendBucketRow, percentiles map[int64]sql.NullFloat64, interval string, location *time.Location) []UsageObservabilityTrendPoint {
	points := make([]UsageObservabilityTrendPoint, 0, len(rows))
	for index := range rows {
		start, end := usageObservabilityBucketRange(time.Unix(rows[index].BucketUnix, 0).UTC(), interval, location)
		point := UsageObservabilityTrendPoint{
			BucketStart:         start,
			BucketEnd:           end,
			RequestCount:        rows[index].RequestCount,
			SuccessCount:        optionalSQLInt64Value(rows[index].SuccessCount),
			FailedCount:         optionalSQLInt64Value(rows[index].FailedCount),
			InputTokens:         optionalSQLInt64Value(rows[index].InputTokens),
			OutputTokens:        optionalSQLInt64Value(rows[index].OutputTokens),
			ReasoningTokens:     optionalSQLInt64Value(rows[index].ReasoningTokens),
			CachedTokens:        optionalSQLInt64Value(rows[index].CachedTokens),
			CacheReadTokens:     optionalSQLInt64Value(rows[index].CacheReadTokens),
			CacheCreationTokens: optionalSQLInt64Value(rows[index].CacheCreationTokens),
			TotalTokens:         optionalSQLInt64Value(rows[index].TotalTokens),
		}
		if rows[index].TotalAmount.Valid {
			amount := rows[index].TotalAmount.Float64
			point.TotalAmount = &amount
		}
		if rows[index].AvgLatencyMS.Valid {
			avg := rows[index].AvgLatencyMS.Float64
			point.AvgLatencyMS = &avg
		}
		if percentile, ok := percentiles[rows[index].BucketUnix]; ok && percentile.Valid {
			p95 := percentile.Float64
			point.P95LatencyMS = &p95
		}
		points = append(points, point)
	}
	return points
}

func usageObservabilityTrendBucketUnixSQL(db *gorm.DB, interval string, location *time.Location, query UsageObservabilityRecordQuery, bounds usageObservabilityOverviewBounds) (string, []any) {
	bucketSeconds := int64(86400)
	subtractSeconds := int64(0)
	switch interval {
	case "minute":
		bucketSeconds = 60
	case "hour":
		bucketSeconds = 3600
	case "week":
		bucketSeconds = 604800
		subtractSeconds = 4 * 86400
	}
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		datePart := "day"
		switch interval {
		case "minute":
			datePart = "minute"
		case "hour":
			datePart = "hour"
		case "week":
			datePart = "week"
		}
		timezoneName := "UTC"
		if location != nil {
			timezoneName = location.String()
		}
		return fmt.Sprintf(`CAST(EXTRACT(EPOCH FROM (date_trunc('%s', timezone(?, "usage"."timestamp")) AT TIME ZONE ?)) AS BIGINT)`, datePart), []any{timezoneName, timezoneName}
	}
	if bucketCaseSQL, ok := usageObservabilitySQLiteTrendBucketCaseSQL(interval, location, query, bounds); ok {
		return bucketCaseSQL, nil
	}
	offsetSeconds := int64(usageObservabilityTimezoneOffsetSeconds(location, query))
	return `CAST(((CAST(strftime('%s', "usage"."timestamp") AS INTEGER) + ? - ?) / ?) AS INTEGER) * ? - ? + ?`, []any{offsetSeconds, subtractSeconds, bucketSeconds, bucketSeconds, offsetSeconds, subtractSeconds}
}

func usageObservabilitySQLiteTrendBucketCaseSQL(interval string, location *time.Location, query UsageObservabilityRecordQuery, bounds usageObservabilityOverviewBounds) (string, bool) {
	if location == nil || location == time.UTC {
		return "", false
	}
	rangeStart, rangeEnd := usageObservabilityTrendCaseRange(query, bounds)
	if rangeStart.IsZero() || rangeEnd.IsZero() || rangeEnd.Before(rangeStart) {
		return "", false
	}
	caseStart, _ := usageObservabilityBucketRange(rangeStart, interval, location)
	_, caseEnd := usageObservabilityBucketRange(rangeEnd, interval, location)
	if caseEnd.Before(rangeEnd) {
		caseEnd = rangeEnd
	}
	if !usageObservabilityTimezoneOffsetChanges(location, caseStart, caseEnd) {
		return "", false
	}

	usageUnixExpr := `CAST(strftime('%s', "usage"."timestamp") AS INTEGER)`
	fallbackOffset := int64(usageObservabilityTimezoneOffsetSeconds(location, query))
	fallbackSQL := usageObservabilitySQLiteTrendBucketOffsetSQL(interval, fallbackOffset)
	segments := usageObservabilityTrendBucketCaseSegments(interval, location, caseStart, caseEnd)
	if len(segments) == 0 {
		return "", false
	}

	var builder strings.Builder
	builder.WriteString("CASE")
	for _, segment := range segments {
		builder.WriteString(fmt.Sprintf(" WHEN %s >= %d AND %s < %d THEN %d", usageUnixExpr, segment.StartUnix, usageUnixExpr, segment.EndUnix, segment.BucketUnix))
	}
	builder.WriteString(" ELSE ")
	builder.WriteString(fallbackSQL)
	builder.WriteString(" END")
	return builder.String(), true
}

type usageObservabilityTrendBucketCaseSegment struct {
	StartUnix  int64
	EndUnix    int64
	BucketUnix int64
}

func usageObservabilityTrendCaseRange(query UsageObservabilityRecordQuery, bounds usageObservabilityOverviewBounds) (time.Time, time.Time) {
	start, end := usageObservabilityBoundsTimes(bounds)
	if query.From != nil {
		start = query.From.UTC()
	}
	if query.To != nil {
		end = query.To.UTC()
	}
	return start.UTC(), end.UTC()
}

func usageObservabilityTimezoneOffsetChanges(location *time.Location, start time.Time, end time.Time) bool {
	if location == nil || location == time.UTC || start.IsZero() || end.IsZero() || end.Before(start) {
		return false
	}
	_, firstOffset := start.In(location).Zone()
	for cursor := start.Add(time.Hour); cursor.Before(end); cursor = cursor.Add(time.Hour) {
		_, offset := cursor.In(location).Zone()
		if offset != firstOffset {
			return true
		}
	}
	_, endOffset := end.In(location).Zone()
	return endOffset != firstOffset
}

func usageObservabilityTrendBucketCaseSegments(interval string, location *time.Location, start time.Time, end time.Time) []usageObservabilityTrendBucketCaseSegment {
	if location == nil {
		location = time.UTC
	}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil
	}
	step := time.Minute
	cursor := start.UTC().Truncate(step)
	limit := end.UTC().Add(step)
	segments := make([]usageObservabilityTrendBucketCaseSegment, 0)
	for cursor.Before(limit) || cursor.Equal(limit) {
		bucketStart, _ := usageObservabilityBucketRange(cursor, interval, location)
		segment := usageObservabilityTrendBucketCaseSegment{
			StartUnix:  cursor.Unix(),
			EndUnix:    cursor.Add(step).Unix(),
			BucketUnix: bucketStart.Unix(),
		}
		lastIndex := len(segments) - 1
		if lastIndex >= 0 && segments[lastIndex].BucketUnix == segment.BucketUnix && segments[lastIndex].EndUnix == segment.StartUnix {
			segments[lastIndex].EndUnix = segment.EndUnix
		} else {
			segments = append(segments, segment)
		}
		cursor = cursor.Add(step)
	}
	return segments
}

func usageObservabilitySQLiteTrendBucketOffsetSQL(interval string, offsetSeconds int64) string {
	bucketSeconds := int64(86400)
	subtractSeconds := int64(0)
	switch interval {
	case "minute":
		bucketSeconds = 60
	case "hour":
		bucketSeconds = 3600
	case "week":
		bucketSeconds = 604800
		subtractSeconds = 4 * 86400
	}
	return fmt.Sprintf(`CAST(((CAST(strftime('%%s', "usage"."timestamp") AS INTEGER) + %d - %d) / %d) AS INTEGER) * %d - %d + %d`, offsetSeconds, subtractSeconds, bucketSeconds, bucketSeconds, offsetSeconds, subtractSeconds)
}

func usageObservabilityTimezoneOffsetSeconds(location *time.Location, query UsageObservabilityRecordQuery) int {
	if location == nil {
		location = time.UTC
	}
	reference := time.Now().UTC()
	if query.From != nil {
		reference = query.From.UTC()
	} else if query.To != nil {
		reference = query.To.UTC()
	}
	_, offset := reference.In(location).Zone()
	return offset
}

func usageObservabilityTopSQL(db *gorm.DB, query UsageObservabilityRecordQuery) (UsageObservabilityTopGroups, error) {
	users, errUsers := usageObservabilityAggregateItemsSQL(db, query, "user", "request_count", "desc", 10)
	if errUsers != nil {
		return UsageObservabilityTopGroups{}, errUsers
	}
	clientKeys, errClientKeys := usageObservabilityAggregateItemsSQL(db, query, "client_key", "request_count", "desc", 10)
	if errClientKeys != nil {
		return UsageObservabilityTopGroups{}, errClientKeys
	}
	credentials, errCredentials := usageObservabilityAggregateItemsSQL(db, query, "credential", "request_count", "desc", 10)
	if errCredentials != nil {
		return UsageObservabilityTopGroups{}, errCredentials
	}
	providers, errProviders := usageObservabilityAggregateItemsSQL(db, query, "provider", "request_count", "desc", 10)
	if errProviders != nil {
		return UsageObservabilityTopGroups{}, errProviders
	}
	models, errModels := usageObservabilityAggregateItemsSQL(db, query, "model", "request_count", "desc", 10)
	if errModels != nil {
		return UsageObservabilityTopGroups{}, errModels
	}
	endpoints, errEndpoints := usageObservabilityAggregateItemsSQL(db, query, "endpoint", "request_count", "desc", 10)
	if errEndpoints != nil {
		return UsageObservabilityTopGroups{}, errEndpoints
	}
	errorQuery := query
	errorQuery.Status = "failed"
	errors, errErrors := usageObservabilityAggregateItemsSQL(db, errorQuery, "status_code", "failed_count", "desc", 10)
	if errErrors != nil {
		return UsageObservabilityTopGroups{}, errErrors
	}
	return UsageObservabilityTopGroups{
		Users:       users,
		ClientKeys:  clientKeys,
		Credentials: credentials,
		Providers:   providers,
		Models:      models,
		Endpoints:   endpoints,
		Errors:      errors,
	}, nil
}

func usageObservabilityAggregateItemsSQL(db *gorm.DB, query UsageObservabilityRecordQuery, groupBy string, metric string, direction string, limit int) ([]UsageObservabilityAggregateItem, error) {
	if limit <= 0 {
		limit = UsageObservabilityDefaultGroupLimit
	}
	includeP95ForSort := strings.TrimSpace(metric) == "p95_latency_ms"
	baseForItems := usageObservabilityAggregateBaseQuery(db, query, groupBy)
	itemsQuery := db.Table("(?) AS scoped", baseForItems)
	if includeP95ForSort {
		p95Query := usageObservabilityAggregateP95Query(db, query, groupBy)
		itemsQuery = itemsQuery.Joins("LEFT JOIN (?) AS p95 ON p95.aggregate_id = scoped.aggregate_id", p95Query)
	}
	var rows []usageObservabilityAggregateRow
	if errFind := itemsQuery.
		Select(usageObservabilityAggregateSQLSelect(includeP95ForSort)).
		Group("scoped.aggregate_id").
		Order(usageObservabilityAggregateSQLOrder(metric, direction)).
		Limit(limit).
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	if !includeP95ForSort && len(rows) > 0 {
		p95Values, errP95 := usageObservabilityAggregateP95Values(db, query, groupBy, usageObservabilityAggregateRowIDs(rows))
		if errP95 != nil {
			return nil, errP95
		}
		for index := range rows {
			if value, ok := p95Values[rows[index].AggregateID]; ok {
				rows[index].P95LatencyMS = value
			}
		}
	}
	items := make([]UsageObservabilityAggregateItem, 0, len(rows))
	for index := range rows {
		items = append(items, usageObservabilityAggregateItemFromRow(&rows[index], groupBy))
	}
	return items, nil
}

func usageObservabilityRealtimeVelocitySQL(db *gorm.DB, query UsageObservabilityRecordQuery, bucketSeconds int) ([]UsageObservabilityRealtimeVelocityPoint, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 60
	}
	bucketExpr, bucketArgs := usageObservabilityBucketUnixSQL(db, bucketSeconds)
	selectSQL := fmt.Sprintf(`
		%s AS bucket_unix,
		COUNT(*) AS request_count,
		SUM(CASE WHEN "usage"."failed" THEN 1 ELSE 0 END) AS failed_count,
		SUM("usage"."total_tokens") AS total_tokens`, bucketExpr)
	var rows []usageObservabilityRealtimeVelocityRow
	if errFind := usageObservabilityNarrowUsageScope(db.Table("usage"), query).
		Select(selectSQL, bucketArgs...).
		Group("bucket_unix").
		Order("bucket_unix ASC").
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	points := make([]UsageObservabilityRealtimeVelocityPoint, 0, len(rows))
	minutes := float64(bucketSeconds) / 60
	for index := range rows {
		start := time.Unix(rows[index].BucketUnix, 0).UTC()
		requestCount := rows[index].RequestCount
		failedCount := optionalSQLInt64Value(rows[index].FailedCount)
		totalTokens := optionalSQLInt64Value(rows[index].TotalTokens)
		errorRate := 0.0
		if requestCount > 0 {
			errorRate = float64(failedCount) / float64(requestCount)
		}
		points = append(points, UsageObservabilityRealtimeVelocityPoint{
			BucketStart: start,
			BucketEnd:   start.Add(time.Duration(bucketSeconds)*time.Second - time.Nanosecond),
			RPM:         float64(requestCount) / minutes,
			TPM:         float64(totalTokens) / minutes,
			ErrorRate:   errorRate,
		})
	}
	return points, nil
}

func usageObservabilityBucketUnixSQL(db *gorm.DB, bucketSeconds int) (string, []any) {
	if bucketSeconds <= 0 {
		bucketSeconds = 60
	}
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return `CAST(FLOOR(EXTRACT(EPOCH FROM "usage"."timestamp") / ?) * ? AS BIGINT)`, []any{bucketSeconds, bucketSeconds}
	}
	return `CAST((CAST(strftime('%s', "usage"."timestamp") AS INTEGER) / ?) AS INTEGER) * ?`, []any{bucketSeconds, bucketSeconds}
}

func usageObservabilityLatencyDistributionSQL(db *gorm.DB, query UsageObservabilityRecordQuery) ([]UsageObservabilityLatencyDistributionBucket, error) {
	labels := []string{"0-500ms", "500-1000ms", "1000-3000ms", "3000ms+"}
	counts := map[string]int64{}
	for _, label := range labels {
		counts[label] = 0
	}
	var rows []usageObservabilityLatencyDistributionRow
	if errFind := usageObservabilityNarrowUsageScope(db.Table("usage"), query).
		Where(`"usage"."latency_ms" >= ?`, 0).
		Select(`
			CASE
				WHEN "usage"."latency_ms" <= 500 THEN '0-500ms'
				WHEN "usage"."latency_ms" <= 1000 THEN '500-1000ms'
				WHEN "usage"."latency_ms" <= 3000 THEN '1000-3000ms'
				ELSE '3000ms+'
			END AS bucket,
			COUNT(*) AS request_count`).
		Group("bucket").
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	for index := range rows {
		counts[rows[index].Bucket] = rows[index].RequestCount
	}
	buckets := make([]UsageObservabilityLatencyDistributionBucket, 0, len(labels))
	for _, label := range labels {
		buckets = append(buckets, UsageObservabilityLatencyDistributionBucket{Bucket: label, RequestCount: counts[label]})
	}
	return buckets, nil
}

func usageObservabilityHealthLastErrorsSQL(db *gorm.DB, query UsageObservabilityRecordQuery, subject string) (map[string]UsageObservabilityHealthDetail, error) {
	subjectExpr := usageObservabilityHealthSubjectSQL(subject)
	scoped := usageObservabilityRecordScope(db.Table("usage"), query).
		Where(`"usage"."failed" = ?`, true).
		Select(fmt.Sprintf(`
			%s AS subject_id,
			"usage"."id" AS usage_id,
			"usage"."timestamp" AS timestamp,
			"usage"."fail_status_code" AS fail_status_code`, subjectExpr))
	ranked := db.Table("(?) AS scoped_error", scoped).
		Select(`
			scoped_error.subject_id AS subject_id,
			scoped_error.usage_id AS usage_id,
			scoped_error.timestamp AS timestamp,
			scoped_error.fail_status_code AS fail_status_code,
			ROW_NUMBER() OVER (PARTITION BY scoped_error.subject_id ORDER BY scoped_error.timestamp DESC, scoped_error.usage_id DESC) AS error_rank`)
	var rows []usageObservabilityHealthLastErrorRow
	if errFind := db.Table("(?) AS ranked_error", ranked).
		Where("ranked_error.error_rank = ?", 1).
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	failBodies, errBodies := usageObservabilityHealthFailBodiesSQL(db, rows)
	if errBodies != nil {
		return nil, errBodies
	}
	details := make(map[string]UsageObservabilityHealthDetail, len(rows))
	for index := range rows {
		timestamp := rows[index].Timestamp.UTC()
		details[rows[index].SubjectID] = UsageObservabilityHealthDetail{
			LastErrorAt:      &timestamp,
			LastErrorStatus:  rows[index].FailStatusCode,
			LastErrorMessage: usageObservabilityErrorMessage(failBodies[rows[index].UsageID]),
		}
	}
	return details, nil
}

func usageObservabilityHealthFailBodiesSQL(db *gorm.DB, rows []usageObservabilityHealthLastErrorRow) (map[uint]string, error) {
	usageIDs := make([]uint, 0, len(rows))
	seen := make(map[uint]struct{}, len(rows))
	for index := range rows {
		usageID := rows[index].UsageID
		if usageID == 0 {
			continue
		}
		if _, ok := seen[usageID]; ok {
			continue
		}
		seen[usageID] = struct{}{}
		usageIDs = append(usageIDs, usageID)
	}
	if len(usageIDs) == 0 {
		return map[uint]string{}, nil
	}
	var bodyRows []usageObservabilityHealthLastErrorBodyRow
	if errFind := db.Table("usage").
		Select(`"usage"."id" AS usage_id, "usage"."fail_body" AS fail_body`).
		Where(`"usage"."id" IN ?`, usageIDs).
		Scan(&bodyRows).Error; errFind != nil {
		return nil, errFind
	}
	out := make(map[uint]string, len(bodyRows))
	for index := range bodyRows {
		out[bodyRows[index].UsageID] = bodyRows[index].FailBody
	}
	return out, nil
}

func usageObservabilityHealthNextRetriesSQL(db *gorm.DB, query UsageObservabilityRecordQuery, subject string) (map[string]time.Time, error) {
	subjectExpr := usageObservabilityHealthSubjectSQL(subject)
	nextRetryExpr := usageObservabilitySQLAuthNextRetryAfter()
	scoped := usageObservabilityRecordScope(db.Table("usage"), query).
		Select(fmt.Sprintf(`
			%s AS subject_id,
			%s AS next_retry_at`, subjectExpr, nextRetryExpr))
	var rows []usageObservabilityHealthNextRetryRow
	if errFind := db.Table("(?) AS scoped_retry", scoped).
		Select("DISTINCT scoped_retry.subject_id AS subject_id, scoped_retry.next_retry_at AS next_retry_at").
		Where("scoped_retry.next_retry_at IS NOT NULL").
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	nextRetries := map[string]time.Time{}
	for index := range rows {
		nextRetryAt := usageObservabilityOptionalAggregateTime(rows[index].NextRetryAt)
		if nextRetryAt == nil || nextRetryAt.IsZero() {
			continue
		}
		key := strings.TrimSpace(rows[index].SubjectID)
		if key == "" {
			continue
		}
		value := nextRetryAt.UTC()
		current, exists := nextRetries[key]
		if exists && !value.Before(current) {
			continue
		}
		nextRetries[key] = value
	}
	return nextRetries, nil
}

func usageObservabilityHealthSubjectSQL(subject string) string {
	if strings.TrimSpace(subject) == "credential" {
		id, _ := usageObservabilityAggregateSQLIdentity("credential")
		return id
	}
	id, _ := usageObservabilityAggregateSQLIdentity("provider")
	return id
}

func usageObservabilityAggregateBaseQuery(db *gorm.DB, query UsageObservabilityRecordQuery, groupBy string) *gorm.DB {
	scope := db.Table("usage").Joins(`LEFT JOIN "billing_charge" ON "billing_charge"."usage_id" = "usage"."id"`)
	switch strings.TrimSpace(groupBy) {
	case "user", "client_key":
		scope = scope.
			Joins(`LEFT JOIN "api_key" ON "api_key"."api_key" = "usage"."api_key"`).
			Joins(`LEFT JOIN "user" ON "user"."id" = COALESCE("billing_charge"."user_id", "api_key"."user_id")`)
	case "credential":
		scope = scope.
			Joins(`LEFT JOIN "auth" AS "auth_uuid" ON "auth_uuid"."deleted_at" IS NULL AND "auth_uuid"."uuid" = "usage"."auth_index"`).
			Joins(`LEFT JOIN "auth" AS "auth_by_index" ON "auth_uuid"."uuid" IS NULL AND "auth_by_index"."deleted_at" IS NULL AND "auth_by_index"."index" = "usage"."auth_index"`).
			Joins(`LEFT JOIN "auth" AS "auth_by_id" ON "auth_uuid"."uuid" IS NULL AND "auth_by_index"."uuid" IS NULL AND "auth_by_id"."deleted_at" IS NULL AND "auth_by_id"."id" = "usage"."auth_index"`)
	}
	return usageObservabilityApplyUsageFilters(scope, query).
		Select(usageObservabilityAggregateBaseSelect(groupBy))
}

func usageObservabilityAggregateP95Query(db *gorm.DB, query UsageObservabilityRecordQuery, groupBy string) *gorm.DB {
	base := usageObservabilityAggregateBaseQuery(db, query, groupBy)
	ranked := db.Table("(?) AS scoped_latency", base).
		Select(`
			scoped_latency.aggregate_id AS aggregate_id,
			scoped_latency.latency_ms AS latency_ms,
			COUNT(*) OVER (PARTITION BY scoped_latency.aggregate_id) AS latency_count,
			ROW_NUMBER() OVER (PARTITION BY scoped_latency.aggregate_id ORDER BY scoped_latency.latency_ms ASC) AS latency_rank`).
		Where("scoped_latency.latency_ms >= ?", 0)
	return db.Table("(?) AS ranked_latency", ranked).
		Select("ranked_latency.aggregate_id AS aggregate_id, MIN(ranked_latency.latency_ms) AS p95_latency_ms").
		Where("ranked_latency.latency_rank * 100 >= ranked_latency.latency_count * 95").
		Group("ranked_latency.aggregate_id")
}

func usageObservabilityAggregateP95Values(db *gorm.DB, query UsageObservabilityRecordQuery, groupBy string, aggregateIDs []string) (map[string]sql.NullFloat64, error) {
	if len(aggregateIDs) == 0 {
		return map[string]sql.NullFloat64{}, nil
	}
	base := usageObservabilityAggregateBaseQuery(db, query, groupBy)
	ranked := db.Table("(?) AS scoped_latency", base).
		Select(`
			scoped_latency.aggregate_id AS aggregate_id,
			scoped_latency.latency_ms AS latency_ms,
			COUNT(*) OVER (PARTITION BY scoped_latency.aggregate_id) AS latency_count,
			ROW_NUMBER() OVER (PARTITION BY scoped_latency.aggregate_id ORDER BY scoped_latency.latency_ms ASC) AS latency_rank`).
		Where("scoped_latency.latency_ms >= ?", 0).
		Where("scoped_latency.aggregate_id IN ?", aggregateIDs)

	var rows []struct {
		AggregateID  string          `gorm:"column:aggregate_id"`
		P95LatencyMS sql.NullFloat64 `gorm:"column:p95_latency_ms"`
	}
	if errScan := db.Table("(?) AS ranked_latency", ranked).
		Select("ranked_latency.aggregate_id AS aggregate_id, MIN(ranked_latency.latency_ms) AS p95_latency_ms").
		Where("ranked_latency.latency_rank * 100 >= ranked_latency.latency_count * 95").
		Group("ranked_latency.aggregate_id").
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	out := make(map[string]sql.NullFloat64, len(rows))
	for index := range rows {
		out[rows[index].AggregateID] = rows[index].P95LatencyMS
	}
	return out, nil
}

func usageObservabilityAggregateRowIDs(rows []usageObservabilityAggregateRow) []string {
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for index := range rows {
		id := strings.TrimSpace(rows[index].AggregateID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func usageObservabilityAggregateBaseSelect(groupBy string) string {
	idExpr, labelExpr := usageObservabilityAggregateSQLIdentity(groupBy)
	clientUserIDExpr := `NULL`
	usernameExpr := `''`
	clientAPIKeyIDExpr := `NULL`
	clientAPIKeyLabelExpr := `''`
	clientAPIKeyMaskedExpr := `''`
	credentialTypeExpr := `''`
	credentialIDExpr := `''`
	authIndexExpr := `''`
	credentialProviderExpr := `''`
	credentialLabelExpr := `''`
	authUUIDExpr := `''`
	authNextRetryAtExpr := `NULL`
	authStatusExpr := `''`
	authDisabledExpr := `FALSE`
	authUnavailableExpr := `FALSE`
	switch strings.TrimSpace(groupBy) {
	case "user", "client_key":
		clientUserIDExpr = `COALESCE("billing_charge"."user_id", "api_key"."user_id")`
		usernameExpr = `"user"."username"`
		clientAPIKeyIDExpr = `COALESCE("billing_charge"."api_key_id", "api_key"."id")`
		clientAPIKeyLabelExpr = `COALESCE("billing_charge"."api_key_label", CASE WHEN "api_key"."id" IS NULL THEN '' ELSE 'api-key-' || CAST("api_key"."id" AS TEXT) END)`
		clientAPIKeyMaskedExpr = `"billing_charge"."api_key_masked"`
	case "credential":
		credentialTypeExpr = `"usage"."auth_type"`
		credentialIDExpr = usageObservabilitySQLCredentialID()
		authIndexExpr = usageObservabilitySQLAuthIndex()
		credentialProviderExpr = usageObservabilitySQLAuthProvider()
		credentialLabelExpr = usageObservabilitySQLAuthLabel()
		authUUIDExpr = usageObservabilitySQLAuthUUID()
		authNextRetryAtExpr = usageObservabilitySQLAuthNextRetryAfter()
		authStatusExpr = usageObservabilitySQLAuthStatus()
		authDisabledExpr = usageObservabilitySQLAuthDisabled()
		authUnavailableExpr = usageObservabilitySQLAuthUnavailable()
	}
	return fmt.Sprintf(`
		%s AS aggregate_id,
		%s AS aggregate_label,
		%s AS metadata_user_id,
		%s AS metadata_username,
		%s AS metadata_api_key_id,
		%s AS metadata_api_key_label,
		%s AS metadata_api_key_masked,
		%s AS metadata_credential_type,
		%s AS metadata_credential_id,
		%s AS metadata_auth_index,
		%s AS metadata_credential_provider,
		%s AS metadata_credential_label,
		%s AS metadata_auth_uuid,
		%s AS metadata_auth_next_retry_at,
		%s AS metadata_auth_status,
		%s AS metadata_auth_disabled,
		%s AS metadata_auth_unavailable,
		"usage"."provider" AS metadata_provider,
		CASE WHEN "usage"."failed" THEN 'failed' ELSE 'success' END AS metadata_status,
		"usage"."timestamp" AS timestamp,
		"usage"."latency_ms" AS latency_ms,
		"usage"."failed" AS failed,
		"usage"."input_tokens" AS input_tokens,
		"usage"."output_tokens" AS output_tokens,
		"usage"."reasoning_tokens" AS reasoning_tokens,
		"usage"."cached_tokens" AS cached_tokens,
		"usage"."cache_read_tokens" AS cache_read_tokens,
		"usage"."cache_creation_tokens" AS cache_creation_tokens,
		"usage"."total_tokens" AS total_tokens,
		"billing_charge"."amount" AS amount`,
		idExpr,
		labelExpr,
		clientUserIDExpr,
		usernameExpr,
		clientAPIKeyIDExpr,
		clientAPIKeyLabelExpr,
		clientAPIKeyMaskedExpr,
		credentialTypeExpr,
		credentialIDExpr,
		authIndexExpr,
		credentialProviderExpr,
		credentialLabelExpr,
		authUUIDExpr,
		authNextRetryAtExpr,
		authStatusExpr,
		authDisabledExpr,
		authUnavailableExpr,
	)
}

func usageObservabilityAggregateSQLIdentity(groupBy string) (string, string) {
	switch groupBy {
	case "user":
		id := `COALESCE(CAST(COALESCE("billing_charge"."user_id", "api_key"."user_id") AS TEXT), 'unknown')`
		label := `COALESCE(NULLIF("user"."username", ''), ` + id + `)`
		return id, label
	case "client_key":
		id := `COALESCE(CAST(COALESCE("billing_charge"."api_key_id", "api_key"."id") AS TEXT), 'unknown')`
		label := `COALESCE(NULLIF(COALESCE("billing_charge"."api_key_label", CASE WHEN "api_key"."id" IS NULL THEN '' ELSE 'api-key-' || CAST("api_key"."id" AS TEXT) END), ''), NULLIF("billing_charge"."api_key_masked", ''), ` + id + `)`
		return id, label
	case "credential":
		id := `COALESCE(NULLIF(` + usageObservabilitySQLAuthUUID() + `, ''), NULLIF(` + usageObservabilitySQLAuthID() + `, ''), NULLIF(` + usageObservabilitySQLAuthIndex() + `, ''), NULLIF("usage"."auth_index", ''), 'unknown')`
		label := `COALESCE(NULLIF(` + usageObservabilitySQLAuthLabel() + `, ''), NULLIF(` + usageObservabilitySQLAuthIndex() + `, ''), ` + id + `)`
		return id, label
	case "model":
		id := `COALESCE(NULLIF("usage"."model", ''), 'unknown')`
		return id, id
	case "endpoint":
		id := `COALESCE(NULLIF("usage"."endpoint", ''), 'unknown')`
		return id, id
	case "home_ip":
		id := `COALESCE(NULLIF("usage"."home_ip", ''), 'unknown')`
		return id, id
	case "executor_type":
		id := `COALESCE(NULLIF("usage"."executor_type", ''), 'unknown')`
		return id, id
	case "status_code":
		id := `CASE WHEN "usage"."failed" THEN CAST("usage"."fail_status_code" AS TEXT) ELSE '200' END`
		return id, id
	default:
		id := `COALESCE(NULLIF("usage"."provider", ''), 'unknown')`
		return id, id
	}
}

func usageObservabilityAggregateSQLSelect(includeP95 bool) string {
	p95Select := "NULL AS p95_latency_ms"
	if includeP95 {
		p95Select = "MAX(p95.p95_latency_ms) AS p95_latency_ms"
	}
	return fmt.Sprintf(`
		scoped.aggregate_id AS aggregate_id,
		COALESCE(NULLIF(MAX(scoped.aggregate_label), ''), scoped.aggregate_id) AS aggregate_label,
		MAX(scoped.metadata_user_id) AS metadata_user_id,
		MAX(scoped.metadata_username) AS metadata_username,
		MAX(scoped.metadata_api_key_id) AS metadata_api_key_id,
		MAX(scoped.metadata_api_key_label) AS metadata_api_key_label,
		MAX(scoped.metadata_api_key_masked) AS metadata_api_key_masked,
		MAX(scoped.metadata_credential_type) AS metadata_credential_type,
		MAX(scoped.metadata_credential_id) AS metadata_credential_id,
		MAX(scoped.metadata_auth_index) AS metadata_auth_index,
		MAX(scoped.metadata_credential_provider) AS metadata_credential_provider,
		MAX(scoped.metadata_credential_label) AS metadata_credential_label,
		MAX(scoped.metadata_auth_uuid) AS metadata_auth_uuid,
		MAX(scoped.metadata_auth_next_retry_at) AS metadata_auth_next_retry_at,
		MAX(scoped.metadata_auth_status) AS metadata_auth_status,
		MAX(CASE WHEN scoped.metadata_auth_disabled THEN 1 ELSE 0 END) AS metadata_auth_disabled,
		MAX(CASE WHEN scoped.metadata_auth_unavailable THEN 1 ELSE 0 END) AS metadata_auth_unavailable,
		MAX(scoped.metadata_provider) AS metadata_provider,
		MAX(scoped.metadata_status) AS metadata_status,
		COUNT(*) AS request_count,
		SUM(CASE WHEN scoped.failed THEN 0 ELSE 1 END) AS success_count,
		SUM(CASE WHEN scoped.failed THEN 1 ELSE 0 END) AS failed_count,
		SUM(scoped.input_tokens) AS input_tokens,
		SUM(scoped.output_tokens) AS output_tokens,
		SUM(scoped.reasoning_tokens) AS reasoning_tokens,
		SUM(scoped.cached_tokens) AS cached_tokens,
		SUM(scoped.cache_read_tokens) AS cache_read_tokens,
		SUM(scoped.cache_creation_tokens) AS cache_creation_tokens,
		SUM(scoped.total_tokens) AS total_tokens,
		SUM(scoped.amount) AS total_amount,
		AVG(CASE WHEN scoped.latency_ms >= 0 THEN scoped.latency_ms END) AS avg_latency_ms,
		%s,
		MAX(scoped.timestamp) AS last_used_at`, p95Select)
}

func usageObservabilityAggregateSQLOrder(metric string, direction string) string {
	metricColumn := "request_count"
	switch strings.TrimSpace(metric) {
	case "total_tokens":
		metricColumn = "total_tokens"
	case "total_amount":
		metricColumn = "COALESCE(total_amount, 0)"
	case "failed_count":
		metricColumn = "failed_count"
	case "avg_latency_ms":
		metricColumn = "COALESCE(avg_latency_ms, 0)"
	case "p95_latency_ms":
		metricColumn = "COALESCE(p95_latency_ms, 0)"
	}
	sortDirection := "DESC"
	if strings.TrimSpace(direction) == "asc" {
		sortDirection = "ASC"
	}
	return metricColumn + " " + sortDirection + ", last_used_at DESC, aggregate_label ASC"
}

func usageObservabilitySQLAuthUUID() string {
	return `COALESCE("auth_uuid"."uuid", "auth_by_index"."uuid", "auth_by_id"."uuid")`
}

func usageObservabilitySQLCredentialID() string {
	return `COALESCE(NULLIF(` + usageObservabilitySQLAuthUUID() + `, ''), NULLIF(` + usageObservabilitySQLAuthID() + `, ''), NULLIF(` + usageObservabilitySQLAuthIndex() + `, ''), NULLIF("usage"."auth_index", ''))`
}

func usageObservabilitySQLAuthID() string {
	return `COALESCE("auth_uuid"."id", "auth_by_index"."id", "auth_by_id"."id")`
}

func usageObservabilitySQLAuthIndex() string {
	return `COALESCE("auth_uuid"."index", "auth_by_index"."index", "auth_by_id"."index")`
}

func usageObservabilitySQLAuthProvider() string {
	return `COALESCE("auth_uuid"."provider", "auth_by_index"."provider", "auth_by_id"."provider")`
}

func usageObservabilitySQLAuthLabel() string {
	return `COALESCE("auth_uuid"."label", "auth_by_index"."label", "auth_by_id"."label")`
}

func usageObservabilitySQLAuthStatus() string {
	return `COALESCE("auth_uuid"."status", "auth_by_index"."status", "auth_by_id"."status")`
}

func usageObservabilitySQLAuthDisabled() string {
	return `COALESCE("auth_uuid"."disabled", "auth_by_index"."disabled", "auth_by_id"."disabled", FALSE)`
}

func usageObservabilitySQLAuthUnavailable() string {
	return `COALESCE("auth_uuid"."unavailable", "auth_by_index"."unavailable", "auth_by_id"."unavailable", FALSE)`
}

func usageObservabilitySQLAuthNextRetryAfter() string {
	return `COALESCE("auth_uuid"."next_retry_after", "auth_by_index"."next_retry_after", "auth_by_id"."next_retry_after")`
}

func usageObservabilityRecordSelect() string {
	return `
		"usage"."id" AS usage_id,
		"usage"."timestamp" AS timestamp,
		"usage"."latency_ms" AS latency_ms,
		"usage"."ttft_ms" AS ttft_ms,
		"usage"."input_tokens" AS input_tokens,
		"usage"."output_tokens" AS output_tokens,
		"usage"."reasoning_tokens" AS reasoning_tokens,
		"usage"."cached_tokens" AS cached_tokens,
		"usage"."cache_read_tokens" AS cache_read_tokens,
		"usage"."cache_creation_tokens" AS cache_creation_tokens,
		"usage"."total_tokens" AS total_tokens,
		"usage"."failed" AS failed,
		"usage"."fail_status_code" AS fail_status_code,
		"usage"."fail_body" AS fail_body,
		"usage"."source" AS source,
		"usage"."provider" AS provider,
		"usage"."executor_type" AS executor_type,
		"usage"."model" AS model,
		"usage"."alias" AS alias,
		"usage"."effort" AS reasoning_effort,
		"usage"."service_tier" AS service_tier,
		"usage"."endpoint" AS endpoint,
		"usage"."auth_type" AS auth_type,
		"usage"."api_key" AS raw_api_key,
		"usage"."request_id" AS request_id,
		"usage"."upstream_request_id" AS upstream_request_id,
		"usage"."event_type" AS event_type,
		"usage"."upstream_status_code" AS upstream_status_code,
		"usage"."home_ip" AS home_ip,
		"usage"."home_port" AS home_port,
		"usage"."cpa_node_id" AS cpa_node_id,
		"usage"."cpa_ip" AS cpa_ip,
		"usage"."cpa_port" AS cpa_port,
		"usage"."cpa_label" AS cpa_label,
		"usage"."payload" AS payload_json,
		"usage"."auth_index" AS usage_auth_index,
		COALESCE("billing_charge"."api_key_id", "api_key"."id") AS client_api_key_id,
		COALESCE("billing_charge"."api_key_label", CASE WHEN "api_key"."id" IS NULL THEN '' ELSE 'api-key-' || CAST("api_key"."id" AS TEXT) END) AS client_api_key_label,
		"billing_charge"."api_key_masked" AS client_api_key_masked,
		COALESCE("billing_charge"."user_id", "api_key"."user_id") AS client_user_id,
		"user"."username" AS username,
		"billing_charge"."id" AS charge_id,
		"billing_charge"."amount" AS amount,
		"billing_charge"."matched_price_rule" AS matched_price_rule,
		"billing_charge"."balance_before" AS balance_before,
		"billing_charge"."balance_after" AS balance_after,
		COALESCE("auth_uuid"."uuid", "auth_by_index"."uuid", "auth_by_id"."uuid") AS auth_uuid,
		COALESCE("auth_uuid"."auth_json", "auth_by_index"."auth_json", "auth_by_id"."auth_json") AS auth_json,
		COALESCE("auth_uuid"."id", "auth_by_index"."id", "auth_by_id"."id") AS auth_id,
		COALESCE("auth_uuid"."index", "auth_by_index"."index", "auth_by_id"."index") AS auth_index,
		COALESCE("auth_uuid"."provider", "auth_by_index"."provider", "auth_by_id"."provider") AS auth_provider,
		COALESCE("auth_uuid"."label", "auth_by_index"."label", "auth_by_id"."label") AS auth_label,
		COALESCE("auth_uuid"."status", "auth_by_index"."status", "auth_by_id"."status") AS auth_status,
		COALESCE("auth_uuid"."disabled", "auth_by_index"."disabled", "auth_by_id"."disabled") AS auth_disabled,
		COALESCE("auth_uuid"."unavailable", "auth_by_index"."unavailable", "auth_by_id"."unavailable") AS auth_unavailable,
		COALESCE("auth_uuid"."next_refresh_after", "auth_by_index"."next_refresh_after", "auth_by_id"."next_refresh_after") AS auth_next_refresh_after,
		COALESCE("auth_uuid"."next_retry_after", "auth_by_index"."next_retry_after", "auth_by_id"."next_retry_after") AS auth_next_retry_after`
}

func usageObservabilityRecordScope(scope *gorm.DB, query UsageObservabilityRecordQuery) *gorm.DB {
	scope = scope.
		Joins(`LEFT JOIN "billing_charge" ON "billing_charge"."usage_id" = "usage"."id"`).
		Joins(`LEFT JOIN "api_key" ON "api_key"."api_key" = "usage"."api_key"`).
		Joins(`LEFT JOIN "user" ON "user"."id" = COALESCE("billing_charge"."user_id", "api_key"."user_id")`).
		Joins(`LEFT JOIN "auth" AS "auth_uuid" ON "auth_uuid"."deleted_at" IS NULL AND "auth_uuid"."uuid" = "usage"."auth_index"`).
		Joins(`LEFT JOIN "auth" AS "auth_by_index" ON "auth_uuid"."uuid" IS NULL AND "auth_by_index"."deleted_at" IS NULL AND "auth_by_index"."index" = "usage"."auth_index"`).
		Joins(`LEFT JOIN "auth" AS "auth_by_id" ON "auth_uuid"."uuid" IS NULL AND "auth_by_index"."uuid" IS NULL AND "auth_by_id"."deleted_at" IS NULL AND "auth_by_id"."id" = "usage"."auth_index"`)
	return usageObservabilityApplyRecordFilters(scope, query)
}

func usageObservabilityDistinctStrings(ctx context.Context, db *gorm.DB, query UsageObservabilityRecordQuery, expression string) ([]string, error) {
	type valueRow struct {
		Value string `gorm:"column:value"`
	}
	var rows []valueRow
	scope := usageObservabilityRecordScope(db.WithContext(ctx).Table("usage"), query).
		Where(expression + ` IS NOT NULL`).
		Where(`TRIM(` + expression + `) <> ''`).
		Select(`DISTINCT ` + expression + ` AS value`).
		Order("value ASC").
		Limit(500)
	if errFind := scope.Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	values := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		value := strings.TrimSpace(row.Value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}

func usageObservabilityDistinctStatusCodes(ctx context.Context, db *gorm.DB, query UsageObservabilityRecordQuery) ([]int, error) {
	type valueRow struct {
		Value int `gorm:"column:value"`
	}
	expression := `CASE WHEN "usage"."failed" THEN "usage"."fail_status_code" WHEN "usage"."upstream_status_code" > 0 THEN "usage"."upstream_status_code" ELSE 200 END`
	var rows []valueRow
	scope := usageObservabilityRecordScope(db.WithContext(ctx).Table("usage"), query).
		Where(expression + ` > 0`).
		Select(`DISTINCT ` + expression + ` AS value`).
		Order("value ASC").
		Limit(500)
	if errFind := scope.Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	values := make([]int, 0, len(rows))
	seen := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if row.Value <= 0 {
			continue
		}
		if _, exists := seen[row.Value]; exists {
			continue
		}
		seen[row.Value] = struct{}{}
		values = append(values, row.Value)
	}
	return values, nil
}

func usageObservabilityUsageBillingScope(scope *gorm.DB, query UsageObservabilityRecordQuery) *gorm.DB {
	return usageObservabilityNarrowUsageScope(scope.
		Joins(`LEFT JOIN "billing_charge" ON "billing_charge"."usage_id" = "usage"."id"`), query)
}

func usageObservabilityNarrowUsageScope(scope *gorm.DB, query UsageObservabilityRecordQuery) *gorm.DB {
	return usageObservabilityApplyUsageFilters(scope, query)
}

func usageObservabilityApplyRecordFilters(scope *gorm.DB, query UsageObservabilityRecordQuery) *gorm.DB {
	if query.From != nil {
		scope = scope.Where(`"usage"."timestamp" >= ?`, query.From.UTC())
	}
	if query.To != nil {
		scope = scope.Where(`"usage"."timestamp" <= ?`, query.To.UTC())
	}
	if provider := strings.ToLower(strings.TrimSpace(query.Provider)); provider != "" {
		scope = scope.Where(`LOWER("usage"."provider") = ?`, provider)
	}
	if model := strings.TrimSpace(query.Model); model != "" {
		scope = scope.Where(`"usage"."model" LIKE ?`, "%"+model+"%")
	}
	if homeIP := strings.TrimSpace(query.HomeIP); homeIP != "" {
		scope = scope.Where(`"usage"."home_ip" = ?`, homeIP)
	}
	if endpoint := strings.TrimSpace(query.Endpoint); endpoint != "" {
		scope = scope.Where(`"usage"."endpoint" LIKE ?`, "%"+endpoint+"%")
	}
	scope = usageObservabilityCredentialTypeScope(scope, query.CredentialType)
	scope = usageObservabilityStatusScope(scope, query.Status)
	scope = usageObservabilityStatusCodeScope(scope, query.StatusCode)
	if requestID := strings.TrimSpace(query.RequestID); requestID != "" {
		scope = scope.Where(`"usage"."request_id" = ?`, requestID)
	}
	if user := strings.TrimSpace(query.User); user != "" {
		matcher := "%" + user + "%"
		scope = scope.Where(`"user"."username" LIKE ? OR CAST("user"."id" AS TEXT) LIKE ?`, matcher, matcher)
	}
	if userID := normalizeOptionalUint(query.UserID); userID != nil {
		scope = scope.Where(`COALESCE("billing_charge"."user_id", "api_key"."user_id") = ?`, *userID)
	}
	if clientKey := strings.TrimSpace(query.ClientKey); clientKey != "" {
		matcher := "%" + clientKey + "%"
		scope = scope.Where(`"api_key"."api_key" LIKE ? OR "billing_charge"."api_key_masked" LIKE ? OR "billing_charge"."api_key_label" LIKE ? OR CAST("api_key"."id" AS TEXT) LIKE ?`, matcher, matcher, matcher, matcher)
	}
	if clientKeyID := normalizeOptionalUint(query.ClientKeyID); clientKeyID != nil {
		scope = scope.Where(`COALESCE("billing_charge"."api_key_id", "api_key"."id") = ?`, *clientKeyID)
	}
	if credentialID := strings.TrimSpace(query.CredentialID); credentialID != "" {
		scope = scope.Where(`"usage"."auth_index" = ? OR COALESCE("auth_uuid"."uuid", "auth_by_index"."uuid", "auth_by_id"."uuid") = ? OR COALESCE("auth_uuid"."id", "auth_by_index"."id", "auth_by_id"."id") = ? OR COALESCE("auth_uuid"."index", "auth_by_index"."index", "auth_by_id"."index") = ?`, credentialID, credentialID, credentialID, credentialID)
	}
	if authIndex := strings.TrimSpace(query.AuthIndex); authIndex != "" {
		scope = scope.Where(`"usage"."auth_index" = ? OR COALESCE("auth_uuid"."index", "auth_by_index"."index", "auth_by_id"."index") = ?`, authIndex, authIndex)
	}
	if executorType := strings.TrimSpace(query.ExecutorType); executorType != "" {
		scope = scope.Where(`"usage"."executor_type" = ?`, executorType)
	}
	scope = usageObservabilityEventTypeScope(scope, query.EventType)
	if cpaNode := strings.TrimSpace(query.CPANode); cpaNode != "" {
		matcher := "%" + strings.ToLower(cpaNode) + "%"
		scope = scope.Where(`LOWER("usage"."cpa_node_id") LIKE ? OR LOWER("usage"."cpa_ip") LIKE ? OR LOWER("usage"."cpa_label") LIKE ? OR CAST("usage"."cpa_port" AS TEXT) LIKE ?`, matcher, matcher, matcher, "%"+cpaNode+"%")
	}
	if query.MinLatencyMS != nil {
		scope = scope.Where(`"usage"."latency_ms" >= ?`, *query.MinLatencyMS)
	}
	if query.MaxLatencyMS != nil {
		scope = scope.Where(`"usage"."latency_ms" <= ?`, *query.MaxLatencyMS)
	}
	if query.MinAmount != nil {
		scope = scope.Where(`"billing_charge"."amount" >= ?`, *query.MinAmount)
	}
	if query.MaxAmount != nil {
		scope = scope.Where(`"billing_charge"."amount" <= ?`, *query.MaxAmount)
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		matcher := "%" + search + "%"
		scope = scope.Where(`(
			CAST("usage"."id" AS TEXT) LIKE ? OR
			"usage"."request_id" LIKE ? OR
			"usage"."provider" LIKE ? OR
			"usage"."model" LIKE ? OR
			"usage"."endpoint" LIKE ? OR
			"usage"."home_ip" LIKE ? OR
			"usage"."cpa_node_id" LIKE ? OR
			"usage"."cpa_ip" LIKE ? OR
			"usage"."cpa_label" LIKE ? OR
			"user"."username" LIKE ? OR
			"billing_charge"."api_key_masked" LIKE ? OR
			COALESCE("auth_uuid"."label", "auth_by_index"."label", "auth_by_id"."label") LIKE ? OR
			"usage"."auth_index" LIKE ?)`,
			matcher, matcher, matcher, matcher, matcher, matcher, matcher, matcher, matcher, matcher, matcher, matcher, matcher)
	}
	if search := strings.TrimSpace(query.RequestLogSearch); search != "" {
		matcher := "%" + search + "%"
		status := strings.ToLower(search)
		scope = scope.Where(`(
			"usage"."request_id" LIKE ? OR
			"usage"."provider" LIKE ? OR
			"usage"."model" LIKE ? OR
			(? = 'success' AND "usage"."failed" = ?) OR
			(? = 'failed' AND "usage"."failed" = ?))`,
			matcher, matcher, matcher, status, false, status, true)
	}
	return scope
}

func usageObservabilityApplyUsageFilters(scope *gorm.DB, query UsageObservabilityRecordQuery) *gorm.DB {
	if query.From != nil {
		scope = scope.Where(`"usage"."timestamp" >= ?`, query.From.UTC())
	}
	if query.To != nil {
		scope = scope.Where(`"usage"."timestamp" <= ?`, query.To.UTC())
	}
	if provider := strings.ToLower(strings.TrimSpace(query.Provider)); provider != "" {
		scope = scope.Where(`LOWER("usage"."provider") = ?`, provider)
	}
	if model := strings.TrimSpace(query.Model); model != "" {
		scope = scope.Where(`"usage"."model" LIKE ?`, "%"+model+"%")
	}
	if homeIP := strings.TrimSpace(query.HomeIP); homeIP != "" {
		scope = scope.Where(`"usage"."home_ip" = ?`, homeIP)
	}
	if endpoint := strings.TrimSpace(query.Endpoint); endpoint != "" {
		scope = scope.Where(`"usage"."endpoint" LIKE ?`, "%"+endpoint+"%")
	}
	scope = usageObservabilityCredentialTypeScope(scope, query.CredentialType)
	scope = usageObservabilityStatusScope(scope, query.Status)
	scope = usageObservabilityStatusCodeScope(scope, query.StatusCode)
	if requestID := strings.TrimSpace(query.RequestID); requestID != "" {
		scope = scope.Where(`"usage"."request_id" = ?`, requestID)
	}
	if executorType := strings.TrimSpace(query.ExecutorType); executorType != "" {
		scope = scope.Where(`"usage"."executor_type" = ?`, executorType)
	}
	scope = usageObservabilityEventTypeScope(scope, query.EventType)
	if cpaNode := strings.TrimSpace(query.CPANode); cpaNode != "" {
		matcher := "%" + strings.ToLower(cpaNode) + "%"
		scope = scope.Where(`LOWER("usage"."cpa_node_id") LIKE ? OR LOWER("usage"."cpa_ip") LIKE ? OR LOWER("usage"."cpa_label") LIKE ? OR CAST("usage"."cpa_port" AS TEXT) LIKE ?`, matcher, matcher, matcher, "%"+cpaNode+"%")
	}
	if query.MinLatencyMS != nil {
		scope = scope.Where(`"usage"."latency_ms" >= ?`, *query.MinLatencyMS)
	}
	if query.MaxLatencyMS != nil {
		scope = scope.Where(`"usage"."latency_ms" <= ?`, *query.MaxLatencyMS)
	}
	if search := strings.TrimSpace(query.RequestLogSearch); search != "" {
		matcher := "%" + search + "%"
		status := strings.ToLower(search)
		scope = scope.Where(`(
			"usage"."request_id" LIKE ? OR
			"usage"."provider" LIKE ? OR
			"usage"."model" LIKE ? OR
			(? = 'success' AND "usage"."failed" = ?) OR
			(? = 'failed' AND "usage"."failed" = ?))`,
			matcher, matcher, matcher, status, false, status, true)
	}
	return scope
}

func usageObservabilityCredentialTypeScope(scope *gorm.DB, credentialType string) *gorm.DB {
	if strings.TrimSpace(credentialType) == "" {
		return scope
	}
	value := normalizeUsageObservabilityCredentialType(credentialType)
	switch value {
	case "provider_api_key":
		return scope.Where(`LOWER(REPLACE("usage"."auth_type", '-', '_')) IN (?, ?, ?)`, "provider_api_key", "api_key", "apikey")
	case "oauth":
		return scope.Where(`LOWER("usage"."auth_type") = ?`, "oauth")
	case "file_auth":
		return scope.Where(`LOWER(REPLACE("usage"."auth_type", '-', '_')) = ?`, "file_auth")
	case "vertex":
		return scope.Where(`LOWER("usage"."auth_type") = ?`, "vertex")
	case "unknown":
		return scope.Where(`TRIM(COALESCE("usage"."auth_type", '')) = '' OR LOWER(REPLACE("usage"."auth_type", '-', '_')) NOT IN (?, ?, ?, ?, ?, ?)`, "provider_api_key", "api_key", "apikey", "oauth", "file_auth", "vertex")
	default:
		return scope
	}
}

func usageObservabilityStatusScope(scope *gorm.DB, status string) *gorm.DB {
	value := strings.ToLower(strings.TrimSpace(status))
	switch value {
	case "":
		return scope
	case "success", "succeeded", "ok":
		return scope.Where(`"usage"."failed" = ?`, false)
	case "failed", "failure", "error":
		return scope.Where(`"usage"."failed" = ?`, true)
	default:
		statusCode, errAtoi := strconv.Atoi(value)
		if errAtoi == nil {
			if statusCode >= 200 && statusCode < 400 {
				return scope.Where(`"usage"."failed" = ?`, false)
			}
			return scope.Where(`"usage"."fail_status_code" = ?`, statusCode)
		}
		return scope
	}
}

func usageObservabilityStatusCodeScope(scope *gorm.DB, statusCode *int) *gorm.DB {
	if statusCode == nil || *statusCode <= 0 {
		return scope
	}
	if *statusCode >= 200 && *statusCode < 400 {
		return scope.Where(`"usage"."failed" = ?`, false)
	}
	return scope.Where(`"usage"."fail_status_code" = ?`, *statusCode)
}

func usageObservabilityEventTypeScope(scope *gorm.DB, eventType string) *gorm.DB {
	value := strings.ToLower(strings.TrimSpace(eventType))
	if value == "all" {
		value = ""
	}
	if value == "" {
		return scope
	}
	normalized := normalizeUsageObservabilityEventType(value)
	if normalized == "" {
		normalized = value
	}
	return scope.Where(`LOWER("usage"."event_type") = ?`, normalized)
}

func usageObservabilityEventType(row *usageObservabilityRecordRow, payload map[string]any) string {
	if row != nil {
		if normalized := normalizeUsageObservabilityEventType(row.EventType); normalized != "" {
			return normalized
		}
	}
	raw := firstStringFromPayload(payload, "event_type", "event.type", "type")
	if normalized := normalizeUsageObservabilityEventType(raw); normalized != "" {
		return normalized
	}
	if row != nil {
		return normalizeUsageObservabilityEndpointEventType(row.Endpoint)
	}
	return "completion"
}

func firstNonZeroInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func normalizeUsageObservabilityEventType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, ".", "_")
	switch normalized {
	case "":
		return ""
	case "embedding", "embeddings":
		return "embedding"
	case "response", "responses":
		return "response"
	case "message", "messages":
		return "message"
	case "stream", "streaming", "delta", "chunk":
		return "stream"
	case "completion", "completions", "chat_completion", "chat_completions":
		return "completion"
	default:
		if strings.Contains(normalized, "embedding") {
			return "embedding"
		}
		if strings.Contains(normalized, "response") {
			return "response"
		}
		if strings.Contains(normalized, "message") {
			return "message"
		}
		if strings.Contains(normalized, "stream") || strings.Contains(normalized, "delta") || strings.Contains(normalized, "chunk") {
			return "stream"
		}
		if strings.Contains(normalized, "completion") || strings.Contains(normalized, "chat") {
			return "completion"
		}
		return normalized
	}
}

func normalizeUsageObservabilityEndpointEventType(endpoint string) string {
	normalized := strings.ToLower(strings.TrimSpace(endpoint))
	switch {
	case strings.Contains(normalized, "embedding"):
		return "embedding"
	case strings.Contains(normalized, "response"):
		return "response"
	case strings.Contains(normalized, "message"):
		return "message"
	case strings.Contains(normalized, "completion"), strings.Contains(normalized, "chat"):
		return "completion"
	default:
		return "completion"
	}
}

func usageObservabilityRecordOrder(sortValue string) string {
	switch strings.TrimSpace(sortValue) {
	case "timestamp_asc":
		return `"usage"."timestamp" ASC, "usage"."id" ASC`
	case "tokens_desc":
		return `"usage"."total_tokens" DESC, "usage"."timestamp" DESC, "usage"."id" DESC`
	case "tokens_asc":
		return `"usage"."total_tokens" ASC, "usage"."timestamp" ASC, "usage"."id" ASC`
	case "cost_desc":
		return `COALESCE("billing_charge"."amount", 0) DESC, "usage"."timestamp" DESC, "usage"."id" DESC`
	case "cost_asc":
		return `COALESCE("billing_charge"."amount", 0) ASC, "usage"."timestamp" ASC, "usage"."id" ASC`
	case "latency_desc":
		return `"usage"."latency_ms" DESC, "usage"."timestamp" DESC, "usage"."id" DESC`
	case "latency_asc":
		return `"usage"."latency_ms" ASC, "usage"."timestamp" ASC, "usage"."id" ASC`
	case "failed_first":
		return `"usage"."failed" DESC, "usage"."timestamp" DESC, "usage"."id" DESC`
	default:
		return `"usage"."timestamp" DESC, "usage"."id" DESC`
	}
}

func usageObservabilityRecordFromRow(row *usageObservabilityRecordRow) UsageObservabilityRecord {
	if row == nil {
		return UsageObservabilityRecord{}
	}
	payload := usageObservabilityPayloadMap(row.PayloadJSON)
	record := UsageObservabilityRecord{
		ID:                 strconv.FormatUint(uint64(row.UsageID), 10),
		UsageID:            row.UsageID,
		Timestamp:          row.Timestamp.UTC(),
		RequestID:          strings.TrimSpace(row.RequestID),
		UpstreamRequestID:  firstNonEmptyUsageObservabilityString(row.UpstreamRequestID, firstStringFromPayload(payload, "upstream_request_id", "upstream.request_id", "response.request_id", "response.id")),
		EventType:          usageObservabilityEventType(row, payload),
		Status:             usageObservabilityRecordStatus(row.Failed),
		Failed:             row.Failed,
		StatusCode:         usageObservabilityStatusCode(row.Failed, row.FailStatusCode),
		UpstreamStatusCode: firstNonZeroInt(row.UpstreamStatusCode, int(firstIntFromPayload(payload, "upstream_status_code", "upstream.status_code", "response.status_code"))),
		Source:             strings.TrimSpace(row.Source),
		Provider:           strings.TrimSpace(row.Provider),
		Model:              strings.TrimSpace(row.Model),
		OriginalModel:      firstNonEmptyUsageObservabilityString(row.Alias, row.Model),
		Endpoint:           strings.TrimSpace(row.Endpoint),
		ServiceTier:        strings.TrimSpace(row.ServiceTier),
		ReasoningEffort:    strings.TrimSpace(row.ReasoningEffort),
		ExecutorType:       strings.TrimSpace(row.ExecutorType),
		Tokens: UsageObservabilityTokens{
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			ReasoningTokens:     row.ReasoningTokens,
			CachedTokens:        row.CachedTokens,
			CacheReadTokens:     row.CacheReadTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			TotalTokens:         row.TotalTokens,
		},
		Performance: usageObservabilityPerformance(row),
		Client:      usageObservabilityClient(row, payload),
		Credential:  usageObservabilityCredential(row),
		Billing:     usageObservabilityBilling(row),
		Runtime: UsageObservabilityRuntime{
			HomeIP:              strings.TrimSpace(row.HomeIP),
			HomePort:            row.HomePort,
			CPANodeID:           firstNonEmptyUsageObservabilityString(row.CPANodeID, firstStringFromPayload(payload, "cpa_node_id", "cpa.node_id", "node_id")),
			CPAIP:               firstNonEmptyUsageObservabilityString(row.CPAIP, firstStringFromPayload(payload, "cpa_ip", "cpa.ip")),
			CPAPort:             firstNonZeroInt(row.CPAPort, int(firstIntFromPayload(payload, "cpa_port", "cpa.port"))),
			CPALabel:            firstNonEmptyUsageObservabilityString(row.CPALabel, firstStringFromPayload(payload, "cpa_label", "cpa.label")),
			RequestLogAvailable: strings.TrimSpace(row.RequestID) != "",
			LogHomeIPRequired:   true,
		},
	}
	if row.Failed {
		record.Error = &UsageObservabilityError{
			StatusCode:         row.FailStatusCode,
			UpstreamStatusCode: record.UpstreamStatusCode,
			Reason:             firstStringFromPayload(payload, "fail.reason", "error.reason", "error.type", "error.code"),
			Message:            usageObservabilityErrorMessage(row.FailBody),
			BodyPreview:        usageObservabilityBodyPreview(row.FailBody),
		}
	}
	return record
}

func usageObservabilityAggregateItemsFromRows(rows []usageObservabilityRecordRow, groupBy string) []UsageObservabilityAggregateItem {
	accumulators := make(map[string]*usageObservabilityAggregateAccumulator)
	for index := range rows {
		key, item := usageObservabilityAggregateIdentity(&rows[index], groupBy)
		accumulator := accumulators[key]
		if accumulator == nil {
			accumulator = &usageObservabilityAggregateAccumulator{Item: item}
			accumulators[key] = accumulator
		}
		accumulator.add(&rows[index])
	}
	items := make([]UsageObservabilityAggregateItem, 0, len(accumulators))
	for _, accumulator := range accumulators {
		items = append(items, accumulator.result())
	}
	return items
}

func usageObservabilityAggregateItemFromRow(row *usageObservabilityAggregateRow, groupBy string) UsageObservabilityAggregateItem {
	if row == nil {
		return UsageObservabilityAggregateItem{}
	}
	item := UsageObservabilityAggregateItem{
		ID:                  strings.TrimSpace(row.AggregateID),
		Label:               firstNonEmptyUsageObservabilityString(row.AggregateLabel, row.AggregateID),
		Metadata:            usageObservabilityAggregateMetadataFromRow(row, groupBy),
		RequestCount:        row.RequestCount,
		SuccessCount:        row.SuccessCount,
		FailedCount:         row.FailedCount,
		InputTokens:         row.InputTokens,
		OutputTokens:        row.OutputTokens,
		ReasoningTokens:     row.ReasoningTokens,
		CachedTokens:        row.CachedTokens,
		CacheReadTokens:     row.CacheReadTokens,
		CacheCreationTokens: row.CacheCreationTokens,
		TotalTokens:         row.TotalTokens,
	}
	if item.RequestCount > 0 {
		item.SuccessRate = float64(item.SuccessCount) / float64(item.RequestCount)
		item.ErrorRate = float64(item.FailedCount) / float64(item.RequestCount)
	}
	if item.TotalTokens > 0 {
		cacheTokens := item.CachedTokens + item.CacheReadTokens + item.CacheCreationTokens
		item.CacheRate = float64(cacheTokens) / float64(item.TotalTokens)
	}
	if row.TotalAmount.Valid {
		amount := row.TotalAmount.Float64
		item.TotalAmount = &amount
		item.Currency = UsageObservabilityCurrencyCredits
	}
	if row.AvgLatencyMS.Valid {
		avg := row.AvgLatencyMS.Float64
		item.AvgLatencyMS = &avg
	}
	if row.P95LatencyMS.Valid {
		p95 := row.P95LatencyMS.Float64
		item.P95LatencyMS = &p95
	}
	if parsedLastUsedAt, ok := usageObservabilityAggregateTime(row.LastUsedAt); ok {
		lastUsedAt := parsedLastUsedAt.UTC()
		item.LastUsedAt = &lastUsedAt
	}
	return item
}

func usageObservabilityOptionalAggregateTime(raw string) *time.Time {
	parsed, ok := usageObservabilityAggregateTime(raw)
	if !ok {
		return nil
	}
	value := parsed.UTC()
	return &value
}

func usageObservabilityAggregateTime(raw string) (time.Time, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		parsed, errParse := time.Parse(layout, value)
		if errParse == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func usageObservabilityAggregateMetadataFromRow(row *usageObservabilityAggregateRow, groupBy string) map[string]any {
	if row == nil {
		return map[string]any{}
	}
	switch groupBy {
	case "user":
		return map[string]any{
			"user_id":  optionalSQLInt64MapValue(row.MetadataUserID),
			"username": strings.TrimSpace(row.MetadataUsername),
		}
	case "client_key":
		return map[string]any{
			"api_key_id":     optionalSQLInt64MapValue(row.MetadataAPIKeyID),
			"api_key_label":  strings.TrimSpace(row.MetadataAPIKeyLabel),
			"api_key_masked": strings.TrimSpace(row.MetadataAPIKeyMasked),
			"user_id":        optionalSQLInt64MapValue(row.MetadataUserID),
		}
	case "credential":
		nextRetryAt := usageObservabilityOptionalTimeMapValue(usageObservabilityOptionalAggregateTime(row.MetadataAuthNextRetryAt))
		return map[string]any{
			"credential_type": normalizeUsageObservabilityCredentialType(row.MetadataCredentialType),
			"provider":        firstNonEmptyUsageObservabilityString(row.MetadataCredentialProvider, row.MetadataProvider),
			"source":          usageObservabilityAggregateCredentialSource(row),
			"status":          usageObservabilityAggregateCredentialStatus(row),
			"auth_index":      firstNonEmptyUsageObservabilityString(row.MetadataAuthIndex, row.MetadataCredentialID),
			"next_retry_at":   nextRetryAt,
		}
	case "model":
		return map[string]any{
			"provider": strings.TrimSpace(row.MetadataProvider),
		}
	case "status_code":
		return map[string]any{
			"status": strings.TrimSpace(row.MetadataStatus),
		}
	default:
		return map[string]any{}
	}
}

func usageObservabilityAggregateCredentialSource(row *usageObservabilityAggregateRow) string {
	if row == nil {
		return ""
	}
	if strings.TrimSpace(row.MetadataAuthUUID) != "" {
		return "db"
	}
	if strings.TrimSpace(row.MetadataAuthIndex) != "" || strings.TrimSpace(row.MetadataCredentialID) != "" {
		return "usage"
	}
	return ""
}

func usageObservabilityAggregateCredentialStatus(row *usageObservabilityAggregateRow) string {
	if row == nil {
		return ""
	}
	if row.MetadataAuthDisabled > 0 {
		return "disabled"
	}
	if row.MetadataAuthUnavailable > 0 {
		return "unavailable"
	}
	if status := strings.TrimSpace(row.MetadataAuthStatus); status != "" {
		return status
	}
	if strings.TrimSpace(row.MetadataAuthIndex) != "" || strings.TrimSpace(row.MetadataCredentialID) != "" {
		return "unknown"
	}
	return ""
}

func usageObservabilityAggregateIdentity(row *usageObservabilityRecordRow, groupBy string) (string, UsageObservabilityAggregateItem) {
	switch groupBy {
	case "user":
		id := usageObservabilityUintID(row.ClientUserID)
		label := firstNonEmptyUsageObservabilityString(row.Username, id, "Unknown user")
		return id, UsageObservabilityAggregateItem{
			ID:    id,
			Label: label,
			Metadata: map[string]any{
				"user_id":  optionalUintMapValue(row.ClientUserID),
				"username": strings.TrimSpace(row.Username),
			},
		}
	case "client_key":
		id := usageObservabilityUintID(row.ClientAPIKeyID)
		label := firstNonEmptyUsageObservabilityString(row.ClientAPIKeyLabel, row.ClientAPIKeyMasked, id, "Unknown client key")
		return id, UsageObservabilityAggregateItem{
			ID:    id,
			Label: label,
			Metadata: map[string]any{
				"api_key_id":     optionalUintMapValue(row.ClientAPIKeyID),
				"api_key_label":  strings.TrimSpace(row.ClientAPIKeyLabel),
				"api_key_masked": firstNonEmptyUsageObservabilityString(row.ClientAPIKeyMasked, maskBillingAPIKey(row.RawAPIKey)),
				"user_id":        optionalUintMapValue(row.ClientUserID),
			},
		}
	case "credential":
		credential := usageObservabilityCredential(row)
		id := firstNonEmptyUsageObservabilityString(credential.CredentialID, "unknown")
		label := firstNonEmptyUsageObservabilityString(credential.Label, credential.AuthIndex, id, "Unknown credential")
		return id, UsageObservabilityAggregateItem{
			ID:    id,
			Label: label,
			Metadata: map[string]any{
				"credential_type": credential.CredentialType,
				"provider":        credential.Provider,
				"source":          credential.Source,
				"status":          credential.Status,
				"auth_index":      credential.AuthIndex,
				"next_retry_at":   usageObservabilityOptionalTimeMapValue(credential.NextRetryAt),
			},
		}
	case "model":
		id := firstNonEmptyUsageObservabilityString(row.Model, "unknown")
		return id, UsageObservabilityAggregateItem{
			ID:    id,
			Label: id,
			Metadata: map[string]any{
				"provider": strings.TrimSpace(row.Provider),
			},
		}
	case "endpoint":
		id := firstNonEmptyUsageObservabilityString(row.Endpoint, "unknown")
		return id, UsageObservabilityAggregateItem{ID: id, Label: id, Metadata: map[string]any{}}
	case "home_ip":
		id := firstNonEmptyUsageObservabilityString(row.HomeIP, "unknown")
		return id, UsageObservabilityAggregateItem{ID: id, Label: id, Metadata: map[string]any{}}
	case "executor_type":
		id := firstNonEmptyUsageObservabilityString(row.ExecutorType, "unknown")
		return id, UsageObservabilityAggregateItem{ID: id, Label: id, Metadata: map[string]any{}}
	case "status_code":
		statusCode := usageObservabilityStatusCode(row.Failed, row.FailStatusCode)
		id := strconv.Itoa(statusCode)
		return id, UsageObservabilityAggregateItem{
			ID:    id,
			Label: id,
			Metadata: map[string]any{
				"status": usageObservabilityRecordStatus(row.Failed),
			},
		}
	default:
		id := firstNonEmptyUsageObservabilityString(row.Provider, "unknown")
		return id, UsageObservabilityAggregateItem{ID: id, Label: id, Metadata: map[string]any{}}
	}
}

func usageObservabilityTotals(rows []usageObservabilityRecordRow) UsageObservabilityTotals {
	accumulator := usageObservabilityAggregateAccumulator{}
	userIDs := map[string]struct{}{}
	clientKeyIDs := map[string]struct{}{}
	credentialIDs := map[string]struct{}{}
	models := map[string]struct{}{}
	ttftValues := make([]int64, 0, len(rows))

	for index := range rows {
		row := &rows[index]
		accumulator.add(row)
		if id := usageObservabilityUintID(row.ClientUserID); id != "unknown" {
			userIDs[id] = struct{}{}
		}
		if id := usageObservabilityUintID(row.ClientAPIKeyID); id != "unknown" {
			clientKeyIDs[id] = struct{}{}
		}
		credential := usageObservabilityCredential(row)
		if id := firstNonEmptyUsageObservabilityString(credential.CredentialID, "unknown"); id != "unknown" {
			credentialIDs[id] = struct{}{}
		}
		if model := strings.TrimSpace(row.Model); model != "" {
			models[model] = struct{}{}
		}
		if row.TTFTMS > 0 {
			ttftValues = append(ttftValues, row.TTFTMS)
		}
	}

	item := accumulator.result()
	totals := UsageObservabilityTotals{
		RequestCount:          item.RequestCount,
		SuccessCount:          item.SuccessCount,
		FailedCount:           item.FailedCount,
		ErrorRate:             item.ErrorRate,
		SuccessRate:           item.SuccessRate,
		InputTokens:           item.InputTokens,
		OutputTokens:          item.OutputTokens,
		ReasoningTokens:       item.ReasoningTokens,
		CachedTokens:          item.CachedTokens,
		CacheReadTokens:       item.CacheReadTokens,
		CacheCreationTokens:   item.CacheCreationTokens,
		TotalTokens:           item.TotalTokens,
		TotalAmount:           item.TotalAmount,
		Currency:              item.Currency,
		AvgLatencyMS:          item.AvgLatencyMS,
		P95LatencyMS:          item.P95LatencyMS,
		ActiveUserCount:       int64(len(userIDs)),
		ActiveClientKeyCount:  int64(len(clientKeyIDs)),
		ActiveCredentialCount: int64(len(credentialIDs)),
		ActiveModelCount:      int64(len(models)),
	}
	if len(accumulator.LatencyValues) > 0 {
		p50 := usageObservabilityPercentile(accumulator.LatencyValues, 0.50)
		totals.P50LatencyMS = &p50
	}
	if len(ttftValues) > 0 {
		var ttftTotal int64
		for _, value := range ttftValues {
			ttftTotal += value
		}
		avgTTFT := float64(ttftTotal) / float64(len(ttftValues))
		totals.AvgTTFTMS = &avgTTFT
	}
	if totals.TotalAmount != nil && totals.TotalTokens > 0 {
		blended := *totals.TotalAmount * 1000000 / float64(totals.TotalTokens)
		totals.BlendedCostPer1M = &blended
	}
	return totals
}

func usageObservabilityLive(rows []usageObservabilityRecordRow, windowSeconds int) UsageObservabilityLiveSummary {
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second)
	windowRows := make([]usageObservabilityRecordRow, 0)
	for _, row := range rows {
		if !row.Timestamp.Before(cutoff) {
			windowRows = append(windowRows, row)
		}
	}
	totals := usageObservabilityTotals(windowRows)
	live := UsageObservabilityLiveSummary{
		WindowSeconds: windowSeconds,
		ErrorRate:     totals.ErrorRate,
		SuccessRate:   totals.SuccessRate,
		P50LatencyMS:  totals.P50LatencyMS,
		P95LatencyMS:  totals.P95LatencyMS,
	}
	if windowSeconds > 0 {
		minutes := float64(windowSeconds) / 60
		live.RPM = float64(totals.RequestCount) / minutes
		live.TPM = float64(totals.TotalTokens) / minutes
	}
	return live
}

func usageObservabilityTrend(rows []usageObservabilityRecordRow, interval string, location *time.Location) []UsageObservabilityTrendPoint {
	type bucketAccumulator struct {
		start       time.Time
		end         time.Time
		accumulator usageObservabilityAggregateAccumulator
	}
	if location == nil {
		location = time.UTC
	}
	buckets := map[int64]*bucketAccumulator{}
	for index := range rows {
		row := &rows[index]
		start, end := usageObservabilityBucketRange(row.Timestamp.UTC(), interval, location)
		key := start.UnixNano()
		bucket := buckets[key]
		if bucket == nil {
			bucket = &bucketAccumulator{start: start, end: end}
			buckets[key] = bucket
		}
		bucket.accumulator.add(row)
	}
	points := make([]UsageObservabilityTrendPoint, 0, len(buckets))
	for _, bucket := range buckets {
		item := bucket.accumulator.result()
		points = append(points, UsageObservabilityTrendPoint{
			BucketStart:         bucket.start,
			BucketEnd:           bucket.end,
			RequestCount:        item.RequestCount,
			SuccessCount:        item.SuccessCount,
			FailedCount:         item.FailedCount,
			InputTokens:         item.InputTokens,
			OutputTokens:        item.OutputTokens,
			ReasoningTokens:     item.ReasoningTokens,
			CachedTokens:        item.CachedTokens,
			CacheReadTokens:     item.CacheReadTokens,
			CacheCreationTokens: item.CacheCreationTokens,
			TotalTokens:         item.TotalTokens,
			TotalAmount:         item.TotalAmount,
			AvgLatencyMS:        item.AvgLatencyMS,
			P95LatencyMS:        item.P95LatencyMS,
		})
	}
	sort.Slice(points, func(i int, j int) bool {
		return points[i].BucketStart.Before(points[j].BucketStart)
	})
	return points
}

func usageObservabilityActivity(trend []UsageObservabilityTrendPoint) []UsageObservabilityActivityPoint {
	activity := make([]UsageObservabilityActivityPoint, 0, len(trend))
	for _, point := range trend {
		successRate := 0.0
		errorRate := 0.0
		if point.RequestCount > 0 {
			successRate = float64(point.SuccessCount) / float64(point.RequestCount)
			errorRate = float64(point.FailedCount) / float64(point.RequestCount)
		}
		activity = append(activity, UsageObservabilityActivityPoint{
			BucketStart:  point.BucketStart,
			BucketEnd:    point.BucketEnd,
			RequestCount: point.RequestCount,
			SuccessCount: point.SuccessCount,
			FailedCount:  point.FailedCount,
			SuccessRate:  successRate,
			ErrorRate:    errorRate,
			Status:       usageObservabilityHealthStatus(errorRate, point.RequestCount),
		})
	}
	return activity
}

func usageObservabilityTop(rows []usageObservabilityRecordRow) UsageObservabilityTopGroups {
	return UsageObservabilityTopGroups{
		Users:       usageObservabilityTopItems(rows, "user", "request_count", "desc", 10),
		ClientKeys:  usageObservabilityTopItems(rows, "client_key", "request_count", "desc", 10),
		Credentials: usageObservabilityTopItems(rows, "credential", "request_count", "desc", 10),
		Providers:   usageObservabilityTopItems(rows, "provider", "request_count", "desc", 10),
		Models:      usageObservabilityTopItems(rows, "model", "request_count", "desc", 10),
		Endpoints:   usageObservabilityTopItems(rows, "endpoint", "request_count", "desc", 10),
		Errors:      usageObservabilityTopItems(usageObservabilityFailedRows(rows), "status_code", "failed_count", "desc", 10),
	}
}

func usageObservabilityTopItems(rows []usageObservabilityRecordRow, groupBy string, metric string, direction string, limit int) []UsageObservabilityAggregateItem {
	items := usageObservabilityAggregateItemsFromRows(rows, groupBy)
	sortUsageObservabilityAggregateItems(items, metric, direction)
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func usageObservabilityFailedRows(rows []usageObservabilityRecordRow) []usageObservabilityRecordRow {
	out := make([]usageObservabilityRecordRow, 0)
	for _, row := range rows {
		if row.Failed {
			out = append(out, row)
		}
	}
	return out
}

func (a *usageObservabilityAggregateAccumulator) add(row *usageObservabilityRecordRow) {
	a.Item.RequestCount++
	if row.Failed {
		a.Item.FailedCount++
	} else {
		a.Item.SuccessCount++
	}
	a.Item.InputTokens += row.InputTokens
	a.Item.OutputTokens += row.OutputTokens
	a.Item.ReasoningTokens += row.ReasoningTokens
	a.Item.CachedTokens += row.CachedTokens
	a.Item.CacheReadTokens += row.CacheReadTokens
	a.Item.CacheCreationTokens += row.CacheCreationTokens
	a.Item.TotalTokens += row.TotalTokens
	if row.Amount.Valid {
		a.AmountTotal += row.Amount.Float64
		a.AmountValid = true
	}
	if row.LatencyMS >= 0 {
		a.LatencyValues = append(a.LatencyValues, row.LatencyMS)
		a.LatencyTotal += row.LatencyMS
	}
	if a.Item.LastUsedAt == nil || row.Timestamp.After(*a.Item.LastUsedAt) {
		timestamp := row.Timestamp.UTC()
		a.Item.LastUsedAt = &timestamp
	}
}

func (a *usageObservabilityAggregateAccumulator) result() UsageObservabilityAggregateItem {
	item := a.Item
	if item.RequestCount > 0 {
		item.SuccessRate = float64(item.SuccessCount) / float64(item.RequestCount)
		item.ErrorRate = float64(item.FailedCount) / float64(item.RequestCount)
	}
	if item.TotalTokens > 0 {
		cacheTokens := item.CachedTokens + item.CacheReadTokens + item.CacheCreationTokens
		item.CacheRate = float64(cacheTokens) / float64(item.TotalTokens)
	}
	if a.AmountValid {
		amount := a.AmountTotal
		item.TotalAmount = &amount
		item.Currency = UsageObservabilityCurrencyCredits
	}
	if len(a.LatencyValues) > 0 {
		avg := float64(a.LatencyTotal) / float64(len(a.LatencyValues))
		p95 := usageObservabilityPercentile(a.LatencyValues, 0.95)
		item.AvgLatencyMS = &avg
		item.P95LatencyMS = &p95
	}
	return item
}

func sortUsageObservabilityAggregateItems(items []UsageObservabilityAggregateItem, metric string, direction string) {
	metric = strings.TrimSpace(metric)
	if metric == "" {
		metric = "request_count"
	}
	desc := strings.TrimSpace(direction) != "asc"
	sort.SliceStable(items, func(i int, j int) bool {
		left := usageObservabilityAggregateMetricValue(&items[i], metric)
		right := usageObservabilityAggregateMetricValue(&items[j], metric)
		if left == right {
			if items[i].LastUsedAt != nil && items[j].LastUsedAt != nil && !items[i].LastUsedAt.Equal(*items[j].LastUsedAt) {
				return items[i].LastUsedAt.After(*items[j].LastUsedAt)
			}
			return items[i].Label < items[j].Label
		}
		if desc {
			return left > right
		}
		return left < right
	})
}

func usageObservabilityAggregateMetricValue(item *UsageObservabilityAggregateItem, metric string) float64 {
	if item == nil {
		return 0
	}
	switch metric {
	case "total_tokens":
		return float64(item.TotalTokens)
	case "total_amount":
		if item.TotalAmount == nil {
			return 0
		}
		return *item.TotalAmount
	case "failed_count":
		return float64(item.FailedCount)
	case "avg_latency_ms":
		if item.AvgLatencyMS == nil {
			return 0
		}
		return *item.AvgLatencyMS
	case "p95_latency_ms":
		if item.P95LatencyMS == nil {
			return 0
		}
		return *item.P95LatencyMS
	default:
		return float64(item.RequestCount)
	}
}

func usageObservabilityPercentile(values []int64, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sortedValues := append([]int64(nil), values...)
	sort.Slice(sortedValues, func(i int, j int) bool {
		return sortedValues[i] < sortedValues[j]
	})
	index := int(math.Ceil(percentile*float64(len(sortedValues)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sortedValues) {
		index = len(sortedValues) - 1
	}
	return float64(sortedValues[index])
}

func usageObservabilityOverviewInterval(requested string, from *time.Time, to *time.Time, rows []usageObservabilityRecordRow) string {
	start, end := usageObservabilityRangeBounds(from, to, rows)
	return usageObservabilityOverviewIntervalFromTimes(requested, start, end)
}

func usageObservabilityOverviewIntervalFromBounds(requested string, from *time.Time, to *time.Time, bounds usageObservabilityOverviewBounds) string {
	start, end := usageObservabilityBoundsTimes(bounds)
	if from != nil {
		start = from.UTC()
	}
	if to != nil {
		end = to.UTC()
	}
	return usageObservabilityOverviewIntervalFromTimes(requested, start, end)
}

func usageObservabilityOverviewIntervalFromTimes(requested string, start time.Time, end time.Time) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "minute", "hour", "day", "week":
		return strings.ToLower(strings.TrimSpace(requested))
	}
	if start.IsZero() || end.IsZero() {
		return "day"
	}
	if end.Sub(start) <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

func usageObservabilityRangeTime(value *time.Time, rows []usageObservabilityRecordRow, first bool) string {
	start, end := usageObservabilityRangeBounds(nil, nil, rows)
	if value != nil {
		if first {
			start = value.UTC()
		} else {
			end = value.UTC()
		}
	}
	if first {
		if start.IsZero() {
			return ""
		}
		return start.UTC().Format(time.RFC3339Nano)
	}
	if end.IsZero() {
		return ""
	}
	return end.UTC().Format(time.RFC3339Nano)
}

func usageObservabilityRangeTimeFromBounds(value *time.Time, bounds usageObservabilityOverviewBounds, first bool) string {
	start, end := usageObservabilityBoundsTimes(bounds)
	if value != nil {
		if first {
			start = value.UTC()
		} else {
			end = value.UTC()
		}
	}
	if first {
		if start.IsZero() {
			return ""
		}
		return start.UTC().Format(time.RFC3339Nano)
	}
	if end.IsZero() {
		return ""
	}
	return end.UTC().Format(time.RFC3339Nano)
}

func usageObservabilityBoundsTimes(bounds usageObservabilityOverviewBounds) (time.Time, time.Time) {
	var start time.Time
	var end time.Time
	if bounds.MinTimestamp.Valid {
		if parsedStart, ok := usageObservabilityAggregateTime(bounds.MinTimestamp.String); ok {
			start = parsedStart.UTC()
		}
	}
	if bounds.MaxTimestamp.Valid {
		if parsedEnd, ok := usageObservabilityAggregateTime(bounds.MaxTimestamp.String); ok {
			end = parsedEnd.UTC()
		}
	}
	return start, end
}

func usageObservabilityLocation(timezone string) *time.Location {
	value := strings.TrimSpace(timezone)
	if value == "" {
		return time.UTC
	}
	location, errLoad := time.LoadLocation(value)
	if errLoad != nil {
		return time.UTC
	}
	return location
}

func usageObservabilityRangeBounds(from *time.Time, to *time.Time, rows []usageObservabilityRecordRow) (time.Time, time.Time) {
	var start time.Time
	var end time.Time
	if from != nil {
		start = from.UTC()
	}
	if to != nil {
		end = to.UTC()
	}
	for _, row := range rows {
		timestamp := row.Timestamp.UTC()
		if start.IsZero() || timestamp.Before(start) {
			start = timestamp
		}
		if end.IsZero() || timestamp.After(end) {
			end = timestamp
		}
	}
	return start, end
}

func usageObservabilityBucketRange(timestamp time.Time, interval string, location *time.Location) (time.Time, time.Time) {
	if location == nil {
		location = time.UTC
	}
	local := timestamp.UTC().In(location)
	var start time.Time
	var end time.Time
	switch interval {
	case "minute":
		start = time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, location)
		end = start.Add(time.Minute - time.Nanosecond)
	case "hour":
		start = time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location)
		end = start.Add(time.Hour - time.Nanosecond)
	case "week":
		daysSinceMonday := (int(local.Weekday()) + 6) % 7
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -daysSinceMonday)
		end = start.AddDate(0, 0, 7).Add(-time.Nanosecond)
	default:
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		end = start.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	return start.UTC(), end.UTC()
}

func usageObservabilityHealthStatus(errorRate float64, requestCount int64) string {
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

func usageObservabilityPerformance(row *usageObservabilityRecordRow) UsageObservabilityPerformance {
	performance := UsageObservabilityPerformance{LatencyMS: row.LatencyMS}
	if row.TTFTMS > 0 {
		ttft := row.TTFTMS
		performance.TTFTMS = &ttft
	}
	if row.LatencyMS > 0 && row.OutputTokens > 0 {
		tps := float64(row.OutputTokens) * 1000 / float64(row.LatencyMS)
		performance.TPS = &tps
	}
	return performance
}

func usageObservabilityClient(row *usageObservabilityRecordRow, payload map[string]any) UsageObservabilityClient {
	masked := strings.TrimSpace(row.ClientAPIKeyMasked)
	if masked == "" {
		masked = maskBillingAPIKey(row.RawAPIKey)
	}
	label := strings.TrimSpace(row.ClientAPIKeyLabel)
	if label == "" && row.ClientAPIKeyID != nil && *row.ClientAPIKeyID != 0 {
		label = fmt.Sprintf("api-key-%d", *row.ClientAPIKeyID)
	}
	return UsageObservabilityClient{
		APIKeyID:     normalizeOptionalUint(row.ClientAPIKeyID),
		APIKeyLabel:  label,
		APIKeyMasked: masked,
		UserID:       normalizeOptionalUint(row.ClientUserID),
		Username:     strings.TrimSpace(row.Username),
		ClientIP:     firstStringFromPayload(payload, "client_ip", "client.ip", "remote_addr"),
	}
}

func usageObservabilityCredential(row *usageObservabilityRecordRow) UsageObservabilityCredential {
	credentialID := firstNonEmptyUsageObservabilityString(row.AuthUUID, row.AuthID, row.AuthIndex, row.UsageAuthIndex)
	authIndex := firstNonEmptyUsageObservabilityString(row.AuthIndex, row.UsageAuthIndex)
	source := ""
	if strings.TrimSpace(row.AuthUUID) != "" {
		source = "db"
	} else if strings.TrimSpace(row.UsageAuthIndex) != "" {
		source = "usage"
	}
	return UsageObservabilityCredential{
		CredentialType: normalizeUsageObservabilityCredentialType(row.AuthType),
		CredentialID:   credentialID,
		AuthIndex:      authIndex,
		Provider:       firstNonEmptyUsageObservabilityString(row.AuthProvider, row.Provider),
		Label:          strings.TrimSpace(row.AuthLabel),
		Source:         source,
		Status:         usageObservabilityCredentialStatus(row),
		NextRetryAt:    usageObservabilityAuthNextRetryAt(row),
	}
}

func usageObservabilityAuthNextRetryAt(row *usageObservabilityRecordRow) *time.Time {
	if row == nil {
		return nil
	}
	if row.AuthNextRetryAfter.Valid {
		nextRetryAt := row.AuthNextRetryAfter.Time.UTC()
		return &nextRetryAt
	}
	return usageObservabilityAuthJSONNextRetryAt(string(row.AuthJSON))
}

func usageObservabilityAuthJSONNextRetryAt(raw string) *time.Time {
	authJSON := strings.TrimSpace(raw)
	if authJSON == "" {
		return nil
	}
	var payload struct {
		NextRetryAfter time.Time `json:"next_retry_after"`
		ModelStates    map[string]struct {
			NextRetryAfter time.Time `json:"next_retry_after"`
		} `json:"model_states"`
	}
	if errUnmarshal := json.Unmarshal([]byte(authJSON), &payload); errUnmarshal != nil {
		return nil
	}
	var earliest time.Time
	remember := func(value time.Time) {
		if value.IsZero() {
			return
		}
		value = value.UTC()
		if earliest.IsZero() || value.Before(earliest) {
			earliest = value
		}
	}
	remember(payload.NextRetryAfter)
	for _, state := range payload.ModelStates {
		remember(state.NextRetryAfter)
	}
	if earliest.IsZero() {
		return nil
	}
	return &earliest
}

func usageObservabilityCredentialStatus(row *usageObservabilityRecordRow) string {
	if row.AuthDisabled {
		return "disabled"
	}
	if row.AuthUnavailable {
		return "unavailable"
	}
	if status := strings.TrimSpace(row.AuthStatus); status != "" {
		return status
	}
	if strings.TrimSpace(row.UsageAuthIndex) != "" {
		return "unknown"
	}
	return ""
}

func usageObservabilityBilling(row *usageObservabilityRecordRow) UsageObservabilityBilling {
	billing := UsageObservabilityBilling{
		ChargeID:         strings.TrimSpace(row.ChargeID),
		BillingBasis:     UsageObservabilityBillingBasisUnknown,
		MatchedPriceRule: strings.TrimSpace(row.MatchedPriceRule),
	}
	if row.Amount.Valid {
		amount := row.Amount.Float64
		billing.Amount = &amount
		billing.Currency = UsageObservabilityCurrencyCredits
		billing.BillingBasis = UsageObservabilityBillingBasisCharge
	}
	if row.BalanceBefore.Valid {
		balanceBefore := row.BalanceBefore.Float64
		billing.BalanceBefore = &balanceBefore
	}
	if row.BalanceAfter.Valid {
		balanceAfter := row.BalanceAfter.Float64
		billing.BalanceAfter = &balanceAfter
	}
	return billing
}

func usageObservabilityRecordStatus(failed bool) string {
	if failed {
		return "failed"
	}
	return "success"
}

func usageObservabilityStatusCode(failed bool, failStatusCode int) int {
	if failed {
		return failStatusCode
	}
	return httpStatusOK
}

func normalizeUsageObservabilityPagination(limit int, offset int, defaultLimit int, maxLimit int) (int, int) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func normalizeOptionalUint(value *uint) *uint {
	if value == nil || *value == 0 {
		return nil
	}
	return value
}

func usageObservabilityUintID(value *uint) string {
	if value == nil || *value == 0 {
		return "unknown"
	}
	return strconv.FormatUint(uint64(*value), 10)
}

func optionalUintMapValue(value *uint) any {
	if value == nil || *value == 0 {
		return nil
	}
	return *value
}

func optionalSQLInt64MapValue(value sql.NullInt64) any {
	if !value.Valid || value.Int64 <= 0 {
		return nil
	}
	return value.Int64
}

func usageObservabilityOptionalTimeMapValue(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func firstNonEmptyUsageObservabilityString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeUsageObservabilityCredentialType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "":
		return "unknown"
	case "provider_api_key", "api_key", "apikey":
		return "provider_api_key"
	case "oauth":
		return "oauth"
	case "file_auth":
		return "file_auth"
	case "vertex":
		return "vertex"
	default:
		return "unknown"
	}
}

func usageObservabilityErrorMessage(body string) string {
	preview := usageObservabilityBodyPreview(body)
	if len(preview) > 200 {
		return strings.TrimSpace(preview[:200])
	}
	return preview
}

func usageObservabilityBodyPreview(body string) string {
	preview := strings.TrimSpace(body)
	if preview == "" {
		return ""
	}
	preview = usageObservabilitySecretPattern.ReplaceAllString(preview, "[redacted]")
	preview = strings.NewReplacer(
		"Authorization", "[redacted]",
		"authorization", "[redacted]",
		"access_token", "[redacted]",
		"refresh_token", "[redacted]",
		"api_key", "[redacted]",
		"cookie", "[redacted]",
		"Cookie", "[redacted]",
	).Replace(preview)
	if len(preview) > 500 {
		return preview[:500]
	}
	return preview
}

func usageObservabilityPayloadMap(payload JSONB) map[string]any {
	payloadText := strings.TrimSpace(string(payload))
	if payloadText == "" {
		return nil
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payloadText), &out); errUnmarshal != nil {
		return nil
	}
	return out
}

func usageObservabilityPayloadSummary(payload map[string]any) UsageObservabilityPayloadSummary {
	summary := UsageObservabilityPayloadSummary{}
	if method := firstStringFromPayload(payload, "method", "type", "endpoint"); method != "" {
		summary.Method = &method
	}
	if stream, ok := firstBoolFromPayload(payload, "stream"); ok {
		summary.Stream = &stream
	}
	if messages := firstArrayLengthFromPayload(payload, "messages", "body.messages", "request.messages"); messages != nil {
		summary.MessageCount = messages
	}
	toolCount := usageObservabilityToolCount(payload)
	summary.ToolCount = &toolCount
	return summary
}

func firstStringFromPayload(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		if value, ok := payloadPathValue(payload, path).(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func firstIntFromPayload(payload map[string]any, paths ...string) int64 {
	for _, path := range paths {
		value := payloadPathValue(payload, path)
		switch typed := value.(type) {
		case float64:
			return int64(typed)
		case int64:
			return typed
		case int:
			return int64(typed)
		case json.Number:
			parsed, errParse := typed.Int64()
			if errParse == nil {
				return parsed
			}
		case string:
			parsed, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
			if errParse == nil {
				return parsed
			}
		}
	}
	return 0
}

func firstBoolFromPayload(payload map[string]any, paths ...string) (bool, bool) {
	for _, path := range paths {
		if value, ok := payloadPathValue(payload, path).(bool); ok {
			return value, true
		}
	}
	return false, false
}

func firstArrayLengthFromPayload(payload map[string]any, paths ...string) *int {
	for _, path := range paths {
		if value, ok := payloadPathValue(payload, path).([]any); ok {
			count := len(value)
			return &count
		}
	}
	return nil
}

func usageObservabilityToolCount(payload map[string]any) int {
	count := 0
	for _, path := range []string{"tools", "body.tools", "request.tools"} {
		if tools, ok := payloadPathValue(payload, path).([]any); ok {
			count += len(tools)
		}
	}
	for _, path := range []string{"messages", "body.messages", "request.messages"} {
		messages, ok := payloadPathValue(payload, path).([]any)
		if !ok {
			continue
		}
		for _, message := range messages {
			messageMap, ok := message.(map[string]any)
			if !ok {
				continue
			}
			if toolCalls, ok := messageMap["tool_calls"].([]any); ok {
				count += len(toolCalls)
			}
		}
	}
	return count
}

func payloadPathValue(payload map[string]any, path string) any {
	var current any = payload
	for _, segment := range strings.Split(path, ".") {
		currentMap, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = currentMap[segment]
	}
	return current
}

const httpStatusOK = 200
