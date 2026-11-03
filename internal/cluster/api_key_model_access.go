package cluster

import (
	"context"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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
			return nil, errChannels
		}
		if channelsRestricted {
			modelChannels[modelKey] = channelIDs
			allChannelIDs = append(allChannelIDs, channelIDs...)
		}
	}
	channelDetails, errChannelDetails := channelGroupDetailsForGroups(ctx, db, allChannelIDs)
	if errChannelDetails != nil {
		return nil, errChannelDetails
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
	return coreauth.NewModelAccess(authIDs, modelIDs, modelAuthIDs), nil
}
