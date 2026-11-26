package auth

import "time"

// Leave the same request safety margin used when retaining a token after a
// transient refresh failure. Nodes receive no refresh token from Home.
const dispatchExpirySafetyWindow = 5 * time.Minute

func credentialExpiryBlocksDispatch(auth *Auth, now time.Time) bool {
	if auth == nil || auth.AuthKind() != AuthKindOAuth {
		return false
	}
	expiresAt, known := auth.ExpirationTime()
	return known && !expiresAt.After(now.Add(dispatchExpirySafetyWindow))
}
