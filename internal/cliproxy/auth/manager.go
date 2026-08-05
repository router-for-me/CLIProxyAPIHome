package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
)

const (
	refreshCheckInterval       = 5 * time.Second
	refreshMaxConcurrency      = 16
	refreshPendingBackoff      = time.Minute
	refreshFailureBackoff      = 5 * time.Minute
	refreshAuthErrorCode       = "authentication_error"
	refreshAuthErrorMsg        = "credential unauthorized"
	refreshTransientErrorCode  = "refresh_temporarily_unavailable"
	refreshTransientErrorMsg   = "credential refresh temporarily unavailable"
	refreshUnsupportedCode     = "refresh_unsupported"
	refreshUnsupportedMsg      = "credential does not support refresh"
	refreshProviderCallTimeout = 25 * time.Second
	// refreshIneffectiveBackoff throttles refresh attempts when the refresh completes
	// successfully but the auth still evaluates as needing refresh (e.g. token expiry
	// wasn't updated). Without this guard, the auto-refresh loop can tight-loop and
	// burn CPU at idle.
	refreshIneffectiveBackoff = 30 * time.Second
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

type FullAuthResolver interface {
	GetFullAuth(ctx context.Context, uuid string) (*Auth, error)
}

var (
	ErrFullAuthNotFound   = errors.New("full auth not found")
	ErrRefreshUnsupported = &Error{
		Code:       refreshUnsupportedCode,
		Message:    refreshUnsupportedMsg,
		HTTPStatus: http.StatusServiceUnavailable,
	}
)

// Manager orchestrates auth lifecycle, selection, and persistence for CLIProxyAPIHome.
//
// This is intentionally narrower than CPA's full execution manager: it only supports
// registering/updating auths, scheduling selection (Dispatch), and background refresh.
type Manager struct {
	store    Store
	selector Selector

	mu        sync.RWMutex
	auths     map[string]*Auth
	indexAuth map[string]*Auth
	scheduler *authScheduler

	oauthModelAlias atomic.Value
	runtimeConfig   atomic.Value

	rtProvider         RoundTripperProvider
	fullResolver       FullAuthResolver
	pluginRefresher    PluginAuthRefresher
	pluginScheduler    PluginScheduler
	autoRefreshHandler func(context.Context, *Auth) error

	refreshCancel context.CancelFunc
	refreshLoop   *authAutoRefreshLoop

	resultPersistOnce    sync.Once
	resultPersistMu      sync.Mutex
	resultPersistWake    chan struct{}
	resultPersistPending map[string]*Auth
}

// NewManager creates a new manager.
func NewManager(store Store, selector Selector, _ any) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	mgr := &Manager{
		store:                store,
		selector:             selector,
		auths:                make(map[string]*Auth),
		indexAuth:            make(map[string]*Auth),
		resultPersistWake:    make(chan struct{}, 1),
		resultPersistPending: make(map[string]*Auth),
	}
	mgr.runtimeConfig.Store(&internalconfig.Config{})
	// atomic.Value requires non-nil initial value.
	mgr.oauthModelAlias.Store(&oauthModelAliasTable{})
	mgr.scheduler = newAuthScheduler(selector, func(auth *Auth, routeModel string) string {
		return mgr.resolveDispatchModel(auth, routeModel).Model
	})
	return mgr
}

// SetAutoRefreshHandler routes background refresh through the cluster lock owner.
func (m *Manager) SetAutoRefreshHandler(handler func(context.Context, *Auth) error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.autoRefreshHandler = handler
	m.mu.Unlock()
}

// SetRoundTripperProvider sets a round tripper provider.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// roundTripperFor returns a round tripper for.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// SetFullAuthResolver sets a full auth resolver.
func (m *Manager) SetFullAuthResolver(resolver FullAuthResolver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.fullResolver = resolver
	m.mu.Unlock()
}

// SetStore sets a store.
func (m *Manager) SetStore(store Store) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.store = store
	m.mu.Unlock()
}

// SetConfig sets a config.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	if m.scheduler != nil {
		m.scheduler.resetModelShards()
	}
}

// SetSelector sets a selector.
func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}

	m.mu.Lock()
	prev := m.selector
	m.selector = selector
	scheduler := m.scheduler
	m.mu.Unlock()

	if scheduler != nil {
		scheduler.setSelector(selector)
	}
	if stoppable, ok := prev.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

// Register wires package handlers into the provided registry.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	// Keep validation before state changes so failures leave existing data intact.
	if m == nil {
		return nil, fmt.Errorf("auth manager: nil manager")
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil, fmt.Errorf("auth manager: missing auth id")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	next := auth.Clone()
	if next.CreatedAt.IsZero() {
		next.CreatedAt = now
	}
	next.UpdatedAt = now
	next.EnsureIndex()

	m.mu.Lock()
	if m.auths == nil {
		m.auths = make(map[string]*Auth)
	}
	if m.indexAuth == nil {
		m.indexAuth = make(map[string]*Auth)
	}
	if _, exists := m.auths[next.ID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth manager: auth already exists")
	}
	m.auths[next.ID] = next
	if idx := strings.TrimSpace(next.Index); idx != "" {
		m.indexAuth[idx] = next
	}
	m.mu.Unlock()

	m.scheduler.upsertAuth(next)
	if errPersist := m.persist(ctx, next); errPersist != nil {
		return nil, errPersist
	}
	m.queueRefreshReschedule(next.ID)
	return next.Clone(), nil
}

// Delete handles delete.
func (m *Manager) Delete(ctx context.Context, id string) error {
	// Validate request inputs before mutating persisted state.
	if m == nil {
		return fmt.Errorf("auth manager: nil manager")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth manager: missing auth id")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("auth manager: auth not found")
	}
	if auth != nil {
		idx := strings.TrimSpace(auth.Index)
		if idx != "" {
			if cur, ok := m.indexAuth[idx]; ok && cur != nil && cur.ID == auth.ID {
				delete(m.indexAuth, idx)
			}
		}
	}
	delete(m.auths, id)
	loop := m.refreshLoop
	m.mu.Unlock()

	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	if loop != nil {
		loop.remove(id)
	}
	if invalidator, ok := m.selector.(interface{ InvalidateAuth(string) }); ok {
		invalidator.InvalidateAuth(id)
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if m.store == nil {
		return nil
	}
	return m.store.Delete(ctx, id)
}

// Update updates the value.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	// Keep validation before state changes so failures leave existing data intact.
	if m == nil {
		return nil, fmt.Errorf("auth manager: nil manager")
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil, fmt.Errorf("auth manager: missing auth id")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()

	m.mu.Lock()
	current, ok := m.auths[auth.ID]
	if !ok || current == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	next := auth.Clone()
	if next.CreatedAt.IsZero() {
		next.CreatedAt = current.CreatedAt
	}
	next.UpdatedAt = now
	next.EnsureIndex()
	prevIndex := ""
	if current != nil {
		prevIndex = strings.TrimSpace(current.Index)
	}
	newIndex := strings.TrimSpace(next.Index)
	m.auths[next.ID] = next
	if m.indexAuth != nil {
		if prevIndex != "" && prevIndex != newIndex {
			if cur, ok := m.indexAuth[prevIndex]; ok && cur != nil && cur.ID == next.ID {
				delete(m.indexAuth, prevIndex)
			}
		}
		if newIndex != "" {
			m.indexAuth[newIndex] = next
		}
	}
	m.mu.Unlock()

	m.scheduler.upsertAuth(next)
	if errPersist := m.persist(ctx, next); errPersist != nil {
		return nil, errPersist
	}
	m.queueRefreshReschedule(next.ID)
	return next.Clone(), nil
}

// persist persists the value.
func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m == nil || m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.Attributes["source"])), "config:") {
			return nil
		}
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true") {
			return nil
		}
	}
	if auth.Disabled {
		// Keep disabled auth entries persisted to disk too, consistent with CPA.
	}
	if auth.Metadata == nil && auth.Storage == nil {
		return nil
	}
	if versionedStore, ok := m.store.(StateVersionSaver); ok {
		persisted := auth.Clone()
		_, stateVersion, errSave := versionedStore.SaveWithStateVersion(ctx, persisted)
		if errSave != nil {
			return errSave
		}
		if stateVersion > 0 {
			m.mu.Lock()
			if current := m.auths[auth.ID]; current == auth {
				current.StateVersion = stateVersion
			}
			m.mu.Unlock()
		}
		return nil
	}
	_, errSave := m.store.Save(ctx, auth)
	return errSave
}

// enqueueResultPersist queues runtime result state for background persistence.
func (m *Manager) enqueueResultPersist(ctx context.Context, auth *Auth) {
	if m == nil || m.store == nil || auth == nil {
		return
	}
	if shouldSkipPersist(ctx) {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}

	m.resultPersistOnce.Do(func() {
		m.resultPersistMu.Lock()
		if m.resultPersistWake == nil {
			m.resultPersistWake = make(chan struct{}, 1)
		}
		if m.resultPersistPending == nil {
			m.resultPersistPending = make(map[string]*Auth)
		}
		m.resultPersistMu.Unlock()
		go m.runResultPersistWorker()
	})

	m.resultPersistMu.Lock()
	if m.resultPersistPending == nil {
		m.resultPersistPending = make(map[string]*Auth)
	}
	m.resultPersistPending[authID] = auth.Clone()
	wake := m.resultPersistWake
	m.resultPersistMu.Unlock()

	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// runResultPersistWorker persists queued result state snapshots.
func (m *Manager) runResultPersistWorker() {
	if m == nil {
		return
	}
	for range m.resultPersistWake {
		m.flushResultPersistQueue()
	}
}

// flushResultPersistQueue drains queued result state snapshots.
func (m *Manager) flushResultPersistQueue() {
	if m == nil {
		return
	}
	for {
		m.resultPersistMu.Lock()
		if len(m.resultPersistPending) == 0 {
			m.resultPersistMu.Unlock()
			return
		}
		pending := m.resultPersistPending
		m.resultPersistPending = make(map[string]*Auth, len(pending))
		m.resultPersistMu.Unlock()

		for _, auth := range pending {
			_ = m.persist(context.Background(), auth)
		}
	}
}

// List returns the available entries.
func (m *Manager) List() []*Auth {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		if a == nil {
			continue
		}
		out = append(out, a.Clone())
	}
	return out
}

// GetByID returns a by id.
func (m *Manager) GetByID(id string) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	a := m.auths[id]
	m.mu.RUnlock()
	if a == nil {
		return nil, false
	}
	return a.Clone(), true
}

// GetByIndex returns a by index.
func (m *Manager) GetByIndex(index string) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	index = strings.TrimSpace(index)
	if index == "" {
		return nil, false
	}
	m.mu.RLock()
	a := m.indexAuth[index]
	m.mu.RUnlock()
	if a == nil {
		return nil, false
	}
	return a.Clone(), true
}

// RefreshSchedulerEntry refreshes refresh scheduler entry.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil {
		return
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		return
	}
	m.scheduler.upsertAuth(auth)
}

// ReconcileRegistryModelStates aligns the derived model registry with the
// authoritative scheduler state held by this Home instance.
func (m *Manager) ReconcileRegistryModelStates(_ context.Context, authID string) {
	if m == nil {
		return
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		return
	}

	modelRegistry := registry.GetGlobalRegistry()
	// Registry IDs are client-visible route models, while execution state is
	// keyed by the credential-specific upstream model. Keep both so aliases and
	// credential prefixes reconcile against the same state used by Dispatch.
	models := make(map[string]string, len(auth.ModelStates))
	for model := range auth.ModelStates {
		if model = canonicalModelKey(model); model != "" {
			models[model] = model
		}
	}
	for _, model := range modelRegistry.GetModelsForClient(auth.ID) {
		if model == nil {
			continue
		}
		registryModel := strings.TrimSpace(model.ID)
		if registryModel == "" {
			continue
		}
		stateModel := canonicalModelKey(registryModel)
		if resolved := m.resolveDispatchModel(auth, registryModel); resolved.Key != "" {
			stateModel = resolved.Key
		}
		models[registryModel] = stateModel
	}

	now := time.Now()
	for registryModel, stateModel := range models {
		blocked, reason, _ := isAuthBlockedForModel(auth, stateModel, now)
		legacyModel := canonicalModelKey(registryModel)
		if !blocked && stateModel != legacyModel {
			if legacyBlocked, legacyReason, _ := isAuthBlockedForModel(auth, legacyModel, now); legacyBlocked {
				blocked = true
				reason = legacyReason
				stateModel = legacyModel
			}
		}
		if blocked && reason == blockReasonCooldown {
			modelRegistry.SetModelQuotaExceeded(auth.ID, registryModel)
		} else {
			modelRegistry.ClearModelQuotaExceeded(auth.ID, registryModel)
		}

		if !blocked || !shouldSuspendRegistryModel(auth, stateModel, reason) {
			modelRegistry.ResumeClientModel(auth.ID, registryModel)
			continue
		}
		suspendReason := "unavailable"
		switch reason {
		case blockReasonCooldown:
			suspendReason = "quota"
		case blockReasonDisabled:
			suspendReason = "disabled"
		default:
			if state := auth.ModelStates[stateModel]; state != nil && strings.TrimSpace(state.StatusMessage) != "" {
				suspendReason = strings.TrimSpace(state.StatusMessage)
			}
		}
		// Recreate the suspension so a transition from quota to another error
		// also updates the registry reason.
		modelRegistry.ResumeClientModel(auth.ID, registryModel)
		modelRegistry.SuspendClientModel(auth.ID, registryModel, suspendReason)
	}
}

func shouldSuspendRegistryModel(auth *Auth, model string, reason blockReason) bool {
	switch reason {
	case blockReasonCooldown, blockReasonDisabled:
		return true
	}
	if auth == nil {
		return false
	}
	state := auth.ModelStates[canonicalModelKey(model)]
	if state == nil {
		return false
	}
	if isModelSupportResultError(state.LastError) {
		return true
	}
	switch statusCodeFromResult(state.LastError) {
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

// ensureRequestedModelMetadata ensures a requested model metadata.
func ensureRequestedModelMetadata(opts Options, requestedModel string) Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if opts.Metadata == nil {
		opts.Metadata = map[string]any{RequestedModelMetadataKey: requestedModel}
		return opts
	}
	if _, exists := opts.Metadata[RequestedModelMetadataKey]; !exists {
		opts.Metadata[RequestedModelMetadataKey] = requestedModel
	}
	return opts
}

// isBuiltInSelector reports whether built in selector.
func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *FillFirstSelector, *RoundRobinSelector:
		return true
	default:
		return false
	}
}

// selectionArgForSelector returns a selection arg for selector.
func selectionArgForSelector(selector Selector, routeModel string) string {
	if isBuiltInSelector(selector) {
		return ""
	}
	return routeModel
}

type dispatchCandidate struct {
	auth          *Auth
	providerKey   string
	upstreamModel string
	upstreamKey   string
	forceMapping  bool
	originalAlias string
}

func (m *Manager) buildDispatchCandidate(auth *Auth, providerKey, routeModel, credentialPolicy string, now time.Time) (dispatchCandidate, bool, blockReason, time.Time) {
	authForResolution := auth
	if auth != nil && strings.TrimSpace(auth.Provider) == "" {
		normalizedProvider := strings.ToLower(strings.TrimSpace(providerKey))
		if normalizedProvider != "" && normalizedProvider != "mixed" {
			authCopy := *auth
			authCopy.Provider = normalizedProvider
			authForResolution = &authCopy
		}
	}
	if !credentialPolicyAllows(credentialPolicy, authForResolution) {
		return dispatchCandidate{}, false, blockReasonOther, time.Time{}
	}
	resolved := m.resolveDispatchModel(authForResolution, routeModel)
	if strings.TrimSpace(resolved.Model) == "" {
		return dispatchCandidate{}, false, blockReasonOther, time.Time{}
	}
	blocked, reason, next := isAuthBlockedForModel(auth, resolved.Key, now)
	if !blocked && resolved.Key != canonicalModelKey(routeModel) {
		blocked, reason, next = isAuthBlockedForModel(auth, routeModel, now)
	}
	if blocked {
		return dispatchCandidate{}, false, reason, next
	}
	return dispatchCandidate{
		auth:          auth,
		providerKey:   providerKey,
		upstreamModel: resolved.Model,
		upstreamKey:   resolved.Key,
		forceMapping:  resolved.ForceMapping,
		originalAlias: resolved.OriginalAlias,
	}, true, blockReasonNone, time.Time{}
}

func dispatchUnavailableError(routeModel, provider string, total int, cooldownCount int, earliest time.Time, now time.Time) error {
	if total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if cooldownCount == total && !earliest.IsZero() {
		providerForError := provider
		if providerForError == "mixed" {
			providerForError = ""
		}
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(routeModel, providerForError, resetIn)
	}
	return &Error{Code: "auth_unavailable", Message: "no auth available"}
}

// useSchedulerFastPath reports whether use scheduler fast path.
func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	return isBuiltInSelector(selector)
}

// Dispatch processes dispatch.
func (m *Manager) Dispatch(ctx context.Context, providers []string, requestedModel string, opts Options) (*DispatchDecision, error) {
	// Build the candidate view before applying availability rules.
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	routeModel := strings.TrimSpace(requestedModel)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	credentialPolicy := credentialPolicyFromOptions(opts)
	if normalizedPolicy, okPolicy := NormalizeCredentialPolicy(credentialPolicy); !okPolicy {
		return nil, &Error{Code: "unsupported_credential_policy", Message: "unsupported credential policy " + strconv.Quote(credentialPolicy)}
	} else {
		credentialPolicy = normalizedPolicy
	}
	allowedAuthIDs := allowedAuthIDsFromOptions(opts)
	allowedModelIDs := allowedModelIDsFromOptions(opts)
	routeKey := canonicalModelKey(routeModel)
	if !modelAllowedByID(routeKey, allowedModelIDs) {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}

	if !m.hasPluginScheduler() && m.useSchedulerFastPath() {
		tried := make(map[string]struct{})
		for {
			auth, providerKey, errPick := m.scheduler.pickMixed(ctx, providers, routeModel, opts, tried)
			if errPick != nil {
				return nil, errPick
			}
			if auth == nil {
				return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
			}
			tried[auth.ID] = struct{}{}

			fullAuth, okFull, errFull := m.resolveFullDispatchAuth(ctx, auth)
			if errFull != nil {
				return nil, errFull
			}
			if !okFull {
				continue
			}

			fullProviderKey := strings.ToLower(strings.TrimSpace(fullAuth.Provider))
			if fullProviderKey == "" {
				fullProviderKey = providerKey
			}
			fullCandidate, okCandidate, _, _ := m.buildDispatchCandidate(fullAuth, fullProviderKey, routeModel, credentialPolicy, time.Now())
			if !okCandidate || concurrencyCandidateExcluded(opts, fullAuth.ID, fullCandidate.upstreamKey) {
				continue
			}
			upstream := strings.TrimSpace(fullCandidate.upstreamModel)
			if upstream == "" {
				upstream = routeModel
			}
			return &DispatchDecision{
				Auth:          fullAuth.Clone(),
				Provider:      fullProviderKey,
				UpstreamModel: upstream,
				PooledModels:  false,
				ForceMapping:  fullCandidate.forceMapping,
				OriginalAlias: fullCandidate.originalAlias,
			}, nil
		}
	}

	normalizedProviders := normalizeProviderKeys(providers)
	if len(normalizedProviders) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	providerForSelector := "mixed"
	if len(normalizedProviders) == 1 {
		providerForSelector = normalizedProviders[0]
	}
	registryRef := registry.GetGlobalRegistry()

	tried := make(map[string]struct{})
	for {
		now := time.Now()
		m.mu.RLock()
		selector := m.selector
		pluginScheduler := m.pluginScheduler
		dispatchCandidates := make([]dispatchCandidate, 0, len(m.auths))
		availableAuths := make([]*Auth, 0, len(m.auths))
		totalCandidates := 0
		cooldownCount := 0
		var earliest time.Time
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Disabled {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if !authAllowedByID(candidate.ID, allowedAuthIDs) {
				continue
			}
			providerKey := strings.ToLower(strings.TrimSpace(candidate.Provider))
			if providerKey == "" {
				continue
			}
			if !containsProvider(normalizedProviders, providerKey) {
				continue
			}
			if routeKey != "" && (registryRef == nil || !registryRef.ClientSupportsModel(candidate.ID, routeKey)) {
				continue
			}
			totalCandidates++
			dispatchCandidate, okCandidate, reason, next := m.buildDispatchCandidate(candidate, providerKey, routeModel, credentialPolicy, now)
			if !okCandidate {
				if reason == blockReasonCooldown {
					cooldownCount++
					if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
						earliest = next
					}
				}
				continue
			}
			if concurrencyCandidateExcluded(opts, candidate.ID, dispatchCandidate.upstreamKey) {
				continue
			}
			dispatchCandidates = append(dispatchCandidates, dispatchCandidate)
			availableAuths = append(availableAuths, candidate)
		}
		m.mu.RUnlock()

		if len(availableAuths) == 0 {
			return nil, dispatchUnavailableError(routeModel, providerForSelector, totalCandidates, cooldownCount, earliest, now)
		}

		var auth *Auth
		if pluginScheduler != nil {
			pluginAuth, handled, errPluginPick := m.pickViaPluginScheduler(ctx, pluginScheduler, providerForSelector, normalizedProviders, routeModel, opts, tried, availableAuths)
			if errPluginPick != nil {
				return nil, errPluginPick
			}
			if handled && pluginAuth != nil {
				auth = pluginAuth
			}
		}

		if auth == nil {
			if isBuiltInSelector(selector) && len(excludedConcurrencyCandidatesFromOptions(opts)) == 0 {
				builtinAuth, handledBuiltin, errBuiltinPick := m.pickViaBuiltinScheduler(ctx, schedulerStrategyCurrent, providerForSelector, normalizedProviders, routeModel, opts, tried)
				if errBuiltinPick != nil {
					return nil, errBuiltinPick
				}
				if handledBuiltin {
					auth = builtinAuth
				}
			}
			if auth == nil {
				var errPick error
				auth, errPick = selector.Pick(ctx, providerForSelector, selectionArgForSelector(selector, routeModel), opts, availableAuths)
				if errPick != nil {
					return nil, errPick
				}
			}
		}
		if auth == nil {
			return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		var selectedCandidate dispatchCandidate
		foundCandidate := false
		for _, candidate := range dispatchCandidates {
			if candidate.auth != nil && candidate.auth.ID == auth.ID {
				selectedCandidate = candidate
				foundCandidate = true
				break
			}
		}
		if !foundCandidate {
			return nil, &Error{Code: "auth_unavailable", Message: "selector returned auth outside candidates"}
		}
		tried[auth.ID] = struct{}{}

		fullAuth, okFull, errFull := m.resolveFullDispatchAuth(ctx, auth)
		if errFull != nil {
			return nil, errFull
		}
		if !okFull {
			continue
		}

		providerKey := strings.ToLower(strings.TrimSpace(fullAuth.Provider))
		if providerKey == "" {
			providerKey = selectedCandidate.providerKey
		}
		if providerKey == "" {
			providerKey = providerForSelector
		}
		fullCandidate, okCandidate, _, _ := m.buildDispatchCandidate(fullAuth, providerKey, routeModel, credentialPolicy, time.Now())
		if !okCandidate || concurrencyCandidateExcluded(opts, fullAuth.ID, fullCandidate.upstreamKey) {
			continue
		}
		upstream := strings.TrimSpace(fullCandidate.upstreamModel)
		if upstream == "" {
			upstream = routeModel
		}
		return &DispatchDecision{
			Auth:          fullAuth.Clone(),
			Provider:      providerKey,
			UpstreamModel: upstream,
			PooledModels:  false,
			ForceMapping:  fullCandidate.forceMapping,
			OriginalAlias: fullCandidate.originalAlias,
		}, nil
	}
}

// resolveFullDispatchAuth resolves a full dispatch auth.
func (m *Manager) resolveFullDispatchAuth(ctx context.Context, auth *Auth) (*Auth, bool, error) {
	// Build the candidate view before applying availability rules.
	if auth == nil {
		return nil, false, nil
	}
	m.mu.RLock()
	resolver := m.fullResolver
	m.mu.RUnlock()
	if resolver == nil {
		return auth, true, nil
	}

	uuid := strings.TrimSpace(auth.ID)
	if uuid == "" {
		return nil, false, nil
	}
	fullAuth, errFull := resolver.GetFullAuth(ctx, uuid)
	if errFull != nil {
		if errors.Is(errFull, ErrFullAuthNotFound) {
			return nil, false, nil
		}
		return nil, false, errFull
	}
	if fullAuth == nil || fullAuth.Disabled || fullAuth.Status == StatusDisabled {
		return nil, false, nil
	}
	fullAuth.ID = uuid
	fullAuth.Index = uuid
	return fullAuth, true, nil
}

// resolveFullRefreshAuth resolves a full refresh auth.
func (m *Manager) resolveFullRefreshAuth(ctx context.Context, auth *Auth) (*Auth, bool, error) {
	// Resolve credential context before calling upstream OAuth services.
	if auth == nil {
		return nil, false, nil
	}
	m.mu.RLock()
	resolver := m.fullResolver
	m.mu.RUnlock()
	if resolver == nil {
		return auth, true, nil
	}

	uuid := strings.TrimSpace(auth.ID)
	if uuid == "" {
		return nil, false, nil
	}
	fullAuth, errFull := resolver.GetFullAuth(ctx, uuid)
	if errFull != nil {
		if errors.Is(errFull, ErrFullAuthNotFound) {
			return nil, false, nil
		}
		return nil, false, errFull
	}
	if fullAuth == nil || fullAuth.Disabled || fullAuth.Status == StatusDisabled {
		return nil, false, nil
	}
	fullAuth.ID = uuid
	fullAuth.Index = uuid
	return fullAuth, true, nil
}

// executionModelCandidates handles an execution model candidates.
func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	resolved := m.resolveDispatchModel(auth, routeModel)
	if strings.TrimSpace(resolved.Model) == "" {
		return nil
	}
	return []string{resolved.Model}
}

// shouldRefresh reports whether should refresh.
func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	// Resolve credential context before calling upstream OAuth services.
	if authRefreshDisabled(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return expiry.Sub(now) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

// ShouldRefreshCredential reports whether the credential is due for refresh.
func (m *Manager) ShouldRefreshCredential(auth *Auth, now time.Time) bool {
	if m == nil || auth == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return m.shouldRefresh(auth, now.UTC())
}

// authRefreshDisabled reports whether auth should be absent from auto-refresh scheduling.
func authRefreshDisabled(auth *Auth) bool {
	return auth == nil || auth.Disabled || auth.Status == StatusDisabled
}

// applyRefreshFailureState backs off only credential acquisition after a
// transient refresh failure. It must not make a still-valid access token
// unavailable for request dispatch.
func applyRefreshFailureState(auth *Auth, errRefresh error, now time.Time) {
	if auth == nil || errRefresh == nil {
		return
	}
	if isTerminalRefreshAuthError(errRefresh) {
		disableAuthAfterUnauthorized(auth, nil, newUnauthorizedRefreshError(), now)
		return
	}
	auth.NextRefreshAfter = now.Add(refreshFailureBackoff)
	auth.LastError = newTransientRefreshError()
	auth.UpdatedAt = now
}

func applyUnsupportedRefreshBackoff(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.NextRefreshAfter = now.Add(refreshFailureBackoff)
	auth.LastError = cloneError(ErrRefreshUnsupported)
	auth.UpdatedAt = now
}

// RefreshRetryBackoffOpen reports whether provider refresh calls are currently
// backed off. This state is intentionally ignored by request selection.
func RefreshRetryBackoffOpen(auth *Auth, now time.Time) bool {
	return auth != nil && auth.NextRefreshAfter.After(now)
}

func newTransientRefreshError() *Error {
	return &Error{
		Code:       refreshTransientErrorCode,
		Message:    refreshTransientErrorMsg,
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

// NewTransientRefreshError returns a redacted refresh acquisition error.
func NewTransientRefreshError() error {
	return newTransientRefreshError()
}

// newUnauthorizedRefreshError returns the fixed wire error for failed forced refresh.
func newUnauthorizedRefreshError() *Error {
	return &Error{
		Code:       refreshAuthErrorCode,
		Message:    refreshAuthErrorMsg,
		HTTPStatus: http.StatusUnauthorized,
	}
}

// isTerminalRefreshAuthError reports whether retrying the same refresh token
// cannot restore the credential. Generic HTTP 401 responses are not enough:
// proxies and provider edges can return them for transient failures.
func isTerminalRefreshAuthError(errRefresh error) bool {
	if errRefresh == nil || errors.Is(errRefresh, context.Canceled) || errors.Is(errRefresh, context.DeadlineExceeded) {
		return false
	}
	var authErr *Error
	if errors.As(errRefresh, &authErr) && authErr != nil {
		switch strings.ToLower(strings.TrimSpace(authErr.Code)) {
		case refreshAuthErrorCode:
			return strings.EqualFold(strings.TrimSpace(authErr.Message), refreshAuthErrorMsg)
		case "invalid_grant", "refresh_token_expired", "refresh_token_revoked", "refresh_token_reused":
			return true
		}
	}
	raw := strings.ToLower(errRefresh.Error())
	return strings.Contains(raw, "invalid_grant") ||
		strings.Contains(raw, "refresh_token_expired") ||
		strings.Contains(raw, "refresh_token_revoked") ||
		strings.Contains(raw, "refresh_token_reused") ||
		isTerminalOAuthRefreshDescription(raw)
}

func authSupportsBuiltInRefresh(auth *Auth) bool {
	if auth == nil || metaStringValue(auth.Metadata, "refresh_token") == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "codex", "claude", "kimi", "antigravity", "xai":
		return true
	default:
		return false
	}
}

func withRefreshProviderTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, refreshProviderCallTimeout)
}

// AuthIsNewerThanObserved reports whether Home stores a different access token
// from the one used by the failed downstream attempt.
func AuthIsNewerThanObserved(auth *Auth, observedAccessTokenSHA256 string) bool {
	observedHash, okObserved := normalizeSHA256Hex(observedAccessTokenSHA256)
	currentHash, okCurrent := normalizeSHA256Hex(AccessTokenSHA256(auth))
	return okObserved && okCurrent && currentHash != observedHash
}

func normalizeSHA256Hex(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", false
	}
	if _, errDecode := hex.DecodeString(value); errDecode != nil {
		return "", false
	}
	return value, true
}

// AccessTokenSHA256 fingerprints an OAuth token without exposing it.
func AccessTokenSHA256(auth *Auth) string {
	accessToken := accessTokenForFingerprint(auth)
	if accessToken == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(digest[:])
}

func accessTokenForFingerprint(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"access_token", "accessToken"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"token", "Token"} {
		switch token := auth.Metadata[key].(type) {
		case map[string]any:
			for _, tokenKey := range []string{"access_token", "accessToken"} {
				if value, ok := token[tokenKey].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		case map[string]string:
			for _, tokenKey := range []string{"access_token", "accessToken"} {
				if value := strings.TrimSpace(token[tokenKey]); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

// ApplyRefreshPendingState opens refresh-only backoff without changing request availability.
func ApplyRefreshPendingState(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	auth.UpdatedAt = now
}

// markRefreshPending handles a mark refresh pending.
func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	ApplyRefreshPendingState(auth, now)
	m.auths[id] = auth
	m.mu.Unlock()

	m.queueRefreshReschedule(id)
	return true
}

// authPreferredInterval handles an auth preferred interval.
func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

// durationFromMetadata derives duration from metadata.
func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

// durationFromAttributes derives duration from attributes.
func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

// parseDurationValue parses a duration value.
func parseDurationValue(val any) time.Duration {
	// Validate input data before converting it into runtime state.
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

// parseDurationString parses a duration string.
func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

// authLastRefreshTimestamp handles an auth last refresh timestamp.
func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

// lookupMetadataTime handles a lookup metadata time.
func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

// queueRefreshReschedule queues a refresh reschedule.
func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

// StartAutoRefresh starts an auto refresh.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	// Resolve credential context before calling upstream OAuth services.
	if m == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	loop := newAuthAutoRefreshLoop(m, interval, refreshMaxConcurrency)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.mu.Unlock()

	loop.rebuild(time.Now())
	go loop.run(ctx)
}

// StopAutoRefresh stops an auto refresh.
func (m *Manager) StopAutoRefresh() {
	m.stopAutoRefresh(false)
}

// Shutdown manages shutdown.
func (m *Manager) Shutdown() {
	m.stopAutoRefresh(true)
}

// stopAutoRefresh stops an auto refresh.
func (m *Manager) stopAutoRefresh(stopSelector bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !stopSelector {
		return
	}
	if stoppable, ok := m.selector.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

// RefreshNow forces a best-effort credential refresh for the given auth.
// It updates the in-memory record and persists it when enabled.
func (m *Manager) RefreshNow(ctx context.Context, authIndex string) (*Auth, error) {
	return m.RefreshNowObserved(ctx, authIndex, "")
}

// RefreshNowObserved refreshes unless Home already replaced the access token
// observed by the downstream attempt that received a 401.
func (m *Manager) RefreshNowObserved(ctx context.Context, authIndex, observedAccessTokenSHA256 string) (*Auth, error) {
	if m == nil {
		return nil, fmt.Errorf("auth manager: nil manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil, fmt.Errorf("auth manager: missing auth index")
	}

	m.mu.RLock()
	requested := m.indexAuth[authIndex]
	targetIndex := authIndex
	target := m.indexAuth[targetIndex]
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	m.mu.RUnlock()
	if requested == nil || target == nil {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	fullTarget, okFull, errFull := m.resolveFullRefreshAuth(ctx, target)
	if errFull != nil {
		return nil, errFull
	}
	if !okFull {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	target = fullTarget
	if authRefreshDisabled(target) {
		return nil, newUnauthorizedRefreshError()
	}
	if AuthIsNewerThanObserved(target, observedAccessTokenSHA256) {
		return target.Clone(), nil
	}
	if RefreshRetryBackoffOpen(target, time.Now().UTC()) {
		return nil, NewTransientRefreshError()
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	refreshCtx, cancelRefresh := withRefreshProviderTimeout(ctx)
	updated, handledPluginRefresh, errRefresh := m.refreshViaPlugin(refreshCtx, target)
	refreshAttempted := handledPluginRefresh
	if !handledPluginRefresh {
		refreshAttempted = authSupportsBuiltInRefresh(target)
		updated, errRefresh = refreshCredential(refreshCtx, cfg, target.Clone(), m.roundTripperFor(target))
	}
	cancelRefresh()
	now := time.Now().UTC()
	if errRefresh != nil {
		snapshot := target.Clone()
		terminalAuthFailure := isTerminalRefreshAuthError(errRefresh)
		applyRefreshFailureState(snapshot, errRefresh, now)
		if _, errUpdate := m.Update(ctx, snapshot); errUpdate != nil {
			return nil, errUpdate
		}
		if terminalAuthFailure {
			return nil, newUnauthorizedRefreshError()
		}
		if errors.Is(errRefresh, context.Canceled) || errors.Is(errRefresh, context.DeadlineExceeded) {
			return nil, errRefresh
		}
		return nil, NewTransientRefreshError()
	}
	if !refreshAttempted {
		snapshot := target.Clone()
		applyUnsupportedRefreshBackoff(snapshot, now)
		if _, errUpdate := m.Update(ctx, snapshot); errUpdate != nil {
			return nil, errUpdate
		}
		return nil, ErrRefreshUnsupported
	}
	if updated == nil {
		updated = target.Clone()
	}
	if updated.Runtime == nil {
		updated.Runtime = target.Runtime
	}
	modelsToResume := applyRefreshSuccessState(updated, now)
	if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	updatedPersisted, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		return nil, errUpdate
	}
	resumeRefreshedModels(updated.ID, modelsToResume)
	if targetIndex != authIndex {
		refreshed, ok := m.GetByIndex(authIndex)
		if !ok || refreshed == nil {
			return nil, fmt.Errorf("auth manager: auth not found")
		}
		return refreshed, nil
	}
	return updatedPersisted, nil
}

// RefreshAuthCredential refreshes a full auth value without updating memory or persistence.
func (m *Manager) RefreshAuthCredential(ctx context.Context, target *Auth) (*Auth, error) {
	if m == nil {
		return nil, fmt.Errorf("auth manager: nil manager")
	}
	if target == nil {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.RLock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	m.mu.RUnlock()
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	refreshCtx, cancelRefresh := withRefreshProviderTimeout(ctx)
	updated, handledPluginRefresh, errRefresh := m.refreshViaPlugin(refreshCtx, target)
	refreshAttempted := handledPluginRefresh
	if !handledPluginRefresh {
		refreshAttempted = authSupportsBuiltInRefresh(target)
		updated, errRefresh = refreshCredential(refreshCtx, cfg, target.Clone(), m.roundTripperFor(target))
	}
	cancelRefresh()
	now := time.Now().UTC()
	if errRefresh != nil {
		snapshot := target.Clone()
		terminalAuthFailure := isTerminalRefreshAuthError(errRefresh)
		applyRefreshFailureState(snapshot, errRefresh, now)
		if terminalAuthFailure {
			return snapshot, newUnauthorizedRefreshError()
		}
		if errors.Is(errRefresh, context.Canceled) || errors.Is(errRefresh, context.DeadlineExceeded) {
			return snapshot, errRefresh
		}
		return snapshot, NewTransientRefreshError()
	}
	if !refreshAttempted {
		snapshot := target.Clone()
		applyUnsupportedRefreshBackoff(snapshot, now)
		return snapshot, ErrRefreshUnsupported
	}
	if updated == nil {
		updated = target.Clone()
	}
	if updated.Runtime == nil {
		updated.Runtime = target.Runtime
	}
	applyRefreshSuccessState(updated, now)
	if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	return updated, nil
}

// refreshAuth refreshes an auth.
func (m *Manager) refreshAuth(ctx context.Context, authID string) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}

	m.mu.RLock()
	current := m.auths[authID]
	autoRefreshHandler := m.autoRefreshHandler
	m.mu.RUnlock()
	if current == nil {
		return
	}
	fullCurrent, okFull, errFull := m.resolveFullRefreshAuth(ctx, current)
	if errFull != nil {
		logEntryWithRequestID(ctx).Warnf("auth refresh full auth lookup failed | auth=%s err=%v", authID, errFull)
		return
	}
	if !okFull {
		return
	}
	current = fullCurrent
	now := time.Now().UTC()
	if !m.shouldRefresh(current, now) {
		return
	}

	if autoRefreshHandler != nil {
		if errRefresh := autoRefreshHandler(ctx, current.Clone()); errRefresh != nil {
			switch {
			case errors.Is(errRefresh, context.Canceled), errors.Is(errRefresh, context.DeadlineExceeded):
				log.Debugf("auth refresh canceled | auth=%s provider=%s", authID, current.Provider)
			case errors.Is(errRefresh, ErrRefreshUnsupported):
				log.Debugf("auth refresh unsupported | auth=%s provider=%s", authID, current.Provider)
			default:
				logEntryWithRequestID(ctx).Warnf("auth refresh failed | auth=%s provider=%s err=%s", authID, current.Provider, refreshTransientErrorMsg)
			}
		}
		return
	}

	beforeRefresh := current.Clone()
	updated, errRefresh := m.RefreshAuthCredential(ctx, current)
	if errRefresh != nil {
		if updated != nil {
			if _, errUpdate := m.Update(ctx, updated); errUpdate != nil {
				logEntryWithRequestID(ctx).Warnf("auth refresh failure state update failed | auth=%s provider=%s err=%v", authID, current.Provider, errUpdate)
			}
		}
		return
	}
	modelsToResume := refreshedUnauthorizedModels(beforeRefresh, updated)
	if _, errUpdate := m.Update(ctx, updated); errUpdate != nil {
		logEntryWithRequestID(ctx).Warnf("auth refresh state update failed | auth=%s provider=%s err=%v", authID, current.Provider, errUpdate)
		return
	}
	resumeRefreshedModels(updated.ID, modelsToResume)
}

// ApplyRefreshSuccessState clears refresh errors and unauthorized execution state.
func ApplyRefreshSuccessState(auth *Auth, now time.Time) []string {
	return applyRefreshSuccessState(auth, now)
}

// applyRefreshSuccessState clears only refresh errors and unauthorized execution state.
func applyRefreshSuccessState(auth *Auth, now time.Time) []string {
	if auth == nil {
		return nil
	}
	wasDisabled := auth.Disabled || auth.Status == StatusDisabled
	clearUnauthorizedAuth := isUnauthorizedAuthState(auth)
	clearRefreshAcquisition := isRefreshAcquisitionState(auth)

	auth.LastRefreshedAt = now
	auth.NextRefreshAfter = time.Time{}
	auth.UpdatedAt = now
	if clearRefreshAcquisition {
		auth.LastError = nil
	}
	if clearUnauthorizedAuth {
		auth.NextRetryAfter = time.Time{}
		auth.Unavailable = false
		auth.LastError = nil
		auth.StatusMessage = ""
	}

	resumed := make([]string, 0)
	for model, state := range auth.ModelStates {
		if !isUnauthorizedModelState(state) {
			continue
		}
		if resetUnauthorizedModelStateAfterRefresh(state, now) {
			resumed = append(resumed, model)
		}
	}
	if clearUnauthorizedAuth || len(resumed) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	if wasDisabled {
		auth.Status = StatusDisabled
		auth.Unavailable = true
	} else if hasModelError(auth, now) {
		auth.Status = StatusError
	} else if clearUnauthorizedAuth || len(resumed) > 0 {
		auth.Status = StatusActive
	}
	return resumed
}

func resetUnauthorizedModelStateAfterRefresh(state *ModelState, now time.Time) bool {
	if state == nil {
		return false
	}
	quota := state.Quota
	resetModelState(state, now)
	if quota.Exceeded && quota.NextRecoverAt.After(now) {
		state.Status = StatusError
		state.StatusMessage = strings.TrimSpace(quota.Reason)
		state.Unavailable = true
		state.NextRetryAfter = quota.NextRecoverAt
		state.Quota = quota
		return false
	}
	return true
}

func isRefreshAcquisitionState(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.LastError.Code)) {
	case refreshTransientErrorCode, refreshUnsupportedCode:
		return true
	default:
		return false
	}
}

func isUnauthorizedAuthState(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "unauthorized") {
		return true
	}
	return auth.LastError != nil && (auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(strings.TrimSpace(auth.LastError.Code), "unauthorized"))
}

func isUnauthorizedModelState(state *ModelState) bool {
	if state == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(state.StatusMessage), "unauthorized") {
		return true
	}
	return state.LastError != nil && (state.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(strings.TrimSpace(state.LastError.Code), "unauthorized"))
}

func refreshedUnauthorizedModels(before, after *Auth) []string {
	if before == nil || after == nil {
		return nil
	}
	models := make([]string, 0)
	for model, state := range before.ModelStates {
		if !isUnauthorizedModelState(state) {
			continue
		}
		if next := after.ModelStates[model]; next == nil || !isUnauthorizedModelState(next) {
			models = append(models, model)
		}
	}
	return models
}

func resumeRefreshedModels(authID string, models []string) {
	for _, model := range models {
		registry.GetGlobalRegistry().ResumeClientModel(authID, model)
	}
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}
