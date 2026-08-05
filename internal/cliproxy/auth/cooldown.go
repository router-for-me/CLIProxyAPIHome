package auth

import (
	"context"
	"errors"
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
	clearedModels []string
}

func (t cooldownClearTransition) changed() bool {
	return len(t.clearedModels) > 0
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

	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	mutator, ok := store.(StateMutator)
	if !ok || mutator == nil {
		return result, ErrCooldownMutationUnsupported
	}

	now := time.Now().UTC()
	transition := cooldownClearTransition{}
	persisted, errMutate := mutator.MutateAuthState(ctx, credentialID, func(auth *Auth) bool {
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
		transition = clearQuotaCooldownState(auth, modelKeys, now)
		return transition.changed()
	})
	if errMutate != nil {
		return result, errMutate
	}
	if persisted == nil {
		return result, errors.New("auth manager: cooldown reset returned no auth")
	}

	authSnapshot, adopted := m.adoptPersistedCooldownState(persisted, now)
	if adopted && m.scheduler != nil && authSnapshot != nil {
		m.scheduler.upsertAuth(authSnapshot)
	}
	if adopted {
		m.ReconcileRegistryModelStates(ctx, credentialID)
	}

	result.Cleared = transition.changed()
	result.ClearedModels = append([]string(nil), transition.clearedModels...)
	return result, nil
}

func clearQuotaCooldownState(auth *Auth, models []string, now time.Time) cooldownClearTransition {
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

	preserveLegacyCredentialQuota := hasLegacyCredentialQuota(auth)
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
	if state == nil || !state.Quota.Exceeded {
		return false
	}
	quotaOwnsAvailability := state.LastError == nil || statusCodeFromResult(state.LastError) == http.StatusTooManyRequests
	state.Quota = QuotaState{}
	if state.Status != StatusDisabled && quotaOwnsAvailability {
		state.Status = StatusActive
		state.StatusMessage = ""
		state.LastError = nil
		state.Unavailable = false
		state.NextRetryAfter = time.Time{}
	}
	// Keep the execution timestamp unchanged. Older Home versions merge model
	// states only by UpdatedAt, so advancing it for an operator reset would let
	// the cleared quota snapshot overwrite a newer local 401/403/404/5xx state
	// during a rolling upgrade. QuotaResetAt carries the mutation timestamp for
	// reset-aware peers and state-version fingerprints.
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
