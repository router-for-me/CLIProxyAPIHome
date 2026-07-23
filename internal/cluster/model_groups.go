package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// ErrInvalidModelChannelGroupID indicates that a model detail channel binding contains ID zero.
var ErrInvalidModelChannelGroupID = errors.New("model channel group id must be greater than zero")

type ModelGroupUpdate struct {
	GroupName *string
	Disabled  *bool
}

type ModelGroupDetailUpdate struct {
	ModelGroupID *uint
	ModelID      *string
	Channels     *[]uint
}

type ModelGroupDetailFilter struct {
	ModelGroupID *uint
	ModelID      string
}

// ListModelGroups returns model groups.
func (r *Repository) ListModelGroups(ctx context.Context) ([]ModelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []ModelGroupRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// GetModelGroup returns a model group by ID.
func (r *Repository) GetModelGroup(ctx context.Context, id uint) (*ModelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("model group id is required")
	}

	record := &ModelGroupRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// CreateModelGroup creates a model group.
func (r *Repository) CreateModelGroup(ctx context.Context, groupName string, disabled bool) (*ModelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		return nil, fmt.Errorf("group name is required")
	}

	record := &ModelGroupRecord{
		GroupName: groupName,
		Disabled:  disabled,
	}
	if errCreate := db.WithContext(contextOrBackground(ctx)).Create(record).Error; errCreate != nil {
		return nil, errCreate
	}
	return record, nil
}

// UpdateModelGroup updates a model group.
func (r *Repository) UpdateModelGroup(ctx context.Context, id uint, update ModelGroupUpdate) (*ModelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("model group id is required")
	}

	record := &ModelGroupRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errFirst := tx.Where("id = ?", id).First(record).Error; errFirst != nil {
			return errFirst
		}
		if update.GroupName != nil {
			groupName := strings.TrimSpace(*update.GroupName)
			if groupName == "" {
				return fmt.Errorf("group name is required")
			}
			record.GroupName = groupName
		}
		if update.Disabled != nil {
			record.Disabled = *update.Disabled
		}
		return tx.Save(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// DeleteModelGroup deletes a model group and its details.
func (r *Repository) DeleteModelGroup(ctx context.Context, id uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if id == 0 {
		return fmt.Errorf("model group id is required")
	}

	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := ModelGroupRecord{}
		if errFirst := tx.Where("id = ?", id).First(&record).Error; errFirst != nil {
			return errFirst
		}
		if errDeleteDetails := tx.Where("model_group_id = ?", id).Delete(&ModelGroupDetailRecord{}).Error; errDeleteDetails != nil {
			return errDeleteDetails
		}
		return tx.Delete(&record).Error
	})
}

// ListModelGroupDetails returns model group details.
func (r *Repository) ListModelGroupDetails(ctx context.Context, filter ModelGroupDetailFilter) ([]ModelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	query := db.WithContext(contextOrBackground(ctx)).Order("model_group_id ASC, id ASC")
	if filter.ModelGroupID != nil {
		query = query.Where("model_group_id = ?", *filter.ModelGroupID)
	}

	var records []ModelGroupDetailRecord
	if errFind := query.Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	rawModelID := strings.TrimSpace(filter.ModelID)
	if rawModelID == "" {
		return records, nil
	}
	modelID := canonicalModelGroupModelID(rawModelID)
	if modelID == "" {
		return []ModelGroupDetailRecord{}, nil
	}
	filtered := make([]ModelGroupDetailRecord, 0, len(records))
	for i := range records {
		if strings.EqualFold(canonicalModelGroupModelID(records[i].ModelID), modelID) {
			filtered = append(filtered, records[i])
		}
	}
	return filtered, nil
}

// GetModelGroupDetail returns a model group detail by ID.
func (r *Repository) GetModelGroupDetail(ctx context.Context, id uint) (*ModelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("model group detail id is required")
	}

	record := &ModelGroupDetailRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// CreateModelGroupDetail creates a model group detail.
func (r *Repository) CreateModelGroupDetail(ctx context.Context, modelGroupID uint, modelID string, channels []uint) (*ModelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	modelID = canonicalModelGroupModelID(modelID)
	if modelGroupID == 0 {
		return nil, fmt.Errorf("model group id is required")
	}
	if modelID == "" {
		return nil, fmt.Errorf("model id is required")
	}
	if errChannels := validateModelGroupDetailChannelIDs(channels); errChannels != nil {
		return nil, errChannels
	}
	channelsJSON, errChannels := modelGroupDetailChannelsJSON(channels)
	if errChannels != nil {
		return nil, errChannels
	}

	record := &ModelGroupDetailRecord{
		ModelGroupID: modelGroupID,
		ModelID:      modelID,
		Channels:     channelsJSON,
	}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errGroup := ensureModelGroupExists(ctx, tx, modelGroupID); errGroup != nil {
			return errGroup
		}
		if errChannelGroups := ensureChannelGroupsExist(ctx, tx, channels); errChannelGroups != nil {
			return errChannelGroups
		}
		return tx.Create(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UpdateModelGroupDetail updates a model group detail.
func (r *Repository) UpdateModelGroupDetail(ctx context.Context, id uint, update ModelGroupDetailUpdate) (*ModelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("model group detail id is required")
	}

	record := &ModelGroupDetailRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errFirst := tx.Where("id = ?", id).First(record).Error; errFirst != nil {
			return errFirst
		}
		if update.ModelGroupID != nil {
			if *update.ModelGroupID == 0 {
				return fmt.Errorf("model group id is required")
			}
			if errGroup := ensureModelGroupExists(ctx, tx, *update.ModelGroupID); errGroup != nil {
				return errGroup
			}
			record.ModelGroupID = *update.ModelGroupID
		}
		if update.ModelID != nil {
			modelID := canonicalModelGroupModelID(*update.ModelID)
			if modelID == "" {
				return fmt.Errorf("model id is required")
			}
			record.ModelID = modelID
		}
		if update.Channels != nil {
			if errChannelGroups := ensureChannelGroupsExist(ctx, tx, *update.Channels); errChannelGroups != nil {
				return errChannelGroups
			}
			channelsJSON, errChannels := modelGroupDetailChannelsJSON(*update.Channels)
			if errChannels != nil {
				return errChannels
			}
			record.Channels = channelsJSON
		}
		return tx.Save(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// DeleteModelGroupDetail deletes a model group detail.
func (r *Repository) DeleteModelGroupDetail(ctx context.Context, id uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if id == 0 {
		return fmt.Errorf("model group detail id is required")
	}

	record := ModelGroupDetailRecord{}
	ctx = contextOrBackground(ctx)
	if errFirst := db.WithContext(ctx).Where("id = ?", id).First(&record).Error; errFirst != nil {
		return errFirst
	}
	return db.WithContext(ctx).Delete(&record).Error
}

// ParseModelRecordID parses a positive unsigned record ID.
func ParseModelRecordID(value string) (uint, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("record id is required")
	}
	parsed, errParse := strconv.ParseUint(value, 10, 64)
	if errParse != nil {
		return 0, errParse
	}
	if parsed == 0 {
		return 0, fmt.Errorf("record id is required")
	}
	return uint(parsed), nil
}

func canonicalModelGroupModelID(modelID string) string {
	return coreauth.CanonicalModelID(modelID)
}

func ensureModelGroupExists(ctx context.Context, tx *gorm.DB, id uint) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	record := ModelGroupRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(&record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return fmt.Errorf("model group not found: %w", errFirst)
	}
	return errFirst
}

func validateModelGroupDetailChannelIDs(ids []uint) error {
	for _, id := range ids {
		if id == 0 {
			return ErrInvalidModelChannelGroupID
		}
	}
	return nil
}

func ensureChannelGroupsExist(ctx context.Context, tx *gorm.DB, ids []uint) error {
	if errIDs := validateModelGroupDetailChannelIDs(ids); errIDs != nil {
		return errIDs
	}
	for _, id := range normalizeChannelGroupIDs(ids) {
		if errGroup := ensureChannelGroupExists(ctx, tx, id); errGroup != nil {
			return errGroup
		}
	}
	return nil
}

func modelGroupDetailChannelsJSON(channels []uint) (JSONB, error) {
	return apiKeyChannelsJSON(channels)
}

func modelGroupDetailChannelsFromJSON(raw JSONB) ([]uint, error) {
	return apiKeyChannelsFromJSON(raw)
}

// ModelGroupDetailChannelIDs returns the channel group IDs bound to a model detail.
func ModelGroupDetailChannelIDs(record *ModelGroupDetailRecord) ([]uint, error) {
	if record == nil {
		return nil, fmt.Errorf("model group detail record is nil")
	}
	return modelGroupDetailChannelsFromJSON(record.Channels)
}

func migrateModelGroupDetailChannels(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	return db.Model(&ModelGroupDetailRecord{}).
		Where("channels IS NULL").
		Update("channels", emptyAPIKeyChannelsJSON()).Error
}
