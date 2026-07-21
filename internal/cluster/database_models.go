package cluster

// databaseModel describes one managed Home database table. The same registry
// drives schema migration and logical snapshot handling so newly managed tables
// cannot silently be omitted from snapshots.
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

var homeDatabaseModels = []databaseModel{
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
	newDatabaseModel[APIKeyRecord]("api_key", []string{"id"}, true, true),
	newDatabaseModel[ChannelGroupDetailRecord]("channel_group_detail", []string{"id"}, true, true),
	newDatabaseModel[ModelGroupDetailRecord]("model_group_detail", []string{"id"}, true, true),
	newDatabaseModel[ClusterNodeRecord]("cluster", []string{"ip", "port"}, false, false),
	newDatabaseModel[CPANodeRecord]("cpa_node", []string{"home_ip", "home_port", "node_key"}, false, false),
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

func databaseMigrationModels() []any {
	models := make([]any, 0, len(homeDatabaseModels))
	for _, model := range homeDatabaseModels {
		models = append(models, model.newRecord())
	}
	return models
}
