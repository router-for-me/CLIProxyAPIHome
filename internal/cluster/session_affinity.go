package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SessionAffinityRecord stores opaque binding/session keys and a credential ID.
// Revisions prevent stale renewals or rebindings; sequence orders accepted selections.
type SessionAffinityRecord struct {
	Key               string    `gorm:"column:key;type:varchar(64);primaryKey"`
	CredentialID      string    `gorm:"column:credential_id;not null"`
	Revision          string    `gorm:"column:revision;type:varchar(36);not null"`
	ExpiresAt         time.Time `gorm:"column:expires_at;not null;index:idx_session_affinity_expires"`
	SessionKey        string    `gorm:"column:session_key;type:varchar(64);not null;default:'';index:idx_session_affinity_latest,priority:1"`
	Provider          string    `gorm:"column:provider;not null;default:'';index:idx_session_affinity_latest,priority:2"`
	SelectionSequence int64     `gorm:"column:selection_sequence;not null;default:0;index:idx_session_affinity_latest,priority:3,sort:desc"`
}

// SessionAffinitySequenceRecord serializes selection order only within one session.
// Its lease covers every binding in that session so cleanup cannot reset a live sequence.
type SessionAffinitySequenceRecord struct {
	Key       string    `gorm:"column:key;type:varchar(64);primaryKey"`
	Sequence  int64     `gorm:"column:sequence;not null"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null;index:idx_session_affinity_sequence_expires"`
}

func (SessionAffinitySequenceRecord) TableName() string {
	return "session_affinity_sequence"
}

func (SessionAffinityRecord) TableName() string {
	return "session_affinity"
}

// SessionAffinityStore exposes the authoritative store without caching bindings
// in the adapter. Every Home must observe conditional rebindings immediately.
func (a *RuntimeAdapter) SessionAffinityStore() coreauth.SessionAffinityStore {
	return a.repo
}

func (r *Repository) LoadSessionBinding(ctx context.Context, key string) (coreauth.SessionBinding, error) {
	db, errDB := r.database()
	if errDB != nil {
		return coreauth.SessionBinding{}, errDB
	}
	var record SessionAffinityRecord
	errFind := db.WithContext(contextOrBackground(ctx)).Where("key = ?", key).Take(&record).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return coreauth.SessionBinding{}, nil
	}
	if errFind != nil {
		return coreauth.SessionBinding{}, errFind
	}
	return coreauth.SessionBinding{AuthID: record.CredentialID, Revision: record.Revision, ExpiresAt: record.ExpiresAt}, nil
}

func (r *Repository) LoadLatestSessionBinding(ctx context.Context, sessionKey string, providers []string) (coreauth.SessionBinding, error) {
	if sessionKey == "" || len(providers) == 0 {
		return coreauth.SessionBinding{}, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return coreauth.SessionBinding{}, errDB
	}
	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return coreauth.SessionBinding{}, errNow
	}
	var record SessionAffinityRecord
	errFind := db.WithContext(contextOrBackground(ctx)).
		Where("session_key = ? AND provider IN ? AND expires_at > ? AND selection_sequence > 0", sessionKey, providers, now).
		Order("selection_sequence DESC").Take(&record).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return coreauth.SessionBinding{}, nil
	}
	if errFind != nil {
		return coreauth.SessionBinding{}, errFind
	}
	return coreauth.SessionBinding{AuthID: record.CredentialID, Revision: record.Revision, ExpiresAt: record.ExpiresAt}, nil
}

func (r *Repository) CompareAndSwapSessionBinding(ctx context.Context, commit coreauth.SessionAffinityCommit) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	var swapped bool
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		var errSwap error
		swapped, errSwap = compareAndSwapSessionBindingTx(tx, commit)
		return errSwap
	})
	return swapped && errTransaction == nil, errTransaction
}

// compareAndSwapSessionBindingTx conditionally commits a binding and its selection
// order in the caller's transaction. Rejected admission rolls both changes back.
func compareAndSwapSessionBindingTx(tx *gorm.DB, commit coreauth.SessionAffinityCommit) (bool, error) {
	if len(commit.Key) != 64 || commit.AuthID == "" || commit.TTL <= 0 {
		return false, fmt.Errorf("invalid session affinity commit")
	}
	if (commit.SessionKey != "" || commit.Provider != "") && (len(commit.SessionKey) != 64 || commit.Provider == "") {
		return false, fmt.Errorf("invalid session selection metadata")
	}
	now, errNow := DatabaseNow(tx.Statement.Context, tx)
	if errNow != nil {
		return false, errNow
	}
	record := SessionAffinityRecord{
		Key: commit.Key, CredentialID: commit.AuthID,
		Revision: uuid.NewString(), ExpiresAt: now.Add(commit.TTL),
	}
	var result *gorm.DB
	if commit.ExpectedRevision == "" {
		result = tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).Create(&record)
	} else {
		query := tx.Model(&SessionAffinityRecord{}).Where("key = ? AND revision = ?", commit.Key, commit.ExpectedRevision)
		if !commit.ExpiredAt.IsZero() {
			query = query.Where("expires_at = ?", commit.ExpiredAt.UTC())
		}
		result = query.Updates(map[string]any{
			"credential_id": record.CredentialID,
			"session_key":   commit.SessionKey, "provider": commit.Provider, "selection_sequence": 0,
			// Concurrent renewals of the same credential do not invalidate each
			// other; rebinds, expired claims, and recreations change it.
			"revision":   gorm.Expr("CASE WHEN credential_id = ? AND expires_at > ? THEN revision ELSE ? END", record.CredentialID, now, record.Revision),
			"expires_at": record.ExpiresAt,
		})
	}
	if result.Error != nil || result.RowsAffected != 1 {
		return false, result.Error
	}
	if commit.SessionKey == "" {
		return true, nil
	}
	// The upsert holds a per-session row lock until admission commits. It gives
	// different models/providers a common order without relying on node clocks.
	sequence := SessionAffinitySequenceRecord{Key: commit.SessionKey, Sequence: 1, ExpiresAt: record.ExpiresAt}
	errSequence := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"sequence":   gorm.Expr("session_affinity_sequence.sequence + 1"),
			"expires_at": gorm.Expr("CASE WHEN session_affinity_sequence.expires_at > ? THEN session_affinity_sequence.expires_at ELSE ? END", record.ExpiresAt, record.ExpiresAt),
		}),
	}, clause.Returning{}).Create(&sequence).Error
	if errSequence != nil {
		return false, errSequence
	}
	errSelection := tx.Model(&SessionAffinityRecord{}).Where("key = ?", commit.Key).Updates(map[string]any{
		"session_key": commit.SessionKey, "provider": commit.Provider, "selection_sequence": sequence.Sequence,
	}).Error
	return errSelection == nil, errSelection
}

// PurgeExpiredSessionBindings deletes a bounded batch without racing renewals.
func (r *Repository) PurgeExpiredSessionBindings(ctx context.Context, now time.Time, limit int) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	if limit <= 0 {
		return 0, nil
	}
	db = db.WithContext(contextOrBackground(ctx))
	keys := db.Model(&SessionAffinityRecord{}).Select("key").Where("expires_at <= ?", now.UTC()).Order("expires_at").Limit(limit)
	result := db.Where("key IN (?) AND expires_at <= ?", keys, now.UTC()).Delete(&SessionAffinityRecord{})
	if result.Error != nil {
		return result.RowsAffected, result.Error
	}
	sequenceKeys := db.Model(&SessionAffinitySequenceRecord{}).Select("key").Where("expires_at <= ?", now.UTC()).Order("expires_at").Limit(limit)
	errPurge := db.Where("key IN (?) AND expires_at <= ?", sequenceKeys, now.UTC()).Delete(&SessionAffinitySequenceRecord{}).Error
	return result.RowsAffected, errPurge
}
