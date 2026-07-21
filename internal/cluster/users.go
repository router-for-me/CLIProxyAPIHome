package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrUserNotFound indicates that the referenced user record does not exist.
var ErrUserNotFound = errors.New("user not found")

// IsUserConflictError reports whether an error is a username uniqueness conflict.
func IsUserConflictError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "idx_user_username_active_unique") {
		return true
	}
	if strings.Contains(message, "unique constraint") && strings.Contains(message, "user") && strings.Contains(message, "username") {
		return true
	}
	return strings.Contains(message, "duplicate key") && strings.Contains(message, "username")
}

// IsUserEmailConflictError reports whether an error is a verified active-email ownership conflict.
func IsUserEmailConflictError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "idx_user_email_verified_active_unique") || strings.Contains(message, "idx_user_email_active_unique") {
		return true
	}
	if strings.Contains(message, "unique constraint") && strings.Contains(message, "user") && strings.Contains(message, "email") {
		return true
	}
	return strings.Contains(message, "duplicate key") && strings.Contains(message, "email")
}

type UserUpdate struct {
	Username         *string
	Password         *string
	Email            *string
	Credits          *float64
	CreditsUnlimited *bool
	MFA              *JSONB
	Passkey          *JSONB

	Timezone        *string
	Limit5hCredits  OptionalFloatUpdate
	WindowMode5h    *string
	Limit1dCredits  OptionalFloatUpdate
	WindowMode1d    *string
	Limit7dCredits  OptionalFloatUpdate
	WindowMode7d    *string
	WeekResetDay    *int
	WeekResetHour   *int
	Limit30dCredits OptionalFloatUpdate
	WindowMode30d   *string
}

// ListUsers returns users.
func (r *Repository) ListUsers(ctx context.Context) ([]UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []UserRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// GetUser returns a user by ID.
func (r *Repository) GetUser(ctx context.Context, id uint) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("user id is required")
	}

	record := &UserRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// GetUserByUsername returns a user by username.
func (r *Repository) GetUserByUsername(ctx context.Context, username string) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("username is required")
	}

	record := &UserRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("username = ?", username).Order("id").First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// GetUserByEmail returns the active user that owns a verified normalized email.
func (r *Repository) GetUserByEmail(ctx context.Context, email string) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}

	record := &UserRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("email = ? AND email_verified_at IS NOT NULL", email).Order("id").First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// CreateUser creates a user.
func (r *Repository) CreateUser(ctx context.Context, update UserUpdate) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if update.Username == nil {
		return nil, fmt.Errorf("username is required")
	}
	username := strings.TrimSpace(*update.Username)
	if username == "" {
		return nil, fmt.Errorf("username is required")
	}

	record := &UserRecord{
		Username: username,
	}
	defaultUserPeriodLimitFields(record)
	if update.Password != nil {
		record.Password = *update.Password
	}
	if update.Email != nil {
		email := strings.TrimSpace(*update.Email)
		if email != "" {
			record.Email = &email
			record.EmailVersion = 1
		}
	}
	if update.Credits != nil {
		record.Credits = *update.Credits
	}
	if update.CreditsUnlimited != nil {
		record.CreditsUnlimited = *update.CreditsUnlimited
	}
	if errApply := applyUserPeriodLimitUpdate(record, update); errApply != nil {
		return nil, errApply
	}
	if update.MFA != nil {
		record.MFA = cloneJSONB(*update.MFA)
	}
	if update.Passkey != nil {
		record.Passkey = cloneJSONB(*update.Passkey)
	}
	if errCreate := db.WithContext(contextOrBackground(ctx)).Create(record).Error; errCreate != nil {
		return nil, errCreate
	}
	return record, nil
}

// UpdateUser updates a user.
func (r *Repository) UpdateUser(ctx context.Context, id uint, update UserUpdate) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("user id is required")
	}

	record := &UserRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(record).Error; errFirst != nil {
			return errFirst
		}
		if update.Username != nil {
			username := strings.TrimSpace(*update.Username)
			if username == "" {
				return fmt.Errorf("username is required")
			}
			record.Username = username
		}
		if update.Password != nil {
			record.Password = *update.Password
			record.SessionVersion++
			if errInvalidate := invalidateUserPasswordSecurityTx(tx, record.ID, time.Now().UTC()); errInvalidate != nil {
				return errInvalidate
			}
		}
		if update.Email != nil {
			email := strings.TrimSpace(*update.Email)
			currentEmail := ""
			if record.Email != nil {
				currentEmail = strings.TrimSpace(*record.Email)
			}
			if email != currentEmail {
				if email == "" {
					record.Email = nil
				} else {
					record.Email = &email
				}
				record.EmailVerifiedAt = nil
				record.EmailVersion++
				if errInvalidate := invalidateUserEmailSecurityTx(tx, record.ID, time.Now().UTC()); errInvalidate != nil {
					return errInvalidate
				}
			}
		}
		if update.Credits != nil {
			record.Credits = *update.Credits
		}
		if update.CreditsUnlimited != nil {
			record.CreditsUnlimited = *update.CreditsUnlimited
		}
		if errApply := applyUserPeriodLimitUpdate(record, update); errApply != nil {
			return errApply
		}
		if update.MFA != nil {
			record.MFA = cloneJSONB(*update.MFA)
		}
		if update.Passkey != nil {
			record.Passkey = cloneJSONB(*update.Passkey)
		}
		return tx.Save(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// DeleteUser deletes a user.
func (r *Repository) DeleteUser(ctx context.Context, id uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if id == 0 {
		return fmt.Errorf("user id is required")
	}

	record := UserRecord{}
	ctx = contextOrBackground(ctx)
	if errFirst := db.WithContext(ctx).Where("id = ?", id).First(&record).Error; errFirst != nil {
		return errFirst
	}
	return db.WithContext(ctx).Delete(&record).Error
}

// ParseUserRecordID parses a positive unsigned user record ID.
func ParseUserRecordID(value string) (uint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("user id is required")
	}
	parsed, errParse := strconv.ParseUint(value, 10, 64)
	if errParse != nil {
		return 0, errParse
	}
	if parsed == 0 {
		return 0, fmt.Errorf("user id is required")
	}
	return uint(parsed), nil
}

func normalizeOptionalUserID(userID *uint) *uint {
	if userID == nil || *userID == 0 {
		return nil
	}
	value := *userID
	return &value
}

func ensureUserExists(ctx context.Context, tx *gorm.DB, id uint) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if id == 0 {
		return nil
	}
	record := UserRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(&record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w: %d", ErrUserNotFound, id)
	}
	return errFirst
}

func cloneJSONB(value JSONB) JSONB {
	if len(value) == 0 {
		return nil
	}
	return append(JSONB(nil), value...)
}

// NormalizeJSONB validates and normalizes raw JSON for JSONB fields.
func NormalizeJSONB(raw json.RawMessage) (*JSONB, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid json value")
	}
	if string(raw) == "null" {
		empty := JSONB(nil)
		return &empty, nil
	}
	value := JSONB(append([]byte(nil), raw...))
	return &value, nil
}

func applyUserPeriodLimitUpdate(record *UserRecord, update UserUpdate) error {
	if record == nil {
		return fmt.Errorf("user record is nil")
	}
	defaultUserPeriodLimitFields(record)
	if update.Timezone != nil {
		timezone, errTZ := normalizeUserTimezone(*update.Timezone)
		if errTZ != nil {
			return periodLimitFieldError("timezone", PeriodLimitErrInvalidTimezone, errTZ.Error())
		}
		record.Timezone = timezone
	}
	if errLimit := applyOptionalFloat(update.Limit5hCredits, &record.Limit5hCredits); errLimit != nil {
		return periodLimitFieldError("limit_5h_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_5h_credits: %s", errLimit.Error()))
	}
	if update.WindowMode5h != nil {
		mode, errMode := normalizePeriodWindowMode(*update.WindowMode5h, false)
		if errMode != nil {
			return periodLimitFieldError("window_mode_5h", PeriodLimitErrInvalidWindowMode, fmt.Sprintf("window_mode_5h: %s", errMode.Error()))
		}
		record.WindowMode5h = mode
	}
	if errLimit := applyOptionalFloat(update.Limit1dCredits, &record.Limit1dCredits); errLimit != nil {
		return periodLimitFieldError("limit_1d_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_1d_credits: %s", errLimit.Error()))
	}
	if update.WindowMode1d != nil {
		mode, errMode := normalizePeriodWindowMode(*update.WindowMode1d, true)
		if errMode != nil {
			return periodLimitFieldError("window_mode_1d", PeriodLimitErrInvalidWindowMode, fmt.Sprintf("window_mode_1d: %s", errMode.Error()))
		}
		record.WindowMode1d = mode
	}
	if errLimit := applyOptionalFloat(update.Limit7dCredits, &record.Limit7dCredits); errLimit != nil {
		return periodLimitFieldError("limit_7d_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_7d_credits: %s", errLimit.Error()))
	}
	if update.WindowMode7d != nil {
		mode, errMode := normalizePeriodWindowMode(*update.WindowMode7d, true)
		if errMode != nil {
			return periodLimitFieldError("window_mode_7d", PeriodLimitErrInvalidWindowMode, fmt.Sprintf("window_mode_7d: %s", errMode.Error()))
		}
		record.WindowMode7d = mode
	}
	if update.WeekResetDay != nil {
		day, errDay := normalizeWeekResetDay(*update.WeekResetDay)
		if errDay != nil {
			return periodLimitFieldError("week_reset_day", PeriodLimitErrInvalidWeekResetDay, errDay.Error())
		}
		record.WeekResetDay = day
	}
	if update.WeekResetHour != nil {
		hour, errHour := normalizeWeekResetHour(*update.WeekResetHour)
		if errHour != nil {
			return periodLimitFieldError("week_reset_hour", PeriodLimitErrInvalidWeekResetHour, errHour.Error())
		}
		record.WeekResetHour = hour
	}
	if errLimit := applyOptionalFloat(update.Limit30dCredits, &record.Limit30dCredits); errLimit != nil {
		return periodLimitFieldError("limit_30d_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_30d_credits: %s", errLimit.Error()))
	}
	if update.WindowMode30d != nil {
		mode, errMode := normalizePeriodWindowMode(*update.WindowMode30d, true)
		if errMode != nil {
			return periodLimitFieldError("window_mode_30d", PeriodLimitErrInvalidWindowMode, fmt.Sprintf("window_mode_30d: %s", errMode.Error()))
		}
		record.WindowMode30d = mode
	}
	return nil
}
