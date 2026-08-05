package auth

import (
	"net/http"
)

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
