package cluster

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type JSONB []byte

// GormDataType returns the logical GORM data type.
func (JSONB) GormDataType() string {
	return "json"
}

// GormDBDataType returns the dialect-specific database type.
func (JSONB) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db == nil || db.Dialector == nil {
		return "text"
	}
	switch db.Dialector.Name() {
	case "postgres":
		return "jsonb"
	case "sqlite":
		return "text"
	default:
		return "text"
	}
}

// Value converts the value for database storage.
func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("invalid jsonb value")
	}
	return string(j), nil
}

// Scan loads the value from database storage.
func (j *JSONB) Scan(value any) error {
	if j == nil {
		return fmt.Errorf("jsonb scan target is nil")
	}
	switch data := value.(type) {
	case nil:
		*j = nil
		return nil
	case []byte:
		*j = append((*j)[:0], data...)
		return nil
	case string:
		*j = append((*j)[:0], data...)
		return nil
	default:
		return fmt.Errorf("unsupported jsonb scan type %T", value)
	}
}

// MarshalJSON encodes a json.
func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON decodes a json.
func (j *JSONB) UnmarshalJSON(data []byte) error {
	if j == nil {
		return fmt.Errorf("jsonb unmarshal target is nil")
	}
	if len(data) == 0 {
		*j = nil
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid jsonb value")
	}
	*j = append((*j)[:0], data...)
	return nil
}

type AuthRecord struct {
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

// TableName returns the database table name.
func (AuthRecord) TableName() string {
	return "auth"
}

type ConfigRecord struct {
	Key       string    `gorm:"column:key;primaryKey"`
	Value     JSONB     `gorm:"column:value;not null"`
	Version   int64     `gorm:"column:version;not null;default:1"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (ConfigRecord) TableName() string {
	return "config"
}

// LifecycleConfigRecord stores the singleton credential concurrency lifecycle configuration.
type LifecycleConfigRecord struct {
	ID                   int           `gorm:"column:id;primaryKey"`
	Revision             int64         `gorm:"column:revision;not null"`
	NodeHeartbeatTimeout time.Duration `gorm:"column:node_heartbeat_timeout;not null"`
	Payload              JSONB         `gorm:"column:payload;not null"`
	UpdatedAt            time.Time     `gorm:"column:updated_at"`
}

func (LifecycleConfigRecord) TableName() string {
	return "lifecycle_config"
}

// ConcurrencyActivationGateRecord serializes legacy admission and policy activation.
type ConcurrencyActivationGateRecord struct {
	ID                int   `gorm:"column:id;primaryKey"`
	ActivePolicyCount int64 `gorm:"column:active_policy_count;not null"`
}

func (ConcurrencyActivationGateRecord) TableName() string {
	return "concurrency_activation_gate"
}

// ClusterMasterGateRecord serializes the single persisted Home master selection.
type ClusterMasterGateRecord struct {
	ID int `gorm:"column:id;primaryKey"`
}

func (ClusterMasterGateRecord) TableName() string {
	return "cluster_master_gate"
}

// HomeProcessIncarnationRecord stores an append-only Home process incarnation.
type HomeProcessIncarnationRecord struct {
	HomeIP       string     `gorm:"column:home_ip;primaryKey"`
	HomePort     int        `gorm:"column:home_port;primaryKey"`
	StartedAt    time.Time  `gorm:"column:started_at;primaryKey"`
	LastSeenAt   time.Time  `gorm:"column:last_seen_at;not null;index"`
	State        string     `gorm:"column:state;not null;index"`
	FencedAt     *time.Time `gorm:"column:fenced_at"`
	FenceReason  string     `gorm:"column:fence_reason"`
	Capabilities JSONB      `gorm:"column:capabilities;not null"`
}

func (HomeProcessIncarnationRecord) TableName() string {
	return "home_process_incarnation"
}

// CPANodeMembershipRecord contains only the membership fields needed by the lifecycle gate.
type CPANodeMembershipRecord struct {
	CertificateFingerprint  string        `gorm:"column:certificate_fingerprint;primaryKey"`
	NodeID                  string        `gorm:"column:node_id;not null"`
	HomeIP                  string        `gorm:"column:home_ip;not null"`
	HomePort                int           `gorm:"column:home_port;not null"`
	HomeStartedAt           time.Time     `gorm:"column:home_started_at;not null"`
	ProtocolVersion         int           `gorm:"column:protocol_version;not null"`
	State                   string        `gorm:"column:state;not null;index"`
	CancelRevision          int64         `gorm:"column:cancel_revision;not null"`
	CancelStartedAt         *time.Time    `gorm:"column:cancel_started_at"`
	ExpectedQuiescenceCount int64         `gorm:"column:expected_quiescence_count;not null"`
	ConnectedAt             time.Time     `gorm:"column:connected_at;not null"`
	LifecycleConfigRevision int64         `gorm:"column:lifecycle_config_revision;not null"`
	CPAHeartbeatTimeout     time.Duration `gorm:"column:cpa_heartbeat_timeout;not null"`
	CPACancelBound          time.Duration `gorm:"column:cpa_cancel_bound;not null"`
	ReclaimGrace            time.Duration `gorm:"column:reclaim_grace;not null"`
	LastSeenAt              time.Time     `gorm:"column:last_seen_at;not null"`
	UpdatedAt               time.Time     `gorm:"column:updated_at;not null"`
}

func (CPANodeMembershipRecord) TableName() string {
	return "cpa_node_membership"
}

// CPANodeParticipationRecord identifies a Home's connection to a membership lifetime.
type CPANodeParticipationRecord struct {
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;primaryKey"`
	MembershipConnectedAt  time.Time `gorm:"column:membership_connected_at;primaryKey"`
	HomeIP                 string    `gorm:"column:home_ip;primaryKey"`
	HomePort               int       `gorm:"column:home_port;primaryKey"`
	HomeStartedAt          time.Time `gorm:"column:home_started_at;primaryKey"`
	CreatedAt              time.Time `gorm:"column:created_at;not null"`
}

func (CPANodeParticipationRecord) TableName() string {
	return "cpa_node_participation"
}

// CPANodeQuiescenceRecord records one Home incarnation's fence acknowledgement.
type CPANodeQuiescenceRecord struct {
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;primaryKey"`
	MembershipConnectedAt  time.Time `gorm:"column:membership_connected_at;primaryKey"`
	CancelRevision         int64     `gorm:"column:cancel_revision;primaryKey"`
	HomeIP                 string    `gorm:"column:home_ip;primaryKey"`
	HomePort               int       `gorm:"column:home_port;primaryKey"`
	HomeStartedAt          time.Time `gorm:"column:home_started_at;primaryKey"`
	Status                 string    `gorm:"column:status;not null"`
	UpdatedAt              time.Time `gorm:"column:updated_at;not null"`
}

func (CPANodeQuiescenceRecord) TableName() string {
	return "cpa_node_quiescence"
}

type KVRecord struct {
	Key       string     `gorm:"column:key;primaryKey"`
	Value     []byte     `gorm:"column:value;not null"`
	Version   int64      `gorm:"column:version;not null;default:1"`
	ExpiresAt *time.Time `gorm:"column:expires_at;index"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (KVRecord) TableName() string {
	return "kv_store"
}

type PluginStatusRecord struct {
	NodeType      string     `gorm:"column:node_type;primaryKey;size:16;index:idx_plugin_status_node,priority:1;index:idx_plugin_status_plugin,priority:1"`
	NodeID        string     `gorm:"column:node_id;primaryKey;size:128;index:idx_plugin_status_node,priority:2"`
	PluginID      string     `gorm:"column:plugin_id;primaryKey;size:128;index:idx_plugin_status_plugin,priority:2"`
	TaskID        uint       `gorm:"column:task_id;index"`
	ClientIP      string     `gorm:"column:client_ip;index"`
	SchemaVersion int        `gorm:"column:schema_version"`
	Task          string     `gorm:"column:task"`
	TaskStatus    string     `gorm:"column:task_status;index"`
	Phase         string     `gorm:"column:phase;index"`
	OK            bool       `gorm:"column:ok;index"`
	TaskError     string     `gorm:"column:task_error;type:text"`
	StartedAt     time.Time  `gorm:"column:started_at"`
	FinishedAt    *time.Time `gorm:"column:finished_at"`
	ReportedAt    time.Time  `gorm:"column:reported_at;index"`
	GOOS          string     `gorm:"column:goos;index"`
	GOARCH        string     `gorm:"column:goarch;index"`
	Version       string     `gorm:"column:version"`
	ReleaseTag    string     `gorm:"column:release_tag"`
	Repository    string     `gorm:"column:repository;type:text"`
	InstallStatus string     `gorm:"column:install_status;index"`
	LoadStatus    string     `gorm:"column:load_status;index"`
	Path          string     `gorm:"column:path;type:text"`
	Skipped       bool       `gorm:"column:skipped"`
	Overwritten   bool       `gorm:"column:overwritten"`
	PluginError   string     `gorm:"column:plugin_error;type:text"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (PluginStatusRecord) TableName() string {
	return "plugin_status"
}

type PluginTaskRecord struct {
	ID             uint      `gorm:"column:id;primaryKey;autoIncrement"`
	Operation      string    `gorm:"column:operation;size:32;index:idx_plugin_tasks_operation"`
	PluginID       string    `gorm:"column:plugin_id;size:128;index:idx_plugin_tasks_plugin"`
	TargetNodeType string    `gorm:"column:target_node_type;size:16;index:idx_plugin_tasks_target,priority:1"`
	TargetNodeID   string    `gorm:"column:target_node_id;size:128;index:idx_plugin_tasks_target,priority:2"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

type PluginStoreAuthRecord struct {
	ID                   uint   `gorm:"column:id;primaryKey;autoIncrement"`
	Name                 string `gorm:"column:name;not null"`
	Match                string `gorm:"column:match;not null;type:text"`
	ApplyTo              JSONB  `gorm:"column:apply_to"`
	AuthType             string `gorm:"column:auth_type;not null"`
	HeaderName           string `gorm:"column:header_name"`
	EncryptedCredentials []byte `gorm:"column:encrypted_credentials"`
	KeyVersion           int    `gorm:"column:key_version;not null"`
	Enabled              bool   `gorm:"column:enabled;not null;default:true"`
	Version              int64  `gorm:"column:version;not null;default:1"`
}

func (PluginStoreAuthRecord) TableName() string {
	return "plugin_store_auth"
}

type PluginStoreAuthKeyRecord struct {
	ID         uint   `gorm:"column:id;primaryKey"`
	Key        []byte `gorm:"column:key;not null"`
	KeyVersion int    `gorm:"column:key_version;not null"`
}

func (PluginStoreAuthKeyRecord) TableName() string {
	return "plugin_store_auth_key"
}

// TableName returns the database table name.
func (PluginTaskRecord) TableName() string {
	return "plugin_tasks"
}

type UserRecord struct {
	ID               uint       `gorm:"column:id;primaryKey;autoIncrement;index:idx_user_active_order,priority:2"`
	Username         string     `gorm:"column:username;not null;index;index:idx_user_username_active,priority:1"`
	Password         string     `gorm:"column:password;type:text"`
	Email            *string    `gorm:"column:email;type:text;index"`
	EmailVerifiedAt  *time.Time `gorm:"column:email_verified_at"`
	EmailVersion     uint64     `gorm:"column:email_version;not null;default:0"`
	SessionVersion   uint64     `gorm:"column:session_version;not null;default:0"`
	Credits          float64    `gorm:"column:credits;not null;default:0"`
	CreditsUnlimited bool       `gorm:"column:credits_unlimited;not null;default:false"`
	Timezone         string     `gorm:"column:timezone;not null;default:Asia/Shanghai"`
	// Period cost limits (null = disabled). Subject is user; all API keys inherit.
	// window_mode_*: first_use (default; first billable charge opens a duration
	// window, Claude/Codex-style session), sliding (rolling last-N duration),
	// calendar (natural day/week/month; 1d/7d/30d only). Legacy alias: fixed -> first_use.
	Limit5hCredits  *float64 `gorm:"column:limit_5h_credits"`
	WindowMode5h    string   `gorm:"column:window_mode_5h;not null;default:first_use"`
	Limit1dCredits  *float64 `gorm:"column:limit_1d_credits"`
	WindowMode1d    string   `gorm:"column:window_mode_1d;not null;default:first_use"`
	Limit7dCredits  *float64 `gorm:"column:limit_7d_credits"`
	WindowMode7d    string   `gorm:"column:window_mode_7d;not null;default:first_use"`
	WeekResetDay    int      `gorm:"column:week_reset_day;not null;default:1"`
	WeekResetHour   int      `gorm:"column:week_reset_hour;not null;default:0"`
	Limit30dCredits *float64 `gorm:"column:limit_30d_credits"`
	WindowMode30d   string   `gorm:"column:window_mode_30d;not null;default:first_use"`
	// first_use window runtime state (first billable charge opens the window).
	PeriodWindowStart5h  *time.Time `gorm:"column:period_window_start_5h"`
	PeriodWindowStart1d  *time.Time `gorm:"column:period_window_start_1d"`
	PeriodWindowStart7d  *time.Time `gorm:"column:period_window_start_7d"`
	PeriodWindowStart30d *time.Time `gorm:"column:period_window_start_30d"`
	// Soft reset markers: SUM only counts charges at/after max(window_lower, epoch).
	UsageEpoch5h  *time.Time     `gorm:"column:usage_epoch_5h"`
	UsageEpoch1d  *time.Time     `gorm:"column:usage_epoch_1d"`
	UsageEpoch7d  *time.Time     `gorm:"column:usage_epoch_7d"`
	UsageEpoch30d *time.Time     `gorm:"column:usage_epoch_30d"`
	MFA           JSONB          `gorm:"column:mfa"`
	Passkey       JSONB          `gorm:"column:passkey"`
	CreatedAt     time.Time      `gorm:"column:created_at"`
	UpdatedAt     time.Time      `gorm:"column:updated_at"`
	DeletedAt     gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_user_active_order,priority:1;index:idx_user_username_active,priority:2"`
}

// TableName returns the database table name.
func (UserRecord) TableName() string {
	return "user"
}

type UserSecurityTokenRecord struct {
	ID           uint       `gorm:"column:id;primaryKey;autoIncrement"`
	UserID       uint       `gorm:"column:user_id;not null;index:idx_user_security_token_user_purpose,priority:1"`
	Purpose      string     `gorm:"column:purpose;type:varchar(32);not null;index:idx_user_security_token_user_purpose,priority:2"`
	TokenHash    string     `gorm:"column:token_hash;type:varchar(64);not null;uniqueIndex"`
	EmailVersion uint64     `gorm:"column:email_version;not null;default:0"`
	ExpiresAt    time.Time  `gorm:"column:expires_at;not null;index"`
	UsedAt       *time.Time `gorm:"column:used_at;index"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

// TableName returns the database table name.
func (UserSecurityTokenRecord) TableName() string {
	return "user_security_token"
}

type UserMailJobRecord struct {
	ID             uint       `gorm:"column:id;primaryKey;autoIncrement;index:idx_user_mail_job_status_next,priority:3"`
	UserID         uint       `gorm:"column:user_id;not null;index:idx_user_mail_job_user_purpose,priority:1"`
	Purpose        string     `gorm:"column:purpose;type:varchar(32);not null;index:idx_user_mail_job_user_purpose,priority:2"`
	EmailVersion   uint64     `gorm:"column:email_version;not null;default:0"`
	SessionVersion uint64     `gorm:"column:session_version;not null;default:0"`
	Status         string     `gorm:"column:status;type:varchar(24);not null;index:idx_user_mail_job_status_next,priority:1"`
	Attempts       int        `gorm:"column:attempts;not null;default:0"`
	NextAttemptAt  time.Time  `gorm:"column:next_attempt_at;not null;index:idx_user_mail_job_status_next,priority:2"`
	ClaimedBy      string     `gorm:"column:claimed_by;type:varchar(128)"`
	ClaimExpiresAt *time.Time `gorm:"column:claim_expires_at;index"`
	LastErrorCode  string     `gorm:"column:last_error_code;type:varchar(64)"`
	SentAt         *time.Time `gorm:"column:sent_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (UserMailJobRecord) TableName() string {
	return "user_mail_job"
}

type UserSecurityThrottleRecord struct {
	Key       string    `gorm:"column:key;type:varchar(160);primaryKey"`
	Count     int       `gorm:"column:count;not null;default:0"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null;index"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (UserSecurityThrottleRecord) TableName() string {
	return "user_security_throttle"
}

type APIKeyRecord struct {
	ID          uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_api_key_active_order,priority:2"`
	APIKey      string         `gorm:"column:api_key;not null;uniqueIndex"`
	UserID      *uint          `gorm:"column:user_id;index;index:idx_api_key_user_active,priority:1"`
	Channels    JSONB          `gorm:"column:channels"`
	ModelGroups JSONB          `gorm:"column:model_groups"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_api_key_active_order,priority:1;index:idx_api_key_user_active,priority:2"`
}

// TableName returns the database table name.
func (APIKeyRecord) TableName() string {
	return "api_key"
}

type ChannelGroupRecord struct {
	ID          uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_channel_group_active_order,priority:2;index:idx_channel_group_enabled_order,priority:3"`
	ChannelName string         `gorm:"column:channel_name;not null"`
	Disabled    bool           `gorm:"column:disabled;not null;default:false;index:idx_channel_group_enabled_order,priority:2"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_channel_group_active_order,priority:1;index:idx_channel_group_enabled_order,priority:1"`
}

// TableName returns the database table name.
func (ChannelGroupRecord) TableName() string {
	return "channel_group"
}

type ChannelGroupDetailRecord struct {
	ID             uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_channel_group_detail_group_active_order,priority:3;index:idx_channel_group_detail_auth_active_order,priority:4"`
	ChannelGroupID uint           `gorm:"column:channel_group_id;not null;index:idx_channel_group_detail_group_active_order,priority:1;index:idx_channel_group_detail_auth_active_order,priority:3"`
	AuthID         string         `gorm:"column:auth_id;not null;index:idx_channel_group_detail_auth_active_order,priority:1"`
	CreatedAt      time.Time      `gorm:"column:created_at"`
	UpdatedAt      time.Time      `gorm:"column:updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_channel_group_detail_group_active_order,priority:2;index:idx_channel_group_detail_auth_active_order,priority:2"`
}

// TableName returns the database table name.
func (ChannelGroupDetailRecord) TableName() string {
	return "channel_group_detail"
}

type ModelGroupRecord struct {
	ID        uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_model_group_active_order,priority:2;index:idx_model_group_enabled_order,priority:3"`
	GroupName string         `gorm:"column:group_name;not null"`
	Disabled  bool           `gorm:"column:disabled;not null;default:false;index:idx_model_group_enabled_order,priority:2"`
	CreatedAt time.Time      `gorm:"column:created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_model_group_active_order,priority:1;index:idx_model_group_enabled_order,priority:1"`
}

// TableName returns the database table name.
func (ModelGroupRecord) TableName() string {
	return "model_group"
}

type ModelGroupDetailRecord struct {
	ID           uint           `gorm:"column:id;primaryKey;autoIncrement;index:idx_model_group_detail_group_active_order,priority:3;index:idx_model_group_detail_model_active_order,priority:4;index:idx_model_group_detail_active_order,priority:3"`
	ModelGroupID uint           `gorm:"column:model_group_id;not null;index:idx_model_group_detail_group_active_order,priority:1;index:idx_model_group_detail_model_active_order,priority:3;index:idx_model_group_detail_active_order,priority:2"`
	ModelID      string         `gorm:"column:model_id;not null;index:idx_model_group_detail_model_active_order,priority:1"`
	Channels     JSONB          `gorm:"column:channels"`
	CreatedAt    time.Time      `gorm:"column:created_at"`
	UpdatedAt    time.Time      `gorm:"column:updated_at"`
	DeletedAt    gorm.DeletedAt `gorm:"column:deleted_at;index;index:idx_model_group_detail_group_active_order,priority:2;index:idx_model_group_detail_model_active_order,priority:2;index:idx_model_group_detail_active_order,priority:1"`
}

// TableName returns the database table name.
func (ModelGroupDetailRecord) TableName() string {
	return "model_group_detail"
}

type ClusterNodeRecord struct {
	IP          string    `gorm:"column:ip;primaryKey;index:idx_cluster_auth_lookup,priority:1;index:idx_cluster_live_nodes,priority:3;index:idx_cluster_master_nodes,priority:4;index:idx_cluster_auth_live_first,priority:1;index:idx_cluster_live_schedule,priority:3;index:idx_cluster_master_current,priority:4"`
	Port        int       `gorm:"column:port;primaryKey;index:idx_cluster_auth_lookup,priority:5;index:idx_cluster_live_nodes,priority:4;index:idx_cluster_master_nodes,priority:5;index:idx_cluster_auth_live_first,priority:4;index:idx_cluster_live_schedule,priority:4;index:idx_cluster_master_current,priority:5"`
	SecretHash  string    `gorm:"column:secret_hash;index:idx_cluster_auth_lookup,priority:2;index:idx_cluster_auth_live_first,priority:2"`
	IsMaster    bool      `gorm:"column:is_master;index:idx_cluster_master_nodes,priority:1;index:idx_cluster_master_current,priority:1"`
	ClientCount int       `gorm:"column:client_count;index:idx_cluster_live_schedule,priority:1"`
	StartedAt   time.Time `gorm:"column:started_at;index:idx_cluster_auth_lookup,priority:4;index:idx_cluster_live_nodes,priority:2;index:idx_cluster_master_nodes,priority:3;index:idx_cluster_auth_live_first,priority:3;index:idx_cluster_live_schedule,priority:2;index:idx_cluster_master_current,priority:3"`
	LastSeenAt  time.Time `gorm:"column:last_seen_at;index:idx_cluster_auth_lookup,priority:3;index:idx_cluster_live_nodes,priority:1;index:idx_cluster_master_nodes,priority:2;index:idx_cluster_auth_live_first,priority:5;index:idx_cluster_live_schedule,priority:5;index:idx_cluster_master_current,priority:2,sort:desc"`
}

// TableName returns the database table name.
func (ClusterNodeRecord) TableName() string {
	return "cluster"
}

type CPANodeRecord struct {
	HomeIP                 string    `gorm:"column:home_ip;primaryKey;index:idx_cpa_node_home_live,priority:1;index:idx_cpa_node_live,priority:3"`
	HomePort               int       `gorm:"column:home_port;primaryKey;index:idx_cpa_node_home_live,priority:2;index:idx_cpa_node_live,priority:4"`
	HomeStartedAt          time.Time `gorm:"column:home_started_at;primaryKey"`
	NodeKey                string    `gorm:"column:node_key;primaryKey;size:256"`
	NodeID                 string    `gorm:"column:node_id;index"`
	ClientIP               string    `gorm:"column:client_ip;index"`
	ClientCount            int       `gorm:"column:client_count"`
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint"`
	OpenConnections        int       `gorm:"column:open_connections"`
	ActiveHandlers         int       `gorm:"column:active_handlers"`
	LatestCancelRevision   int64     `gorm:"column:latest_cancel_revision"`
	ConnectedAt            time.Time `gorm:"column:connected_at"`
	LastSeenAt             time.Time `gorm:"column:last_seen_at;index:idx_cpa_node_home_live,priority:3;index:idx_cpa_node_live,priority:1"`
	CreatedAt              time.Time `gorm:"column:created_at"`
	UpdatedAt              time.Time `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (CPANodeRecord) TableName() string {
	return "cpa_node"
}

type ClusterEventRecord struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement;index:idx_cluster_events_scope_id,priority:2"`
	Scope      string    `gorm:"column:scope;index:idx_cluster_events_scope_id,priority:1"`
	Op         string    `gorm:"column:op"`
	EntityUUID string    `gorm:"column:entity_uuid"`
	Version    int64     `gorm:"column:version"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName returns the database table name.
func (ClusterEventRecord) TableName() string {
	return "cluster_events"
}

type OAuthSessionRecord struct {
	State       string     `gorm:"column:state;primaryKey"`
	Provider    string     `gorm:"column:provider;index"`
	Status      string     `gorm:"column:status"`
	Error       string     `gorm:"column:error"`
	Data        JSONB      `gorm:"column:data"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
	ExpiresAt   time.Time  `gorm:"column:expires_at;index"`
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

// TableName returns the database table name.
func (OAuthSessionRecord) TableName() string {
	return "oauth_sessions"
}

type CertificateRecord struct {
	ID                     string    `gorm:"column:id;primaryKey;index:idx_certificate_ca_order,priority:3"`
	ClusterID              string    `gorm:"column:cluster_id;index"`
	CertificatePEM         string    `gorm:"column:certificate_pem;type:text"`
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;index;index:idx_certificate_peer_lookup,priority:1"`
	PrivateKeyPEM          string    `gorm:"column:private_key_pem;type:text"`
	CSRPEM                 string    `gorm:"column:csr_pem;type:text"`
	IP                     string    `gorm:"column:ip;index:idx_certificate_server_ip,priority:2"`
	CAFingerprint          string    `gorm:"column:ca_fingerprint"`
	EnrollmentSecretHash   string    `gorm:"column:enrollment_secret_hash"`
	IsCA                   bool      `gorm:"column:is_ca;index;index:idx_certificate_ca_order,priority:1"`
	IsServer               bool      `gorm:"column:is_server;index:idx_certificate_server_ip,priority:1;index:idx_certificate_peer_lookup,priority:3"`
	IsClient               bool      `gorm:"column:is_client;index;index:idx_certificate_peer_lookup,priority:2"`
	SerialNumber           string    `gorm:"column:serial_number"`
	NotBefore              time.Time `gorm:"column:not_before"`
	NotAfter               time.Time `gorm:"column:not_after"`
	CreatedAt              time.Time `gorm:"column:created_at;index:idx_certificate_ca_order,priority:2"`
	UpdatedAt              time.Time `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (CertificateRecord) TableName() string {
	return "certificate"
}

type AuthIndex struct {
	UUID           string
	Version        int64
	ID             string
	Index          string
	Provider       string
	Label          string
	Prefix         string
	Status         coreauth.Status
	Disabled       bool
	Unavailable    bool
	NextRetryAfter time.Time
	Quota          coreauth.QuotaState
	ModelStates    map[string]*coreauth.ModelState
	BaseURL        string
	ModelsHash     string
	Attributes     map[string]string
	ModelMetadata  map[string]any
}
