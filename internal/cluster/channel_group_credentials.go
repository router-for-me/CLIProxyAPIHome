package cluster

import (
	"context"
	"slices"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// ChannelGroupCredential contains only fields used to identify a credential in an access scope.
type ChannelGroupCredential struct {
	AuthID      string `json:"auth_id"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider,omitempty"`
	State       string `json:"state,omitempty"`
}

func channelGroupCredentialQuery(db *gorm.DB) *gorm.DB {
	// AuthToRecord writes all three identifiers together. Legacy aliases are ambiguous.
	return db.Model(&AuthRecord{}).
		Where(`uuid <> '' AND uuid = id AND uuid = "index"`).
		Select("uuid", "label", "compat_name", "provider", "disabled", "status", "unavailable", "deleted_at")
}

func channelGroupCredential(record AuthRecord) ChannelGroupCredential {
	name := quotaSafeDisplayLabel(record.Label)
	if name == "" {
		name = quotaSafeDisplayLabel(record.CompatName)
	}
	if name == "" {
		name = record.UUID
	}
	item := ChannelGroupCredential{AuthID: record.UUID, DisplayName: name, Provider: strings.TrimSpace(record.Provider)}
	switch {
	case record.DeletedAt.Valid:
		item.State = "deleted"
	case record.Disabled || record.Status == coreauth.StatusDisabled:
		item.State = "disabled"
	case record.Unavailable:
		item.State = "unavailable"
	}
	return item
}

// ChannelGroupBoundCredentials resolves only the identities referenced by the supplied bindings.
func (r *Repository) ChannelGroupBoundCredentials(ctx context.Context, bindings []ChannelGroupDetailRecord) (map[string]ChannelGroupCredential, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ids := make([]string, 0, len(bindings))
	seen := make(map[string]bool)
	for _, binding := range bindings {
		if binding.AuthID != "" && !seen[binding.AuthID] {
			ids = append(ids, binding.AuthID)
			seen[binding.AuthID] = true
		}
	}
	byID := make(map[string]ChannelGroupCredential, len(ids))
	for batch := range slices.Chunk(ids, 500) {
		var records []AuthRecord
		query := channelGroupCredentialQuery(db.WithContext(contextOrBackground(ctx)).Unscoped())
		if errFind := query.Where("uuid IN ?", batch).Find(&records).Error; errFind != nil {
			return nil, errFind
		}
		for _, record := range records {
			byID[record.UUID] = channelGroupCredential(record)
		}
	}
	return byID, nil
}

// ListChannelGroupCredentialCandidates lists current canonical identities from all credential sources.
func (r *Repository) ListChannelGroupCredentialCandidates(ctx context.Context) ([]ChannelGroupCredential, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []AuthRecord
	query := channelGroupCredentialQuery(db.WithContext(contextOrBackground(ctx)))
	if errFind := query.Order("uuid ASC").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	items := make([]ChannelGroupCredential, 0, len(records))
	for _, record := range records {
		items = append(items, channelGroupCredential(record))
	}
	return items, nil
}
