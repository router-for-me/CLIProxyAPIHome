package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ChannelGroupUpdate struct {
	ChannelName *string
	Disabled    *bool
}

type ChannelGroupDetailUpdate struct {
	ChannelGroupID *uint
	AuthID         *string
}

type ChannelGroupDetailFilter struct {
	ChannelGroupID *uint
	AuthID         string
}

type ChannelGroupDetailAuthMeta struct {
	Label       string
	Provider    string
	Status      string
	Disabled    bool
	Unavailable bool
	Deleted     bool
}

// ListChannelGroups returns channel groups.
func (r *Repository) ListChannelGroups(ctx context.Context) ([]ChannelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []ChannelGroupRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// GetChannelGroup returns a channel group by ID.
func (r *Repository) GetChannelGroup(ctx context.Context, id uint) (*ChannelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("channel group id is required")
	}

	record := &ChannelGroupRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// CreateChannelGroup creates a channel group.
func (r *Repository) CreateChannelGroup(ctx context.Context, channelName string, disabled bool) (*ChannelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	channelName = strings.TrimSpace(channelName)
	if channelName == "" {
		return nil, fmt.Errorf("channel name is required")
	}

	record := &ChannelGroupRecord{
		ChannelName: channelName,
		Disabled:    disabled,
	}
	if errCreate := db.WithContext(contextOrBackground(ctx)).Create(record).Error; errCreate != nil {
		return nil, errCreate
	}
	return record, nil
}

// UpdateChannelGroup updates a channel group.
func (r *Repository) UpdateChannelGroup(ctx context.Context, id uint, update ChannelGroupUpdate) (*ChannelGroupRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("channel group id is required")
	}

	record := &ChannelGroupRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if errFirst := tx.Where("id = ?", id).First(record).Error; errFirst != nil {
			return errFirst
		}
		if update.ChannelName != nil {
			channelName := strings.TrimSpace(*update.ChannelName)
			if channelName == "" {
				return fmt.Errorf("channel name is required")
			}
			record.ChannelName = channelName
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

// DeleteChannelGroup deletes a channel group, its details, and references to it.
func (r *Repository) DeleteChannelGroup(ctx context.Context, id uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if id == 0 {
		return fmt.Errorf("channel group id is required")
	}

	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		record := ChannelGroupRecord{}
		if errFirst := tx.Where("id = ?", id).First(&record).Error; errFirst != nil {
			return errFirst
		}
		if errDeleteDetails := tx.Where("channel_group_id = ?", id).Delete(&ChannelGroupDetailRecord{}).Error; errDeleteDetails != nil {
			return errDeleteDetails
		}
		if errDelete := tx.Delete(&record).Error; errDelete != nil {
			return errDelete
		}
		return removeAccessGroupBindings(tx, []uint{id}, nil)
	})
}

// ListChannelGroupDetails returns channel group details.
func (r *Repository) ListChannelGroupDetails(ctx context.Context, filter ChannelGroupDetailFilter) ([]ChannelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	query := db.WithContext(contextOrBackground(ctx)).Order("channel_group_id ASC, id ASC")
	if filter.ChannelGroupID != nil {
		query = query.Where("channel_group_id = ?", *filter.ChannelGroupID)
	}
	if authID := strings.TrimSpace(filter.AuthID); authID != "" {
		query = query.Where("auth_id = ?", authID)
	}

	var records []ChannelGroupDetailRecord
	if errFind := query.Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// GetChannelGroupDetail returns a channel group detail by ID.
func (r *Repository) GetChannelGroupDetail(ctx context.Context, id uint) (*ChannelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("channel group detail id is required")
	}

	record := &ChannelGroupDetailRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(record).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// CreateChannelGroupDetail creates a channel group detail.
func (r *Repository) CreateChannelGroupDetail(ctx context.Context, channelGroupID uint, authID string) (*ChannelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	authID = strings.TrimSpace(authID)
	if channelGroupID == 0 {
		return nil, fmt.Errorf("channel group id is required")
	}
	if authID == "" {
		return nil, fmt.Errorf("auth id is required")
	}

	record := &ChannelGroupDetailRecord{
		ChannelGroupID: channelGroupID,
		AuthID:         authID,
	}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if errGroup := ensureChannelGroupExists(ctx, tx, channelGroupID); errGroup != nil {
			return errGroup
		}
		if errAuth := ensureAuthExists(ctx, tx, authID); errAuth != nil {
			return errAuth
		}
		return tx.Create(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UpdateChannelGroupDetail updates a channel group detail.
func (r *Repository) UpdateChannelGroupDetail(ctx context.Context, id uint, update ChannelGroupDetailUpdate) (*ChannelGroupDetailRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, fmt.Errorf("channel group detail id is required")
	}

	record := &ChannelGroupDetailRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if errFirst := tx.Where("id = ?", id).First(record).Error; errFirst != nil {
			return errFirst
		}
		if update.ChannelGroupID != nil {
			if *update.ChannelGroupID == 0 {
				return fmt.Errorf("channel group id is required")
			}
			if errGroup := ensureChannelGroupExists(ctx, tx, *update.ChannelGroupID); errGroup != nil {
				return errGroup
			}
			record.ChannelGroupID = *update.ChannelGroupID
		}
		if update.AuthID != nil {
			authID := strings.TrimSpace(*update.AuthID)
			if authID == "" {
				return fmt.Errorf("auth id is required")
			}
			if errAuth := ensureAuthExists(ctx, tx, authID); errAuth != nil {
				return errAuth
			}
			record.AuthID = authID
		}
		return tx.Save(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// DeleteChannelGroupDetail deletes a channel group detail.
func (r *Repository) DeleteChannelGroupDetail(ctx context.Context, id uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if id == 0 {
		return fmt.Errorf("channel group detail id is required")
	}

	record := ChannelGroupDetailRecord{}
	ctx = contextOrBackground(ctx)
	if errFirst := db.WithContext(ctx).Where("id = ?", id).First(&record).Error; errFirst != nil {
		return errFirst
	}
	return db.WithContext(ctx).Delete(&record).Error
}

// ParseChannelRecordID parses a positive unsigned record ID.
func ParseChannelRecordID(value string) (uint, error) {
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

func ensureChannelGroupExists(ctx context.Context, tx *gorm.DB, id uint) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	record := ChannelGroupRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).Where("id = ?", id).First(&record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return fmt.Errorf("channel group not found: %w", errFirst)
	}
	return errFirst
}

func ensureAuthExists(ctx context.Context, tx *gorm.DB, authID string) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return fmt.Errorf("auth id is required")
	}
	record := AuthRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).
		Where(clause.Or(
			clause.Eq{Column: clause.Column{Name: "uuid"}, Value: authID},
			clause.Eq{Column: clause.Column{Name: "id"}, Value: authID},
			clause.Eq{Column: clause.Column{Name: "index"}, Value: authID},
		)).
		First(&record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return fmt.Errorf("auth not found: %w", errFirst)
	}
	return errFirst
}

// ResolveChannelGroupDetailAuthMetadata loads display metadata for the given
// credential identifiers, including soft-deleted auth records so historical
// channel bindings remain identifiable.
func (r *Repository) ResolveChannelGroupDetailAuthMetadata(ctx context.Context, authIDs []string) (map[string]ChannelGroupDetailAuthMeta, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	uniqueIDs := make([]string, 0, len(authIDs))
	seen := make(map[string]struct{}, len(authIDs))
	for _, rawID := range authIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return nil, nil
	}

	values := make([]any, len(uniqueIDs))
	for i, id := range uniqueIDs {
		values[i] = id
	}

	var records []AuthRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Unscoped().
		Model(&AuthRecord{}).
		Where(clause.Or(
			clause.IN{Column: clause.Column{Name: "uuid"}, Values: values},
			clause.IN{Column: clause.Column{Name: "id"}, Values: values},
			clause.IN{Column: clause.Column{Name: "index"}, Values: values},
		)).
		Find(&records).Error; errFind != nil {
		return nil, errFind
	}

	result := make(map[string]ChannelGroupDetailAuthMeta, len(records))
	setMeta := func(key string, meta ChannelGroupDetailAuthMeta) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if existing, exists := result[key]; exists && !existing.Deleted && meta.Deleted {
			return
		}
		result[key] = meta
	}

	for i := range records {
		rec := &records[i]
		meta := ChannelGroupDetailAuthMeta{
			Label:       extractAuthRecordLabel(rec),
			Provider:    strings.TrimSpace(rec.Provider),
			Status:      strings.TrimSpace(string(rec.Status)),
			Disabled:    rec.Disabled,
			Unavailable: rec.Unavailable,
			Deleted:     rec.DeletedAt.Valid,
		}
		setMeta(rec.UUID, meta)
		setMeta(rec.ID, meta)
		setMeta(rec.Index, meta)
	}
	return result, nil
}

func extractAuthRecordLabel(record *AuthRecord) string {
	if record == nil {
		return ""
	}
	if label := strings.TrimSpace(record.Label); label != "" {
		return label
	}
	if compat := strings.TrimSpace(record.CompatName); compat != "" {
		return compat
	}
	if len(record.AuthJSON) == 0 {
		return ""
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal([]byte(record.AuthJSON), &payload); errUnmarshal != nil || payload == nil {
		return ""
	}
	for _, key := range []string{"label", "email", "account", "name"} {
		if value, ok := payload[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	if metadata, ok := payload["metadata"].(map[string]any); ok && metadata != nil {
		if email, ok := metadata["email"].(string); ok {
			if trimmed := strings.TrimSpace(email); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
