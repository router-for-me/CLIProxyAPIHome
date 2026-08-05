package auth

import (
	"net/http"
	"testing"
	"time"
)

func quotaCooldownModelState(now time.Time, delay time.Duration) *ModelState {
	next := now.Add(delay)
	return &ModelState{
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: next,
			BackoffLevel:  3,
		},
		UpdatedAt: now,
	}
}

func TestModelScopedAggregateDoesNotBlockUnrelatedModel(t *testing.T) {
	now := time.Now().UTC()
	early := now.Add(10 * time.Minute)
	late := now.Add(20 * time.Minute)
	auth := &Auth{
		ID:       "auth-model-aggregate",
		Provider: "antigravity",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: early,
				Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: early, BackoffLevel: 1},
			},
			"model-b": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: late,
				Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: late, BackoffLevel: 3},
			},
		},
	}

	updateAggregatedAvailability(auth, now)
	if !auth.Quota.Exceeded || auth.Quota.Scope != quotaScopeModel || !auth.Quota.NextRecoverAt.Equal(early) || auth.Quota.BackoffLevel != 3 {
		t.Fatalf("model aggregate = %#v, want earliest recovery and maximum backoff", auth.Quota)
	}
	if blocked, reason, next := isAuthBlockedForModel(auth, "model-c", now); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("unrelated model blocked/reason/next = %v/%v/%v, want available", blocked, reason, next)
	}
}

func TestLegacyMixedModelAggregateDoesNotBlockUnrelatedModel(t *testing.T) {
	now := time.Now().UTC()
	early := now.Add(10 * time.Minute)
	late := now.Add(20 * time.Minute)
	auth := &Auth{
		ID:       "auth-legacy-model-aggregate",
		Provider: "antigravity",
		Status:   StatusError,
		Quota:    QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: early, BackoffLevel: 3},
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: early,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: early, BackoffLevel: 1},
			},
			"model-b": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: late,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: late, BackoffLevel: 3},
			},
		},
	}

	if blocked, reason, next := isAuthBlockedForModel(auth, "model-c", now); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("unrelated model blocked/reason/next = %v/%v/%v, want available", blocked, reason, next)
	}
}

func TestLegacyCredentialWideStateDoesNotBlockDispatch(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(15 * time.Minute)
	auth := &Auth{
		ID:             "auth-legacy-wide",
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          QuotaState{Exceeded: true, Scope: "credential", Reason: "quota", NextRecoverAt: next, BackoffLevel: 4},
	}

	for _, model := range []string{"", "gpt-5"} {
		if blocked, reason, retryAt := isAuthBlockedForModel(auth, model, now); blocked || reason != blockReasonNone || !retryAt.IsZero() {
			t.Fatalf("model %q blocked/reason/next = %v/%v/%v, want available", model, blocked, reason, retryAt)
		}
	}
}
