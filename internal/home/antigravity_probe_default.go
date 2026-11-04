//go:build !cpadev

package home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityModelBaseURLDaily     = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelsPath            = "/v1internal:fetchAvailableModels"
	antigravityPersistedHintsMetaKey = "antigravity_capability_hints"
	antigravityMaxCatalogModels      = 1024
	antigravityMaxModelIDBytes       = 256
	antigravityHubFallbackVersion    = "2.29.0"
	antigravityHubPlatform           = "os_type=darwin; arch=arm64"
)

var antigravityHubChangelists = map[string]string{
	"2.23.0": "998057262",
	"2.24.0": "1000335682",
	"2.29.0": "1013060670",
}

type antigravityProbeStatus int

const (
	antigravityProbeStatusSuccess antigravityProbeStatus = iota
	antigravityProbeStatusAuthError
	antigravityProbeStatusTransientError
)

type antigravityFetchAvailableModelsResponse struct {
	WebSearchModelIDs []string                   `json:"webSearchModelIds"`
	Models            map[string]json.RawMessage `json:"models"`
}

func defaultAntigravityModelUserAgent(configured string) string {
	ua := strings.TrimSpace(cpasdkauth.AntigravityRequestUserAgent(configured))
	if strings.TrimSpace(configured) != "" {
		return ua
	}
	version := extractAntigravityVersion(ua)
	if compareAntigravitySemver(version, antigravityHubFallbackVersion) < 0 {
		version = antigravityHubFallbackVersion
	}
	metadata := "aidev_client; " + antigravityHubPlatform
	if cl := antigravityHubChangelists[version]; cl != "" {
		metadata += "; cl=" + cl
	}
	return fmt.Sprintf("antigravity/hub/%s (%s)", version, metadata)
}

func extractAntigravityVersion(ua string) string {
	ua = strings.TrimSpace(ua)
	lower := strings.ToLower(ua)
	for _, prefix := range []string{"antigravity/hub/", "antigravity/"} {
		if strings.HasPrefix(lower, prefix) {
			rest := ua[len(prefix):]
			if idx := strings.IndexAny(rest, " 	"); idx >= 0 {
				rest = rest[:idx]
			}
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func compareAntigravitySemver(a, b string) int {
	pa := strings.Split(strings.TrimSpace(a), ".")
	pb := strings.Split(strings.TrimSpace(b), ".")
	if len(pa) != 3 || len(pb) != 3 {
		return -1
	}
	for i := 0; i < 3; i++ {
		var na, nb int
		for _, ch := range pa[i] {
			if ch < '0' || ch > '9' {
				return -1
			}
			na = na*10 + int(ch-'0')
		}
		for _, ch := range pb[i] {
			if ch < '0' || ch > '9' {
				return -1
			}
			nb = nb*10 + int(ch-'0')
		}
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
	}
	return 0
}

func sanitizeAntigravityPersistedEndpoint(endpoint string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if trimmed == "" {
		return ""
	}
	if parsed, errParse := url.Parse(trimmed); errParse == nil && parsed.User != nil {
		parsed.User = nil
		return strings.TrimRight(parsed.String(), "/")
	}
	return trimmed
}

func buildPersistedAntigravityCapabilityHintsValue(project, endpoint string, hints antigravityModelCapabilityHints) map[string]any {
	modelIDs := make([]string, 0, len(hints.ModelIDs))
	for id := range hints.ModelIDs {
		if normalized := normalizeAntigravityFetchedModelID(id); normalized != "" {
			modelIDs = append(modelIDs, normalized)
		}
	}
	slices.Sort(modelIDs)
	modelItems := make([]any, len(modelIDs))
	for i, id := range modelIDs {
		modelItems[i] = id
	}
	value := map[string]any{
		"model_ids": modelItems,
	}
	if project = strings.TrimSpace(project); project != "" {
		value["project_id"] = project
	}
	if endpoint = sanitizeAntigravityPersistedEndpoint(endpoint); endpoint != "" {
		value["endpoint"] = endpoint
	}
	if len(hints.WebSearchModelIDs) > 0 {
		webSearchIDs := make([]string, 0, len(hints.WebSearchModelIDs))
		for id := range hints.WebSearchModelIDs {
			if normalized := normalizeAntigravityFetchedModelID(id); normalized != "" {
				webSearchIDs = append(webSearchIDs, normalized)
			}
		}
		slices.Sort(webSearchIDs)
		if len(webSearchIDs) > 0 {
			webSearchItems := make([]any, len(webSearchIDs))
			for i, id := range webSearchIDs {
				webSearchItems[i] = id
			}
			value["web_search_model_ids"] = webSearchItems
		}
	}
	return value
}

func parsePersistedAntigravityCapabilityHints(metadata map[string]any, expectedProject, expectedEndpoint string) (antigravityModelCapabilityHints, bool) {
	if metadata == nil {
		return antigravityModelCapabilityHints{}, false
	}
	raw, ok := metadata[antigravityPersistedHintsMetaKey]
	if !ok || raw == nil {
		return antigravityModelCapabilityHints{}, false
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return antigravityModelCapabilityHints{}, false
	}
	persistedProject, _ := rawMap["project_id"].(string)
	persistedEndpoint, _ := rawMap["endpoint"].(string)
	if strings.TrimSpace(persistedProject) != strings.TrimSpace(expectedProject) {
		return antigravityModelCapabilityHints{}, false
	}
	if sanitizeAntigravityPersistedEndpoint(persistedEndpoint) != sanitizeAntigravityPersistedEndpoint(expectedEndpoint) {
		return antigravityModelCapabilityHints{}, false
	}
	rawModels, hasModels := rawMap["model_ids"]
	if !hasModels || rawModels == nil {
		return antigravityModelCapabilityHints{}, false
	}
	modelIDs, ok := parsePersistedAntigravityModelSet(rawModels)
	if !ok {
		return antigravityModelCapabilityHints{}, false
	}
	webSearchModels := make(map[string]struct{})
	if rawWebSearch, hasWebSearch := rawMap["web_search_model_ids"]; hasWebSearch && rawWebSearch != nil {
		parsedWebSearch, okWebSearch := parsePersistedAntigravityModelSet(rawWebSearch)
		if !okWebSearch {
			return antigravityModelCapabilityHints{}, false
		}
		webSearchModels = parsedWebSearch
	}
	return antigravityModelCapabilityHints{
		ModelIDs:          modelIDs,
		WebSearchModelIDs: webSearchModels,
	}, true
}

func parsePersistedAntigravityModelSet(raw any) (map[string]struct{}, bool) {
	switch items := raw.(type) {
	case []any:
		set := make(map[string]struct{}, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			if normalized := normalizeAntigravityFetchedModelID(s); normalized != "" {
				set[normalized] = struct{}{}
			}
		}
		return set, true
	case []string:
		set := make(map[string]struct{}, len(items))
		for _, s := range items {
			if normalized := normalizeAntigravityFetchedModelID(s); normalized != "" {
				set[normalized] = struct{}{}
			}
		}
		return set, true
	default:
		return nil, false
	}
}

func (r *Runtime) probeAntigravityModelCapabilityHints(ctx context.Context, auth *coreauth.Auth, baseURLs []string, proxyURL, accessToken, userAgent string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	if len(baseURLs) == 0 {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	transport, _, errProxy := proxyutil.BuildHTTPTransport(proxyURL)
	if errProxy != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	if transport != nil {
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	project := antigravityProjectID(auth)
	body, errMarshal := json.Marshal(map[string]string{"project": project})
	if errMarshal != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURLs[0], "/")+antigravityModelsPath, bytes.NewReader(body))
	if errReq != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", userAgent)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("antigravity model fetch: response close failed")
		}
	}()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusAuthError
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	const maxCatalogBytes = 8 << 20
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if errRead != nil || len(data) > maxCatalogBytes {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	hints, ok := parseAntigravityModelCapabilityHints(data)
	if !ok {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	return hints, antigravityProbeStatusSuccess
}

func parseAntigravityModelCapabilityHints(body []byte) (antigravityModelCapabilityHints, bool) {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}, false
	}
	if len(parsed.Models) > antigravityMaxCatalogModels || len(parsed.WebSearchModelIDs) > antigravityMaxCatalogModels {
		return antigravityModelCapabilityHints{}, false
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}
	var modelIDs map[string]struct{}
	if parsed.Models != nil {
		modelIDs = make(map[string]struct{}, len(parsed.Models))
		for id := range parsed.Models {
			if normalized := normalizeAntigravityFetchedModelID(id); normalized != "" {
				modelIDs[normalized] = struct{}{}
			}
		}
	}
	return antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels, ModelIDs: modelIDs}, true
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	trimmed := strings.ToLower(strings.TrimSpace(modelID))
	if len(trimmed) > antigravityMaxModelIDBytes {
		return ""
	}
	return trimmed
}
