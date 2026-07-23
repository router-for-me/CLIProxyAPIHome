package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MembershipStateActive    = "active"
	MembershipStateCanceling = "canceling"
	MembershipStateClosed    = "closed"
)

var ErrLifecycleConfigInUse = errors.New("lifecycle configuration is in use")

// ValidateCredentialConcurrencyLifecycle verifies the lifecycle timing safety invariant.
func ValidateCredentialConcurrencyLifecycle(nodeHeartbeatTimeout time.Duration, cfg config.CredentialConcurrencyConfig) error {
	if nodeHeartbeatTimeout <= 0 || cfg.CPAHeartbeatTimeout <= 0 || cfg.CPACancelBound <= 0 || cfg.ReclaimGrace <= 0 || cfg.CleanupInterval <= 0 {
		return fmt.Errorf("credential concurrency lifecycle durations must be positive")
	}
	left, errLeft := addPositiveDuration(nodeHeartbeatTimeout, cfg.ReclaimGrace)
	if errLeft != nil {
		return fmt.Errorf("node heartbeat timeout plus reclaim grace: %w", errLeft)
	}
	right, errRight := addPositiveDuration(cfg.CPAHeartbeatTimeout, cfg.CPACancelBound)
	if errRight != nil {
		return fmt.Errorf("CPA heartbeat timeout plus cancel bound: %w", errRight)
	}
	if left <= right {
		return fmt.Errorf("node heartbeat timeout plus reclaim grace must exceed CPA heartbeat timeout plus cancel bound")
	}
	return nil
}

func addPositiveDuration(left time.Duration, right time.Duration) (time.Duration, error) {
	if left > time.Duration(math.MaxInt64)-right {
		return 0, fmt.Errorf("duration overflow")
	}
	return left + right, nil
}

// EnsureLifecycleConfig creates the singleton lifecycle configuration when absent.
func (r *Repository) EnsureLifecycleConfig(ctx context.Context, nodeHeartbeatTimeout time.Duration) (LifecycleConfigRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return LifecycleConfigRecord{}, errDB
	}
	if nodeHeartbeatTimeout <= 0 {
		return LifecycleConfigRecord{}, fmt.Errorf("node heartbeat timeout must be positive")
	}

	var record LifecycleConfigRecord
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		var errEnsure error
		record, errEnsure = ensureLifecycleConfigTx(tx, nodeHeartbeatTimeout)
		return errEnsure
	})
	if errTransaction != nil {
		return LifecycleConfigRecord{}, errTransaction
	}
	return record, nil
}

func ensureLifecycleConfigTx(tx *gorm.DB, nodeHeartbeatTimeout time.Duration) (LifecycleConfigRecord, error) {
	if tx == nil {
		return LifecycleConfigRecord{}, fmt.Errorf("database connection is nil")
	}
	if nodeHeartbeatTimeout <= 0 {
		return LifecycleConfigRecord{}, fmt.Errorf("node heartbeat timeout must be positive")
	}

	defaults := config.DefaultCredentialConcurrencyConfig()
	payload, errPayload := lifecycleConfigPayload(defaults, 1)
	if errPayload != nil {
		return LifecycleConfigRecord{}, errPayload
	}
	candidate := LifecycleConfigRecord{
		ID:                   1,
		Revision:             1,
		NodeHeartbeatTimeout: nodeHeartbeatTimeout,
		Payload:              payload,
	}
	if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; errCreate != nil {
		return LifecycleConfigRecord{}, errCreate
	}

	record := LifecycleConfigRecord{}
	if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "id = ?", 1).Error; errLock != nil {
		return LifecycleConfigRecord{}, errLock
	}
	if record.NodeHeartbeatTimeout != nodeHeartbeatTimeout {
		return LifecycleConfigRecord{}, fmt.Errorf("node heartbeat timeout does not match lifecycle configuration")
	}
	cfg, errConfig := lifecycleConfigFromRecord(record)
	if errConfig != nil {
		return LifecycleConfigRecord{}, errConfig
	}
	if errValidate := ValidateCredentialConcurrencyLifecycle(nodeHeartbeatTimeout, cfg); errValidate != nil {
		return LifecycleConfigRecord{}, errValidate
	}
	return record, nil
}

// UpdateLifecycleConfig validates and persists a lifecycle configuration revision.
func (r *Repository) UpdateLifecycleConfig(ctx context.Context, nodeHeartbeatTimeout time.Duration, next config.CredentialConcurrencyConfig) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}

	var revision int64
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		var errUpdate error
		revision, _, errUpdate = updateLifecycleConfigTx(ctx, tx, nodeHeartbeatTimeout, next)
		return errUpdate
	})
	if errTransaction != nil {
		return 0, errTransaction
	}
	return revision, nil
}

func updateLifecycleConfigTx(ctx context.Context, tx *gorm.DB, nodeHeartbeatTimeout time.Duration, next config.CredentialConcurrencyConfig) (int64, bool, error) {
	record, errEnsure := ensureLifecycleConfigTx(tx, nodeHeartbeatTimeout)
	if errEnsure != nil {
		return 0, false, errEnsure
	}
	if errValidate := ValidateCredentialConcurrencyLifecycle(nodeHeartbeatTimeout, next); errValidate != nil {
		return 0, false, errValidate
	}
	if errValidate := config.ValidateCredentialConcurrencyConfig(next); errValidate != nil {
		return 0, false, errValidate
	}

	next.ObservationBarrierRevision = 0
	current, errCurrent := lifecycleConfigFromRecord(record)
	if errCurrent != nil {
		return 0, false, errCurrent
	}
	if lifecycleConfigEqual(current, next) {
		return record.Revision, false, nil
	}

	safetyChanged := lifecycleConfigSafetyChanged(current, next)
	if safetyChanged {
		var inUse int64
		if errCount := tx.WithContext(contextOrBackground(ctx)).Model(&CPANodeMembershipRecord{}).Where("state IN ?", []string{MembershipStateActive, MembershipStateCanceling}).Count(&inUse).Error; errCount != nil {
			return 0, false, errCount
		}
		if inUse != 0 {
			return 0, false, ErrLifecycleConfigInUse
		}
	}

	revision := record.Revision
	if safetyChanged {
		revision++
	}
	payload, errPayload := lifecycleConfigPayload(next, revision)
	if errPayload != nil {
		return 0, false, errPayload
	}
	updates := map[string]any{"payload": payload}
	if safetyChanged {
		updates["revision"] = revision
	}
	if errUpdate := tx.WithContext(contextOrBackground(ctx)).Model(&LifecycleConfigRecord{}).Where("id = ?", 1).Updates(updates).Error; errUpdate != nil {
		return 0, false, errUpdate
	}
	return revision, true, nil
}

// LifecycleConfig returns the configuration synthesized from the singleton row.
func (r *Repository) LifecycleConfig(ctx context.Context) (config.CredentialConcurrencyConfig, error) {
	db, errDB := r.database()
	if errDB != nil {
		return config.CredentialConcurrencyConfig{}, errDB
	}
	record := LifecycleConfigRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).First(&record, "id = ?", 1).Error; errFirst != nil {
		return config.CredentialConcurrencyConfig{}, errFirst
	}
	return lifecycleConfigFromRecord(record)
}

func lifecycleConfigPayload(cfg config.CredentialConcurrencyConfig, revision int64) (JSONB, error) {
	cfg.LifecycleConfigRevision = revision
	cfg.ObservationBarrierRevision = 0
	rawPayload, errMarshal := json.Marshal(cfg)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return JSONB(rawPayload), nil
}

func lifecycleConfigFromRecord(record LifecycleConfigRecord) (config.CredentialConcurrencyConfig, error) {
	cfg := config.DefaultCredentialConcurrencyConfig()
	if errUnmarshal := json.Unmarshal(record.Payload, &cfg); errUnmarshal != nil {
		return config.CredentialConcurrencyConfig{}, errUnmarshal
	}
	cfg.LifecycleConfigRevision = record.Revision
	cfg.ObservationBarrierRevision = 0
	return cfg, nil
}

func lifecycleConfigSafetyChanged(left config.CredentialConcurrencyConfig, right config.CredentialConcurrencyConfig) bool {
	return left.CPAHeartbeatTimeout != right.CPAHeartbeatTimeout ||
		left.CPACancelBound != right.CPACancelBound ||
		left.ReclaimGrace != right.ReclaimGrace ||
		left.CleanupInterval != right.CleanupInterval
}

func lifecycleConfigEqual(left config.CredentialConcurrencyConfig, right config.CredentialConcurrencyConfig) bool {
	return left.CPAHeartbeatTimeout == right.CPAHeartbeatTimeout &&
		left.CPACancelBound == right.CPACancelBound &&
		left.ReclaimGrace == right.ReclaimGrace &&
		left.CleanupInterval == right.CleanupInterval &&
		left.ReleaseFlushInterval == right.ReleaseFlushInterval &&
		left.ReleaseMaxBackoff == right.ReleaseMaxBackoff &&
		left.BusyRetryMin == right.BusyRetryMin &&
		left.BusyRetryMax == right.BusyRetryMax &&
		left.MaxLimit == right.MaxLimit
}

func credentialConcurrencyConfigFromValue(value any) (config.CredentialConcurrencyConfig, error) {
	if cfg, okConfig := value.(config.CredentialConcurrencyConfig); okConfig {
		return cfg, nil
	}
	cfg := config.DefaultCredentialConcurrencyConfig()
	data, errMarshal := yaml.Marshal(value)
	if errMarshal != nil {
		return config.CredentialConcurrencyConfig{}, errMarshal
	}
	if errUnmarshal := yaml.Unmarshal(data, &cfg); errUnmarshal != nil {
		return config.CredentialConcurrencyConfig{}, errUnmarshal
	}
	return cfg, nil
}

func (r *Repository) lifecycleConfigNodeHeartbeatTimeout(ctx context.Context) (time.Duration, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	record := LifecycleConfigRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).First(&record, "id = ?", 1).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return DefaultHeartbeatTimeout(), nil
	}
	if errFirst != nil {
		return 0, errFirst
	}
	return record.NodeHeartbeatTimeout, nil
}

// WithConcurrencyActivationGate runs fn while holding the singleton activation gate lock.
func (r *Repository) WithConcurrencyActivationGate(ctx context.Context, fn func(*gorm.DB, *ConcurrencyActivationGateRecord) error) error {
	if fn == nil {
		return fmt.Errorf("concurrency activation gate callback is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		gate := &ConcurrencyActivationGateRecord{ID: 1}
		if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(gate).Error; errCreate != nil {
			return errCreate
		}
		if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(gate, "id = ?", 1).Error; errLock != nil {
			return errLock
		}
		return fn(tx, gate)
	})
}
