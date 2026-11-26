package auth

import (
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const maxModelAliasHints = 3

type modelAliasHintTarget struct {
	authID string
	alias  string
}

type modelAliasHintEntry struct {
	upstream string
	alias    string
}

// modelAliasHintIndex is updated when Home publishes a credential's models.
// Error paths never decode credential metadata or scan unrelated credentials.
type modelAliasHintIndex struct {
	mu         sync.RWMutex
	byAuth     map[string][]modelAliasHintEntry
	byUpstream map[string]map[modelAliasHintTarget]struct{}
}

func (i *modelAliasHintIndex) replace(authID string, entries []modelAliasHintEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, entry := range i.byAuth[authID] {
		targets := i.byUpstream[entry.upstream]
		delete(targets, modelAliasHintTarget{authID: authID, alias: entry.alias})
		if len(targets) == 0 {
			delete(i.byUpstream, entry.upstream)
		}
	}
	delete(i.byAuth, authID)
	if len(entries) == 0 {
		return
	}
	if i.byAuth == nil {
		i.byAuth = make(map[string][]modelAliasHintEntry)
		i.byUpstream = make(map[string]map[modelAliasHintTarget]struct{})
	}
	i.byAuth[authID] = entries
	for _, entry := range entries {
		if i.byUpstream[entry.upstream] == nil {
			i.byUpstream[entry.upstream] = make(map[modelAliasHintTarget]struct{})
		}
		i.byUpstream[entry.upstream][modelAliasHintTarget{authID: authID, alias: entry.alias}] = struct{}{}
	}
}

func (i *modelAliasHintIndex) lookup(requestedModel string) []modelAliasHintTarget {
	_, candidates := modelAliasLookupCandidates(requestedModel)
	i.mu.RLock()
	defer i.mu.RUnlock()
	var targets []modelAliasHintTarget
	for _, candidate := range candidates {
		for target := range i.byUpstream[strings.ToLower(candidate)] {
			targets = append(targets, target)
		}
	}
	return targets
}

// SetModelAliasHints indexes the effective upstream models behind published
// public model IDs, including OAuth aliases and credential prefixes. Passing
// no public models removes this credential's hints.
func (m *Manager) SetModelAliasHints(auth *Auth, publicModels []string) {
	if m == nil || auth == nil {
		return
	}
	var entries []modelAliasHintEntry
	if len(publicModels) > 0 && !auth.Disabled && auth.Status != StatusDisabled {
		// Decode metadata once for this publication pass. The temporary snapshot
		// is never installed as credential state or persisted.
		aliasAuth := auth
		if raw := auth.Metadata[homeConfigModelsMetadataKey]; raw != nil {
			snapshot := *auth
			snapshot.Metadata = maps.Clone(auth.Metadata)
			snapshot.Metadata[homeConfigModelsMetadataKey] = modelAliasEntriesFromMetadata(raw)
			aliasAuth = &snapshot
		}
		seen := make(map[modelAliasHintEntry]struct{})
		for _, publicModel := range publicModels {
			alias := strings.TrimSpace(publicModel)
			upstream := m.ResolveExecutionModel(aliasAuth, alias)
			if alias == "" || upstream == "" || strings.EqualFold(upstream, rewriteModelForAuth(alias, auth)) {
				continue
			}
			upstreamNames := []string{upstream}
			if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
				upstreamNames = append(upstreamNames, prefix+"/"+upstream)
			}
			for _, name := range upstreamNames {
				entry := modelAliasHintEntry{upstream: strings.ToLower(name), alias: alias}
				if _, exists := seen[entry]; !exists {
					seen[entry] = struct{}{}
					entries = append(entries, entry)
				}
			}
		}
	}
	m.modelAliasHints.replace(auth.ID, entries)
}

// AliasesForUpstreamModel returns only published aliases visible to the caller.
// Permissions are checked before deduplication and truncation, using each
// target alias's model-specific channel scope rather than the requested ID's.
func (m *Manager) AliasesForUpstreamModel(requestedModel string, access *ModelAccess) []string {
	if m == nil {
		return nil
	}
	targets := m.modelAliasHints.lookup(requestedModel)
	if len(targets) == 0 {
		return nil
	}
	_, candidates := modelAliasLookupCandidates(requestedModel)
	aliasesByKey := make(map[string]string)
	modelRegistry := registry.GetGlobalRegistry()
	m.mu.RLock()
	for _, target := range targets {
		auth := m.auths[target.authID]
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled ||
			!access.Allows(target.authID, target.alias) || matchesAnyCandidate(target.alias, candidates) ||
			!modelRegistry.ClientSupportsModel(target.authID, target.alias) {
			continue
		}
		key := strings.ToLower(target.alias)
		if previous, exists := aliasesByKey[key]; !exists || target.alias < previous {
			aliasesByKey[key] = target.alias
		}
	}
	m.mu.RUnlock()
	var aliases []string
	for _, alias := range aliasesByKey {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	if len(aliases) > maxModelAliasHints {
		aliases = aliases[:maxModelAliasHints]
	}
	return aliases
}

// ModelAliasHint formats the public model IDs for a lookup error message.
func ModelAliasHint(requestedModel string, aliases []string) string {
	if len(aliases) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		quoted = append(quoted, strconv.Quote(alias))
	}
	noun := "alias"
	if len(quoted) > 1 {
		noun = "aliases"
	}
	return "model " + strconv.Quote(strings.TrimSpace(requestedModel)) + " is configured under the " + noun + " " +
		strings.Join(quoted, ", ") + "; request that model ID or clear the alias"
}

func matchesAnyCandidate(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}
