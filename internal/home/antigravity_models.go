package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityCapabilityCacheTTL  = registry.ModelsRefreshInterval
	antigravityCapabilityRetryBase = time.Minute
	antigravityCapabilityRetryMax  = 30 * time.Minute
	antigravityRefreshScanInterval = time.Minute
	antigravityProbeMaxWorkers     = 4
)

type antigravityCapabilityCacheEntry struct {
	hints             antigravityModelCapabilityHints
	expiresAt         time.Time
	lastUsed          time.Time
	probeRevision     uint64
	appliedRevision   uint64
	persistedRevision uint64
	pending           bool
}

type antigravityFailureState struct {
	failures    uint8
	lastFailure time.Time
	nextRetry   time.Time
}

func nextAntigravityFailure(previous antigravityFailureState, now time.Time, randomInt64N func(int64) int64) antigravityFailureState {
	if now.Sub(previous.lastFailure) > 2*antigravityCapabilityCacheTTL {
		previous.failures = 0
	}
	failures := min(previous.failures, 4) + 1
	window := min(2*antigravityCapabilityRetryBase<<(failures-1), antigravityCapabilityRetryMax)
	half := window / 2
	delay := half + time.Duration(randomInt64N(int64(half)+1))
	return antigravityFailureState{failures: failures, lastFailure: now, nextRetry: now.Add(delay)}
}

var (
	antigravityNowFunc          = time.Now
	antigravityModelUserAgent   = defaultAntigravityModelUserAgent
	antigravityCapabilityMu     sync.RWMutex
	antigravityCapabilityCache  = make(map[string]antigravityCapabilityCacheEntry)
	antigravityAuthFailureCache = make(map[string]antigravityFailureState)
	antigravityCapabilityGroup  singleflight.Group
	antigravityProbeSequence    atomic.Uint64
	antigravityProbeSlots       = make(chan struct{}, antigravityProbeMaxWorkers)
)

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
	// Nil means unknown; an empty non-nil set is an authoritative empty catalog.
	ModelIDs map[string]struct{}
	revision uint64
}

func (h antigravityModelCapabilityHints) clone() antigravityModelCapabilityHints {
	return antigravityModelCapabilityHints{
		WebSearchModelIDs: maps.Clone(h.WebSearchModelIDs),
		ModelIDs:          maps.Clone(h.ModelIDs),
		revision:          h.revision,
	}
}

type antigravityModelRefreshRequest struct {
	ctx      context.Context
	auth     *coreauth.Auth
	provider string
}

type antigravityHintPersistRequest struct {
	ctx      context.Context
	auth     *coreauth.Auth
	routeKey string
	endpoint string
	hints    antigravityModelCapabilityHints
	cancel   context.CancelFunc
}

type antigravityAccountProbe struct {
	auth      *coreauth.Auth
	routeKey  string
	flightKey string
	token     string
	cancel    context.CancelFunc
}

func resetAntigravityCapabilityCache() {
	antigravityCapabilityMu.Lock()
	antigravityCapabilityCache = make(map[string]antigravityCapabilityCacheEntry)
	antigravityAuthFailureCache = make(map[string]antigravityFailureState)
	antigravityCapabilityMu.Unlock()
}

func resolveAntigravityModelUserAgent(auth *coreauth.Auth) string {
	configured := ""
	if auth != nil {
		if auth.Attributes != nil {
			configured = strings.TrimSpace(auth.Attributes["user_agent"])
		}
		if configured == "" && auth.Metadata != nil {
			if raw, ok := auth.Metadata["user_agent"].(string); ok {
				configured = strings.TrimSpace(raw)
			}
		}
	}
	return antigravityModelUserAgent(configured)
}

func antigravityProjectID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"project_id", "projectId", "project"} {
			if raw, ok := auth.Metadata[key].(string); ok {
				if trimmed := strings.TrimSpace(raw); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"project_id", "projectId", "project"} {
			if trimmed := strings.TrimSpace(auth.Attributes[key]); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// antigravityCapabilityKey isolates catalogs by account, project, route, proxy, and client fingerprint.
func (r *Runtime) antigravityCapabilityKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	return antigravityCapabilityKeyForRoute(
		auth,
		antigravityModelBaseURLs(auth)[0],
		r.antigravityModelFetchProxyURL(auth),
		resolveAntigravityModelUserAgent(auth),
	)
}

func antigravityCapabilityKeyForRoute(auth *coreauth.Auth, endpoint, proxy, userAgent string) string {
	project := antigravityProjectID(auth)
	identity := ""
	if auth != nil {
		identity = strings.TrimSpace(auth.ID)
		if identity == "" {
			token := extractAccessToken(auth)
			identity = fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
		}
	}
	return fmt.Sprintf("%q/%q/%q/%q/%q", identity, project, strings.TrimRight(strings.TrimSpace(endpoint), "/"), strings.TrimSpace(proxy), strings.TrimSpace(userAgent))
}

func (r *Runtime) cachedAntigravityHints(auth *coreauth.Auth) antigravityModelCapabilityHints {
	if auth == nil {
		return antigravityModelCapabilityHints{}
	}
	endpoint := antigravityModelBaseURLs(auth)[0]
	key := antigravityCapabilityKeyForRoute(auth, endpoint, r.antigravityModelFetchProxyURL(auth), resolveAntigravityModelUserAgent(auth))
	return r.cachedAntigravityHintsForKey(auth, endpoint, key)
}

func (r *Runtime) cachedAntigravityHintsForKey(auth *coreauth.Auth, endpoint, key string) antigravityModelCapabilityHints {
	antigravityCapabilityMu.RLock()
	entry := antigravityCapabilityCache[key]
	antigravityCapabilityMu.RUnlock()
	if entry.hints.ModelIDs != nil {
		return entry.hints.clone()
	}
	persisted, ok := r.persistedAntigravityHintsForAuth(auth, endpoint)
	if !ok {
		return entry.hints.clone()
	}
	antigravityCapabilityMu.Lock()
	defer antigravityCapabilityMu.Unlock()
	entry = seedAntigravityCapabilityCacheLocked(key, persisted)
	return entry.hints.clone()
}

func seedAntigravityCapabilityCacheLocked(cacheKey string, persisted antigravityModelCapabilityHints) antigravityCapabilityCacheEntry {
	entry := antigravityCapabilityCache[cacheKey]
	if entry.hints.ModelIDs != nil {
		return entry
	}
	seeded := persisted.clone()
	seeded.revision = antigravityProbeSequence.Add(1)
	entry.hints = seeded
	entry.persistedRevision = seeded.revision
	entry.lastUsed = antigravityNowFunc()
	antigravityCapabilityCache[cacheKey] = entry
	return entry
}

func (r *Runtime) persistedAntigravityHintsForAuth(auth *coreauth.Auth, endpoint string) (antigravityModelCapabilityHints, bool) {
	if auth == nil {
		return antigravityModelCapabilityHints{}, false
	}
	var current *coreauth.Auth
	if r != nil && r.coreManager != nil && auth.ID != "" {
		var exists bool
		current, exists = r.coreManager.GetByID(auth.ID)
		if !exists || current == nil || current.Disabled || current.Status == coreauth.StatusDisabled {
			return antigravityModelCapabilityHints{}, false
		}
	}
	project := antigravityProjectID(auth)
	if project == "" && current != nil {
		project = antigravityProjectID(current)
	}
	if hints, ok := parsePersistedAntigravityCapabilityHints(auth.Metadata, project, endpoint); ok {
		return hints, true
	}
	if current != nil {
		return parsePersistedAntigravityCapabilityHints(current.Metadata, project, endpoint)
	}
	return antigravityModelCapabilityHints{}, false
}

func persistedAntigravityHintsEqual(existingRaw any, desired map[string]any) bool {
	if existingRaw == nil {
		return false
	}
	existingJSON, errExisting := json.Marshal(existingRaw)
	if errExisting != nil {
		return false
	}
	desiredJSON, errDesired := json.Marshal(desired)
	if errDesired != nil {
		return false
	}
	return bytes.Equal(existingJSON, desiredJSON)
}

func (r *Runtime) resolveAntigravityProbeAuth(ctx context.Context, auth *coreauth.Auth) *coreauth.Auth {
	if auth == nil {
		return nil
	}
	resolved := auth
	if r != nil && r.coreManager != nil && auth.ID != "" {
		if current, ok := r.coreManager.GetByID(auth.ID); ok && current != nil {
			resolved = current
		}
	}
	if extractAccessToken(resolved) != "" {
		return resolved
	}
	if r != nil && r.clusterAdapter != nil && r.clusterAdapter.Enabled() && strings.TrimSpace(auth.ID) != "" {
		if full, errFull := r.clusterAdapter.GetFullAuth(ctx, auth.ID); errFull == nil && full != nil {
			return full
		}
	}
	return resolved
}

func (r *Runtime) fetchAntigravityModelCapabilityHintsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityModelCapabilityHints {
	_, hints := r.fetchAntigravityModelCapabilityHints(ctx, auth)
	return hints
}

func (r *Runtime) fetchAntigravityModelCapabilityHints(ctx context.Context, auth *coreauth.Auth) (string, antigravityModelCapabilityHints) {
	if auth == nil {
		return "", antigravityModelCapabilityHints{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeAuth := r.resolveAntigravityProbeAuth(ctx, auth)
	if probeAuth == nil {
		return "", antigravityModelCapabilityHints{}
	}
	baseURLs := antigravityModelBaseURLs(probeAuth)
	proxyURL := r.antigravityModelFetchProxyURL(probeAuth)
	userAgent := resolveAntigravityModelUserAgent(probeAuth)
	cacheKey := antigravityCapabilityKeyForRoute(probeAuth, baseURLs[0], proxyURL, userAgent)
	accessToken := extractAccessToken(probeAuth)
	if accessToken == "" || ctx.Err() != nil {
		return cacheKey, r.cachedAntigravityHintsForKey(probeAuth, baseURLs[0], cacheKey)
	}
	failureKey := cacheKey + fmt.Sprintf("/%x", sha256.Sum256([]byte(accessToken)))
	lookup := func() (antigravityModelCapabilityHints, bool) {
		persisted, hasPersisted := r.persistedAntigravityHintsForAuth(probeAuth, baseURLs[0])
		antigravityCapabilityMu.Lock()
		defer antigravityCapabilityMu.Unlock()
		entry, exists := antigravityCapabilityCache[cacheKey]
		if entry.hints.ModelIDs == nil && hasPersisted {
			entry = seedAntigravityCapabilityCacheLocked(cacheKey, persisted)
			exists = true
		}
		now := antigravityNowFunc()
		if exists {
			entry.lastUsed = now
			antigravityCapabilityCache[cacheKey] = entry
		}
		return entry.hints.clone(), now.Before(entry.expiresAt) || now.Before(antigravityAuthFailureCache[failureKey].nextRetry)
	}
	if hints, cached := lookup(); cached {
		return cacheKey, hints
	}

	resultCh := antigravityCapabilityGroup.DoChan(failureKey, func() (any, error) {
		if hints, cached := lookup(); cached {
			return hints, nil
		}
		probeCtx, finish, active := r.beginAntigravityAccountProbe(ctx, probeAuth, cacheKey, failureKey, accessToken)
		if !active {
			return r.cachedAntigravityHintsForKey(probeAuth, baseURLs[0], cacheKey), nil
		}
		defer finish()
		revision := antigravityProbeSequence.Add(1)
		antigravityCapabilityMu.Lock()
		entry := antigravityCapabilityCache[cacheKey]
		entry.probeRevision, entry.lastUsed, entry.pending = revision, antigravityNowFunc(), true
		antigravityCapabilityCache[cacheKey] = entry
		antigravityCapabilityMu.Unlock()
		defer func() {
			antigravityCapabilityMu.Lock()
			current := antigravityCapabilityCache[cacheKey]
			if current.probeRevision == revision {
				current.pending = false
				antigravityCapabilityCache[cacheKey] = current
			}
			antigravityCapabilityMu.Unlock()
		}()
		select {
		case antigravityProbeSlots <- struct{}{}:
		case <-probeCtx.Done():
			hints, _ := lookup()
			return hints, nil
		}
		defer func() { <-antigravityProbeSlots }()

		hints, status := r.probeAntigravityModelCapabilityHints(probeCtx, probeAuth, baseURLs, proxyURL, accessToken, userAgent)
		antigravityCapabilityMu.Lock()
		defer antigravityCapabilityMu.Unlock()
		current := antigravityCapabilityCache[cacheKey]
		if probeCtx.Err() != nil || current.probeRevision != revision {
			return current.hints.clone(), nil
		}
		now := antigravityNowFunc()
		for key, other := range antigravityCapabilityCache {
			if key != cacheKey && !other.pending && now.Sub(other.lastUsed) > 2*antigravityCapabilityCacheTTL {
				delete(antigravityCapabilityCache, key)
			}
		}
		for key, failure := range antigravityAuthFailureCache {
			if now.Sub(failure.lastFailure) > 2*antigravityCapabilityCacheTTL {
				delete(antigravityAuthFailureCache, key)
			}
		}
		if status == antigravityProbeStatusSuccess && hints.ModelIDs != nil {
			hints.revision = revision
			current.hints, current.expiresAt, current.lastUsed = hints.clone(), now.Add(antigravityCapabilityCacheTTL), now
			antigravityCapabilityCache[cacheKey] = current
			delete(antigravityAuthFailureCache, failureKey)
			return hints, nil
		}
		antigravityAuthFailureCache[failureKey] = nextAntigravityFailure(antigravityAuthFailureCache[failureKey], now, rand.Int64N)
		retained := current.hints.clone()
		if status == antigravityProbeStatusSuccess && len(hints.WebSearchModelIDs) > 0 {
			retained.WebSearchModelIDs = hints.WebSearchModelIDs
		}
		return retained, nil
	})
	select {
	case <-ctx.Done():
		hints, _ := lookup()
		return cacheKey, hints
	case result := <-resultCh:
		if result.Err != nil || result.Val == nil {
			hints, _ := lookup()
			return cacheKey, hints
		}
		return cacheKey, result.Val.(antigravityModelCapabilityHints).clone()
	}
}

func (r *Runtime) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
		if auth.Metadata != nil {
			if proxyURL, ok := auth.Metadata["proxy_url"].(string); ok {
				if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if r != nil {
		r.cfgMu.RLock()
		defer r.cfgMu.RUnlock()
		if r.cfg != nil {
			return strings.TrimSpace(r.cfg.ProxyURL)
		}
	}
	return ""
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["base_urls"]); raw != "" {
			parts := strings.Split(raw, ",")
			urls := make([]string, 0, len(parts))
			for _, p := range parts {
				if trimmed := strings.TrimRight(strings.TrimSpace(p), "/"); trimmed != "" {
					urls = append(urls, trimmed)
				}
			}
			if len(urls) > 0 {
				return urls
			}
		}
	}
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily}
}

func resolveAntigravityModelBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["base_url"].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return ""
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	if len(models) == 0 || len(hints.WebSearchModelIDs) == 0 {
		return models
	}
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		if _, ok := hints.WebSearchModelIDs[modelID]; !ok {
			continue
		}
		if model.NativeCapabilities != nil && model.NativeCapabilities.WebSearch != nil && !*model.NativeCapabilities.WebSearch {
			continue
		}
		var capabilities registry.NativeCapabilities
		if model.NativeCapabilities != nil {
			capabilities = *model.NativeCapabilities
		}
		enabled := true
		capabilities.WebSearch = &enabled
		model.NativeCapabilities = &capabilities
	}
	return models
}

func filterAntigravityModels(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model != nil {
			if _, ok := hints.ModelIDs[normalizeAntigravityFetchedModelID(model.ID)]; ok {
				filtered = append(filtered, model)
			}
		}
	}
	return filtered
}

func antigravityAuthKind(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	authKind := ""
	if auth.Attributes != nil {
		authKind = strings.ToLower(strings.TrimSpace(auth.Attributes["auth_kind"]))
	}
	if authKind == "" {
		if kind, _ := auth.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	return authKind
}

func (r *Runtime) antigravityModelsForHints(auth *coreauth.Auth, hints antigravityModelCapabilityHints) []*ModelInfo {
	r.cfgMu.RLock()
	cfg := r.cfg
	r.cfgMu.RUnlock()
	models, _ := r.antigravityModelsAndCandidatesForHintsWithConfig(auth, hints, cfg)
	return models
}

func (r *Runtime) antigravityModelsAndCandidatesForHintsWithConfig(auth *coreauth.Auth, hints antigravityModelCapabilityHints, cfg *config.Config) ([]*ModelInfo, []*ModelInfo) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	baseModels := registry.GetAntigravityModels()
	if hints.ModelIDs != nil {
		baseModels = filterAntigravityModels(baseModels, hints)
	}
	baseModels = applyAntigravityFetchedModelCapabilities(baseModels, hints)
	baseModels = r.appendPluginModels("antigravity", baseModels)
	candidates := append([]*ModelInfo{}, baseModels...)

	authKind := antigravityAuthKind(auth)
	excluded := r.oauthExcludedModels(cfg, "antigravity", authKind)
	if auth != nil && auth.Attributes != nil {
		if val, ok := auth.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	models := applyExcludedModels(baseModels, excluded)
	models = applyOAuthModelAlias(cfg, "antigravity", authKind, models)
	var prefix string
	if auth != nil {
		prefix = auth.Prefix
	}
	models = applyModelPrefixes(models, prefix, cfg.ForceModelPrefix)
	return models, candidates
}

// WaitAntigravityProbes waits for any in-flight asynchronous Antigravity capability probes to complete.
func (r *Runtime) WaitAntigravityProbes() {
	if r == nil {
		return
	}
	r.antigravityProbeWg.Wait()
}

func antigravityAuthModelSettingsEqual(a, b *coreauth.Auth) bool {
	return a != nil && b != nil &&
		strings.EqualFold(strings.TrimSpace(a.Provider), strings.TrimSpace(b.Provider)) &&
		strings.TrimSpace(a.Prefix) == strings.TrimSpace(b.Prefix) &&
		a.Disabled == b.Disabled &&
		(a.Status == coreauth.StatusDisabled) == (b.Status == coreauth.StatusDisabled) &&
		antigravityAuthKind(a) == antigravityAuthKind(b) &&
		maps.Equal(a.Attributes, b.Attributes)
}

func (r *Runtime) applyAntigravityModelHints(ctx context.Context, auth *coreauth.Auth, providerKey string, hints antigravityModelCapabilityHints, expectedKey string) {
	if r == nil || auth == nil || auth.ID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || r.antigravityCapabilityKey(auth) != expectedKey {
		return
	}
	if hints.ModelIDs == nil && len(hints.WebSearchModelIDs) == 0 {
		return
	}
	if r.coreManager != nil {
		if current, exists := r.coreManager.GetByID(auth.ID); exists {
			if current == nil || current.Disabled || current.Status == coreauth.StatusDisabled ||
				!antigravityAuthModelSettingsEqual(current, auth) || r.antigravityCapabilityKey(current) != expectedKey {
				return
			}
		}
	}

	r.cfgMu.RLock()
	models, candidates := r.antigravityModelsAndCandidatesForHintsWithConfig(auth, hints, r.cfg)
	antigravityCapabilityMu.Lock()
	entry := antigravityCapabilityCache[expectedKey]
	if hints.ModelIDs != nil && entry.hints.revision != hints.revision {
		antigravityCapabilityMu.Unlock()
		r.cfgMu.RUnlock()
		return
	}
	r.registerResolvedModelsForAuth(auth, providerKey, models, candidates)
	if hints.ModelIDs != nil {
		entry.appliedRevision = hints.revision
		antigravityCapabilityCache[expectedKey] = entry
	}
	antigravityCapabilityMu.Unlock()
	r.cfgMu.RUnlock()

	if r.coreManager != nil {
		r.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)
		r.coreManager.RefreshSchedulerEntry(auth.ID)
	}
}

func (r *Runtime) asyncProbeAntigravityCapabilities(ctx context.Context, auth *coreauth.Auth, providerKey string) {
	r.queueAntigravityModelRefresh(ctx, auth, providerKey)
}

func (r *Runtime) probeAndRegisterAntigravityModels(ctx context.Context, auth *coreauth.Auth, providerKey string) (string, antigravityModelCapabilityHints) {
	expectedKey, hints := r.fetchAntigravityModelCapabilityHints(ctx, auth)
	r.applyAntigravityModelHints(ctx, auth, providerKey, hints, expectedKey)
	return expectedKey, hints
}

func (r *Runtime) persistAntigravityCapabilityHints(ctx context.Context, auth *coreauth.Auth, expectedKey, endpoint string, hints antigravityModelCapabilityHints) bool {
	if r == nil || auth == nil || auth.ID == "" || expectedKey == "" || hints.ModelIDs == nil || hints.revision == 0 {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	antigravityCapabilityMu.RLock()
	entry := antigravityCapabilityCache[expectedKey]
	stale := entry.hints.ModelIDs == nil || entry.hints.revision != hints.revision || entry.persistedRevision >= hints.revision
	antigravityCapabilityMu.RUnlock()
	if stale {
		return false
	}

	probeAuth := r.resolveAntigravityProbeAuth(ctx, auth)
	project := antigravityProjectID(probeAuth)
	value := buildPersistedAntigravityCapabilityHintsValue(project, endpoint, hints)

	markPersisted := func() {
		antigravityCapabilityMu.Lock()
		current := antigravityCapabilityCache[expectedKey]
		if current.hints.revision == hints.revision && current.persistedRevision < hints.revision {
			current.persistedRevision = hints.revision
			antigravityCapabilityCache[expectedKey] = current
		}
		antigravityCapabilityMu.Unlock()
	}

	if r.clusterAdapter != nil && r.clusterAdapter.Enabled() {
		mutator, ok := r.clusterAdapter.(coreauth.StateMutator)
		if !ok || mutator == nil {
			return false
		}
		alreadyCurrent := false
		changed := false
		updated, errMutate := mutator.MutateAuthState(ctx, auth.ID, func(existing *coreauth.Auth) bool {
			if existing == nil || existing.Disabled || existing.Status == coreauth.StatusDisabled ||
				!strings.EqualFold(strings.TrimSpace(existing.Provider), "antigravity") ||
				r.antigravityCapabilityKey(existing) != expectedKey {
				return false
			}
			antigravityCapabilityMu.RLock()
			currentEntry := antigravityCapabilityCache[expectedKey]
			valid := currentEntry.hints.ModelIDs != nil && currentEntry.hints.revision == hints.revision && currentEntry.persistedRevision < hints.revision
			antigravityCapabilityMu.RUnlock()
			if !valid {
				return false
			}
			if existing.Metadata != nil && persistedAntigravityHintsEqual(existing.Metadata[antigravityPersistedHintsMetaKey], value) {
				alreadyCurrent = true
				return false
			}
			if existing.Metadata == nil {
				existing.Metadata = make(map[string]any)
			}
			existing.Metadata[antigravityPersistedHintsMetaKey] = value
			changed = true
			return true
		})
		if errMutate != nil {
			return false
		}
		if changed || alreadyCurrent {
			markPersisted()
			if updated != nil && r.coreManager != nil {
				_, _ = r.UpdateAuthInMemory(ctx, updated)
			}
		}
		return false
	}

	if r.coreManager == nil {
		return false
	}
	current, exists := r.coreManager.GetByID(auth.ID)
	if !exists || current == nil || current.Disabled || current.Status == coreauth.StatusDisabled ||
		!strings.EqualFold(strings.TrimSpace(current.Provider), "antigravity") ||
		r.antigravityCapabilityKey(current) != expectedKey {
		return false
	}
	if current.Metadata != nil && persistedAntigravityHintsEqual(current.Metadata[antigravityPersistedHintsMetaKey], value) {
		markPersisted()
		return false
	}
	next := current.Clone()
	if next.Metadata == nil {
		next.Metadata = make(map[string]any)
	}
	next.Metadata[antigravityPersistedHintsMetaKey] = value
	if _, errUpdate := r.coreManager.Update(ctx, next); errUpdate != nil {
		return false
	}
	markPersisted()
	return false
}

func (r *Runtime) queueAntigravityModelRefresh(ctx context.Context, auth *coreauth.Auth, provider string) {
	if r == nil || auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	r.cfgMu.RLock()
	lifetime := r.antigravityContext
	r.cfgMu.RUnlock()
	if lifetime != nil && lifetime.Err() != nil {
		return
	}
	probeAuth := r.resolveAntigravityProbeAuth(ctx, auth)
	token := extractAccessToken(probeAuth)
	if token == "" {
		return
	}
	request := &antigravityModelRefreshRequest{
		ctx:      ctx,
		auth:     probeAuth.Clone(),
		provider: provider,
	}
	probe := &antigravityAccountProbe{
		auth:     request.auth,
		routeKey: r.antigravityCapabilityKey(request.auth),
		token:    token,
	}
	r.antigravityProbeMu.Lock()
	if ctx.Err() != nil || !r.antigravityAccountProbeCurrent(probe) {
		r.antigravityProbeMu.Unlock()
		return
	}
	if r.antigravityRefreshPending == nil {
		r.antigravityRefreshPending = make(map[string]*antigravityModelRefreshRequest)
	}
	_, running := r.antigravityRefreshPending[auth.ID]
	r.antigravityRefreshPending[auth.ID] = request
	if !running {
		r.antigravityProbeWg.Add(1)
	}
	r.antigravityProbeMu.Unlock()
	if !running {
		go r.runAntigravityAccountRefresh(auth.ID)
	}
}

func (r *Runtime) runAntigravityAccountRefresh(authID string) {
	defer r.antigravityProbeWg.Done()
	defer r.signalAntigravityModelRefresh()
	for {
		r.antigravityProbeMu.Lock()
		request := r.antigravityRefreshPending[authID]
		if request == nil {
			delete(r.antigravityRefreshPending, authID)
			r.antigravityProbeMu.Unlock()
			return
		}
		r.antigravityRefreshPending[authID] = nil
		r.antigravityProbeMu.Unlock()
		r.executeAntigravityModelRefresh(request)
	}
}

func (r *Runtime) executeAntigravityModelRefresh(request *antigravityModelRefreshRequest) {
	if request == nil || request.ctx.Err() != nil {
		return
	}
	probeCtx, cancel := context.WithCancel(request.ctx)
	defer cancel()
	r.cfgMu.RLock()
	lifetime := r.antigravityContext
	r.cfgMu.RUnlock()
	if lifetime != nil {
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		if lifetime.Err() != nil {
			return
		}
	}
	expectedKey, hints := r.probeAndRegisterAntigravityModels(probeCtx, request.auth, request.provider)
	if hints.ModelIDs != nil {
		r.queueAntigravityHintPersistence(context.WithoutCancel(request.ctx), request.auth, expectedKey, antigravityModelBaseURLs(request.auth)[0], hints)
	}
}

func (r *Runtime) beginAntigravityAccountProbe(parent context.Context, auth *coreauth.Auth, routeKey, flightKey, token string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	r.cfgMu.RLock()
	lifetime := r.antigravityContext
	r.cfgMu.RUnlock()
	var stop func() bool
	if lifetime != nil {
		stop = context.AfterFunc(lifetime, cancel)
		if lifetime.Err() != nil {
			cancel()
		}
	}
	probe := &antigravityAccountProbe{
		auth:      auth.Clone(),
		routeKey:  routeKey,
		flightKey: flightKey,
		token:     token,
		cancel:    cancel,
	}
	r.antigravityProbeMu.Lock()
	valid := ctx.Err() == nil && r.antigravityAccountProbeCurrent(probe)
	if valid {
		if r.antigravityAccountProbes == nil {
			r.antigravityAccountProbes = make(map[string]map[*antigravityAccountProbe]struct{})
		}
		if r.antigravityAccountProbes[auth.ID] == nil {
			r.antigravityAccountProbes[auth.ID] = make(map[*antigravityAccountProbe]struct{})
		}
		r.antigravityAccountProbes[auth.ID][probe] = struct{}{}
	}
	r.antigravityProbeMu.Unlock()
	cleanup := func() {
		cancel()
		if stop != nil {
			stop()
		}
		r.antigravityProbeMu.Lock()
		delete(r.antigravityAccountProbes[auth.ID], probe)
		if len(r.antigravityAccountProbes[auth.ID]) == 0 {
			delete(r.antigravityAccountProbes, auth.ID)
		}
		r.antigravityProbeMu.Unlock()
		r.signalAntigravityModelRefresh()
	}
	if !valid {
		cleanup()
		return ctx, func() {}, false
	}
	return ctx, cleanup, true
}

func (r *Runtime) antigravityAccountProbeCurrent(probe *antigravityAccountProbe) bool {
	if probe == nil || probe.auth == nil {
		return false
	}
	current := probe.auth
	if r != nil && r.coreManager != nil && probe.auth.ID != "" {
		var exists bool
		current, exists = r.coreManager.GetByID(probe.auth.ID)
		if !exists || current == nil {
			return false
		}
	}
	if current.Disabled || current.Status == coreauth.StatusDisabled ||
		!antigravityAuthModelSettingsEqual(current, probe.auth) ||
		r.antigravityCapabilityKey(current) != probe.routeKey {
		return false
	}
	if currentToken := extractAccessToken(current); currentToken != "" && currentToken != probe.token {
		return false
	}
	return true
}

func (r *Runtime) queueAntigravityHintPersistence(ctx context.Context, auth *coreauth.Auth, expectedKey, endpoint string, hints antigravityModelCapabilityHints) {
	if r == nil || auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled || expectedKey == "" || hints.ModelIDs == nil || hints.revision == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	r.cfgMu.RLock()
	lifetime := r.antigravityContext
	r.cfgMu.RUnlock()
	if lifetime != nil && lifetime.Err() != nil {
		return
	}
	antigravityCapabilityMu.RLock()
	entry := antigravityCapabilityCache[expectedKey]
	stale := entry.hints.ModelIDs == nil || entry.hints.revision != hints.revision || entry.persistedRevision >= hints.revision
	antigravityCapabilityMu.RUnlock()
	if stale {
		return
	}
	request := &antigravityHintPersistRequest{
		ctx:      ctx,
		auth:     auth.Clone(),
		routeKey: expectedKey,
		endpoint: endpoint,
		hints:    hints.clone(),
	}
	r.antigravityProbeMu.Lock()
	if ctx.Err() != nil || !r.antigravityHintPersistCurrent(request) {
		r.antigravityProbeMu.Unlock()
		return
	}
	if r.antigravityPersistPending == nil {
		r.antigravityPersistPending = make(map[string]*antigravityHintPersistRequest)
	}
	queued, running := r.antigravityPersistPending[auth.ID]
	if queued != nil && queued.hints.revision > request.hints.revision {
		r.antigravityProbeMu.Unlock()
		return
	}
	if active := r.antigravityPersistActive[auth.ID]; active != nil && (active.hints.revision < request.hints.revision || active.routeKey != request.routeKey) {
		if active.cancel != nil {
			active.cancel()
		}
	}
	r.antigravityPersistPending[auth.ID] = request
	if !running {
		r.antigravityProbeWg.Add(1)
	}
	r.antigravityProbeMu.Unlock()
	if !running {
		go r.runAntigravityHintPersistence(auth.ID)
	}
}

func (r *Runtime) runAntigravityHintPersistence(authID string) {
	defer r.antigravityProbeWg.Done()
	defer r.signalAntigravityModelRefresh()
	for {
		r.antigravityProbeMu.Lock()
		request := r.antigravityPersistPending[authID]
		if request == nil {
			delete(r.antigravityPersistPending, authID)
			delete(r.antigravityPersistActive, authID)
			r.antigravityProbeMu.Unlock()
			return
		}
		r.antigravityPersistPending[authID] = nil
		persistCtx, cancel := context.WithCancel(request.ctx)
		request.cancel = cancel
		if r.antigravityPersistActive == nil {
			r.antigravityPersistActive = make(map[string]*antigravityHintPersistRequest)
		}
		r.antigravityPersistActive[authID] = request
		r.antigravityProbeMu.Unlock()
		r.executeAntigravityHintPersistence(persistCtx, cancel, request)
	}
}

func (r *Runtime) executeAntigravityHintPersistence(persistCtx context.Context, cancel context.CancelFunc, request *antigravityHintPersistRequest) {
	defer cancel()
	if request == nil || persistCtx.Err() != nil {
		return
	}
	r.cfgMu.RLock()
	lifetime := r.antigravityContext
	r.cfgMu.RUnlock()
	if lifetime != nil {
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		if lifetime.Err() != nil {
			return
		}
	}
	if r.persistAntigravityCapabilityHints(persistCtx, request.auth, request.routeKey, request.endpoint, request.hints) && request.ctx.Err() == nil {
		r.queueAntigravityHintPersistence(request.ctx, request.auth, request.routeKey, request.endpoint, request.hints)
	}
}

func (r *Runtime) antigravityHintPersistCurrent(request *antigravityHintPersistRequest) bool {
	if request == nil || request.auth == nil {
		return false
	}
	current := request.auth
	if r != nil && r.coreManager != nil && request.auth.ID != "" {
		var exists bool
		current, exists = r.coreManager.GetByID(request.auth.ID)
		if !exists || current == nil {
			return false
		}
	}
	return !current.Disabled && current.Status != coreauth.StatusDisabled &&
		strings.EqualFold(strings.TrimSpace(current.Provider), strings.TrimSpace(request.auth.Provider)) &&
		r.antigravityCapabilityKey(current) == request.routeKey
}

func (r *Runtime) cancelStaleAntigravityProbes(authID string) {
	if r == nil {
		return
	}
	r.antigravityProbeMu.Lock()
	for id, probes := range r.antigravityAccountProbes {
		if authID != "" && id != authID {
			continue
		}
		for probe := range probes {
			if !r.antigravityAccountProbeCurrent(probe) {
				probe.cancel()
				antigravityCapabilityGroup.Forget(probe.flightKey)
				delete(probes, probe)
			}
		}
		if len(probes) == 0 {
			delete(r.antigravityAccountProbes, id)
		}
	}
	for id, active := range r.antigravityPersistActive {
		if authID != "" && id != authID {
			continue
		}
		if active != nil && !r.antigravityHintPersistCurrent(active) {
			if active.cancel != nil {
				active.cancel()
			}
		}
	}
	for id, pending := range r.antigravityPersistPending {
		if authID != "" && id != authID {
			continue
		}
		if pending != nil && !r.antigravityHintPersistCurrent(pending) {
			r.antigravityPersistPending[id] = nil
		}
	}
	r.antigravityProbeMu.Unlock()
	r.signalAntigravityModelRefresh()
}

func (r *Runtime) antigravityModelRefreshWake() chan struct{} {
	r.antigravityProbeMu.Lock()
	defer r.antigravityProbeMu.Unlock()
	if r.antigravityRefreshWake == nil {
		r.antigravityRefreshWake = make(chan struct{}, 1)
	}
	return r.antigravityRefreshWake
}

func (r *Runtime) signalAntigravityModelRefresh() {
	if r == nil {
		return
	}
	select {
	case r.antigravityModelRefreshWake() <- struct{}{}:
	default:
	}
}

func (r *Runtime) refreshAntigravityModels(ctx context.Context) {
	if r == nil || r.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.cancelStaleAntigravityProbes("")
	for _, auth := range r.coreManager.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		snapshot := r.resolveAntigravityProbeAuth(ctx, auth)
		if snapshot == nil {
			continue
		}
		key := r.antigravityCapabilityKey(snapshot)
		token := extractAccessToken(snapshot)
		if token == "" {
			continue
		}
		failureKey := key + fmt.Sprintf("/%x", sha256.Sum256([]byte(token)))
		hasModels := len(registry.GetGlobalRegistry().GetModelsForClient(snapshot.ID)) > 0 ||
			registry.GetGlobalRegistry().GetModelCandidatesForClient(snapshot.ID) != nil
		antigravityCapabilityMu.RLock()
		entry := antigravityCapabilityCache[key]
		registered := entry.appliedRevision == entry.hints.revision && hasModels
		persisted := entry.hints.ModelIDs == nil || entry.persistedRevision == entry.hints.revision
		fresh := (antigravityNowFunc().Before(entry.expiresAt) && registered && persisted) || antigravityNowFunc().Before(antigravityAuthFailureCache[failureKey].nextRetry)
		antigravityCapabilityMu.RUnlock()
		if fresh {
			continue
		}
		r.scheduleAntigravityModelRefresh(ctx, snapshot)
	}
}

func (r *Runtime) scheduleAntigravityModelRefresh(ctx context.Context, auth *coreauth.Auth) {
	r.queueAntigravityModelRefresh(ctx, auth, "antigravity")
}

func (r *Runtime) nextAntigravityModelRefreshDelay() time.Duration {
	now := antigravityNowFunc()
	next := now.Add(antigravityRefreshScanInterval)
	if r == nil || r.coreManager == nil {
		return antigravityRefreshScanInterval
	}
	for _, auth := range r.coreManager.List() {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		key := r.antigravityCapabilityKey(auth)
		antigravityCapabilityMu.RLock()
		expiry := antigravityCapabilityCache[key].expiresAt
		if expiry.After(now) && expiry.Before(next) {
			next = expiry
		}
		for fk, failure := range antigravityAuthFailureCache {
			if strings.HasPrefix(fk, key+"/") && failure.nextRetry.After(now) && failure.nextRetry.Before(next) {
				next = failure.nextRetry
			}
		}
		antigravityCapabilityMu.RUnlock()
	}
	return next.Sub(now)
}

func (r *Runtime) runAntigravityModelRefresh(ctx context.Context) {
	wake := r.antigravityModelRefreshWake()
	delay := r.nextAntigravityModelRefreshDelay()
	deadline := antigravityNowFunc().Add(delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			delay = r.nextAntigravityModelRefreshDelay()
			earlier := antigravityNowFunc().Add(delay)
			if earlier.Before(deadline) {
				deadline = earlier
				timer.Reset(delay)
			}
			continue
		case <-timer.C:
			r.refreshAntigravityModels(ctx)
		}
		delay = r.nextAntigravityModelRefreshDelay()
		deadline = antigravityNowFunc().Add(delay)
		timer.Reset(delay)
	}
}
