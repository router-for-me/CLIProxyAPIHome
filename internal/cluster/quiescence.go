package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	QuiescenceStatusPending      = "pending"
	QuiescenceStatusAcknowledged = "acknowledged"
	QuiescenceStatusFenced       = "fenced"
)

var (
	ErrQuiescenceRevisionMismatch         = errors.New("fingerprint quiescence revision does not match")
	ErrFingerprintQuiescenceSetIncomplete = errors.New("fingerprint quiescence set is incomplete")
	ErrFingerprintNotQuiescent            = errors.New("fingerprint is not quiescent")
)

type quiescenceLockOrderContextKey struct{}

func recordQuiescenceLock(ctx context.Context, step string) {
	record, okRecord := contextOrBackground(ctx).Value(quiescenceLockOrderContextKey{}).(func(string))
	if okRecord && record != nil {
		record(step)
	}
}

// BeginFingerprintCancellation creates the exact Home acknowledgement set for an active membership.
func (r *Repository) BeginFingerprintCancellation(ctx context.Context, fingerprint string) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return 0, fmt.Errorf("CPA certificate fingerprint is required")
	}

	var revision int64
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		var errBegin error
		revision, errBegin = beginFingerprintCancellationTx(ctx, tx, fingerprint, time.Time{})
		return errBegin
	})
	return revision, errTransaction
}

// BeginFingerprintCancellationForLifetime starts cancellation only when the exact connection lifetime is still active.
func (r *Repository) BeginFingerprintCancellationForLifetime(ctx context.Context, lifetime ConnectionLifetime) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	lifetime.Fingerprint = strings.TrimSpace(lifetime.Fingerprint)
	lifetime.Home.IP = strings.TrimSpace(lifetime.Home.IP)
	if !lifetime.Controlled || lifetime.Fingerprint == "" || lifetime.ConnectedAt.IsZero() || lifetime.Home.IP == "" || lifetime.Home.Port <= 0 || lifetime.Home.StartedAt.IsZero() {
		return 0, ErrMembershipNotActive
	}

	var revision int64
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		membership := CPANodeMembershipRecord{}
		errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ?", lifetime.Fingerprint).First(&membership).Error
		if errors.Is(errLock, gorm.ErrRecordNotFound) {
			return ErrMembershipNotActive
		}
		if errLock != nil {
			return errLock
		}
		recordQuiescenceLock(ctx, "membership")
		if membership.State != MembershipStateActive || !membership.ConnectedAt.Equal(lifetime.ConnectedAt) || !membershipOwnedByHome(membership, lifetime.Home) {
			return ErrMembershipNotActive
		}
		var errBegin error
		revision, errBegin = beginFingerprintCancellationForMembershipTx(ctx, tx, &membership, time.Time{})
		return errBegin
	})
	return revision, errTransaction
}

func beginFingerprintCancellationTx(ctx context.Context, tx *gorm.DB, fingerprint string, now time.Time) (int64, error) {
	membership := CPANodeMembershipRecord{}
	if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ?", fingerprint).First(&membership).Error; errLock != nil {
		return 0, errLock
	}
	recordQuiescenceLock(ctx, "membership")
	return beginFingerprintCancellationForMembershipTx(ctx, tx, &membership, now)
}

func beginFingerprintCancellationForMembershipTx(ctx context.Context, tx *gorm.DB, membership *CPANodeMembershipRecord, now time.Time) (int64, error) {
	if membership == nil || membership.State != MembershipStateActive {
		return 0, ErrMembershipNotActive
	}
	if now.IsZero() {
		var errNow error
		now, errNow = DatabaseNow(ctx, tx)
		if errNow != nil {
			return 0, errNow
		}
	}
	candidates, errCandidates := quiescenceHomeCandidates(tx, *membership)
	if errCandidates != nil {
		return 0, errCandidates
	}
	if len(candidates) == 0 {
		return 0, ErrFingerprintQuiescenceSetIncomplete
	}

	revision := membership.CancelRevision + 1
	updates := map[string]any{
		"state":                     MembershipStateCanceling,
		"cancel_revision":           revision,
		"cancel_started_at":         now,
		"expected_quiescence_count": len(candidates),
		"updated_at":                now,
	}
	if errUpdate := tx.Model(membership).Updates(updates).Error; errUpdate != nil {
		return 0, errUpdate
	}
	rows, errRows := createAndLockQuiescenceRowsTx(ctx, tx, *membership, revision, candidates, now)
	if errRows != nil {
		return 0, errRows
	}
	lifecycle, errLifecycle := lifecycleConfigForQuiescence(tx)
	if errLifecycle != nil {
		return 0, errLifecycle
	}
	if errResolve := resolveQuiescenceHomeEligibilityTx(ctx, tx, rows, lifecycle, now); errResolve != nil {
		return 0, errResolve
	}
	if errEvent := appendEvent(tx, "cpa-fence", membership.ConnectedAt.UTC().Format(time.RFC3339Nano), membership.CertificateFingerprint, revision); errEvent != nil {
		return 0, errEvent
	}
	return revision, nil
}

// CleanupStaleMemberships begins cancellation for active memberships whose database liveness has expired.
func (r *Repository) CleanupStaleMemberships(ctx context.Context) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return errNow
	}
	var active []CPANodeMembershipRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Where("state = ?", MembershipStateActive).Order("certificate_fingerprint").Find(&active).Error; errFind != nil {
		return errFind
	}
	for _, membership := range active {
		if membership.CPAHeartbeatTimeout <= 0 || !membership.LastSeenAt.Before(now.Add(-membership.CPAHeartbeatTimeout)) {
			continue
		}
		if errCleanup := r.cleanupStaleMembership(ctx, membership.CertificateFingerprint, membership.CPAHeartbeatTimeout); errCleanup != nil {
			return errCleanup
		}
	}
	return nil
}

func (r *Repository) cleanupStaleMembership(ctx context.Context, fingerprint string, heartbeatTimeout time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		membership := CPANodeMembershipRecord{}
		errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ? AND state = ? AND last_seen_at < ?", fingerprint, MembershipStateActive, now.Add(-heartbeatTimeout)).First(&membership).Error
		if errors.Is(errLock, gorm.ErrRecordNotFound) {
			return nil
		}
		if errLock != nil {
			return errLock
		}
		if !membership.LastSeenAt.Before(now.Add(-membership.CPAHeartbeatTimeout)) {
			return nil
		}
		_, errBegin := beginFingerprintCancellationForMembershipTx(ctx, tx, &membership, now)
		return errBegin
	})
}

// AcknowledgeQuiescence records one active Home's successful local fence for the current lifetime.
func (r *Repository) AcknowledgeQuiescence(ctx context.Context, fingerprint string, connectedAt time.Time, revision int64, home HomeIncarnationID) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" || connectedAt.IsZero() || revision <= 0 || strings.TrimSpace(home.IP) == "" || home.Port <= 0 || home.StartedAt.IsZero() {
		return ErrQuiescenceRevisionMismatch
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		membership := CPANodeMembershipRecord{}
		if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ?", fingerprint).First(&membership).Error; errLock != nil {
			if errors.Is(errLock, gorm.ErrRecordNotFound) {
				return ErrQuiescenceRevisionMismatch
			}
			return errLock
		}
		if membership.State != MembershipStateCanceling || membership.CancelRevision != revision || !membership.ConnectedAt.Equal(connectedAt) {
			return ErrQuiescenceRevisionMismatch
		}
		row := CPANodeQuiescenceRecord{}
		errRow := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", fingerprint, membership.ConnectedAt, revision, strings.TrimSpace(home.IP), home.Port, home.StartedAt).Error
		if errors.Is(errRow, gorm.ErrRecordNotFound) {
			return ErrQuiescenceRevisionMismatch
		}
		if errRow != nil {
			return errRow
		}
		if errHome := verifyActiveHomeIncarnation(tx, home); errHome != nil {
			return errHome
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		return tx.Model(&row).Updates(map[string]any{"status": QuiescenceStatusAcknowledged, "updated_at": now}).Error
	})
}

// CompleteFingerprintCancellation closes a membership only after every expected Home has quiesced.
func (r *Repository) CompleteFingerprintCancellation(ctx context.Context, fingerprint string, connectedAt time.Time, revision int64) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" || connectedAt.IsZero() || revision <= 0 {
		return ErrQuiescenceRevisionMismatch
	}
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		return completeFingerprintCancellationTx(ctx, tx, fingerprint, revision, connectedAt)
	})
	if errTransaction == nil {
		r.cleanupClosedInFlightLifetime(ctx, fingerprint, connectedAt)
	}
	return errTransaction
}

// RecoverStaleQuiescence fences eligible unavailable Homes and completes affected cancellations.
func (r *Repository) RecoverStaleQuiescence(ctx context.Context) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	var canceling []CPANodeMembershipRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Where("state = ?", MembershipStateCanceling).Order("certificate_fingerprint").Find(&canceling).Error; errFind != nil {
		return errFind
	}
	for _, membership := range canceling {
		if errRecover := r.recoverStaleMembershipQuiescence(ctx, membership.CertificateFingerprint, membership.ConnectedAt, membership.CancelRevision); errRecover != nil {
			return errRecover
		}
	}
	return nil
}

func (r *Repository) recoverStaleMembershipQuiescence(ctx context.Context, fingerprint string, connectedAt time.Time, revision int64) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	completed := false
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		lifecycle, errLifecycle := lifecycleConfigForQuiescence(tx)
		if errLifecycle != nil {
			return errLifecycle
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		membership := CPANodeMembershipRecord{}
		errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ? AND connected_at = ? AND cancel_revision = ? AND state = ?", fingerprint, connectedAt, revision, MembershipStateCanceling).First(&membership).Error
		if errors.Is(errLock, gorm.ErrRecordNotFound) {
			return nil
		}
		if errLock != nil {
			return errLock
		}
		recordQuiescenceLock(ctx, "membership")
		var rows []CPANodeQuiescenceRecord
		if errRows := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ? AND status = ?", membership.CertificateFingerprint, membership.ConnectedAt, membership.CancelRevision, QuiescenceStatusPending).Order("home_ip, home_port, home_started_at").Find(&rows).Error; errRows != nil {
			return errRows
		}
		recordQuiescenceLock(ctx, "quiescence")
		if errRecover := recoverPendingQuiescenceRowsTx(ctx, tx, rows, lifecycle, now); errRecover != nil {
			return errRecover
		}
		if errComplete := completeFingerprintCancellationTx(ctx, tx, membership.CertificateFingerprint, membership.CancelRevision, membership.ConnectedAt); errComplete != nil {
			if !errors.Is(errComplete, ErrFingerprintNotQuiescent) && !errors.Is(errComplete, ErrFingerprintQuiescenceSetIncomplete) {
				return errComplete
			}
			return nil
		}
		completed = true
		return nil
	})
	if errTransaction == nil && completed {
		r.cleanupClosedInFlightLifetime(ctx, fingerprint, connectedAt)
	}
	return errTransaction
}

func recoverPendingQuiescenceRowsTx(ctx context.Context, tx *gorm.DB, rows []CPANodeQuiescenceRecord, lifecycle LifecycleConfigRecord, now time.Time) error {
	for _, row := range rows {
		if errRecover := resolveQuiescenceHomeRowTx(ctx, tx, row, lifecycle, now, "CPA fingerprint cancellation recovery"); errRecover != nil {
			return errRecover
		}
	}
	return nil
}

func (r *Repository) cleanupClosedInFlightLifetime(ctx context.Context, fingerprint string, connectedAt time.Time) {
	if errCleanup := r.CleanupInFlightLifetime(ctx, fingerprint, connectedAt); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to clean in-flight observation lifetime")
	}
}

func completeFingerprintCancellationTx(ctx context.Context, tx *gorm.DB, fingerprint string, revision int64, connectedAt time.Time) error {
	membership := CPANodeMembershipRecord{}
	if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ?", fingerprint).First(&membership).Error; errLock != nil {
		if errors.Is(errLock, gorm.ErrRecordNotFound) {
			return ErrQuiescenceRevisionMismatch
		}
		return errLock
	}
	if connectedAt.IsZero() || membership.State != MembershipStateCanceling || membership.CancelRevision != revision || !membership.ConnectedAt.Equal(connectedAt) {
		return ErrQuiescenceRevisionMismatch
	}
	var rows []CPANodeQuiescenceRecord
	if errRows := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ?", fingerprint, membership.ConnectedAt, revision).Order("home_ip, home_port, home_started_at").Find(&rows).Error; errRows != nil {
		return errRows
	}
	if membership.ExpectedQuiescenceCount <= 0 || int64(len(rows)) != membership.ExpectedQuiescenceCount {
		return ErrFingerprintQuiescenceSetIncomplete
	}
	for _, row := range rows {
		if row.Status != QuiescenceStatusAcknowledged && row.Status != QuiescenceStatusFenced {
			return ErrFingerprintNotQuiescent
		}
	}
	if errDelete := tx.Where("certificate_fingerprint = ? AND membership_connected_at = ?", fingerprint, membership.ConnectedAt).Delete(&CPANodeParticipationRecord{}).Error; errDelete != nil {
		return errDelete
	}
	if errDeleteCounters := DeleteFingerprintConcurrencyCountersTx(ctx, tx, fingerprint); errDeleteCounters != nil {
		return errDeleteCounters
	}
	now, errNow := DatabaseNow(ctx, tx)
	if errNow != nil {
		return errNow
	}
	return tx.Model(&membership).Updates(map[string]any{"state": MembershipStateClosed, "updated_at": now}).Error
}

// RefreshCPALiveness records a subscription pong only while its Home and membership lifetime remain active.
func (r *Repository) RefreshCPALiveness(ctx context.Context, lifetime ConnectionLifetime) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	fingerprint := strings.TrimSpace(lifetime.Fingerprint)
	if fingerprint == "" || lifetime.ConnectedAt.IsZero() {
		return ErrMembershipNotActive
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		membership := CPANodeMembershipRecord{}
		errMember := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ?", fingerprint).First(&membership).Error
		if errors.Is(errMember, gorm.ErrRecordNotFound) {
			return ErrMembershipNotActive
		}
		if errMember != nil {
			return errMember
		}
		if membership.State != MembershipStateActive || !membership.ConnectedAt.Equal(lifetime.ConnectedAt) || !membershipOwnedByHome(membership, lifetime.Home) {
			return ErrMembershipNotActive
		}
		if errHome := verifyActiveHomeIncarnation(tx, lifetime.Home); errHome != nil {
			return errHome
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		return tx.Model(&membership).Updates(map[string]any{"last_seen_at": now, "updated_at": now}).Error
	})
}

// ListPendingQuiescence returns the exact pending fence rows owned by a Home incarnation.
func (r *Repository) ListPendingQuiescence(ctx context.Context, home HomeIncarnationID) ([]CPANodeQuiescenceRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if strings.TrimSpace(home.IP) == "" || home.Port <= 0 || home.StartedAt.IsZero() {
		return nil, fmt.Errorf("Home incarnation is required")
	}
	var rows []CPANodeQuiescenceRecord
	errFind := db.WithContext(contextOrBackground(ctx)).Where("home_ip = ? AND home_port = ? AND home_started_at = ? AND status = ?", strings.TrimSpace(home.IP), home.Port, home.StartedAt, QuiescenceStatusPending).Order("updated_at ASC").Find(&rows).Error
	return rows, errFind
}

func quiescenceHomeCandidates(tx *gorm.DB, membership CPANodeMembershipRecord) ([]HomeIncarnationID, error) {
	homes := map[HomeIncarnationID]struct{}{
		{IP: strings.TrimSpace(membership.HomeIP), Port: membership.HomePort, StartedAt: membership.HomeStartedAt}: {},
	}
	var participations []CPANodeParticipationRecord
	if errFind := tx.Where("certificate_fingerprint = ? AND membership_connected_at = ?", membership.CertificateFingerprint, membership.ConnectedAt).Find(&participations).Error; errFind != nil {
		return nil, errFind
	}
	for _, participation := range participations {
		homes[HomeIncarnationID{IP: strings.TrimSpace(participation.HomeIP), Port: participation.HomePort, StartedAt: participation.HomeStartedAt}] = struct{}{}
	}
	ordered := make([]HomeIncarnationID, 0, len(homes))
	for home := range homes {
		ordered = append(ordered, home)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].IP != ordered[j].IP {
			return ordered[i].IP < ordered[j].IP
		}
		if ordered[i].Port != ordered[j].Port {
			return ordered[i].Port < ordered[j].Port
		}
		return ordered[i].StartedAt.Before(ordered[j].StartedAt)
	})
	return ordered, nil
}

func createAndLockQuiescenceRowsTx(ctx context.Context, tx *gorm.DB, membership CPANodeMembershipRecord, revision int64, homes []HomeIncarnationID, now time.Time) ([]CPANodeQuiescenceRecord, error) {
	for _, home := range homes {
		row := CPANodeQuiescenceRecord{
			CertificateFingerprint: membership.CertificateFingerprint,
			MembershipConnectedAt:  membership.ConnectedAt,
			CancelRevision:         revision,
			HomeIP:                 home.IP,
			HomePort:               home.Port,
			HomeStartedAt:          home.StartedAt,
			Status:                 QuiescenceStatusPending,
			UpdatedAt:              now,
		}
		if errCreate := tx.Create(&row).Error; errCreate != nil {
			return nil, errCreate
		}
	}
	var rows []CPANodeQuiescenceRecord
	if errRows := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ?", membership.CertificateFingerprint, membership.ConnectedAt, revision).Order("home_ip, home_port, home_started_at").Find(&rows).Error; errRows != nil {
		return nil, errRows
	}
	if len(rows) != len(homes) {
		return nil, ErrFingerprintQuiescenceSetIncomplete
	}
	recordQuiescenceLock(ctx, "quiescence")
	return rows, nil
}

func resolveQuiescenceHomeEligibilityTx(ctx context.Context, tx *gorm.DB, rows []CPANodeQuiescenceRecord, lifecycle LifecycleConfigRecord, now time.Time) error {
	for _, row := range rows {
		if errResolve := resolveQuiescenceHomeRowTx(ctx, tx, row, lifecycle, now, "CPA fingerprint cancellation"); errResolve != nil {
			return errResolve
		}
	}
	return nil
}

func resolveQuiescenceHomeRowTx(ctx context.Context, tx *gorm.DB, row CPANodeQuiescenceRecord, lifecycle LifecycleConfigRecord, now time.Time, reason string) error {
	recordQuiescenceLock(ctx, "homes")
	home := HomeProcessIncarnationRecord{}
	errHome := tx.First(&home, "home_ip = ? AND home_port = ? AND started_at = ?", row.HomeIP, row.HomePort, row.HomeStartedAt).Error
	if errors.Is(errHome, gorm.ErrRecordNotFound) {
		return ErrHomeIncarnationNotFound
	}
	if errHome != nil {
		return errHome
	}

	if home.State == HomeIncarnationActive {
		cutoff := now.Add(-lifecycle.NodeHeartbeatTimeout)
		if errExpire := tx.Model(&HomeProcessIncarnationRecord{}).
			Where("home_ip = ? AND home_port = ? AND started_at = ? AND state = ? AND last_seen_at < ?", row.HomeIP, row.HomePort, row.HomeStartedAt, HomeIncarnationActive, cutoff).
			Update("state", HomeIncarnationExpired).Error; errExpire != nil {
			return errExpire
		}
	}

	cfg, errConfig := lifecycleConfigFromRecord(lifecycle)
	if errConfig != nil {
		return errConfig
	}
	graceCutoff := now.Add(-(lifecycle.NodeHeartbeatTimeout + cfg.ReclaimGrace))
	if errFence := tx.Model(&HomeProcessIncarnationRecord{}).
		Where("home_ip = ? AND home_port = ? AND started_at = ? AND state IN ? AND last_seen_at < ?", row.HomeIP, row.HomePort, row.HomeStartedAt, []string{HomeIncarnationExpired, HomeIncarnationRetired}, graceCutoff).
		Updates(map[string]any{"state": HomeIncarnationFenced, "fenced_at": now, "fence_reason": reason}).Error; errFence != nil {
		return errFence
	}

	homeEligible := tx.Model(&HomeProcessIncarnationRecord{}).Select("1").Where("home_ip = ? AND home_port = ? AND started_at = ? AND state IN ? AND last_seen_at < ?", row.HomeIP, row.HomePort, row.HomeStartedAt, []string{HomeIncarnationExpired, HomeIncarnationRetired, HomeIncarnationFenced}, graceCutoff)
	return tx.Model(&CPANodeQuiescenceRecord{}).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ? AND home_ip = ? AND home_port = ? AND home_started_at = ? AND status = ?", row.CertificateFingerprint, row.MembershipConnectedAt, row.CancelRevision, row.HomeIP, row.HomePort, row.HomeStartedAt, QuiescenceStatusPending).Where("EXISTS (?)", homeEligible).Updates(map[string]any{"status": QuiescenceStatusFenced, "updated_at": now}).Error
}

func lifecycleConfigForQuiescence(tx *gorm.DB) (LifecycleConfigRecord, error) {
	lifecycle := LifecycleConfigRecord{}
	if errLifecycle := tx.First(&lifecycle, "id = ?", 1).Error; errLifecycle != nil {
		return LifecycleConfigRecord{}, errLifecycle
	}
	return lifecycle, nil
}

func verifyActiveHomeIncarnation(tx *gorm.DB, home HomeIncarnationID) error {
	record := HomeProcessIncarnationRecord{}
	errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "home_ip = ? AND home_port = ? AND started_at = ?", strings.TrimSpace(home.IP), home.Port, home.StartedAt).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return ErrHomeIncarnationNotFound
	}
	if errFirst != nil {
		return errFirst
	}
	if record.State == HomeIncarnationFenced {
		return ErrHomeIncarnationFenced
	}
	if record.State != HomeIncarnationActive {
		return ErrHomeIncarnationInactive
	}
	return nil
}
