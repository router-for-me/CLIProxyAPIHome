package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrCooldownMutationUnsupported indicates that the auth store cannot apply an
// atomic cooldown reset. Home requires atomic mutation so multiple instances
// sharing one database cannot overwrite each other's credential state.
var ErrCooldownMutationUnsupported = errors.New("auth manager: cooldown reset requires an atomic state store")

// CooldownClearResult describes quota cooldown state removed by an operator.
type CooldownClearResult struct {
	CredentialID  string
	Model         string
	Cleared       bool
	ClearedModels []string
}

type cooldownClearTransition struct {
	clearedModels     []string
	clearedCredential bool
}

func (t cooldownClearTransition) changed() bool {
	return t.clearedCredential || len(t.clearedModels) > 0
}

type cooldownMutationResult struct {
	authSnapshot *Auth
	changed      bool
	adopted      bool
}

// mutateCooldownState applies one cooldown transition to the authoritative
// auth row and synchronizes the local scheduler and registry with the result.
// Both operator resets and global disable-cooling use this same pipeline so
// persisted state remains authoritative across Home nodes. persistUnchanged
// advances the row revision even when the stored state is already clean, which
// fences older result snapshots still waiting in the asynchronous save queue.
func (m *Manager) mutateCooldownState(ctx context.Context, credentialID string, now time.Time, persistUnchanged bool, mutate func(*Auth) bool) (cooldownMutationResult, error) {
	result := cooldownMutationResult{}
	if m == nil {
		return result, errors.New("auth manager: nil manager")
	}
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return result, errors.New("auth manager: missing auth id")
	}
	if mutate == nil {
		return result, errors.New("auth manager: missing cooldown mutation")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	mutator, ok := store.(StateMutator)
	if !ok || mutator == nil {
		return result, ErrCooldownMutationUnsupported
	}

	persisted, errMutate := mutator.MutateAuthState(ctx, credentialID, func(auth *Auth) bool {
		result.changed = mutate(auth)
		return result.changed || persistUnchanged
	})
	if errMutate != nil {
		return result, errMutate
	}
	if persisted == nil {
		return result, errors.New("auth manager: cooldown reset returned no auth")
	}

	result.authSnapshot, result.adopted = m.adoptPersistedCooldownState(persisted, now)
	if result.adopted && result.authSnapshot != nil {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(result.authSnapshot)
		}
		m.ReconcileRegistryModelStates(ctx, credentialID)
	}
	return result, nil
}

// ClearQuotaCooldown atomically clears model quota cooldown state for one
// credential. An empty model clears every model; a non-empty model clears only
// that canonical model key.
func (m *Manager) ClearQuotaCooldown(ctx context.Context, credentialID, model string) (CooldownClearResult, error) {
	result := CooldownClearResult{}
	if m == nil {
		return result, errors.New("auth manager: nil manager")
	}
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return result, errors.New("auth manager: missing auth id")
	}
	requestedModel := canonicalModelKey(model)
	result.CredentialID = credentialID
	result.Model = requestedModel
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	transition := cooldownClearTransition{}
	mutation, errMutate := m.mutateCooldownState(ctx, credentialID, now, false, func(auth *Auth) bool {
		modelKeys := []string(nil)
		if requestedModel != "" {
			resolvedModel := requestedModel
			if resolved := m.resolveDispatchModel(auth, requestedModel); resolved.Key != "" {
				resolvedModel = resolved.Key
			}
			result.Model = resolvedModel
			modelKeys = append(modelKeys, resolvedModel)
			if requestedModel != resolvedModel {
				// Clear the pre-upstream-key route state too so rolling upgrades do
				// not leave the scheduler blocked by its compatibility fallback.
				modelKeys = append(modelKeys, requestedModel)
			}
		}
		transition = clearQuotaCooldownState(auth, modelKeys, now, false)
		return transition.changed()
	})
	if errMutate != nil {
		return result, errMutate
	}

	result.Cleared = mutation.changed
	result.ClearedModels = append([]string(nil), transition.clearedModels...)
	return result, nil
}

// ClearDisabledCooldownStates clears request-error and quota cooldown state
// covered by the global or auth-scoped disable-cooling setting.
// Cluster-backed stores are mutated through StateMutator so the persisted row
// remains authoritative across Home nodes.
func (m *Manager) ClearDisabledCooldownStates(ctx context.Context) error {
	return m.clearDisabledCooldownStates(ctx, false)
}

// FenceDisabledCooldownStates clears covered cooldown state and advances the
// revision of auths with queued result snapshots or a prior failed cleanup. It
// is used when global disable-cooling changes so older asynchronous snapshots
// cannot restore a cooldown after the configuration switch.
func (m *Manager) FenceDisabledCooldownStates(ctx context.Context) error {
	return m.clearDisabledCooldownStates(ctx, true)
}

func (m *Manager) clearDisabledCooldownStates(ctx context.Context, forceFence bool) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.resultPersistMu.Lock()
	fenceAuthIDs := make(map[string]struct{}, len(m.cooldownFencePending)+len(m.resultPersistPending)+len(m.resultPersistActive))
	for authID := range m.cooldownFencePending {
		fenceAuthIDs[authID] = struct{}{}
	}
	if forceFence {
		for authID := range m.resultPersistPending {
			fenceAuthIDs[authID] = struct{}{}
		}
		for authID := range m.resultPersistActive {
			fenceAuthIDs[authID] = struct{}{}
		}
	}
	m.resultPersistMu.Unlock()

	m.mu.RLock()
	store := m.store
	authIDs := make([]string, 0, len(m.auths))
	for authID, auth := range m.auths {
		_, needsFence := fenceAuthIDs[authID]
		if strings.TrimSpace(authID) != "" &&
			m.quotaCooldownDisabledForAuth(auth) &&
			(needsFence || hasDisabledCooldownState(auth)) {
			authIDs = append(authIDs, authID)
		}
	}
	m.mu.RUnlock()
	sort.Strings(authIDs)
	if len(authIDs) == 0 {
		return nil
	}

	now := time.Now().UTC()
	mutator, hasMutator := store.(StateMutator)
	if !hasMutator || mutator == nil {
		if forceFence {
			m.resultPersistMu.Lock()
			if m.cooldownFencePending == nil {
				m.cooldownFencePending = make(map[string]struct{}, len(authIDs))
			}
			for _, authID := range authIDs {
				m.cooldownFencePending[authID] = struct{}{}
			}
			m.resultPersistMu.Unlock()
			return ErrCooldownMutationUnsupported
		}
		var firstErr error
		failedAuthIDs := make(map[string]struct{})
		for _, authID := range authIDs {
			_, needsFence := fenceAuthIDs[authID]
			snapshot, changed := m.clearLocalDisabledCooldownState(ctx, authID, now)
			if snapshot == nil {
				continue
			}
			if needsFence {
				failedAuthIDs[authID] = struct{}{}
				if firstErr == nil {
					firstErr = ErrCooldownMutationUnsupported
				}
			}
			if !changed {
				continue
			}
			if errPersist := m.persist(ctx, snapshot); errPersist != nil {
				failedAuthIDs[authID] = struct{}{}
				if firstErr == nil {
					firstErr = errPersist
				}
			}
		}
		m.resultPersistMu.Lock()
		if m.cooldownFencePending == nil {
			m.cooldownFencePending = make(map[string]struct{}, len(failedAuthIDs))
		}
		for _, authID := range authIDs {
			if _, failed := failedAuthIDs[authID]; failed {
				m.cooldownFencePending[authID] = struct{}{}
			} else {
				delete(m.cooldownFencePending, authID)
			}
		}
		m.resultPersistMu.Unlock()
		return firstErr
	}

	var firstErr error
	failedAuthIDs := make(map[string]struct{})
	for _, authID := range authIDs {
		_, errMutate := m.mutateCooldownState(ctx, authID, now, true, func(auth *Auth) bool {
			return clearDisabledCooldownState(auth, now)
		})
		if errMutate != nil {
			failedAuthIDs[authID] = struct{}{}
			// The config switch must take effect locally even when the shared row
			// cannot be updated yet. A later auth reload will retry persistence.
			m.clearLocalDisabledCooldownState(ctx, authID, now)
			if firstErr == nil {
				firstErr = fmt.Errorf("clear disabled cooldown state for %s: %w", authID, errMutate)
			}
			continue
		}

		// The persisted row may already be clear while this node still has a
		// local cooldown from an older observation. The shared mutation pipeline
		// first adopts the authoritative row; this local pass then removes only
		// the covered cooldown fields that the merge intentionally retained.
		m.clearLocalDisabledCooldownState(ctx, authID, now)
	}
	m.resultPersistMu.Lock()
	if m.cooldownFencePending == nil {
		m.cooldownFencePending = make(map[string]struct{}, len(failedAuthIDs))
	}
	for _, authID := range authIDs {
		if _, failed := failedAuthIDs[authID]; failed {
			m.cooldownFencePending[authID] = struct{}{}
		} else {
			delete(m.cooldownFencePending, authID)
		}
	}
	m.resultPersistMu.Unlock()
	return firstErr
}

func (m *Manager) clearLocalDisabledCooldownState(ctx context.Context, authID string, now time.Time) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil {
		m.mu.Unlock()
		return nil, false
	}
	changed := clearDisabledCooldownState(auth, now)
	snapshot := auth.Clone()
	m.mu.Unlock()
	if changed {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
		m.ReconcileRegistryModelStates(ctx, authID)
	}
	return snapshot, changed
}

func hasDisabledCooldownState(auth *Auth) bool {
	if auth == nil {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
			return true
		}
		if state.Status == StatusDisabled {
			continue
		}
		status := statusCodeFromResult(state.LastError)
		legacyRequestError := status == 0 && !state.Quota.Exceeded
		quotaOwnsRetry := state.Quota.Exceeded && state.NextRetryAfter.Equal(state.Quota.NextRecoverAt)
		if !state.NextRetryAfter.IsZero() && !quotaOwnsRetry && (legacyRequestError || isRequestErrorCooldownStatus(status)) {
			return true
		}
	}
	if auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero() {
		return true
	}
	if auth.Disabled || auth.Status == StatusDisabled || len(auth.ModelStates) > 0 {
		return false
	}
	if RefreshBlocksDispatch(auth) {
		return false
	}
	authStatus := statusCodeFromResult(auth.LastError)
	legacyRequestError := authStatus == 0 && !auth.Quota.Exceeded
	quotaOwnsRetry := auth.Quota.Exceeded && auth.NextRetryAfter.Equal(auth.Quota.NextRecoverAt)
	return !auth.NextRetryAfter.IsZero() && !quotaOwnsRetry && (legacyRequestError || isRequestErrorCooldownStatus(authStatus))
}

func clearDisabledCooldownState(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	preserveRefreshAcquisition := !auth.Disabled && auth.Status != StatusDisabled && RefreshBlocksDispatch(auth)
	refreshStatus := auth.Status
	refreshStatusMessage := auth.StatusMessage
	refreshLastError := cloneError(auth.LastError)
	refreshUnavailable := auth.Unavailable
	refreshNextRetryAfter := auth.NextRetryAfter
	refreshRuntimeBlocked := auth.RuntimeRefreshBlocked
	quotaTransition := clearQuotaCooldownState(auth, nil, now, true)

	wasDisabled := auth.Disabled || auth.Status == StatusDisabled
	disabledStatus := auth.Status
	disabledMessage := auth.StatusMessage
	disabledLastError := cloneError(auth.LastError)
	disabledUnavailable := auth.Unavailable
	disabledRetryAfter := auth.NextRetryAfter
	disabledQuota := auth.Quota
	authStatus := statusCodeFromResult(auth.LastError)
	preserveAuthState := auth.LastError != nil &&
		!isRequestErrorCooldownStatus(authStatus) &&
		!authErrorMirroredByModel(auth)
	preservedUnavailable := auth.Unavailable
	preservedRetryAfter := auth.NextRetryAfter
	changed := quotaTransition.changed()

	for _, state := range auth.ModelStates {
		if state == nil || state.Status == StatusDisabled {
			continue
		}
		status := statusCodeFromResult(state.LastError)
		legacyRequestError := status == 0 && !state.Quota.Exceeded
		quotaOwnsRetry := state.Quota.Exceeded && state.NextRetryAfter.Equal(state.Quota.NextRecoverAt)
		if state.NextRetryAfter.IsZero() || quotaOwnsRetry || (!legacyRequestError && !isRequestErrorCooldownStatus(status)) {
			continue
		}
		if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) {
			state.Unavailable = true
			state.NextRetryAfter = state.Quota.NextRecoverAt
		} else {
			state.Unavailable = false
			state.NextRetryAfter = time.Time{}
		}
		// Keep the execution timestamp so this reset cannot overwrite a newer
		// non-covered model error observed by another Home node.
		changed = true
	}

	legacyAuthRequestError := authStatus == 0 && !auth.Quota.Exceeded
	quotaOwnsAuthRetry := auth.Quota.Exceeded && auth.NextRetryAfter.Equal(auth.Quota.NextRecoverAt)
	clearAuthRetry := !wasDisabled && !preserveRefreshAcquisition && len(auth.ModelStates) == 0 && !auth.NextRetryAfter.IsZero() &&
		!quotaOwnsAuthRetry && (legacyAuthRequestError || isRequestErrorCooldownStatus(authStatus))
	if clearAuthRetry {
		if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
			auth.Unavailable = true
			auth.NextRetryAfter = auth.Quota.NextRecoverAt
		} else {
			auth.Unavailable = false
			auth.NextRetryAfter = time.Time{}
		}
		auth.UpdatedAt = now
		changed = true
	}

	if !changed {
		return false
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	if wasDisabled {
		auth.Disabled = true
		auth.Status = disabledStatus
		auth.StatusMessage = disabledMessage
		auth.LastError = disabledLastError
		auth.Unavailable = disabledUnavailable
		auth.NextRetryAfter = disabledRetryAfter
		auth.Quota = disabledQuota
	} else if preserveAuthState {
		auth.Unavailable = preservedUnavailable
		auth.NextRetryAfter = preservedRetryAfter
	}
	if preserveRefreshAcquisition {
		auth.Status = refreshStatus
		auth.StatusMessage = refreshStatusMessage
		auth.LastError = refreshLastError
		auth.Unavailable = refreshUnavailable
		auth.NextRetryAfter = refreshNextRetryAfter
		auth.RuntimeRefreshBlocked = refreshRuntimeBlocked
	}
	auth.UpdatedAt = now
	return true
}

func isRequestErrorCooldownStatus(status int) bool {
	switch status {
	case http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusRequestTimeout,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func clearQuotaCooldownState(auth *Auth, models []string, now time.Time, clearCredentialQuota bool) cooldownClearTransition {
	transition := cooldownClearTransition{}
	if auth == nil {
		return transition
	}

	wasDisabled := auth.Disabled || auth.Status == StatusDisabled
	disabledStatus := auth.Status
	disabledMessage := auth.StatusMessage
	disabledLastError := cloneError(auth.LastError)
	disabledUnavailable := auth.Unavailable
	disabledRetryAfter := auth.NextRetryAfter

	preserveLegacyCredentialQuota := !clearCredentialQuota && hasLegacyCredentialQuota(auth)
	clearLegacyCredentialQuota := clearCredentialQuota && (auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.IsZero())
	preservedAuthQuota := auth.Quota
	preservedAuthStatus := auth.Status
	preservedAuthMessage := auth.StatusMessage
	preservedAuthLastError := cloneError(auth.LastError)
	preservedAuthUnavailable := auth.Unavailable
	preservedAuthRetryAfter := auth.NextRetryAfter
	preserveNonQuotaAuthError := auth.LastError != nil && statusCodeFromResult(auth.LastError) != http.StatusTooManyRequests && !authErrorMirroredByModel(auth)

	if len(models) > 0 {
		seen := make(map[string]struct{}, len(models))
		for _, modelID := range models {
			modelID = canonicalModelKey(modelID)
			if modelID == "" {
				continue
			}
			if _, exists := seen[modelID]; exists {
				continue
			}
			seen[modelID] = struct{}{}
			if state := auth.ModelStates[modelID]; clearModelQuotaCooldown(state, now) {
				transition.clearedModels = append(transition.clearedModels, modelID)
			}
		}
		sort.Strings(transition.clearedModels)
	} else {
		allModels := make([]string, 0, len(auth.ModelStates))
		for modelID := range auth.ModelStates {
			allModels = append(allModels, modelID)
		}
		sort.Strings(allModels)
		for _, modelID := range allModels {
			state := auth.ModelStates[modelID]
			if !clearModelQuotaCooldown(state, now) {
				continue
			}
			transition.clearedModels = append(transition.clearedModels, modelID)
		}
	}
	if clearLegacyCredentialQuota {
		auth.Quota = QuotaState{}
		transition.clearedCredential = true
	}

	if !transition.changed() {
		return transition
	}

	updateAggregatedAvailability(auth, now)
	if preserveLegacyCredentialQuota {
		auth.Quota = preservedAuthQuota
	}
	switch {
	case wasDisabled:
		auth.Disabled = true
		auth.Status = disabledStatus
		auth.StatusMessage = disabledMessage
		auth.LastError = disabledLastError
		auth.Unavailable = disabledUnavailable
		auth.NextRetryAfter = disabledRetryAfter
	case preserveLegacyCredentialQuota:
		auth.Status = preservedAuthStatus
		auth.StatusMessage = preservedAuthMessage
		auth.LastError = preservedAuthLastError
		auth.Unavailable = preservedAuthUnavailable
		auth.NextRetryAfter = preservedAuthRetryAfter
	case preserveNonQuotaAuthError:
		auth.Status = preservedAuthStatus
		auth.StatusMessage = preservedAuthMessage
		auth.LastError = preservedAuthLastError
		auth.Unavailable = preservedAuthUnavailable
		auth.NextRetryAfter = preservedAuthRetryAfter
	case !auth.Quota.Exceeded && !hasModelError(auth, now):
		auth.Status = StatusActive
		auth.StatusMessage = ""
		auth.LastError = nil
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
	}
	reconcileAuthErrorAfterQuotaClear(auth)
	auth.UpdatedAt = now
	return transition
}

func hasLegacyCredentialQuota(auth *Auth) bool {
	if auth == nil || !auth.Quota.Exceeded {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Quota.Scope)) {
	case "credential":
		return true
	case quotaScopeModel:
		return false
	}

	// Quota scope was not persisted before Home introduced model-only
	// scheduling. Distinguish the old model aggregate from an independent
	// credential quota so an operator reset changes only ModelStates.
	hasModelQuota := false
	recoverAt := time.Time{}
	maxBackoffLevel := 0
	for _, state := range auth.ModelStates {
		if state == nil || !state.Quota.Exceeded {
			continue
		}
		hasModelQuota = true
		if recoverAt.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(recoverAt)) {
			recoverAt = state.Quota.NextRecoverAt
		}
		if state.Quota.BackoffLevel > maxBackoffLevel {
			maxBackoffLevel = state.Quota.BackoffLevel
		}
	}
	return !hasModelQuota ||
		!strings.EqualFold(strings.TrimSpace(auth.Quota.Reason), "quota") ||
		!auth.Quota.NextRecoverAt.Equal(recoverAt) ||
		auth.Quota.BackoffLevel != maxBackoffLevel
}

func clearModelQuotaCooldown(state *ModelState, now time.Time) bool {
	if state == nil || (!state.Quota.Exceeded && state.Quota.NextRecoverAt.IsZero()) {
		return false
	}
	quotaOwnsAvailability := state.LastError == nil || statusCodeFromResult(state.LastError) == http.StatusTooManyRequests
	clearModelQuotaState(state, now)
	if state.Status != StatusDisabled && quotaOwnsAvailability {
		state.Status = StatusActive
		state.StatusMessage = ""
		state.LastError = nil
		state.Unavailable = false
		state.NextRetryAfter = time.Time{}
	}
	return true
}

func clearModelQuotaState(state *ModelState, now time.Time) bool {
	if state == nil || (!state.Quota.Exceeded && state.Quota.NextRecoverAt.IsZero()) {
		return false
	}
	state.Quota = QuotaState{}
	// Keep the execution timestamp unchanged. Older Home versions merge model
	// states only by UpdatedAt, so advancing it for a reset would let the cleared
	// quota snapshot overwrite a newer local 401/403/404/5xx state during a
	// rolling upgrade. QuotaResetAt carries the mutation timestamp instead.
	state.QuotaResetAt = now
	return true
}

func authErrorMirroredByModel(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil || state.LastError == nil {
			continue
		}
		if state.LastError.HTTPStatus == auth.LastError.HTTPStatus &&
			strings.TrimSpace(state.LastError.Code) == strings.TrimSpace(auth.LastError.Code) &&
			strings.TrimSpace(state.LastError.Message) == strings.TrimSpace(auth.LastError.Message) {
			return true
		}
	}
	return false
}

func reconcileAuthErrorAfterQuotaClear(auth *Auth) {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || auth.Quota.Exceeded || auth.LastError == nil || statusCodeFromResult(auth.LastError) != http.StatusTooManyRequests {
		return
	}
	var latest *ModelState
	for _, state := range auth.ModelStates {
		if state == nil || state.LastError == nil || state.Quota.Exceeded {
			continue
		}
		if latest == nil || state.UpdatedAt.After(latest.UpdatedAt) {
			latest = state
		}
	}
	if latest == nil {
		return
	}
	auth.Status = latest.Status
	if auth.Status == StatusActive || auth.Status == StatusDisabled {
		auth.Status = StatusError
	}
	auth.StatusMessage = latest.StatusMessage
	auth.LastError = cloneError(latest.LastError)
}

func mergePersistedCooldownModelStates(persisted, local map[string]*ModelState) map[string]*ModelState {
	merged := make(map[string]*ModelState, len(persisted)+len(local))
	for model, state := range persisted {
		merged[model] = state.Clone()
	}
	for model, localState := range local {
		if localState == nil {
			continue
		}
		merged[model] = MergePersistedModelState(merged[model], localState)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// MergePersistedModelState combines authoritative persisted quota state with
// newer execution-only state accumulated by one Home instance.
func MergePersistedModelState(persistedState, localState *ModelState) *ModelState {
	if persistedState == nil {
		if localState == nil {
			return nil
		}
		return localState.Clone()
	}
	if localState == nil {
		return persistedState.Clone()
	}

	if !persistedState.QuotaResetAt.IsZero() {
		// A reset owns quota state even when a repeated in-window 429 gave the
		// local copy a newer execution timestamp.
		if modelHasNonQuotaState(localState) {
			return mergePersistedQuotaIntoLocalState(persistedState, localState)
		}
		return persistedState.Clone()
	}
	if persistedState.Quota.Exceeded {
		// Shared quota remains authoritative. Preserve a newer local non-quota
		// error without shortening the persisted quota recovery window.
		if localState.UpdatedAt.After(persistedState.UpdatedAt) && modelHasNonQuotaState(localState) {
			return mergePersistedQuotaIntoLocalState(persistedState, localState)
		}
		return persistedState.Clone()
	}
	if localState.UpdatedAt.After(persistedState.UpdatedAt) {
		return localState.Clone()
	}
	return persistedState.Clone()
}

func mergePersistedQuotaIntoLocalState(persistedState, localState *ModelState) *ModelState {
	merged := localState.Clone()
	merged.Quota = persistedState.Quota
	merged.QuotaResetAt = persistedState.QuotaResetAt
	if !persistedState.Quota.Exceeded {
		return merged
	}

	merged.Unavailable = true
	persistedRetry := persistedState.NextRetryAfter
	if persistedState.Quota.NextRecoverAt.After(persistedRetry) {
		persistedRetry = persistedState.Quota.NextRecoverAt
	}
	if persistedRetry.After(merged.NextRetryAfter) {
		merged.NextRetryAfter = persistedRetry
	}
	return merged
}

func modelHasNonQuotaState(state *ModelState) bool {
	if state == nil {
		return false
	}
	if state.Status == StatusDisabled {
		return true
	}
	if state.LastError != nil {
		return statusCodeFromResult(state.LastError) != http.StatusTooManyRequests
	}
	return state.Status == StatusError && !state.Quota.Exceeded
}

func (m *Manager) adoptPersistedCooldownState(persisted *Auth, now time.Time) (*Auth, bool) {
	if m == nil || persisted == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	local := m.auths[persisted.ID]
	if local == nil {
		return persisted.Clone(), false
	}
	if local.StateVersion > 0 && persisted.StateVersion > 0 && local.StateVersion > persisted.StateVersion {
		return local.Clone(), false
	}

	persistedClone := persisted.Clone()
	persistedLegacyCredentialQuota := hasLegacyCredentialQuota(persistedClone)
	persistedGlobalError := persistedClone.LastError != nil && statusCodeFromResult(persistedClone.LastError) != http.StatusTooManyRequests && !authErrorMirroredByModel(persistedClone)
	local.StateVersion = persistedClone.StateVersion
	local.Disabled = persistedClone.Disabled
	local.Status = persistedClone.Status
	local.StatusMessage = persistedClone.StatusMessage
	local.LastError = cloneError(persistedClone.LastError)
	local.Unavailable = persistedClone.Unavailable
	local.RuntimeRefreshBlocked = RefreshBlocksDispatch(persistedClone)
	local.NextRetryAfter = persistedClone.NextRetryAfter
	local.Quota = persistedClone.Quota
	local.ModelStates = mergePersistedCooldownModelStates(persistedClone.ModelStates, local.ModelStates)
	local.UpdatedAt = persistedClone.UpdatedAt

	if !local.Disabled && local.Status != StatusDisabled {
		updateAggregatedAvailability(local, now)
		switch {
		case persistedLegacyCredentialQuota:
			local.Status = persistedClone.Status
			local.StatusMessage = persistedClone.StatusMessage
			local.LastError = cloneError(persistedClone.LastError)
			local.Unavailable = persistedClone.Unavailable
			local.NextRetryAfter = persistedClone.NextRetryAfter
			local.Quota = persistedClone.Quota
		case persistedGlobalError:
			local.Status = persistedClone.Status
			local.StatusMessage = persistedClone.StatusMessage
			local.LastError = cloneError(persistedClone.LastError)
			local.Unavailable = persistedClone.Unavailable
			local.NextRetryAfter = persistedClone.NextRetryAfter
		}
		reconcileAuthErrorAfterQuotaClear(local)
	}
	return local.Clone(), true
}
