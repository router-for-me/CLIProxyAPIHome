package cluster

import (
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// currentDatabaseVersion is shared by the live schema migration gate and the
// portable snapshot format. Increment it for every required startup migration
// or snapshot format change, and retain mappings for prior snapshot formats.
const currentDatabaseVersion = 5

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

var databaseSnapshotV1Models = []databaseModel{
	newDatabaseModel[databaseSnapshotV4AuthRecord]("auth", []string{"uuid"}, false, true),
	newDatabaseModel[ConfigRecord]("config", []string{"key"}, false, true),
	newDatabaseModel[KVRecord]("kv_store", []string{"key"}, false, true),
	newDatabaseModel[PluginStatusRecord]("plugin_status", []string{"node_type", "node_id", "plugin_id"}, false, false),
	newDatabaseModel[PluginTaskRecord]("plugin_tasks", []string{"id"}, true, false),
	newDatabaseModel[PluginStoreAuthKeyRecord]("plugin_store_auth_key", []string{"id"}, false, true),
	newDatabaseModel[PluginStoreAuthRecord]("plugin_store_auth", []string{"id"}, true, true),
	newDatabaseModel[UserRecord]("user", []string{"id"}, true, true),
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
	newDatabaseModel[QuotaSnapshotRecord]("quota_snapshot", []string{"credential_id"}, false, true),
	newDatabaseModel[QuotaWindowRecord]("quota_window", []string{"credential_id", "window_id"}, false, true),
	newDatabaseModel[BillingModelPriceRecord]("billing_model_price", []string{"id"}, false, true),
	newDatabaseModel[BillingModelPriceImportPreviewRecord]("billing_model_price_import_preview", []string{"id"}, false, true),
	newDatabaseModel[BillingModelPriceImportOperationRecord]("billing_model_price_import_operation", []string{"id"}, false, true),
	newDatabaseModel[BillingBalanceRecord]("billing_balance_record", []string{"id"}, false, true),
	newDatabaseModel[BillingChargeRecord]("billing_charge", []string{"id"}, false, true),
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

// homeDatabaseModels is the current database snapshot registry.
var homeDatabaseModels = currentDatabaseModels()

var databaseMigrationOnlyModels = []databaseModel{
	newDatabaseModel[schemaMigrationRecord]("home_schema_migration", []string{"id"}, false, false),
	newDatabaseModel[ClusterMasterGateRecord]("cluster_master_gate", []string{"id"}, false, false),
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

func currentDatabaseModels() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV4Models...)
	for index := range models {
		switch models[index].name {
		case "auth":
			models[index] = newDatabaseModel[AuthRecord]("auth", []string{"uuid"}, false, true)
		case "usage":
			models[index] = newDatabaseModel[UsageRecord]("usage", []string{"id"}, true, true)
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
	case currentDatabaseVersion:
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
