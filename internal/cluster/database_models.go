package cluster

import (
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// currentDatabaseVersion is shared by the live schema migration gate and the
// portable snapshot format. Increment it for every required startup migration
// or snapshot format change, and retain mappings for prior snapshot formats.
const currentDatabaseVersion = 19

// databaseModel describes one managed Home database table.
type databaseModel struct {
	name          string
	newRecord     func() any
	newBatch      func() any
	orderBy       []string
	autoIncrement bool
	restore       bool
}

func newDatabaseModel[T any](name string, orderBy []string, autoIncrement bool, restore bool) databaseModel {
	return databaseModel{
		name:          name,
		newRecord:     func() any { return new(T) },
		newBatch:      func() any { return new([]T) },
		orderBy:       append([]string(nil), orderBy...),
		autoIncrement: autoIncrement,
		restore:       restore,
	}
}

type databaseSnapshotV1CPANodeRecord struct {
	HomeIP      string    `gorm:"column:home_ip;primaryKey"`
	HomePort    int       `gorm:"column:home_port;primaryKey"`
	NodeKey     string    `gorm:"column:node_key;primaryKey;size:256"`
	NodeID      string    `gorm:"column:node_id"`
	ClientIP    string    `gorm:"column:client_ip"`
	ClientCount int       `gorm:"column:client_count"`
	ConnectedAt time.Time `gorm:"column:connected_at"`
	LastSeenAt  time.Time `gorm:"column:last_seen_at"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (databaseSnapshotV1CPANodeRecord) TableName() string {
	return "cpa_node"
}

// databaseSnapshotV3APIKeyRecord freezes the API key shape used by snapshot
// formats v1 through v3, before display names were added.
type databaseSnapshotV3APIKeyRecord struct {
	ID          uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_api_key_active_order,priority:2"`
	APIKey      string         `gorm:"column:api_key;not null;uniqueIndex"`
	UserID      *uint          `gorm:"column:user_id;index;index:idx_api_key_user_active,priority:1"`
	Channels    JSONB          `gorm:"column:channels"`
	ModelGroups JSONB          `gorm:"column:model_groups"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_api_key_active_order,priority:1;index:idx_api_key_user_active,priority:2"`
}

func (databaseSnapshotV3APIKeyRecord) TableName() string {
	return "api_key"
}

// databaseSnapshotV4AuthRecord freezes the auth shape used by snapshot
// formats v1 through v4, before quota activity identity generations were added.
type databaseSnapshotV4AuthRecord struct {
	UUID             string          `gorm:"column:uuid;primaryKey"`
	AuthJSON         JSONB           `gorm:"column:auth_json;not null"`
	Version          int64           `gorm:"column:version;not null;default:1"`
	ID               string          `gorm:"column:id;index:idx_auth_active_order,priority:2"`
	Index            string          `gorm:"column:index;index:idx_auth_index_active,priority:1"`
	Provider         string          `gorm:"column:provider"`
	Label            string          `gorm:"column:label"`
	Prefix           string          `gorm:"column:prefix"`
	Status           coreauth.Status `gorm:"column:status"`
	Disabled         bool            `gorm:"column:disabled"`
	Unavailable      bool            `gorm:"column:unavailable"`
	BaseURL          string          `gorm:"column:base_url"`
	APIKeyHash       string          `gorm:"column:api_key_hash"`
	CompatName       string          `gorm:"column:compat_name"`
	ProviderKey      string          `gorm:"column:provider_key"`
	ModelsHash       string          `gorm:"column:models_hash"`
	CreatedAt        time.Time       `gorm:"column:created_at"`
	UpdatedAt        time.Time       `gorm:"column:updated_at"`
	LastRefreshedAt  *time.Time      `gorm:"column:last_refreshed_at"`
	NextRefreshAfter *time.Time      `gorm:"column:next_refresh_after"`
	NextRetryAfter   *time.Time      `gorm:"column:next_retry_after"`
	DeletedAt        gorm.DeletedAt  `gorm:"column:deleted_at;index;index:idx_auth_active_order,priority:1;index:idx_auth_index_active,priority:2"`
}

func (databaseSnapshotV4AuthRecord) TableName() string {
	return "auth"
}

// databaseSnapshotV4UsageRecord freezes the usage shape used by snapshot
// formats v1 through v4, before quota activity identity fields were added.
type databaseSnapshotV4UsageRecord struct {
	ID              uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_usage_time_order,priority:2"`
	Timestamp       time.Time `gorm:"column:timestamp;not null;index:idx_usage_timestamp;index:idx_usage_time_order,priority:1,sort:desc;index:idx_usage_source_time,priority:2,sort:desc;index:idx_usage_auth_time,priority:2,sort:desc;index:idx_usage_failed_time,priority:2,sort:desc;index:idx_usage_failed_status_time,priority:3,sort:desc;index:idx_usage_provider_model_time,priority:3,sort:desc;index:idx_usage_provider_time,priority:2,sort:desc;index:idx_usage_endpoint_time,priority:2,sort:desc;index:idx_usage_home_time,priority:2,sort:desc;index:idx_usage_auth_type_time,priority:2,sort:desc"`
	LatencyMS       int64     `gorm:"column:latency_ms;not null;default:0"`
	TTFTMS          int64     `gorm:"column:ttft_ms;not null;default:0"`
	Source          string    `gorm:"column:source;index:idx_usage_source;index:idx_usage_source_time,priority:1"`
	AuthIndex       string    `gorm:"column:auth_index;index:idx_usage_auth_index;index:idx_usage_auth_time,priority:1"`
	InputTokens     int64     `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens    int64     `gorm:"column:output_tokens;not null;default:0"`
	ReasoningTokens int64     `gorm:"column:reasoning_tokens;not null;default:0"`
	CachedTokens    int64     `gorm:"column:cached_tokens;not null;default:0"`
	CacheReadTokens int64     `gorm:"column:cache_read_tokens;not null;default:0"`
	// CacheReadTokensPresent distinguishes a canonical zero from a legacy CPA
	// payload that did not know the cache_read_tokens field.
	CacheReadTokensPresent     bool      `gorm:"column:cache_read_tokens_present;not null;default:false"`
	CacheCreationTokens        int64     `gorm:"column:cache_creation_tokens;not null;default:0"`
	TotalTokens                int64     `gorm:"column:total_tokens;not null;default:0"`
	TokenAccountingVersion     int       `gorm:"column:token_accounting_version;not null;default:0;index:idx_usage_token_accounting_version"`
	TokenAccountingQuality     string    `gorm:"column:token_accounting_quality;not null;default:'unclassified'"`
	AccountingTotalTokens      int64     `gorm:"column:accounting_total_tokens;not null;default:0"`
	AccountingInputTokens      int64     `gorm:"column:accounting_input_tokens;not null;default:0"`
	UncachedInputTokens        int64     `gorm:"column:uncached_input_tokens;not null;default:0"`
	AccountingCacheReadTokens  int64     `gorm:"column:accounting_cache_read_tokens;not null;default:0"`
	AccountingCacheWriteTokens int64     `gorm:"column:accounting_cache_write_tokens;not null;default:0"`
	AccountingOutputTokens     int64     `gorm:"column:accounting_output_tokens;not null;default:0"`
	NonReasoningOutputTokens   int64     `gorm:"column:non_reasoning_output_tokens;not null;default:0"`
	AccountingReasoningTokens  int64     `gorm:"column:accounting_reasoning_tokens;not null;default:0"`
	UnclassifiedTokens         int64     `gorm:"column:unclassified_tokens;not null;default:0"`
	Failed                     bool      `gorm:"column:failed;not null;default:false;index:idx_usage_failed;index:idx_usage_failed_time,priority:1;index:idx_usage_failed_status_time,priority:1"`
	FailStatusCode             int       `gorm:"column:fail_status_code;not null;default:0;index:idx_usage_failed_status_time,priority:2"`
	FailBody                   string    `gorm:"column:fail_body;type:text"`
	Provider                   string    `gorm:"column:provider;index:idx_usage_provider_model,priority:1;index:idx_usage_provider_model_time,priority:1;index:idx_usage_provider_time,priority:1"`
	ExecutorType               string    `gorm:"column:executor_type"`
	Model                      string    `gorm:"column:model;index:idx_usage_provider_model,priority:2;index:idx_usage_provider_model_time,priority:2"`
	Alias                      string    `gorm:"column:alias"`
	Effort                     string    `gorm:"column:effort"`
	ServiceTier                string    `gorm:"column:service_tier"`
	ResponseServiceTier        string    `gorm:"column:response_service_tier"`
	Endpoint                   string    `gorm:"column:endpoint;index:idx_usage_endpoint;index:idx_usage_endpoint_time,priority:1"`
	AuthType                   string    `gorm:"column:auth_type;index:idx_usage_auth_type_time,priority:1"`
	APIKey                     string    `gorm:"column:api_key;index:idx_usage_api_key"`
	RequestID                  string    `gorm:"column:request_id;index:idx_usage_request_id"`
	UpstreamRequestID          string    `gorm:"column:upstream_request_id;index:idx_usage_upstream_request_id"`
	EventType                  string    `gorm:"column:event_type;index:idx_usage_event_type;index:idx_usage_event_time,priority:1"`
	UpstreamStatusCode         int       `gorm:"column:upstream_status_code;not null;default:0;index:idx_usage_upstream_status_code"`
	HomeIP                     string    `gorm:"column:home_ip;index:idx_usage_home_ip;index:idx_usage_home_time,priority:1;index:idx_usage_home_port_time,priority:1"`
	HomePort                   int       `gorm:"column:home_port;not null;default:0;index:idx_usage_home_port_time,priority:2"`
	CPANodeID                  string    `gorm:"column:cpa_node_id;index:idx_usage_cpa_node_id;index:idx_usage_cpa_node_time,priority:1"`
	CPAIP                      string    `gorm:"column:cpa_ip;index:idx_usage_cpa_ip"`
	CPAPort                    int       `gorm:"column:cpa_port;not null;default:0"`
	CPALabel                   string    `gorm:"column:cpa_label;index:idx_usage_cpa_label"`
	TokensJSON                 JSONB     `gorm:"column:tokens"`
	FailJSON                   JSONB     `gorm:"column:fail"`
	PayloadJSON                JSONB     `gorm:"column:payload;not null"`
	CreatedAt                  time.Time `gorm:"column:created_at;not null"`
}

func (databaseSnapshotV4UsageRecord) TableName() string {
	return "usage"
}

// databaseSnapshotV5UsageRecord freezes the usage shape used by snapshot
// format v5, before session hierarchy identity fields were added.
type databaseSnapshotV5UsageRecord struct {
	ID                         uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_usage_time_order,priority:2"`
	Timestamp                  time.Time `gorm:"column:timestamp;not null;index:idx_usage_timestamp;index:idx_usage_time_order,priority:1,sort:desc;index:idx_usage_source_time,priority:2,sort:desc;index:idx_usage_auth_time,priority:2,sort:desc;index:idx_usage_failed_time,priority:2,sort:desc;index:idx_usage_failed_status_time,priority:3,sort:desc;index:idx_usage_provider_model_time,priority:3,sort:desc;index:idx_usage_provider_time,priority:2,sort:desc;index:idx_usage_endpoint_time,priority:2,sort:desc;index:idx_usage_home_time,priority:2,sort:desc;index:idx_usage_auth_type_time,priority:2,sort:desc"`
	LatencyMS                  int64     `gorm:"column:latency_ms;not null;default:0"`
	TTFTMS                     int64     `gorm:"column:ttft_ms;not null;default:0"`
	Source                     string    `gorm:"column:source;index:idx_usage_source;index:idx_usage_source_time,priority:1"`
	AuthIndex                  string    `gorm:"column:auth_index;index:idx_usage_auth_index;index:idx_usage_auth_time,priority:1"`
	QuotaCredentialID          string    `gorm:"column:quota_credential_id;not null;default:''"`
	QuotaIdentityVersion       int64     `gorm:"column:quota_identity_version;not null;default:0"`
	QuotaIdentityKey           string    `gorm:"column:quota_identity_key;not null;default:'';size:128"`
	InputTokens                int64     `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens               int64     `gorm:"column:output_tokens;not null;default:0"`
	ReasoningTokens            int64     `gorm:"column:reasoning_tokens;not null;default:0"`
	CachedTokens               int64     `gorm:"column:cached_tokens;not null;default:0"`
	CacheReadTokens            int64     `gorm:"column:cache_read_tokens;not null;default:0"`
	CacheReadTokensPresent     bool      `gorm:"column:cache_read_tokens_present;not null;default:false"`
	CacheCreationTokens        int64     `gorm:"column:cache_creation_tokens;not null;default:0"`
	TotalTokens                int64     `gorm:"column:total_tokens;not null;default:0"`
	TokenAccountingVersion     int       `gorm:"column:token_accounting_version;not null;default:0;index:idx_usage_token_accounting_version"`
	TokenAccountingQuality     string    `gorm:"column:token_accounting_quality;not null;default:'unclassified'"`
	AccountingTotalTokens      int64     `gorm:"column:accounting_total_tokens;not null;default:0"`
	AccountingInputTokens      int64     `gorm:"column:accounting_input_tokens;not null;default:0"`
	UncachedInputTokens        int64     `gorm:"column:uncached_input_tokens;not null;default:0"`
	AccountingCacheReadTokens  int64     `gorm:"column:accounting_cache_read_tokens;not null;default:0"`
	AccountingCacheWriteTokens int64     `gorm:"column:accounting_cache_write_tokens;not null;default:0"`
	AccountingOutputTokens     int64     `gorm:"column:accounting_output_tokens;not null;default:0"`
	NonReasoningOutputTokens   int64     `gorm:"column:non_reasoning_output_tokens;not null;default:0"`
	AccountingReasoningTokens  int64     `gorm:"column:accounting_reasoning_tokens;not null;default:0"`
	UnclassifiedTokens         int64     `gorm:"column:unclassified_tokens;not null;default:0"`
	Failed                     bool      `gorm:"column:failed;not null;default:false;index:idx_usage_failed;index:idx_usage_failed_time,priority:1;index:idx_usage_failed_status_time,priority:1"`
	FailStatusCode             int       `gorm:"column:fail_status_code;not null;default:0;index:idx_usage_failed_status_time,priority:2"`
	FailBody                   string    `gorm:"column:fail_body;type:text"`
	Provider                   string    `gorm:"column:provider;index:idx_usage_provider_model,priority:1;index:idx_usage_provider_model_time,priority:1;index:idx_usage_provider_time,priority:1"`
	ExecutorType               string    `gorm:"column:executor_type"`
	Model                      string    `gorm:"column:model;index:idx_usage_provider_model,priority:2;index:idx_usage_provider_model_time,priority:2"`
	Alias                      string    `gorm:"column:alias"`
	Effort                     string    `gorm:"column:effort"`
	ServiceTier                string    `gorm:"column:service_tier"`
	ResponseServiceTier        string    `gorm:"column:response_service_tier"`
	Endpoint                   string    `gorm:"column:endpoint;index:idx_usage_endpoint;index:idx_usage_endpoint_time,priority:1"`
	AuthType                   string    `gorm:"column:auth_type;index:idx_usage_auth_type_time,priority:1"`
	APIKey                     string    `gorm:"column:api_key;index:idx_usage_api_key"`
	RequestID                  string    `gorm:"column:request_id;index:idx_usage_request_id"`
	UpstreamRequestID          string    `gorm:"column:upstream_request_id;index:idx_usage_upstream_request_id"`
	EventType                  string    `gorm:"column:event_type;index:idx_usage_event_type;index:idx_usage_event_time,priority:1"`
	UpstreamStatusCode         int       `gorm:"column:upstream_status_code;not null;default:0;index:idx_usage_upstream_status_code"`
	HomeIP                     string    `gorm:"column:home_ip;index:idx_usage_home_ip;index:idx_usage_home_time,priority:1;index:idx_usage_home_port_time,priority:1"`
	HomePort                   int       `gorm:"column:home_port;not null;default:0;index:idx_usage_home_port_time,priority:2"`
	CPANodeID                  string    `gorm:"column:cpa_node_id;index:idx_usage_cpa_node_id;index:idx_usage_cpa_node_time,priority:1"`
	CPAIP                      string    `gorm:"column:cpa_ip;index:idx_usage_cpa_ip"`
	CPAPort                    int       `gorm:"column:cpa_port;not null;default:0"`
	CPALabel                   string    `gorm:"column:cpa_label;index:idx_usage_cpa_label"`
	TokensJSON                 JSONB     `gorm:"column:tokens"`
	FailJSON                   JSONB     `gorm:"column:fail"`
	PayloadJSON                JSONB     `gorm:"column:payload;not null"`
	CreatedAt                  time.Time `gorm:"column:created_at;not null"`
}

func (databaseSnapshotV5UsageRecord) TableName() string {
	return "usage"
}

// databaseSnapshotV17UsageRecord freezes the usage shape used by snapshot
// formats v6 through v17, before the dedupe_key column was added.
type databaseSnapshotV17UsageRecord struct {
	ID                         uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_usage_time_order,priority:2"`
	Timestamp                  time.Time `gorm:"column:timestamp;not null;index:idx_usage_timestamp;index:idx_usage_time_order,priority:1,sort:desc;index:idx_usage_source_time,priority:2,sort:desc;index:idx_usage_auth_time,priority:2,sort:desc;index:idx_usage_failed_time,priority:2,sort:desc;index:idx_usage_failed_status_time,priority:3,sort:desc;index:idx_usage_provider_model_time,priority:3,sort:desc;index:idx_usage_provider_time,priority:2,sort:desc;index:idx_usage_endpoint_time,priority:2,sort:desc;index:idx_usage_home_time,priority:2,sort:desc;index:idx_usage_auth_type_time,priority:2,sort:desc;index:idx_usage_session_time,priority:2,sort:desc;index:idx_usage_parent_session_time,priority:2,sort:desc;index:idx_usage_root_session_time,priority:2,sort:desc"`
	LatencyMS                  int64     `gorm:"column:latency_ms;not null;default:0"`
	TTFTMS                     int64     `gorm:"column:ttft_ms;not null;default:0"`
	Source                     string    `gorm:"column:source;index:idx_usage_source;index:idx_usage_source_time,priority:1"`
	AuthIndex                  string    `gorm:"column:auth_index;index:idx_usage_auth_index;index:idx_usage_auth_time,priority:1"`
	QuotaCredentialID          string    `gorm:"column:quota_credential_id;not null;default:''"`
	QuotaIdentityVersion       int64     `gorm:"column:quota_identity_version;not null;default:0"`
	QuotaIdentityKey           string    `gorm:"column:quota_identity_key;not null;default:'';size:128"`
	InputTokens                int64     `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens               int64     `gorm:"column:output_tokens;not null;default:0"`
	ReasoningTokens            int64     `gorm:"column:reasoning_tokens;not null;default:0"`
	CachedTokens               int64     `gorm:"column:cached_tokens;not null;default:0"`
	CacheReadTokens            int64     `gorm:"column:cache_read_tokens;not null;default:0"`
	CacheReadTokensPresent     bool      `gorm:"column:cache_read_tokens_present;not null;default:false"`
	CacheCreationTokens        int64     `gorm:"column:cache_creation_tokens;not null;default:0"`
	TotalTokens                int64     `gorm:"column:total_tokens;not null;default:0"`
	TokenAccountingVersion     int       `gorm:"column:token_accounting_version;not null;default:0;index:idx_usage_token_accounting_version"`
	TokenAccountingQuality     string    `gorm:"column:token_accounting_quality;not null;default:'unclassified'"`
	AccountingTotalTokens      int64     `gorm:"column:accounting_total_tokens;not null;default:0"`
	AccountingInputTokens      int64     `gorm:"column:accounting_input_tokens;not null;default:0"`
	UncachedInputTokens        int64     `gorm:"column:uncached_input_tokens;not null;default:0"`
	AccountingCacheReadTokens  int64     `gorm:"column:accounting_cache_read_tokens;not null;default:0"`
	AccountingCacheWriteTokens int64     `gorm:"column:accounting_cache_write_tokens;not null;default:0"`
	AccountingOutputTokens     int64     `gorm:"column:accounting_output_tokens;not null;default:0"`
	NonReasoningOutputTokens   int64     `gorm:"column:non_reasoning_output_tokens;not null;default:0"`
	AccountingReasoningTokens  int64     `gorm:"column:accounting_reasoning_tokens;not null;default:0"`
	UnclassifiedTokens         int64     `gorm:"column:unclassified_tokens;not null;default:0"`
	Failed                     bool      `gorm:"column:failed;not null;default:false;index:idx_usage_failed;index:idx_usage_failed_time,priority:1;index:idx_usage_failed_status_time,priority:1"`
	FailStatusCode             int       `gorm:"column:fail_status_code;not null;default:0;index:idx_usage_failed_status_time,priority:2"`
	FailBody                   string    `gorm:"column:fail_body;type:text"`
	Provider                   string    `gorm:"column:provider;index:idx_usage_provider_model,priority:1;index:idx_usage_provider_model_time,priority:1;index:idx_usage_provider_time,priority:1"`
	ExecutorType               string    `gorm:"column:executor_type"`
	Model                      string    `gorm:"column:model;index:idx_usage_provider_model,priority:2;index:idx_usage_provider_model_time,priority:2"`
	Alias                      string    `gorm:"column:alias"`
	Effort                     string    `gorm:"column:effort"`
	ServiceTier                string    `gorm:"column:service_tier"`
	ResponseServiceTier        string    `gorm:"column:response_service_tier"`
	Endpoint                   string    `gorm:"column:endpoint;index:idx_usage_endpoint;index:idx_usage_endpoint_time,priority:1"`
	AuthType                   string    `gorm:"column:auth_type;index:idx_usage_auth_type_time,priority:1"`
	APIKey                     string    `gorm:"column:api_key;index:idx_usage_api_key"`
	RequestID                  string    `gorm:"column:request_id;index:idx_usage_request_id"`
	SessionID                  string    `gorm:"column:session_id;index:idx_usage_session_time,priority:1"`
	ParentSessionID            string    `gorm:"column:parent_session_id;index:idx_usage_parent_session_time,priority:1"`
	RootSessionID              string    `gorm:"column:root_session_id;index:idx_usage_root_session_time,priority:1"`
	UpstreamRequestID          string    `gorm:"column:upstream_request_id;index:idx_usage_upstream_request_id"`
	EventType                  string    `gorm:"column:event_type;index:idx_usage_event_type;index:idx_usage_event_time,priority:1"`
	UpstreamStatusCode         int       `gorm:"column:upstream_status_code;not null;default:0;index:idx_usage_upstream_status_code"`
	HomeIP                     string    `gorm:"column:home_ip;index:idx_usage_home_ip;index:idx_usage_home_time,priority:1;index:idx_usage_home_port_time,priority:1"`
	HomePort                   int       `gorm:"column:home_port;not null;default:0;index:idx_usage_home_port_time,priority:2"`
	CPANodeID                  string    `gorm:"column:cpa_node_id;index:idx_usage_cpa_node_id;index:idx_usage_cpa_node_time,priority:1"`
	CPAIP                      string    `gorm:"column:cpa_ip;index:idx_usage_cpa_ip"`
	CPAPort                    int       `gorm:"column:cpa_port;not null;default:0"`
	CPALabel                   string    `gorm:"column:cpa_label;index:idx_usage_cpa_label"`
	TokensJSON                 JSONB     `gorm:"column:tokens"`
	FailJSON                   JSONB     `gorm:"column:fail"`
	PayloadJSON                JSONB     `gorm:"column:payload;not null"`
	CreatedAt                  time.Time `gorm:"column:created_at;not null"`
}

func (databaseSnapshotV17UsageRecord) TableName() string {
	return "usage"
}

// databaseSnapshotV14UserRecord freezes the user shape used by snapshot formats
// v1 through v14, before per-user billing multipliers were added.
type databaseSnapshotV14UserRecord struct {
	ID                   uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_user_active_order,priority:2"`
	Username             string         `gorm:"column:username;not null;index;index:idx_user_username_active,priority:1"`
	Password             string         `gorm:"column:password;type:text"`
	Email                *string        `gorm:"column:email;type:text;index"`
	EmailVerifiedAt      *time.Time     `gorm:"column:email_verified_at"`
	EmailVersion         uint64         `gorm:"column:email_version;not null;default:0"`
	SessionVersion       uint64         `gorm:"column:session_version;not null;default:0"`
	Credits              float64        `gorm:"column:credits;not null;default:0"`
	CreditsUnlimited     bool           `gorm:"column:credits_unlimited;not null;default:false"`
	Timezone             string         `gorm:"column:timezone;not null;default:Asia/Shanghai"`
	Limit5hCredits       *float64       `gorm:"column:limit_5h_credits"`
	WindowMode5h         string         `gorm:"column:window_mode_5h;not null;default:first_use"`
	Limit1dCredits       *float64       `gorm:"column:limit_1d_credits"`
	WindowMode1d         string         `gorm:"column:window_mode_1d;not null;default:first_use"`
	Limit7dCredits       *float64       `gorm:"column:limit_7d_credits"`
	WindowMode7d         string         `gorm:"column:window_mode_7d;not null;default:first_use"`
	WeekResetDay         int            `gorm:"column:week_reset_day;not null;default:1"`
	WeekResetHour        int            `gorm:"column:week_reset_hour;not null;default:0"`
	Limit30dCredits      *float64       `gorm:"column:limit_30d_credits"`
	WindowMode30d        string         `gorm:"column:window_mode_30d;not null;default:first_use"`
	PeriodWindowStart5h  *time.Time     `gorm:"column:period_window_start_5h"`
	PeriodWindowStart1d  *time.Time     `gorm:"column:period_window_start_1d"`
	PeriodWindowStart7d  *time.Time     `gorm:"column:period_window_start_7d"`
	PeriodWindowStart30d *time.Time     `gorm:"column:period_window_start_30d"`
	UsageEpoch5h         *time.Time     `gorm:"column:usage_epoch_5h"`
	UsageEpoch1d         *time.Time     `gorm:"column:usage_epoch_1d"`
	UsageEpoch7d         *time.Time     `gorm:"column:usage_epoch_7d"`
	UsageEpoch30d        *time.Time     `gorm:"column:usage_epoch_30d"`
	MFA                  JSONB          `gorm:"column:mfa"`
	Passkey              JSONB          `gorm:"column:passkey"`
	CreatedAt            time.Time      `gorm:"column:created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_user_active_order,priority:1;index:idx_user_username_active,priority:2"`
}

func (databaseSnapshotV14UserRecord) TableName() string {
	return "user"
}

// databaseSnapshotV14BillingModelPriceRecord freezes the model price shape used
// by snapshot formats v1 through v14, before extended dimensions and expressions.
type databaseSnapshotV14BillingModelPriceRecord struct {
	ID                        string         `gorm:"column:id;primaryKey"`
	Provider                  string         `gorm:"column:provider;not null;index:idx_billing_model_price_lookup,priority:1"`
	Model                     string         `gorm:"column:model;not null;index:idx_billing_model_price_lookup,priority:2"`
	ServiceTier               string         `gorm:"column:service_tier;not null;default:*"`
	MinInputTokens            int64          `gorm:"column:min_input_tokens;not null;default:0"`
	InputPricePerMillion      float64        `gorm:"column:input_price_per_million;not null;default:0"`
	OutputPricePerMillion     float64        `gorm:"column:output_price_per_million;not null;default:0"`
	CacheReadPricePerMillion  float64        `gorm:"column:cache_read_price_per_million;not null;default:0"`
	CacheWritePricePerMillion float64        `gorm:"column:cache_write_price_per_million;not null;default:0"`
	RequestPrice              float64        `gorm:"column:request_price;not null;default:0"`
	Source                    string         `gorm:"column:source;not null;default:manual"`
	Enabled                   bool           `gorm:"column:enabled;not null;index:idx_billing_model_price_enabled"`
	Note                      string         `gorm:"column:note;type:text"`
	CreatedAt                 time.Time      `gorm:"column:created_at"`
	UpdatedAt                 time.Time      `gorm:"column:updated_at"`
	Revision                  int64          `gorm:"column:revision;not null;default:1"`
	DeletedAt                 gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

func (databaseSnapshotV14BillingModelPriceRecord) TableName() string {
	return "billing_model_price"
}

// databaseSnapshotV14BillingChargeRecord freezes the billing charge shape used
// by snapshot formats v1 through v14, before base_amount, multipliers, and breakdowns.
type databaseSnapshotV14BillingChargeRecord struct {
	ID               string    `gorm:"column:id;primaryKey;index:idx_billing_charge_time_order,priority:2,sort:desc"`
	UsageID          uint      `gorm:"column:usage_id;not null;uniqueIndex"`
	PayloadHash      string    `gorm:"column:payload_hash;not null;uniqueIndex"`
	UserID           *uint     `gorm:"column:user_id;index:idx_billing_charge_user_time,priority:1"`
	APIKeyID         *uint     `gorm:"column:api_key_id;index"`
	APIKeyLabel      string    `gorm:"column:api_key_label"`
	APIKeyMasked     string    `gorm:"column:api_key_masked"`
	Provider         string    `gorm:"column:provider;index:idx_billing_charge_provider_model_time,priority:1"`
	Model            string    `gorm:"column:model;index:idx_billing_charge_provider_model_time,priority:2"`
	OriginalModel    string    `gorm:"column:original_model"`
	ActualModel      string    `gorm:"column:actual_model"`
	RequestID        string    `gorm:"column:request_id;index"`
	Endpoint         string    `gorm:"column:endpoint;index"`
	InputTokens      int64     `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens     int64     `gorm:"column:output_tokens;not null;default:0"`
	CacheTokens      int64     `gorm:"column:cache_tokens;not null;default:0"`
	Amount           float64   `gorm:"column:amount;not null;default:0"`
	BalanceBefore    float64   `gorm:"column:balance_before;not null;default:0"`
	BalanceAfter     float64   `gorm:"column:balance_after;not null;default:0"`
	MatchedPriceRule string    `gorm:"column:matched_price_rule"`
	PriceSnapshot    JSONB     `gorm:"column:price_snapshot;not null"`
	RequestSummary   string    `gorm:"column:request_summary;type:text"`
	CreatedAt        time.Time `gorm:"column:created_at;not null;index:idx_billing_charge_time_order,priority:1,sort:desc;index:idx_billing_charge_user_time,priority:2,sort:desc;index:idx_billing_charge_provider_model_time,priority:3,sort:desc"`
}

func (databaseSnapshotV14BillingChargeRecord) TableName() string {
	return "billing_charge"
}

// databaseSnapshotV15QuotaSnapshotRecord freezes quota rows before plan expiry was added.
type databaseSnapshotV15QuotaSnapshotRecord struct {
	CredentialID        string     `gorm:"column:credential_id;primaryKey;size:128"`
	QuotaStatus         string     `gorm:"column:quota_status;not null;size:32;index"`
	CollectionStatus    string     `gorm:"column:collection_status;not null;size:32;index"`
	Source              string     `gorm:"column:source;size:32;index"`
	ObservedAt          *time.Time `gorm:"column:observed_at;index"`
	ExpiresAt           *time.Time `gorm:"column:expires_at;index"`
	LastAttemptAt       *time.Time `gorm:"column:last_attempt_at"`
	LastSuccessAt       *time.Time `gorm:"column:last_success_at"`
	NextProbeAt         *time.Time `gorm:"column:next_probe_at;index"`
	LastActiveProbeAt   *time.Time `gorm:"column:last_active_probe_at" json:"-"`
	ProbeActivityAt     *time.Time `gorm:"column:probe_activity_at" json:"-"`
	ConsecutiveFailure  int        `gorm:"column:consecutive_failures;not null;default:0"`
	ErrorCode           string     `gorm:"column:error_code;size:128"`
	ErrorMessage        string     `gorm:"column:error_message;size:500"`
	ErrorRetryable      bool       `gorm:"column:error_retryable;not null;default:false"`
	ErrorOccurredAt     *time.Time `gorm:"column:error_occurred_at"`
	ErrorStatusCode     int        `gorm:"column:error_status_code;not null;default:0"`
	ErrorRequestID      string     `gorm:"column:error_request_id;size:128"`
	HomeID              string     `gorm:"column:home_id;size:256"`
	HomeLabel           string     `gorm:"column:home_label;size:256"`
	CPANodeID           string     `gorm:"column:cpa_node_id;size:256"`
	CPANodeLabel        string     `gorm:"column:cpa_node_label;size:256"`
	PlanName            string     `gorm:"column:plan_name;size:64"`
	PlanPremium         *bool      `gorm:"column:plan_premium"`
	ResetCredits        JSONB      `gorm:"column:reset_credits"`
	ProbeLeaseOwner     string     `gorm:"column:probe_lease_owner;size:256"`
	ProbeLeaseExpiresAt *time.Time `gorm:"column:probe_lease_expires_at;index"`
	ParserVersion       int        `gorm:"column:parser_version;not null;default:1"`
	CollectorVersion    int        `gorm:"column:collector_version;not null;default:1"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

func (databaseSnapshotV15QuotaSnapshotRecord) TableName() string { return "quota_snapshot" }

var databaseSnapshotV1Models = []databaseModel{
	newDatabaseModel[databaseSnapshotV4AuthRecord]("auth", []string{"uuid"}, false, true),
	newDatabaseModel[ConfigRecord]("config", []string{"key"}, false, true),
	newDatabaseModel[KVRecord]("kv_store", []string{"key"}, false, true),
	newDatabaseModel[PluginStatusRecord]("plugin_status", []string{"node_type", "node_id", "plugin_id"}, false, false),
	newDatabaseModel[PluginTaskRecord]("plugin_tasks", []string{"id"}, true, false),
	newDatabaseModel[PluginStoreAuthKeyRecord]("plugin_store_auth_key", []string{"id"}, false, true),
	newDatabaseModel[PluginStoreAuthRecord]("plugin_store_auth", []string{"id"}, true, true),
	newDatabaseModel[databaseSnapshotV14UserRecord]("user", []string{"id"}, true, true),
	newDatabaseModel[UserSecurityTokenRecord]("user_security_token", []string{"id"}, true, false),
	newDatabaseModel[UserMailJobRecord]("user_mail_job", []string{"id"}, true, false),
	newDatabaseModel[UserSecurityThrottleRecord]("user_security_throttle", []string{"key"}, false, false),
	newDatabaseModel[ChannelGroupRecord]("channel_group", []string{"id"}, true, true),
	newDatabaseModel[ModelGroupRecord]("model_group", []string{"id"}, true, true),
	newDatabaseModel[databaseSnapshotV3APIKeyRecord]("api_key", []string{"id"}, true, true),
	newDatabaseModel[ChannelGroupDetailRecord]("channel_group_detail", []string{"id"}, true, true),
	newDatabaseModel[ModelGroupDetailRecord]("model_group_detail", []string{"id"}, true, true),
	newDatabaseModel[ClusterNodeRecord]("cluster", []string{"ip", "port"}, false, false),
	newDatabaseModel[databaseSnapshotV1CPANodeRecord]("cpa_node", []string{"home_ip", "home_port", "node_key"}, false, false),
	newDatabaseModel[ClusterEventRecord]("cluster_events", []string{"id"}, true, false),
	newDatabaseModel[databaseSnapshotV4UsageRecord]("usage", []string{"id"}, true, true),
	newDatabaseModel[databaseSnapshotV15QuotaSnapshotRecord]("quota_snapshot", []string{"credential_id"}, false, true),
	newDatabaseModel[QuotaWindowRecord]("quota_window", []string{"credential_id", "window_id"}, false, true),
	newDatabaseModel[databaseSnapshotV14BillingModelPriceRecord]("billing_model_price", []string{"id"}, false, true),
	newDatabaseModel[BillingModelPriceImportPreviewRecord]("billing_model_price_import_preview", []string{"id"}, false, true),
	newDatabaseModel[BillingModelPriceImportOperationRecord]("billing_model_price_import_operation", []string{"id"}, false, true),
	newDatabaseModel[BillingBalanceRecord]("billing_balance_record", []string{"id"}, false, true),
	newDatabaseModel[databaseSnapshotV14BillingChargeRecord]("billing_charge", []string{"id"}, false, true),
	newDatabaseModel[ProxyPoolRecord]("proxy_pool", []string{"id"}, false, true),
	newDatabaseModel[AppLogRecord]("log", []string{"id"}, true, true),
	newDatabaseModel[OAuthSessionRecord]("oauth_sessions", []string{"state"}, false, false),
	newDatabaseModel[CertificateRecord]("certificate", []string{"id"}, false, true),
}

// databaseSnapshotV2Models is the frozen database snapshot format v2 registry.
var databaseSnapshotV2Models = databaseSnapshotV2ModelRegistry()

// databaseSnapshotV3Models is the frozen database snapshot format v3 registry.
var databaseSnapshotV3Models = append(append([]databaseModel(nil), databaseSnapshotV2Models...),
	newDatabaseModel[CPANodeMetadataRecord]("cpa_node_metadata", []string{"node_id"}, false, true),
)

// databaseSnapshotV4Models is the frozen database snapshot format v4 registry.
var databaseSnapshotV4Models = databaseSnapshotV4ModelRegistry()

// databaseSnapshotV5Models is the frozen database snapshot format v5 registry.
var databaseSnapshotV5Models = databaseSnapshotV5ModelRegistry()

// databaseSnapshotV6Models is the frozen database snapshot format v6..v14 registry.
var databaseSnapshotV6Models = databaseSnapshotV6ModelRegistry()

// databaseSnapshotV15Models keeps the portable format used before plan expiry.
var databaseSnapshotV15Models = databaseSnapshotV15ModelRegistry()

// databaseSnapshotV16Models freezes the format before the refresh journal.
var databaseSnapshotV16Models = databaseSnapshotV16ModelRegistry()

// databaseSnapshotV17Models freezes the format before usage dedupe keys.
var databaseSnapshotV17Models = databaseSnapshotV17ModelRegistry()

// homeDatabaseModels is the current database snapshot registry.
var homeDatabaseModels = currentDatabaseModels()

var databaseMigrationOnlyModels = []databaseModel{
	// Session bindings are expiring runtime state, not portable snapshot data.
	newDatabaseModel[SessionAffinityRecord]("session_affinity", []string{"key"}, false, false),
	newDatabaseModel[SessionAffinitySequenceRecord]("session_affinity_sequence", []string{"key"}, false, false),
	newDatabaseModel[schemaMigrationRecord]("home_schema_migration", []string{"id"}, false, false),
	newDatabaseModel[ClusterMasterGateRecord]("cluster_master_gate", []string{"id"}, false, false),
	newDatabaseModel[ClusterRebalanceGateRecord]("cluster_rebalance_gate", []string{"id"}, false, false),
	newDatabaseModel[CPARebalanceClientRecord]("cpa_rebalance_clients", []string{"fingerprint"}, false, false),
	newDatabaseModel[CPARebalancePlanRecord]("cpa_rebalance_plans", []string{"id"}, false, false),
	newDatabaseModel[CPALatencyRecord]("cpa_latency_summaries", []string{"fingerprint", "address"}, false, false),
	newDatabaseModel[CPALatencyConfirmationRecord]("cpa_latency_confirmations", []string{"id"}, false, false),
	newDatabaseModel[ManagementInFlightSnapshotCursorRecord]("management_in_flight_snapshot_cursors", []string{"cursor"}, false, false),
	newDatabaseModel[ManagementInFlightSnapshotCursorItemRecord]("management_in_flight_snapshot_cursor_items", []string{"cursor", "ordinal"}, false, false),
	newDatabaseModel[ManagementInFlightSnapshotCursorObservedRecord]("management_in_flight_snapshot_cursor_observed", []string{"cursor", "credential_id"}, false, false),
	newDatabaseModel[ManagementInFlightSnapshotCursorStateRecord]("management_in_flight_snapshot_cursor_states", []string{"cursor", "credential_id"}, false, false),
	newDatabaseModel[ManagementInFlightSnapshotCursorStateModelRecord]("management_in_flight_snapshot_cursor_state_models", []string{"cursor", "credential_id", "model"}, false, false),
}

func databaseSnapshotV2ModelRegistry() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV1Models...)
	for index := range models {
		if models[index].name == "cpa_node" {
			models[index] = newDatabaseModel[CPANodeRecord]("cpa_node", []string{"home_ip", "home_port", "home_started_at", "node_key"}, false, false)
			break
		}
	}
	return append(models,
		newDatabaseModel[LifecycleConfigRecord]("lifecycle_config", []string{"id"}, false, true),
		newDatabaseModel[ConcurrencyActivationGateRecord]("concurrency_activation_gate", []string{"id"}, false, true),
		newDatabaseModel[CredentialConcurrencyPolicyRecord]("credential_concurrency_policies", []string{"credential_id"}, false, true),
		newDatabaseModel[CredentialConcurrencyModelPolicyRecord]("credential_concurrency_model_policies", []string{"credential_id", "model"}, false, true),
		newDatabaseModel[CredentialConcurrencyCounterRecord]("credential_concurrency_counters", []string{"credential_id", "model", "certificate_fingerprint"}, false, false),
		newDatabaseModel[ConcurrencyObservationBarrierRecord]("credential_concurrency_observation_barrier", []string{"id"}, false, true),
		newDatabaseModel[HomeProcessIncarnationRecord]("home_process_incarnation", []string{"home_ip", "home_port", "started_at"}, false, false),
		newDatabaseModel[CPANodeMembershipRecord]("cpa_node_membership", []string{"certificate_fingerprint"}, false, false),
		newDatabaseModel[CPANodeParticipationRecord]("cpa_node_participation", []string{"certificate_fingerprint", "membership_connected_at", "home_ip", "home_port", "home_started_at"}, false, false),
		newDatabaseModel[CPANodeQuiescenceRecord]("cpa_node_quiescence", []string{"certificate_fingerprint", "membership_connected_at", "cancel_revision", "home_ip", "home_port", "home_started_at"}, false, false),
		newDatabaseModel[CPAInFlightSnapshotRecord]("cpa_in_flight_snapshots", []string{"certificate_fingerprint"}, false, false),
		newDatabaseModel[CPAInFlightSnapshotAttemptRecord]("cpa_in_flight_snapshot_attempts", []string{"certificate_fingerprint", "membership_connected_at"}, false, false),
		newDatabaseModel[CPAInFlightSnapshotPartRecord]("cpa_in_flight_snapshot_parts", []string{"certificate_fingerprint", "membership_connected_at", "revision", "part_index"}, false, false),
	)
}

func databaseSnapshotV4ModelRegistry() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV3Models...)
	for index := range models {
		if models[index].name == "api_key" {
			models[index] = newDatabaseModel[APIKeyRecord]("api_key", []string{"id"}, true, true)
			break
		}
	}
	return models
}

func databaseSnapshotV5ModelRegistry() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV4Models...)
	for index := range models {
		switch models[index].name {
		case "auth":
			models[index] = newDatabaseModel[AuthRecord]("auth", []string{"uuid"}, false, true)
		case "usage":
			models[index] = newDatabaseModel[databaseSnapshotV5UsageRecord]("usage", []string{"id"}, true, true)
		}
	}
	return models
}

func databaseSnapshotV6ModelRegistry() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV5Models...)
	for index := range models {
		switch models[index].name {
		case "usage":
			models[index] = newDatabaseModel[databaseSnapshotV17UsageRecord]("usage", []string{"id"}, true, true)
		}
	}
	return models
}

func databaseSnapshotV15ModelRegistry() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV6Models...)
	for index := range models {
		switch models[index].name {
		case "user":
			models[index] = newDatabaseModel[UserRecord]("user", []string{"id"}, true, true)
		case "billing_model_price":
			models[index] = newDatabaseModel[BillingModelPriceRecord]("billing_model_price", []string{"id"}, false, true)
		case "billing_charge":
			models[index] = newDatabaseModel[BillingChargeRecord]("billing_charge", []string{"id"}, false, true)
		}
	}
	return models
}

func databaseSnapshotV16ModelRegistry() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV15Models...)
	for index := range models {
		if models[index].name == "quota_snapshot" {
			models[index] = newDatabaseModel[QuotaSnapshotRecord]("quota_snapshot", []string{"credential_id"}, false, true)
			break
		}
	}
	return models
}

func databaseSnapshotV17ModelRegistry() []databaseModel {
	return append(append([]databaseModel(nil), databaseSnapshotV16Models...),
		newDatabaseModel[CodexRefreshRecord]("codex_refresh_journal", []string{"grant_sha256"}, false, true),
	)
}

func currentDatabaseModels() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV17Models...)
	for index := range models {
		if models[index].name == "usage" {
			models[index] = newDatabaseModel[UsageRecord]("usage", []string{"id"}, true, true)
			break
		}
	}
	return models
}

func databaseSnapshotModels(formatVersion int) ([]databaseModel, bool) {
	switch formatVersion {
	case 1:
		return databaseSnapshotV1Models, true
	case 2:
		return databaseSnapshotV2Models, true
	case 3:
		return databaseSnapshotV3Models, true
	case 4:
		return databaseSnapshotV4Models, true
	case 5:
		return databaseSnapshotV5Models, true
	case 6, 7, 8, 9, 10, 11, 12, 13, 14:
		// Versions 7 through 14 add migrations, indexes, and transient tables;
		// portable table shapes still match v6.
		return databaseSnapshotV6Models, true
	case 15:
		return databaseSnapshotV15Models, true
	case 16:
		return databaseSnapshotV16Models, true
	case 17:
		return databaseSnapshotV17Models, true
	case 18, currentDatabaseVersion:
		// Version 19 only adds expiring Guardian selection metadata.
		return homeDatabaseModels, true
	default:
		return nil, false
	}
}

func databaseMigrationModels() []any {
	models := make([]any, 0, len(homeDatabaseModels)+len(databaseMigrationOnlyModels))
	for _, model := range homeDatabaseModels {
		models = append(models, model.newRecord())
	}
	for _, model := range databaseMigrationOnlyModels {
		models = append(models, model.newRecord())
	}
	return models
}
