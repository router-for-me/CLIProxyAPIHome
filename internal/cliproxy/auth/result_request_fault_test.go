package auth

import (
	"net/http"
	"testing"
	"time"
)

// TestRequestFaultLeavesCredentialUntouched covers rejections caused by the request
// itself. Every credential would answer the same way, so cooling one down only shrinks
// the pool while the caller replays a request that cannot succeed until it is rebuilt.
func TestRequestFaultLeavesCredentialUntouched(t *testing.T) {
	faults := []struct {
		name   string
		status int
		body   string
	}{
		{name: "bad request", status: http.StatusBadRequest, body: "invalid argument"},
		{name: "conflict", status: http.StatusConflict, body: "conflict"},
		{name: "payload too large", status: http.StatusRequestEntityTooLarge, body: "too large"},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, body: "unprocessable"},
		{name: "context length", status: http.StatusInternalServerError, body: `{"error":{"code":"context_length_exceeded"}}`},
		{name: "invalid request type", status: http.StatusInternalServerError, body: `{"error":{"type":"invalid_request_error"}}`},
		{name: "nested response body", status: http.StatusInternalServerError, body: `{"response":{"error":{"code":"invalid_prompt"}}}`},
		{name: "cyber policy", status: http.StatusInternalServerError, body: `{"code":"cyber_policy"}`},
	}

	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			now := time.Now().UTC()
			auth := failingAuth("auth-request-fault", false)

			applyFailure(auth, "gpt-5", fault.status, fault.body, now)

			if state := auth.ModelStates["gpt-5"]; state != nil {
				t.Fatalf("a request fault recorded model state: %#v", state)
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", now); blocked {
				t.Fatal("a request fault blocked the credential/model pair")
			}
			if auth.Unavailable || auth.Status == StatusError {
				t.Fatalf("a request fault touched credential state: unavailable=%v status=%v", auth.Unavailable, auth.Status)
			}
		})
	}
}

// TestProviderVerdictsOutrankRequestFaultCodes locks the classification order. Several
// provider verdicts arrive on the same status codes a request fault uses, so treating
// the status code as decisive would let a revoked grant or an unsupported model slip
// through without ever cooling down.
func TestProviderVerdictsOutrankRequestFaultCodes(t *testing.T) {
	verdicts := []struct {
		name   string
		status int
		body   string
	}{
		{name: "invalid grant on 400", status: http.StatusBadRequest, body: "invalid_grant"},
		{name: "model not supported on 400", status: http.StatusBadRequest, body: "model not supported"},
		{name: "model not supported on 422", status: http.StatusUnprocessableEntity, body: "model not supported"},
		{name: "cloudflare challenge on 400", status: http.StatusBadRequest, body: "cf-mitigated"},
	}

	for _, verdict := range verdicts {
		t.Run(verdict.name, func(t *testing.T) {
			now := time.Now().UTC()
			auth := failingAuth("auth-verdict", false)

			applyFailure(auth, "gpt-5", verdict.status, verdict.body, now)

			state := auth.ModelStates["gpt-5"]
			if state == nil {
				t.Fatal("a provider verdict was mistaken for a request fault")
			}
			blocked, _, next := isAuthBlockedForModel(auth, "gpt-5", now)
			if !blocked {
				t.Fatal("a provider verdict left the model dispatchable")
			}
			if !next.After(now) {
				t.Fatalf("deadline = %v, want a bounded window after %v", next, now)
			}
		})
	}
}

// TestRequestFaultKeepsExistingCooldown makes sure a request fault cannot be used to
// wipe a real cooldown recorded moments earlier by a genuine provider rejection.
func TestRequestFaultKeepsExistingCooldown(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-request-fault-after-block", false)

	applyFailure(auth, "gpt-5", http.StatusForbidden, "payment required", now)
	blockedBefore, _, deadlineBefore := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blockedBefore {
		t.Fatal("forbidden failure did not block the model")
	}

	applyFailure(auth, "gpt-5", http.StatusBadRequest, "invalid argument", now.Add(time.Second))

	blockedAfter, _, deadlineAfter := isAuthBlockedForModel(auth, "gpt-5", now.Add(time.Second))
	if !blockedAfter {
		t.Fatal("a request fault cleared an active cooldown")
	}
	if !deadlineAfter.Equal(deadlineBefore) {
		t.Fatalf("deadline moved from %v to %v", deadlineBefore, deadlineAfter)
	}
}
