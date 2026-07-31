package auth

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	quotaBackoffBase = time.Second
	quotaBackoffMax  = 30 * time.Minute
)

var usageRetryDelayPattern = regexp.MustCompile(`(?i)\b(?:resets in|after)\s+([0-9]+(?:\.[0-9]+)?(?:h|m|s)(?:[0-9]+(?:\.[0-9]+)?(?:h|m|s))*)`)

// Result captures an upstream execution result reported by a downstream CPA node.
type Result struct {
	AuthID     string
	AuthIndex  string
	Provider   string
	Model      string
	Success    bool
	Error      *Error
	RetryAfter *time.Duration
}

// markResultTransition captures the registry side effects derived from a result transition.
type markResultTransition struct {
	shouldResumeModel  bool
	shouldSuspendModel bool
	suspendReason      string
	clearModelQuota    bool
	setModelQuota      bool
}

// MarkResult records a downstream execution result and updates auth cooldown state.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	// Keep validation before state changes so failures leave existing data intact.
	if m == nil || (strings.TrimSpace(result.AuthID) == "" && strings.TrimSpace(result.AuthIndex) == "") {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	transition := markResultTransition{}
	var authSnapshot *Auth
	resultModel := canonicalModelKey(result.Model)
	resultAuthID := ""
	now := time.Now()

	m.mu.Lock()
	auth := m.resultAuthLocked(result)
	var mutator StateMutator
	if auth != nil {
		resultAuthID = auth.ID
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		if stateMutator, ok := m.store.(StateMutator); ok && m.resultNeedsGlobalTransition(auth, result, resultModel, now) {
			mutator = stateMutator
		}
		if mutator == nil {
			transition = m.applyResultTransition(auth, result, resultModel, now)
			authSnapshot = auth.Clone()
		}
	}
	m.mu.Unlock()
	if resultAuthID == "" {
		return
	}

	if mutator != nil {
		// Apply the transition against the persisted auth so concurrent quota
		// results reported to other Home nodes cannot clobber the shared state.
		persisted, errMutate := mutator.MutateAuthState(ctx, resultAuthID, func(persisted *Auth) bool {
			before := availabilityFingerprint(persisted, resultModel)
			transition = m.applyResultTransition(persisted, result, resultModel, now)
			return availabilityFingerprint(persisted, resultModel) != before
		})
		if errMutate != nil || persisted == nil {
			if errMutate != nil {
				log.Warnf("auth manager: persisted result transition failed for %s, applying locally: %v", resultAuthID, errMutate)
			}
			m.mu.Lock()
			if local := m.resultAuthLocked(result); local != nil {
				transition = m.applyResultTransition(local, result, resultModel, now)
				authSnapshot = local.Clone()
			}
			m.mu.Unlock()
			m.enqueueResultPersist(ctx, authSnapshot)
		} else {
			authSnapshot = m.adoptPersistedResultState(result, resultModel, persisted, now)
		}
	} else {
		m.enqueueResultPersist(ctx, authSnapshot)
	}

	if m.scheduler != nil && authSnapshot != nil {
		m.scheduler.upsertAuth(authSnapshot)
	}
	if authSnapshot != nil && authRefreshDisabled(authSnapshot) {
		m.queueRefreshReschedule(authSnapshot.ID)
	}
	if transition.clearModelQuota && resultModel != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(resultAuthID, resultModel)
	}
	if transition.setModelQuota && resultModel != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(resultAuthID, resultModel)
	}
	if transition.shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(resultAuthID, resultModel)
	} else if transition.shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(resultAuthID, resultModel, transition.suspendReason)
	}
}

// applyResultTransition applies the result state machine to the provided auth
// and reports the derived registry side effects. The auth may be the manager's
// in-memory copy or a persisted copy loaded by a StateMutator; the function
// must not touch manager state beyond configuration reads.
func (m *Manager) applyResultTransition(auth *Auth, result Result, resultModel string, now time.Time) markResultTransition {
	transition := markResultTransition{}
	if auth == nil {
		return transition
	}

	if result.Success {
		if resultModel != "" {
			state := ensureModelState(auth, resultModel)
			resetModelState(state, now)
			updateAggregatedAvailability(auth, now)
			if !hasModelError(auth, now) {
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
			}
			auth.UpdatedAt = now
			transition.shouldResumeModel = true
			transition.clearModelQuota = true
		} else {
			clearAuthStateOnSuccess(auth, now)
		}
		return transition
	}

	if resultModel != "" {
		if isRequestScopedNotFoundResultError(result.Error) {
			return transition
		}
		disableCooling := m.quotaCooldownDisabledForAuth(auth)
		state := ensureModelState(auth, resultModel)
		state.Unavailable = true
		state.Status = StatusError
		state.UpdatedAt = now
		if result.Error != nil {
			state.LastError = cloneError(result.Error)
			state.StatusMessage = result.Error.Message
			auth.LastError = cloneError(result.Error)
			auth.StatusMessage = result.Error.Message
		}

		statusCode := statusCodeFromResult(result.Error)
		disableAuth := false
		if isModelSupportResultError(result.Error) {
			next := now.Add(12 * time.Hour)
			state.NextRetryAfter = next
			transition.suspendReason = "model_not_supported"
			transition.shouldSuspendModel = true
		} else {
			switch statusCode {
			case http.StatusUnauthorized:
				disableAuth = true
				transition.suspendReason = "unauthorized"
				transition.shouldSuspendModel = true
			case http.StatusPaymentRequired, http.StatusForbidden:
				if disableCooling {
					state.NextRetryAfter = time.Time{}
				} else {
					next := now.Add(30 * time.Minute)
					state.NextRetryAfter = next
					transition.suspendReason = "payment_required"
					transition.shouldSuspendModel = true
				}
			case http.StatusNotFound:
				if disableCooling {
					state.NextRetryAfter = time.Time{}
				} else {
					next := now.Add(12 * time.Hour)
					state.NextRetryAfter = next
					transition.suspendReason = "not_found"
					transition.shouldSuspendModel = true
				}
			case http.StatusTooManyRequests:
				next, backoffLevel := nextQuotaRecoverAt(now, result.RetryAfter, state.Quota, disableCooling)
				state.NextRetryAfter = next
				state.Quota = QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
					BackoffLevel:  backoffLevel,
				}
				if !disableCooling {
					transition.suspendReason = "quota"
					transition.shouldSuspendModel = true
					transition.setModelQuota = true
				}
			case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				if disableCooling {
					state.NextRetryAfter = time.Time{}
				} else {
					state.NextRetryAfter = now.Add(time.Minute)
				}
			default:
				state.NextRetryAfter = time.Time{}
			}
		}

		if disableAuth {
			disableAuthAfterUnauthorized(auth, state, result.Error, now)
		} else {
			auth.Status = StatusError
			auth.UpdatedAt = now
			updateAggregatedAvailability(auth, now)
		}
		return transition
	}

	applyAuthFailureState(m, auth, result.Error, result.RetryAfter, now)
	return transition
}

// resultNeedsGlobalTransition reports whether the result must be applied to
// the persisted auth through the store's StateMutator so the transition stays
// atomic across Home nodes. Only quota (429) transitions and successes that
// clear existing availability state need the shared row; everything else
// stays on the local in-memory path.
func (m *Manager) resultNeedsGlobalTransition(auth *Auth, result Result, resultModel string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if result.Success {
		return authHasClearableAvailabilityState(auth, resultModel, now)
	}
	if statusCodeFromResult(result.Error) != http.StatusTooManyRequests {
		return false
	}
	if isModelSupportResultError(result.Error) {
		return false
	}
	if m.quotaCooldownDisabledForAuth(auth) {
		return false
	}
	// A locally visible open window means the shared row already carries this
	// cooldown, so the failure can be absorbed without a database round-trip.
	// Provider retry hints still go through to extend the persisted window.
	if result.RetryAfter == nil && authQuotaWindowOpen(auth, resultModel, now) {
		return false
	}
	return true
}

// authQuotaWindowOpen reports whether the auth (or the given model state)
// already tracks an unexpired quota cooldown window.
func authQuotaWindowOpen(auth *Auth, resultModel string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if resultModel != "" {
		state := auth.ModelStates[resultModel]
		return state != nil && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now)
	}
	return auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now)
}

// authHasClearableAvailabilityState reports whether a success outcome would
// clear availability state that other Home nodes can observe.
func authHasClearableAvailabilityState(auth *Auth, resultModel string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if resultModel != "" {
		if state := auth.ModelStates[resultModel]; state != nil {
			if state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || state.Status == StatusError {
				return true
			}
		}
		// A success can also clear auth-scoped failure state that no model
		// state explains (for example a model-less 429 recorded earlier).
		if auth.Unavailable || !auth.NextRetryAfter.IsZero() {
			return true
		}
		if auth.Quota.Exceeded && !anyModelQuotaExceeded(auth) {
			return true
		}
		return auth.Status == StatusError && !hasModelError(auth, now)
	}
	return auth.Unavailable || auth.Quota.Exceeded || !auth.NextRetryAfter.IsZero() || auth.Status == StatusError
}

// anyModelQuotaExceeded reports whether any model state tracks an exceeded quota.
func anyModelQuotaExceeded(auth *Auth) bool {
	if auth == nil {
		return false
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded {
			return true
		}
	}
	return false
}

// quotaFingerprint condenses the scheduling-relevant fields of a QuotaState.
type quotaFingerprint struct {
	exceeded    bool
	reason      string
	recoverUnix int64
	level       int
}

// availabilityFingerprintValue condenses the fields of an auth (and one model
// state) that determine scheduling availability, so state mutations can
// cheaply detect whether a transition changed anything worth persisting.
// Volatile fields such as LastError, StatusMessage, and UpdatedAt are
// intentionally excluded to keep in-window failures write-free.
type availabilityFingerprintValue struct {
	status         Status
	disabled       bool
	unavailable    bool
	nextRetryUnix  int64
	quota          quotaFingerprint
	modelPresent   bool
	modelStatus    Status
	modelUnavail   bool
	modelRetryUnix int64
	modelQuota     quotaFingerprint
}

// fingerprintUnix normalizes a time for fingerprint comparison.
func fingerprintUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fingerprintQuota builds a quota fingerprint.
func fingerprintQuota(quota QuotaState) quotaFingerprint {
	return quotaFingerprint{
		exceeded:    quota.Exceeded,
		reason:      quota.Reason,
		recoverUnix: fingerprintUnix(quota.NextRecoverAt),
		level:       quota.BackoffLevel,
	}
}

// availabilityFingerprint builds the comparable availability view of an auth.
func availabilityFingerprint(auth *Auth, resultModel string) availabilityFingerprintValue {
	fp := availabilityFingerprintValue{}
	if auth == nil {
		return fp
	}
	fp.status = auth.Status
	fp.disabled = auth.Disabled
	fp.unavailable = auth.Unavailable
	fp.nextRetryUnix = fingerprintUnix(auth.NextRetryAfter)
	fp.quota = fingerprintQuota(auth.Quota)
	if resultModel != "" {
		if state := auth.ModelStates[resultModel]; state != nil {
			fp.modelPresent = true
			fp.modelStatus = state.Status
			fp.modelUnavail = state.Unavailable
			fp.modelRetryUnix = fingerprintUnix(state.NextRetryAfter)
			fp.modelQuota = fingerprintQuota(state.Quota)
		}
	}
	return fp
}

// adoptPersistedResultState merges the authoritative persisted state produced
// by a StateMutator back into the manager's in-memory auth and returns a
// snapshot for scheduler updates.
func (m *Manager) adoptPersistedResultState(result Result, resultModel string, persisted *Auth, now time.Time) *Auth {
	if persisted == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	local := m.resultAuthLocked(result)
	if local == nil {
		return persisted.Clone()
	}
	local.Disabled = persisted.Disabled
	local.UpdatedAt = persisted.UpdatedAt
	if resultModel != "" {
		if state := persisted.ModelStates[resultModel]; state != nil {
			if local.ModelStates == nil {
				local.ModelStates = make(map[string]*ModelState)
			}
			local.ModelStates[resultModel] = state.Clone()
		} else if local.ModelStates != nil {
			delete(local.ModelStates, resultModel)
		}
		// Recompute the aggregate from the merged local view so cooldowns that
		// only exist locally (non-persisted transitions) are preserved.
		updateAggregatedAvailability(local, now)
		// The persisted copy can report a clean active state even though this
		// node still tracks a model error that was never persisted (for
		// example a transient 5xx). Mirror the hasModelError guard of the
		// local success path and keep the local error view in that case.
		if persisted.Status != StatusActive || !hasModelError(local, now) {
			local.Status = persisted.Status
			local.StatusMessage = persisted.StatusMessage
			local.LastError = cloneError(persisted.LastError)
		}
	} else {
		local.Status = persisted.Status
		local.StatusMessage = persisted.StatusMessage
		local.LastError = cloneError(persisted.LastError)
		local.Unavailable = persisted.Unavailable
		local.NextRetryAfter = persisted.NextRetryAfter
		local.Quota = persisted.Quota
	}
	return local.Clone()
}

// resultAuthLocked handles a result auth locked.
func (m *Manager) resultAuthLocked(result Result) *Auth {
	if m == nil {
		return nil
	}
	if id := strings.TrimSpace(result.AuthID); id != "" {
		return m.auths[id]
	}
	if index := strings.TrimSpace(result.AuthIndex); index != "" {
		return m.indexAuth[index]
	}
	return nil
}

// quotaCooldownDisabledForAuth returns a quota cooldown disabled for auth.
func (m *Manager) quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			if override {
				return true
			}
		}
	}
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	return cfg != nil && cfg.DisableCooling
}

// ensureModelState ensures a model state.
func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

// resetModelState resets a model state.
func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

// updateAggregatedAvailability updates an aggregated availability.
func updateAggregatedAvailability(auth *Auth, now time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	if auth == nil {
		return
	}
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

// clearAggregatedAvailability clears an aggregated availability.
func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
}

// hasModelError reports whether model error is present.
func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

// clearAuthStateOnSuccess clears an auth state on success.
func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

// cloneError clones an error.
func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

// statusCodeFromResult derives status code from result.
func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

// isModelSupportErrorMessage reports whether model support error message.
func isModelSupportErrorMessage(message string) bool {
	// Normalize source data before building the derived payload.
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// isModelSupportResultError reports whether model support result error.
func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

// isRequestScopedNotFoundMessage reports whether request scoped not found message.
func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

// isRequestScopedNotFoundResultError reports whether request scoped not found result error.
func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

// applyAuthFailureState applies an auth failure state.
func applyAuthFailureState(m *Manager, auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	// Normalize auth state before updating runtime indexes.
	if auth == nil {
		return
	}
	if isRequestScopedNotFoundResultError(resultErr) {
		return
	}
	disableCooling := m.quotaCooldownDisabledForAuth(auth)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	switch statusCode {
	case http.StatusUnauthorized:
		disableAuthAfterUnauthorized(auth, nil, resultErr, now)
	case http.StatusPaymentRequired, http.StatusForbidden:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case http.StatusNotFound:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case http.StatusTooManyRequests:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		next, backoffLevel := nextQuotaRecoverAt(now, retryAfter, auth.Quota, disableCooling)
		auth.Quota.BackoffLevel = backoffLevel
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		auth.StatusMessage = "transient upstream error"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

// disableAuthAfterUnauthorized removes a credential from request and refresh retries.
func disableAuthAfterUnauthorized(auth *Auth, state *ModelState, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	auth.Disabled = true
	auth.Unavailable = true
	auth.Status = StatusDisabled
	auth.StatusMessage = "unauthorized"
	auth.NextRetryAfter = time.Time{}
	auth.NextRefreshAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
	} else if auth.LastError == nil {
		auth.LastError = &Error{Message: http.StatusText(http.StatusUnauthorized), HTTPStatus: http.StatusUnauthorized}
	}
	if state == nil {
		return
	}
	state.Unavailable = true
	state.Status = StatusDisabled
	state.StatusMessage = "unauthorized"
	state.NextRetryAfter = time.Time{}
	state.Quota = QuotaState{}
	state.UpdatedAt = now
	if resultErr != nil {
		state.LastError = cloneError(resultErr)
	} else if state.LastError == nil {
		state.LastError = &Error{Message: http.StatusText(http.StatusUnauthorized), HTTPStatus: http.StatusUnauthorized}
	}
}

// nextQuotaCooldown returns a next quota cooldown.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

func nextQuotaRecoverAt(now time.Time, retryAfter *time.Duration, quota QuotaState, disableCooling bool) (time.Time, int) {
	if disableCooling {
		return time.Time{}, quota.BackoffLevel
	}
	if retryAfter != nil && *retryAfter > 0 {
		return now.Add(*retryAfter), quota.BackoffLevel
	}
	return quotaCooldownAfterFailure(quota, now)
}

// quotaCooldownAfterFailure returns the recovery deadline and backoff level for
// a quota failure observed at now. Failures that land while a previous quota
// window is still open reuse that window instead of escalating, so a burst of
// concurrent failures advances the backoff ladder at most once per window.
func quotaCooldownAfterFailure(quota QuotaState, now time.Time) (time.Time, int) {
	if quota.NextRecoverAt.After(now) {
		return quota.NextRecoverAt, quota.BackoffLevel
	}
	cooldown, nextLevel := nextQuotaCooldown(quota.BackoffLevel, false)
	if cooldown <= 0 {
		return time.Time{}, nextLevel
	}
	return now.Add(cooldown), nextLevel
}

// NewUsageResult creates a new usage result.
func NewUsageResult(authIndex, provider, model string, statusCode int, body string) Result {
	// Keep validation before state changes so failures leave existing data intact.
	authIndex = strings.TrimSpace(authIndex)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	body = strings.TrimSpace(body)
	if statusCode <= 0 {
		statusCode = http.StatusOK
	}
	if statusCode == http.StatusOK {
		return Result{
			AuthIndex: authIndex,
			Provider:  provider,
			Model:     model,
			Success:   true,
		}
	}
	message := body
	if message == "" {
		message = http.StatusText(statusCode)
	}
	if message == "" {
		message = fmt.Sprintf("request failed with status %d", statusCode)
	}
	return Result{
		AuthIndex:  authIndex,
		Provider:   provider,
		Model:      model,
		Success:    false,
		RetryAfter: parseUsageRetryAfter(body, statusCode),
		Error: &Error{
			Message:    message,
			HTTPStatus: statusCode,
		},
	}
}

func parseUsageRetryAfter(body string, statusCode int) *time.Duration {
	if statusCode != http.StatusTooManyRequests {
		return nil
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if gjson.Valid(body) {
		if retryAfter := parseGoogleRetryDelay(body); retryAfter != nil {
			return retryAfter
		}
		if retryAfter := parseRetryDelayFromMessage(gjson.Get(body, "error.message").String()); retryAfter != nil {
			return retryAfter
		}
	}
	return parseRetryDelayFromMessage(body)
}

func parseGoogleRetryDelay(body string) *time.Duration {
	details := gjson.Get(body, "error.details")
	if !details.Exists() || !details.IsArray() {
		return nil
	}
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.RetryInfo" {
			continue
		}
		if retryAfter := parseDurationPointer(detail.Get("retryDelay").String()); retryAfter != nil {
			return retryAfter
		}
	}
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		if retryAfter := parseDurationPointer(detail.Get("metadata.quotaResetDelay").String()); retryAfter != nil {
			return retryAfter
		}
	}
	return nil
}

func parseRetryDelayFromMessage(message string) *time.Duration {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}
	matches := usageRetryDelayPattern.FindStringSubmatch(message)
	if len(matches) < 2 {
		return nil
	}
	return parseDurationPointer(matches[1])
}

func parseDurationPointer(value string) *time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	duration, errParse := time.ParseDuration(value)
	if errParse != nil || duration <= 0 {
		return nil
	}
	return &duration
}
