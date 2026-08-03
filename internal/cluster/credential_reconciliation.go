package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrCredentialUUIDMappingRequired = errors.New("credential uuid mapping required")
	ErrCredentialValidation          = errors.New("credential validation failed")
	ErrCredentialIdentityConflict    = errors.New("credential identity conflict")
)

// CredentialReferenceChecker reports whether another store still references a credential.
type CredentialReferenceChecker interface {
	HasCredentialReferences(context.Context, *gorm.DB, string) (bool, error)
}

// ListAuthsUnscoped lists active and soft-deleted credentials for identity checks.
func (r *Repository) ListAuthsUnscoped(ctx context.Context, tx *gorm.DB) ([]*coreauth.Auth, error) {
	db := tx
	if db == nil {
		var errDB error
		db, errDB = r.database()
		if errDB != nil {
			return nil, errDB
		}
	}

	var records []AuthRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Unscoped().Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	auths := make([]*coreauth.Auth, 0, len(records))
	for i := range records {
		auth, errAuth := RecordToAuth(&records[i])
		if errAuth != nil {
			return nil, errAuth
		}
		auth.ID = records[i].UUID
		auth.Index = records[i].UUID
		auths = append(auths, auth)
	}
	return auths, nil
}

// ReconcileProviderAuths atomically updates one provider config credential set.
func (r *Repository) ReconcileProviderAuths(ctx context.Context, key string, next []*coreauth.Auth, checker CredentialReferenceChecker) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	ctx = contextOrBackground(ctx)
	if checker == nil {
		checker = ConcurrencyCredentialReferenceChecker{}
	}
	return withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		if _, errGate := lockConcurrencyActivationGate(tx); errGate != nil {
			return errGate
		}
		return mapCredentialConcurrencyOrphan(r.reconcileProviderAuthsWithLockedActivationGateTx(ctx, tx, key, next, checker))
	})
}

func (r *Repository) reconcileProviderAuthsWithLockedActivationGateTx(ctx context.Context, tx *gorm.DB, key string, next []*coreauth.Auth, checker CredentialReferenceChecker) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	existing, errList := r.ListAuthsUnscoped(ctx, tx)
	if errList != nil {
		return errList
	}
	existingByID := make(map[string]*coreauth.Auth, len(existing))
	for _, auth := range existing {
		if auth != nil {
			existingByID[auth.ID] = auth
		}
	}

	nextIDs := make(map[string]struct{}, len(next))
	for _, auth := range next {
		if auth == nil || auth.ID == "" || auth.ID != auth.Index {
			return fmt.Errorf("%w: provider credential id is required and must match index", ErrCredentialValidation)
		}
		parsed, errUUID := uuid.Parse(auth.ID)
		if errUUID != nil || parsed.String() != auth.ID {
			return fmt.Errorf("%w: provider credential id must be a canonical UUID", ErrCredentialValidation)
		}
		if !isProviderAuthForConfigKey(auth, key) {
			return fmt.Errorf("%w: credential does not belong to provider config key %s", ErrCredentialValidation, key)
		}
		if _, duplicate := nextIDs[auth.ID]; duplicate {
			return fmt.Errorf("%w: duplicate provider credential id: %s", ErrCredentialIdentityConflict, auth.ID)
		}
		if previous := existingByID[auth.ID]; previous != nil && (!isProviderAuthForConfigKey(previous, key) || previous.Provider != auth.Provider) {
			return fmt.Errorf("%w: provider credential id belongs to another auth: %s", ErrCredentialIdentityConflict, auth.ID)
		}
		nextIDs[auth.ID] = struct{}{}
	}

	providerIDs := make([]string, 0, len(existing))
	for _, auth := range existing {
		if isProviderAuthForConfigKey(auth, key) {
			providerIDs = append(providerIDs, auth.ID)
		}
	}
	activeByID, errLock := lockProviderAuthsForReconciliationTx(ctx, tx, providerIDs)
	if errLock != nil {
		return errLock
	}

	for _, auth := range existing {
		if !isProviderAuthForConfigKey(auth, key) {
			continue
		}
		if _, active := activeByID[auth.ID]; !active {
			continue
		}
		if _, kept := nextIDs[auth.ID]; kept {
			continue
		}
		if checker != nil {
			referenced, errReferences := checker.HasCredentialReferences(ctx, tx, auth.ID)
			if errReferences != nil {
				return errReferences
			}
			if referenced {
				return fmt.Errorf("%w: %s", ErrCredentialUUIDMappingRequired, auth.ID)
			}
		}
		if errRetire := retireProviderAuthTx(ctx, tx, auth.ID); errRetire != nil {
			return errRetire
		}
	}

	txRepo := NewRepository(tx)
	for _, auth := range next {
		if auth == nil {
			continue
		}
		delete(auth.Attributes, "provider_credential_id_generated")
		if _, errUpsert := txRepo.UpsertAuthPreservingDisabled(ctx, auth, "upsert"); errUpsert != nil {
			return errUpsert
		}
	}
	return nil
}

func (r *Repository) lockAllProviderAuthsForReconciliationTx(ctx context.Context, tx *gorm.DB) error {
	existing, errList := r.ListAuthsUnscoped(ctx, tx)
	if errList != nil {
		return errList
	}
	credentialIDs := make([]string, 0, len(existing))
	for _, auth := range existing {
		if isAnyProviderConfigAuth(auth) {
			credentialIDs = append(credentialIDs, auth.ID)
		}
	}
	_, errLock := lockProviderAuthsForReconciliationTx(ctx, tx, credentialIDs)
	return errLock
}

func lockProviderAuthsForReconciliationTx(ctx context.Context, tx *gorm.DB, credentialIDs []string) (map[string]struct{}, error) {
	activeByID := make(map[string]struct{}, len(credentialIDs))
	if len(credentialIDs) == 0 {
		return activeByID, nil
	}
	sort.Strings(credentialIDs)
	locked := make([]AuthRecord, 0, len(credentialIDs))
	if errFind := tx.WithContext(contextOrBackground(ctx)).Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).Where("uuid IN ?", credentialIDs).Order("uuid ASC").Find(&locked).Error; errFind != nil {
		return nil, errFind
	}
	for _, record := range locked {
		if !record.DeletedAt.Valid {
			activeByID[record.UUID] = struct{}{}
		}
	}
	return activeByID, nil
}

// RetireProviderAuth soft-deletes a provider credential without removing related state.
func (r *Repository) RetireProviderAuth(ctx context.Context, credentialID string) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return retireProviderAuthTx(ctx, tx, credentialID)
	})
}

func retireProviderAuthTx(ctx context.Context, tx *gorm.DB, credentialID string) error {
	record := AuthRecord{}
	if errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("uuid = ?", credentialID).First(&record).Error; errFirst != nil {
		return errFirst
	}
	record.Version++
	if errUpdate := tx.Model(&record).Update("version", record.Version).Error; errUpdate != nil {
		return errUpdate
	}
	if errDelete := tx.Delete(&record).Error; errDelete != nil {
		return errDelete
	}
	return appendEvent(tx, "auth", "delete", record.UUID, record.Version)
}

func mapCredentialConcurrencyOrphan(err error) error {
	if errors.Is(err, ErrCredentialUUIDMappingRequired) {
		return fmt.Errorf("%w: %w", ErrCredentialConcurrencyOrphan, err)
	}
	return err
}

func isProviderAuthForConfigKey(auth *coreauth.Auth, key string) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	source := strings.TrimSpace(auth.Attributes["source"])
	switch strings.TrimSpace(key) {
	case "gemini-api-key":
		return auth.Provider == "gemini" && strings.HasPrefix(source, "config:gemini[")
	case "vertex-api-key":
		return auth.Provider == "vertex" && strings.HasPrefix(source, "config:vertex-apikey[")
	case "codex-api-key":
		return auth.Provider == "codex" && strings.HasPrefix(source, "config:codex[")
	case "xai-api-key":
		return auth.Provider == "xai" && strings.HasPrefix(source, "config:xai[")
	case "claude-api-key":
		return auth.Provider == "claude" && strings.HasPrefix(source, "config:claude[")
	case "openai-compatibility":
		return strings.TrimSpace(auth.Attributes["compat_name"]) != ""
	default:
		return false
	}
}
