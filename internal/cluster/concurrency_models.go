package cluster

import "time"

// CredentialConcurrencyPolicyRecord stores a credential's total concurrency policy.
type CredentialConcurrencyPolicyRecord struct {
	CredentialID               string    `gorm:"column:credential_id;primaryKey;size:128"`
	MaxInFlight                *int64    `gorm:"column:max_in_flight"`
	Version                    int64     `gorm:"column:version;not null"`
	EffectiveAt                time.Time `gorm:"column:effective_at;not null"`
	ObservationBarrierRevision int64     `gorm:"column:observation_barrier_revision;not null;default:0"`
	CreatedAt                  time.Time `gorm:"column:created_at"`
	UpdatedAt                  time.Time `gorm:"column:updated_at"`
}

func (CredentialConcurrencyPolicyRecord) TableName() string {
	return "credential_concurrency_policies"
}

// CredentialConcurrencyModelPolicyRecord stores a credential's model concurrency policy.
type CredentialConcurrencyModelPolicyRecord struct {
	CredentialID string `gorm:"column:credential_id;primaryKey;size:128"`
	Model        string `gorm:"column:model;primaryKey;size:256"`
	MaxInFlight  int64  `gorm:"column:max_in_flight;not null"`
}

func (CredentialConcurrencyModelPolicyRecord) TableName() string {
	return "credential_concurrency_model_policies"
}

// CredentialConcurrencyCounterRecord is reserved for aggregate limiter counters.
type CredentialConcurrencyCounterRecord struct {
	CredentialID           string    `gorm:"column:credential_id;primaryKey;size:128"`
	Model                  string    `gorm:"column:model;primaryKey;size:256"`
	CertificateFingerprint string    `gorm:"column:certificate_fingerprint;primaryKey;size:64"`
	ActiveCount            int64     `gorm:"column:active_count;not null"`
	LastReleaseSeq         int64     `gorm:"column:last_release_seq;not null"`
	UpdatedAt              time.Time `gorm:"column:updated_at;not null"`
}

func (CredentialConcurrencyCounterRecord) TableName() string {
	return "credential_concurrency_counters"
}

// ConcurrencyObservationBarrierRecord keeps the global observation barrier revision.
type ConcurrencyObservationBarrierRecord struct {
	ID        int       `gorm:"column:id;primaryKey"`
	Revision  int64     `gorm:"column:revision;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (ConcurrencyObservationBarrierRecord) TableName() string {
	return "credential_concurrency_observation_barrier"
}
