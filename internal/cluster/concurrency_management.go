package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AuthConcurrencyPatchRequest combines an optional auth update with a presence-aware limiter policy update.
type AuthConcurrencyPatchRequest struct {
	CredentialID          string
	Auth                  *coreauth.Auth
	AuthChanged           bool
	PolicyPatch           ConcurrencyPolicyPatch
	ExpectedPolicyVersion *int64
}

// PatchAuthAndConcurrency atomically applies auth metadata and limiter policy changes.
func (r *Repository) PatchAuthAndConcurrency(ctx context.Context, request AuthConcurrencyPatchRequest) error {
	credentialID := strings.TrimSpace(request.CredentialID)
	if credentialID == "" {
		return ErrConcurrencyCredentialNotFound
	}
	if errValidate := validateConcurrencyPolicyPatchModelKeys(request.PolicyPatch); errValidate != nil {
		return errValidate
	}
	if request.AuthChanged && request.Auth == nil {
		return fmt.Errorf("auth is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		policy := CredentialConcurrencyPolicy{}
		if errPolicy := r.patchCredentialConcurrencyPolicyTx(ctx, tx, credentialID, request.PolicyPatch, request.ExpectedPolicyVersion, &policy); errPolicy != nil {
			return errPolicy
		}
		if !request.AuthChanged {
			return nil
		}
		return r.patchAuthTx(ctx, tx, credentialID, request.Auth)
	})
}

func (r *Repository) patchAuthTx(ctx context.Context, tx *gorm.DB, credentialID string, auth *coreauth.Auth) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if auth == nil {
		return fmt.Errorf("auth is required")
	}
	record, errRecord := AuthToRecord(auth)
	if errRecord != nil {
		return errRecord
	}
	if record.UUID != credentialID || record.Index != credentialID {
		return fmt.Errorf("auth credential id does not match concurrency credential")
	}
	existing := AuthRecord{}
	errExisting := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, "uuid = ?", credentialID).Error
	if errors.Is(errExisting, gorm.ErrRecordNotFound) {
		return ErrConcurrencyCredentialNotFound
	}
	if errExisting != nil {
		return errExisting
	}
	sameJSON, errEqual := semanticJSONEqual([]byte(existing.AuthJSON), []byte(record.AuthJSON))
	if errEqual != nil {
		return errEqual
	}
	if sameJSON {
		return nil
	}
	record.Version = existing.Version + 1
	record.CreatedAt = existing.CreatedAt
	if errUpdate := tx.WithContext(contextOrBackground(ctx)).Select("*").Where("uuid = ?", credentialID).Updates(record).Error; errUpdate != nil {
		return errUpdate
	}
	return appendEvent(tx, "auth", "update", credentialID, record.Version)
}
