package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// sessionAffinityTransientAuthIDsKey carries manager-validated candidates that
// may be missing from the selector's pool because of cooldown or request retries.
type sessionAffinityTransientAuthIDsKey struct{}

func sessionAffinityRebindFailure(err *Error) bool {
	if err == nil || err.Code == ErrorCodeRequestScoped || err.Code == ErrorCodeStreamTruncated {
		return false
	}
	if isModelSupportResultError(err) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(err.Code)) {
	case "invalid_grant", "credit_balance_too_low", "cloudflare_challenge":
		return true
	}
	switch statusCodeFromResult(err) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return true
	}
	return false
}

func sessionAffinityQuotaBlocked(auth *Auth, model string, now time.Time) bool {
	// Refresh acquisition blocks are temporary; expiry and authentication
	// failures still permit permanent rebinding.
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return true
	}
	if expiresAt, ok := auth.ExpirationTime(); ok && !expiresAt.IsZero() && !expiresAt.After(now) {
		return true
	}
	if RefreshBlocksDispatch(auth) && sessionAffinityRebindFailure(auth.LastError) {
		return true
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && (auth.Quota.NextRecoverAt.IsZero() || auth.Quota.NextRecoverAt.After(now)) {
		return true
	}
	if state := auth.ModelStates[canonicalModelKey(model)]; state != nil {
		if state.Status == StatusDisabled || (state.Quota.Exceeded && (state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now))) {
			return true
		}
		// Errors remain in history after cooldown expires. They must not turn
		// a later retry/admission exclusion into a permanent session rebind.
		return state.Unavailable && state.NextRetryAfter.After(now) && sessionAffinityRebindFailure(state.LastError)
	}
	if len(auth.ModelStates) > 0 {
		// Auth-level availability aggregates other models; only an explicit
		// credential-wide fence above applies to a model with no failure state.
		return false
	}
	return (auth.Quota.Exceeded && (auth.Quota.NextRecoverAt.IsZero() || auth.Quota.NextRecoverAt.After(now))) ||
		(auth.Unavailable && auth.NextRetryAfter.After(now) && sessionAffinityRebindFailure(auth.LastError))
}

func preserveSessionBinding(ctx context.Context, auths []*Auth, authID, model string, now time.Time) bool {
	if ids, ok := ctx.Value(sessionAffinityTransientAuthIDsKey{}).(map[string]struct{}); ok {
		_, found := ids[authID]
		return found
	}
	for _, auth := range auths {
		if auth != nil && auth.ID == authID {
			return !sessionAffinityQuotaBlocked(auth, model, now)
		}
	}
	return false
}
