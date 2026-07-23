package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	usageTokenAccountingBackfillAdvisoryLockKey int64 = 749327842680272318
	usageTokenAccountingBackfillBatchSize             = 250
	usageTokenAccountingBackfillStateKey              = "internal:migration:usage-token-accounting:v2"
)

type usageTokenAccountingBackfillState struct {
	HighWaterID   uint `json:"high_water_id"`
	LastScannedID uint `json:"last_scanned_id"`
	Done          bool `json:"done"`
}

type UsageTokenAccountingBackfillResult struct {
	Scanned int
	Updated int64
	Done    bool
	Skipped bool
}

func (r *Repository) RunUsageTokenAccountingBackfillBatch(ctx context.Context) (UsageTokenAccountingBackfillResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageTokenAccountingBackfillResult{}, errDB
	}
	return runUsageTokenAccountingBackfillBatch(contextOrBackground(ctx), db, usageTokenAccountingBackfillBatchSize)
}

func runUsageTokenAccountingBackfillBatch(ctx context.Context, db *gorm.DB, batchSize int) (UsageTokenAccountingBackfillResult, error) {
	if db == nil {
		return UsageTokenAccountingBackfillResult{}, fmt.Errorf("database connection is nil")
	}
	if batchSize <= 0 {
		return UsageTokenAccountingBackfillResult{}, fmt.Errorf("usage token accounting backfill batch size must be positive")
	}
	result := UsageTokenAccountingBackfillResult{}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			var acquired bool
			if errLock := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", usageTokenAccountingBackfillAdvisoryLockKey).Scan(&acquired).Error; errLock != nil {
				return errLock
			}
			if !acquired {
				result.Skipped = true
				return nil
			}
		}
		return runUsageTokenAccountingBackfillBatchTx(ctx, tx, batchSize, &result)
	})
	if errTransaction != nil {
		return UsageTokenAccountingBackfillResult{}, errTransaction
	}
	return result, nil
}

func runUsageTokenAccountingBackfillBatchTx(ctx context.Context, tx *gorm.DB, batchSize int, result *UsageTokenAccountingBackfillResult) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if result == nil {
		return fmt.Errorf("usage token accounting backfill result is nil")
	}
	state, stateRecord, exists, errState := loadUsageTokenAccountingBackfillState(ctx, tx)
	if errState != nil {
		return errState
	}
	if state.Done {
		pending, errPending := firstPendingUsageTokenAccountingRecord(ctx, tx)
		if errPending != nil {
			return errPending
		}
		if pending == nil {
			result.Done = true
			return nil
		}
		latest, errLatest := latestUsageTokenAccountingRecord(ctx, tx)
		if errLatest != nil {
			return errLatest
		}
		state.Done = false
		state.LastScannedID = 0
		if pending.ID > 0 {
			state.LastScannedID = pending.ID - 1
		}
		state.HighWaterID = latest.ID
	}
	if !exists {
		latest, errLatest := latestUsageTokenAccountingRecord(ctx, tx)
		if errLatest != nil {
			return errLatest
		}
		state.HighWaterID = latest.ID
		if state.HighWaterID == 0 {
			state.Done = true
		}
	}
	if state.LastScannedID >= state.HighWaterID {
		state.Done = true
	}
	if state.Done {
		result.Done = true
		return saveUsageTokenAccountingBackfillState(ctx, tx, stateRecord, exists, state)
	}

	var records []UsageRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).
		Select("id", "provider", "executor_type", "input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_read_tokens_present", "cache_creation_tokens", "total_tokens", "token_accounting_version", "payload").
		Where("id > ? AND id <= ?", state.LastScannedID, state.HighWaterID).
		Order("id ASC").
		Limit(batchSize).
		Find(&records).Error; errFind != nil {
		return errFind
	}
	if len(records) == 0 {
		state.Done = true
		result.Done = true
		return saveUsageTokenAccountingBackfillState(ctx, tx, stateRecord, exists, state)
	}

	result.Scanned = len(records)
	for index := range records {
		record := &records[index]
		if record.TokenAccountingVersion == UsageTokenAccountingSchemaVersion {
			continue
		}
		cacheReadTokens := normalizedUsageCacheReadTokens(record.Provider, record.ExecutorType, record.CachedTokens, record.CacheReadTokens, record.CacheReadTokensPresent)
		legacy := usageLegacyTokenCounters{
			Provider:            record.Provider,
			ExecutorType:        record.ExecutorType,
			InputTokens:         record.InputTokens,
			OutputTokens:        record.OutputTokens,
			ReasoningTokens:     record.ReasoningTokens,
			CacheReadTokens:     cacheReadTokens,
			CacheCreationTokens: record.CacheCreationTokens,
			TotalTokens:         record.TotalTokens,
		}
		breakdown := usageTokenBreakdownFromLegacy(legacy)
		if payloadBreakdown, errPayload := usageTokenBreakdownFromPayload(string(record.PayloadJSON), legacy); errPayload == nil {
			breakdown = payloadBreakdown
		}
		update := tx.WithContext(contextOrBackground(ctx)).Model(&UsageRecord{}).
			Where("id = ? AND token_accounting_version <> ?", record.ID, UsageTokenAccountingSchemaVersion).
			Updates(usageTokenBreakdownUpdates(breakdown))
		if update.Error != nil {
			return update.Error
		}
		result.Updated += update.RowsAffected
	}
	state.LastScannedID = records[len(records)-1].ID
	state.Done = state.LastScannedID >= state.HighWaterID
	result.Done = state.Done
	return saveUsageTokenAccountingBackfillState(ctx, tx, stateRecord, exists, state)
}

func firstPendingUsageTokenAccountingRecord(ctx context.Context, tx *gorm.DB) (*UsageRecord, error) {
	var record UsageRecord
	result := tx.WithContext(contextOrBackground(ctx)).Select("id").
		Where("token_accounting_version <> ?", UsageTokenAccountingSchemaVersion).
		Order("id ASC").
		Limit(1).
		Find(&record)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &record, nil
}

func latestUsageTokenAccountingRecord(ctx context.Context, tx *gorm.DB) (UsageRecord, error) {
	var record UsageRecord
	result := tx.WithContext(contextOrBackground(ctx)).Select("id").Order("id DESC").Limit(1).Find(&record)
	return record, result.Error
}

func loadUsageTokenAccountingBackfillState(ctx context.Context, tx *gorm.DB) (usageTokenAccountingBackfillState, KVRecord, bool, error) {
	record := KVRecord{}
	errFind := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", usageTokenAccountingBackfillStateKey).First(&record).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return usageTokenAccountingBackfillState{}, KVRecord{}, false, nil
	}
	if errFind != nil {
		return usageTokenAccountingBackfillState{}, KVRecord{}, false, errFind
	}
	state := usageTokenAccountingBackfillState{}
	if errDecode := json.Unmarshal(record.Value, &state); errDecode != nil {
		return usageTokenAccountingBackfillState{}, KVRecord{}, false, fmt.Errorf("decode usage token accounting backfill state: %w", errDecode)
	}
	return state, record, true, nil
}

func saveUsageTokenAccountingBackfillState(ctx context.Context, tx *gorm.DB, record KVRecord, exists bool, state usageTokenAccountingBackfillState) error {
	value, errEncode := json.Marshal(state)
	if errEncode != nil {
		return fmt.Errorf("encode usage token accounting backfill state: %w", errEncode)
	}
	if !exists {
		return tx.WithContext(contextOrBackground(ctx)).Create(&KVRecord{Key: usageTokenAccountingBackfillStateKey, Value: value, Version: 1}).Error
	}
	record.Value = value
	record.Version++
	if record.Version <= 1 {
		record.Version = 1
	}
	return tx.WithContext(contextOrBackground(ctx)).Save(&record).Error
}

// UsageTokenBreakdownV2Ready reports whether every persisted usage row has a
// canonical v2 breakdown. The indexed existence check remains cheap while the
// resumable backfill is still draining historical rows.
func (r *Repository) UsageTokenBreakdownV2Ready(ctx context.Context) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	var record UsageRecord
	result := db.WithContext(contextOrBackground(ctx)).Select("id").
		Where("token_accounting_version <> ?", UsageTokenAccountingSchemaVersion).
		Limit(1).
		Find(&record)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 0, nil
}
