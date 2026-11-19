package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func genericCodex429Result(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Error: &Error{
			Code:       "rate_limit_exceeded",
			Message:    "rate limit exceeded",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
	}
}

func TestGenericCodex429EscalatesAcrossDistinctModels(t *testing.T) {
	const authID = "auth-generic-codex-429"
	store := &fakeMutatorStore{persisted: &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
	}}
	nodeA := newHomeNodeManager(t, store, authID)
	nodeB := newHomeNodeManager(t, store, authID)

	nodeA.MarkResult(context.Background(), genericCodex429Result(authID, "gpt-5"))
	first := store.persistedSnapshot()
	if first.Quota.Scope != quotaScopeModel || first.Quota.Reason != "quota" || first.GenericCodex429 == nil || len(first.GenericCodex429.Models) != 1 {
		t.Fatalf("first generic 429 state = %#v, want model-only cooldown and one observation", first)
	}
	if state := first.ModelStates["gpt-5"]; state == nil || !state.Quota.Exceeded {
		t.Fatalf("first model state = %#v, want model quota cooldown", state)
	}

	nodeB.MarkResult(context.Background(), genericCodex429Result(authID, "gpt-5-mini"))
	escalated := store.persistedSnapshot()
	if !escalated.Quota.Exceeded || escalated.Quota.Scope != "credential" || escalated.Quota.Reason != "credential_quota" {
		t.Fatalf("escalated credential quota = %#v, want credential-wide cooldown", escalated.Quota)
	}
	if !escalated.Quota.NextRecoverAt.After(time.Now()) || escalated.Quota.NextRecoverAt.Sub(time.Now()) > genericCodex429Cooldown {
		t.Fatalf("escalated recovery = %v, want approximately %v", escalated.Quota.NextRecoverAt, genericCodex429Cooldown)
	}
	if got := len(escalated.GenericCodex429.Models); got != genericCodex429ModelLimit {
		t.Fatalf("generic observation models = %d, want %d", got, genericCodex429ModelLimit)
	}
	if blocked, reason, next := isAuthBlockedForModel(escalated, "an-unrelated-model", time.Now()); !blocked || reason != blockReasonCooldown || next.IsZero() {
		t.Fatalf("unrelated model availability = %v/%v/%v, want credential cooldown", blocked, reason, next)
	}
}

func TestGenericCodex429SameModelDoesNotEscalate(t *testing.T) {
	const authID = "auth-generic-codex-same-model"
	store := &fakeMutatorStore{persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive}}
	manager := newHomeNodeManager(t, store, authID)

	manager.MarkResult(context.Background(), genericCodex429Result(authID, "gpt-5"))
	manager.MarkResult(context.Background(), genericCodex429Result(authID, "gpt-5"))

	got := store.persistedSnapshot()
	if got.Quota.Scope == "credential" || got.Quota.Reason == "credential_quota" {
		t.Fatalf("same-model generic 429 escalated credential quota = %#v", got.Quota)
	}
	if got.GenericCodex429 == nil || len(got.GenericCodex429.Models) != 1 || got.GenericCodex429.Models[0] != "gpt-5" {
		t.Fatalf("same-model generic observations = %#v, want one model", got.GenericCodex429)
	}
}

func TestGenericCodex429SuccessClearsObservation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-generic-codex-success", Index: "auth-generic-codex-success", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}

	manager.MarkResult(context.Background(), genericCodex429Result(auth.ID, "gpt-5"))
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true})
	manager.MarkResult(context.Background(), genericCodex429Result(auth.ID, "gpt-5-mini"))

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("auth disappeared after success recovery")
	}
	if got.GenericCodex429 == nil || len(got.GenericCodex429.Models) != 1 || got.GenericCodex429.Models[0] != "gpt-5-mini" {
		t.Fatalf("observation after success recovery = %#v, want only new model", got.GenericCodex429)
	}
	if got.Quota.Scope == "credential" || got.Quota.Reason == "credential_quota" {
		t.Fatalf("success recovery left credential quota = %#v", got.Quota)
	}
}

func TestGenericCodex429WindowExpiryStartsFresh(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		Provider: "codex",
		GenericCodex429: &GenericCodex429State{
			WindowStartedAt: now.Add(-genericCodex429Window - time.Second),
			Models:          []string{"gpt-5"},
		},
	}

	if escalated := recordGenericCodex429(auth, "gpt-5-mini", now); escalated {
		t.Fatal("expired observation window escalated credential quota")
	}
	if auth.GenericCodex429 == nil || len(auth.GenericCodex429.Models) != 1 || auth.GenericCodex429.Models[0] != "gpt-5-mini" || !auth.GenericCodex429.WindowStartedAt.Equal(now) {
		t.Fatalf("fresh observation window = %#v, want only new model at now", auth.GenericCodex429)
	}
}

func TestGenericCodex429ClassificationExcludesExplicitLimits(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
	}{
		{name: "credential quota", err: &Error{Code: "credential_quota", Message: "quota exceeded", HTTPStatus: http.StatusTooManyRequests}},
		{name: "usage limit", err: &Error{Code: "usage_limit_reached", Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests}},
		{name: "turn capacity", err: &Error{Code: "turn_rate_limit_exceeded", Message: "Codex is at capacity", HTTPStatus: http.StatusTooManyRequests}},
		{name: "transient rate limit", err: &Error{Code: "transient_rate_limit", Message: "rate limited", HTTPStatus: http.StatusTooManyRequests}},
		{name: "request scoped", err: &Error{Code: ErrorCodeRequestScoped, Message: "request scoped", HTTPStatus: http.StatusTooManyRequests}},
		{name: "forced cooldown", err: &Error{Code: ErrorCodeForceCooldown, Message: "forced", HTTPStatus: http.StatusTooManyRequests}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Result{Provider: "codex", Model: "gpt-5", Error: test.err}
			if isGenericCodex429Result(result) {
				t.Fatal("explicitly classified 429 was treated as generic")
			}
		})
	}
	if !isGenericCodex429Result(genericCodex429Result("auth", "gpt-5")) {
		t.Fatal("unclassified rate_limit_exceeded 429 was not treated as generic")
	}
}

func TestGenericCodex429StatePersistsThroughJSON(t *testing.T) {
	original := &Auth{
		ID:       "auth-generic-codex-json",
		Provider: "codex",
		GenericCodex429: &GenericCodex429State{
			WindowStartedAt: time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC),
			Models:          []string{"gpt-5", "gpt-5-mini"},
		},
	}
	raw, errMarshal := json.Marshal(original)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var restored Auth
	if errUnmarshal := json.Unmarshal(raw, &restored); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if restored.GenericCodex429 == nil || restored.GenericCodex429.WindowStartedAt.IsZero() || len(restored.GenericCodex429.Models) != 2 {
		t.Fatalf("restored generic observation = %#v, want persisted state", restored.GenericCodex429)
	}
}

func TestGenericCodex429TriggersSessionAffinityRebind(t *testing.T) {
	now := time.Now().UTC()
	blocked := &Auth{
		ID:       "generic-codex-blocked",
		Provider: "codex",
		Status:   StatusError,
		Quota: QuotaState{
			Exceeded:      true,
			Scope:         "credential",
			Reason:        "credential_quota",
			NextRecoverAt: now.Add(genericCodex429Cooldown),
		},
	}
	if !sessionAffinityQuotaBlocked(blocked, "gpt-5", now) {
		t.Fatal("credential-wide generic 429 cooldown did not force session rebind")
	}

	recovered := blocked.Clone()
	recovered.Quota.NextRecoverAt = now.Add(-time.Second)
	if sessionAffinityQuotaBlocked(recovered, "gpt-5", now) {
		t.Fatal("expired generic 429 cooldown forced a session rebind")
	}
}
