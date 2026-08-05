package home

import (
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestMergeModelStatesQuotaResetPreservesLocalNonQuotaError(t *testing.T) {
	now := time.Now().UTC()
	resetAt := now.Add(time.Minute)
	incoming := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:       coreauth.StatusActive,
			UpdatedAt:    resetAt,
			QuotaResetAt: resetAt,
		},
	}
	localRetry := now.Add(2 * time.Minute)
	local := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:         coreauth.StatusError,
			StatusMessage:  "transient upstream error",
			Unavailable:    true,
			NextRetryAfter: localRetry,
			LastError:      &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(10 * time.Minute), BackoffLevel: 3},
			UpdatedAt:      now,
		},
	}

	merged := mergeModelStates(incoming, local)
	state := merged["gpt-a"]
	if state == nil || state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable || !state.NextRetryAfter.Equal(localRetry) {
		t.Fatalf("merged state = %#v, want local 5xx preserved", state)
	}
	if state.Quota.Exceeded || !state.QuotaResetAt.Equal(resetAt) {
		t.Fatalf("merged quota/reset = %#v/%v, want quota cleared at %v", state.Quota, state.QuotaResetAt, resetAt)
	}
}

func TestMergeModelStatesQuotaResetOverridesNewerLocalQuota(t *testing.T) {
	now := time.Now().UTC()
	resetAt := now.Add(2 * time.Minute)
	incoming := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:       coreauth.StatusActive,
			UpdatedAt:    now,
			QuotaResetAt: resetAt,
		},
	}
	local := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:         coreauth.StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: now.Add(10 * time.Minute),
			LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
			Quota:          coreauth.QuotaState{Exceeded: true, Scope: "model", Reason: "quota", NextRecoverAt: now.Add(10 * time.Minute), BackoffLevel: 3},
			UpdatedAt:      now.Add(time.Minute),
		},
	}

	merged := mergeModelStates(incoming, local)
	state := merged["gpt-a"]
	if state == nil || state.Status != coreauth.StatusActive || state.Unavailable || state.LastError != nil || state.Quota.Exceeded {
		t.Fatalf("merged state = %#v, want persisted quota reset", state)
	}
	if !state.QuotaResetAt.Equal(resetAt) {
		t.Fatalf("QuotaResetAt = %v, want %v", state.QuotaResetAt, resetAt)
	}
}

func TestMergeModelStatesPreservesSharedQuotaWithNewerLocalError(t *testing.T) {
	now := time.Now().UTC()
	quotaRecover := now.Add(10 * time.Minute)
	incoming := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:         coreauth.StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: quotaRecover,
			LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
			Quota:          coreauth.QuotaState{Exceeded: true, Scope: "model", Reason: "quota", NextRecoverAt: quotaRecover, BackoffLevel: 3},
			UpdatedAt:      now,
		},
	}
	localUpdated := now.Add(2 * time.Minute)
	local := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:         coreauth.StatusError,
			StatusMessage:  "transient upstream error",
			Unavailable:    true,
			NextRetryAfter: now.Add(time.Minute),
			LastError:      &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			UpdatedAt:      localUpdated,
		},
	}

	merged := mergeModelStates(incoming, local)
	state := merged["gpt-a"]
	if state == nil || state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("merged state = %#v, want newer local 5xx", state)
	}
	if !state.Quota.Exceeded || state.Quota.Scope != "model" || !state.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("merged quota = %#v, want shared quota preserved", state.Quota)
	}
	if !state.Unavailable || !state.NextRetryAfter.Equal(quotaRecover) || !state.UpdatedAt.Equal(localUpdated) {
		t.Fatalf("merged availability/timestamp = %v/%v/%v", state.Unavailable, state.NextRetryAfter, state.UpdatedAt)
	}
}

func TestMergeModelStatesNewerSuccessSupersedesLocalErrorWithoutResetMarker(t *testing.T) {
	now := time.Now().UTC()
	incoming := map[string]*coreauth.ModelState{
		"gpt-a": {Status: coreauth.StatusActive, UpdatedAt: now.Add(time.Minute)},
	}
	local := map[string]*coreauth.ModelState{
		"gpt-a": {
			Status:      coreauth.StatusError,
			Unavailable: true,
			LastError:   &coreauth.Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			UpdatedAt:   now,
		},
	}

	merged := mergeModelStates(incoming, local)
	state := merged["gpt-a"]
	if state == nil || state.Status != coreauth.StatusActive || state.Unavailable || state.LastError != nil {
		t.Fatalf("merged state = %#v, want newer successful state", state)
	}
}
