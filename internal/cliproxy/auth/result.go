package auth

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	quotaBackoffBase            = time.Second
	quotaBackoffMax             = 30 * time.Minute
	unauthorizedRetryBackoff    = time.Minute
	quotaScopeModel             = "model"
	providerQuotaHintMaxHorizon = 60 * 24 * time.Hour
)

// Result captures an upstream execution result reported by a downstream CPA node.
type Result struct {
	AuthID            string
	AuthIndex         string
	Provider          string
	Model             string
	Success           bool
	Error             *Error
	RetryAfter        *time.Duration
	ResetAt           *time.Time
	AccessTokenSHA256 string
	// CredentialScope reports whether the parsed reset hint applies to the
	// whole credential rather than just the requested model. Today this is
	// only ever set true for a Claude request explicitly rejected on the
	// shared Anthropic 5h/7d unified rate-limit window (see
	// claudeHeadersIndicateUnifiedRateLimitRejection).
	CredentialScope bool
	// RateLimitWarnings carries provider early-warning windows parsed from a
	// SUCCESSFUL response's headers (Claude only today). Nil for every other
	// provider and for every failure result.
	RateLimitWarnings map[string]RateLimitWarning
	// RateLimitClearedWindows lists windows whose per-window status header
	// explicitly reported "allowed" on this SUCCESSFUL response (Claude
	// only), so applyRateLimitWarningTransition can clear a stale warning for
	// that ONE window even when the unsuffixed status still reads
	// "allowed_warning" because a DIFFERENT window is newly warned on the
	// same response. An ABSENT window header is never collected here -- only
	// an explicit "allowed" clears; absence must stay a no-op, same as
	// today (see parseClaudeRateLimitWarnings). Not part of any comparable
	// fingerprint struct -- it is a transient per-result signal, never
	// itself persisted on Auth.
	RateLimitClearedWindows []string
	// RateLimitAllClear reports that the provider affirmatively signalled that
	// every window is healthy, so any previously recorded warning is stale.
	RateLimitAllClear bool
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
	transitionVersion := int64(0)
	var authSnapshot *Auth
	skipResultPersist := false
	localOnlyDisabledQuota := false
	localAvailabilityBefore := availabilityFingerprintValue{}
	disableCooling := false
	resultModel := canonicalModelKey(result.Model)
	if resultModel == "" {
		return
	}
	resultAuthID := m.resultAuthID(result)
	if resultAuthID == "" {
		return
	}
	now := time.Now()

	unlockUpdate := m.updateLocks.lock(resultAuthID)
	m.mu.Lock()
	auth := m.resultAuthLocked(result)
	if auth == nil || auth.ID != resultAuthID {
		m.mu.Unlock()
		unlockUpdate()
		return
	}
	var mutator StateMutator
	stateMutatorAvailable := false
	disableCooling = m.quotaCooldownDisabledForAuth(auth)
	auth.recordRecentRequest(now, result.Success)
	if result.Success {
		auth.Success++
	} else {
		auth.Failed++
	}
	localOnlyDisabledQuota = statusCodeFromResult(result.Error) == http.StatusTooManyRequests &&
		!isModelSupportResultError(result.Error) &&
		disableCooling &&
		disabledQuotaStateAlreadyApplied(auth, resultModel)
	if localOnlyDisabledQuota {
		localAvailabilityBefore = availabilityFingerprint(auth, resultModel)
	}
	if stateMutator, ok := m.store.(StateMutator); ok {
		stateMutatorAvailable = true
		if m.resultNeedsGlobalTransition(auth, result, resultModel, now, disableCooling) {
			mutator = stateMutator
		}
	}
	// A locally clean token-versioned success has no state to clear. Drop its
	// transition rather than enqueue a stale row or read the database per success;
	// cluster events reconcile any newer authoritative state.
	if mutator == nil && !(stateMutatorAvailable && isTokenVersionedSuccessResult(result)) {
		transition = m.applyResultTransition(auth, result, resultModel, now, disableCooling)
		if localOnlyDisabledQuota {
			skipResultPersist = availabilityFingerprint(auth, resultModel) == localAvailabilityBefore
		}
		authSnapshot = auth.Clone()
	}
	m.mu.Unlock()
	unlockUpdate()

	if mutator != nil {
		// Apply the transition against the persisted auth so concurrent quota
		// results reported to other Home nodes cannot clobber the shared state.
		persisted, errMutate := mutator.MutateAuthState(ctx, resultAuthID, func(persisted *Auth) bool {
			before := availabilityFingerprint(persisted, resultModel)
			baseVersion := persisted.StateVersion
			transition = m.applyResultTransition(persisted, result, resultModel, now, disableCooling)
			changed := availabilityFingerprint(persisted, resultModel) != before
			if baseVersion > 0 {
				transitionVersion = baseVersion
				if changed {
					transitionVersion++
				}
			}
			return changed
		})
		if errMutate != nil {
			log.Warnf("auth manager: persisted result transition failed for %s: %v", resultAuthID, errMutate)
			return
		}
		if persisted == nil {
			log.Warnf("auth manager: persisted result transition returned no auth for %s", resultAuthID)
			return
		}
		if transitionVersion > 0 && persisted.StateVersion > transitionVersion {
			transition = markResultTransition{}
		}
		var adopted bool
		authSnapshot, adopted = m.adoptPersistedResultState(result, resultModel, persisted, now)
		if !adopted {
			transition = markResultTransition{}
		}
	} else if !skipResultPersist {
		persistAuthID, schedulePersist := m.stageResultPersist(ctx, authSnapshot)
		if schedulePersist {
			defer m.scheduleResultPersist(persistAuthID)
		}
	}

	m.RefreshSchedulerEntry(resultAuthID)
	if authSnapshot != nil && authRefreshDisabled(authSnapshot) {
		m.queueRefreshReschedule(authSnapshot.ID)
	}
	resultStatus := statusCodeFromResult(result.Error)
	if resultStatus == http.StatusTooManyRequests || isRequestErrorCooldownStatus(resultStatus) {
		m.reconcileCoveredCooldownAfterResult(ctx, resultAuthID)
		return
	}
	if transition.clearModelQuota || transition.setModelQuota || transition.shouldResumeModel || transition.shouldSuspendModel {
		m.ReconcileRegistryModelStates(ctx, resultAuthID)
	}
}

// applyResultTransition applies the result state machine to the provided auth
// and reports the derived registry side effects. The auth may be the manager's
// in-memory copy or a persisted copy loaded by a StateMutator; the function
// uses the caller's captured cooling policy and does not read manager state.
func (m *Manager) applyResultTransition(auth *Auth, result Result, resultModel string, now time.Time, disableCooling bool) markResultTransition {
	transition := markResultTransition{}
	if auth == nil {
		return transition
	}
	if isTokenVersionFencedResult(result) && AuthIsNewerThanObserved(auth, result.AccessTokenSHA256) {
		return transition
	}
	preserveRefreshAcquisition := RefreshBlocksDispatch(auth)
	refreshStatus := auth.Status
	refreshStatusMessage := auth.StatusMessage
	refreshLastError := cloneError(auth.LastError)
	refreshNextRetryAfter := auth.NextRetryAfter
	refreshRuntimeBlocked := auth.RuntimeRefreshBlocked

	if result.Success {
		state := ensureModelState(auth, resultModel)
		resetModelState(state, now)
		updateAggregatedAvailability(auth, now)
		if preserveRefreshAcquisition {
			auth.Status = refreshStatus
			auth.StatusMessage = refreshStatusMessage
			auth.LastError = refreshLastError
			auth.Unavailable = true
			auth.NextRetryAfter = refreshNextRetryAfter
			auth.RuntimeRefreshBlocked = refreshRuntimeBlocked
		} else if !hasModelError(auth, now) {
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
		applyRateLimitWarningTransition(auth, result, now)
		auth.UpdatedAt = now
		transition.shouldResumeModel = true
		transition.clearModelQuota = true
		return transition
	}

	if isRequestScopedNotFoundResultError(result.Error) {
		return transition
	}
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
	if isModelSupportResultError(result.Error) {
		next := now.Add(12 * time.Hour)
		state.NextRetryAfter = next
		transition.suspendReason = "model_not_supported"
		transition.shouldSuspendModel = true
	} else {
		switch statusCode {
		case http.StatusUnauthorized:
			state.NextRetryAfter = now.Add(unauthorizedRetryBackoff)
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
			backoffLevel := state.Quota.BackoffLevel
			if disableCooling {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
				state.QuotaResetAt = time.Time{}
				state.Quota = QuotaState{Scope: quotaScopeModel, Reason: "quota", BackoffLevel: backoffLevel}
				transition.clearModelQuota = true
				transition.shouldResumeModel = true
			} else {
				next, nextBackoffLevel := quotaCooldownAfterFailure(state.Quota, now, result)
				state.NextRetryAfter = next
				state.QuotaResetAt = time.Time{}
				state.Quota = QuotaState{
					Exceeded:      true,
					Scope:         quotaScopeModel,
					Reason:        "quota",
					NextRecoverAt: next,
					BackoffLevel:  nextBackoffLevel,
				}
				transition.suspendReason = "quota"
				transition.shouldSuspendModel = true
				transition.setModelQuota = true
				if result.CredentialScope {
					blockSiblingModelStatesUntil(auth, resultModel, next, nextBackoffLevel, now)
				}
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
	auth.Status = StatusError
	auth.UpdatedAt = now
	updateAggregatedAvailability(auth, now)
	if preserveRefreshAcquisition {
		auth.Status = refreshStatus
		auth.StatusMessage = refreshStatusMessage
		auth.LastError = refreshLastError
		auth.Unavailable = true
		auth.NextRetryAfter = refreshNextRetryAfter
		auth.RuntimeRefreshBlocked = refreshRuntimeBlocked
	}
	return transition
}

// blockSiblingModelStatesUntil marks every ModelState on auth other than
// resultModel unavailable until deadline. It is used when a 429 result
// carries a credential-scoped signal (currently: a Claude request explicitly
// rejected on the shared Anthropic 5h/7d unified rate-limit window), so a
// sibling model on the same credential is not left selectable.
//
// This intentionally does NOT write auth.Unavailable/auth.Quota/
// auth.NextRetryAfter directly: updateAggregatedAvailability (called right
// after this, at the end of applyResultTransition's failure path) derives
// those three fields unconditionally from auth.ModelStates on every result,
// so a direct write here would be overwritten immediately. Instead this
// works WITH that aggregation: once every ModelState is unavailable,
// updateAggregatedAvailability's own allUnavailable computation sets the
// auth-level fields correctly, and the existing per-model gate in
// isAuthBlockedForModel (selector.go) blocks every model -- no selector.go
// change required. Mirrors CPA's own sdk/cliproxy/auth/conductor_cooldown.go
// fan-out for a credential-scoped rate limit.
//
// Known limitation, intentionally not addressed here: a model with no
// recorded ModelState (never dispatched under this credential) has nothing
// to mark and is not covered. Addressing that would require an auth-level
// check in selector.go, which the base-branch parity constraint (result.go
// and usage_result.go are byte-identical between v1.0.72 and dev; selector.go
// is not) rules out for this fix.
func blockSiblingModelStatesUntil(auth *Auth, resultModel string, deadline time.Time, backoffLevel int, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	for key, sibling := range auth.ModelStates {
		if sibling == nil || key == resultModel {
			continue
		}
		sibling.Unavailable = true
		sibling.Status = StatusError
		sibling.NextRetryAfter = deadline
		sibling.QuotaResetAt = time.Time{}
		sibling.Quota = QuotaState{
			Exceeded: true,
			// Deliberately quotaScopeModel, not quotaScopeCredential, even
			// though this whole fan-out only runs for a credential-scoped
			// rejection (result.CredentialScope): aggregateModelQuota
			// (invoked by updateAggregatedAvailability right after this
			// call returns) unconditionally hardcodes Scope: quotaScopeModel
			// on the auth-level Quota it derives from every ModelState,
			// ignoring whatever Scope each individual ModelState carries.
			// Setting "credential" here would be silently clobbered on the
			// very next aggregation pass -- the credential-scoped signal is
			// expressed by fanning this state out to every sibling model,
			// not by this field's value.
			Scope:         quotaScopeModel,
			Reason:        "quota",
			NextRecoverAt: deadline,
			BackoffLevel:  backoffLevel,
		}
		sibling.UpdatedAt = now
	}
}

// rateLimitWarningActive reports whether a recorded warning is still within
// its window, so both the selector's read-only check and this file's
// mutation path apply the same expiry rule. A warning with no parseable
// Reset (zero ResetAt) never expires on time and relies on an explicit
// all-clear instead.
func rateLimitWarningActive(warning RateLimitWarning, now time.Time) bool {
	return warning.ResetAt.IsZero() || warning.ResetAt.After(now)
}

// rateLimitWarningWindowChanged reports whether applying incoming to a
// credential's existing warning for one window would change persisted
// state. ObservedAt is deliberately excluded from the comparison: it
// refreshes on every successful request and must not itself count as a
// change (that would hammer the store on every request).
func rateLimitWarningWindowChanged(existing RateLimitWarning, hadExisting bool, incoming RateLimitWarning) bool {
	if !hadExisting {
		return true
	}
	return !existing.ResetAt.Equal(incoming.ResetAt)
}

// rateLimitWarningsWouldChange reports whether applying result's rate-limit
// warning signal to auth would change auth.RateLimitWarnings: a new or
// changed window, a per-window clear (RateLimitClearedWindows) against a
// window that currently carries a live mark, an all-clear against a
// non-empty map, or an entry already past its Reset sitting in the map.
// applyRateLimitWarningTransition builds its own set/clear/expire decisions
// out of the same rateLimitWarningWindowChanged/rateLimitWarningActive
// primitives this function uses, so the two can never disagree about what
// counts as a change. The per-window clear check matters on its own: a
// response that ONLY clears a window (no new warning, nothing expired)
// would otherwise report no change and have its clear silently discarded by
// MutateAuthState's unchanged-fingerprint guard.
func rateLimitWarningsWouldChange(auth *Auth, result Result, now time.Time) bool {
	if auth == nil {
		return false
	}
	if result.RateLimitAllClear {
		return len(auth.RateLimitWarnings) > 0
	}
	for window, incoming := range result.RateLimitWarnings {
		existing, hadExisting := auth.RateLimitWarnings[window]
		if rateLimitWarningWindowChanged(existing, hadExisting, incoming) {
			return true
		}
	}
	for _, window := range result.RateLimitClearedWindows {
		if _, ok := auth.RateLimitWarnings[window]; ok {
			return true
		}
	}
	for _, existing := range auth.RateLimitWarnings {
		if !rateLimitWarningActive(existing, now) {
			return true
		}
	}
	return false
}

// authFingerprintPrefix returns a short, non-identifying prefix of auth.ID
// suitable for log correlation without exposing a label, email, or token.
func authFingerprintPrefix(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// applyRateLimitWarningTransition applies a success result's provider
// early-warning signal to auth.RateLimitWarnings: an affirmative all-clear
// wipes every window, otherwise each warned window from the result is
// set/refreshed, each window in RateLimitClearedWindows (an explicit
// per-window "allowed" status) is deleted individually -- this is what lets
// one window clear while a DIFFERENT window newly warns on the very same
// response, since the unsuffixed status in that case still reads
// allowed_warning and the wholesale all-clear branch above never fires --
// and independently any window already past its Reset is swept. Logs one
// info line per actual change (set / cleared / expired), naming only the
// credential fingerprint -- the first 8 characters of auth.ID -- never a
// label, email, token, or full id.
func applyRateLimitWarningTransition(auth *Auth, result Result, now time.Time) {
	if auth == nil {
		return
	}
	fp := authFingerprintPrefix(auth.ID)
	if result.RateLimitAllClear {
		if len(auth.RateLimitWarnings) > 0 {
			log.Infof("auth manager: rate limit warnings cleared (all-clear) for %s", fp)
		}
		auth.RateLimitWarnings = nil
		return
	}
	for window, incoming := range result.RateLimitWarnings {
		existing, hadExisting := auth.RateLimitWarnings[window]
		if !rateLimitWarningWindowChanged(existing, hadExisting, incoming) {
			continue
		}
		if auth.RateLimitWarnings == nil {
			auth.RateLimitWarnings = make(map[string]RateLimitWarning)
		}
		incoming.ObservedAt = now
		auth.RateLimitWarnings[window] = incoming
		log.Infof("auth manager: rate limit warning set for %s window=%s", fp, window)
	}
	for _, window := range result.RateLimitClearedWindows {
		if _, ok := auth.RateLimitWarnings[window]; !ok {
			continue
		}
		delete(auth.RateLimitWarnings, window)
		log.Infof("auth manager: rate limit warning cleared for %s window=%s", fp, window)
	}
	for window, existing := range auth.RateLimitWarnings {
		if rateLimitWarningActive(existing, now) {
			continue
		}
		delete(auth.RateLimitWarnings, window)
		log.Infof("auth manager: rate limit warning expired for %s window=%s", fp, window)
	}
}

// resultNeedsGlobalTransition reports whether the result must be applied to
// the persisted auth through the store's StateMutator so the transition stays
// atomic across Home nodes. Unauthorized transitions must preserve credentials
// that may be refreshed concurrently, while quota (429) and clearing successes
// need shared availability state. Other failures stay on the local path.
func (m *Manager) resultNeedsGlobalTransition(auth *Auth, result Result, resultModel string, now time.Time, disableCooling bool) bool {
	if auth == nil {
		return false
	}
	if result.Success {
		return authHasClearableAvailabilityState(auth, resultModel, now) || rateLimitWarningsWouldChange(auth, result, now)
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode == http.StatusUnauthorized {
		return true
	}
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if isModelSupportResultError(result.Error) {
		return false
	}
	if disableCooling {
		// Apply disabled 429 transitions to the persisted full auth. Minimal
		// projections intentionally omit provider credentials and must never be
		// written back as complete auth snapshots.
		return !disabledQuotaStateAlreadyApplied(auth, resultModel)
	}
	state := auth.ModelStates[resultModel]
	quota := QuotaState{}
	if state != nil {
		quota = state.Quota
	}
	next, nextBackoffLevel := quotaCooldownAfterFailure(quota, now, result)
	if !authQuotaWindowOpen(auth, resultModel, now) {
		return true
	}
	return state == nil ||
		!state.NextRetryAfter.Equal(next) ||
		!state.Quota.NextRecoverAt.Equal(next) ||
		state.Quota.BackoffLevel != nextBackoffLevel
}

// disabledQuotaStateAlreadyApplied reports whether a disabled-cooling 429 can
// update local diagnostics without changing persisted scheduling state.
func disabledQuotaStateAlreadyApplied(auth *Auth, resultModel string) bool {
	if auth == nil || resultModel == "" || auth.Status != StatusError ||
		auth.Unavailable || !auth.NextRetryAfter.IsZero() ||
		fingerprintQuota(auth.Quota) != fingerprintQuota(aggregateModelQuota(auth.ModelStates)) {
		return false
	}
	state := auth.ModelStates[resultModel]
	if state == nil {
		return false
	}
	return state.Status == StatusError &&
		!state.Unavailable &&
		state.NextRetryAfter.IsZero() &&
		state.QuotaResetAt.IsZero() &&
		!state.Quota.Exceeded &&
		state.Quota.Scope == quotaScopeModel &&
		state.Quota.Reason == "quota" &&
		state.Quota.NextRecoverAt.IsZero()
}

// reconcileCoveredCooldownAfterResult derives registry state from the current
// manager view after a covered result. This closes the race where an older
// result finishes after a config reload has already disabled and cleared cooling.
func (m *Manager) reconcileCoveredCooldownAfterResult(ctx context.Context, authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		return
	}
	if m.quotaCooldownDisabledForAuth(auth) && hasDisabledCooldownState(auth) {
		if errClear := m.ClearDisabledCooldownStates(ctx); errClear != nil {
			log.WithError(errClear).WithField("auth_id", authID).Warn("auth manager: failed to reconcile disabled cooldown after result")
		}
	}
	// Registry state is derived even when the local auth is already clean: an
	// older transition may have reached the registry after config reconciliation.
	m.ReconcileRegistryModelStates(ctx, authID)
}

// authQuotaWindowOpen reports whether the auth (or the given model state)
// already tracks an unexpired quota cooldown window within the maximum horizon.
func authQuotaWindowOpen(auth *Auth, resultModel string, now time.Time) bool {
	if auth == nil || resultModel == "" {
		return false
	}
	state := auth.ModelStates[resultModel]
	return state != nil && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) && state.Quota.NextRecoverAt.Sub(now) <= providerQuotaHintMaxHorizon
}

// authHasClearableAvailabilityState reports whether a success outcome would
// clear availability state that other Home nodes can observe.
func authHasClearableAvailabilityState(auth *Auth, resultModel string, now time.Time) bool {
	if auth == nil || resultModel == "" {
		return false
	}
	if state := auth.ModelStates[resultModel]; state != nil {
		if state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || state.Status == StatusError || !state.QuotaResetAt.IsZero() {
			return true
		}
	}
	return auth.Status == StatusError && !hasModelError(auth, now)
}

// quotaFingerprint condenses the scheduling-relevant fields of a QuotaState.
type quotaFingerprint struct {
	exceeded    bool
	scope       string
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
	modelResetUnix int64
	modelQuota     quotaFingerprint
	warningsDigest uint64
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
		scope:       quota.Scope,
		reason:      quota.Reason,
		recoverUnix: fingerprintUnix(quota.NextRecoverAt),
		level:       quota.BackoffLevel,
	}
}

// rateLimitWarningsDigest computes an FNV-1a digest over a credential's
// recorded rate-limit warnings, hashing each window (sorted by name, so map
// iteration order can never affect the result) plus its ResetAt unix value.
// ObservedAt is deliberately excluded: it refreshes on every successful
// request and would make every success look like a state change, hammering
// the store on every request.
func rateLimitWarningsDigest(warnings map[string]RateLimitWarning) uint64 {
	if len(warnings) == 0 {
		return 0
	}
	windows := make([]string, 0, len(warnings))
	for window := range warnings {
		windows = append(windows, window)
	}
	sort.Strings(windows)
	h := fnv.New64a()
	for _, window := range windows {
		warning := warnings[window]
		h.Write([]byte(window))
		h.Write([]byte{':'})
		h.Write([]byte(strconv.FormatInt(fingerprintUnix(warning.ResetAt), 10)))
		h.Write([]byte{'\n'})
	}
	return h.Sum64()
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
	fp.warningsDigest = rateLimitWarningsDigest(auth.RateLimitWarnings)
	if resultModel != "" {
		if state := auth.ModelStates[resultModel]; state != nil {
			fp.modelPresent = true
			fp.modelStatus = state.Status
			fp.modelUnavail = state.Unavailable
			fp.modelRetryUnix = fingerprintUnix(state.NextRetryAfter)
			fp.modelResetUnix = fingerprintUnix(state.QuotaResetAt)
			fp.modelQuota = fingerprintQuota(state.Quota)
		}
	}
	return fp
}

// adoptPersistedCredentialLocked replaces persisted credential fields while
// retaining state that only exists in the local runtime. The caller must hold m.mu.
func (m *Manager) adoptPersistedCredentialLocked(local, persisted *Auth) {
	if m == nil || local == nil || persisted == nil {
		return
	}

	previousIndex := strings.TrimSpace(local.Index)
	localModelStates := local.ModelStates
	localStorage := local.Storage
	localRuntime := local.Runtime
	localFileName := local.FileName
	localSuccess := local.Success
	localFailed := local.Failed
	localRecentRequests := local.recentRequests
	localIndexAssigned := local.indexAssigned

	replacement := persisted.Clone()
	if strings.TrimSpace(replacement.Index) == "" {
		replacement.Index = local.Index
	}
	if strings.TrimSpace(replacement.FileName) == "" {
		replacement.FileName = localFileName
	}
	if replacement.Storage == nil {
		replacement.Storage = localStorage
	}
	if replacement.Runtime == nil {
		replacement.Runtime = localRuntime
	}
	replacement.ModelStates = localModelStates
	replacement.Success = localSuccess
	replacement.Failed = localFailed
	replacement.recentRequests = localRecentRequests
	replacement.indexAssigned = localIndexAssigned
	*local = *replacement

	nextIndex := strings.TrimSpace(local.Index)
	if previousIndex != "" && previousIndex != nextIndex {
		if indexed := m.indexAuth[previousIndex]; indexed == local {
			delete(m.indexAuth, previousIndex)
		}
	}
	if nextIndex != "" {
		if m.indexAuth == nil {
			m.indexAuth = make(map[string]*Auth)
		}
		m.indexAuth[nextIndex] = local
	}
}

// adoptPersistedResultState merges the authoritative persisted state produced
// by a StateMutator back into the manager's in-memory auth. The boolean reports
// whether that snapshot remained current enough to apply its registry effects.
func (m *Manager) adoptPersistedResultState(result Result, resultModel string, persisted *Auth, now time.Time) (*Auth, bool) {
	if persisted == nil {
		return nil, false
	}
	unlockUpdate := m.updateLocks.lock(persisted.ID)
	defer unlockUpdate()
	m.mu.Lock()
	defer m.mu.Unlock()
	local := m.auths[persisted.ID]
	if local == nil {
		return nil, false
	}
	if local.StateVersion > 0 && persisted.StateVersion > 0 && local.StateVersion > persisted.StateVersion {
		return local.Clone(), false
	}
	localRefreshBlocked := local.RuntimeRefreshBlocked ||
		(local.LastError != nil && strings.EqualFold(strings.TrimSpace(local.LastError.Code), refreshTransientErrorCode))
	adoptPersistedCredential := persisted.StateVersion > 0 && persisted.StateVersion > local.StateVersion
	if isTokenVersionFencedResult(result) {
		if AuthIsNewerThanObserved(local, result.AccessTokenSHA256) {
			return local.Clone(), false
		}
		if AuthIsNewerThanObserved(persisted, result.AccessTokenSHA256) {
			adoptPersistedCredential = true
		}
	}
	if adoptPersistedCredential {
		m.adoptPersistedCredentialLocked(local, persisted)
		local.ModelStates = mergePersistedCooldownModelStates(persisted.ModelStates, local.ModelStates)
	}
	local.Disabled = persisted.Disabled
	local.UpdatedAt = persisted.UpdatedAt
	local.StateVersion = persisted.StateVersion
	local.LastRefreshError = cloneError(persisted.LastRefreshError)
	local.NextRefreshAfter = persisted.NextRefreshAfter
	if state := persisted.ModelStates[resultModel]; state != nil {
		if local.ModelStates == nil {
			local.ModelStates = make(map[string]*ModelState)
		}
		local.ModelStates[resultModel] = state.Clone()
	} else if local.ModelStates != nil {
		delete(local.ModelStates, resultModel)
	}
	updateAggregatedAvailability(local, now)
	persistedRefreshBlocked := RefreshBlocksDispatch(persisted)
	local.RuntimeRefreshBlocked = persistedRefreshBlocked
	if localRefreshBlocked && !persistedRefreshBlocked {
		local.StatusMessage = ""
		local.LastError = nil
		var latestModelError *ModelState
		for _, state := range local.ModelStates {
			if state == nil {
				continue
			}
			activeError := state.LastError != nil ||
				(state.Status == StatusError && state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)))
			if !activeError {
				continue
			}
			if latestModelError == nil || state.UpdatedAt.After(latestModelError.UpdatedAt) {
				latestModelError = state
			}
		}
		if latestModelError != nil {
			local.Status = StatusError
			local.StatusMessage = latestModelError.StatusMessage
			local.LastError = cloneError(latestModelError.LastError)
			if local.StatusMessage == "" && local.LastError != nil {
				local.StatusMessage = local.LastError.Message
			}
		}
	}
	// The persisted copy can report a clean active state even though this
	// node still tracks a model error that was never persisted (for example a
	// transient 5xx). Keep the local error view in that case.
	if persisted.Status != StatusActive || !hasModelError(local, now) {
		local.Status = persisted.Status
		local.StatusMessage = persisted.StatusMessage
		local.LastError = cloneError(persisted.LastError)
	}
	if persistedRefreshBlocked {
		local.Unavailable = true
		local.NextRetryAfter = persisted.NextRetryAfter
	}
	return local.Clone(), true
}

// resultAuthLocked handles a result auth locked.
func isTokenVersionedSuccessResult(result Result) bool {
	_, validHash := normalizeSHA256Hex(result.AccessTokenSHA256)
	return validHash && result.Success
}

func isTokenVersionFencedResult(result Result) bool {
	_, validHash := normalizeSHA256Hex(result.AccessTokenSHA256)
	return validHash && (result.Success || statusCodeFromResult(result.Error) == http.StatusUnauthorized)
}

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

func (m *Manager) resultAuthID(result Result) string {
	if m == nil {
		return ""
	}
	if id := strings.TrimSpace(result.AuthID); id != "" {
		return id
	}
	index := strings.TrimSpace(result.AuthIndex)
	if index == "" {
		return ""
	}
	m.mu.RLock()
	auth := m.indexAuth[index]
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	m.mu.RUnlock()
	return authID
}

// quotaCooldownDisabledForAuth reports whether cooldown scheduling is disabled
// for this auth.
func (m *Manager) quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if disabled := auth.DisableCoolingOverride(); disabled != nil {
			return *disabled
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
	state.QuotaResetAt = time.Time{}
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
	auth.Quota = aggregateModelQuota(auth.ModelStates)
}

// aggregateModelQuota builds the credential-level quota view derived from all
// model states.
func aggregateModelQuota(states map[string]*ModelState) QuotaState {
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	for _, state := range states {
		if state == nil || !state.Quota.Exceeded {
			continue
		}
		quotaExceeded = true
		if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
			quotaRecover = state.Quota.NextRecoverAt
		}
		if state.Quota.BackoffLevel > maxBackoffLevel {
			maxBackoffLevel = state.Quota.BackoffLevel
		}
	}
	if !quotaExceeded {
		return QuotaState{}
	}
	return QuotaState{
		Exceeded:      true,
		Scope:         quotaScopeModel,
		Reason:        "quota",
		NextRecoverAt: quotaRecover,
		BackoffLevel:  maxBackoffLevel,
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

// cloneError clones an error.
func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	cloned := &Error{
		Code:       err.Code,
		Message:    err.Message,
		Diagnostic: err.Diagnostic,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
	if err.Upstream != nil {
		cloned.Upstream = &UpstreamResponse{
			Status: err.Upstream.Status,
			Body:   append([]byte(nil), err.Upstream.Body...),
		}
	}
	return cloned
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

// disableAuthAfterUnauthorized permanently disables a credential after a terminal refresh failure.
func disableAuthAfterUnauthorized(auth *Auth, state *ModelState, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	auth.Disabled = true
	auth.Unavailable = true
	auth.RuntimeRefreshBlocked = false
	auth.Status = StatusDisabled
	auth.StatusMessage = "unauthorized"
	auth.NextRetryAfter = time.Time{}
	auth.NextRefreshAfter = time.Time{}
	auth.LastRefreshError = nil
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
	if prevLevel >= 11 {
		return quotaBackoffMax, prevLevel
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

// quotaCooldownAfterFailure returns the recovery deadline and backoff level for
// a quota failure observed at now. Failures that land while a previous quota
// window is still open reuse that window instead of escalating, so a burst of
// concurrent failures advances the backoff ladder at most once per window.
// Future provider reset timestamps are authoritative and suppress relative
// retry delays. Without one, the later of exponential backoff and RetryAfter
// is used. Open windows keep their backoff level and are never shortened.
func quotaCooldownAfterFailure(quota QuotaState, now time.Time, result Result) (time.Time, int) {
	windowOpen := quota.NextRecoverAt.After(now) && quota.NextRecoverAt.Sub(now) <= providerQuotaHintMaxHorizon
	deadline := quota.NextRecoverAt
	nextLevel := quota.BackoffLevel
	if !windowOpen {
		cooldown, level := nextQuotaCooldown(quota.BackoffLevel, false)
		deadline = now.Add(cooldown)
		nextLevel = level
	}

	// Gate on whether a reset hint was actually produced, not on a hardcoded
	// provider allowlist. parseUsageRetryHints returns (nil, nil) for any
	// provider without an explicit case (its own default branch), so this
	// cannot change antigravity/codex behavior -- it only stops the
	// allowlist here and the provider switch in parseUsageRetryHints from
	// drifting apart (see cooldown_backoff_test.go parity coverage).
	if result.ResetAt == nil && result.RetryAfter == nil {
		return deadline, nextLevel
	}
	if result.ResetAt != nil && result.ResetAt.After(now) {
		resetAt := result.ResetAt.UTC()
		if resetAt.Sub(now) <= providerQuotaHintMaxHorizon {
			if resetAt.After(deadline) {
				deadline = resetAt
			}
			return deadline, nextLevel
		}
	}
	if result.RetryAfter != nil && *result.RetryAfter > 0 && *result.RetryAfter <= providerQuotaHintMaxHorizon {
		if retryAt := now.Add(*result.RetryAfter); retryAt.After(deadline) {
			deadline = retryAt
		}
	}
	return deadline, nextLevel
}

// NewUsageResult creates a new usage result from a status code and response
// body only. It carries no response headers, so provider hints that require
// headers (Claude's Anthropic-Ratelimit-Unified-* headers) are unavailable
// through this entry point -- use NewUsageResultWithHeaders when headers are
// available.
func NewUsageResult(authIndex, provider, model string, statusCode int, body string) Result {
	return NewUsageResultWithHeaders(authIndex, provider, model, statusCode, body, nil)
}

// NewUsageResultWithHeaders creates a new usage result, additionally parsing
// provider-specific reset hints out of response headers for providers whose
// rate-limit information is not carried in the response body (Claude's
// Anthropic-Ratelimit-Unified-* headers).
func NewUsageResultWithHeaders(authIndex, provider, model string, statusCode int, body string, headers http.Header) Result {
	// Keep validation before state changes so failures leave existing data intact.
	authIndex = strings.TrimSpace(authIndex)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	body = strings.TrimSpace(body)
	if statusCode <= 0 {
		statusCode = http.StatusOK
	}
	if statusCode == http.StatusOK {
		warnings, clearedWindows, allClear := parseClaudeRateLimitWarnings(provider, headers)
		return Result{
			AuthIndex:               authIndex,
			Provider:                provider,
			Model:                   model,
			Success:                 true,
			RateLimitWarnings:       warnings,
			RateLimitClearedWindows: clearedWindows,
			RateLimitAllClear:       allClear,
		}
	}
	message := body
	if message == "" {
		message = http.StatusText(statusCode)
	}
	if message == "" {
		message = fmt.Sprintf("request failed with status %d", statusCode)
	}
	retryAfter, resetAt, credentialScope := parseUsageRetryHints(provider, body, statusCode, headers)
	return Result{
		AuthIndex:       authIndex,
		Provider:        provider,
		Model:           model,
		Success:         false,
		RetryAfter:      retryAfter,
		ResetAt:         resetAt,
		CredentialScope: credentialScope,
		Error: &Error{
			Message:    message,
			HTTPStatus: statusCode,
		},
	}
}

// claudeRateLimitWarningWindows lists the Anthropic unified rate-limit window
// keys checked for an allowed_warning status on a successful response.
var claudeRateLimitWarningWindows = [...]string{"5h", "7d", "7d_oi"}

// parseClaudeRateLimitWarnings parses provider early-warning windows out of a
// SUCCESSFUL Claude response's headers. Anthropic sends
// Anthropic-Ratelimit-Unified-<window>-Status: allowed_warning on a
// successful reply roughly 20 minutes before it starts rejecting requests on
// that window, so this only ever runs on the 200 path (see
// NewUsageResultWithHeaders), never on a 429.
//
// Returns (nil, nil, false) for every provider other than "claude" so no
// other provider's behavior can change. The second return value lists
// windows whose per-window status header explicitly read "allowed" on this
// response -- a targeted, single-window clear signal that lets
// applyRateLimitWarningTransition clear one window's stale mark even when a
// DIFFERENT window is newly warned on the very same response (so the
// unsuffixed status still reads allowed_warning and the third return value
// below never fires). An ABSENT window header is deliberately excluded from
// this list -- it stays a no-op, same as today, because a header can vanish
// from a later response even though the credential is still warned. The
// third return value reports an affirmative provider-wide all-clear (the
// unsuffixed Anthropic-Ratelimit-Unified-Status header equals "allowed"),
// which the caller treats as clearing every previously recorded warning,
// not just the windows present on this response. Only "rejected" (worse
// than warned) and "allowed_warning" (unchanged) fall through neither new
// branch; only an explicit "allowed" clears.
func parseClaudeRateLimitWarnings(provider string, headers http.Header) (map[string]RateLimitWarning, []string, bool) {
	if strings.ToLower(strings.TrimSpace(provider)) != "claude" || headers == nil {
		return nil, nil, false
	}
	now := time.Now().UTC()
	var warnings map[string]RateLimitWarning
	var clearedWindows []string
	for _, window := range claudeRateLimitWarningWindows {
		status := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-"+window+"-Status")))
		switch status {
		case "allowed_warning":
			warning := RateLimitWarning{Window: window, ObservedAt: now}
			if raw := strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-"+window+"-Reset")); raw != "" {
				if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
					warning.ResetAt = time.Unix(seconds, 0).UTC()
				}
			}
			if warnings == nil {
				warnings = make(map[string]RateLimitWarning, len(claudeRateLimitWarningWindows))
			}
			warnings[window] = warning
		case "allowed":
			clearedWindows = append(clearedWindows, window)
		default:
			// Absent header, "rejected", or any other value: no-op for this
			// window. Absence must never be read as an all-clear, and
			// "rejected" is worse than warned, not better -- neither clears.
		}
	}
	allClear := strings.ToLower(strings.TrimSpace(getHeaderCaseInsensitive(headers, "Anthropic-Ratelimit-Unified-Status"))) == "allowed"
	return warnings, clearedWindows, allClear
}

// parseUsageRetryHints returns a provider-specific retry delay and/or reset
// timestamp for a 429 response, plus whether the hint is credential-scoped
// (applies to every model on the credential) rather than model-scoped. A
// provider with no explicit case below always returns (nil, nil, false), so
// extending or narrowing the quotaCooldownAfterFailure gate to "a hint was
// produced" instead of a hardcoded provider string cannot change behavior
// for a provider that has no case here.
func parseUsageRetryHints(provider, body string, statusCode int, headers http.Header) (*time.Duration, *time.Time, bool) {
	if statusCode != http.StatusTooManyRequests {
		return nil, nil, false
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "antigravity":
		if !gjson.Valid(body) {
			return nil, nil, false
		}
		details := gjson.Get(body, "error.details")
		if !details.Exists() || !details.IsArray() {
			return nil, nil, false
		}

		var retryAfter *time.Duration
		var resetAt *time.Time
		for _, detail := range details.Array() {
			switch detail.Get("@type").String() {
			case "type.googleapis.com/google.rpc.RetryInfo":
				if retryAfter == nil {
					retryAfter = parseDurationPointer(detail.Get("retryDelay").String())
				}
			case "type.googleapis.com/google.rpc.ErrorInfo":
				if resetAt == nil {
					value := strings.TrimSpace(detail.Get("metadata.quotaResetTimeStamp").String())
					if value != "" {
						parsedResetAt, errParse := time.Parse(time.RFC3339Nano, value)
						if errParse == nil {
							parsedResetAt = parsedResetAt.UTC()
							resetAt = &parsedResetAt
						}
					}
				}
			}
		}
		return retryAfter, resetAt, false
	case "codex":
		if !gjson.Valid(body) {
			return nil, nil, false
		}
		if strings.TrimSpace(gjson.Get(body, "error.type").String()) != "usage_limit_reached" {
			return nil, nil, false
		}
		var resetAt *time.Time
		if seconds, ok := parseUsageHintSeconds(gjson.Get(body, "error.resets_at")); ok {
			parsedResetAt := time.Unix(seconds, 0).UTC()
			resetAt = &parsedResetAt
		}
		var retryAfter *time.Duration
		if seconds, ok := parseUsageHintSeconds(gjson.Get(body, "error.resets_in_seconds")); ok {
			duration := time.Duration(seconds) * time.Second
			retryAfter = &duration
		}
		return retryAfter, resetAt, false
	case "claude":
		// Anthropic reports rate-limit reset information in response headers,
		// not the body, so this case ignores body entirely.
		credentialScope := claudeHeadersIndicateUnifiedRateLimitRejection(headers)
		resetAt := parseClaudeRateLimitResetAt(headers, time.Now().UTC())
		return nil, resetAt, credentialScope
	default:
		return nil, nil, false
	}
}

func parseUsageHintSeconds(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	seconds, errParse := strconv.ParseInt(value.Raw, 10, 64)
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	if errParse != nil || seconds <= 0 || seconds > maxSeconds {
		return 0, false
	}
	return seconds, true
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
