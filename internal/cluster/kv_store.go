package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type KVSetMode string

const (
	KVSetModeAlways KVSetMode = ""
	KVSetModeNX     KVSetMode = "nx"
	KVSetModeXX     KVSetMode = "xx"
)

type KVGetResult struct {
	Value []byte
	Found bool
}

// KVGet returns the active value for a key.
func (r *Repository) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, false, errDB
	}
	key, errKey := normalizeKVKey(key)
	if errKey != nil {
		return nil, false, errKey
	}

	record := KVRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("key = ?", key).First(&record).Error
	switch {
	case errors.Is(errFirst, gorm.ErrRecordNotFound):
		return nil, false, nil
	case errFirst != nil:
		return nil, false, errFirst
	}
	now := time.Now().UTC()
	if kvRecordExpired(record, now) {
		if errDelete := lazyDeleteExpiredKV(ctx, db, key, record.ExpiresAt); errDelete != nil {
			return nil, false, errDelete
		}
		return nil, false, nil
	}
	return cloneKVBytes(record.Value), true, nil
}

// KVSet writes a value using the requested conditional mode.
func (r *Repository) KVSet(ctx context.Context, key string, value []byte, ttl time.Duration, mode KVSetMode) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	key, errKey := normalizeKVKey(key)
	if errKey != nil {
		return false, errKey
	}
	if errMode := validateKVSetMode(mode); errMode != nil {
		return false, errMode
	}
	copiedValue := cloneKVBytes(value)
	expiresAt := kvExpiresAt(ttl)

	ctx = contextOrBackground(ctx)
	written := false
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := KVRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&record).Error
		now := time.Now().UTC()
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			if mode == KVSetModeXX {
				return nil
			}
			record = KVRecord{Key: key, Value: copiedValue, Version: 1, ExpiresAt: expiresAt}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			written = true
			return nil
		case errFirst != nil:
			return errFirst
		case kvRecordExpired(record, now):
			if errDelete := deleteExpiredKVTx(tx, key, record.ExpiresAt); errDelete != nil {
				return errDelete
			}
			if mode == KVSetModeXX {
				return nil
			}
			record = KVRecord{Key: key, Value: copiedValue, Version: record.Version + 1, ExpiresAt: expiresAt}
			if record.Version <= 1 {
				record.Version = 1
			}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			written = true
			return nil
		case mode == KVSetModeNX:
			return nil
		default:
			record.Value = copiedValue
			record.ExpiresAt = expiresAt
			record.Version++
			if errSave := tx.Save(&record).Error; errSave != nil {
				return errSave
			}
			written = true
			return nil
		}
	})
	return written, errTransaction
}

// KVCompareAndSwap writes a value only when the active state matches the
// expected state, and reports whether the swap happened.
//
// When expectedExists is true the key must be present and active with a value
// equal to expected. When expectedExists is false the key must be absent or
// expired. The comparison and the write share one transaction so concurrent
// writers cannot both observe a matching state.
func (r *Repository) KVCompareAndSwap(ctx context.Context, key string, expected []byte, expectedExists bool, value []byte, ttl time.Duration) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	key, errKey := normalizeKVKey(key)
	if errKey != nil {
		return false, errKey
	}
	copiedValue := cloneKVBytes(value)
	expectedValue := cloneKVBytes(expected)
	expiresAt := kvExpiresAt(ttl)

	ctx = contextOrBackground(ctx)
	swapped := false
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := KVRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&record).Error
		now := time.Now().UTC()
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			if expectedExists {
				return nil
			}
			record = KVRecord{Key: key, Value: copiedValue, Version: 1, ExpiresAt: expiresAt}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			swapped = true
			return nil
		case errFirst != nil:
			return errFirst
		case kvRecordExpired(record, now):
			if errDelete := deleteExpiredKVTx(tx, key, record.ExpiresAt); errDelete != nil {
				return errDelete
			}
			if expectedExists {
				return nil
			}
			nextVersion := record.Version + 1
			if nextVersion <= 1 {
				nextVersion = 1
			}
			record = KVRecord{Key: key, Value: copiedValue, Version: nextVersion, ExpiresAt: expiresAt}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			swapped = true
			return nil
		case !expectedExists || !bytes.Equal(record.Value, expectedValue):
			return nil
		default:
			record.Value = copiedValue
			record.ExpiresAt = expiresAt
			record.Version++
			if errSave := tx.Save(&record).Error; errSave != nil {
				return errSave
			}
			swapped = true
			return nil
		}
	})
	return swapped, errTransaction
}

// KVDel deletes active keys and returns the active deletion count.
func (r *Repository) KVDel(ctx context.Context, keys []string) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	normalizedKeys, errKeys := normalizeKVKeys(keys)
	if errKeys != nil {
		return 0, errKeys
	}
	if len(normalizedKeys) == 0 {
		return 0, nil
	}

	ctx = contextOrBackground(ctx)
	var deleted int64
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		for _, key := range normalizedKeys {
			record := KVRecord{}
			errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&record).Error
			switch {
			case errors.Is(errFirst, gorm.ErrRecordNotFound):
				continue
			case errFirst != nil:
				return errFirst
			case kvRecordExpired(record, now):
				if errDelete := deleteExpiredKVTx(tx, key, record.ExpiresAt); errDelete != nil {
					return errDelete
				}
				continue
			}
			if errDelete := tx.Delete(&KVRecord{}, "key = ?", key).Error; errDelete != nil {
				return errDelete
			}
			deleted++
		}
		return nil
	})
	return deleted, errTransaction
}

// KVExpire updates the TTL for an active key.
func (r *Repository) KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	key, errKey := normalizeKVKey(key)
	if errKey != nil {
		return false, errKey
	}
	expiresAt := time.Now().UTC().Add(ttl)

	ctx = contextOrBackground(ctx)
	updated := false
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := KVRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&record).Error
		now := time.Now().UTC()
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			return nil
		case errFirst != nil:
			return errFirst
		case kvRecordExpired(record, now):
			return deleteExpiredKVTx(tx, key, record.ExpiresAt)
		}
		record.ExpiresAt = &expiresAt
		record.Version++
		if errSave := tx.Save(&record).Error; errSave != nil {
			return errSave
		}
		updated = true
		return nil
	})
	return updated, errTransaction
}

// KVTTL returns Redis-compatible TTL seconds.
func (r *Repository) KVTTL(ctx context.Context, key string) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	key, errKey := normalizeKVKey(key)
	if errKey != nil {
		return 0, errKey
	}

	record := KVRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("key = ?", key).First(&record).Error
	switch {
	case errors.Is(errFirst, gorm.ErrRecordNotFound):
		return -2, nil
	case errFirst != nil:
		return 0, errFirst
	}
	now := time.Now().UTC()
	if kvRecordExpired(record, now) {
		if errDelete := lazyDeleteExpiredKV(ctx, db, key, record.ExpiresAt); errDelete != nil {
			return 0, errDelete
		}
		return -2, nil
	}
	if record.ExpiresAt == nil {
		return -1, nil
	}
	remaining := int64(record.ExpiresAt.Sub(now) / time.Second)
	if remaining < 0 {
		return -2, nil
	}
	return remaining, nil
}

// KVIncrBy increments a decimal integer value.
func (r *Repository) KVIncrBy(ctx context.Context, key string, delta int64) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	key, errKey := normalizeKVKey(key)
	if errKey != nil {
		return 0, errKey
	}

	ctx = contextOrBackground(ctx)
	var out int64
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := KVRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&record).Error
		now := time.Now().UTC()
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			out = delta
			record = KVRecord{Key: key, Value: []byte(strconv.FormatInt(out, 10)), Version: 1}
			return tx.Create(&record).Error
		case errFirst != nil:
			return errFirst
		case kvRecordExpired(record, now):
			if errDelete := deleteExpiredKVTx(tx, key, record.ExpiresAt); errDelete != nil {
				return errDelete
			}
			out = delta
			record = KVRecord{Key: key, Value: []byte(strconv.FormatInt(out, 10)), Version: record.Version + 1}
			if record.Version <= 1 {
				record.Version = 1
			}
			return tx.Create(&record).Error
		}
		current, errParse := strconv.ParseInt(string(record.Value), 10, 64)
		if errParse != nil {
			return fmt.Errorf("kv value is not an integer: %w", errParse)
		}
		out = current + delta
		record.Value = []byte(strconv.FormatInt(out, 10))
		record.Version++
		return tx.Save(&record).Error
	})
	return out, errTransaction
}

// KVMGet returns values in the same order as the requested keys.
func (r *Repository) KVMGet(ctx context.Context, keys []string) ([]KVGetResult, error) {
	results := make([]KVGetResult, 0, len(keys))
	for _, key := range keys {
		value, found, errGet := r.KVGet(ctx, key)
		if errGet != nil {
			return nil, errGet
		}
		results = append(results, KVGetResult{Value: value, Found: found})
	}
	return results, nil
}

// KVMSet atomically writes key/value pairs without TTL.
func (r *Repository) KVMSet(ctx context.Context, pairs map[string][]byte) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	keys := make([]string, 0, len(pairs))
	values := make(map[string][]byte, len(pairs))
	for key, value := range pairs {
		normalizedKey, errKey := normalizeKVKey(key)
		if errKey != nil {
			return errKey
		}
		keys = append(keys, normalizedKey)
		values[normalizedKey] = cloneKVBytes(value)
	}
	sort.Strings(keys)

	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, key := range keys {
			record := KVRecord{}
			errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&record).Error
			switch {
			case errors.Is(errFirst, gorm.ErrRecordNotFound):
				record = KVRecord{Key: key, Value: values[key], Version: 1}
				if errCreate := tx.Create(&record).Error; errCreate != nil {
					return errCreate
				}
			case errFirst != nil:
				return errFirst
			default:
				record.Value = values[key]
				record.ExpiresAt = nil
				record.Version++
				if errSave := tx.Save(&record).Error; errSave != nil {
					return errSave
				}
			}
		}
		return nil
	})
}

// KVPurgeExpired deletes expired rows up to limit.
func (r *Repository) KVPurgeExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	if limit <= 0 {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var keys []string
	query := db.WithContext(contextOrBackground(ctx)).
		Model(&KVRecord{}).
		Where("expires_at IS NOT NULL AND expires_at <= ?", now).
		Order("key").
		Limit(limit).
		Pluck("key", &keys)
	if query.Error != nil {
		return 0, query.Error
	}
	if len(keys) == 0 {
		return 0, nil
	}
	result := db.WithContext(contextOrBackground(ctx)).
		Where("key IN ? AND expires_at IS NOT NULL AND expires_at <= ?", keys, now).
		Delete(&KVRecord{})
	return result.RowsAffected, result.Error
}

func normalizeKVKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("kv key is required")
	}
	return key, nil
}

func normalizeKVKeys(keys []string) ([]string, error) {
	out := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		normalizedKey, errKey := normalizeKVKey(key)
		if errKey != nil {
			return nil, errKey
		}
		if _, ok := seen[normalizedKey]; ok {
			continue
		}
		seen[normalizedKey] = struct{}{}
		out = append(out, normalizedKey)
	}
	sort.Strings(out)
	return out, nil
}

func validateKVSetMode(mode KVSetMode) error {
	switch mode {
	case KVSetModeAlways, KVSetModeNX, KVSetModeXX:
		return nil
	default:
		return fmt.Errorf("unsupported kv set mode %q", mode)
	}
}

func cloneKVBytes(value []byte) []byte {
	if len(value) == 0 {
		return []byte{}
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out
}

func kvExpiresAt(ttl time.Duration) *time.Time {
	if ttl <= 0 {
		return nil
	}
	expiresAt := time.Now().UTC().Add(ttl)
	return &expiresAt
}

func kvRecordExpired(record KVRecord, now time.Time) bool {
	return record.ExpiresAt != nil && !record.ExpiresAt.After(now)
}

func lazyDeleteExpiredKV(ctx context.Context, db *gorm.DB, key string, expiresAt *time.Time) error {
	if db == nil || expiresAt == nil {
		return nil
	}
	return db.WithContext(contextOrBackground(ctx)).
		Where("key = ? AND expires_at IS NOT NULL AND expires_at <= ?", key, *expiresAt).
		Delete(&KVRecord{}).Error
}

func deleteExpiredKVTx(tx *gorm.DB, key string, expiresAt *time.Time) error {
	if tx == nil || expiresAt == nil {
		return nil
	}
	return tx.
		Where("key = ? AND expires_at IS NOT NULL AND expires_at <= ?", key, *expiresAt).
		Delete(&KVRecord{}).Error
}
