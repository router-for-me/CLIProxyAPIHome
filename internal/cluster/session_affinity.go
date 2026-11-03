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

// SessionAffinityRecord stores only an opaque session key and a credential ID.
// Revisions prevent stale renewals or rebindings, even after expiry and cleanup.
type SessionAffinityRecord struct {
	Key          string    `gorm:"column:key;type:varchar(64);primaryKey"`
	CredentialID string    `gorm:"column:credential_id;not null"`
	Revision     string    `gorm:"column:revision;type:varchar(36);not null"`
	ExpiresAt    time.Time `gorm:"column:expires_at;not null;index:idx_session_affinity_expires"`
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

func (r *Repository) CompareAndSwapSessionBinding(ctx context.Context, commit coreauth.SessionAffinityCommit) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	return compareAndSwapSessionBindingTx(db.WithContext(contextOrBackground(ctx)), commit)
}

// compareAndSwapSessionBindingTx uses one conditional statement, including for
// absent rows. It may run inside the concurrency admission transaction, so a
// rejected admission never establishes or renews a session binding.
func compareAndSwapSessionBindingTx(tx *gorm.DB, commit coreauth.SessionAffinityCommit) (bool, error) {
	if len(commit.Key) != 64 || commit.AuthID == "" || commit.TTL <= 0 {
		return false, fmt.Errorf("invalid session affinity commit")
	}
	now := time.Now().UTC()
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
			// Concurrent renewals of the same credential do not invalidate each
			// other; rebinds, expired claims, and recreations change it.
			"revision":   gorm.Expr("CASE WHEN credential_id = ? AND expires_at > ? THEN revision ELSE ? END", record.CredentialID, now, record.Revision),
			"expires_at": record.ExpiresAt,
		})
	}
	return result.RowsAffected == 1, result.Error
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
	return result.RowsAffected, result.Error
}
