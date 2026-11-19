package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// ModelAccessForAPIKey loads catalog permissions without performing dispatch or
// checking transient availability. Policy detail queries are batched across models.
func (r *Repository) ModelAccessForAPIKey(ctx context.Context, apiKey string) (*coreauth.ModelAccess, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("api key is required")
	}
	record, errFirst := r.findAPIKeyForAccess(ctx, db, apiKey)
	if errFirst != nil {
		return nil, errFirst
	}
	baseChannelIDs, errBaseChannels := apiKeyChannelsFromJSON(record.Channels)
	if errBaseChannels != nil {
		return nil, errBaseChannels
	}
	modelDetails, restricted, errModelDetails := allowedModelGroupDetailsForAPIKeyRecord(ctx, db, &record)
	if errModelDetails != nil {
		return nil, errModelDetails
	}
	policy, _, errPolicy := buildModelAccessForAPIKeyParts(ctx, db, baseChannelIDs, modelDetails, restricted)
	return policy, errPolicy
}

// UserModelAccessForClients resolves what the user's own API keys are permitted
// to call, accounting for both model_groups and channel_group bindings.
func (r *Repository) UserModelAccessForClients(
	ctx context.Context,
	userID uint,
	activeAuthIDs []string,
	modelsForClient func(authID string) []string,
) (UserModelAccess, error) {
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
	addModel := func(rawModelID string) {
		modelID := canonicalModelGroupModelID(rawModelID)
		if modelID == "" {
			return
		}
		key := strings.ToLower(modelID)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		allowed = append(allowed, modelID)
	}

	for i := range records {
		baseChannelIDs, errBaseChannels := apiKeyChannelsFromJSON(records[i].Channels)
		if errBaseChannels != nil {
			return UserModelAccess{}, errBaseChannels
		}
		modelDetails, restricted, errDetails := allowedModelGroupDetailsForAPIKeyRecord(ctx, db, &records[i])
		if errDetails != nil {
			return UserModelAccess{}, errDetails
		}
		hasModelChannels, errModelChannels := modelDetailsHaveChannelRestrictions(modelDetails)
		if errModelChannels != nil {
			return UserModelAccess{}, errModelChannels
		}
		if restricted {
			access.ModelGroups = true
		}
		if len(baseChannelIDs) > 0 || hasModelChannels {
			access.ChannelGroups = true
		}

		if modelsForClient == nil {
			if !restricted {
				return UserModelAccess{APIKeyCount: len(records), Restricted: false}, nil
			}
			for _, modelID := range allowedModelIDsFromDetails(modelDetails, restricted) {
				addModel(modelID)
			}
			continue
		}

		if len(baseChannelIDs) == 0 && !restricted {
			return UserModelAccess{APIKeyCount: len(records), Restricted: false}, nil
		}

		if len(baseChannelIDs) == 0 && !hasModelChannels {
			for _, modelID := range allowedModelIDsFromDetails(modelDetails, restricted) {
				addModel(modelID)
			}
			continue
		}

		policy, channelDetails, errPolicy := buildModelAccessForAPIKeyParts(ctx, db, baseChannelIDs, modelDetails, restricted)
		if errPolicy != nil {
			return UserModelAccess{}, errPolicy
		}

		if len(baseChannelIDs) == 0 && restricted {
			// Models without per-model channel restrictions are not bound to specific channels.
			for j := range modelDetails {
				detailChannels, errDetail := ModelGroupDetailChannelIDs(&modelDetails[j])
				if errDetail != nil {
					return UserModelAccess{}, errDetail
				}
				if len(detailChannels) == 0 {
					addModel(modelDetails[j].ModelID)
				}
			}
		}

		candidates := activeAuthIDs
		if candidates == nil {
			candidates = uniqueChannelDetailAuthIDs(channelDetails)
			disabledSet, errDisabled := disabledAuthIDsForCatalog(ctx, db, candidates)
			if errDisabled != nil {
				return UserModelAccess{}, errDisabled
			}
			if len(disabledSet) > 0 {
				filtered := candidates[:0]
				for _, authID := range candidates {
					if _, isDisabled := disabledSet[authID]; !isDisabled {
						filtered = append(filtered, authID)
					}
				}
				candidates = filtered
			}
		}

		for _, authID := range candidates {
			for _, rawModelID := range modelsForClient(authID) {
				if policy.Allows(authID, rawModelID) {
					addModel(rawModelID)
				}
			}
		}
	}

	sort.Strings(allowed)
	access.ModelIDs = allowed
	return access, nil
}

func buildModelAccessForAPIKeyParts(
	ctx context.Context,
	db *gorm.DB,
	baseChannelIDs []uint,
	modelDetails []ModelGroupDetailRecord,
	restricted bool,
) (*coreauth.ModelAccess, []ChannelGroupDetailRecord, error) {
	modelIDs := allowedModelIDsFromDetails(modelDetails, restricted)

	// Group details first so the dispatch resolver visits each detail only once.
	detailsByModel := make(map[string][]ModelGroupDetailRecord)
	for _, detail := range modelDetails {
		modelKey := strings.ToLower(canonicalModelGroupModelID(detail.ModelID))
		detailsByModel[modelKey] = append(detailsByModel[modelKey], detail)
	}
	modelChannels := make(map[string][]uint)
	allChannelIDs := append([]uint(nil), baseChannelIDs...)
	for modelKey, details := range detailsByModel {
		channelIDs, channelsRestricted, errChannels := modelChannelGroupIDsFromDetails(details, modelKey)
		if errChannels != nil {
			return nil, nil, errChannels
		}
		if channelsRestricted {
			modelChannels[modelKey] = channelIDs
			allChannelIDs = append(allChannelIDs, channelIDs...)
		}
	}
	channelDetails, errChannelDetails := channelGroupDetailsForGroups(ctx, db, allChannelIDs)
	if errChannelDetails != nil {
		return nil, nil, errChannelDetails
	}
	// Index once so each model only scans and allocates for its own channels.
	authIDsByChannel := make(map[uint][]string)
	for _, detail := range channelDetails {
		if authID := strings.TrimSpace(detail.AuthID); authID != "" {
			authIDsByChannel[detail.ChannelGroupID] = append(authIDsByChannel[detail.ChannelGroupID], authID)
		}
	}
	authIDsForChannels := func(channelIDs []uint) []string {
		capacity := 0
		for _, channelID := range channelIDs {
			capacity += len(authIDsByChannel[channelID])
		}
		allowed := make([]string, 0, capacity)
		seen := make(map[string]struct{}, capacity)
		for _, channelID := range channelIDs {
			for _, authID := range authIDsByChannel[channelID] {
				if _, exists := seen[authID]; exists {
					continue
				}
				seen[authID] = struct{}{}
				allowed = append(allowed, authID)
			}
		}
		return allowed
	}
	var authIDs []string
	if len(baseChannelIDs) > 0 {
		authIDs = authIDsForChannels(baseChannelIDs)
	}
	modelAuthIDs := make(map[string][]string, len(modelChannels))
	for modelKey, channelIDs := range modelChannels {
		modelAuthIDs[modelKey] = authIDsForChannels(channelIDs)
	}
	return coreauth.NewModelAccess(authIDs, modelIDs, modelAuthIDs), channelDetails, nil
}

func modelDetailsHaveChannelRestrictions(details []ModelGroupDetailRecord) (bool, error) {
	for i := range details {
		channelIDs, errChannels := ModelGroupDetailChannelIDs(&details[i])
		if errChannels != nil {
			return false, errChannels
		}
		if len(channelIDs) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func uniqueChannelDetailAuthIDs(details []ChannelGroupDetailRecord) []string {
	if len(details) == 0 {
		return nil
	}
	out := make([]string, 0, len(details))
	seen := make(map[string]struct{}, len(details))
	for _, detail := range details {
		authID := strings.TrimSpace(detail.AuthID)
		if authID == "" {
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		out = append(out, authID)
	}
	return out
}

func disabledAuthIDsForCatalog(ctx context.Context, db *gorm.DB, authIDs []string) (map[string]struct{}, error) {
	if db == nil || len(authIDs) == 0 {
		return nil, nil
	}
	var disabledRecords []AuthRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Select("id").
		Where("id IN ? AND (disabled = ? OR status = ?)", authIDs, true, coreauth.StatusDisabled).
		Find(&disabledRecords).Error; errFind != nil {
		return nil, errFind
	}
	if len(disabledRecords) == 0 {
		return nil, nil
	}
	out := make(map[string]struct{}, len(disabledRecords))
	for _, rec := range disabledRecords {
		if id := strings.TrimSpace(rec.ID); id != "" {
			out[id] = struct{}{}
		}
	}
	return out, nil
}
