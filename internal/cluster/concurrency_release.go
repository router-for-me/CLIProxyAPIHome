package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrConcurrencyInvalidRelease       = errors.New("credential concurrency release is invalid")
	ErrConcurrencyReleaseExceedsActive = errors.New("credential concurrency release exceeds active count")
)

// ConcurrencyReleaseRequest identifies a cumulative release for one credential and model.
type ConcurrencyReleaseRequest struct {
	CredentialID string
	Model        string
	ReleaseSeq   int64
	Lifetime     ConnectionLifetime
}

// ApplyConcurrencyRelease atomically applies a cumulative release sequence.
func (r *Repository) ApplyConcurrencyRelease(ctx context.Context, req ConcurrencyReleaseRequest) error {
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	model, validModel := concurrency.ValidCanonicalConcurrencyModelKey(req.Model)
	if req.CredentialID == "" || !validModel {
		return ErrConcurrencyInvalidRelease
	}
	req.Model = model

	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		if errHook := recordConcurrencyReleaseContenderBackendPID(ctx, tx); errHook != nil {
			return errHook
		}
		return applyConcurrencyReleaseTx(ctx, r, tx, req)
	})
}

func applyConcurrencyReleaseTx(ctx context.Context, repo *Repository, tx *gorm.DB, req ConcurrencyReleaseRequest) error {
	if repo == nil || tx == nil || req.ReleaseSeq <= 0 {
		return ErrConcurrencyInvalidRelease
	}
	credentialID := strings.TrimSpace(req.CredentialID)
	model, validModel := concurrency.ValidCanonicalConcurrencyModelKey(req.Model)
	if credentialID == "" || !validModel {
		return ErrConcurrencyInvalidRelease
	}
	if errLifetime := repo.LockActiveConcurrencyLifetimeTx(ctx, tx, req.Lifetime); errLifetime != nil {
		return ErrConcurrencyNodeUnavailable
	}
	if _, _, errPolicy := lockConcurrencyPolicyTx(ctx, tx, credentialID); errPolicy != nil {
		return errPolicy
	}
	counter, errCounter := lockConcurrencyCounterTx(ctx, tx, credentialID, model, strings.TrimSpace(req.Lifetime.Fingerprint))
	if errCounter != nil {
		return errCounter
	}
	if counter.ActiveCount < 0 || counter.LastReleaseSeq < 0 {
		return ErrConcurrencyCounterInvalid
	}

	delta := req.ReleaseSeq - counter.LastReleaseSeq
	if delta <= 0 {
		return nil
	}
	if delta > counter.ActiveCount {
		return ErrConcurrencyReleaseExceedsActive
	}
	now, errNow := DatabaseNow(ctx, tx)
	if errNow != nil {
		return errNow
	}
	counter.ActiveCount -= delta
	counter.LastReleaseSeq = req.ReleaseSeq
	counter.UpdatedAt = now
	return tx.WithContext(contextOrBackground(ctx)).Save(&counter).Error
}

func lockConcurrencyCounterTx(ctx context.Context, tx *gorm.DB, credentialID string, model string, fingerprint string) (CredentialConcurrencyCounterRecord, error) {
	if tx == nil {
		return CredentialConcurrencyCounterRecord{}, fmt.Errorf("database transaction is nil")
	}
	counter := CredentialConcurrencyCounterRecord{}
	errCounter := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("credential_id = ? AND model = ? AND certificate_fingerprint = ?", credentialID, model, fingerprint).
		First(&counter).Error
	if errors.Is(errCounter, gorm.ErrRecordNotFound) {
		return CredentialConcurrencyCounterRecord{}, ErrConcurrencyReleaseExceedsActive
	}
	if errCounter != nil {
		return CredentialConcurrencyCounterRecord{}, errCounter
	}
	return counter, nil
}

// DeleteFingerprintConcurrencyCountersTx removes all limiter state for a closed membership fingerprint.
func DeleteFingerprintConcurrencyCountersTx(ctx context.Context, tx *gorm.DB, fingerprint string) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ErrConcurrencyInvalidRelease
	}
	return tx.WithContext(contextOrBackground(ctx)).Where("certificate_fingerprint = ?", fingerprint).Delete(&CredentialConcurrencyCounterRecord{}).Error
}
