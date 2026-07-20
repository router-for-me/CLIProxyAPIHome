package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	UserSecurityTokenPurposeEmailVerification = "verify_email"
	UserSecurityTokenPurposePasswordReset     = "reset_password"

	UserMailJobStatusPending    = "pending"
	UserMailJobStatusSending    = "sending"
	UserMailJobStatusSent       = "sent"
	UserMailJobStatusFailed     = "failed"
	UserMailJobStatusSuperseded = "superseded"
)

var ErrUserSecurityTokenInvalid = errors.New("user security token is invalid or expired")

var ErrUserMailJobClaimLost = errors.New("user mail job claim is no longer active")

// HashUserSecurityValue returns a stable SHA-256 hex digest for tokens and throttle scopes.
func HashUserSecurityValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ReplaceUserSecurityToken invalidates outstanding tokens for the same purpose and stores a new hash.
func (r *Repository) ReplaceUserSecurityToken(ctx context.Context, record UserSecurityTokenRecord) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if record.UserID == 0 || strings.TrimSpace(record.Purpose) == "" || strings.TrimSpace(record.TokenHash) == "" {
		return fmt.Errorf("user security token fields are required")
	}
	if record.ExpiresAt.IsZero() {
		return fmt.Errorf("user security token expiry is required")
	}
	record.Purpose = strings.TrimSpace(record.Purpose)
	record.TokenHash = strings.TrimSpace(record.TokenHash)
	record.CreatedAt = record.CreatedAt.UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	record.ExpiresAt = record.ExpiresAt.UTC()

	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		if errInvalidate := invalidateUserSecurityTokensTx(tx, record.UserID, record.Purpose, record.CreatedAt); errInvalidate != nil {
			return errInvalidate
		}
		return tx.Create(&record).Error
	})
}

// ReplaceUserSecurityTokenForMailJob stores a token only while the originating
// mail job is still claimed by the worker. A concurrent resend or email change
// supersedes the job and prevents an older worker from replacing the newer token.
func (r *Repository) ReplaceUserSecurityTokenForMailJob(ctx context.Context, jobID uint, workerID string, record UserSecurityTokenRecord) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	workerID = strings.TrimSpace(workerID)
	if jobID == 0 || workerID == "" {
		return fmt.Errorf("user mail job claim is required")
	}
	if record.UserID == 0 || strings.TrimSpace(record.Purpose) == "" || strings.TrimSpace(record.TokenHash) == "" {
		return fmt.Errorf("user security token fields are required")
	}
	if record.ExpiresAt.IsZero() {
		return fmt.Errorf("user security token expiry is required")
	}
	record.Purpose = strings.TrimSpace(record.Purpose)
	record.TokenHash = strings.TrimSpace(record.TokenHash)
	record.CreatedAt = record.CreatedAt.UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	record.ExpiresAt = record.ExpiresAt.UTC()

	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		// User mutations lock the user before touching security tokens or mail
		// jobs. Keep the same order here to avoid PostgreSQL deadlocks with a
		// concurrent password or email update.
		user := UserRecord{}
		if errUser := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", record.UserID).First(&user).Error; errUser != nil {
			if errors.Is(errUser, gorm.ErrRecordNotFound) {
				return ErrUserMailJobClaimLost
			}
			return errUser
		}
		if user.Email == nil || strings.TrimSpace(*user.Email) == "" || user.EmailVersion != record.EmailVersion {
			return ErrUserMailJobClaimLost
		}
		if record.Purpose == UserSecurityTokenPurposePasswordReset && user.EmailVerifiedAt == nil {
			return ErrUserMailJobClaimLost
		}

		job := UserMailJobRecord{}
		errJob := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND claimed_by = ? AND claim_expires_at > ? AND user_id = ? AND purpose = ? AND email_version = ?", jobID, UserMailJobStatusSending, workerID, record.CreatedAt, record.UserID, record.Purpose, record.EmailVersion).
			First(&job).Error
		if errors.Is(errJob, gorm.ErrRecordNotFound) {
			return ErrUserMailJobClaimLost
		}
		if errJob != nil {
			return errJob
		}
		if record.Purpose == UserSecurityTokenPurposePasswordReset && job.SessionVersion != user.SessionVersion {
			return ErrUserMailJobClaimLost
		}
		if errInvalidate := invalidateUserSecurityTokensTx(tx, record.UserID, record.Purpose, record.CreatedAt); errInvalidate != nil {
			return errInvalidate
		}
		return tx.Create(&record).Error
	})
}

// ConsumeUserEmailVerificationToken verifies an email token and marks the current email as verified.
func (r *Repository) ConsumeUserEmailVerificationToken(ctx context.Context, tokenHash string, now time.Time) (*UserRecord, error) {
	return r.consumeUserSecurityToken(ctx, tokenHash, UserSecurityTokenPurposeEmailVerification, "", now)
}

// ConsumeUserPasswordResetToken updates the password, revokes sessions, and consumes the reset token.
func (r *Repository) ConsumeUserPasswordResetToken(ctx context.Context, tokenHash string, passwordHash string, now time.Time) (*UserRecord, error) {
	passwordHash = strings.TrimSpace(passwordHash)
	if passwordHash == "" {
		return nil, fmt.Errorf("password hash is required")
	}
	return r.consumeUserSecurityToken(ctx, tokenHash, UserSecurityTokenPurposePasswordReset, passwordHash, now)
}

func (r *Repository) consumeUserSecurityToken(ctx context.Context, tokenHash string, purpose string, passwordHash string, now time.Time) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return nil, ErrUserSecurityTokenInvalid
	}
	now = now.UTC()
	candidate := UserSecurityTokenRecord{}
	errCandidate := db.WithContext(contextOrBackground(ctx)).
		Select("user_id").
		Where("token_hash = ? AND purpose = ?", tokenHash, purpose).
		First(&candidate).Error
	if errors.Is(errCandidate, gorm.ErrRecordNotFound) {
		return nil, ErrUserSecurityTokenInvalid
	}
	if errCandidate != nil {
		return nil, errCandidate
	}
	record := &UserRecord{}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		// Lock the user first, matching UpdateUser and mail-job token creation.
		// The token is reloaded under lock below, so a concurrent consume or
		// invalidation between the lookup and this transaction remains safe.
		if errUser := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", candidate.UserID).First(record).Error; errUser != nil {
			if errors.Is(errUser, gorm.ErrRecordNotFound) {
				return ErrUserSecurityTokenInvalid
			}
			return errUser
		}
		token := UserSecurityTokenRecord{}
		errToken := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("token_hash = ? AND purpose = ? AND user_id = ? AND used_at IS NULL AND expires_at > ?", tokenHash, purpose, candidate.UserID, now).
			First(&token).Error
		if errors.Is(errToken, gorm.ErrRecordNotFound) {
			return ErrUserSecurityTokenInvalid
		}
		if errToken != nil {
			return errToken
		}
		if record.Email == nil || strings.TrimSpace(*record.Email) == "" || record.EmailVerifiedAt == nil && purpose == UserSecurityTokenPurposePasswordReset {
			return ErrUserSecurityTokenInvalid
		}
		if token.EmailVersion != record.EmailVersion {
			return ErrUserSecurityTokenInvalid
		}

		switch purpose {
		case UserSecurityTokenPurposeEmailVerification:
			verifiedAt := now
			record.EmailVerifiedAt = &verifiedAt
		case UserSecurityTokenPurposePasswordReset:
			record.Password = passwordHash
			record.SessionVersion++
		default:
			return ErrUserSecurityTokenInvalid
		}
		if errSave := tx.Save(record).Error; errSave != nil {
			return errSave
		}
		if purpose == UserSecurityTokenPurposePasswordReset {
			return invalidateUserPasswordSecurityTx(tx, record.ID, now)
		}
		return invalidateUserSecurityTokensTx(tx, record.ID, purpose, now)
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

func invalidateUserSecurityTokensTx(tx *gorm.DB, userID uint, purpose string, now time.Time) error {
	if tx == nil || userID == 0 {
		return nil
	}
	query := tx.Model(&UserSecurityTokenRecord{}).Where("user_id = ? AND used_at IS NULL", userID)
	if strings.TrimSpace(purpose) != "" {
		query = query.Where("purpose = ?", strings.TrimSpace(purpose))
	}
	return query.Update("used_at", now.UTC()).Error
}

// InvalidateUserSecurityTokenHash invalidates one token after a failed delivery attempt.
func (r *Repository) InvalidateUserSecurityTokenHash(ctx context.Context, tokenHash string, now time.Time) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Model(&UserSecurityTokenRecord{}).
		Where("token_hash = ? AND used_at IS NULL", strings.TrimSpace(tokenHash)).
		Update("used_at", now.UTC()).Error
}

func invalidateUserEmailSecurityTx(tx *gorm.DB, userID uint, now time.Time) error {
	if errTokens := invalidateUserSecurityTokensTx(tx, userID, "", now); errTokens != nil {
		return errTokens
	}
	return supersedeUserMailJobsTx(tx, userID, "", now)
}

func invalidateUserPasswordSecurityTx(tx *gorm.DB, userID uint, now time.Time) error {
	if errTokens := invalidateUserSecurityTokensTx(tx, userID, UserSecurityTokenPurposePasswordReset, now); errTokens != nil {
		return errTokens
	}
	return supersedeUserMailJobsTx(tx, userID, UserSecurityTokenPurposePasswordReset, now)
}

func supersedeUserMailJobsTx(tx *gorm.DB, userID uint, purpose string, now time.Time) error {
	query := tx.Model(&UserMailJobRecord{}).
		Where("user_id = ? AND status IN ?", userID, []string{UserMailJobStatusPending, UserMailJobStatusSending})
	if strings.TrimSpace(purpose) != "" {
		query = query.Where("purpose = ?", strings.TrimSpace(purpose))
	}
	return query.Updates(map[string]any{
		"status":           UserMailJobStatusSuperseded,
		"claimed_by":       "",
		"claim_expires_at": nil,
		"updated_at":       now.UTC(),
	}).Error
}

// EnqueueUserMailJob supersedes older pending jobs for the same purpose.
func (r *Repository) EnqueueUserMailJob(ctx context.Context, userID uint, purpose string, emailVersion uint64, now time.Time) (*UserMailJobRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	purpose = strings.TrimSpace(purpose)
	if userID == 0 || purpose == "" {
		return nil, fmt.Errorf("user mail job fields are required")
	}
	if purpose != UserSecurityTokenPurposeEmailVerification && purpose != UserSecurityTokenPurposePasswordReset {
		return nil, fmt.Errorf("user mail job purpose is invalid")
	}
	now = now.UTC()
	record := &UserMailJobRecord{
		UserID:        userID,
		Purpose:       purpose,
		EmailVersion:  emailVersion,
		Status:        UserMailJobStatusPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		user := UserRecord{}
		if errUser := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", userID).First(&user).Error; errUser != nil {
			return errUser
		}
		if user.Email == nil || strings.TrimSpace(*user.Email) == "" || user.EmailVersion != emailVersion {
			return fmt.Errorf("user email state changed before mail enqueue")
		}
		if purpose == UserSecurityTokenPurposePasswordReset && user.EmailVerifiedAt == nil {
			return fmt.Errorf("verified user email is required for password reset mail")
		}
		record.SessionVersion = user.SessionVersion
		if errSupersede := tx.Model(&UserMailJobRecord{}).
			Where("user_id = ? AND purpose = ? AND status IN ?", userID, purpose, []string{UserMailJobStatusPending, UserMailJobStatusSending}).
			Updates(map[string]any{
				"status":           UserMailJobStatusSuperseded,
				"claimed_by":       "",
				"claim_expires_at": nil,
				"updated_at":       now,
			}).Error; errSupersede != nil {
			return errSupersede
		}
		if errInvalidate := invalidateUserSecurityTokensTx(tx, userID, purpose, now); errInvalidate != nil {
			return errInvalidate
		}
		return tx.Create(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// RenewUserMailJobClaim extends an active worker lease before network delivery.
func (r *Repository) RenewUserMailJobClaim(ctx context.Context, jobID uint, workerID string, now time.Time, lease time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	workerID = strings.TrimSpace(workerID)
	if jobID == 0 || workerID == "" {
		return fmt.Errorf("user mail job claim is required")
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	now = now.UTC()
	result := db.WithContext(contextOrBackground(ctx)).Model(&UserMailJobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ? AND claim_expires_at > ?", jobID, UserMailJobStatusSending, workerID, now).
		Updates(map[string]any{
			"claim_expires_at": now.Add(lease),
			"updated_at":       now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrUserMailJobClaimLost
	}
	return nil
}

// ClaimUserMailJob atomically leases one ready job to a worker.
func (r *Repository) ClaimUserMailJob(ctx context.Context, workerID string, now time.Time, lease time.Duration) (*UserMailJobRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	now = now.UTC()
	claimExpiresAt := now.Add(lease)

	for attempt := 0; attempt < 3; attempt++ {
		candidate := UserMailJobRecord{}
		errFind := db.WithContext(contextOrBackground(ctx)).
			Where("(status = ? AND next_attempt_at <= ?) OR (status = ? AND claim_expires_at <= ?)", UserMailJobStatusPending, now, UserMailJobStatusSending, now).
			Order("id ASC").
			First(&candidate).Error
		if errors.Is(errFind, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if errFind != nil {
			return nil, errFind
		}

		result := db.WithContext(contextOrBackground(ctx)).Model(&UserMailJobRecord{}).
			Where("id = ? AND ((status = ? AND next_attempt_at <= ?) OR (status = ? AND claim_expires_at <= ?))", candidate.ID, UserMailJobStatusPending, now, UserMailJobStatusSending, now).
			Updates(map[string]any{
				"status":           UserMailJobStatusSending,
				"claimed_by":       workerID,
				"claim_expires_at": claimExpiresAt,
				"attempts":         gorm.Expr("attempts + 1"),
				"updated_at":       now,
			})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		claimed := UserMailJobRecord{}
		if errLoad := db.WithContext(contextOrBackground(ctx)).Where("id = ?", candidate.ID).First(&claimed).Error; errLoad != nil {
			return nil, errLoad
		}
		return &claimed, nil
	}
	return nil, nil
}

// CompleteUserMailJob marks a claimed job as sent.
func (r *Repository) CompleteUserMailJob(ctx context.Context, jobID uint, workerID string, now time.Time) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	now = now.UTC()
	result := db.WithContext(contextOrBackground(ctx)).Model(&UserMailJobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", jobID, UserMailJobStatusSending, strings.TrimSpace(workerID)).
		Updates(map[string]any{
			"status":           UserMailJobStatusSent,
			"sent_at":          now,
			"claimed_by":       "",
			"claim_expires_at": nil,
			"last_error_code":  "",
			"updated_at":       now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrUserMailJobClaimLost
	}
	return nil
}

// FailUserMailJob releases or terminates a failed claimed job.
func (r *Repository) FailUserMailJob(ctx context.Context, job *UserMailJobRecord, workerID string, errorCode string, now time.Time, retryAfter time.Duration, maxAttempts int) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if job == nil || job.ID == 0 {
		return fmt.Errorf("user mail job is required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if retryAfter <= 0 {
		retryAfter = time.Minute
	}
	now = now.UTC()
	status := UserMailJobStatusPending
	nextAttemptAt := now.Add(retryAfter)
	if job.Attempts >= maxAttempts {
		status = UserMailJobStatusFailed
		nextAttemptAt = now
	}
	result := db.WithContext(contextOrBackground(ctx)).Model(&UserMailJobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", job.ID, UserMailJobStatusSending, strings.TrimSpace(workerID)).
		Updates(map[string]any{
			"status":           status,
			"next_attempt_at":  nextAttemptAt,
			"claimed_by":       "",
			"claim_expires_at": nil,
			"last_error_code":  strings.TrimSpace(errorCode),
			"updated_at":       now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrUserMailJobClaimLost
	}
	return nil
}

// SupersedeUserMailJob marks a claimed job obsolete.
func (r *Repository) SupersedeUserMailJob(ctx context.Context, jobID uint, workerID string, now time.Time) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	now = now.UTC()
	result := db.WithContext(contextOrBackground(ctx)).Model(&UserMailJobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", jobID, UserMailJobStatusSending, strings.TrimSpace(workerID)).
		Updates(map[string]any{
			"status":           UserMailJobStatusSuperseded,
			"claimed_by":       "",
			"claim_expires_at": nil,
			"updated_at":       now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrUserMailJobClaimLost
	}
	return nil
}

// AllowUserSecurityActionWithRetry increments an anchored-window counter and
// reports how long a denied caller must wait before the current window resets.
func (r *Repository) AllowUserSecurityActionWithRetry(ctx context.Context, action string, scopeHash string, limit int, window time.Duration, now time.Time) (bool, time.Duration, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, 0, errDB
	}
	action = strings.TrimSpace(action)
	scopeHash = strings.TrimSpace(scopeHash)
	if action == "" || scopeHash == "" || limit <= 0 || window <= 0 {
		return false, 0, fmt.Errorf("user security throttle fields are required")
	}
	now = now.UTC()
	key := fmt.Sprintf("%s:%s", action, scopeHash)
	record := UserSecurityThrottleRecord{
		Key:       key,
		Count:     1,
		ExpiresAt: now.Add(window),
		UpdatedAt: now,
	}
	// Cleanup can delete an expired conflicting row between the insert and the
	// locked reload. Retry that narrow race once so callers do not see a 500.
	for attempt := 0; attempt < 2; attempt++ {
		allowed := false
		retryAfter := time.Duration(0)
		errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
			result := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoNothing: true,
			}).Create(&record)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				allowed = true
				return nil
			}

			stored := UserSecurityThrottleRecord{}
			if errLoad := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).First(&stored).Error; errLoad != nil {
				return errLoad
			}
			if !stored.ExpiresAt.After(now) {
				allowed = true
				return tx.Model(&UserSecurityThrottleRecord{}).Where("key = ?", key).Updates(map[string]any{
					"count":      1,
					"expires_at": now.Add(window),
					"updated_at": now,
				}).Error
			}

			nextCount := stored.Count + 1
			allowed = nextCount <= limit
			if !allowed {
				retryAfter = stored.ExpiresAt.Sub(now)
			}
			return tx.Model(&UserSecurityThrottleRecord{}).Where("key = ?", key).Updates(map[string]any{
				"count":      nextCount,
				"updated_at": now,
			}).Error
		})
		if errTransaction == nil {
			return allowed, retryAfter, nil
		}
		if !errors.Is(errTransaction, gorm.ErrRecordNotFound) || attempt == 1 {
			return false, 0, errTransaction
		}
	}
	return false, 0, gorm.ErrRecordNotFound
}

// PurgeExpiredUserSecurity removes old terminal security records.
func (r *Repository) PurgeExpiredUserSecurity(ctx context.Context, now time.Time, retention time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	now = now.UTC()
	cutoff := now.Add(-retention)
	db = db.WithContext(contextOrBackground(ctx))
	if errDelete := db.Where("expires_at < ? OR (used_at IS NOT NULL AND used_at < ?)", cutoff, cutoff).Delete(&UserSecurityTokenRecord{}).Error; errDelete != nil {
		return errDelete
	}
	if errDelete := db.Where("status IN ? AND updated_at < ?", []string{UserMailJobStatusSent, UserMailJobStatusFailed, UserMailJobStatusSuperseded}, cutoff).Delete(&UserMailJobRecord{}).Error; errDelete != nil {
		return errDelete
	}
	if errDelete := db.Where(
		"(status = ? AND created_at < ?) OR (status = ? AND created_at < ? AND (claim_expires_at IS NULL OR claim_expires_at <= ?))",
		UserMailJobStatusPending,
		cutoff,
		UserMailJobStatusSending,
		cutoff,
		now,
	).Delete(&UserMailJobRecord{}).Error; errDelete != nil {
		return errDelete
	}
	return db.Where("expires_at < ?", now).Delete(&UserSecurityThrottleRecord{}).Error
}
