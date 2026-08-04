package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type modelAliasEntry interface {
	GetName() string
	GetAlias() string
}

type oauthModelAliasEntry struct {
	upstreamModel string
	alias         string
	forceMapping  bool
}

type oauthModelAliasResult struct {
	upstreamModel string
	originalAlias string
	forceMapping  bool
}

type oauthModelAliasTable struct {
	// reverse maps channel -> alias (lower) -> upstream model and response mapping metadata.
	reverse map[string]map[string]oauthModelAliasEntry
}

// compileOAuthModelAliasTable compiles an o auth model alias table.
func compileOAuthModelAliasTable(aliases map[string][]internalconfig.OAuthModelAlias) *oauthModelAliasTable {
	// Resolve credential context before calling upstream OAuth services.
	if len(aliases) == 0 {
		return &oauthModelAliasTable{}
	}
	out := &oauthModelAliasTable{
		reverse: make(map[string]map[string]oauthModelAliasEntry, len(aliases)),
	}
	for rawChannel, entries := range aliases {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(entries) == 0 {
			continue
		}
		rev := make(map[string]oauthModelAliasEntry, len(entries))
		for _, entry := range entries {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, exists := rev[aliasKey]; exists {
				continue
			}
			rev[aliasKey] = oauthModelAliasEntry{
				upstreamModel: name,
				alias:         alias,
				forceMapping:  entry.ForceMapping,
			}
		}
		if len(rev) > 0 {
			out.reverse[channel] = rev
		}
	}
	if len(out.reverse) == 0 {
		out.reverse = nil
	}
	return out
}

// SetOAuthModelAlias updates the OAuth model name alias table used during execution.
// The alias is applied per-auth channel to resolve the upstream model name while keeping the
// client-visible model name unchanged for translation/response formatting.
func (m *Manager) SetOAuthModelAlias(aliases map[string][]internalconfig.OAuthModelAlias) {
	if m == nil {
		return
	}
	table := compileOAuthModelAliasTable(aliases)
	// atomic.Value requires non-nil store values.
	if table == nil {
		table = &oauthModelAliasTable{}
	}
	m.oauthModelAlias.Store(table)
	if m.scheduler != nil {
		m.scheduler.resetModelShards()
	}
}

// applyOAuthModelAlias resolves the upstream model from OAuth model alias.
// If an alias exists, the returned model is the upstream model.
func (m *Manager) applyOAuthModelAlias(auth *Auth, requestedModel string) string {
	upstreamModel := m.resolveOAuthUpstreamModel(auth, requestedModel)
	if upstreamModel == "" {
		return requestedModel
	}
	return upstreamModel
}

// modelAliasLookupCandidates handles a model alias lookup candidates.
func modelAliasLookupCandidates(requestedModel string) (suffixResult, []string) {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return suffixResult{}, nil
	}
	requestResult := parseModelSuffix(requestedModel)
	base := requestResult.ModelName
	if base == "" {
		base = requestedModel
	}
	candidates := []string{base}
	if base != requestedModel {
		candidates = append(candidates, requestedModel)
	}
	return requestResult, candidates
}

// preserveResolvedModelSuffix preserves a resolved model suffix.
func preserveResolvedModelSuffix(resolved string, requestResult suffixResult) string {
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return ""
	}
	if parseModelSuffix(resolved).HasSuffix {
		return resolved
	}
	if requestResult.HasSuffix && requestResult.RawSuffix != "" {
		return resolved + "(" + requestResult.RawSuffix + ")"
	}
	return resolved
}

// resolveModelAliasPoolFromConfigModels derives resolve model alias pool from config models.
func resolveModelAliasPoolFromConfigModels(requestedModel string, models []modelAliasEntry) []string {
	// Normalize source data before building the derived payload.
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	if len(models) == 0 {
		return nil
	}

	requestResult, candidates := modelAliasLookupCandidates(requestedModel)
	if len(candidates) == 0 {
		return nil
	}

	out := make([]string, 0)
	seen := make(map[string]struct{})
	for i := range models {
		name := strings.TrimSpace(models[i].GetName())
		alias := strings.TrimSpace(models[i].GetAlias())
		for _, candidate := range candidates {
			if candidate == "" || alias == "" || !strings.EqualFold(alias, candidate) {
				continue
			}
			resolved := candidate
			if name != "" {
				resolved = name
			}
			resolved = preserveResolvedModelSuffix(resolved, requestResult)
			key := strings.ToLower(strings.TrimSpace(resolved))
			if key == "" {
				break
			}
			if _, exists := seen[key]; exists {
				break
			}
			seen[key] = struct{}{}
			out = append(out, resolved)
			break
		}
	}
	if len(out) > 0 {
		return out
	}

	for i := range models {
		name := strings.TrimSpace(models[i].GetName())
		for _, candidate := range candidates {
			if candidate == "" || name == "" || !strings.EqualFold(name, candidate) {
				continue
			}
			return []string{preserveResolvedModelSuffix(name, requestResult)}
		}
	}
	return nil
}

// resolveModelAliasFromConfigModels derives resolve model alias from config models.
func resolveModelAliasFromConfigModels(requestedModel string, models []modelAliasEntry) string {
	resolved := resolveModelAliasPoolFromConfigModels(requestedModel, models)
	if len(resolved) > 0 {
		return resolved[0]
	}
	return ""
}

// resolveOAuthUpstreamModel resolves the upstream model name from OAuth model alias.
// If an alias exists, returns the original (upstream) model name that corresponds
// to the requested alias.
//
// If the requested model contains a thinking suffix (e.g., "gemini-2.5-pro(8192)"),
// the suffix is preserved in the returned model name. However, if the alias's
// original name already contains a suffix, the config suffix takes priority.
func (m *Manager) resolveOAuthUpstreamModel(auth *Auth, requestedModel string) string {
	return m.resolveOAuthModelAlias(auth, requestedModel).upstreamModel
}

func (m *Manager) resolveOAuthModelAlias(auth *Auth, requestedModel string) oauthModelAliasResult {
	return resolveModelAliasFromOAuthTable(m, auth, requestedModel, modelAliasChannel(auth))
}

// resolveModelAliasFromOAuthTable resolves an upstream model and response mapping metadata.
func resolveModelAliasFromOAuthTable(m *Manager, auth *Auth, requestedModel, channel string) oauthModelAliasResult {
	if m == nil || auth == nil {
		return oauthModelAliasResult{}
	}
	if channel == "" {
		return oauthModelAliasResult{}
	}

	// Extract suffix from requested model.
	requestResult := parseModelSuffix(requestedModel)
	baseModel := requestResult.ModelName

	// Candidate keys to match: base model and raw input (handles suffix-parsing edge cases).
	candidates := []string{baseModel}
	if baseModel != requestedModel {
		candidates = append(candidates, requestedModel)
	}

	raw := m.oauthModelAlias.Load()
	table, _ := raw.(*oauthModelAliasTable)
	if table == nil || table.reverse == nil {
		return oauthModelAliasResult{}
	}
	rev := table.reverse[channel]
	if rev == nil {
		return oauthModelAliasResult{}
	}

	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		entry, exists := rev[key]
		if !exists {
			continue
		}
		original := strings.TrimSpace(entry.upstreamModel)
		if original == "" {
			continue
		}
		if strings.EqualFold(original, baseModel) {
			return oauthModelAliasResult{}
		}

		resolved := original
		// If config already has suffix, it takes priority.
		if !parseModelSuffix(original).HasSuffix && requestResult.HasSuffix && requestResult.RawSuffix != "" {
			resolved = original + "(" + requestResult.RawSuffix + ")"
		}
		result := oauthModelAliasResult{upstreamModel: resolved}
		if entry.forceMapping {
			result.forceMapping = true
			result.originalAlias = strings.TrimSpace(entry.alias)
		}
		return result
	}

	return oauthModelAliasResult{}
}

// modelAliasChannel extracts the OAuth model alias channel from an Auth object.
// It determines the provider and auth kind from the Auth's attributes and delegates
// to OAuthModelAliasChannel for the actual channel resolution.
func modelAliasChannel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	authKind := ""
	if auth.Attributes != nil {
		authKind = strings.ToLower(strings.TrimSpace(auth.Attributes["auth_kind"]))
	}
	if authKind == "" {
		if kind, _ := auth.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	return OAuthModelAliasChannel(provider, authKind)
}

// OAuthModelAliasChannel returns the OAuth model alias channel name for a given provider
// and auth kind. Returns empty string if the provider/authKind combination doesn't support
// OAuth model alias (e.g., API key authentication).
//
// Supported channels: vertex, antigravity, claude, codex, kimi, xai.
func OAuthModelAliasChannel(provider, authKind string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	authKind = strings.ToLower(strings.TrimSpace(authKind))
	authKind = strings.ReplaceAll(authKind, "-", "_")
	if authKind == "api_key" || authKind == "apikey" {
		return ""
	}
	switch provider {
	case "gemini":
		// gemini provider uses gemini-api-key config, not oauth-model-alias.
		return ""
	case "vertex":
		return "vertex"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "antigravity", "kimi", "xai":
		return provider
	default:
		return ""
	}
}
