package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	inFlightSnapshotCursorRandomBytes     = 24
	inFlightSnapshotCursorMaxPayloadBytes = 64 << 20
)

var ErrInFlightSnapshotCursorExpired = errors.New("in-flight snapshot cursor expired")

// InFlightSnapshotCursor is a short-lived immutable Management API payload.
type InFlightSnapshotCursor struct {
	Cursor    string
	Payload   []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	ReadAt    time.Time
}

// CreateInFlightSnapshotCursor persists one immutable payload for stable pagination.
func (r *Repository) CreateInFlightSnapshotCursor(ctx context.Context, payload []byte, ttl time.Duration) (InFlightSnapshotCursor, error) {
	if errValidate := validateInFlightSnapshotCursorPayload(payload, inFlightSnapshotCursorMaxPayloadBytes); errValidate != nil {
		return InFlightSnapshotCursor{}, errValidate
	}
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
		if errDelete := tx.WithContext(ctx).
			Where("expires_at <= ?", now).
			Delete(&ManagementInFlightSnapshotCursorRecord{}).Error; errDelete != nil {
			return errDelete
		}
		expiresAt := now.Add(ttl)
		record := ManagementInFlightSnapshotCursorRecord{
			Cursor:    cursor,
			Payload:   append(JSONB(nil), payload...),
			CreatedAt: now,
			ExpiresAt: expiresAt,
		}
		if errCreate := tx.WithContext(ctx).Create(&record).Error; errCreate != nil {
			return errCreate
		}
		result = InFlightSnapshotCursor{
			Cursor:    cursor,
			Payload:   append([]byte(nil), payload...),
			CreatedAt: now.UTC(),
			ExpiresAt: expiresAt.UTC(),
			ReadAt:    now.UTC(),
		}
		return nil
	})
	if errTransaction != nil {
		return InFlightSnapshotCursor{}, errTransaction
	}
	return result, nil
}

func validateInFlightSnapshotCursorPayload(payload []byte, maxBytes int) error {
	if len(payload) == 0 {
		return fmt.Errorf("in-flight snapshot cursor payload is empty")
	}
	if maxBytes > 0 && len(payload) > maxBytes {
		return fmt.Errorf("in-flight snapshot cursor payload exceeds %d bytes", maxBytes)
	}
	if !json.Valid(payload) {
		return fmt.Errorf("in-flight snapshot cursor payload is invalid JSON")
	}
	return nil
}

// ReadInFlightSnapshotCursor returns one unexpired immutable payload.
func (r *Repository) ReadInFlightSnapshotCursor(ctx context.Context, cursor string) (InFlightSnapshotCursor, error) {
	normalized, okCursor := normalizeInFlightSnapshotCursorToken(cursor)
	if !okCursor {
		return InFlightSnapshotCursor{}, ErrInFlightSnapshotCursorExpired
	}
	db, errDB := r.database()
	if errDB != nil {
		return InFlightSnapshotCursor{}, errDB
	}
	ctx = contextOrBackground(ctx)
	result := InFlightSnapshotCursor{}
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		record := ManagementInFlightSnapshotCursorRecord{}
		errFirst := tx.WithContext(ctx).
			Where("cursor = ? AND expires_at > ?", normalized, now).
			First(&record).Error
		if errors.Is(errFirst, gorm.ErrRecordNotFound) {
			return ErrInFlightSnapshotCursorExpired
		}
		if errFirst != nil {
			return errFirst
		}
		result = InFlightSnapshotCursor{
			Cursor:    record.Cursor,
			Payload:   append([]byte(nil), record.Payload...),
			CreatedAt: record.CreatedAt.UTC(),
			ExpiresAt: record.ExpiresAt.UTC(),
			ReadAt:    now.UTC(),
		}
		return nil
	})
	if errTransaction != nil {
		return InFlightSnapshotCursor{}, errTransaction
	}
	return result, nil
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
