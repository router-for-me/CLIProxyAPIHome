package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

// ErrAPIKeyExists indicates that an API key value is already owned by another record.
var ErrAPIKeyExists = errors.New("api key already exists")

// ErrAPIKeySelectorMismatch indicates that multiple selectors do not identify the same API key.
var ErrAPIKeySelectorMismatch = errors.New("api key selector mismatch")

type APIKeyEntry struct {
	ID          uint
	APIKey      string
	DisplayName *string
	UserID      *uint
	Channels    []uint
	ModelGroups []uint
}

type APIKeyEntryUpdate struct {
	ID             uint
	APIKey         string
	DisplayName    *string
	DisplayNameSet bool
	UserID         *uint
	UserIDSet      bool
	Channels       *[]uint
	ModelGroups    *[]uint
}

type APIKeyUserUpdate struct {
	APIKey      *string
	Channels    *[]uint
	ModelGroups *[]uint
}

type APIKeySelector struct {
	ID     uint
	APIKey string
	Index  *int
}

type APIKeyAdminUpdate struct {
	APIKey         *string
	DisplayName    *string
	DisplayNameSet bool
	UserID         *uint
	Channels       *[]uint
	ModelGroups    *[]uint
}

const APIKeyDisplayNameMaxLength = 128

var ErrInvalidAPIKeyDisplayName = errors.New("API key display_name is invalid")

// ErrAPIKeyPreconditionFailed indicates that a conditional full-list replacement used a stale ETag.
var ErrAPIKeyPreconditionFailed = errors.New("API key collection precondition failed")

const apiKeyMutationAdvisoryLockKey int64 = 749327842680272318

// IsAPIKeyConflictError reports whether an error is an API key uniqueness conflict.
func IsAPIKeyConflictError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAPIKeyExists) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "duplicate key")
}

// ListAPIKeyEntries returns API key rows with group bindings.
func (r *Repository) ListAPIKeyEntries(ctx context.Context) ([]APIKeyEntry, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	return listAPIKeyEntriesTx(ctx, db)
}

// ListAPIKeyEntriesWithETag returns one coherent collection representation and
// a strong ETag that can guard a later full-list replacement.
func (r *Repository) ListAPIKeyEntriesWithETag(ctx context.Context) ([]APIKeyEntry, string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, "", errDB
	}

	ctx = contextOrBackground(ctx)
	var entries []APIKeyEntry
	var etag string
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyReadTransaction(tx); errLock != nil {
			return errLock
		}
		var errEntries error
		entries, errEntries = listAPIKeyEntriesTx(ctx, tx)
		if errEntries != nil {
			return errEntries
		}
		var errETag error
		etag, errETag = apiKeyEntriesETag(entries)
		return errETag
	})
	return entries, etag, errTransaction
}

// ReplaceAPIKeyEntries replaces active API key rows and updates explicit channel bindings.
func (r *Repository) ReplaceAPIKeyEntries(ctx context.Context, entries []APIKeyEntryUpdate) (APIKeyUpsertStats, error) {
	return r.ReplaceAPIKeyEntriesIfMatch(ctx, entries, nil)
}

// ReplaceAPIKeyEntriesIfMatch replaces active API key rows after optionally
// verifying the collection ETag inside the mutation transaction.
func (r *Repository) ReplaceAPIKeyEntriesIfMatch(ctx context.Context, entries []APIKeyEntryUpdate, ifMatch *string) (APIKeyUpsertStats, error) {
	db, errDB := r.database()
	if errDB != nil {
		return APIKeyUpsertStats{}, errDB
	}

	ctx = contextOrBackground(ctx)
	var stats APIKeyUpsertStats
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if ifMatch != nil {
			currentEntries, errEntries := listAPIKeyEntriesTx(ctx, tx)
			if errEntries != nil {
				return errEntries
			}
			currentETag, errETag := apiKeyEntriesETag(currentEntries)
			if errETag != nil {
				return errETag
			}
			if !apiKeyETagMatches(*ifMatch, currentETag) {
				return ErrAPIKeyPreconditionFailed
			}
		}
		var errReplace error
		stats, errReplace = replaceAPIKeyEntriesTxWithStats(ctx, tx, entries)
		if errReplace != nil {
			return errReplace
		}
		deleteResult := tx.Delete(&ConfigRecord{}, "key = ?", configAPIKeysRootKey)
		if deleteResult.Error != nil {
			return deleteResult.Error
		}
		if deleteResult.RowsAffected > 0 {
			stats.RuntimeChanged = true
		}
		if !stats.RequiresRuntimeRefresh() && deleteResult.RowsAffected == 0 {
			return nil
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	return stats, errTransaction
}

// CreateAPIKey creates or restores one API key without replacing the full key list.
func (r *Repository) CreateAPIKey(ctx context.Context, update APIKeyEntryUpdate) (*APIKeyRecord, error) {
	record, _, errCreate := r.CreateAPIKeyWithRuntimeChange(ctx, update)
	return record, errCreate
}

// CreateAPIKeyWithRuntimeChange creates or restores one API key and reports
// whether the accepted runtime key set changed.
func (r *Repository) CreateAPIKeyWithRuntimeChange(ctx context.Context, update APIKeyEntryUpdate) (*APIKeyRecord, bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, false, errDB
	}
	key := strings.TrimSpace(update.APIKey)
	if key == "" {
		return nil, false, fmt.Errorf("api key is required")
	}

	userID := normalizeOptionalUserID(update.UserID)
	var displayName *string
	if update.DisplayNameSet {
		var errDisplayName error
		displayName, errDisplayName = normalizeAPIKeyDisplayName(update.DisplayName)
		if errDisplayName != nil {
			return nil, false, errDisplayName
		}
	}
	channelsJSON := emptyAPIKeyChannelsJSON()
	if update.Channels != nil {
		var errChannels error
		channelsJSON, errChannels = apiKeyChannelsJSON(*update.Channels)
		if errChannels != nil {
			return nil, false, errChannels
		}
	}
	modelGroupsJSON := emptyAPIKeyModelGroupsJSON()
	if update.ModelGroups != nil {
		var errModelGroups error
		modelGroupsJSON, errModelGroups = apiKeyModelGroupsJSON(*update.ModelGroups)
		if errModelGroups != nil {
			return nil, false, errModelGroups
		}
	}

	record := &APIKeyRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if userID != nil {
			if errUser := ensureUserExists(ctx, tx, *userID); errUser != nil {
				return errUser
			}
		}

		existing := &APIKeyRecord{}
		errFirst := tx.WithContext(ctx).Unscoped().Where("api_key = ?", key).First(existing).Error
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			record.APIKey = key
			record.DisplayName = displayName
			record.UserID = userID
			record.Channels = channelsJSON
			record.ModelGroups = modelGroupsJSON
			if errCreate := tx.WithContext(ctx).Create(record).Error; errCreate != nil {
				if IsAPIKeyConflictError(errCreate) {
					return ErrAPIKeyExists
				}
				return errCreate
			}
		case errFirst != nil:
			return errFirst
		case existing.DeletedAt.Valid:
			updates := map[string]any{
				"user_id":      userID,
				"channels":     channelsJSON,
				"model_groups": modelGroupsJSON,
				"deleted_at":   nil,
			}
			if update.DisplayNameSet {
				updates["display_name"] = displayName
			}
			if errRestore := tx.WithContext(ctx).Unscoped().
				Model(&APIKeyRecord{}).
				Where("id = ?", existing.ID).
				Updates(updates).Error; errRestore != nil {
				return errRestore
			}
			if errReload := tx.WithContext(ctx).Where("id = ?", existing.ID).First(record).Error; errReload != nil {
				return errReload
			}
		default:
			return ErrAPIKeyExists
		}

		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, false, errTransaction
	}
	return record, true, nil
}

// UpdateAPIKey updates one API key selected by stable ID or a legacy selector.
func (r *Repository) UpdateAPIKey(ctx context.Context, selector APIKeySelector, update APIKeyAdminUpdate) (*APIKeyRecord, error) {
	record, _, errUpdate := r.UpdateAPIKeyWithRuntimeChange(ctx, selector, update)
	return record, errUpdate
}

// UpdateAPIKeyWithRuntimeChange updates one API key and reports whether the
// accepted runtime key set or dispatch-affecting bindings changed.
func (r *Repository) UpdateAPIKeyWithRuntimeChange(ctx context.Context, selector APIKeySelector, update APIKeyAdminUpdate) (*APIKeyRecord, bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, false, errDB
	}
	if errSelector := validateAPIKeySelector(selector); errSelector != nil {
		return nil, false, errSelector
	}

	record := &APIKeyRecord{}
	runtimeChanged := false
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if errFind := findAPIKeyRecord(ctx, tx, selector, record); errFind != nil {
			return errFind
		}
		updates := make(map[string]any)
		if update.APIKey != nil {
			nextKey := strings.TrimSpace(*update.APIKey)
			if nextKey == "" {
				return fmt.Errorf("api key is required")
			}
			if nextKey != strings.TrimSpace(record.APIKey) {
				if errAvailable := ensureAPIKeyValueAvailable(ctx, tx, nextKey, record.ID); errAvailable != nil {
					return errAvailable
				}
			}
			if nextKey != strings.TrimSpace(record.APIKey) {
				updates["api_key"] = nextKey
				runtimeChanged = true
			}
		}
		if update.DisplayNameSet {
			nextDisplayName, errDisplayName := normalizeAPIKeyDisplayName(update.DisplayName)
			if errDisplayName != nil {
				return errDisplayName
			}
			if !sameOptionalString(record.DisplayName, nextDisplayName) {
				updates["display_name"] = nextDisplayName
			}
		}
		if update.UserID != nil {
			nextUserID := normalizeOptionalUserID(update.UserID)
			if nextUserID != nil {
				if errUser := ensureUserExists(ctx, tx, *nextUserID); errUser != nil {
					return errUser
				}
			}
			if !sameOptionalUint(record.UserID, nextUserID) {
				updates["user_id"] = nextUserID
				runtimeChanged = true
			}
		}
		if update.Channels != nil {
			channelsJSON, errChannels := apiKeyChannelsJSON(*update.Channels)
			if errChannels != nil {
				return errChannels
			}
			currentChannels, errCurrent := apiKeyChannelsFromJSON(record.Channels)
			if errCurrent != nil {
				return errCurrent
			}
			if !reflect.DeepEqual(currentChannels, normalizeChannelGroupIDs(*update.Channels)) {
				updates["channels"] = channelsJSON
				runtimeChanged = true
			}
		}
		if update.ModelGroups != nil {
			modelGroupsJSON, errModelGroups := apiKeyModelGroupsJSON(*update.ModelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			currentModelGroups, errCurrent := apiKeyModelGroupsFromJSON(record.ModelGroups)
			if errCurrent != nil {
				return errCurrent
			}
			if !reflect.DeepEqual(currentModelGroups, normalizeModelGroupIDs(*update.ModelGroups)) {
				updates["model_groups"] = modelGroupsJSON
				runtimeChanged = true
			}
		}
		if len(updates) == 0 {
			return nil
		}
		if errUpdate := tx.WithContext(ctx).Model(&APIKeyRecord{}).Where("id = ?", record.ID).Updates(updates).Error; errUpdate != nil {
			if IsAPIKeyConflictError(errUpdate) {
				return ErrAPIKeyExists
			}
			return errUpdate
		}
		if errReload := tx.WithContext(ctx).Where("id = ?", record.ID).First(record).Error; errReload != nil {
			return errReload
		}
		if !runtimeChanged {
			return nil
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, false, errTransaction
	}
	return record, runtimeChanged, nil
}

// DeleteAPIKey deletes one API key selected by stable ID or a legacy selector.
func (r *Repository) DeleteAPIKey(ctx context.Context, selector APIKeySelector) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if errSelector := validateAPIKeySelector(selector); errSelector != nil {
		return errSelector
	}

	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		record := &APIKeyRecord{}
		if errFind := findAPIKeyRecord(ctx, tx, selector, record); errFind != nil {
			return errFind
		}
		if errDelete := tx.WithContext(ctx).Delete(record).Error; errDelete != nil {
			return errDelete
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
}

func validateAPIKeySelector(selector APIKeySelector) error {
	if selector.ID > 0 && selector.Index != nil {
		return ErrAPIKeySelectorMismatch
	}
	if selector.Index != nil && *selector.Index < 0 {
		return fmt.Errorf("invalid api key index")
	}
	if selector.ID == 0 && selector.Index == nil && strings.TrimSpace(selector.APIKey) == "" {
		return fmt.Errorf("api key selector is required")
	}
	return nil
}

func findAPIKeyRecord(ctx context.Context, tx *gorm.DB, selector APIKeySelector, record *APIKeyRecord) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if record == nil {
		return fmt.Errorf("api key record is nil")
	}

	query := tx.WithContext(contextOrBackground(ctx))
	switch {
	case selector.ID > 0:
		query = query.Where("id = ?", selector.ID)
	case selector.Index != nil:
		query = query.Order("id").Offset(*selector.Index)
	default:
		query = query.Where("api_key = ?", strings.TrimSpace(selector.APIKey))
	}
	if errFirst := query.First(record).Error; errFirst != nil {
		return errFirst
	}
	if selector.ID > 0 && strings.TrimSpace(selector.APIKey) != "" && strings.TrimSpace(record.APIKey) != strings.TrimSpace(selector.APIKey) {
		return ErrAPIKeySelectorMismatch
	}
	return nil
}

// UpdateAPIKeyBindings updates user and group bindings for one API key.
func (r *Repository) UpdateAPIKeyBindings(ctx context.Context, apiKey string, userID *uint, channels *[]uint, modelGroups *[]uint) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("api key is required")
	}

	record := &APIKeyRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if errFirst := tx.Where("api_key = ?", apiKey).First(record).Error; errFirst != nil {
			return errFirst
		}
		updates := make(map[string]any)
		if userID != nil {
			nextUserID := normalizeOptionalUserID(userID)
			if nextUserID != nil {
				if errUser := ensureUserExists(ctx, tx, *nextUserID); errUser != nil {
					return errUser
				}
			}
			updates["user_id"] = nextUserID
		}
		if channels != nil {
			channelsJSON, errChannels := apiKeyChannelsJSON(*channels)
			if errChannels != nil {
				return errChannels
			}
			updates["channels"] = channelsJSON
		}
		if modelGroups != nil {
			modelGroupsJSON, errModelGroups := apiKeyModelGroupsJSON(*modelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			updates["model_groups"] = modelGroupsJSON
		}
		if len(updates) > 0 {
			if errUpdate := tx.WithContext(ctx).Model(&APIKeyRecord{}).Where("id = ?", record.ID).Updates(updates).Error; errUpdate != nil {
				return errUpdate
			}
			if errReload := tx.WithContext(ctx).Where("id = ?", record.ID).First(record).Error; errReload != nil {
				return errReload
			}
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UpdateAPIKeyChannels updates channel bindings for one API key.
func (r *Repository) UpdateAPIKeyChannels(ctx context.Context, apiKey string, channels []uint) (*APIKeyRecord, error) {
	return r.UpdateAPIKeyBindings(ctx, apiKey, nil, &channels, nil)
}

// ListAPIKeyRecordsForUser returns active API key rows owned by one user.
func (r *Repository) ListAPIKeyRecordsForUser(ctx context.Context, userID uint) ([]APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}

	var records []APIKeyRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Where("user_id = ?", userID).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// CreateAPIKeyForUser creates an API key owned by one user.
func (r *Repository) CreateAPIKeyForUser(ctx context.Context, userID uint, update APIKeyUserUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}
	if update.APIKey == nil || strings.TrimSpace(*update.APIKey) == "" {
		return nil, fmt.Errorf("api key is required")
	}

	record := &APIKeyRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if errUser := ensureUserExists(ctx, tx, userID); errUser != nil {
			return errUser
		}
		channelsJSON := emptyAPIKeyChannelsJSON()
		if update.Channels != nil {
			nextChannels, errChannels := apiKeyChannelsJSON(*update.Channels)
			if errChannels != nil {
				return errChannels
			}
			channelsJSON = nextChannels
		}
		modelGroupsJSON := emptyAPIKeyModelGroupsJSON()
		if update.ModelGroups != nil {
			nextModelGroups, errModelGroups := apiKeyModelGroupsJSON(*update.ModelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			modelGroupsJSON = nextModelGroups
		}
		key := strings.TrimSpace(*update.APIKey)
		existing := &APIKeyRecord{}
		errFirst := tx.WithContext(ctx).Unscoped().Where("api_key = ?", key).First(existing).Error
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			record.APIKey = key
			record.UserID = &userID
			record.Channels = channelsJSON
			record.ModelGroups = modelGroupsJSON
			if errCreate := tx.WithContext(ctx).Create(record).Error; errCreate != nil {
				return errCreate
			}
		case errFirst != nil:
			return errFirst
		case existing.DeletedAt.Valid:
			if errRestore := tx.WithContext(ctx).Unscoped().
				Model(&APIKeyRecord{}).
				Where("id = ?", existing.ID).
				Updates(map[string]any{
					"user_id":      &userID,
					"channels":     channelsJSON,
					"model_groups": modelGroupsJSON,
					"deleted_at":   nil,
				}).Error; errRestore != nil {
				return errRestore
			}
			if errReload := tx.WithContext(ctx).Where("id = ?", existing.ID).First(record).Error; errReload != nil {
				return errReload
			}
		default:
			if !sameOptionalUint(existing.UserID, &userID) {
				return ErrAPIKeyExists
			}
			if errUpdate := tx.WithContext(ctx).Model(&APIKeyRecord{}).Where("id = ?", existing.ID).Updates(map[string]any{
				"channels":     channelsJSON,
				"model_groups": modelGroupsJSON,
			}).Error; errUpdate != nil {
				return errUpdate
			}
			if errReload := tx.WithContext(ctx).Where("id = ?", existing.ID).First(record).Error; errReload != nil {
				return errReload
			}
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UpdateAPIKeyForUser updates one API key owned by one user.
func (r *Repository) UpdateAPIKeyForUser(ctx context.Context, userID uint, id uint, apiKey string, update APIKeyUserUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}
	apiKey = strings.TrimSpace(apiKey)
	if id == 0 && apiKey == "" {
		return nil, fmt.Errorf("api key id or value is required")
	}

	record := &APIKeyRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		query := tx.WithContext(ctx).Where("user_id = ?", userID)
		if id > 0 {
			query = query.Where("id = ?", id)
		} else {
			query = query.Where("api_key = ?", apiKey)
		}
		if errFirst := query.First(record).Error; errFirst != nil {
			return errFirst
		}
		updates := make(map[string]any)
		if update.APIKey != nil {
			nextKey := strings.TrimSpace(*update.APIKey)
			if nextKey == "" {
				return fmt.Errorf("api key is required")
			}
			if nextKey != strings.TrimSpace(record.APIKey) {
				if errAvailable := ensureAPIKeyValueAvailable(ctx, tx, nextKey, record.ID); errAvailable != nil {
					return errAvailable
				}
			}
			if nextKey != strings.TrimSpace(record.APIKey) {
				updates["api_key"] = nextKey
			}
		}
		if update.Channels != nil {
			channelsJSON, errChannels := apiKeyChannelsJSON(*update.Channels)
			if errChannels != nil {
				return errChannels
			}
			updates["channels"] = channelsJSON
		}
		if update.ModelGroups != nil {
			modelGroupsJSON, errModelGroups := apiKeyModelGroupsJSON(*update.ModelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			updates["model_groups"] = modelGroupsJSON
		}
		if len(updates) > 0 {
			if errUpdate := tx.WithContext(ctx).Model(&APIKeyRecord{}).Where("id = ?", record.ID).Updates(updates).Error; errUpdate != nil {
				if IsAPIKeyConflictError(errUpdate) {
					return ErrAPIKeyExists
				}
				return errUpdate
			}
			if errReload := tx.WithContext(ctx).Where("id = ?", record.ID).First(record).Error; errReload != nil {
				return errReload
			}
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

func ensureAPIKeyValueAvailable(ctx context.Context, tx *gorm.DB, apiKey string, currentID uint) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return fmt.Errorf("api key is required")
	}
	record := APIKeyRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).
		Unscoped().
		Select("id").
		Where("api_key = ?", apiKey).
		First(&record).Error
	switch {
	case errors.Is(errFirst, gorm.ErrRecordNotFound):
		return nil
	case errFirst != nil:
		return errFirst
	case record.ID == currentID:
		return nil
	default:
		return ErrAPIKeyExists
	}
}

// DeleteAPIKeyForUser deletes one API key owned by one user.
func (r *Repository) DeleteAPIKeyForUser(ctx context.Context, userID uint, id uint, apiKey string) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if userID == 0 {
		return fmt.Errorf("user id is required")
	}
	apiKey = strings.TrimSpace(apiKey)
	if id == 0 && apiKey == "" {
		return fmt.Errorf("api key id or value is required")
	}

	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		query := tx.WithContext(ctx).Where("user_id = ?", userID)
		if id > 0 {
			query = query.Where("id = ?", id)
		} else {
			query = query.Where("api_key = ?", apiKey)
		}
		record := &APIKeyRecord{}
		if errFirst := query.First(record).Error; errFirst != nil {
			return errFirst
		}
		if errDelete := tx.Delete(record).Error; errDelete != nil {
			return errDelete
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
}

// ValidateAPIKey reports whether an API key exists as an active record.
func (r *Repository) ValidateAPIKey(ctx context.Context, apiKey string) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false, nil
	}

	var record APIKeyRecord
	errFirst := db.WithContext(contextOrBackground(ctx)).
		Select("id").
		Where("api_key = ?", apiKey).
		First(&record).Error
	switch {
	case errors.Is(errFirst, gorm.ErrRecordNotFound):
		return false, nil
	case errFirst != nil:
		return false, errFirst
	default:
		return true, nil
	}
}

// AllowedDispatchIDsForAPIKey returns auth and model IDs allowed by API-key bindings.
func (r *Repository) AllowedDispatchIDsForAPIKey(ctx context.Context, apiKey string) ([]string, []string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, nil, nil
	}

	record := APIKeyRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("api_key = ?", apiKey).First(&record).Error
	if errFirst != nil {
		return nil, nil, errFirst
	}
	errBilling := ensureAPIKeyUserBillingAllowed(ctx, db, &record)
	if errBilling != nil {
		return nil, nil, errBilling
	}

	authIDs, errAuthIDs := allowedAuthIDsForAPIKeyRecord(ctx, db, &record)
	if errAuthIDs != nil {
		return nil, nil, errAuthIDs
	}
	modelIDs, errModelIDs := allowedModelIDsForAPIKeyRecord(ctx, db, &record)
	if errModelIDs != nil {
		return nil, nil, errModelIDs
	}
	return authIDs, modelIDs, nil
}

func ensureAPIKeyUserCreditsAvailable(ctx context.Context, db *gorm.DB, record *APIKeyRecord) error {
	return ensureAPIKeyUserBillingAllowed(ctx, db, record)
}

// AllowedDispatchIDsForAPIKeyModel returns auth and model IDs after applying model-specific channel bindings.
func (r *Repository) AllowedDispatchIDsForAPIKeyModel(ctx context.Context, apiKey string, modelID string) ([]string, []string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	modelID = strings.TrimSpace(modelID)
	if apiKey == "" {
		return nil, nil, nil
	}

	record := APIKeyRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("api_key = ?", apiKey).First(&record).Error; errFirst != nil {
		return nil, nil, errFirst
	}
	if errBilling := ensureAPIKeyUserBillingAllowed(ctx, db, &record); errBilling != nil {
		return nil, nil, errBilling
	}

	baseChannelIDs, errBaseChannels := apiKeyChannelsFromJSON(record.Channels)
	if errBaseChannels != nil {
		return nil, nil, errBaseChannels
	}
	modelDetails, modelGroupsRestricted, errModelDetails := allowedModelGroupDetailsForAPIKeyRecord(ctx, db, &record)
	if errModelDetails != nil {
		return nil, nil, errModelDetails
	}
	modelIDs := allowedModelIDsFromDetails(modelDetails, modelGroupsRestricted)
	modelChannelIDs, modelChannelsRestricted, errModelChannels := modelChannelGroupIDsFromDetails(modelDetails, modelID)
	if errModelChannels != nil {
		return nil, nil, errModelChannels
	}

	allChannelIDs := append([]uint(nil), baseChannelIDs...)
	if modelChannelsRestricted {
		allChannelIDs = append(allChannelIDs, modelChannelIDs...)
	}
	channelDetails, errChannelDetails := channelGroupDetailsForGroups(ctx, db, allChannelIDs)
	if errChannelDetails != nil {
		return nil, nil, errChannelDetails
	}

	var authIDs []string
	if len(baseChannelIDs) > 0 {
		authIDs = allowedAuthIDsFromChannelGroupDetails(channelDetails, baseChannelIDs)
	}
	if !modelChannelsRestricted {
		return authIDs, modelIDs, nil
	}
	modelAuthIDs := allowedAuthIDsFromChannelGroupDetails(channelDetails, modelChannelIDs)
	return intersectAllowedAuthIDs(authIDs, modelAuthIDs), modelIDs, nil
}

func modelChannelGroupIDsFromDetails(details []ModelGroupDetailRecord, modelID string) ([]uint, bool, error) {
	modelKey := strings.ToLower(canonicalModelGroupModelID(modelID))
	if modelKey == "" {
		return nil, false, nil
	}

	channelIDs := make([]uint, 0)
	restricted := false
	for i := range details {
		if strings.ToLower(canonicalModelGroupModelID(details[i].ModelID)) != modelKey {
			continue
		}
		detailChannels, errDetailChannels := ModelGroupDetailChannelIDs(&details[i])
		if errDetailChannels != nil {
			return nil, false, errDetailChannels
		}
		if len(detailChannels) == 0 {
			continue
		}
		restricted = true
		channelIDs = append(channelIDs, detailChannels...)
	}
	return normalizeChannelGroupIDs(channelIDs), restricted, nil
}

func intersectAllowedAuthIDs(base []string, modelSpecific []string) []string {
	if base == nil {
		allowed := make([]string, len(modelSpecific))
		copy(allowed, modelSpecific)
		return allowed
	}
	modelSet := make(map[string]struct{}, len(modelSpecific))
	for _, authID := range modelSpecific {
		authID = strings.TrimSpace(authID)
		if authID != "" {
			modelSet[authID] = struct{}{}
		}
	}
	allowed := make([]string, 0, len(base))
	seen := make(map[string]struct{}, len(base))
	for _, authID := range base {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if _, ok := modelSet[authID]; !ok {
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		allowed = append(allowed, authID)
	}
	return allowed
}

// AllowedAuthIDsForAPIKey returns auth IDs allowed by the API key channel bindings.
func (r *Repository) AllowedAuthIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, nil
	}

	record := APIKeyRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("api_key = ?", apiKey).First(&record).Error
	if errFirst != nil {
		return nil, errFirst
	}

	return allowedAuthIDsForAPIKeyRecord(ctx, db, &record)
}

// AllowedModelIDsForAPIKey returns model IDs allowed by the API key model group bindings.
func (r *Repository) AllowedModelIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, nil
	}

	record := APIKeyRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("api_key = ?", apiKey).First(&record).Error
	if errFirst != nil {
		return nil, errFirst
	}

	return allowedModelIDsForAPIKeyRecord(ctx, db, &record)
}

// UserModelAccess summarizes which models one user's API keys may call.
type UserModelAccess struct {
	// APIKeyCount is how many active keys the user holds. Zero means the user
	// cannot call anything yet, which is a different situation from holding a
	// key that happens to allow nothing.
	APIKeyCount int
	// Restricted reports whether every key is scoped to model groups. One
	// unscoped key is enough to reach everything the cluster serves, so a false
	// value here makes ModelIDs meaningless.
	Restricted bool
	// ModelIDs is the union of canonical model identifiers the scoped keys
	// allow, sorted for stable output. It is only meaningful when Restricted.
	ModelIDs []string
}

// UserModelAccess resolves what the user's own API keys are permitted to call.
//
// Access is the union across keys rather than the intersection: a buyer holding
// a broad key and a narrow one can call everything the broad key reaches, and
// reporting the intersection would hide models they can actually use today.
func (r *Repository) UserModelAccess(ctx context.Context, userID uint) (UserModelAccess, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UserModelAccess{}, errDB
	}
	if userID == 0 {
		return UserModelAccess{}, fmt.Errorf("user id is required")
	}

	ctx = contextOrBackground(ctx)
	var records []APIKeyRecord
	if errFind := db.WithContext(ctx).Where("user_id = ?", userID).Order("id").Find(&records).Error; errFind != nil {
		return UserModelAccess{}, errFind
	}

	access := UserModelAccess{APIKeyCount: len(records), Restricted: true}
	if len(records) == 0 {
		return access, nil
	}

	seen := make(map[string]struct{})
	allowed := make([]string, 0)
	for i := range records {
		details, restricted, errDetails := allowedModelGroupDetailsForAPIKeyRecord(ctx, db, &records[i])
		if errDetails != nil {
			return UserModelAccess{}, errDetails
		}
		if !restricted {
			return UserModelAccess{APIKeyCount: len(records), Restricted: false}, nil
		}
		for _, modelID := range allowedModelIDsFromDetails(details, restricted) {
			key := strings.ToLower(modelID)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			allowed = append(allowed, modelID)
		}
	}
	sort.Strings(allowed)
	access.ModelIDs = allowed
	return access, nil
}

func allowedAuthIDsForAPIKeyRecord(ctx context.Context, db *gorm.DB, record *APIKeyRecord) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if record == nil {
		return nil, fmt.Errorf("api key record is nil")
	}
	channelIDs, errChannels := apiKeyChannelsFromJSON(record.Channels)
	if errChannels != nil {
		return nil, errChannels
	}
	if len(channelIDs) == 0 {
		return nil, nil
	}
	return allowedAuthIDsForChannelGroups(ctx, db, channelIDs)
}

func allowedAuthIDsForChannelGroups(ctx context.Context, db *gorm.DB, channelIDs []uint) ([]string, error) {
	details, errDetails := channelGroupDetailsForGroups(ctx, db, channelIDs)
	if errDetails != nil {
		return nil, errDetails
	}
	return allowedAuthIDsFromChannelGroupDetails(details, channelIDs), nil
}

func channelGroupDetailsForGroups(ctx context.Context, db *gorm.DB, channelIDs []uint) ([]ChannelGroupDetailRecord, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	channelIDs = normalizeChannelGroupIDs(channelIDs)
	if len(channelIDs) == 0 {
		return []ChannelGroupDetailRecord{}, nil
	}

	var details []ChannelGroupDetailRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Model(&ChannelGroupDetailRecord{}).
		Joins("JOIN channel_group ON channel_group.id = channel_group_detail.channel_group_id").
		Where("channel_group.deleted_at IS NULL").
		Where("channel_group.disabled = ?", false).
		Where("channel_group_detail.channel_group_id IN ?", channelIDs).
		Order("channel_group_detail.channel_group_id ASC, channel_group_detail.id ASC").
		Find(&details).Error; errFind != nil {
		return nil, errFind
	}
	return details, nil
}

func allowedAuthIDsFromChannelGroupDetails(details []ChannelGroupDetailRecord, channelIDs []uint) []string {
	channelIDs = normalizeChannelGroupIDs(channelIDs)
	channelSet := make(map[uint]struct{}, len(channelIDs))
	for _, channelID := range channelIDs {
		channelSet[channelID] = struct{}{}
	}

	allowed := make([]string, 0, len(details))
	seen := make(map[string]struct{}, len(details))
	for _, detail := range details {
		if _, ok := channelSet[detail.ChannelGroupID]; !ok {
			continue
		}
		authID := strings.TrimSpace(detail.AuthID)
		if authID == "" {
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		allowed = append(allowed, authID)
	}
	return allowed
}

func allowedModelIDsForAPIKeyRecord(ctx context.Context, db *gorm.DB, record *APIKeyRecord) ([]string, error) {
	details, restricted, errDetails := allowedModelGroupDetailsForAPIKeyRecord(ctx, db, record)
	if errDetails != nil {
		return nil, errDetails
	}
	return allowedModelIDsFromDetails(details, restricted), nil
}

func allowedModelGroupDetailsForAPIKeyRecord(ctx context.Context, db *gorm.DB, record *APIKeyRecord) ([]ModelGroupDetailRecord, bool, error) {
	if db == nil {
		return nil, false, fmt.Errorf("database connection is nil")
	}
	if record == nil {
		return nil, false, fmt.Errorf("api key record is nil")
	}
	modelGroupIDs, errModelGroups := apiKeyModelGroupsFromJSON(record.ModelGroups)
	if errModelGroups != nil {
		return nil, false, errModelGroups
	}
	if len(modelGroupIDs) == 0 {
		return nil, false, nil
	}

	var details []ModelGroupDetailRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Model(&ModelGroupDetailRecord{}).
		Joins("JOIN model_group ON model_group.id = model_group_detail.model_group_id").
		Where("model_group.deleted_at IS NULL").
		Where("model_group.disabled = ?", false).
		Where("model_group_detail.model_group_id IN ?", modelGroupIDs).
		Order("model_group_detail.model_group_id ASC, model_group_detail.id ASC").
		Find(&details).Error; errFind != nil {
		return nil, false, errFind
	}
	return details, true, nil
}

func allowedModelIDsFromDetails(details []ModelGroupDetailRecord, restricted bool) []string {
	if !restricted {
		return nil
	}
	allowed := make([]string, 0, len(details))
	seen := make(map[string]struct{}, len(details))
	for i := range details {
		modelID := canonicalModelGroupModelID(details[i].ModelID)
		if modelID == "" {
			continue
		}
		modelKey := strings.ToLower(modelID)
		if _, ok := seen[modelKey]; ok {
			continue
		}
		seen[modelKey] = struct{}{}
		allowed = append(allowed, modelID)
	}
	return allowed
}

func replaceAPIKeyEntriesTxWithStats(ctx context.Context, tx *gorm.DB, entries []APIKeyEntryUpdate) (APIKeyUpsertStats, error) {
	if tx == nil {
		return APIKeyUpsertStats{}, fmt.Errorf("database connection is nil")
	}
	entries = normalizeAPIKeyEntryUpdates(entries)

	var existing []APIKeyRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).Unscoped().Order("id").Find(&existing).Error; errFind != nil {
		return APIKeyUpsertStats{}, errFind
	}

	stats := APIKeyUpsertStats{}
	existingByID := make(map[uint]*APIKeyRecord, len(existing))
	existingByKey := make(map[string]*APIKeyRecord, len(existing))
	for i := range existing {
		record := &existing[i]
		existingByID[record.ID] = record
		key := strings.TrimSpace(record.APIKey)
		if key == "" {
			continue
		}
		existingByKey[key] = record
	}

	keepIDs := make(map[uint]struct{}, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		userID := normalizeOptionalUserID(entry.UserID)
		userIDSet := entry.UserIDSet || entry.UserID != nil
		var displayName *string
		if entry.DisplayNameSet {
			var errDisplayName error
			displayName, errDisplayName = normalizeAPIKeyDisplayName(entry.DisplayName)
			if errDisplayName != nil {
				return APIKeyUpsertStats{}, errDisplayName
			}
		}
		if userIDSet && userID != nil {
			if errUser := ensureUserExists(ctx, tx, *userID); errUser != nil {
				return APIKeyUpsertStats{}, errUser
			}
		}
		channelsJSON := emptyAPIKeyChannelsJSON()
		var errChannels error
		if entry.Channels != nil {
			channelsJSON, errChannels = apiKeyChannelsJSON(*entry.Channels)
			if errChannels != nil {
				return APIKeyUpsertStats{}, errChannels
			}
		}
		modelGroupsJSON := emptyAPIKeyModelGroupsJSON()
		var errModelGroups error
		if entry.ModelGroups != nil {
			modelGroupsJSON, errModelGroups = apiKeyModelGroupsJSON(*entry.ModelGroups)
			if errModelGroups != nil {
				return APIKeyUpsertStats{}, errModelGroups
			}
		}

		record := existingByID[entry.ID]
		if record == nil {
			record = existingByKey[key]
		}
		if record != nil {
			if duplicate := existingByKey[key]; duplicate != nil && duplicate.ID != record.ID {
				return APIKeyUpsertStats{}, ErrAPIKeyExists
			}
			keepIDs[record.ID] = struct{}{}
			updates := make(map[string]any)
			updatedEntry := false
			if record.DeletedAt.Valid {
				updates["deleted_at"] = nil
				stats.Restored++
				stats.RuntimeChanged = true
			} else {
				stats.Unchanged++
			}
			if strings.TrimSpace(record.APIKey) != key {
				updates["api_key"] = key
				updatedEntry = true
				stats.RuntimeChanged = true
			}
			if entry.DisplayNameSet && !sameOptionalString(record.DisplayName, displayName) {
				updates["display_name"] = displayName
				updatedEntry = true
			}
			if userIDSet && !sameOptionalUint(record.UserID, userID) {
				updates["user_id"] = userID
				updatedEntry = true
				stats.RuntimeChanged = true
			}
			if entry.Channels != nil {
				currentChannels, errCurrent := apiKeyChannelsFromJSON(record.Channels)
				if errCurrent != nil {
					return APIKeyUpsertStats{}, errCurrent
				}
				if !reflect.DeepEqual(currentChannels, normalizeChannelGroupIDs(*entry.Channels)) {
					updates["channels"] = channelsJSON
					updatedEntry = true
					stats.RuntimeChanged = true
				}
			}
			if entry.ModelGroups != nil {
				currentModelGroups, errCurrent := apiKeyModelGroupsFromJSON(record.ModelGroups)
				if errCurrent != nil {
					return APIKeyUpsertStats{}, errCurrent
				}
				if !reflect.DeepEqual(currentModelGroups, normalizeModelGroupIDs(*entry.ModelGroups)) {
					updates["model_groups"] = modelGroupsJSON
					updatedEntry = true
					stats.RuntimeChanged = true
				}
			}
			if updatedEntry && !record.DeletedAt.Valid {
				stats.Updated++
				stats.Unchanged--
			}
			if len(updates) > 0 {
				if errUpdate := tx.WithContext(contextOrBackground(ctx)).Unscoped().
					Model(&APIKeyRecord{}).
					Where("id = ?", record.ID).
					Updates(updates).Error; errUpdate != nil {
					return APIKeyUpsertStats{}, errUpdate
				}
			}
			continue
		}

		created := APIKeyRecord{
			APIKey:      key,
			DisplayName: displayName,
			UserID:      userID,
			Channels:    channelsJSON,
			ModelGroups: modelGroupsJSON,
		}
		if errCreate := tx.WithContext(contextOrBackground(ctx)).Create(&created).Error; errCreate != nil {
			return APIKeyUpsertStats{}, errCreate
		}
		keepIDs[created.ID] = struct{}{}
		stats.Created++
		stats.RuntimeChanged = true
	}

	for _, record := range existing {
		if _, ok := keepIDs[record.ID]; ok {
			continue
		}
		if record.DeletedAt.Valid {
			continue
		}
		if errDelete := tx.WithContext(contextOrBackground(ctx)).Delete(&APIKeyRecord{}, "id = ?", record.ID).Error; errDelete != nil {
			return APIKeyUpsertStats{}, errDelete
		}
		stats.Removed++
		stats.RuntimeChanged = true
	}
	return stats, nil
}

func normalizeAPIKeyEntryUpdates(entries []APIKeyEntryUpdate) []APIKeyEntryUpdate {
	if len(entries) == 0 {
		return nil
	}
	normalized := make([]APIKeyEntryUpdate, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		next := APIKeyEntryUpdate{ID: entry.ID, APIKey: key, DisplayNameSet: entry.DisplayNameSet, UserIDSet: entry.UserIDSet || entry.UserID != nil}
		if entry.DisplayNameSet {
			next.DisplayName = entry.DisplayName
		}
		next.UserID = normalizeOptionalUserID(entry.UserID)
		if entry.Channels != nil {
			channels := normalizeChannelGroupIDs(*entry.Channels)
			next.Channels = &channels
		}
		if entry.ModelGroups != nil {
			modelGroups := normalizeModelGroupIDs(*entry.ModelGroups)
			next.ModelGroups = &modelGroups
		}
		normalized = append(normalized, next)
	}
	for left, right := 0, len(normalized)-1; left < right; left, right = left+1, right-1 {
		normalized[left], normalized[right] = normalized[right], normalized[left]
	}
	return normalized
}

func lockAPIKeyMutationTransaction(tx *gorm.DB) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if tx.Dialector == nil || tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", apiKeyMutationAdvisoryLockKey).Error
}

func lockAPIKeyReadTransaction(tx *gorm.DB) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if tx.Dialector == nil || tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock_shared(?)", apiKeyMutationAdvisoryLockKey).Error
}

func listAPIKeyEntriesTx(ctx context.Context, db *gorm.DB) ([]APIKeyEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	var records []APIKeyRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	entries := make([]APIKeyEntry, 0, len(records))
	for i := range records {
		entry, errEntry := apiKeyEntryFromRecord(&records[i])
		if errEntry != nil {
			return nil, errEntry
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func apiKeyEntriesETag(entries []APIKeyEntry) (string, error) {
	hasher := sha256.New()
	encoder := json.NewEncoder(hasher)
	encoder.SetEscapeHTML(false)
	for i := range entries {
		entry := entries[i]
		fingerprint := struct {
			ID          uint    `json:"id"`
			APIKey      string  `json:"api_key"`
			DisplayName *string `json:"display_name"`
			UserID      *uint   `json:"user_id"`
			Channels    []uint  `json:"channels"`
			ModelGroups []uint  `json:"model_groups"`
		}{
			ID:          entry.ID,
			APIKey:      entry.APIKey,
			DisplayName: entry.DisplayName,
			UserID:      entry.UserID,
			Channels:    entry.Channels,
			ModelGroups: entry.ModelGroups,
		}
		if errEncode := encoder.Encode(fingerprint); errEncode != nil {
			return "", errEncode
		}
	}
	return fmt.Sprintf(`"%x"`, hasher.Sum(nil)), nil
}

func apiKeyETagMatches(ifMatch string, current string) bool {
	current = strings.TrimSpace(current)
	matched := false
	for _, candidate := range strings.Split(ifMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == "*" || strings.HasPrefix(candidate, "W/") || !isQuotedAPIKeyETag(candidate) {
			return false
		}
		if candidate == current {
			matched = true
		}
	}
	return matched
}

func isQuotedAPIKeyETag(value string) bool {
	return len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"'
}

func apiKeyEntryFromRecord(record *APIKeyRecord) (APIKeyEntry, error) {
	if record == nil {
		return APIKeyEntry{}, fmt.Errorf("api key record is nil")
	}
	channels, errChannels := apiKeyChannelsFromJSON(record.Channels)
	if errChannels != nil {
		return APIKeyEntry{}, errChannels
	}
	modelGroups, errModelGroups := apiKeyModelGroupsFromJSON(record.ModelGroups)
	if errModelGroups != nil {
		return APIKeyEntry{}, errModelGroups
	}
	return APIKeyEntry{
		ID:          record.ID,
		APIKey:      strings.TrimSpace(record.APIKey),
		DisplayName: normalizedAPIKeyDisplayName(record.DisplayName),
		UserID:      normalizeOptionalUserID(record.UserID),
		Channels:    channels,
		ModelGroups: modelGroups,
	}, nil
}

// APIKeyEntryFromRecord converts an API key record to a response entry.
func APIKeyEntryFromRecord(record *APIKeyRecord) (APIKeyEntry, error) {
	return apiKeyEntryFromRecord(record)
}

func apiKeyChannelsJSON(channels []uint) (JSONB, error) {
	channels = normalizeChannelGroupIDs(channels)
	raw, errMarshal := json.Marshal(channels)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return JSONB(raw), nil
}

func emptyAPIKeyChannelsJSON() JSONB {
	return JSONB("[]")
}

func apiKeyModelGroupsJSON(modelGroups []uint) (JSONB, error) {
	modelGroups = normalizeModelGroupIDs(modelGroups)
	raw, errMarshal := json.Marshal(modelGroups)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return JSONB(raw), nil
}

func emptyAPIKeyModelGroupsJSON() JSONB {
	return JSONB("[]")
}

func sameOptionalUint(left *uint, right *uint) bool {
	left = normalizeOptionalUserID(left)
	right = normalizeOptionalUserID(right)
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func normalizeAPIKeyDisplayName(displayName *string) (*string, error) {
	if displayName == nil {
		return nil, nil
	}
	value := *displayName
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrInvalidAPIKeyDisplayName)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return nil, fmt.Errorf("%w: control characters are not allowed", ErrInvalidAPIKeyDisplayName)
		}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(value) > APIKeyDisplayNameMaxLength {
		return nil, fmt.Errorf("%w: length exceeds %d characters", ErrInvalidAPIKeyDisplayName, APIKeyDisplayNameMaxLength)
	}
	return &value, nil
}

func normalizedAPIKeyDisplayName(displayName *string) *string {
	if displayName == nil {
		return nil
	}
	value := strings.TrimSpace(*displayName)
	if value == "" {
		return nil
	}
	return &value
}

func sameOptionalString(left *string, right *string) bool {
	left = normalizedAPIKeyDisplayName(left)
	right = normalizedAPIKeyDisplayName(right)
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func migrateAPIKeyChannels(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	return db.Model(&APIKeyRecord{}).Where("channels IS NULL").Update("channels", emptyAPIKeyChannelsJSON()).Error
}

func migrateAPIKeyModelGroups(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	return db.Model(&APIKeyRecord{}).Where("model_groups IS NULL").Update("model_groups", emptyAPIKeyModelGroupsJSON()).Error
}

func apiKeyChannelsFromJSON(raw JSONB) ([]uint, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var channels []uint
	if errUnmarshal := json.Unmarshal(raw, &channels); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return normalizeChannelGroupIDs(channels), nil
}

func apiKeyModelGroupsFromJSON(raw JSONB) ([]uint, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var modelGroups []uint
	if errUnmarshal := json.Unmarshal(raw, &modelGroups); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return normalizeModelGroupIDs(modelGroups), nil
}

func normalizeChannelGroupIDs(ids []uint) []uint {
	return normalizeAPIKeyGroupIDs(ids)
}

func normalizeModelGroupIDs(ids []uint) []uint {
	return normalizeAPIKeyGroupIDs(ids)
}

func normalizeAPIKeyGroupIDs(ids []uint) []uint {
	if len(ids) == 0 {
		return nil
	}
	out := make([]uint, 0, len(ids))
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i] < out[j]
	})
	return out
}
