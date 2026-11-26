package cluster

import "time"

// databaseSnapshotV19UsageRecord freezes the usage shape before delivery tracking.
type databaseSnapshotV19UsageRecord struct {
	ID        uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_usage_time_order,priority:2"`
	Timestamp time.Time `gorm:"column:timestamp;not null;index:idx_usage_timestamp;index:idx_usage_time_order,priority:1,sort:desc;index:idx_usage_source_time,priority:2,sort:desc;index:idx_usage_auth_time,priority:2,sort:desc;index:idx_usage_failed_time,priority:2,sort:desc;index:idx_usage_failed_status_time,priority:3,sort:desc;index:idx_usage_provider_model_time,priority:3,sort:desc;index:idx_usage_provider_time,priority:2,sort:desc;index:idx_usage_endpoint_time,priority:2,sort:desc;index:idx_usage_home_time,priority:2,sort:desc;index:idx_usage_auth_type_time,priority:2,sort:desc;index:idx_usage_session_time,priority:2,sort:desc;index:idx_usage_parent_session_time,priority:2,sort:desc;index:idx_usage_root_session_time,priority:2,sort:desc"`
	LatencyMS int64     `gorm:"column:latency_ms;not null;default:0"`
	TTFTMS    int64     `gorm:"column:ttft_ms;not null;default:0"`
	Source    string    `gorm:"column:source;index:idx_usage_source;index:idx_usage_source_time,priority:1"`
	AuthIndex string    `gorm:"column:auth_index;index:idx_usage_auth_index;index:idx_usage_auth_time,priority:1"`
	// DedupeKey identifies one CPA execution so redelivered payloads collapse onto
	// the first committed row. Rows written before the column existed keep an
	// empty key and stay outside the partial unique index.
	DedupeKey string `gorm:"column:dedupe_key;not null;default:'';size:80;index:idx_usage_dedupe_key,unique,where:dedupe_key <> ''"`
	// Quota identity fields fence activity from concurrent credential replacement;
	// the key also protects rolling upgrades where an older Home cannot advance the generation.
	QuotaCredentialID    string `gorm:"column:quota_credential_id;not null;default:''"`
	QuotaIdentityVersion int64  `gorm:"column:quota_identity_version;not null;default:0"`
	QuotaIdentityKey     string `gorm:"column:quota_identity_key;not null;default:'';size:128"`
	InputTokens          int64  `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens         int64  `gorm:"column:output_tokens;not null;default:0"`
	ReasoningTokens      int64  `gorm:"column:reasoning_tokens;not null;default:0"`
	CachedTokens         int64  `gorm:"column:cached_tokens;not null;default:0"`
	CacheReadTokens      int64  `gorm:"column:cache_read_tokens;not null;default:0"`
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

func (databaseSnapshotV19UsageRecord) TableName() string {
	return "usage"
}
