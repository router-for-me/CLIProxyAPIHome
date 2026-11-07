package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

const (
	OAuthSessionTTL          = 20 * time.Minute
	oauthCompletedSessionTTL = time.Minute
)

// ErrOAuthSessionNotPending reports that session data can no longer be changed.
var ErrOAuthSessionNotPending = errors.New("oauth session is not pending")

// NewOAuthSessionRecord creates a new o auth session record.
func NewOAuthSessionRecord(provider, state string, data map[string]any, now time.Time) (*OAuthSessionRecord, error) {
	// Resolve credential context before calling upstream OAuth services.
	provider = strings.ToLower(strings.TrimSpace(provider))
	state = strings.TrimSpace(state)
	if provider == "" {
		return nil, fmt.Errorf("oauth provider is required")
	}
	if state == "" {
		return nil, fmt.Errorf("oauth state is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	rawData, errMarshal := json.Marshal(data)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return &OAuthSessionRecord{
		State:     state,
		Provider:  provider,
		Data:      JSONB(rawData),
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(OAuthSessionTTL),
	}, nil
}

// OAuthSessionData handles an o auth session data.
func OAuthSessionData(record *OAuthSessionRecord) (map[string]any, error) {
	if record == nil || len(record.Data) == 0 {
		return nil, nil
	}
	var data map[string]any
	if errUnmarshal := json.Unmarshal([]byte(record.Data), &data); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return data, nil
}

// UpsertOAuthSession inserts or updates an o auth session.
func (r *Repository) UpsertOAuthSession(ctx context.Context, record *OAuthSessionRecord) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if record == nil {
		return fmt.Errorf("oauth session record is nil")
	}
	if strings.TrimSpace(record.State) == "" {
		return fmt.Errorf("oauth state is required")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	record.UpdatedAt = time.Now().UTC()
	if record.ExpiresAt.IsZero() {
		record.ExpiresAt = record.UpdatedAt.Add(OAuthSessionTTL)
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.ExpiresAt = record.ExpiresAt.UTC()
	if errDelete := deleteExpiredCompletedOAuthSessions(ctx, db, time.Now().UTC()); errDelete != nil {
		return errDelete
	}
	return db.WithContext(contextOrBackground(ctx)).Save(record).Error
}

// GetOAuthSession returns an o auth session.
func (r *Repository) GetOAuthSession(ctx context.Context, state string) (*OAuthSessionRecord, error) {
	// Resolve credential context before calling upstream OAuth services.
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, fmt.Errorf("oauth state is required")
	}
	record := &OAuthSessionRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("state = ?", state).First(record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if errFirst != nil {
		return nil, errFirst
	}
	now := time.Now().UTC()
	if !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(now) && strings.EqualFold(record.Status, "complete") {
		if errDelete := deleteExpiredCompletedOAuthSessions(ctx, db, now); errDelete != nil {
			return nil, errDelete
		}
		return nil, nil
	}
	if !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(now) && record.Status == "" {
		record.Status = "error"
		record.Error = "OAuth flow timed out"
		record.UpdatedAt = now
		if errExpire := db.WithContext(contextOrBackground(ctx)).Model(&OAuthSessionRecord{}).
			Where("state = ? AND status = ? AND expires_at <= ?", state, "", now).
			Updates(map[string]any{"status": "error", "error": record.Error, "updated_at": now, "data": nil}).Error; errExpire != nil {
			return nil, errExpire
		}
		// Reload after the conditional write to preserve a concurrent completion or cancellation.
		if errReload := db.WithContext(contextOrBackground(ctx)).Where("state = ?", state).First(record).Error; errReload != nil {
			return nil, errReload
		}
	}
	return record, nil
}

func deleteExpiredCompletedOAuthSessions(ctx context.Context, db *gorm.DB, now time.Time) error {
	return db.WithContext(contextOrBackground(ctx)).
		Where("status = ? AND expires_at <= ?", "complete", now).
		Delete(&OAuthSessionRecord{}).Error
}

// MergeOAuthSessionData merges an o auth session data.
func (r *Repository) MergeOAuthSessionData(ctx context.Context, state string, values map[string]any) error {
	// Resolve credential context before calling upstream OAuth services.
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return fmt.Errorf("oauth state is required")
	}
	record, errRecord := r.GetOAuthSession(ctx, state)
	if errRecord != nil {
		return errRecord
	}
	if record == nil {
		return fmt.Errorf("oauth session not found")
	}
	if strings.TrimSpace(record.Status) != "" {
		return ErrOAuthSessionNotPending
	}
	data, errData := OAuthSessionData(record)
	if errData != nil {
		return errData
	}
	if data == nil {
		data = make(map[string]any, len(values))
	}
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if value == nil {
			delete(data, key)
			continue
		}
		data[key] = value
	}
	rawData, errMarshal := json.Marshal(data)
	if errMarshal != nil {
		return errMarshal
	}
	record.Data = JSONB(rawData)
	record.UpdatedAt = time.Now().UTC()
	result := db.WithContext(contextOrBackground(ctx)).
		Model(record).
		Where("state = ? AND status = ? AND expires_at > ?", state, "", record.UpdatedAt).
		Select("data", "updated_at").
		Updates(record)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrOAuthSessionNotPending
	}
	return nil
}

// CompleteOAuthSession completes a pending login; repeated completion is a no-op.
func (r *Repository) CompleteOAuthSession(ctx context.Context, state string) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	errComplete := completePendingOAuthSession(ctx, db, state)
	if errors.Is(errComplete, ErrOAuthSessionNotPending) {
		var record OAuthSessionRecord
		if errRead := db.WithContext(contextOrBackground(ctx)).Where("state = ?", strings.TrimSpace(state)).First(&record).Error; errRead == nil && record.Status == "complete" {
			return nil
		}
	}
	return errComplete
}

// CompleteOAuthSessionWithAuths atomically claims a pending session and persists its credentials.
// The conditional update serializes completion against cancellation across Home nodes.
func (r *Repository) CompleteOAuthSessionWithAuths(ctx context.Context, state string, auths []*coreauth.Auth) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if len(auths) == 0 {
		return fmt.Errorf("oauth credentials are required")
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		if errComplete := completePendingOAuthSession(ctx, tx, state); errComplete != nil {
			return errComplete
		}
		txRepo := NewRepository(tx)
		authIDs := make([]string, 0, len(auths))
		seen := make(map[string]bool, len(auths))
		for _, auth := range auths {
			if auth == nil {
				return fmt.Errorf("oauth credential is nil")
			}
			record, errUpsert := txRepo.UpsertAuth(ctx, auth, "upsert")
			if errUpsert != nil {
				return errUpsert
			}
			if !seen[record.UUID] {
				authIDs = append(authIDs, record.UUID)
				seen[record.UUID] = true
			}
		}
		// Keep only the public result after clearing the pending session's secrets.
		result, errMarshal := json.Marshal(map[string]any{"auth_ids": authIDs})
		if errMarshal != nil {
			return errMarshal
		}
		return tx.Model(&OAuthSessionRecord{}).Where("state = ?", strings.TrimSpace(state)).
			Update("data", JSONB(result)).Error
	})
}

func completePendingOAuthSession(ctx context.Context, db *gorm.DB, state string) error {
	now := time.Now().UTC()
	result := db.WithContext(contextOrBackground(ctx)).Model(&OAuthSessionRecord{}).
		Where("state = ? AND status = ? AND expires_at > ?", strings.TrimSpace(state), "", now).
		Updates(map[string]any{"status": "complete", "error": "", "data": nil,
			"updated_at": now, "expires_at": now.Add(oauthCompletedSessionTTL), "completed_at": &now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrOAuthSessionNotPending
	}
	return nil
}

// SetOAuthSessionError sets an o auth session error.
func (r *Repository) SetOAuthSessionError(ctx context.Context, state string, message string) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Authentication failed"
	}
	now := time.Now().UTC()
	return db.WithContext(contextOrBackground(ctx)).
		Model(&OAuthSessionRecord{}).
		Where("state = ? AND status = ? AND expires_at > ?", strings.TrimSpace(state), "", now).
		Updates(map[string]any{
			"status":     "error",
			"error":      message,
			"data":       nil,
			"updated_at": now,
			"expires_at": now.Add(OAuthSessionTTL),
		}).Error
}

// CancelOAuthSession marks only pending sessions as cancelled, preventing a later
// callback or polling request on any Home node from completing the login.
func (r *Repository) CancelOAuthSession(ctx context.Context, state string) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	result := db.WithContext(contextOrBackground(ctx)).Model(&OAuthSessionRecord{}).
		Where("state = ? AND status = ?", strings.TrimSpace(state), "").
		Updates(map[string]any{"status": "error", "error": "Authentication cancelled", "data": nil, "updated_at": time.Now().UTC()})
	return result.RowsAffected > 0, result.Error
}
