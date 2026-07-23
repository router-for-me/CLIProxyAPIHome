package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConcurrencyCredentialNotFound     = errors.New("credential concurrency policy credential not found")
	ErrConcurrencyDuplicateModel         = errors.New("duplicate credential concurrency model")
	ErrConcurrencyInvalidLimit           = errors.New("invalid credential concurrency limit")
	ErrConcurrencyInvalidModel           = errors.New("invalid credential concurrency model")
	ErrConcurrencyLegacyMembershipActive = errors.New("legacy CPA membership is active")
	ErrConcurrencyHomeCapabilityMissing  = errors.New("active Home lacks credential concurrency limiter capability")
	ErrConcurrencyPolicyVersionConflict  = errors.New("credential concurrency policy version conflict")
)

// OptionalLimit distinguishes an omitted limit from JSON null and a numeric value.
type OptionalLimit struct {
	Set   bool
	Null  bool
	Value int64
}

// UnmarshalJSON records JSON field presence for a policy limit.
func (o *OptionalLimit) UnmarshalJSON(data []byte) error {
	if o == nil {
		return fmt.Errorf("optional limit is nil")
	}
	o.Set = true
	o.Null = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	o.Value = 0
	if o.Null {
		return nil
	}
	return json.Unmarshal(data, &o.Value)
}

// OptionalModelLimitMap distinguishes an omitted model map from JSON null and a map value.
type OptionalModelLimitMap struct {
	Set   bool
	Null  bool
	Value map[string]*int64
}

// UnmarshalJSON records JSON field presence for model policy limits.
func (o *OptionalModelLimitMap) UnmarshalJSON(data []byte) error {
	if o == nil {
		return fmt.Errorf("optional model limit map is nil")
	}
	o.Set = true
	o.Null = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	o.Value = nil
	if o.Null {
		return nil
	}
	return json.Unmarshal(data, &o.Value)
}

// ConcurrencyPolicyPatch is a presence-aware replacement patch for a credential policy.
type ConcurrencyPolicyPatch struct {
	MaxInFlight        OptionalLimit         `json:"max_in_flight"`
	MaxInFlightByModel OptionalModelLimitMap `json:"max_in_flight_by_model"`
}

// CredentialConcurrencyPolicy is the authoritative stored policy for one credential.
type CredentialConcurrencyPolicy struct {
	CredentialID               string           `json:"credential_id"`
	MaxInFlight                *int64           `json:"max_in_flight"`
	MaxInFlightByModel         map[string]int64 `json:"max_in_flight_by_model"`
	Version                    int64            `json:"version"`
	EffectiveAt                time.Time        `json:"effective_at"`
	ObservationBarrierRevision int64            `json:"observation_barrier_revision"`
}

type normalizedConcurrencyPolicyPatch struct {
	maxInFlightSet        bool
	maxInFlight           *int64
	maxInFlightByModelSet bool
	maxInFlightByModel    map[string]int64
}

type concurrencyPolicyLockOrderContextKey struct{}

type concurrencyPolicyPatchAuthLockAcquiredContextKey struct{}

func recordConcurrencyPolicyLock(ctx context.Context, step string) {
	record, okRecord := contextOrBackground(ctx).Value(concurrencyPolicyLockOrderContextKey{}).(func(string))
	if okRecord && record != nil {
		record(step)
	}
}

func waitForConcurrencyPolicyPatchAuthLockAcquired(ctx context.Context) {
	wait, okWait := contextOrBackground(ctx).Value(concurrencyPolicyPatchAuthLockAcquiredContextKey{}).(func())
	if okWait && wait != nil {
		wait()
	}
}

// GetCredentialConcurrencyPolicy returns one consistent policy version without applying admission behavior.
func (r *Repository) GetCredentialConcurrencyPolicy(ctx context.Context, credentialID string) (CredentialConcurrencyPolicy, error) {
	db, errDB := r.database()
	if errDB != nil {
		return CredentialConcurrencyPolicy{}, errDB
	}
	ctx = contextOrBackground(ctx)
	policy := CredentialConcurrencyPolicy{}
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var errGet error
		policy, errGet = getCredentialConcurrencyPolicy(ctx, tx, credentialID, true)
		return errGet
	})
	if errTransaction != nil {
		return CredentialConcurrencyPolicy{}, errTransaction
	}
	return policy, nil
}

// PatchCredentialConcurrencyPolicy atomically validates and replaces policy fields for a credential.
func (r *Repository) PatchCredentialConcurrencyPolicy(ctx context.Context, credentialID string, patch ConcurrencyPolicyPatch, expectedVersion *int64) (CredentialConcurrencyPolicy, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return CredentialConcurrencyPolicy{}, ErrConcurrencyCredentialNotFound
	}
	if errValidate := validateConcurrencyPolicyPatchModelKeys(patch); errValidate != nil {
		return CredentialConcurrencyPolicy{}, errValidate
	}
	db, errDB := r.database()
	if errDB != nil {
		return CredentialConcurrencyPolicy{}, errDB
	}
	policy := CredentialConcurrencyPolicy{}
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		return r.patchCredentialConcurrencyPolicyTx(ctx, tx, credentialID, patch, expectedVersion, &policy)
	})
	if errTransaction != nil {
		return CredentialConcurrencyPolicy{}, errTransaction
	}
	return policy, nil
}

// patchCredentialConcurrencyPolicyTx applies a policy patch within the caller's concurrency transaction.
func (r *Repository) patchCredentialConcurrencyPolicyTx(ctx context.Context, tx *gorm.DB, credentialID string, patch ConcurrencyPolicyPatch, expectedVersion *int64, policy *CredentialConcurrencyPolicy) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	gate, errGate := lockConcurrencyActivationGate(tx)
	if errGate != nil {
		return errGate
	}
	return r.patchCredentialConcurrencyPolicyWithLockedActivationGateTx(ctx, tx, gate, credentialID, patch, expectedVersion, policy)
}

func (r *Repository) patchCredentialConcurrencyPolicyWithLockedActivationGateTx(ctx context.Context, tx *gorm.DB, gate *ConcurrencyActivationGateRecord, credentialID string, patch ConcurrencyPolicyPatch, expectedVersion *int64, policy *CredentialConcurrencyPolicy) error {
	return r.patchCredentialConcurrencyPolicyWithAuthScopeTx(ctx, tx, gate, credentialID, patch, expectedVersion, policy, false)
}

// clearRetiredCredentialConcurrencyPolicyWithLockedActivationGateTx clears a retired credential policy without restoring its auth row.
func (r *Repository) clearRetiredCredentialConcurrencyPolicyWithLockedActivationGateTx(ctx context.Context, tx *gorm.DB, gate *ConcurrencyActivationGateRecord, credentialID string, policy *CredentialConcurrencyPolicy) error {
	patch := ConcurrencyPolicyPatch{
		MaxInFlight:        OptionalLimit{Set: true, Null: true},
		MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: make(map[string]*int64)},
	}
	return r.patchCredentialConcurrencyPolicyWithAuthScopeTx(ctx, tx, gate, credentialID, patch, nil, policy, true)
}

func (r *Repository) patchCredentialConcurrencyPolicyWithAuthScopeTx(ctx context.Context, tx *gorm.DB, gate *ConcurrencyActivationGateRecord, credentialID string, patch ConcurrencyPolicyPatch, expectedVersion *int64, policy *CredentialConcurrencyPolicy, includeRetiredAuth bool) error {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return ErrConcurrencyCredentialNotFound
	}
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if gate == nil {
		return fmt.Errorf("concurrency activation gate is nil")
	}
	if policy == nil {
		return fmt.Errorf("credential concurrency policy result is nil")
	}
	if errValidate := validateConcurrencyPolicyPatchModelKeys(patch); errValidate != nil {
		return errValidate
	}
	lifecycle, errLifecycle := homeIncarnationLifecycleConfig(tx)
	if errLifecycle != nil {
		return errLifecycle
	}
	lifecycleConfig, errLifecycleConfig := lifecycleConfigFromRecord(lifecycle)
	if errLifecycleConfig != nil {
		return errLifecycleConfig
	}
	normalized, errNormalize := normalizeConcurrencyPolicyPatch(patch, lifecycleConfig.MaxLimit)
	if errNormalize != nil {
		return errNormalize
	}
	auth := AuthRecord{}
	authQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
	if includeRetiredAuth {
		authQuery = authQuery.Unscoped()
	}
	errAuth := authQuery.First(&auth, "uuid = ?", credentialID).Error
	if errors.Is(errAuth, gorm.ErrRecordNotFound) {
		return ErrConcurrencyCredentialNotFound
	}
	if errAuth != nil {
		return errAuth
	}
	if includeRetiredAuth && (!auth.DeletedAt.Valid || !concurrencyPolicyPatchIsClearOnly(normalized)) {
		return ErrConcurrencyCredentialNotFound
	}
	recordConcurrencyPolicyLock(ctx, "auth")
	if errHook := recordConcurrencyContenderBackendPID(ctx, tx); errHook != nil {
		return errHook
	}
	waitForConcurrencyPolicyPatchAuthLockAcquired(ctx)
	if concurrencyPolicyPatchCanActivate(normalized) {
		if errSafety := verifyConcurrencyPolicyActivation(ctx, tx, lifecycle.NodeHeartbeatTimeout); errSafety != nil {
			return errSafety
		}
	}
	recordConcurrencyPolicyLock(ctx, "policy")
	barrier, errBarrier := lockConcurrencyObservationBarrier(tx)
	if errBarrier != nil {
		return errBarrier
	}

	record, models, exists, errLoad := loadCredentialConcurrencyPolicyForUpdate(tx, credentialID)
	if errLoad != nil {
		return errLoad
	}
	if expectedVersion != nil {
		currentVersion := int64(0)
		if exists {
			currentVersion = record.Version
		}
		if *expectedVersion != currentVersion {
			return ErrConcurrencyPolicyVersionConflict
		}
	}
	if !normalized.maxInFlightSet && !normalized.maxInFlightByModelSet {
		*policy = credentialConcurrencyPolicyFromRecords(credentialID, record, models, exists)
		return nil
	}

	nextMaxInFlight := record.MaxInFlight
	if normalized.maxInFlightSet {
		nextMaxInFlight = normalized.maxInFlight
	}
	nextModels := models
	if normalized.maxInFlightByModelSet {
		nextModels = normalized.maxInFlightByModel
	}
	nextActive := nextMaxInFlight != nil || len(nextModels) != 0
	currentActive := exists && (record.MaxInFlight != nil || len(models) != 0)

	now, errNow := DatabaseNow(contextOrBackground(ctx), tx)
	if errNow != nil {
		return errNow
	}
	barrier.Revision++
	if errSaveBarrier := tx.Save(barrier).Error; errSaveBarrier != nil {
		return errSaveBarrier
	}
	if !exists {
		record = CredentialConcurrencyPolicyRecord{CredentialID: credentialID, Version: 1, EffectiveAt: now}
	} else {
		record.Version++
		record.EffectiveAt = now
	}
	record.MaxInFlight = cloneInt64(nextMaxInFlight)
	record.ObservationBarrierRevision = barrier.Revision
	if errSavePolicy := tx.Save(&record).Error; errSavePolicy != nil {
		return errSavePolicy
	}
	if normalized.maxInFlightByModelSet {
		if errDelete := tx.Where("credential_id = ?", credentialID).Delete(&CredentialConcurrencyModelPolicyRecord{}).Error; errDelete != nil {
			return errDelete
		}
		for model, limit := range nextModels {
			modelRecord := CredentialConcurrencyModelPolicyRecord{CredentialID: credentialID, Model: model, MaxInFlight: limit}
			if errCreate := tx.Create(&modelRecord).Error; errCreate != nil {
				return errCreate
			}
		}
	}
	if currentActive != nextActive {
		if nextActive {
			gate.ActivePolicyCount++
		} else {
			gate.ActivePolicyCount--
		}
		if gate.ActivePolicyCount < 0 {
			return fmt.Errorf("concurrency activation gate active policy count is negative")
		}
		if errSaveGate := tx.Save(gate).Error; errSaveGate != nil {
			return errSaveGate
		}
	}
	if errEvent := appendEvent(tx, "config", "concurrency-policy", credentialID, barrier.Revision); errEvent != nil {
		return errEvent
	}
	*policy = credentialConcurrencyPolicyFromRecords(credentialID, record, nextModels, true)
	return nil
}

func normalizeConcurrencyPolicyPatch(patch ConcurrencyPolicyPatch, maxLimit int64) (normalizedConcurrencyPolicyPatch, error) {
	normalized := normalizedConcurrencyPolicyPatch{
		maxInFlightSet:        patch.MaxInFlight.Set,
		maxInFlightByModelSet: patch.MaxInFlightByModel.Set,
	}
	if patch.MaxInFlight.Set && !patch.MaxInFlight.Null {
		if errLimit := validateConcurrencyPolicyLimit(patch.MaxInFlight.Value, maxLimit); errLimit != nil {
			return normalizedConcurrencyPolicyPatch{}, errLimit
		}
		if patch.MaxInFlight.Value > 0 {
			normalized.maxInFlight = int64PointerForPolicy(patch.MaxInFlight.Value)
		}
	}
	if !patch.MaxInFlightByModel.Set || patch.MaxInFlightByModel.Null {
		return normalized, nil
	}
	normalized.maxInFlightByModel = make(map[string]int64, len(patch.MaxInFlightByModel.Value))
	for rawModel, limit := range patch.MaxInFlightByModel.Value {
		model, valid := concurrency.ValidCanonicalConcurrencyModelKey(rawModel)
		if !valid {
			return normalizedConcurrencyPolicyPatch{}, ErrConcurrencyInvalidModel
		}
		if _, duplicate := normalized.maxInFlightByModel[model]; duplicate {
			return normalizedConcurrencyPolicyPatch{}, fmt.Errorf("%w: %s", ErrConcurrencyDuplicateModel, model)
		}
		if limit == nil || *limit == 0 {
			normalized.maxInFlightByModel[model] = 0
			continue
		}
		if errLimit := validateConcurrencyPolicyLimit(*limit, maxLimit); errLimit != nil {
			return normalizedConcurrencyPolicyPatch{}, errLimit
		}
		normalized.maxInFlightByModel[model] = *limit
	}
	for model, limit := range normalized.maxInFlightByModel {
		if limit == 0 {
			delete(normalized.maxInFlightByModel, model)
		}
	}
	return normalized, nil
}

func validateConcurrencyPolicyPatchModelKeys(patch ConcurrencyPolicyPatch) error {
	if !patch.MaxInFlightByModel.Set || patch.MaxInFlightByModel.Null {
		return nil
	}
	models := make(map[string]struct{}, len(patch.MaxInFlightByModel.Value))
	for rawModel := range patch.MaxInFlightByModel.Value {
		model, valid := concurrency.ValidCanonicalConcurrencyModelKey(rawModel)
		if !valid {
			return ErrConcurrencyInvalidModel
		}
		if _, duplicate := models[model]; duplicate {
			return fmt.Errorf("%w: %s", ErrConcurrencyDuplicateModel, model)
		}
		models[model] = struct{}{}
	}
	return nil
}

func concurrencyPolicyPatchCanActivate(patch normalizedConcurrencyPolicyPatch) bool {
	return patch.maxInFlight != nil || len(patch.maxInFlightByModel) != 0
}

func concurrencyPolicyPatchIsClearOnly(patch normalizedConcurrencyPolicyPatch) bool {
	return patch.maxInFlightSet && patch.maxInFlight == nil && patch.maxInFlightByModelSet && len(patch.maxInFlightByModel) == 0
}

func validateConcurrencyPolicyLimit(limit int64, maxLimit int64) error {
	if maxLimit < 1 || maxLimit > concurrency.MaxConfiguredLimit {
		return fmt.Errorf("credential concurrency maximum limit is invalid")
	}
	if limit < 0 || limit > maxLimit {
		return fmt.Errorf("%w: must be between 0 and %d", ErrConcurrencyInvalidLimit, maxLimit)
	}
	return nil
}

func lockConcurrencyActivationGate(tx *gorm.DB) (*ConcurrencyActivationGateRecord, error) {
	gate := &ConcurrencyActivationGateRecord{ID: 1}
	if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(gate).Error; errCreate != nil {
		return nil, errCreate
	}
	if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(gate, "id = ?", 1).Error; errLock != nil {
		return nil, errLock
	}
	return gate, nil
}

func lockConcurrencyObservationBarrier(tx *gorm.DB) (*ConcurrencyObservationBarrierRecord, error) {
	barrier := &ConcurrencyObservationBarrierRecord{ID: 1}
	if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(barrier).Error; errCreate != nil {
		return nil, errCreate
	}
	if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(barrier, "id = ?", 1).Error; errLock != nil {
		return nil, errLock
	}
	return barrier, nil
}

func loadCredentialConcurrencyPolicyForUpdate(tx *gorm.DB, credentialID string) (CredentialConcurrencyPolicyRecord, map[string]int64, bool, error) {
	record := CredentialConcurrencyPolicyRecord{}
	errPolicy := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "credential_id = ?", credentialID).Error
	if errPolicy != nil && !errors.Is(errPolicy, gorm.ErrRecordNotFound) {
		return CredentialConcurrencyPolicyRecord{}, nil, false, errPolicy
	}
	exists := errPolicy == nil
	models, errModels := loadCredentialConcurrencyModels(tx, credentialID, true)
	if errModels != nil {
		return CredentialConcurrencyPolicyRecord{}, nil, false, errModels
	}
	return record, models, exists, nil
}

func loadCredentialConcurrencyModels(db *gorm.DB, credentialID string, forUpdate bool) (map[string]int64, error) {
	query := db.Where("credential_id = ?", credentialID).Order("model ASC")
	if forUpdate {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var records []CredentialConcurrencyModelPolicyRecord
	if errFind := query.Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	models := make(map[string]int64, len(records))
	for _, record := range records {
		models[record.Model] = record.MaxInFlight
	}
	return models, nil
}

func verifyConcurrencyPolicyActivation(ctx context.Context, tx *gorm.DB, nodeHeartbeatTimeout time.Duration) error {
	if errExpire := expireStaleHomeIncarnations(ctx, tx, nodeHeartbeatTimeout); errExpire != nil {
		return errExpire
	}
	var nonProtocolOneMemberships int64
	if errCount := tx.Model(&CPANodeMembershipRecord{}).Where("state = ? AND protocol_version <> ?", MembershipStateActive, 1).Count(&nonProtocolOneMemberships).Error; errCount != nil {
		return errCount
	}
	if nonProtocolOneMemberships != 0 {
		return ErrConcurrencyLegacyMembershipActive
	}

	recordConcurrencyPolicyLock(ctx, "homes")
	var homes []HomeProcessIncarnationRecord
	if errFind := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("state = ?", HomeIncarnationActive).Find(&homes).Error; errFind != nil {
		return errFind
	}
	for _, home := range homes {
		if !homeHasConcurrencyLimiterCapability(home) {
			return ErrConcurrencyHomeCapabilityMissing
		}
	}
	if tx.Dialector != nil && tx.Dialector.Name() == "sqlite" && len(homes) > 1 {
		return ErrConcurrencySQLiteMultiHome
	}
	return nil
}

func homeHasConcurrencyLimiterCapability(home HomeProcessIncarnationRecord) bool {
	var capabilities []string
	if errUnmarshal := json.Unmarshal(home.Capabilities, &capabilities); errUnmarshal != nil {
		return false
	}
	for _, capability := range capabilities {
		if capability == credentialConcurrencyLimitsCapability {
			return true
		}
	}
	return false
}

func getCredentialConcurrencyPolicy(ctx context.Context, db *gorm.DB, credentialID string, forUpdate bool) (CredentialConcurrencyPolicy, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return CredentialConcurrencyPolicy{}, ErrConcurrencyCredentialNotFound
	}
	authQuery := db.WithContext(ctx)
	if forUpdate {
		authQuery = authQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	auth := AuthRecord{}
	errAuth := authQuery.First(&auth, "uuid = ?", credentialID).Error
	if errors.Is(errAuth, gorm.ErrRecordNotFound) {
		return CredentialConcurrencyPolicy{}, ErrConcurrencyCredentialNotFound
	}
	if errAuth != nil {
		return CredentialConcurrencyPolicy{}, errAuth
	}
	policyQuery := db.WithContext(ctx)
	if forUpdate {
		policyQuery = policyQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	record := CredentialConcurrencyPolicyRecord{}
	errPolicy := policyQuery.First(&record, "credential_id = ?", credentialID).Error
	if errors.Is(errPolicy, gorm.ErrRecordNotFound) {
		return CredentialConcurrencyPolicy{CredentialID: credentialID, MaxInFlightByModel: map[string]int64{}}, nil
	}
	if errPolicy != nil {
		return CredentialConcurrencyPolicy{}, errPolicy
	}
	models, errModels := loadCredentialConcurrencyModels(db.WithContext(ctx), credentialID, forUpdate)
	if errModels != nil {
		return CredentialConcurrencyPolicy{}, errModels
	}
	return credentialConcurrencyPolicyFromRecords(credentialID, record, models, true), nil
}

func credentialConcurrencyPolicyFromRecords(credentialID string, record CredentialConcurrencyPolicyRecord, models map[string]int64, exists bool) CredentialConcurrencyPolicy {
	policy := CredentialConcurrencyPolicy{CredentialID: credentialID, MaxInFlightByModel: make(map[string]int64, len(models))}
	for model, limit := range models {
		policy.MaxInFlightByModel[model] = limit
	}
	if !exists {
		return policy
	}
	policy.MaxInFlight = cloneInt64(record.MaxInFlight)
	policy.Version = record.Version
	policy.EffectiveAt = record.EffectiveAt
	policy.ObservationBarrierRevision = record.ObservationBarrierRevision
	return policy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func int64PointerForPolicy(value int64) *int64 {
	return &value
}

func (r *Repository) concurrencyObservationBarrierRevision(ctx context.Context) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	record := ConcurrencyObservationBarrierRecord{}
	errFind := db.WithContext(contextOrBackground(ctx)).First(&record, "id = ?", 1).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if errFind != nil {
		return 0, errFind
	}
	return record.Revision, nil
}
