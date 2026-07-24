package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	inFlightSnapshotCursorRandomBytes         = 24
	inFlightSnapshotCursorSchemaVersion       = 1
	inFlightSnapshotCursorWriteBatchSize      = 250
	inFlightSnapshotCursorCleanupBatchSize    = 8
	inFlightSnapshotCursorCleanupRowBatchSize = 1000
)

var ErrInFlightSnapshotCursorExpired = errors.New("in-flight snapshot cursor expired")

// InFlightSnapshotCursorInput is the immutable observation captured for stable
// Management API pagination.
type InFlightSnapshotCursorInput struct {
	CredentialID string
	Model        string
	Observation  InFlightObservationReadModel
	States       []CredentialConcurrencyState
}

// InFlightSnapshotCursor is one page from a short-lived immutable Management
// API view.
type InFlightSnapshotCursor struct {
	Cursor       string
	CredentialID string
	Model        string
	Observation  InFlightObservationReadModel
	States       []CredentialConcurrencyState
	Total        int
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ReadAt       time.Time
}

// CreateInFlightSnapshotCursor persists one immutable relational view for
// stable pagination.
func (r *Repository) CreateInFlightSnapshotCursor(ctx context.Context, input InFlightSnapshotCursorInput, ttl time.Duration) (InFlightSnapshotCursor, error) {
	if ttl <= 0 {
		return InFlightSnapshotCursor{}, fmt.Errorf("in-flight snapshot cursor ttl must be positive")
	}
	db, errDB := r.database()
	if errDB != nil {
		return InFlightSnapshotCursor{}, errDB
	}
	cursor, errCursor := newInFlightSnapshotCursorToken()
	if errCursor != nil {
		return InFlightSnapshotCursor{}, errCursor
	}
	ctx = contextOrBackground(ctx)
	result := InFlightSnapshotCursor{}
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		if errCleanup := cleanupExpiredInFlightSnapshotCursors(ctx, tx, now); errCleanup != nil {
			return errCleanup
		}
		expiresAt := now.Add(ttl)
		record := inFlightSnapshotCursorHeaderRecord(cursor, input, now, expiresAt)
		if errCreate := tx.WithContext(ctx).Create(&record).Error; errCreate != nil {
			return errCreate
		}
		if errItems := createInFlightSnapshotCursorItems(ctx, tx, cursor, input); errItems != nil {
			return errItems
		}
		if errObserved := createInFlightSnapshotCursorObserved(ctx, tx, cursor, input); errObserved != nil {
			return errObserved
		}
		if errStates := createInFlightSnapshotCursorStates(ctx, tx, cursor, input); errStates != nil {
			return errStates
		}
		readAt, errReadAt := DatabaseNow(ctx, tx)
		if errReadAt != nil {
			return errReadAt
		}
		if !expiresAt.After(readAt) {
			return fmt.Errorf("in-flight snapshot cursor expired while being persisted")
		}
		result = InFlightSnapshotCursor{
			Cursor:       cursor,
			CredentialID: input.CredentialID,
			Model:        input.Model,
			Total:        len(input.Observation.Details),
			CreatedAt:    now.UTC(),
			ExpiresAt:    expiresAt.UTC(),
			ReadAt:       readAt.UTC(),
		}
		return nil
	})
	if errTransaction != nil {
		return InFlightSnapshotCursor{}, errTransaction
	}
	return result, nil
}

func inFlightSnapshotCursorHeaderRecord(cursor string, input InFlightSnapshotCursorInput, createdAt time.Time, expiresAt time.Time) ManagementInFlightSnapshotCursorRecord {
	return ManagementInFlightSnapshotCursorRecord{
		Cursor:                          cursor,
		SchemaVersion:                   inFlightSnapshotCursorSchemaVersion,
		CredentialID:                    input.CredentialID,
		Model:                           input.Model,
		ObservedAt:                      cloneInFlightCursorTime(input.Observation.ObservedAt),
		FreshUntil:                      cloneInFlightCursorTime(input.Observation.FreshUntil),
		Stale:                           input.Observation.Stale,
		CoverageComplete:                input.Observation.CoverageComplete,
		AggregatesComplete:              input.Observation.AggregatesComplete,
		ProtocolCoverageComplete:        input.Observation.ProtocolCoverageComplete,
		MinimumProcessedBarrierRevision: cloneInt64(input.Observation.MinimumProcessedBarrierRevision),
		DetailsTruncated:                input.Observation.DetailsTruncated,
		Total:                           int64(len(input.Observation.Details)),
		// Keep a small value for compatibility with databases created from the
		// earlier blob-backed PR revision where payload was NOT NULL.
		Payload:   JSONB(`{"schema_version":1}`),
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}
}

func createInFlightSnapshotCursorItems(ctx context.Context, tx *gorm.DB, cursor string, input InFlightSnapshotCursorInput) error {
	observedIDs := make(map[string]struct{}, len(input.Observation.Credentials))
	for index := range input.Observation.Credentials {
		observedIDs[input.Observation.Credentials[index].CredentialID] = struct{}{}
	}
	stateIDs := make(map[string]struct{}, len(input.States))
	for index := range input.States {
		stateIDs[input.States[index].CredentialID] = struct{}{}
	}
	for start := 0; start < len(input.Observation.Details); start += inFlightSnapshotCursorWriteBatchSize {
		end := min(start+inFlightSnapshotCursorWriteBatchSize, len(input.Observation.Details))
		records := make([]ManagementInFlightSnapshotCursorItemRecord, 0, end-start)
		for index := start; index < end; index++ {
			detail := input.Observation.Details[index]
			_, observedPresent := observedIDs[detail.CredentialID]
			_, statePresent := stateIDs[detail.CredentialID]
			records = append(records, ManagementInFlightSnapshotCursorItemRecord{
				Cursor:          cursor,
				Ordinal:         int64(index),
				RequestID:       detail.RequestID,
				CredentialID:    detail.CredentialID,
				Model:           detail.Model,
				RequestKind:     detail.RequestKind,
				StartedAt:       detail.StartedAt,
				ObservedPresent: observedPresent,
				StatePresent:    statePresent,
			})
		}
		if errCreate := tx.WithContext(ctx).CreateInBatches(records, len(records)).Error; errCreate != nil {
			return errCreate
		}
	}
	return nil
}

func createInFlightSnapshotCursorObserved(ctx context.Context, tx *gorm.DB, cursor string, input InFlightSnapshotCursorInput) error {
	if len(input.Observation.Credentials) == 0 {
		return nil
	}
	records := make([]ManagementInFlightSnapshotCursorObservedRecord, 0, min(len(input.Observation.Credentials), inFlightSnapshotCursorWriteBatchSize))
	seen := make(map[string]struct{}, len(input.Observation.Credentials))
	for index := range input.Observation.Credentials {
		item := input.Observation.Credentials[index]
		if _, exists := seen[item.CredentialID]; exists {
			return fmt.Errorf("duplicate in-flight snapshot cursor observed credential: %s", item.CredentialID)
		}
		seen[item.CredentialID] = struct{}{}
		records = append(records, ManagementInFlightSnapshotCursorObservedRecord{
			Cursor:              cursor,
			CredentialID:        item.CredentialID,
			ObservedInFlight:    item.ObservedInFlight,
			ObservedAccounted:   item.ObservedAccounted,
			ObservedUnaccounted: item.ObservedUnaccounted,
		})
		if len(records) == inFlightSnapshotCursorWriteBatchSize || index == len(input.Observation.Credentials)-1 {
			if errCreate := tx.WithContext(ctx).CreateInBatches(records, len(records)).Error; errCreate != nil {
				return errCreate
			}
			records = records[:0]
		}
	}
	return nil
}

func createInFlightSnapshotCursorStates(ctx context.Context, tx *gorm.DB, cursor string, input InFlightSnapshotCursorInput) error {
	seenStates := make(map[string]struct{}, len(input.States))
	stateRecords := make([]ManagementInFlightSnapshotCursorStateRecord, 0, min(len(input.States), inFlightSnapshotCursorWriteBatchSize))
	modelRecords := make([]ManagementInFlightSnapshotCursorStateModelRecord, 0, inFlightSnapshotCursorWriteBatchSize)
	flushStates := func() error {
		if len(stateRecords) == 0 {
			return nil
		}
		if errCreate := tx.WithContext(ctx).CreateInBatches(stateRecords, len(stateRecords)).Error; errCreate != nil {
			return errCreate
		}
		stateRecords = stateRecords[:0]
		return nil
	}
	flushModels := func() error {
		if len(modelRecords) == 0 {
			return nil
		}
		if errCreate := tx.WithContext(ctx).CreateInBatches(modelRecords, len(modelRecords)).Error; errCreate != nil {
			return errCreate
		}
		modelRecords = modelRecords[:0]
		return nil
	}
	for stateIndex := range input.States {
		state := input.States[stateIndex]
		if _, exists := seenStates[state.CredentialID]; exists {
			return fmt.Errorf("duplicate in-flight snapshot cursor state credential: %s", state.CredentialID)
		}
		seenStates[state.CredentialID] = struct{}{}
		stateRecords = append(stateRecords, ManagementInFlightSnapshotCursorStateRecord{
			Cursor:             cursor,
			CredentialID:       state.CredentialID,
			MaxInFlight:        cloneInt64(state.MaxInFlight),
			AdmittedInFlight:   state.AdmittedInFlight,
			PolicyVersion:      state.PolicyVersion,
			EffectiveAt:        state.EffectiveAt,
			ObservationBarrier: state.ObservationBarrier,
			ModelCount:         int64(len(state.Models)),
		})
		if len(stateRecords) == inFlightSnapshotCursorWriteBatchSize {
			if errFlush := flushStates(); errFlush != nil {
				return errFlush
			}
		}
		seenModels := make(map[string]struct{}, len(state.Models))
		for modelIndex := range state.Models {
			model := state.Models[modelIndex]
			if _, exists := seenModels[model.Model]; exists {
				return fmt.Errorf("duplicate in-flight snapshot cursor state model: %s", model.Model)
			}
			seenModels[model.Model] = struct{}{}
			modelRecords = append(modelRecords, ManagementInFlightSnapshotCursorStateModelRecord{
				Cursor:           cursor,
				CredentialID:     state.CredentialID,
				Model:            model.Model,
				MaxInFlight:      model.MaxInFlight,
				AdmittedInFlight: model.AdmittedInFlight,
			})
			if len(modelRecords) == inFlightSnapshotCursorWriteBatchSize {
				if errFlush := flushModels(); errFlush != nil {
					return errFlush
				}
			}
		}
	}
	if errFlush := flushStates(); errFlush != nil {
		return errFlush
	}
	return flushModels()
}

// ReadInFlightSnapshotCursorPage returns one page from an unexpired immutable
// cursor without loading the cursor's complete item set.
func (r *Repository) ReadInFlightSnapshotCursorPage(ctx context.Context, cursor string, credentialID string, model string, offset int, limit int) (InFlightSnapshotCursor, error) {
	normalized, okCursor := normalizeInFlightSnapshotCursorToken(cursor)
	if !okCursor {
		return InFlightSnapshotCursor{}, ErrInFlightSnapshotCursorExpired
	}
	if offset < 0 || limit <= 0 {
		return InFlightSnapshotCursor{}, fmt.Errorf("invalid in-flight snapshot cursor pagination")
	}
	db, errDB := r.database()
	if errDB != nil {
		return InFlightSnapshotCursor{}, errDB
	}
	ctx = contextOrBackground(ctx)
	result := InFlightSnapshotCursor{}
	expired := false
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := ManagementInFlightSnapshotCursorRecord{}
		errFirst := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "SHARE"}).
			Where("cursor = ?", normalized).
			First(&record).Error
		if errors.Is(errFirst, gorm.ErrRecordNotFound) {
			expired = true
			return nil
		}
		if errFirst != nil {
			return errFirst
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		if !record.ExpiresAt.After(now) || record.SchemaVersion != inFlightSnapshotCursorSchemaVersion {
			// Expiry cleanup owns an update lock. Readers must not upgrade this
			// shared lock to a delete lock because concurrent upgrades can deadlock.
			expired = true
			return nil
		}
		if record.CredentialID != credentialID || record.Model != model {
			expired = true
			return nil
		}
		total, okTotal := inFlightSnapshotCursorTotal(record.Total)
		if !okTotal {
			return fmt.Errorf("in-flight snapshot cursor total is invalid")
		}
		details, itemRecords, errDetails := readInFlightSnapshotCursorDetails(ctx, tx, normalized, total, offset, limit)
		if errDetails != nil {
			return errDetails
		}
		credentials, states, errProjection := readInFlightSnapshotCursorPageProjection(ctx, tx, normalized, itemRecords)
		if errProjection != nil {
			return errProjection
		}
		result = InFlightSnapshotCursor{
			Cursor:       record.Cursor,
			CredentialID: record.CredentialID,
			Model:        record.Model,
			Observation: InFlightObservationReadModel{
				ObservedAt:                      cloneInFlightCursorTime(record.ObservedAt),
				FreshUntil:                      cloneInFlightCursorTime(record.FreshUntil),
				Stale:                           record.Stale,
				CoverageComplete:                record.CoverageComplete,
				AggregatesComplete:              record.AggregatesComplete,
				ProtocolCoverageComplete:        record.ProtocolCoverageComplete,
				MinimumProcessedBarrierRevision: cloneInt64(record.MinimumProcessedBarrierRevision),
				DetailsTruncated:                record.DetailsTruncated,
				Credentials:                     credentials,
				Details:                         details,
			},
			States:    states,
			Total:     total,
			CreatedAt: record.CreatedAt.UTC(),
			ExpiresAt: record.ExpiresAt.UTC(),
			ReadAt:    now.UTC(),
		}
		return nil
	})
	if errTransaction != nil {
		return InFlightSnapshotCursor{}, errTransaction
	}
	if expired {
		return InFlightSnapshotCursor{}, ErrInFlightSnapshotCursorExpired
	}
	return result, nil
}

func readInFlightSnapshotCursorDetails(ctx context.Context, tx *gorm.DB, cursor string, total int, offset int, limit int) ([]InFlightRequestDetail, []ManagementInFlightSnapshotCursorItemRecord, error) {
	expected := inFlightSnapshotCursorPageCount(total, offset, limit)
	if expected == 0 {
		return []InFlightRequestDetail{}, []ManagementInFlightSnapshotCursorItemRecord{}, nil
	}
	var records []ManagementInFlightSnapshotCursorItemRecord
	if errFind := tx.WithContext(ctx).
		Where("cursor = ? AND ordinal >= ?", cursor, int64(offset)).
		Order("ordinal ASC").
		Limit(expected).
		Find(&records).Error; errFind != nil {
		return nil, nil, errFind
	}
	if len(records) != expected {
		return nil, nil, fmt.Errorf("in-flight snapshot cursor page is incomplete")
	}
	details := make([]InFlightRequestDetail, 0, len(records))
	for index := range records {
		record := records[index]
		if record.Ordinal != int64(offset+index) {
			return nil, nil, fmt.Errorf("in-flight snapshot cursor page ordinal is invalid")
		}
		details = append(details, InFlightRequestDetail{
			RequestID:    record.RequestID,
			CredentialID: record.CredentialID,
			Model:        record.Model,
			RequestKind:  record.RequestKind,
			StartedAt:    record.StartedAt.UTC(),
		})
	}
	return details, records, nil
}

func readInFlightSnapshotCursorPageProjection(ctx context.Context, tx *gorm.DB, cursor string, items []ManagementInFlightSnapshotCursorItemRecord) ([]InFlightObservedCredentialItem, []CredentialConcurrencyState, error) {
	if len(items) == 0 {
		return []InFlightObservedCredentialItem{}, []CredentialConcurrencyState{}, nil
	}
	credentialIDs := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	expectedObserved := make(map[string]bool, len(items))
	expectedStates := make(map[string]bool, len(items))
	for index := range items {
		item := items[index]
		expectedObserved[item.CredentialID] = expectedObserved[item.CredentialID] || item.ObservedPresent
		expectedStates[item.CredentialID] = expectedStates[item.CredentialID] || item.StatePresent
		if _, exists := seen[item.CredentialID]; exists {
			continue
		}
		seen[item.CredentialID] = struct{}{}
		credentialIDs = append(credentialIDs, item.CredentialID)
	}
	var observedRecords []ManagementInFlightSnapshotCursorObservedRecord
	for start := 0; start < len(credentialIDs); start += inFlightSnapshotCursorWriteBatchSize {
		end := min(start+inFlightSnapshotCursorWriteBatchSize, len(credentialIDs))
		var records []ManagementInFlightSnapshotCursorObservedRecord
		if errObserved := tx.WithContext(ctx).
			Where("cursor = ? AND credential_id IN ?", cursor, credentialIDs[start:end]).
			Order("credential_id ASC").
			Find(&records).Error; errObserved != nil {
			return nil, nil, errObserved
		}
		observedRecords = append(observedRecords, records...)
	}
	observed := make([]InFlightObservedCredentialItem, 0, len(observedRecords))
	observedSeen := make(map[string]struct{}, len(observedRecords))
	for index := range observedRecords {
		record := observedRecords[index]
		observedSeen[record.CredentialID] = struct{}{}
		observed = append(observed, InFlightObservedCredentialItem{
			CredentialID:        record.CredentialID,
			ObservedInFlight:    record.ObservedInFlight,
			ObservedAccounted:   record.ObservedAccounted,
			ObservedUnaccounted: record.ObservedUnaccounted,
			Models:              []InFlightObservedModelItem{},
		})
	}
	for credentialID, required := range expectedObserved {
		if required {
			if _, exists := observedSeen[credentialID]; !exists {
				return nil, nil, fmt.Errorf("in-flight snapshot cursor observed state is incomplete")
			}
		}
	}
	var stateRecords []ManagementInFlightSnapshotCursorStateRecord
	for start := 0; start < len(credentialIDs); start += inFlightSnapshotCursorWriteBatchSize {
		end := min(start+inFlightSnapshotCursorWriteBatchSize, len(credentialIDs))
		var records []ManagementInFlightSnapshotCursorStateRecord
		if errStates := tx.WithContext(ctx).
			Where("cursor = ? AND credential_id IN ?", cursor, credentialIDs[start:end]).
			Order("credential_id ASC").
			Find(&records).Error; errStates != nil {
			return nil, nil, errStates
		}
		stateRecords = append(stateRecords, records...)
	}
	states := make([]CredentialConcurrencyState, 0, len(stateRecords))
	stateIndex := make(map[string]int, len(stateRecords))
	for index := range stateRecords {
		record := stateRecords[index]
		stateIndex[record.CredentialID] = len(states)
		states = append(states, CredentialConcurrencyState{
			CredentialID:       record.CredentialID,
			MaxInFlight:        cloneInt64(record.MaxInFlight),
			AdmittedInFlight:   record.AdmittedInFlight,
			PolicyVersion:      record.PolicyVersion,
			EffectiveAt:        record.EffectiveAt.UTC(),
			ObservationBarrier: record.ObservationBarrier,
			Models:             []CredentialConcurrencyModelState{},
		})
	}
	for credentialID, required := range expectedStates {
		if required {
			if _, exists := stateIndex[credentialID]; !exists {
				return nil, nil, fmt.Errorf("in-flight snapshot cursor limiter state is incomplete")
			}
		}
	}
	if len(stateRecords) == 0 {
		return observed, states, nil
	}
	var modelRecords []ManagementInFlightSnapshotCursorStateModelRecord
	for start := 0; start < len(credentialIDs); start += inFlightSnapshotCursorWriteBatchSize {
		end := min(start+inFlightSnapshotCursorWriteBatchSize, len(credentialIDs))
		var records []ManagementInFlightSnapshotCursorStateModelRecord
		if errModels := tx.WithContext(ctx).
			Where("cursor = ? AND credential_id IN ?", cursor, credentialIDs[start:end]).
			Order("credential_id ASC, model ASC").
			Find(&records).Error; errModels != nil {
			return nil, nil, errModels
		}
		modelRecords = append(modelRecords, records...)
	}
	for index := range modelRecords {
		record := modelRecords[index]
		statePosition, exists := stateIndex[record.CredentialID]
		if !exists {
			return nil, nil, fmt.Errorf("in-flight snapshot cursor limiter model has no state")
		}
		states[statePosition].Models = append(states[statePosition].Models, CredentialConcurrencyModelState{
			Model:            record.Model,
			MaxInFlight:      record.MaxInFlight,
			AdmittedInFlight: record.AdmittedInFlight,
		})
	}
	for index := range stateRecords {
		statePosition := stateIndex[stateRecords[index].CredentialID]
		if int64(len(states[statePosition].Models)) != stateRecords[index].ModelCount {
			return nil, nil, fmt.Errorf("in-flight snapshot cursor limiter models are incomplete")
		}
	}
	return observed, states, nil
}

func inFlightSnapshotCursorPageCount(total int, offset int, limit int) int {
	if offset >= total {
		return 0
	}
	remaining := total - offset
	if remaining < limit {
		return remaining
	}
	return limit
}

func inFlightSnapshotCursorTotal(value int64) (int, bool) {
	if value < 0 || uint64(value) > uint64(^uint(0)>>1) {
		return 0, false
	}
	return int(value), true
}

func cleanupExpiredInFlightSnapshotCursors(ctx context.Context, tx *gorm.DB, now time.Time) error {
	var records []ManagementInFlightSnapshotCursorRecord
	// Page reads lock the same header before loading child rows, so cleanup
	// cannot remove a projection midway through a successful read.
	if errFind := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("cursor").
		Where("expires_at <= ?", now).
		Order("expires_at ASC, cursor ASC").
		Limit(inFlightSnapshotCursorCleanupBatchSize).
		Find(&records).Error; errFind != nil {
		return errFind
	}
	remaining := inFlightSnapshotCursorCleanupRowBatchSize
	for index := range records {
		deleted, errDelete := deleteInFlightSnapshotCursorRecordsBatch(ctx, tx, records[index].Cursor, remaining)
		if errDelete != nil {
			return errDelete
		}
		remaining -= deleted
		if remaining <= 0 {
			break
		}
	}
	return nil
}

func deleteInFlightSnapshotCursorRecordsBatch(ctx context.Context, tx *gorm.DB, cursor string, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	remaining := limit
	deleted := 0
	complete, count, errModels := deleteInFlightSnapshotCursorStateModelsBatch(ctx, tx, cursor, remaining)
	if errModels != nil {
		return 0, errModels
	}
	deleted += count
	remaining -= count
	if !complete || remaining <= 0 {
		return deleted, nil
	}
	complete, count, errStates := deleteInFlightSnapshotCursorStatesBatch(ctx, tx, cursor, remaining)
	if errStates != nil {
		return 0, errStates
	}
	deleted += count
	remaining -= count
	if !complete || remaining <= 0 {
		return deleted, nil
	}
	complete, count, errObserved := deleteInFlightSnapshotCursorObservedBatch(ctx, tx, cursor, remaining)
	if errObserved != nil {
		return 0, errObserved
	}
	deleted += count
	remaining -= count
	if !complete || remaining <= 0 {
		return deleted, nil
	}
	complete, count, errItems := deleteInFlightSnapshotCursorItemsBatch(ctx, tx, cursor, remaining)
	if errItems != nil {
		return 0, errItems
	}
	deleted += count
	remaining -= count
	if !complete || remaining <= 0 {
		return deleted, nil
	}
	result := tx.WithContext(ctx).Where("cursor = ?", cursor).Delete(&ManagementInFlightSnapshotCursorRecord{})
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected > 1 {
		return 0, fmt.Errorf("deleted multiple in-flight snapshot cursor headers")
	}
	return deleted + int(result.RowsAffected), nil
}

func deleteInFlightSnapshotCursorStateModelsBatch(ctx context.Context, tx *gorm.DB, cursor string, limit int) (bool, int, error) {
	var records []ManagementInFlightSnapshotCursorStateModelRecord
	if errFind := tx.WithContext(ctx).
		Where("cursor = ?", cursor).
		Order("credential_id ASC, model ASC").
		Limit(limit).
		Find(&records).Error; errFind != nil {
		return false, 0, errFind
	}
	if len(records) == 0 {
		return true, 0, nil
	}
	keys := make([][]any, 0, len(records))
	for index := range records {
		keys = append(keys, []any{records[index].CredentialID, records[index].Model})
	}
	result := tx.WithContext(ctx).
		Where("cursor = ?", cursor).
		Where("(credential_id, model) IN ?", keys).
		Delete(&ManagementInFlightSnapshotCursorStateModelRecord{})
	if result.Error != nil {
		return false, 0, result.Error
	}
	if result.RowsAffected != int64(len(records)) {
		return false, 0, fmt.Errorf("in-flight snapshot cursor state model cleanup is inconsistent")
	}
	return len(records) < limit, len(records), nil
}

func deleteInFlightSnapshotCursorStatesBatch(ctx context.Context, tx *gorm.DB, cursor string, limit int) (bool, int, error) {
	var records []ManagementInFlightSnapshotCursorStateRecord
	if errFind := tx.WithContext(ctx).
		Where("cursor = ?", cursor).
		Order("credential_id ASC").
		Limit(limit).
		Find(&records).Error; errFind != nil {
		return false, 0, errFind
	}
	if len(records) == 0 {
		return true, 0, nil
	}
	credentialIDs := make([]string, 0, len(records))
	for index := range records {
		credentialIDs = append(credentialIDs, records[index].CredentialID)
	}
	result := tx.WithContext(ctx).
		Where("cursor = ? AND credential_id IN ?", cursor, credentialIDs).
		Delete(&ManagementInFlightSnapshotCursorStateRecord{})
	if result.Error != nil {
		return false, 0, result.Error
	}
	if result.RowsAffected != int64(len(records)) {
		return false, 0, fmt.Errorf("in-flight snapshot cursor state cleanup is inconsistent")
	}
	return len(records) < limit, len(records), nil
}

func deleteInFlightSnapshotCursorObservedBatch(ctx context.Context, tx *gorm.DB, cursor string, limit int) (bool, int, error) {
	var records []ManagementInFlightSnapshotCursorObservedRecord
	if errFind := tx.WithContext(ctx).
		Where("cursor = ?", cursor).
		Order("credential_id ASC").
		Limit(limit).
		Find(&records).Error; errFind != nil {
		return false, 0, errFind
	}
	if len(records) == 0 {
		return true, 0, nil
	}
	credentialIDs := make([]string, 0, len(records))
	for index := range records {
		credentialIDs = append(credentialIDs, records[index].CredentialID)
	}
	result := tx.WithContext(ctx).
		Where("cursor = ? AND credential_id IN ?", cursor, credentialIDs).
		Delete(&ManagementInFlightSnapshotCursorObservedRecord{})
	if result.Error != nil {
		return false, 0, result.Error
	}
	if result.RowsAffected != int64(len(records)) {
		return false, 0, fmt.Errorf("in-flight snapshot cursor observed cleanup is inconsistent")
	}
	return len(records) < limit, len(records), nil
}

func deleteInFlightSnapshotCursorItemsBatch(ctx context.Context, tx *gorm.DB, cursor string, limit int) (bool, int, error) {
	var records []ManagementInFlightSnapshotCursorItemRecord
	if errFind := tx.WithContext(ctx).
		Where("cursor = ?", cursor).
		Order("ordinal ASC").
		Limit(limit).
		Find(&records).Error; errFind != nil {
		return false, 0, errFind
	}
	if len(records) == 0 {
		return true, 0, nil
	}
	ordinals := make([]int64, 0, len(records))
	for index := range records {
		ordinals = append(ordinals, records[index].Ordinal)
	}
	result := tx.WithContext(ctx).
		Where("cursor = ? AND ordinal IN ?", cursor, ordinals).
		Delete(&ManagementInFlightSnapshotCursorItemRecord{})
	if result.Error != nil {
		return false, 0, result.Error
	}
	if result.RowsAffected != int64(len(records)) {
		return false, 0, fmt.Errorf("in-flight snapshot cursor item cleanup is inconsistent")
	}
	return len(records) < limit, len(records), nil
}

func cloneInFlightCursorTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func newInFlightSnapshotCursorToken() (string, error) {
	raw := make([]byte, inFlightSnapshotCursorRandomBytes)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", fmt.Errorf("generate in-flight snapshot cursor: %w", errRead)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func normalizeInFlightSnapshotCursorToken(cursor string) (string, bool) {
	normalized := strings.TrimSpace(cursor)
	raw, errDecode := base64.RawURLEncoding.DecodeString(normalized)
	return normalized, errDecode == nil && len(raw) == inFlightSnapshotCursorRandomBytes
}
