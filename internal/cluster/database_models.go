package cluster

import (
	"time"

	"gorm.io/gorm"
)

// currentDatabaseVersion is shared by the live schema migration gate and the
// portable snapshot format. Increment it for every required startup migration
// or snapshot format change, and retain mappings for prior snapshot formats.
const currentDatabaseVersion = 4

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

var databaseSnapshotV1Models = []databaseModel{
	newDatabaseModel[AuthRecord]("auth", []string{"uuid"}, false, true),
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
	newDatabaseModel[UsageRecord]("usage", []string{"id"}, true, true),
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

func currentDatabaseModels() []databaseModel {
	models := append([]databaseModel(nil), databaseSnapshotV3Models...)
	for index := range models {
		if models[index].name == "api_key" {
			models[index] = newDatabaseModel[APIKeyRecord]("api_key", []string{"id"}, true, true)
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
