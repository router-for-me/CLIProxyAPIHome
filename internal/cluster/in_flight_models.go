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

// ManagementInFlightSnapshotCursorRecord stores a short-lived immutable
// Management API view used to keep offset pagination on one observation.
type ManagementInFlightSnapshotCursorRecord struct {
	Cursor    string    `gorm:"column:cursor;primaryKey;size:64"`
	Payload   JSONB     `gorm:"column:payload;not null"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null;index"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index"`
}

func (ManagementInFlightSnapshotCursorRecord) TableName() string {
	return "management_in_flight_snapshot_cursors"
}
