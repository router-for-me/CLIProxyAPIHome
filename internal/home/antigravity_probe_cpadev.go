//go:build cpadev

package home

import cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"

const (
	antigravityModelBaseURLDaily     = cpasdkauth.AntigravityModelBaseURLDaily
	antigravityModelsPath            = cpasdkauth.AntigravityModelsPath
	antigravityPersistedHintsMetaKey = cpasdkauth.AntigravityPersistedHintsMetaKey
	antigravityProbeHTTP11           = true
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

func normalizeAntigravityFetchedModelID(modelID string) string {
	return cpasdkauth.NormalizeAntigravityModelID(modelID)
}

func defaultAntigravityModelUserAgent(configured string) string {
	return cpasdkauth.AntigravityRequestUserAgent(configured)
}
