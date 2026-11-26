package home

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityMaxCatalogModels = 1024
	antigravityMaxModelIDBytes  = 256
)

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
	if antigravityProbeHTTP11 {
		if transport == nil {
			if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
				transport = base.Clone()
			}
		}
		if transport != nil {
			transport.Protocols = new(http.Protocols)
			transport.Protocols.SetHTTP1(true)
			// A cloned HTTP/2 transport can already advertise ALPN.
			if transport.TLSClientConfig != nil {
				transport.TLSClientConfig.NextProtos = nil
			}
		}
	}
	if transport != nil {
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	doc := ojson.NewDocString("{}")
	doc.Set("project", antigravityProjectID(auth))
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURLs[0], "/")+antigravityModelsPath, bytes.NewReader(doc.Bytes()))
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
	if !ojson.ValidBytes(body) {
		return antigravityModelCapabilityHints{}, false
	}
	root := ojson.ParseBytes(body)
	if !root.IsObject() {
		return antigravityModelCapabilityHints{}, false
	}

	var hints antigravityModelCapabilityHints
	var webSearchItems []ojson.Result
	hints.WebSearchModelIDs = make(map[string]struct{})
	if webSearch := root.Get("webSearchModelIds"); webSearch.Exists() && webSearch.Type != ojson.Null {
		webSearchItems = webSearch.Array()
		if !webSearch.IsArray() || len(webSearchItems) > antigravityMaxCatalogModels {
			return antigravityModelCapabilityHints{}, false
		}
		for _, item := range webSearchItems {
			if item.Type != ojson.String {
				return antigravityModelCapabilityHints{}, false
			}
			if id := normalizeAntigravityFetchedModelID(item.Str); id != "" {
				hints.WebSearchModelIDs[id] = struct{}{}
			}
		}
	}

	if catalog := root.Get("models"); catalog.Exists() && catalog.Type != ojson.Null {
		items := catalog.Map()
		if !catalog.IsObject() || len(items) > antigravityMaxCatalogModels {
			return antigravityModelCapabilityHints{}, false
		}
		hints.ModelIDs = make(map[string]struct{}, len(items))
		hints.modelEnums = make(map[string]string)
		for id, item := range items {
			if normalized := normalizeAntigravityFetchedModelID(id); normalized != "" {
				hints.ModelIDs[normalized] = struct{}{}
			}
			if id == "" || len(id) > antigravityMaxModelIDBytes {
				continue
			}
			value := item.Get("model")
			if value.Type == ojson.String && len(value.Str) <= antigravityMaxModelIDBytes && strings.TrimSpace(value.Str) != "" {
				hints.modelEnums[strings.Clone(id)] = strings.Clone(value.Str)
			}
		}
		hints.thinkingGroups = parseAntigravityThinkingGroups(root.Get("groupedModels"), items)
		hints.thinkingWebSearch = antigravityThinkingWebSearch(hints.thinkingGroups, webSearchItems)
	}
	return hints, true
}

func antigravityThinkingWebSearch(groups map[string]map[string]int, webSearchItems []ojson.Result) map[string]bool {
	if len(groups) == 0 || len(webSearchItems) == 0 {
		return nil
	}
	// Match exact upstream variant IDs rather than the normalized registration hints.
	declared := make(map[string]struct{}, len(webSearchItems))
	for _, item := range webSearchItems {
		declared[item.Str] = struct{}{}
	}
	var supported map[string]bool
	for group, levels := range groups {
		for level := range levels {
			id := group + "-" + level
			if _, ok := declared[id]; ok {
				if supported == nil {
					supported = make(map[string]bool)
				}
				supported[id] = true
			}
		}
	}
	return supported
}

func parseAntigravityThinkingGroups(groupedModels ojson.Result, models map[string]ojson.Result) map[string]map[string]int {
	if !groupedModels.IsObject() || len(models) == 0 {
		return nil
	}
	var groups map[string]map[string]int
	groupedModels.ForEach(func(key, entry ojson.Result) bool {
		group := key.Str
		if group == "" || len(group) >= antigravityMaxModelIDBytes || !entry.IsObject() {
			return true
		}
		declared := entry.Get("supportedThinkingLevels")
		if !declared.IsArray() {
			return true
		}
		var levels map[string]int
		declared.ForEach(func(_, item ojson.Result) bool {
			if !item.IsObject() {
				return true
			}
			suffix := item.Get("thinkingSuffix")
			if suffix.Type != ojson.String || len(group)+1+len(suffix.Str) > antigravityMaxModelIDBytes || !validAntigravityThinkingSuffix(suffix.Str) {
				return true
			}
			level := strings.ToLower(suffix.Str)
			member := models[group+"-"+level]
			if !member.IsObject() {
				return true
			}
			// Zero records a declared member that is not eligible as a switch target.
			limit := 0
			if value := member.Get("maxOutputTokens"); value.Type == ojson.Number {
				if n, errLimit := strconv.Atoi(value.Raw); errLimit == nil && n > 0 {
					limit = n
				}
			}
			if levels == nil {
				levels = make(map[string]int)
			}
			levels[strings.Clone(level)] = limit
			return true
		})
		if len(levels) > 0 {
			if groups == nil {
				groups = make(map[string]map[string]int)
			}
			groups[strings.Clone(group)] = levels
		}
		return true
	})
	return groups
}

func validAntigravityThinkingSuffix(suffix string) bool {
	lastAlnum := false
	for i := range len(suffix) {
		switch ch := suffix[i]; {
		case ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9':
			lastAlnum = true
		case ch == '-' && lastAlnum:
			lastAlnum = false
		default:
			return false
		}
	}
	return lastAlnum
}
