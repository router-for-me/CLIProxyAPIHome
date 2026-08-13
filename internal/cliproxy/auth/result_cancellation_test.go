package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// canceledManagerAuth registers a credential ready to receive execution results.
func canceledManagerAuth(t *testing.T, id string) (*Manager, *Auth) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       id,
		Index:    id,
		Provider: "antigravity",
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return manager, auth
}

// TestClientCancellationLeavesCredentialUntouched covers the caller hanging up: the
// attempt says nothing about the credential, so it must not be counted, cooled down,
// or recorded as an error.
func TestClientCancellationLeavesCredentialUntouched(t *testing.T) {
	cancellations := []struct {
		name   string
		status int
		body   string
	}{
		{name: "context canceled", status: http.StatusInternalServerError, body: `Post "https://upstream/v1/chat": context canceled`},
		{name: "client closed request", status: statusClientClosedRequest, body: ""},
		{name: "client disconnected", status: http.StatusInternalServerError, body: "client disconnected before response"},
	}

	for _, tc := range cancellations {
		t.Run(tc.name, func(t *testing.T) {
			manager, auth := canceledManagerAuth(t, "auth-canceled-"+tc.name)

			manager.MarkResult(context.Background(), NewUsageResult(auth.Index, "antigravity", "gpt-5", tc.status, tc.body))

			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("updated auth not found")
			}
			if updated.Failed != 0 {
				t.Fatalf("Failed = %d, want the cancelled attempt uncounted", updated.Failed)
			}
			if updated.Status != StatusActive || updated.LastError != nil {
				t.Fatalf("auth = %#v, want an untouched active credential", updated)
			}
			if state := updated.ModelStates["gpt-5"]; state != nil {
				t.Fatalf("model state = %#v, want no state recorded for a cancellation", state)
			}
			if blocked, _, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); blocked {
				t.Fatal("a cancelled request cooled down the credential")
			}
		})
	}
}

// TestUpstreamTimeoutStillCoolsDown keeps the cancellation guard narrow: a deadline
// means the upstream stopped answering, which is exactly what the transient cooldown
// is for, even when the error text says "canceled".
func TestUpstreamTimeoutStillCoolsDown(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-timeout", false)

	applyFailure(auth, "gpt-5", http.StatusInternalServerError, `net/http: request canceled (Client.Timeout exceeded while awaiting headers)`, now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.Equal(now.Add(transientErrorCooldown)) {
		t.Fatalf("NextRetryAfter = %v, want the transient cooldown %v", state.NextRetryAfter, now.Add(transientErrorCooldown))
	}
}

// TestCancellationKeepsExistingCooldownIntact makes sure the guard drops the result
// instead of clearing state an earlier real failure established.
func TestCancellationKeepsExistingCooldownIntact(t *testing.T) {
	now := time.Now().UTC()
	manager, auth := canceledManagerAuth(t, "auth-cancel-after-quota")

	manager.MarkResult(context.Background(), NewUsageResult(auth.Index, "antigravity", "gpt-5", http.StatusTooManyRequests, "quota exceeded"))
	cooled, _ := manager.GetByID(auth.ID)
	state := cooled.ModelStates["gpt-5"]
	if state == nil || !state.Quota.Exceeded {
		t.Fatalf("model state = %#v, want a quota cooldown", state)
	}
	recoverAt := state.Quota.NextRecoverAt

	manager.MarkResult(context.Background(), NewUsageResult(auth.Index, "antigravity", "gpt-5", http.StatusInternalServerError, "context canceled"))

	after, _ := manager.GetByID(auth.ID)
	afterState := after.ModelStates["gpt-5"]
	if afterState == nil || !afterState.Quota.NextRecoverAt.Equal(recoverAt) {
		t.Fatalf("quota window = %#v, want it preserved at %v", afterState, recoverAt)
	}
	if blocked, _, _ := isAuthBlockedForModel(after, "gpt-5", now); !blocked {
		t.Fatal("the cancellation released an open quota cooldown")
	}
}
