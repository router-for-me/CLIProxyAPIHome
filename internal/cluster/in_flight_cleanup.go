package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CleanupInFlightLifetime removes observation rows for one closed CPA membership lifetime.
func (r *Repository) CleanupInFlightLifetime(ctx context.Context, fingerprint string, connectedAt time.Time) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if ctx == nil {
		return fmt.Errorf("in-flight cleanup context is nil")
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" || connectedAt.IsZero() {
		return fmt.Errorf("in-flight cleanup lifetime is invalid")
	}
	connectedAt = connectedAt.UTC()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		predicates := "certificate_fingerprint = ? AND membership_connected_at = ?"
		args := []any{fingerprint, connectedAt}
		if errDelete := tx.Where(predicates, args...).Delete(&CPAInFlightSnapshotPartRecord{}).Error; errDelete != nil {
			return errDelete
		}
		if errDelete := tx.Where(predicates, args...).Delete(&CPAInFlightSnapshotAttemptRecord{}).Error; errDelete != nil {
			return errDelete
		}
		return tx.Where(predicates, args...).Delete(&CPAInFlightSnapshotRecord{}).Error
	})
}

// CleanupExpiredInFlightStaging rejects expired staging only after its membership lifetime is no longer active.
func (r *Repository) CleanupExpiredInFlightStaging(ctx context.Context, retention time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if ctx == nil {
		return fmt.Errorf("in-flight staging cleanup context is nil")
	}
	if retention <= 0 {
		return fmt.Errorf("in-flight staging retention must be positive")
	}

	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return errNow
	}
	cutoff := now.Add(-retention)
	var candidates []CPAInFlightSnapshotAttemptRecord
	if errFind := db.WithContext(ctx).
		Where("state = ? AND updated_at < ?", "staging", cutoff).
		Order("certificate_fingerprint, membership_connected_at").
		Find(&candidates).Error; errFind != nil {
		return errFind
	}
	for _, candidate := range candidates {
		if errCleanup := r.cleanupExpiredInFlightAttempt(ctx, candidate, retention); errCleanup != nil {
			return errCleanup
		}
	}
	return nil
}

func (r *Repository) cleanupExpiredInFlightAttempt(ctx context.Context, candidate CPAInFlightSnapshotAttemptRecord, retention time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		membership := CPANodeMembershipRecord{}
		errMembership := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("certificate_fingerprint = ?", candidate.CertificateFingerprint).
			First(&membership).Error
		if errMembership != nil && !isRecordNotFound(errMembership) {
			return errMembership
		}
		if errMembership == nil && membership.ConnectedAt.Equal(candidate.MembershipConnectedAt) && (membership.State == MembershipStateActive || membership.State == MembershipStateCanceling) {
			return nil
		}

		attempt := CPAInFlightSnapshotAttemptRecord{}
		errAttempt := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("certificate_fingerprint = ? AND membership_connected_at = ?", candidate.CertificateFingerprint, candidate.MembershipConnectedAt).
			First(&attempt).Error
		if isRecordNotFound(errAttempt) {
			return nil
		}
		if errAttempt != nil {
			return errAttempt
		}
		if attempt.State != "staging" || !attempt.UpdatedAt.Before(now.Add(-retention)) {
			return nil
		}
		identity := InFlightIngestIdentity{
			CertificateFingerprint: candidate.CertificateFingerprint,
			MembershipConnectedAt:  candidate.MembershipConnectedAt,
		}
		return rejectInFlightAttempt(ctx, tx, &attempt, identity, now)
	})
}

func isRecordNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}
