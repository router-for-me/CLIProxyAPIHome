package cluster

import "time"

type CPAInFlightSnapshotRecord struct {
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;primaryKey;size:64"`
	NodeID                 string    `gorm:"column:node_id;size:256;not null"`
	MembershipConnectedAt  time.Time `gorm:"column:membership_connected_at;not null;index"`
	Revision               int64     `gorm:"column:revision;not null"`
	ObservedAt             time.Time `gorm:"column:observed_at;not null"`
	BarrierRevision        int64     `gorm:"column:barrier_revision;not null"`
	DetailsTruncated       bool      `gorm:"column:details_truncated;not null"`
	Payload                JSONB     `gorm:"column:payload;not null"`
	UpdatedAt              time.Time `gorm:"column:updated_at;not null;index;autoUpdateTime:false"`
}

func (CPAInFlightSnapshotRecord) TableName() string {
	return "cpa_in_flight_snapshots"
}

type CPAInFlightSnapshotAttemptRecord struct {
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;primaryKey;size:64"`
	MembershipConnectedAt  time.Time `gorm:"column:membership_connected_at;primaryKey"`
	HighestSeenRevision    int64     `gorm:"column:highest_seen_revision;not null"`
	State                  string    `gorm:"column:state;size:16;not null;check:state IN ('staging','complete','overflow','rejected')"`
	ObservedAt             time.Time `gorm:"column:observed_at;not null"`
	BarrierRevision        int64     `gorm:"column:barrier_revision;not null"`
	PartCount              int       `gorm:"column:part_count;not null"`
	ReceivedPartCount      int       `gorm:"column:received_part_count;not null"`
	EncodedBytes           int64     `gorm:"column:encoded_bytes;not null"`
	AggregateGroupCount    int       `gorm:"column:aggregate_group_count;not null"`
	DetailsTruncated       bool      `gorm:"column:details_truncated;not null"`
	UpdatedAt              time.Time `gorm:"column:updated_at;not null;index;autoUpdateTime:false"`
}

func (CPAInFlightSnapshotAttemptRecord) TableName() string {
	return "cpa_in_flight_snapshot_attempts"
}

type CPAInFlightSnapshotPartRecord struct {
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;primaryKey;size:64"`
	MembershipConnectedAt  time.Time `gorm:"column:membership_connected_at;primaryKey"`
	Revision               int64     `gorm:"column:revision;primaryKey"`
	PartIndex              int       `gorm:"column:part_index;primaryKey"`
	Payload                []byte    `gorm:"column:payload;not null"`
	EncodedBytes           int       `gorm:"column:encoded_bytes;not null"`
	CreatedAt              time.Time `gorm:"column:created_at;not null;autoCreateTime:false"`
	UpdatedAt              time.Time `gorm:"column:updated_at;not null;index;autoUpdateTime:false"`
}

func (CPAInFlightSnapshotPartRecord) TableName() string {
	return "cpa_in_flight_snapshot_parts"
}

// ManagementInFlightSnapshotCursorRecord stores the header for a short-lived
// immutable Management API view used to keep pagination on one observation.
type ManagementInFlightSnapshotCursorRecord struct {
	Cursor                          string     `gorm:"column:cursor;primaryKey;size:64"`
	SchemaVersion                   int        `gorm:"column:schema_version;not null;default:0"`
	CredentialID                    string     `gorm:"column:credential_id;not null;default:''"`
	Model                           string     `gorm:"column:model;not null;default:''"`
	ObservedAt                      *time.Time `gorm:"column:observed_at"`
	FreshUntil                      *time.Time `gorm:"column:fresh_until"`
	Stale                           bool       `gorm:"column:stale;not null;default:false"`
	CoverageComplete                bool       `gorm:"column:coverage_complete;not null;default:false"`
	AggregatesComplete              bool       `gorm:"column:aggregates_complete;not null;default:false"`
	ProtocolCoverageComplete        bool       `gorm:"column:protocol_coverage_complete;not null;default:false"`
	MinimumProcessedBarrierRevision *int64     `gorm:"column:minimum_processed_barrier_revision"`
	DetailsTruncated                bool       `gorm:"column:details_truncated;not null;default:false"`
	Total                           int64      `gorm:"column:total;not null;default:0"`
	Payload                         JSONB      `gorm:"column:payload;not null"`
	ExpiresAt                       time.Time  `gorm:"column:expires_at;not null;index"`
	CreatedAt                       time.Time  `gorm:"column:created_at;not null;index"`
}

func (ManagementInFlightSnapshotCursorRecord) TableName() string {
	return "management_in_flight_snapshot_cursors"
}

// ManagementInFlightSnapshotCursorItemRecord stores one stable request row.
type ManagementInFlightSnapshotCursorItemRecord struct {
	Cursor          string    `gorm:"column:cursor;primaryKey;size:64"`
	Ordinal         int64     `gorm:"column:ordinal;primaryKey"`
	RequestID       string    `gorm:"column:request_id;not null"`
	CredentialID    string    `gorm:"column:credential_id;not null;index"`
	Model           string    `gorm:"column:model;not null"`
	RequestKind     string    `gorm:"column:request_kind;not null"`
	StartedAt       time.Time `gorm:"column:started_at;not null"`
	ObservedPresent bool      `gorm:"column:observed_present;not null"`
	StatePresent    bool      `gorm:"column:state_present;not null"`
}

func (ManagementInFlightSnapshotCursorItemRecord) TableName() string {
	return "management_in_flight_snapshot_cursor_items"
}

// ManagementInFlightSnapshotCursorObservedRecord stores the minimum
// per-credential observation required to recompute limiter diagnostics.
type ManagementInFlightSnapshotCursorObservedRecord struct {
	Cursor              string `gorm:"column:cursor;primaryKey;size:64"`
	CredentialID        string `gorm:"column:credential_id;primaryKey;size:256"`
	ObservedInFlight    int64  `gorm:"column:observed_in_flight;not null"`
	ObservedAccounted   int64  `gorm:"column:observed_accounted;not null"`
	ObservedUnaccounted int64  `gorm:"column:observed_unaccounted;not null"`
}

func (ManagementInFlightSnapshotCursorObservedRecord) TableName() string {
	return "management_in_flight_snapshot_cursor_observed"
}

// ManagementInFlightSnapshotCursorStateRecord stores one pinned authoritative
// credential limiter state.
type ManagementInFlightSnapshotCursorStateRecord struct {
	Cursor             string    `gorm:"column:cursor;primaryKey;size:64"`
	CredentialID       string    `gorm:"column:credential_id;primaryKey;size:256"`
	MaxInFlight        *int64    `gorm:"column:max_in_flight"`
	AdmittedInFlight   int64     `gorm:"column:admitted_in_flight;not null"`
	PolicyVersion      int64     `gorm:"column:policy_version;not null"`
	EffectiveAt        time.Time `gorm:"column:effective_at;not null"`
	ObservationBarrier int64     `gorm:"column:observation_barrier;not null"`
	ModelCount         int64     `gorm:"column:model_count;not null"`
}

func (ManagementInFlightSnapshotCursorStateRecord) TableName() string {
	return "management_in_flight_snapshot_cursor_states"
}

// ManagementInFlightSnapshotCursorStateModelRecord stores one pinned model
// limiter row for a credential state.
type ManagementInFlightSnapshotCursorStateModelRecord struct {
	Cursor           string `gorm:"column:cursor;primaryKey;size:64"`
	CredentialID     string `gorm:"column:credential_id;primaryKey;size:256"`
	Model            string `gorm:"column:model;primaryKey;size:256"`
	MaxInFlight      int64  `gorm:"column:max_in_flight;not null"`
	AdmittedInFlight int64  `gorm:"column:admitted_in_flight;not null"`
}

func (ManagementInFlightSnapshotCursorStateModelRecord) TableName() string {
	return "management_in_flight_snapshot_cursor_state_models"
}
