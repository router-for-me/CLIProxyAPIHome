package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ErrAPIKeyExists indicates that an API key value is already owned by another record.
var ErrAPIKeyExists = errors.New("api key already exists")

// ErrAPIKeySelectorMismatch indicates that multiple selectors do not identify the same API key.
var ErrAPIKeySelectorMismatch = errors.New("api key selector mismatch")

type APIKeyEntry struct {
	ID          uint
	APIKey      string
	UserID      *uint
	Channels    []uint
	ModelGroups []uint
}

type APIKeyEntryUpdate struct {
	ID          uint
	APIKey      string
	UserID      *uint
	Channels    *[]uint
	ModelGroups *[]uint
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
	APIKey      *string
	UserID      *uint
	Channels    *[]uint
	ModelGroups *[]uint
}

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

	var records []APIKeyRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}

	out := make([]APIKeyEntry, 0, len(records))
	for _, record := range records {
		entry, errEntry := apiKeyEntryFromRecord(&record)
		if errEntry != nil {
			return nil, errEntry
		}
		out = append(out, entry)
	}
	return out, nil
}

// ReplaceAPIKeyEntries replaces active API key rows and updates explicit channel bindings.
func (r *Repository) ReplaceAPIKeyEntries(ctx context.Context, entries []APIKeyEntryUpdate) (APIKeyUpsertStats, error) {
	db, errDB := r.database()
	if errDB != nil {
		return APIKeyUpsertStats{}, errDB
	}

	ctx = contextOrBackground(ctx)
	var stats APIKeyUpsertStats
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var errReplace error
		stats, errReplace = replaceAPIKeyEntriesTxWithStats(ctx, tx, entries)
		if errReplace != nil {
			return errReplace
		}
		deleteResult := tx.Delete(&ConfigRecord{}, "key = ?", configAPIKeysRootKey)
		if deleteResult.Error != nil {
			return deleteResult.Error
		}
		if !stats.Changed() && deleteResult.RowsAffected == 0 {
			return nil
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	return stats, errTransaction
}

// CreateAPIKey creates or restores one API key without replacing the full key list.
func (r *Repository) CreateAPIKey(ctx context.Context, update APIKeyEntryUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	key := strings.TrimSpace(update.APIKey)
	if key == "" {
		return nil, fmt.Errorf("api key is required")
	}

	userID := normalizeOptionalUserID(update.UserID)
	channelsJSON := emptyAPIKeyChannelsJSON()
	if update.Channels != nil {
		var errChannels error
		channelsJSON, errChannels = apiKeyChannelsJSON(*update.Channels)
		if errChannels != nil {
			return nil, errChannels
		}
	}
	modelGroupsJSON := emptyAPIKeyModelGroupsJSON()
	if update.ModelGroups != nil {
		var errModelGroups error
		modelGroupsJSON, errModelGroups = apiKeyModelGroupsJSON(*update.ModelGroups)
		if errModelGroups != nil {
			return nil, errModelGroups
		}
	}

	record := &APIKeyRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
			if errRestore := tx.WithContext(ctx).Unscoped().
				Model(&APIKeyRecord{}).
				Where("id = ?", existing.ID).
				Updates(map[string]any{
					"user_id":      userID,
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
			return ErrAPIKeyExists
		}

		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UpdateAPIKey updates one API key selected by stable ID or a legacy selector.
func (r *Repository) UpdateAPIKey(ctx context.Context, selector APIKeySelector, update APIKeyAdminUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if errSelector := validateAPIKeySelector(selector); errSelector != nil {
		return nil, errSelector
	}

	record := &APIKeyRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errFind := findAPIKeyRecord(ctx, tx, selector, record); errFind != nil {
			return errFind
		}
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
			record.APIKey = nextKey
		}
		if update.UserID != nil {
			nextUserID := normalizeOptionalUserID(update.UserID)
			if nextUserID != nil {
				if errUser := ensureUserExists(ctx, tx, *nextUserID); errUser != nil {
					return errUser
				}
			}
			record.UserID = nextUserID
		}
		if update.Channels != nil {
			channelsJSON, errChannels := apiKeyChannelsJSON(*update.Channels)
			if errChannels != nil {
				return errChannels
			}
			record.Channels = channelsJSON
		}
		if update.ModelGroups != nil {
			modelGroupsJSON, errModelGroups := apiKeyModelGroupsJSON(*update.ModelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			record.ModelGroups = modelGroupsJSON
		}
		if errSave := tx.WithContext(ctx).Save(record).Error; errSave != nil {
			if IsAPIKeyConflictError(errSave) {
				return ErrAPIKeyExists
			}
			return errSave
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
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
		if errFirst := tx.Where("api_key = ?", apiKey).First(record).Error; errFirst != nil {
			return errFirst
		}
		if userID != nil {
			nextUserID := normalizeOptionalUserID(userID)
			if nextUserID != nil {
				if errUser := ensureUserExists(ctx, tx, *nextUserID); errUser != nil {
					return errUser
				}
			}
			record.UserID = nextUserID
		}
		if channels != nil {
			channelsJSON, errChannels := apiKeyChannelsJSON(*channels)
			if errChannels != nil {
				return errChannels
			}
			record.Channels = channelsJSON
		}
		if modelGroups != nil {
			modelGroupsJSON, errModelGroups := apiKeyModelGroupsJSON(*modelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			record.ModelGroups = modelGroupsJSON
		}
		if errSave := tx.Save(record).Error; errSave != nil {
			return errSave
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
			existing.Channels = channelsJSON
			existing.ModelGroups = modelGroupsJSON
			if errSave := tx.WithContext(ctx).Save(existing).Error; errSave != nil {
				return errSave
			}
			record = existing
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
		query := tx.WithContext(ctx).Where("user_id = ?", userID)
		if id > 0 {
			query = query.Where("id = ?", id)
		} else {
			query = query.Where("api_key = ?", apiKey)
		}
		if errFirst := query.First(record).Error; errFirst != nil {
			return errFirst
		}
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
			record.APIKey = nextKey
		}
		if update.Channels != nil {
			channelsJSON, errChannels := apiKeyChannelsJSON(*update.Channels)
			if errChannels != nil {
				return errChannels
			}
			record.Channels = channelsJSON
		}
		if update.ModelGroups != nil {
			modelGroupsJSON, errModelGroups := apiKeyModelGroupsJSON(*update.ModelGroups)
			if errModelGroups != nil {
				return errModelGroups
			}
			record.ModelGroups = modelGroupsJSON
		}
		if errSave := tx.Save(record).Error; errSave != nil {
			return errSave
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
		if userID != nil {
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
			} else {
				stats.Unchanged++
			}
			if strings.TrimSpace(record.APIKey) != key {
				updates["api_key"] = key
				updatedEntry = true
			}
			if !sameOptionalUint(record.UserID, userID) {
				updates["user_id"] = userID
				updatedEntry = true
			}
			if entry.Channels != nil {
				currentChannels, errCurrent := apiKeyChannelsFromJSON(record.Channels)
				if errCurrent != nil {
					return APIKeyUpsertStats{}, errCurrent
				}
				if !reflect.DeepEqual(currentChannels, normalizeChannelGroupIDs(*entry.Channels)) {
					updates["channels"] = channelsJSON
					updatedEntry = true
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
			UserID:      userID,
			Channels:    channelsJSON,
			ModelGroups: modelGroupsJSON,
		}
		if errCreate := tx.WithContext(contextOrBackground(ctx)).Create(&created).Error; errCreate != nil {
			return APIKeyUpsertStats{}, errCreate
		}
		keepIDs[created.ID] = struct{}{}
		stats.Created++
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
	}
	return stats, nil
}

func normalizeAPIKeyEntryUpdates(entries []APIKeyEntryUpdate) []APIKeyEntryUpdate {
	if len(entries) == 0 {
		return nil
	}
	normalized := make([]APIKeyEntryUpdate, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		next := APIKeyEntryUpdate{ID: entry.ID, APIKey: key}
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
	return normalized
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
