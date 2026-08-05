package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
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

func TestClearModelQuotaCooldownKeepsLegacyMergeTimestamp(t *testing.T) {
	observedAt := time.Now().UTC()
	resetAt := observedAt.Add(2 * time.Minute)
	state := quotaCooldownModelState(observedAt, 10*time.Minute)
	staleQuota := state.Clone()

	if !clearModelQuotaCooldown(state, resetAt) {
		t.Fatal("clearModelQuotaCooldown() = false, want quota cleared")
	}
	if !state.UpdatedAt.Equal(observedAt) {
		t.Fatalf("UpdatedAt = %v, want execution timestamp %v preserved", state.UpdatedAt, observedAt)
	}
	if !state.QuotaResetAt.Equal(resetAt) {
		t.Fatalf("QuotaResetAt = %v, want %v", state.QuotaResetAt, resetAt)
	}

	// Home versions before quota reset markers keep the newer model state by
	// UpdatedAt. Equal timestamps must adopt the cleared persisted quota.
	legacySelected := state.Clone()
	if staleQuota.UpdatedAt.After(legacySelected.UpdatedAt) {
		legacySelected = staleQuota
	}
	if legacySelected.Quota.Exceeded || legacySelected.Unavailable {
		t.Fatalf("legacy peer kept stale quota state: %#v", legacySelected)
	}

	// A non-quota error observed after the quota must remain newer than the
	// reset snapshot, so a rolling-upgrade peer does not resume it early.
	newerError := staleQuota.Clone()
	newerError.LastError = &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable}
	newerError.StatusMessage = "transient upstream error"
	newerError.NextRetryAfter = observedAt.Add(5 * time.Minute)
	newerError.UpdatedAt = observedAt.Add(time.Minute)
	legacySelected = state.Clone()
	if newerError.UpdatedAt.After(legacySelected.UpdatedAt) {
		legacySelected = newerError
	}
	if legacySelected.LastError == nil || legacySelected.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("legacy peer lost newer non-quota error: %#v", legacySelected)
	}
}

func TestClearQuotaCooldownOverridesNewerLocalQuotaTimestamp(t *testing.T) {
	const authID = "auth-reset-newer-local-quota"
	const model = "gpt-5"
	store := &fakeMutatorStore{persisted: &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
	}}
	manager := newHomeNodeManager(t, store, authID)

	manager.MarkResult(context.Background(), quotaResult(authID, model))
	persistedBefore := store.persistedSnapshot().ModelStates[model]
	if persistedBefore == nil || !persistedBefore.Quota.Exceeded {
		t.Fatalf("persisted state after first 429 = %#v", persistedBefore)
	}

	// A repeated 429 inside the open window stays local but advances the
	// execution timestamp beyond the persisted quota snapshot.
	manager.MarkResult(context.Background(), quotaResult(authID, model))
	localBefore, ok := manager.GetByID(authID)
	if !ok || localBefore.ModelStates[model] == nil || !localBefore.ModelStates[model].UpdatedAt.After(persistedBefore.UpdatedAt) {
		t.Fatalf("local state before reset = %#v, want newer in-window 429", localBefore)
	}
	if mutations := store.mutationCount(); mutations != 1 {
		t.Fatalf("mutation count before reset = %d, want one persisted 429", mutations)
	}

	result, errClear := manager.ClearQuotaCooldown(context.Background(), authID, model)
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if !result.Cleared || !reflect.DeepEqual(result.ClearedModels, []string{model}) {
		t.Fatalf("ClearQuotaCooldown() result = %#v", result)
	}

	persistedAfter := store.persistedSnapshot().ModelStates[model]
	if persistedAfter == nil || persistedAfter.Quota.Exceeded || persistedAfter.QuotaResetAt.IsZero() {
		t.Fatalf("persisted state after reset = %#v", persistedAfter)
	}
	localAfter, ok := manager.GetByID(authID)
	if !ok || localAfter.ModelStates[model] == nil {
		t.Fatalf("local state after reset = %#v", localAfter)
	}
	localState := localAfter.ModelStates[model]
	if localState.Quota.Exceeded || localState.QuotaResetAt.IsZero() || localState.Unavailable || !localState.NextRetryAfter.IsZero() {
		t.Fatalf("local state after reset = %#v, want persisted reset adopted", localState)
	}
	if blocked, reason, _ := isAuthBlockedForModel(localAfter, model, time.Now()); blocked || reason != blockReasonNone {
		t.Fatalf("model blocked/reason after reset = %v/%v, want available", blocked, reason)
	}
}

func TestClearQuotaCooldownResolvesConfiguredAlias(t *testing.T) {
	const authID = "auth-reset-alias"
	const routeModel = "team/alias-b"
	const upstreamModel = "upstream-shared"
	now := time.Now().UTC()
	quotaState := quotaCooldownModelState(now, 10*time.Minute)
	quotaState.Quota.Scope = quotaScopeModel
	auth := geminiAPIKeyAuth(authID, "high-key", "1")
	auth.Prefix = "team"
	auth.Status = StatusError
	auth.Quota = quotaState.Quota
	auth.ModelStates = map[string]*ModelState{
		routeModel:    quotaState.Clone(),
		upstreamModel: quotaState,
	}
	store := &fakeMutatorStore{persisted: auth.Clone()}
	manager := NewManager(store, nil, nil)
	setGeminiAliasConfig(manager)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	result, errClear := manager.ClearQuotaCooldown(context.Background(), authID, routeModel+"(high)")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if !result.Cleared || result.Model != upstreamModel || !reflect.DeepEqual(result.ClearedModels, []string{routeModel, upstreamModel}) {
		t.Fatalf("ClearQuotaCooldown() result = %#v", result)
	}
	persisted := store.persistedSnapshot()
	for _, model := range []string{routeModel, upstreamModel} {
		state := persisted.ModelStates[model]
		if state == nil || state.Quota.Exceeded || state.Unavailable || state.QuotaResetAt.IsZero() {
			t.Fatalf("persisted %s state after alias reset = %#v", model, state)
		}
	}
}

func TestMergePersistedModelStatePreservesActiveQuotaWithNewerNonQuotaError(t *testing.T) {
	now := time.Now().UTC()
	quotaRecover := now.Add(10 * time.Minute)
	persisted := quotaCooldownModelState(now, 10*time.Minute)
	persisted.Quota.Scope = quotaScopeModel
	localRetry := now.Add(time.Minute)
	localUpdated := now.Add(2 * time.Minute)
	local := &ModelState{
		Status:         StatusError,
		StatusMessage:  "transient upstream error",
		Unavailable:    true,
		NextRetryAfter: localRetry,
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      localUpdated,
	}

	merged := MergePersistedModelState(persisted, local)
	if merged == nil || merged.LastError == nil || merged.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("merged state = %#v, want newer local 5xx", merged)
	}
	if !merged.Quota.Exceeded || merged.Quota.Scope != quotaScopeModel || !merged.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("merged quota = %#v, want persisted quota", merged.Quota)
	}
	if !merged.Unavailable || !merged.NextRetryAfter.Equal(quotaRecover) {
		t.Fatalf("merged availability = %v/%v, want quota deadline %v", merged.Unavailable, merged.NextRetryAfter, quotaRecover)
	}
	if !merged.UpdatedAt.Equal(localUpdated) {
		t.Fatalf("merged UpdatedAt = %v, want local timestamp %v", merged.UpdatedAt, localUpdated)
	}
	auth := &Auth{ModelStates: map[string]*ModelState{"gpt-a": merged}}
	if blocked, reason, next := isAuthBlockedForModel(auth, "gpt-a", now.Add(2*time.Minute)); !blocked || reason != blockReasonCooldown || !next.Equal(quotaRecover) {
		t.Fatalf("blocked/reason/next after local retry = %v/%v/%v, want shared quota until %v", blocked, reason, next, quotaRecover)
	}
}

func TestClearQuotaCooldownClearsOnlyRequestedModel(t *testing.T) {
	now := time.Now().UTC()
	modelA := quotaCooldownModelState(now, 10*time.Minute)
	modelB := quotaCooldownModelState(now, 20*time.Minute)
	refreshAt := now.Add(time.Hour)
	store := &fakeMutatorStore{persisted: &Auth{
		ID:               "auth-clear-one",
		Index:            "auth-clear-one",
		Provider:         "codex",
		Status:           StatusError,
		Unavailable:      true,
		NextRetryAfter:   modelA.NextRetryAfter,
		NextRefreshAfter: refreshAt,
		Quota:            modelA.Quota,
		ModelStates: map[string]*ModelState{
			"gpt-a": modelA,
			"gpt-b": modelB,
		},
	}}
	manager := newHomeNodeManager(t, store, "auth-clear-one")

	result, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-clear-one", "gpt-a(high)")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if !result.Cleared || result.Model != "gpt-a" || !reflect.DeepEqual(result.ClearedModels, []string{"gpt-a"}) {
		t.Fatalf("ClearQuotaCooldown() result = %#v", result)
	}

	persisted := store.persistedSnapshot()
	stateA := persisted.ModelStates["gpt-a"]
	if stateA == nil || stateA.Status != StatusActive || stateA.Unavailable || stateA.Quota.Exceeded || !stateA.NextRetryAfter.IsZero() || stateA.LastError != nil {
		t.Fatalf("gpt-a state = %#v, want cleared quota cooldown", stateA)
	}
	stateB := persisted.ModelStates["gpt-b"]
	if stateB == nil || !stateB.Quota.Exceeded || !stateB.Unavailable || stateB.NextRetryAfter.IsZero() {
		t.Fatalf("gpt-b state = %#v, want cooldown preserved", stateB)
	}
	if !persisted.NextRefreshAfter.Equal(refreshAt) {
		t.Fatalf("NextRefreshAfter = %v, want %v", persisted.NextRefreshAfter, refreshAt)
	}

	local, ok := manager.GetByID("auth-clear-one")
	if !ok || local == nil {
		t.Fatal("GetByID() missing auth")
	}
	if blocked, _, _ := isAuthBlockedForModel(local, "gpt-a", time.Now()); blocked {
		t.Fatal("gpt-a remains blocked after cooldown clear")
	}
	if blocked, reason, _ := isAuthBlockedForModel(local, "gpt-b", time.Now()); !blocked || reason != blockReasonCooldown {
		t.Fatalf("gpt-b blocked/reason = %v/%v, want quota cooldown", blocked, reason)
	}
}

func TestClearQuotaCooldownClearsAllQuotaModelsAndPreservesOtherErrors(t *testing.T) {
	now := time.Now().UTC()
	modelA := quotaCooldownModelState(now, 10*time.Minute)
	modelB := quotaCooldownModelState(now, 20*time.Minute)
	transientRetry := now.Add(time.Minute)
	transient := &ModelState{
		Status:         StatusError,
		StatusMessage:  "transient upstream error",
		Unavailable:    true,
		NextRetryAfter: transientRetry,
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      now,
	}
	store := &fakeMutatorStore{persisted: &Auth{
		ID:             "auth-clear-all",
		Index:          "auth-clear-all",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  modelA.StatusMessage,
		LastError:      cloneError(modelA.LastError),
		Unavailable:    true,
		NextRetryAfter: modelA.NextRetryAfter,
		Quota:          modelA.Quota,
		ModelStates: map[string]*ModelState{
			"gpt-a": modelA,
			"gpt-b": modelB,
			"gpt-c": transient,
		},
	}}
	manager := newHomeNodeManager(t, store, "auth-clear-all")

	result, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-clear-all", "")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if !result.Cleared || !reflect.DeepEqual(result.ClearedModels, []string{"gpt-a", "gpt-b"}) {
		t.Fatalf("ClearQuotaCooldown() result = %#v", result)
	}

	persisted := store.persistedSnapshot()
	for _, model := range []string{"gpt-a", "gpt-b"} {
		state := persisted.ModelStates[model]
		if state == nil || state.Status != StatusActive || state.Unavailable || state.Quota.Exceeded {
			t.Fatalf("%s state = %#v, want cleared quota cooldown", model, state)
		}
	}
	stateC := persisted.ModelStates["gpt-c"]
	if stateC == nil || stateC.LastError == nil || stateC.LastError.HTTPStatus != http.StatusServiceUnavailable || !stateC.NextRetryAfter.Equal(transientRetry) {
		t.Fatalf("gpt-c state = %#v, want transient error preserved", stateC)
	}
	if persisted.Status != StatusError || persisted.LastError == nil || persisted.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("aggregate state = %#v, want non-quota error preserved", persisted)
	}
	if persisted.Quota.Exceeded {
		t.Fatalf("aggregate quota = %#v, want cleared", persisted.Quota)
	}
}

func TestClearQuotaCooldownPreservesLegacyCredentialQuota(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		quotaScope string
	}{
		{name: "one model explicit scope", model: "gpt-a", quotaScope: "credential"},
		{name: "all models explicit scope", quotaScope: "credential"},
		{name: "one model legacy scope", model: "gpt-a"},
		{name: "all models legacy scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			modelState := quotaCooldownModelState(now, 10*time.Minute)
			credentialRetry := now.Add(20 * time.Minute)
			credentialQuota := QuotaState{
				Exceeded:      true,
				Scope:         tc.quotaScope,
				Reason:        "quota",
				NextRecoverAt: credentialRetry,
				BackoffLevel:  5,
			}
			store := &fakeMutatorStore{persisted: &Auth{
				ID:             "auth-legacy-reset",
				Index:          "auth-legacy-reset",
				Provider:       "codex",
				Status:         StatusError,
				StatusMessage:  "legacy credential quota",
				Unavailable:    true,
				NextRetryAfter: credentialRetry,
				LastError:      &Error{Message: "legacy credential quota", HTTPStatus: http.StatusTooManyRequests},
				Quota:          credentialQuota,
				ModelStates:    map[string]*ModelState{"gpt-a": modelState},
			}}
			manager := newHomeNodeManager(t, store, "auth-legacy-reset")

			result, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-legacy-reset", tc.model)
			if errClear != nil {
				t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
			}
			if !result.Cleared || !reflect.DeepEqual(result.ClearedModels, []string{"gpt-a"}) {
				t.Fatalf("ClearQuotaCooldown() result = %#v", result)
			}
			persisted := store.persistedSnapshot()
			if persisted.ModelStates["gpt-a"].Quota.Exceeded {
				t.Fatalf("model quota = %#v, want cleared", persisted.ModelStates["gpt-a"].Quota)
			}
			if !reflect.DeepEqual(persisted.Quota, credentialQuota) || persisted.Status != StatusError || persisted.StatusMessage != "legacy credential quota" || !persisted.Unavailable || !persisted.NextRetryAfter.Equal(credentialRetry) {
				t.Fatalf("persisted legacy credential state changed: %#v", persisted)
			}
			local, ok := manager.GetByID("auth-legacy-reset")
			if !ok || local == nil || !reflect.DeepEqual(local.Quota, credentialQuota) || local.Status != StatusError || local.StatusMessage != "legacy credential quota" || !local.Unavailable || !local.NextRetryAfter.Equal(credentialRetry) {
				t.Fatalf("local legacy credential state changed: %#v", local)
			}
		})
	}
}

func TestClearQuotaCooldownPreservesDisabledState(t *testing.T) {
	now := time.Now().UTC()
	state := quotaCooldownModelState(now, 10*time.Minute)
	store := &fakeMutatorStore{persisted: &Auth{
		ID:            "auth-disabled",
		Index:         "auth-disabled",
		Provider:      "codex",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "disabled via management API",
		Unavailable:   true,
		Quota:         state.Quota,
		ModelStates:   map[string]*ModelState{"gpt-a": state},
	}}
	manager := newHomeNodeManager(t, store, "auth-disabled")

	result, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-disabled", "")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if !result.Cleared {
		t.Fatalf("ClearQuotaCooldown() result = %#v", result)
	}
	persisted := store.persistedSnapshot()
	if !persisted.Disabled || persisted.Status != StatusDisabled || !persisted.Unavailable || persisted.StatusMessage != "disabled via management API" {
		t.Fatalf("disabled state = %#v, want preserved", persisted)
	}
	if persisted.ModelStates["gpt-a"].Quota.Exceeded {
		t.Fatalf("model quota = %#v, want bookkeeping cleared", persisted.ModelStates["gpt-a"].Quota)
	}
}

func TestClearQuotaCooldownPreservesLocalNonQuotaState(t *testing.T) {
	now := time.Now().UTC()
	quotaState := quotaCooldownModelState(now, 10*time.Minute)
	quotaState.Quota.Scope = quotaScopeModel
	store := &fakeMutatorStore{persisted: &Auth{
		ID:             "auth-local-non-quota",
		Index:          "auth-local-non-quota",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  quotaState.StatusMessage,
		LastError:      cloneError(quotaState.LastError),
		Unavailable:    true,
		NextRetryAfter: quotaState.NextRetryAfter,
		Quota:          quotaState.Quota,
		ModelStates:    map[string]*ModelState{"gpt-a": quotaState},
	}}
	manager := newHomeNodeManager(t, store, "auth-local-non-quota")
	manager.MarkResult(context.Background(), Result{
		AuthID:   "auth-local-non-quota",
		Provider: "codex",
		Model:    "gpt-a",
		Success:  false,
		Error:    &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
	})

	result, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-local-non-quota", "gpt-a")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if !result.Cleared {
		t.Fatalf("ClearQuotaCooldown() result = %#v", result)
	}
	local, ok := manager.GetByID("auth-local-non-quota")
	if !ok || local == nil || local.ModelStates["gpt-a"] == nil {
		t.Fatalf("GetByID() = %#v/%v", local, ok)
	}
	state := local.ModelStates["gpt-a"]
	if state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable || state.Quota.Exceeded || state.NextRetryAfter.IsZero() {
		t.Fatalf("local model state = %#v, want 5xx preserved with quota cleared", state)
	}
}

func TestClearQuotaCooldownDoesNotClearNonQuotaModelError(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(time.Minute)
	state := &ModelState{
		Status:         StatusError,
		StatusMessage:  "transient upstream error",
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      now,
	}
	store := &fakeMutatorStore{persisted: &Auth{
		ID:          "auth-non-quota",
		Index:       "auth-non-quota",
		Provider:    "codex",
		Status:      StatusError,
		Unavailable: true,
		ModelStates: map[string]*ModelState{"gpt-a": state},
	}}
	manager := newHomeNodeManager(t, store, "auth-non-quota")

	result, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-non-quota", "gpt-a")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if result.Cleared || store.mutationCount() != 0 {
		t.Fatalf("ClearQuotaCooldown() result/mutations = %#v/%d, want no-op", result, store.mutationCount())
	}
	persisted := store.persistedSnapshot().ModelStates["gpt-a"]
	if persisted.LastError == nil || persisted.LastError.HTTPStatus != http.StatusServiceUnavailable || !persisted.NextRetryAfter.Equal(next) {
		t.Fatalf("non-quota state = %#v, want preserved", persisted)
	}
}

func TestModelSuccessPersistsQuotaResetMarkerRemoval(t *testing.T) {
	const authID = "auth-reset-marker-success"
	const model = "gpt-a"
	now := time.Now().UTC()
	store := &fakeMutatorStore{persisted: &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: {Status: StatusActive, UpdatedAt: now, QuotaResetAt: now}},
	}}
	manager := NewManager(store, nil, nil)
	local := &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: {Status: StatusActive, UpdatedAt: now, QuotaResetAt: now}},
	}
	if _, errRegister := manager.Register(context.Background(), local); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: "codex", Model: model, Success: true})
	if marker := store.persistedSnapshot().ModelStates[model].QuotaResetAt; !marker.IsZero() {
		t.Fatalf("persisted QuotaResetAt = %v, want zero", marker)
	}
	if store.mutationCount() != 1 {
		t.Fatalf("mutation count = %d, want 1 marker update", store.mutationCount())
	}
	got, ok := manager.GetByID(authID)
	if !ok || got.ModelStates[model] == nil || got.ModelStates[model].LastError != nil || got.ModelStates[model].Status != StatusActive {
		t.Fatalf("local state after success = %#v", got)
	}
}

func TestUnauthorizedPreservesQuotaResetMarker(t *testing.T) {
	const authID = "auth-reset-marker-unauthorized"
	const model = "gpt-a"
	now := time.Now().UTC()
	state := &ModelState{Status: StatusActive, UpdatedAt: now, QuotaResetAt: now}
	store := &fakeMutatorStore{persisted: &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: state.Clone()},
	}}
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: state.Clone()},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID: authID, Provider: "codex", Model: model,
		Error: &Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
	})

	persisted := store.persistedSnapshot().ModelStates[model]
	if persisted == nil || persisted.LastError == nil || persisted.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("persisted unauthorized state = %#v", persisted)
	}
	if !persisted.QuotaResetAt.Equal(now) {
		t.Fatalf("persisted QuotaResetAt = %v, want %v", persisted.QuotaResetAt, now)
	}
}

func TestClearQuotaCooldownKeepsRegistrySuspendedForPreserved403(t *testing.T) {
	const authID = "auth-registry-preserve"
	const model = "registry-preserve-model"
	now := time.Now().UTC()
	quotaState := quotaCooldownModelState(now, 10*time.Minute)
	quotaState.Quota.Scope = quotaScopeModel
	store := &fakeMutatorStore{persisted: &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusError,
		Quota: quotaState.Quota, ModelStates: map[string]*ModelState{model: quotaState},
	}}
	manager := newHomeNodeManager(t, store, authID)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager.MarkResult(context.Background(), Result{
		AuthID: authID, Provider: "codex", Model: model,
		Error: &Error{Message: "forbidden", HTTPStatus: http.StatusForbidden},
	})
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("403 did not suspend model before reset")
	}
	if _, errClear := manager.ClearQuotaCooldown(context.Background(), authID, model); errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	got, ok := manager.GetByID(authID)
	if !ok || got.ModelStates[model] == nil || got.ModelStates[model].LastError == nil || got.ModelStates[model].LastError.HTTPStatus != http.StatusForbidden {
		t.Fatalf("local 403 state after reset = %#v", got)
	}
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("registry resumed model despite preserved 403")
	}
}

func registryHasAvailableModel(modelRegistry *registry.ModelRegistry, model string) bool {
	for _, item := range modelRegistry.GetAvailableModelDefinitions() {
		if item != nil && item.ID == model {
			return true
		}
	}
	return false
}

func TestReconcileRegistryModelStatesKeepsTransientErrorsVisible(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "request-timeout", status: http.StatusRequestTimeout},
		{name: "internal-server-error", status: http.StatusInternalServerError},
		{name: "bad-gateway", status: http.StatusBadGateway},
		{name: "service-unavailable", status: http.StatusServiceUnavailable},
		{name: "gateway-timeout", status: http.StatusGatewayTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authID := "auth-registry-transient-" + tc.name
			model := "registry-transient-" + tc.name
			modelRegistry := registry.GetGlobalRegistry()
			modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			manager := NewManager(nil, nil, nil)
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive}); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			manager.MarkResult(context.Background(), Result{
				AuthID: authID, Provider: "codex", Model: model,
				Error: &Error{Message: http.StatusText(tc.status), HTTPStatus: tc.status},
			})
			manager.ReconcileRegistryModelStates(context.Background(), authID)
			if !registryHasAvailableModel(modelRegistry, model) {
				t.Fatalf("registry hid model for transient HTTP %d", tc.status)
			}
		})
	}
}

func TestReconcileRegistryModelStatesMapsUpstreamStateToRegisteredAlias(t *testing.T) {
	const authID = "auth-registry-alias"
	const routeModel = "team/alias-b"
	const upstreamModel = "upstream-shared"
	manager := NewManager(nil, nil, nil)
	setGeminiAliasConfig(manager)
	auth := geminiAPIKeyAuth(authID, "high-key", "1")
	auth.Prefix = "team"
	registerDispatchTestAuth(t, manager, auth, routeModel)

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "gemini",
		Model:    upstreamModel,
		Error:    &Error{Message: "forbidden", HTTPStatus: http.StatusForbidden},
	})
	manager.ReconcileRegistryModelStates(context.Background(), authID)

	if registryHasAvailableModel(registry.GetGlobalRegistry(), routeModel) {
		t.Fatal("registry kept alias visible despite blocked upstream model")
	}
}

func TestShouldSuspendRegistryModelMatchesResultTransitions(t *testing.T) {
	const model = "registry-transition-model"
	modelUnsupported := &Error{Message: "requested model is not supported", HTTPStatus: http.StatusBadRequest}
	cases := []struct {
		name   string
		reason blockReason
		err    *Error
		want   bool
	}{
		{name: "quota", reason: blockReasonCooldown, want: true},
		{name: "disabled", reason: blockReasonDisabled, want: true},
		{name: "payment-required", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusPaymentRequired}, want: true},
		{name: "forbidden", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusForbidden}, want: true},
		{name: "not-found", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusNotFound}, want: true},
		{name: "model-not-supported", reason: blockReasonOther, err: modelUnsupported, want: true},
		{name: "unauthorized", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusUnauthorized}},
		{name: "request-timeout", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusRequestTimeout}},
		{name: "service-unavailable", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusServiceUnavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{ModelStates: map[string]*ModelState{model: {LastError: tc.err}}}
			if got := shouldSuspendRegistryModel(auth, model, tc.reason); got != tc.want {
				t.Fatalf("shouldSuspendRegistryModel() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileRegistryModelStatesResumesPeerAfterStateReset(t *testing.T) {
	const authID = "auth-registry-peer"
	const model = "registry-peer-model"
	now := time.Now().UTC()
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	modelRegistry.SuspendClientModel(authID, model, "forbidden")
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("test setup did not suspend model")
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: {Status: StatusActive, UpdatedAt: now, QuotaResetAt: now}},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.ReconcileRegistryModelStates(context.Background(), authID)
	if !registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("registry remained suspended after peer reconciliation")
	}
}

func TestClearQuotaCooldownRequiresAtomicStore(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	_, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-no-store", "")
	if !errors.Is(errClear, ErrCooldownMutationUnsupported) {
		t.Fatalf("ClearQuotaCooldown() error = %v, want ErrCooldownMutationUnsupported", errClear)
	}
}
