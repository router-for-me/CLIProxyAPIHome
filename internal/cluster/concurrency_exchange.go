package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

const credentialConcurrencyPoliciesRootKey = "credential-concurrency-policies"

var ErrCredentialConcurrencyOrphan = errors.New("credential concurrency policy would be orphaned")

// CredentialConcurrencyExchangePolicy is the portable policy representation for one credential.
type CredentialConcurrencyExchangePolicy struct {
	MaxInFlight        *int64           `yaml:"max-in-flight,omitempty" json:"max-in-flight,omitempty"`
	MaxInFlightByModel map[string]int64 `yaml:"max-in-flight-by-model,omitempty" json:"max-in-flight-by-model,omitempty"`
}

// ConcurrencyCredentialReferenceChecker prevents replacement of credentials still used by limiter state.
type ConcurrencyCredentialReferenceChecker struct{}

// HasCredentialReferences reports whether a policy or dynamic counter still uses a credential.
func (ConcurrencyCredentialReferenceChecker) HasCredentialReferences(ctx context.Context, tx *gorm.DB, credentialID string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("database connection is nil")
	}
	var policyCount int64
	if errCount := tx.WithContext(contextOrBackground(ctx)).Model(&CredentialConcurrencyPolicyRecord{}).Where("credential_id = ?", credentialID).Count(&policyCount).Error; errCount != nil {
		return false, errCount
	}
	if policyCount != 0 {
		return true, nil
	}
	var counterCount int64
	if errCount := tx.WithContext(contextOrBackground(ctx)).Model(&CredentialConcurrencyCounterRecord{}).Where("credential_id = ?", credentialID).Count(&counterCount).Error; errCount != nil {
		return false, errCount
	}
	return counterCount != 0, nil
}

// ListCredentialConcurrencyPolicies returns all stored policy rows, including inactive rows retained for identity safety.
func (r *Repository) ListCredentialConcurrencyPolicies(ctx context.Context) ([]CredentialConcurrencyPolicy, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	tx := readOnlyRepeatableReadTransaction(ctx, db)
	if tx.Error != nil {
		return nil, tx.Error
	}
	policies, errPolicies := listCredentialConcurrencyPoliciesTx(ctx, tx)
	if errPolicies != nil {
		if errRollback := tx.Rollback().Error; errRollback != nil {
			return nil, fmt.Errorf("list credential concurrency policies: %w; rollback: %v", errPolicies, errRollback)
		}
		return nil, errPolicies
	}
	if errCommit := tx.Commit().Error; errCommit != nil {
		return nil, errCommit
	}
	return policies, nil
}

// ExportCredentialConcurrencyPolicies adds all active limiter policies to an exchange config root.
func (r *Repository) ExportCredentialConcurrencyPolicies(ctx context.Context, root map[string]any) error {
	if root == nil {
		return fmt.Errorf("config root is nil")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	tx := readOnlyRepeatableReadTransaction(ctx, db)
	if tx.Error != nil {
		return tx.Error
	}
	policies, errPolicies := listCredentialConcurrencyPoliciesTx(ctx, tx)
	if errPolicies != nil {
		if errRollback := tx.Rollback().Error; errRollback != nil {
			return fmt.Errorf("export credential concurrency policies: %w; rollback: %v", errPolicies, errRollback)
		}
		return errPolicies
	}
	var credentialIDs []string
	if errFind := tx.WithContext(contextOrBackground(ctx)).Model(&AuthRecord{}).Pluck("uuid", &credentialIDs).Error; errFind != nil {
		if errRollback := tx.Rollback().Error; errRollback != nil {
			return fmt.Errorf("export credential concurrency policies: %w; rollback: %v", errFind, errRollback)
		}
		return errFind
	}
	if errCommit := tx.Commit().Error; errCommit != nil {
		return errCommit
	}
	activeCredentials := make(map[string]struct{}, len(credentialIDs))
	for _, credentialID := range credentialIDs {
		activeCredentials[credentialID] = struct{}{}
	}
	out := make(map[string]CredentialConcurrencyExchangePolicy)
	for _, policy := range policies {
		if _, active := activeCredentials[policy.CredentialID]; !active {
			continue
		}
		if policy.MaxInFlight == nil && len(policy.MaxInFlightByModel) == 0 {
			continue
		}
		item := CredentialConcurrencyExchangePolicy{MaxInFlightByModel: cloneConcurrencyModelLimits(policy.MaxInFlightByModel)}
		if policy.MaxInFlight != nil && *policy.MaxInFlight > 0 {
			value := *policy.MaxInFlight
			item.MaxInFlight = &value
		}
		out[policy.CredentialID] = item
	}
	root[credentialConcurrencyPoliciesRootKey] = out
	return nil
}

// ImportCredentialConcurrencyPolicies imports the policy section in its own concurrency transaction.
func (r *Repository) ImportCredentialConcurrencyPolicies(ctx context.Context, root map[string]any) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		return r.ImportCredentialConcurrencyPoliciesTx(ctx, tx, root)
	})
}

// ImportCredentialConcurrencyPoliciesTx imports policies within a caller-owned concurrency transaction.
func (r *Repository) ImportCredentialConcurrencyPoliciesTx(ctx context.Context, tx *gorm.DB, root map[string]any) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	gate, errGate := lockConcurrencyActivationGate(tx)
	if errGate != nil {
		return errGate
	}
	return r.importCredentialConcurrencyPoliciesWithLockedActivationGateTx(ctx, tx, gate, root)
}

func (r *Repository) importCredentialConcurrencyPoliciesWithLockedActivationGateTx(ctx context.Context, tx *gorm.DB, gate *ConcurrencyActivationGateRecord, root map[string]any) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if gate == nil {
		return fmt.Errorf("concurrency activation gate is nil")
	}
	_, present := root[credentialConcurrencyPoliciesRootKey]
	if !present {
		return nil
	}
	policies, errPolicies := parseCredentialConcurrencyExchangePolicies(root[credentialConcurrencyPoliciesRootKey])
	if errPolicies != nil {
		return errPolicies
	}

	existing, errExisting := listCredentialConcurrencyPoliciesTx(ctx, tx)
	if errExisting != nil {
		return errExisting
	}
	for credentialID := range policies {
		if errCredential := ensureCredentialConcurrencyPolicyCredential(ctx, tx, credentialID); errCredential != nil {
			return errCredential
		}
	}

	credentialIDs := make([]string, 0, len(existing)+len(policies))
	seen := make(map[string]struct{}, len(existing)+len(policies))
	for _, policy := range existing {
		seen[policy.CredentialID] = struct{}{}
		credentialIDs = append(credentialIDs, policy.CredentialID)
	}
	for credentialID := range policies {
		if _, exists := seen[credentialID]; !exists {
			credentialIDs = append(credentialIDs, credentialID)
		}
	}
	sort.Strings(credentialIDs)

	for _, credentialID := range credentialIDs {
		exchangePolicy, exists := policies[credentialID]
		patch := concurrencyExchangePolicyPatch(exchangePolicy, exists)
		policy := CredentialConcurrencyPolicy{}
		errPatch := r.patchCredentialConcurrencyPolicyWithLockedActivationGateTx(ctx, tx, gate, credentialID, patch, nil, &policy)
		if !exists && errors.Is(errPatch, ErrConcurrencyCredentialNotFound) {
			errPatch = r.clearRetiredCredentialConcurrencyPolicyWithLockedActivationGateTx(ctx, tx, gate, credentialID, &policy)
		}
		if errPatch != nil {
			return errPatch
		}
	}
	return nil
}

func parseCredentialConcurrencyExchangePolicies(value any) (map[string]CredentialConcurrencyExchangePolicy, error) {
	if value == nil {
		return map[string]CredentialConcurrencyExchangePolicy{}, nil
	}
	data, errMarshal := yaml.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	policies := make(map[string]CredentialConcurrencyExchangePolicy)
	if errUnmarshal := yaml.Unmarshal(data, &policies); errUnmarshal != nil {
		return nil, fmt.Errorf("parse %s: %w", credentialConcurrencyPoliciesRootKey, errUnmarshal)
	}
	normalized := make(map[string]CredentialConcurrencyExchangePolicy, len(policies))
	for credentialID, policy := range policies {
		credentialID = strings.TrimSpace(credentialID)
		if credentialID == "" {
			return nil, fmt.Errorf("parse %s: credential id is required", credentialConcurrencyPoliciesRootKey)
		}
		if _, duplicate := normalized[credentialID]; duplicate {
			return nil, fmt.Errorf("parse %s: duplicate credential id %s", credentialConcurrencyPoliciesRootKey, credentialID)
		}
		normalized[credentialID] = policy
	}
	return normalized, nil
}

func listCredentialConcurrencyPoliciesTx(ctx context.Context, tx *gorm.DB) ([]CredentialConcurrencyPolicy, error) {
	var records []CredentialConcurrencyPolicyRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).Order("credential_id ASC").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	var modelRecords []CredentialConcurrencyModelPolicyRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).Order("credential_id ASC, model ASC").Find(&modelRecords).Error; errFind != nil {
		return nil, errFind
	}
	modelsByCredential := make(map[string]map[string]int64, len(records))
	for _, record := range modelRecords {
		models := modelsByCredential[record.CredentialID]
		if models == nil {
			models = make(map[string]int64)
			modelsByCredential[record.CredentialID] = models
		}
		models[record.Model] = record.MaxInFlight
	}
	policies := make([]CredentialConcurrencyPolicy, 0, len(records))
	for _, record := range records {
		policies = append(policies, credentialConcurrencyPolicyFromRecords(record.CredentialID, record, modelsByCredential[record.CredentialID], true))
	}
	return policies, nil
}

func ensureCredentialConcurrencyPolicyCredential(ctx context.Context, tx *gorm.DB, credentialID string) error {
	var count int64
	if errCount := tx.WithContext(contextOrBackground(ctx)).Model(&AuthRecord{}).Where("uuid = ?", credentialID).Count(&count).Error; errCount != nil {
		return errCount
	}
	if count == 0 {
		return ErrConcurrencyCredentialNotFound
	}
	return nil
}

func concurrencyExchangePolicyPatch(policy CredentialConcurrencyExchangePolicy, exists bool) ConcurrencyPolicyPatch {
	patch := ConcurrencyPolicyPatch{
		MaxInFlight:        OptionalLimit{Set: true, Null: true},
		MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: make(map[string]*int64)},
	}
	if !exists {
		return patch
	}
	if policy.MaxInFlight != nil {
		patch.MaxInFlight = OptionalLimit{Set: true, Value: *policy.MaxInFlight}
	}
	for model, limit := range policy.MaxInFlightByModel {
		limitCopy := limit
		patch.MaxInFlightByModel.Value[model] = &limitCopy
	}
	return patch
}

func cloneConcurrencyModelLimits(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for model, limit := range in {
		out[model] = limit
	}
	return out
}
