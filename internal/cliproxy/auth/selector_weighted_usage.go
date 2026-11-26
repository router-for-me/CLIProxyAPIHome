package auth

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// maxWeightedUsageEvents bounds process-local replay protection, independently
// of the weighted pool cache. Credits are approximate and are not durable.
const maxWeightedUsageEvents = 65536

// UsesWeightedRouting reports whether built-in weighted routing owns accounting,
// including an affinity wrapper but excluding an active scheduler plugin.
func (m *Manager) UsesWeightedRouting() bool {
	if m == nil || m.hasPluginScheduler() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.weightedSelectorLocked() != nil
}

func (m *Manager) weightedSelectorLocked() *WeightedRoundRobinSelector {
	weighted, _ := m.selector.(*WeightedRoundRobinSelector)
	if affinity, ok := m.selector.(*SessionAffinitySelector); ok && affinity != nil {
		weighted, _ = affinity.fallback.(*WeightedRoundRobinSelector)
	}
	return weighted
}

// CanAccountReusedSelection reports whether a reused execution can change
// weighted credits, before the caller queries external API-key permissions.
func (m *Manager) CanAccountReusedSelection(ctx context.Context, model, authIndex, policy, eventID string) bool {
	modelKey := canonicalModelKey(model)
	if !m.UsesWeightedRouting() || strings.TrimSpace(eventID) == "" || modelKey == "" {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	weighted := m.weightedSelectorLocked()
	selected := m.indexAuth[strings.TrimSpace(authIndex)]
	if weighted == nil || selected == nil || selected.Disabled || authWeight(selected) <= 0 || !credentialPolicyAllows(policy, selected) {
		return false
	}
	if strings.TrimSpace(selected.Provider) == "" || weighted.hasSeenUsage(eventID) {
		return false
	}
	registryRef := registry.GetGlobalRegistry()
	if registryRef == nil || !registryRef.ClientSupportsModel(selected.ID, modelKey) {
		return false
	}
	now := time.Now()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.ID == selected.ID || candidate.Disabled || authWeight(candidate) <= 0 || authPriority(candidate) != authPriority(selected) {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if provider == "" || !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		if _, available, _, _ := m.buildDispatchCandidate(candidate, provider, model, policy, now); available {
			return true
		}
	}
	return false
}

// AccountReusedSelection charges an execution that reused a CPA-held selection.
// It neither selects a credential nor acquires concurrency or changes bindings.
// The pool uses current Home availability and the same API-key/model/policy
// restrictions as dispatch, within the retained credential's priority tier.
func (m *Manager) AccountReusedSelection(ctx context.Context, providers []string, model, authIndex, eventID string, opts Options) {
	modelKey := canonicalModelKey(model)
	if !m.UsesWeightedRouting() || strings.TrimSpace(eventID) == "" || modelKey == "" {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	restrictProviders := providers != nil
	if restrictProviders {
		providers = normalizeProviderKeys(providers)
		if len(providers) == 0 {
			return
		}
	}
	if !modelAllowedByID(modelKey, allowedModelIDsFromOptions(opts)) {
		return
	}
	allowed := unpinnedAllowedAuthIDsFromOptions(opts)
	policy := credentialPolicyFromOptions(opts)
	m.mu.RLock()
	defer m.mu.RUnlock()
	weighted := m.weightedSelectorLocked()
	selected := m.indexAuth[strings.TrimSpace(authIndex)]
	if weighted == nil || selected == nil || selected.Disabled || authWeight(selected) <= 0 || !authAllowedByID(selected.ID, allowed) || !credentialPolicyAllows(policy, selected) {
		return
	}
	selectedProvider := strings.ToLower(strings.TrimSpace(selected.Provider))
	if selectedProvider == "" || (restrictProviders && !containsProvider(providers, selectedProvider)) {
		return
	}
	now := time.Now()
	registryRef := registry.GetGlobalRegistry()
	if registryRef == nil || !registryRef.ClientSupportsModel(selected.ID, modelKey) {
		return
	}
	routeProvider := selectedProvider
	if restrictProviders && len(providers) > 1 {
		routeProvider = "mixed"
	}
	var pool []*Auth
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if provider == "" {
			continue
		}
		if !restrictProviders && provider != selectedProvider {
			routeProvider = "mixed"
		}
		if authWeight(candidate) <= 0 || authPriority(candidate) != authPriority(selected) || !authAllowedByID(candidate.ID, allowed) {
			continue
		}
		if (restrictProviders && !containsProvider(providers, provider)) || !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		view, available, _, _ := m.buildDispatchCandidate(candidate, provider, model, policy, now)
		if available {
			pool = append(pool, view.availabilityAuth)
		} else if candidate.ID == selected.ID && credentialPolicyAllows(policy, candidate) {
			// A retained request has already executed even if an earlier usage
			// delivery cooled its credential. Do not move it or drop its debt.
			pool = append(pool, m.effectiveAvailabilityAuth(candidate, now))
		}
	}
	opts = ensureRequestedModelMetadata(opts, model)
	weighted.accountUsage(weightedStateKey(routeProvider, model, opts), pool, selected.ID, eventID)
}

func (s *WeightedRoundRobinSelector) hasSeenUsage(eventID string) bool {
	key := sha256.Sum256([]byte(eventID))
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	_, seen := s.usageSeen[key]
	return seen
}

func (s *WeightedRoundRobinSelector) accountUsage(route string, pool []*Auth, authID, eventID string) {
	// Serializing replay detection with the credit update handles concurrent
	// deliveries and retries after usage persistence or cooldown feedback fails.
	key := sha256.Sum256([]byte(eventID))
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if _, seen := s.usageSeen[key]; seen {
		return
	}
	if len(pool) > 1 {
		picked, reservation := s.reserve(route, pool, authID)
		if picked == nil {
			return
		}
		reservation.finish(true)
	} else if len(pool) == 0 || pool[0] == nil || pool[0].ID != authID || authWeight(pool[0]) <= 0 {
		return
	}
	if s.usageSeen == nil {
		s.usageSeen = make(map[[32]byte]struct{})
	}
	if len(s.usageOrder) < maxWeightedUsageEvents {
		s.usageOrder = append(s.usageOrder, key)
	} else {
		delete(s.usageSeen, s.usageOrder[s.usageNext])
		s.usageOrder[s.usageNext] = key
		s.usageNext = (s.usageNext + 1) % maxWeightedUsageEvents
	}
	s.usageSeen[key] = struct{}{}
}
