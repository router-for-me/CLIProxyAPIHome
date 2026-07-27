package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConcurrencyNodeUnavailable = errors.New("credential concurrency node is unavailable")
	ErrConcurrencyCounterInvalid  = errors.New("credential concurrency counter is invalid")
)

var sqliteConcurrencyTransactionMu sync.Mutex

type concurrencyAdmissionPolicyLockAcquiredContextKey struct{}
type concurrencyContenderBackendPIDContextKey struct{}
type concurrencyReleaseContenderBackendPIDContextKey struct{}

func waitForConcurrencyAdmissionPolicyLock(ctx context.Context) {
	wait, okWait := contextOrBackground(ctx).Value(concurrencyAdmissionPolicyLockAcquiredContextKey{}).(func())
	if okWait && wait != nil {
		wait()
	}
}

func recordConcurrencyContenderBackendPID(ctx context.Context, tx *gorm.DB) error {
	record, okRecord := contextOrBackground(ctx).Value(concurrencyContenderBackendPIDContextKey{}).(func(*gorm.DB) error)
	if !okRecord || record == nil {
		return nil
	}
	return record(tx)
}

func recordConcurrencyReleaseContenderBackendPID(ctx context.Context, tx *gorm.DB) error {
	record, okRecord := contextOrBackground(ctx).Value(concurrencyReleaseContenderBackendPIDContextKey{}).(func(*gorm.DB) error)
	if !okRecord || record == nil {
		return nil
	}
	return record(tx)
}

// ConcurrencyAdmissionRequest identifies one credential request that may need concurrency accounting.
type ConcurrencyAdmissionRequest struct {
	CredentialID    string
	Model           string
	Lifetime        ConnectionLifetime
	ProtocolVersion int
}

// ConcurrencyAdmissionResult describes the canonical credential and model admitted by the limiter.
type ConcurrencyAdmissionResult struct {
	Accounted    bool
	CredentialID string
	Model        string
}

// ConcurrencyAdmissionError is a typed admission failure suitable for protocol translation.
type ConcurrencyAdmissionError struct {
	Type         string
	RetryAfterMS int64
	cause        error
}

func (e *ConcurrencyAdmissionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Type, e.cause)
	}
	return e.Type
}

// ConcurrencyType returns the stable protocol error type without exposing its cause.
func (e *ConcurrencyAdmissionError) ConcurrencyType() string {
	if e == nil {
		return ""
	}
	return e.Type
}

func (e *ConcurrencyAdmissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newAdmissionError(errorType string, cause error) error {
	return &ConcurrencyAdmissionError{Type: errorType, cause: cause}
}

func newSaturatedError(errorType string, retryAfterMS int64) error {
	return &ConcurrencyAdmissionError{Type: errorType, RetryAfterMS: retryAfterMS}
}

// IsConcurrencySaturated reports whether an admission failed because a configured limit was reached.
func IsConcurrencySaturated(err error) bool {
	var admissionErr *ConcurrencyAdmissionError
	if !errors.As(err, &admissionErr) {
		return false
	}
	return admissionErr.Type == "credential_concurrency_exceeded" || admissionErr.Type == "credential_model_concurrency_exceeded"
}

// withConcurrencyTransaction serializes SQLite limiter transactions in one Home process.
func withConcurrencyTransaction(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if fn == nil {
		return fmt.Errorf("concurrency transaction callback is nil")
	}
	if db.Dialector != nil && db.Dialector.Name() == "sqlite" {
		sqliteConcurrencyTransactionMu.Lock()
		defer sqliteConcurrencyTransactionMu.Unlock()
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(fn)
}

// LockActiveConcurrencyLifetimeTx locks and verifies the active membership and Home that accepted a command.
func (r *Repository) LockActiveConcurrencyLifetimeTx(ctx context.Context, tx *gorm.DB, lifetime ConnectionLifetime) error {
	if tx == nil || !lifetime.Controlled || strings.TrimSpace(lifetime.Fingerprint) == "" || lifetime.ConnectedAt.IsZero() {
		return ErrConcurrencyNodeUnavailable
	}
	if strings.TrimSpace(lifetime.Home.IP) == "" || lifetime.Home.Port <= 0 || lifetime.Home.StartedAt.IsZero() {
		return ErrConcurrencyNodeUnavailable
	}

	var membership CPANodeMembershipRecord
	errMembership := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("certificate_fingerprint = ? AND connected_at = ? AND state = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", strings.TrimSpace(lifetime.Fingerprint), lifetime.ConnectedAt, MembershipStateActive, strings.TrimSpace(lifetime.Home.IP), lifetime.Home.Port, lifetime.Home.StartedAt).
		First(&membership).Error
	if errMembership != nil {
		return ErrConcurrencyNodeUnavailable
	}

	var home HomeProcessIncarnationRecord
	errHome := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("home_ip = ? AND home_port = ? AND started_at = ?", strings.TrimSpace(lifetime.Home.IP), lifetime.Home.Port, lifetime.Home.StartedAt).
		First(&home).Error
	if errHome != nil || home.State != HomeIncarnationActive {
		return ErrConcurrencyNodeUnavailable
	}
	return nil
}

// AdmitCredentialConcurrency atomically checks configured limits and records one admitted request.
func (r *Repository) AdmitCredentialConcurrency(ctx context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
	credentialID := strings.TrimSpace(req.CredentialID)
	model, validModel := concurrency.ValidCanonicalConcurrencyModelKey(req.Model)
	result := ConcurrencyAdmissionResult{CredentialID: credentialID, Model: model}
	if !validModel {
		return result, newAdmissionError("concurrency_invalid_model", ErrConcurrencyInvalidModel)
	}

	db, errDB := r.database()
	if errDB != nil {
		return result, errDB
	}
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		if errLifetime := r.LockActiveConcurrencyLifetimeTx(ctx, tx, req.Lifetime); errLifetime != nil {
			return newAdmissionError("concurrency_node_unavailable", errLifetime)
		}
		if errHook := recordConcurrencyContenderBackendPID(ctx, tx); errHook != nil {
			return errHook
		}
		policy, models, errPolicy := lockConcurrencyPolicyTx(ctx, tx, credentialID)
		if errPolicy != nil {
			return newAdmissionError("concurrency_tracker_unavailable", errPolicy)
		}
		waitForConcurrencyAdmissionPolicyLock(ctx)

		credentialLimit := positiveLimit(policy.MaxInFlight)
		modelLimit := positiveLimitValue(models[model])
		if credentialLimit == 0 && modelLimit == 0 {
			return nil
		}
		if req.ProtocolVersion != 1 {
			return newAdmissionError("concurrency_protocol_required", ErrConcurrencyProtocolRequired)
		}

		credentialActive, modelActive, errCounts := sumConcurrencyCountsTx(ctx, tx, credentialID, model)
		if errCounts != nil {
			return newAdmissionError("concurrency_tracker_unavailable", errCounts)
		}
		if credentialLimit > 0 && credentialActive >= credentialLimit {
			return newSaturatedError("credential_concurrency_exceeded", 0)
		}
		if modelLimit > 0 && modelActive >= modelLimit {
			return newSaturatedError("credential_model_concurrency_exceeded", 0)
		}
		if errIncrement := incrementConcurrencyCounterTx(ctx, tx, credentialID, model, strings.TrimSpace(req.Lifetime.Fingerprint)); errIncrement != nil {
			return newAdmissionError("concurrency_tracker_unavailable", errIncrement)
		}
		result.Accounted = true
		return nil
	})
	return result, errTransaction
}

func lockConcurrencyPolicyTx(ctx context.Context, tx *gorm.DB, credentialID string) (CredentialConcurrencyPolicyRecord, map[string]int64, error) {
	if tx == nil {
		return CredentialConcurrencyPolicyRecord{}, nil, fmt.Errorf("database transaction is nil")
	}
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return CredentialConcurrencyPolicyRecord{}, nil, ErrConcurrencyCredentialNotFound
	}

	policy := CredentialConcurrencyPolicyRecord{}
	errPolicy := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&policy, "credential_id = ?", credentialID).Error
	if errors.Is(errPolicy, gorm.ErrRecordNotFound) {
		return CredentialConcurrencyPolicyRecord{}, map[string]int64{}, nil
	}
	if errPolicy != nil {
		return CredentialConcurrencyPolicyRecord{}, nil, errPolicy
	}
	models, errModels := loadCredentialConcurrencyModels(tx.WithContext(contextOrBackground(ctx)), credentialID, true)
	if errModels != nil {
		return CredentialConcurrencyPolicyRecord{}, nil, errModels
	}
	return policy, models, nil
}

func positiveLimit(value *int64) int64 {
	if value == nil || *value <= 0 {
		return 0
	}
	return *value
}

func positiveLimitValue(value int64) int64 {
	if value <= 0 {
		return 0
	}
	return value
}

func sumConcurrencyCountsTx(ctx context.Context, tx *gorm.DB, credentialID string, model string) (int64, int64, error) {
	if tx == nil {
		return 0, 0, fmt.Errorf("database transaction is nil")
	}
	query := tx.WithContext(contextOrBackground(ctx)).Model(&CredentialConcurrencyCounterRecord{}).Where("credential_id = ?", credentialID)
	var minimum int64
	if errMinimum := query.Select("COALESCE(MIN(active_count), 0)").Scan(&minimum).Error; errMinimum != nil {
		return 0, 0, errMinimum
	}
	if minimum < 0 {
		return 0, 0, ErrConcurrencyCounterInvalid
	}

	var credentialActive int64
	if errCredential := query.Select("COALESCE(SUM(active_count), 0)").Scan(&credentialActive).Error; errCredential != nil {
		return 0, 0, errCredential
	}
	var modelActive int64
	if errModel := query.Where("model = ?", model).Select("COALESCE(SUM(active_count), 0)").Scan(&modelActive).Error; errModel != nil {
		return 0, 0, errModel
	}
	if credentialActive < 0 || modelActive < 0 {
		return 0, 0, ErrConcurrencyCounterInvalid
	}
	return credentialActive, modelActive, nil
}

func incrementConcurrencyCounterTx(ctx context.Context, tx *gorm.DB, credentialID string, model string, fingerprint string) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if credentialID == "" || model == "" || fingerprint == "" {
		return ErrConcurrencyCounterInvalid
	}

	counter := CredentialConcurrencyCounterRecord{}
	errCounter := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("credential_id = ? AND model = ? AND certificate_fingerprint = ?", credentialID, model, fingerprint).
		First(&counter).Error
	now, errNow := DatabaseNow(contextOrBackground(ctx), tx)
	if errNow != nil {
		return errNow
	}
	if errors.Is(errCounter, gorm.ErrRecordNotFound) {
		return tx.WithContext(contextOrBackground(ctx)).Create(&CredentialConcurrencyCounterRecord{
			CredentialID: credentialID, Model: model, CertificateFingerprint: fingerprint, ActiveCount: 1, UpdatedAt: now,
		}).Error
	}
	if errCounter != nil {
		return errCounter
	}
	if counter.ActiveCount < 0 || counter.ActiveCount == math.MaxInt64 {
		return ErrConcurrencyCounterInvalid
	}
	counter.ActiveCount++
	counter.UpdatedAt = now
	return tx.WithContext(contextOrBackground(ctx)).Save(&counter).Error
}

// CredentialConcurrencyState is the authoritative policy and admitted-count state for one credential.
type CredentialConcurrencyState struct {
	CredentialID       string
	MaxInFlight        *int64
	AdmittedInFlight   int64
	PolicyVersion      int64
	EffectiveAt        time.Time
	ObservationBarrier int64
	Models             []CredentialConcurrencyModelState
}

// CredentialConcurrencyModelState is the authoritative policy and admitted count for one model.
type CredentialConcurrencyModelState struct {
	Model            string
	MaxInFlight      int64
	AdmittedInFlight int64
}

// ReadConcurrencyState returns policy and admitted counter state without relying on CPA observations.
func (r *Repository) ReadConcurrencyState(ctx context.Context) ([]CredentialConcurrencyState, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	tx := readOnlyRepeatableReadTransaction(ctx, db)
	if tx.Error != nil {
		return nil, tx.Error
	}
	var states []CredentialConcurrencyState
	errSnapshot := func() error {
		var policies []CredentialConcurrencyPolicyRecord
		if errPolicies := tx.WithContext(contextOrBackground(ctx)).Order("credential_id ASC").Find(&policies).Error; errPolicies != nil {
			return errPolicies
		}
		for _, policy := range policies {
			models, errModels := loadCredentialConcurrencyModels(tx.WithContext(contextOrBackground(ctx)), policy.CredentialID, false)
			if errModels != nil {
				return errModels
			}
			credentialActive, _, errCounts := sumConcurrencyCountsTx(ctx, tx, policy.CredentialID, "")
			if errCounts != nil {
				return errCounts
			}
			state := CredentialConcurrencyState{
				CredentialID: policy.CredentialID, MaxInFlight: cloneInt64(policy.MaxInFlight), AdmittedInFlight: credentialActive,
				PolicyVersion: policy.Version, EffectiveAt: policy.EffectiveAt, ObservationBarrier: policy.ObservationBarrierRevision,
				Models: make([]CredentialConcurrencyModelState, 0, len(models)),
			}
			modelKeys := make([]string, 0, len(models))
			for model := range models {
				modelKeys = append(modelKeys, model)
			}
			sort.Strings(modelKeys)
			for _, model := range modelKeys {
				_, modelActive, errModelCounts := sumConcurrencyCountsTx(ctx, tx, policy.CredentialID, model)
				if errModelCounts != nil {
					return errModelCounts
				}
				state.Models = append(state.Models, CredentialConcurrencyModelState{Model: model, MaxInFlight: models[model], AdmittedInFlight: modelActive})
			}
			states = append(states, state)
		}
		return nil
	}()
	if errSnapshot != nil {
		if errRollback := tx.Rollback().Error; errRollback != nil {
			return nil, fmt.Errorf("read concurrency state: %w; rollback: %v", errSnapshot, errRollback)
		}
		return nil, errSnapshot
	}
	if errCommit := tx.Commit().Error; errCommit != nil {
		return nil, errCommit
	}
	return states, nil
}
