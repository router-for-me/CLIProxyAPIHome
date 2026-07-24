package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
)

// InFlightObservedModelItem summarizes observation counts for one credential model.
type InFlightObservedModelItem struct {
	Model               string
	ObservedInFlight    int64
	ObservedAccounted   int64
	ObservedUnaccounted int64
}

// InFlightObservedCredentialItem summarizes observation counts for one credential.
type InFlightObservedCredentialItem struct {
	CredentialID        string
	ObservedInFlight    int64
	ObservedAccounted   int64
	ObservedUnaccounted int64
	Models              []InFlightObservedModelItem
}

// InFlightObservationReadModel is the active-membership view of in-flight snapshots.
type InFlightObservationReadModel struct {
	ObservedAt                      *time.Time
	FreshUntil                      *time.Time
	Stale                           bool
	CoverageComplete                bool
	AggregatesComplete              bool
	ProtocolCoverageComplete        bool
	MinimumProcessedBarrierRevision *int64
	DetailsTruncated                bool
	Credentials                     []InFlightObservedCredentialItem
	Details                         []InFlightRequestDetail
}

type inFlightObservationCredentialAccumulator struct {
	observedInFlight    int64
	observedAccounted   int64
	observedUnaccounted int64
	models              map[string]*inFlightObservationModelAccumulator
}

type inFlightObservationModelAccumulator struct {
	observedInFlight    int64
	observedAccounted   int64
	observedUnaccounted int64
}

// ReadInFlightObservation returns only snapshots belonging to current active memberships.
func (r *Repository) ReadInFlightObservation(ctx context.Context, staleAfter time.Duration) (InFlightObservationReadModel, error) {
	if staleAfter <= 0 {
		return InFlightObservationReadModel{}, fmt.Errorf("in-flight stale threshold must be positive")
	}
	db, errDB := r.database()
	if errDB != nil {
		return InFlightObservationReadModel{}, errDB
	}
	ctx = contextOrBackground(ctx)

	tx := inFlightReadTransaction(db.WithContext(ctx))
	if tx.Error != nil {
		return InFlightObservationReadModel{}, tx.Error
	}
	defer func() {
		_ = tx.Rollback().Error
	}()

	read, members, errRead := readInFlightObservationSnapshot(ctx, tx, staleAfter)
	if errRead != nil {
		return InFlightObservationReadModel{}, errRead
	}
	consistent, errVerify := verifyInFlightObservationMemberships(ctx, tx, members)
	if errVerify != nil {
		return InFlightObservationReadModel{}, errVerify
	}
	if !consistent {
		read.CoverageComplete = false
		read.AggregatesComplete = false
		read.Stale = true
	}
	if errCommit := tx.Commit().Error; errCommit != nil {
		return InFlightObservationReadModel{}, errCommit
	}
	return read, nil
}

func inFlightReadTransaction(db *gorm.DB) *gorm.DB {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return db.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	}
	return db.Begin()
}

func readInFlightObservationSnapshot(ctx context.Context, tx *gorm.DB, staleAfter time.Duration) (InFlightObservationReadModel, []CPANodeMembershipRecord, error) {
	read := InFlightObservationReadModel{
		CoverageComplete:         true,
		AggregatesComplete:       true,
		ProtocolCoverageComplete: true,
		Credentials:              []InFlightObservedCredentialItem{},
		Details:                  []InFlightRequestDetail{},
	}
	var members []CPANodeMembershipRecord
	if errMembers := tx.WithContext(ctx).Where("state IN ?", []string{MembershipStateActive, MembershipStateCanceling}).Order("certificate_fingerprint ASC").Find(&members).Error; errMembers != nil {
		return InFlightObservationReadModel{}, nil, errMembers
	}
	now, errNow := DatabaseNow(ctx, tx)
	if errNow != nil {
		return InFlightObservationReadModel{}, nil, errNow
	}

	credentials := make(map[string]*inFlightObservationCredentialAccumulator)
	for index := range members {
		member := members[index]
		if member.State == MembershipStateCanceling {
			read.CoverageComplete = false
			continue
		}
		if member.State != MembershipStateActive {
			continue
		}
		if member.ProtocolVersion != 1 {
			read.ProtocolCoverageComplete = false
		}

		snapshot, attempt, found, errSnapshot := readInFlightActiveSnapshot(ctx, tx, member)
		if errSnapshot != nil {
			return InFlightObservationReadModel{}, nil, errSnapshot
		}
		if !found {
			read.Stale = true
			read.CoverageComplete = false
			read.AggregatesComplete = false
			continue
		}
		freshUntil := snapshot.UpdatedAt.Add(staleAfter).UTC()
		if read.FreshUntil == nil || freshUntil.Before(*read.FreshUntil) {
			read.FreshUntil = &freshUntil
		}
		if snapshotUpdatedAtStale(now, snapshot.UpdatedAt, staleAfter) {
			read.Stale = true
			read.CoverageComplete = false
		}
		if attemptIncompleteForSnapshot(attempt, snapshot) {
			read.Stale = true
			read.CoverageComplete = false
			read.AggregatesComplete = false
		}

		var payload InFlightSnapshotPayload
		if errUnmarshal := json.Unmarshal(snapshot.Payload, &payload); errUnmarshal != nil {
			return InFlightObservationReadModel{}, nil, fmt.Errorf("decode in-flight snapshot for active membership: %w", errUnmarshal)
		}
		if errValidate := validateCompleteInFlightPayload(payload, DefaultInFlightLimits()); errValidate != nil {
			return InFlightObservationReadModel{}, nil, fmt.Errorf("validate in-flight snapshot for active membership: %w", errValidate)
		}
		if errAggregate := aggregateInFlightObservationPayload(credentials, &read, payload); errAggregate != nil {
			return InFlightObservationReadModel{}, nil, errAggregate
		}
		if read.ObservedAt == nil || snapshot.ObservedAt.After(*read.ObservedAt) {
			observedAt := snapshot.ObservedAt.UTC()
			read.ObservedAt = &observedAt
		}
		if read.MinimumProcessedBarrierRevision == nil || snapshot.BarrierRevision < *read.MinimumProcessedBarrierRevision {
			barrierRevision := snapshot.BarrierRevision
			read.MinimumProcessedBarrierRevision = &barrierRevision
		}
		read.DetailsTruncated = read.DetailsTruncated || snapshot.DetailsTruncated || payload.DetailsTruncated
	}
	read.Credentials = inFlightObservationCredentialItems(credentials)
	sortInFlightObservationDetails(read.Details)
	return read, members, nil
}

func readInFlightActiveSnapshot(ctx context.Context, tx *gorm.DB, member CPANodeMembershipRecord) (CPAInFlightSnapshotRecord, CPAInFlightSnapshotAttemptRecord, bool, error) {
	snapshot := CPAInFlightSnapshotRecord{}
	errSnapshot := tx.WithContext(ctx).
		Where("certificate_fingerprint = ? AND membership_connected_at = ?", member.CertificateFingerprint, member.ConnectedAt).
		First(&snapshot).Error
	if errSnapshot != nil {
		if errSnapshot == gorm.ErrRecordNotFound {
			return CPAInFlightSnapshotRecord{}, CPAInFlightSnapshotAttemptRecord{}, false, nil
		}
		return CPAInFlightSnapshotRecord{}, CPAInFlightSnapshotAttemptRecord{}, false, errSnapshot
	}
	attempt := CPAInFlightSnapshotAttemptRecord{}
	errAttempt := tx.WithContext(ctx).
		Where("certificate_fingerprint = ? AND membership_connected_at = ?", member.CertificateFingerprint, member.ConnectedAt).
		First(&attempt).Error
	if errAttempt != nil && errAttempt != gorm.ErrRecordNotFound {
		return CPAInFlightSnapshotRecord{}, CPAInFlightSnapshotAttemptRecord{}, false, errAttempt
	}
	return snapshot, attempt, true, nil
}

func snapshotUpdatedAtStale(now time.Time, updatedAt time.Time, staleAfter time.Duration) bool {
	return updatedAt.IsZero() || now.After(updatedAt.Add(staleAfter))
}

func attemptIncompleteForSnapshot(attempt CPAInFlightSnapshotAttemptRecord, snapshot CPAInFlightSnapshotRecord) bool {
	return attempt.HighestSeenRevision != snapshot.Revision || attempt.State != "complete"
}

func aggregateInFlightObservationPayload(credentials map[string]*inFlightObservationCredentialAccumulator, read *InFlightObservationReadModel, payload InFlightSnapshotPayload) error {
	for _, aggregate := range payload.Aggregates {
		credential := credentials[aggregate.CredentialID]
		if credential == nil {
			credential = &inFlightObservationCredentialAccumulator{models: make(map[string]*inFlightObservationModelAccumulator)}
			credentials[aggregate.CredentialID] = credential
		}
		model := credential.models[aggregate.Model]
		if model == nil {
			model = &inFlightObservationModelAccumulator{}
			credential.models[aggregate.Model] = model
		}
		var errAdd error
		credential.observedInFlight, errAdd = checkedInFlightAdd(credential.observedInFlight, aggregate.Count)
		if errAdd != nil {
			return errAdd
		}
		model.observedInFlight, errAdd = checkedInFlightAdd(model.observedInFlight, aggregate.Count)
		if errAdd != nil {
			return errAdd
		}
		switch aggregate.Status {
		case InFlightAccounted:
			credential.observedAccounted, errAdd = checkedInFlightAdd(credential.observedAccounted, aggregate.Count)
			if errAdd == nil {
				model.observedAccounted, errAdd = checkedInFlightAdd(model.observedAccounted, aggregate.Count)
			}
		case InFlightUnaccounted:
			credential.observedUnaccounted, errAdd = checkedInFlightAdd(credential.observedUnaccounted, aggregate.Count)
			if errAdd == nil {
				model.observedUnaccounted, errAdd = checkedInFlightAdd(model.observedUnaccounted, aggregate.Count)
			}
		default:
			return ErrInFlightFrameInvalid
		}
		if errAdd != nil {
			return errAdd
		}
	}
	read.Details = append(read.Details, payload.Details...)
	return nil
}

func inFlightObservationCredentialItems(credentials map[string]*inFlightObservationCredentialAccumulator) []InFlightObservedCredentialItem {
	items := make([]InFlightObservedCredentialItem, 0, len(credentials))
	for credentialID, credential := range credentials {
		item := InFlightObservedCredentialItem{
			CredentialID:        credentialID,
			ObservedInFlight:    credential.observedInFlight,
			ObservedAccounted:   credential.observedAccounted,
			ObservedUnaccounted: credential.observedUnaccounted,
			Models:              make([]InFlightObservedModelItem, 0, len(credential.models)),
		}
		for modelName, model := range credential.models {
			item.Models = append(item.Models, InFlightObservedModelItem{
				Model:               modelName,
				ObservedInFlight:    model.observedInFlight,
				ObservedAccounted:   model.observedAccounted,
				ObservedUnaccounted: model.observedUnaccounted,
			})
		}
		sort.Slice(item.Models, func(left, right int) bool {
			return item.Models[left].Model < item.Models[right].Model
		})
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool {
		return items[left].CredentialID < items[right].CredentialID
	})
	return items
}

func sortInFlightObservationDetails(details []InFlightRequestDetail) {
	sort.Slice(details, func(left, right int) bool {
		if !details[left].StartedAt.Equal(details[right].StartedAt) {
			return details[left].StartedAt.Before(details[right].StartedAt)
		}
		if details[left].RequestID != details[right].RequestID {
			return details[left].RequestID < details[right].RequestID
		}
		if details[left].CredentialID != details[right].CredentialID {
			return details[left].CredentialID < details[right].CredentialID
		}
		if details[left].Model != details[right].Model {
			return details[left].Model < details[right].Model
		}
		return details[left].RequestKind < details[right].RequestKind
	})
}

func verifyInFlightObservationMemberships(ctx context.Context, tx *gorm.DB, members []CPANodeMembershipRecord) (bool, error) {
	for index := range members {
		member := members[index]
		var count int64
		if errCount := tx.WithContext(ctx).Model(&CPANodeMembershipRecord{}).
			Where("certificate_fingerprint = ? AND connected_at = ? AND state = ?", member.CertificateFingerprint, member.ConnectedAt, member.State).
			Count(&count).Error; errCount != nil {
			return false, errCount
		}
		if count != 1 {
			return false, nil
		}
	}
	return true, nil
}
