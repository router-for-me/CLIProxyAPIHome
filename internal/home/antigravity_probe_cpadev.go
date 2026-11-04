//go:build cpadev

package home

import (
	"context"

	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

const (
	antigravityModelBaseURLDaily     = cpasdkauth.AntigravityModelBaseURLDaily
	antigravityModelsPath            = cpasdkauth.AntigravityModelsPath
	antigravityPersistedHintsMetaKey = cpasdkauth.AntigravityPersistedHintsMetaKey
)

type antigravityProbeStatus = cpasdkauth.AntigravityProbeStatus

const (
	antigravityProbeStatusSuccess        = cpasdkauth.AntigravityProbeStatusSuccess
	antigravityProbeStatusAuthError      = cpasdkauth.AntigravityProbeStatusAuthError
	antigravityProbeStatusTransientError = cpasdkauth.AntigravityProbeStatusTransientError
)

func buildPersistedAntigravityCapabilityHintsValue(project, endpoint string, hints antigravityModelCapabilityHints) map[string]any {
	return cpasdkauth.BuildPersistedAntigravityCapabilityHints(project, endpoint, cpasdkauth.AntigravityModelCapabilityHints{
		ModelIDs:          hints.ModelIDs,
		WebSearchModelIDs: hints.WebSearchModelIDs,
	})
}

func parsePersistedAntigravityCapabilityHints(metadata map[string]any, expectedProject, expectedEndpoint string) (antigravityModelCapabilityHints, bool) {
	hints, ok := cpasdkauth.ParsePersistedAntigravityCapabilityHints(metadata, expectedProject, expectedEndpoint)
	if !ok {
		return antigravityModelCapabilityHints{}, false
	}
	return antigravityModelCapabilityHints{
		ModelIDs:          hints.ModelIDs,
		WebSearchModelIDs: hints.WebSearchModelIDs,
	}, true
}

func (r *Runtime) probeAntigravityModelCapabilityHints(ctx context.Context, auth *coreauth.Auth, baseURLs []string, proxyURL, accessToken, userAgent string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	hints, status := cpasdkauth.FetchAntigravityAvailableModels(ctx, cpasdkauth.AntigravityModelProbeRequest{
		BaseURLs:    baseURLs,
		ProxyURL:    proxyURL,
		AccessToken: accessToken,
		ProjectID:   antigravityProjectID(auth),
		UserAgent:   userAgent,
	})
	return antigravityModelCapabilityHints{
		ModelIDs:          hints.ModelIDs,
		WebSearchModelIDs: hints.WebSearchModelIDs,
	}, status
}

func parseAntigravityModelCapabilityHints(body []byte) (antigravityModelCapabilityHints, bool) {
	hints, ok := cpasdkauth.ParseAntigravityModelCapabilityHints(body)
	if !ok {
		return antigravityModelCapabilityHints{}, false
	}
	return antigravityModelCapabilityHints{
		ModelIDs:          hints.ModelIDs,
		WebSearchModelIDs: hints.WebSearchModelIDs,
	}, true
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return cpasdkauth.NormalizeAntigravityModelID(modelID)
}

func defaultAntigravityModelUserAgent(configured string) string {
	return cpasdkauth.AntigravityRequestUserAgent(configured)
}
