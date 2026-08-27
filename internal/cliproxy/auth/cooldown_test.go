package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
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

func TestClearDisabledCooldownStatesClearsCoolingAndPreservesOtherErrors(t *testing.T) {
	now := time.Now().UTC()
	transientRetry := now.Add(5 * time.Minute)
	quotaRetry := now.Add(10 * time.Minute)
	unauthorizedRetry := now.Add(15 * time.Minute)
	unsupportedRetry := now.Add(20 * time.Minute)
	const authID = "auth-clear-disabled-cooling"

	store := &fakeMutatorStore{persisted: &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusError,
		Metadata: map[string]any{"email": "user@example.com"},
		ModelStates: map[string]*ModelState{
			"transient": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: transientRetry,
				LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
				UpdatedAt:      now,
			},
			"quota": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: quotaRetry,
				LastError:      &Error{Message: "rate limited", HTTPStatus: http.StatusTooManyRequests},
				Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: quotaRetry},
				UpdatedAt:      now,
			},
			"unauthorized": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: unauthorizedRetry,
				LastError:      &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized},
				UpdatedAt:      now,
			},
			"unsupported": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: unsupportedRetry,
				LastError:      &Error{Message: "model not supported", HTTPStatus: http.StatusBadRequest},
				UpdatedAt:      now,
			},
		},
	}}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
	}
	if got := store.mutationCount(); got != 1 {
		t.Fatalf("database mutation count = %d, want 1", got)
	}

	assertState := func(auth *Auth, model string) *ModelState {
		t.Helper()
		state := auth.ModelStates[model]
		if state == nil {
			t.Fatalf("ModelStates[%s] missing in %#v", model, auth.ModelStates)
		}
		return state
	}
	persisted := store.persistedSnapshot()
	transient := assertState(persisted, "transient")
	if transient.Unavailable || !transient.NextRetryAfter.IsZero() || transient.LastError == nil || transient.LastError.HTTPStatus != http.StatusServiceUnavailable || !transient.UpdatedAt.Equal(now) {
		t.Fatalf("persisted transient state = %#v, want dispatchable state with error retained", transient)
	}
	quota := assertState(persisted, "quota")
	if quota.Unavailable || !quota.NextRetryAfter.IsZero() || quota.Quota.Exceeded || quota.QuotaResetAt.IsZero() || !quota.UpdatedAt.Equal(now) {
		t.Fatalf("persisted quota state = %#v, want quota cooldown cleared", quota)
	}
	unauthorized := assertState(persisted, "unauthorized")
	if !unauthorized.Unavailable || !unauthorized.NextRetryAfter.Equal(unauthorizedRetry) {
		t.Fatalf("persisted unauthorized state = %#v, want preserved", unauthorized)
	}
	unsupported := assertState(persisted, "unsupported")
	if !unsupported.Unavailable || !unsupported.NextRetryAfter.Equal(unsupportedRetry) {
		t.Fatalf("persisted unsupported state = %#v, want preserved", unsupported)
	}

	local, ok := manager.GetByID(authID)
	if !ok || local == nil {
		t.Fatalf("GetByID(%s) missing local auth", authID)
	}
	if blocked, _, _ := isAuthBlockedForModel(local, "transient", time.Now()); blocked {
		t.Fatal("transient model remained blocked after clearing disabled cooldown")
	}
	if blocked, reason, _ := isAuthBlockedForModel(local, "quota", time.Now()); blocked || reason != blockReasonNone {
		t.Fatalf("quota model block = %v/%v, want dispatchable", blocked, reason)
	}
	if blocked, _, _ := isAuthBlockedForModel(local, "unauthorized", time.Now()); !blocked {
		t.Fatal("unauthorized model was cleared unexpectedly")
	}
	if blocked, _, _ := isAuthBlockedForModel(local, "unsupported", time.Now()); !blocked {
		t.Fatal("unsupported model was cleared unexpectedly")
	}
	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("second ClearDisabledCooldownStates() error = %v", errClear)
	}
	if got := store.mutationCount(); got != 1 {
		t.Fatalf("database mutation count after clean reload = %d, want 1", got)
	}
}

func TestClearDisabledCooldownStatesPreservesRefreshBlock(t *testing.T) {
	tests := []struct {
		name          string
		modelState    func(time.Time) *ModelState
		wantMutations int
	}{
		{
			name:          "refresh block only",
			wantMutations: 0,
		},
		{
			name: "model request cooldown",
			modelState: func(now time.Time) *ModelState {
				return &ModelState{
					Status:         StatusError,
					StatusMessage:  "upstream unavailable",
					Unavailable:    true,
					NextRetryAfter: now.Add(2 * time.Minute),
					LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
					UpdatedAt:      now,
				}
			},
			wantMutations: 1,
		},
		{
			name: "model quota cooldown",
			modelState: func(now time.Time) *ModelState {
				return quotaCooldownModelState(now, 3*time.Minute)
			},
			wantMutations: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			refreshRetryAt := now.Add(5 * time.Minute)
			authID := "auth-refresh-block-" + strings.ReplaceAll(test.name, " ", "-")
			refreshMessage := `antigravity refresh: upstream request failed with status 400 error="invalid_request" request_id="req-123"`
			persistedAuth := &Auth{
				ID:                    authID,
				Index:                 authID,
				Provider:              "antigravity",
				Status:                StatusError,
				StatusMessage:         refreshMessage,
				Unavailable:           true,
				NextRetryAfter:        refreshRetryAt,
				NextRefreshAfter:      refreshRetryAt,
				RuntimeRefreshBlocked: true,
				LastError: &Error{
					Code:       refreshTransientErrorCode,
					Message:    refreshMessage,
					Retryable:  true,
					HTTPStatus: http.StatusServiceUnavailable,
				},
				UpdatedAt: now,
			}
			if test.modelState != nil {
				persistedAuth.ModelStates = map[string]*ModelState{"gpt-5": test.modelState(now)}
			} else {
				direct := persistedAuth.Clone()
				if changed := clearDisabledCooldownState(direct, now.Add(time.Second)); changed {
					t.Fatalf("clearDisabledCooldownState() = true for refresh-only block: %#v", direct)
				}
				if !RefreshBlocksDispatch(direct) || !direct.NextRetryAfter.Equal(refreshRetryAt) {
					t.Fatalf("direct refresh-only block = %#v, want preserved", direct)
				}
			}

			store := &fakeMutatorStore{persisted: persistedAuth}
			manager := NewManager(store, nil, nil)
			manager.SetConfig(&internalconfig.Config{DisableCooling: true})
			if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
				t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
			}
			if got := store.mutationCount(); got != test.wantMutations {
				t.Fatalf("database mutation count = %d, want %d", got, test.wantMutations)
			}

			local, ok := manager.GetByID(authID)
			if !ok || local == nil {
				t.Fatalf("GetByID(%s) missing local auth", authID)
			}
			for stateName, auth := range map[string]*Auth{
				"persisted": store.persistedSnapshot(),
				"local":     local,
			} {
				if auth.Status != StatusError || auth.StatusMessage != refreshMessage || auth.LastError == nil || auth.LastError.Code != refreshTransientErrorCode || auth.LastError.Message != refreshMessage {
					t.Fatalf("%s refresh diagnostic state = %#v, want preserved", stateName, auth)
				}
				if !auth.Unavailable || !auth.RuntimeRefreshBlocked || !auth.NextRetryAfter.Equal(refreshRetryAt) || !auth.NextRefreshAfter.Equal(refreshRetryAt) || !RefreshBlocksDispatch(auth) {
					t.Fatalf("%s refresh block = %#v, want unavailable until %v", stateName, auth, refreshRetryAt)
				}
				if blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now); !blocked || reason != blockReasonOther || !next.Equal(refreshRetryAt) {
					t.Fatalf("%s model block = %v/%v/%v, want refresh block until %v", stateName, blocked, reason, next, refreshRetryAt)
				}
				if test.modelState != nil {
					modelState := auth.ModelStates["gpt-5"]
					if modelState == nil || modelState.Unavailable || !modelState.NextRetryAfter.IsZero() || modelState.Quota.Exceeded || !modelState.Quota.NextRecoverAt.IsZero() {
						t.Fatalf("%s model cooldown = %#v, want cleared", stateName, modelState)
					}
				}
			}

			if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
				t.Fatalf("second ClearDisabledCooldownStates() error = %v", errClear)
			}
			if got := store.mutationCount(); got != test.wantMutations {
				t.Fatalf("database mutation count after clean reload = %d, want %d", got, test.wantMutations)
			}
		})
	}
}

func TestClearDisabledCooldownStatesClearsLegacyCredentialQuota(t *testing.T) {
	tests := []struct {
		name              string
		globalDisable     bool
		credentialDisable bool
		exceeded          bool
		withModelState    bool
	}{
		{name: "global setting", globalDisable: true, exceeded: true},
		{name: "credential override with clean model", credentialDisable: true, exceeded: true, withModelState: true},
		{name: "recovery timestamp without exceeded flag", globalDisable: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			retryAt := now.Add(10 * time.Minute)
			authID := "auth-clear-legacy-credential-quota-" + strings.ReplaceAll(tc.name, " ", "-")
			metadata := map[string]any{"access_token": "preserved"}
			if tc.credentialDisable {
				metadata["disable_cooling"] = true
			}
			persistedAuth := &Auth{
				ID:             authID,
				Index:          authID,
				Provider:       "codex",
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota: QuotaState{
					Exceeded:      tc.exceeded,
					Scope:         "credential",
					Reason:        "quota",
					NextRecoverAt: retryAt,
					BackoffLevel:  4,
				},
				Metadata: metadata,
			}
			if tc.withModelState {
				persistedAuth.ModelStates = map[string]*ModelState{"gpt-5": {Status: StatusActive, UpdatedAt: now}}
			}
			store := &fakeMutatorStore{persisted: persistedAuth}
			manager := NewManager(store, nil, nil)
			manager.SetConfig(&internalconfig.Config{DisableCooling: tc.globalDisable})
			if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
				t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
			}
			persisted := store.persistedSnapshot()
			if persisted.Unavailable || !persisted.NextRetryAfter.IsZero() || persisted.Quota.Exceeded || !persisted.Quota.NextRecoverAt.IsZero() {
				t.Fatalf("persisted credential quota state = %#v, want dispatchable", persisted)
			}
			if persisted.Status != StatusActive || persisted.StatusMessage != "" || persisted.LastError != nil {
				t.Fatalf("persisted credential status = %#v, want active", persisted)
			}
			if got := persisted.Metadata["access_token"]; got != "preserved" {
				t.Fatalf("access_token = %v, want preserved", got)
			}
			if got := store.mutationCount(); got != 1 {
				t.Fatalf("database mutation count = %d, want 1", got)
			}

			if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
				t.Fatalf("second ClearDisabledCooldownStates() error = %v", errClear)
			}
			if got := store.mutationCount(); got != 1 {
				t.Fatalf("database mutation count after cleared cooldown = %d, want 1", got)
			}
		})
	}
}

func TestClearDisabledCooldownStatePreservesNewerUncoveredModelError(t *testing.T) {
	observedAt := time.Now().UTC()
	clearAt := observedAt.Add(time.Minute)
	auth := &Auth{
		ID:       "auth-clear-request-timestamp",
		Index:    "auth-clear-request-timestamp",
		Provider: "codex",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: observedAt.Add(5 * time.Minute),
				LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
				UpdatedAt:      observedAt,
			},
		},
	}

	if changed := clearDisabledCooldownState(auth, clearAt); !changed {
		t.Fatal("clearDisabledCooldownState() did not clear request cooldown")
	}
	persistedState := auth.ModelStates["gpt-5"]
	if !persistedState.UpdatedAt.Equal(observedAt) {
		t.Fatalf("cleared request UpdatedAt = %v, want original %v", persistedState.UpdatedAt, observedAt)
	}

	newerUnsupported := &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: clearAt.Add(12 * time.Hour),
		LastError:      &Error{Message: "model not supported", HTTPStatus: http.StatusBadRequest},
		UpdatedAt:      observedAt.Add(30 * time.Second),
	}
	merged := MergePersistedModelState(persistedState, newerUnsupported)
	if merged == nil || merged.LastError == nil || merged.LastError.HTTPStatus != http.StatusBadRequest || !merged.NextRetryAfter.Equal(newerUnsupported.NextRetryAfter) {
		t.Fatalf("merged state = %#v, want newer uncovered model error preserved", merged)
	}
}

func TestDisabledQuotaResultResumesRegistryModel(t *testing.T) {
	const authID = "auth-disabled-quota-registry"
	const model = "disabled-quota-registry-model"

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	modelRegistry.SuspendClientModel(authID, model, "not_found")
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("test setup did not suspend model")
	}

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Error:    &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
	})

	got, ok := manager.GetByID(authID)
	if !ok || got == nil || got.ModelStates[model] == nil {
		t.Fatalf("GetByID() missing model state: %#v", got)
	}
	state := got.ModelStates[model]
	if state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("disabled quota state = %#v, want dispatchable", state)
	}
	if !registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("registry remained suspended after disabled 429 result")
	}
}

func TestClearDisabledCooldownStatesPreservesDisabledAuthState(t *testing.T) {
	now := time.Now().UTC()
	quotaState := quotaCooldownModelState(now, 10*time.Minute)
	transientState := &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(5 * time.Minute),
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      now,
	}
	store := &fakeMutatorStore{persisted: &Auth{
		ID:             "auth-clear-disabled-auth",
		Index:          "auth-clear-disabled-auth",
		Provider:       "codex",
		Disabled:       true,
		Status:         StatusDisabled,
		StatusMessage:  "disabled via management API",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Hour),
		Quota:          quotaState.Quota,
		ModelStates: map[string]*ModelState{
			"quota":     quotaState,
			"transient": transientState,
		},
	}}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
	}
	persisted := store.persistedSnapshot()
	if !persisted.Disabled || persisted.Status != StatusDisabled || persisted.StatusMessage != "disabled via management API" || !persisted.Unavailable || !persisted.NextRetryAfter.After(now) {
		t.Fatalf("disabled auth state = %#v, want preserved", persisted)
	}
	if persisted.ModelStates["quota"].Quota.Exceeded || !persisted.ModelStates["quota"].NextRetryAfter.IsZero() {
		t.Fatalf("disabled quota model state = %#v, want quota cooldown cleared", persisted.ModelStates["quota"])
	}
	if persisted.ModelStates["transient"].LastError == nil || persisted.ModelStates["transient"].LastError.HTTPStatus != http.StatusServiceUnavailable || !persisted.ModelStates["transient"].NextRetryAfter.IsZero() {
		t.Fatalf("disabled transient model state = %#v, want retry cleared with error retained", persisted.ModelStates["transient"])
	}
	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("second ClearDisabledCooldownStates() error = %v", errClear)
	}
	if got := store.mutationCount(); got != 1 {
		t.Fatalf("database mutation count after clean reload = %d, want 1", got)
	}
}

func TestClearDisabledCooldownStatesAdoptsAlreadyClearedPersistedState(t *testing.T) {
	now := time.Now().UTC()
	localState := &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(5 * time.Minute),
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      now,
	}
	const authID = "auth-clear-disabled-authoritative"
	store := &fakeMutatorStore{persisted: &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		ModelStates: map[string]*ModelState{
			"gpt-5": {Status: StatusActive},
		},
	}}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{"gpt-5": localState},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
	}
	local, ok := manager.GetByID(authID)
	if !ok || local == nil || !local.Disabled || local.Status != StatusDisabled {
		t.Fatalf("local auth = %#v/%v, want persisted disabled state", local, ok)
	}
	state := local.ModelStates["gpt-5"]
	if state == nil || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("local model state = %#v, want stale cooldown cleared", state)
	}
}

func TestClearDisabledCooldownStatesCanRetryAfterMutationFailure(t *testing.T) {
	const authID = "auth-clear-disabled-retry"
	now := time.Now().UTC()
	store := &fakeMutatorStore{
		persisted: &Auth{
			ID:       authID,
			Index:    authID,
			Provider: "codex",
			Status:   StatusError,
			ModelStates: map[string]*ModelState{
				"gpt-5": {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: now.Add(time.Minute),
					LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
				},
			},
		},
		mutateErr: errors.New("database temporarily unavailable"),
	}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear == nil {
		t.Fatal("ClearDisabledCooldownStates() succeeded during mutation failure")
	}
	if state := store.persistedSnapshot().ModelStates["gpt-5"]; state == nil || state.NextRetryAfter.IsZero() {
		t.Fatalf("persisted state after failed clear = %#v, want cooldown retained", state)
	}
	local, ok := manager.GetByID(authID)
	if !ok || local == nil || local.ModelStates["gpt-5"] == nil || !local.ModelStates["gpt-5"].NextRetryAfter.IsZero() || local.ModelStates["gpt-5"].Unavailable {
		t.Fatalf("local state after failed persistence = %#v, want cooldown cleared", local)
	}

	store.mu.Lock()
	store.mutateErr = nil
	store.mu.Unlock()
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), store.persistedSnapshot()); errUpdate != nil {
		t.Fatalf("Update() reload error = %v", errUpdate)
	}
	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() retry error = %v", errClear)
	}
	if state := store.persistedSnapshot().ModelStates["gpt-5"]; state == nil || !state.NextRetryAfter.IsZero() || state.Unavailable {
		t.Fatalf("persisted state after retry = %#v, want cleared", state)
	}
}

func TestClearDisabledCooldownStatesSkipsCleanAuthMutation(t *testing.T) {
	const authID = "auth-clear-disabled-clean"
	store := &fakeMutatorStore{
		persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive},
		mutateErr: errors.New("mutation should not be called"),
	}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() clean auth error = %v", errClear)
	}
}

func TestFenceDisabledCooldownStatesMutatesCleanQueuedAuth(t *testing.T) {
	const authID = "auth-fence-disabled-clean"
	store := &fakeMutatorStore{
		persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive},
	}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if errFence := manager.FenceDisabledCooldownStates(context.Background()); errFence != nil {
		t.Fatalf("FenceDisabledCooldownStates() clean error = %v", errFence)
	}
	if got := store.mutationCount(); got != 0 {
		t.Fatalf("clean auth database mutations = %d, want zero without a queued snapshot", got)
	}
	manager.resultPersistMu.Lock()
	manager.resultPersistActive[authID] = struct{}{}
	manager.resultPersistMu.Unlock()

	if errFence := manager.FenceDisabledCooldownStates(context.Background()); errFence != nil {
		t.Fatalf("FenceDisabledCooldownStates() error = %v", errFence)
	}
	if got := store.mutationCount(); got != 1 {
		t.Fatalf("database mutation count = %d, want one revision fence", got)
	}
}

func TestBuildDispatchCandidateAppliesCurrentDisableCoolingPolicy(t *testing.T) {
	now := time.Now().UTC()
	const model = "gpt-5"
	disableCooling := true
	enableCooling := false
	tests := []struct {
		name               string
		globalDisable      bool
		credentialOverride *bool
		wantAvailable      bool
	}{
		{name: "cooling enabled", wantAvailable: false},
		{name: "global disable", globalDisable: true, wantAvailable: true},
		{name: "credential disable", credentialOverride: &disableCooling, wantAvailable: true},
		{name: "credential enable overrides global", globalDisable: true, credentialOverride: &enableCooling, wantAvailable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authID := "auth-dispatch-policy-" + strings.ReplaceAll(tc.name, " ", "-")
			auth := &Auth{
				ID:                    authID,
				Index:                 authID,
				Provider:              "codex",
				Status:                StatusError,
				RuntimeDisableCooling: tc.credentialOverride,
				ModelStates: map[string]*ModelState{
					model: {
						Status:         StatusError,
						Unavailable:    true,
						NextRetryAfter: now.Add(time.Minute),
						LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
					},
				},
			}
			manager := NewManager(nil, nil, nil)
			registerDispatchTestAuth(t, manager, auth, model)
			manager.SetConfig(&internalconfig.Config{DisableCooling: tc.globalDisable})

			_, available, _, _ := manager.buildDispatchCandidate(auth, auth.Provider, model, "", now)
			if available != tc.wantAvailable {
				t.Fatalf("buildDispatchCandidate() available = %t, want %t", available, tc.wantAvailable)
			}
			state := auth.ModelStates[model]
			if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() {
				t.Fatalf("source auth was mutated while deriving availability: %#v", state)
			}
			decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, model, Options{})
			if tc.wantAvailable {
				if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != authID {
					t.Fatalf("Dispatch() = %#v, %v; want auth %s", decision, errDispatch, authID)
				}
			} else if errDispatch == nil {
				t.Fatalf("Dispatch() = %#v, nil; want cooldown error", decision)
			}
		})
	}
}

func TestDispatchSessionAffinityUsesEffectiveAvailabilityAfterHotSwitch(t *testing.T) {
	const authID = "auth-session-affinity-hot-switch"
	const model = "session-affinity-hot-switch-model"
	now := time.Now().UTC()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Minute,
	})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	registerDispatchTestAuth(t, manager, &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}, model)
	opts := Options{Headers: http.Header{"X-Session-ID": []string{"hot-switch-session"}}}

	if decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, model, opts); errDispatch == nil {
		t.Fatalf("Dispatch() before hot switch = %#v, nil; want cooldown error", decision)
	}
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, model, opts)
	if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != authID {
		t.Fatalf("Dispatch() after hot switch = %#v, %v; want auth %s", decision, errDispatch, authID)
	}
	source, ok := manager.GetByID(authID)
	if !ok || source.ModelStates[model] == nil || !source.ModelStates[model].Unavailable || source.ModelStates[model].NextRetryAfter.IsZero() {
		t.Fatalf("source auth after effective selection = %#v, want persisted cooldown unchanged", source)
	}
}

func TestReconcileRegistryModelStatesUsesEffectiveAvailabilityAfterHotSwitch(t *testing.T) {
	const authID = "auth-registry-effective-hot-switch"
	const model = "registry-effective-hot-switch-model"
	now := time.Now().UTC()
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	modelRegistry.SuspendClientModel(authID, model, "upstream unavailable")
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("test setup did not suspend model")
	}

	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	manager.ReconcileRegistryModelStates(context.Background(), authID)

	if !registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("registry remained suspended after disable-cooling hot switch")
	}
	source, ok := manager.GetByID(authID)
	if !ok || source.ModelStates[model] == nil || !source.ModelStates[model].Unavailable || source.ModelStates[model].NextRetryAfter.IsZero() {
		t.Fatalf("source auth after registry reconciliation = %#v, want persisted cooldown unchanged", source)
	}
}

func TestCoveredResultReconcilesLateRegistryTransitionAfterHotSwitch(t *testing.T) {
	const authID = "auth-registry-late-transition"
	const model = "registry-late-transition-model"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	// Simulate an older result transition reaching the registry after config
	// cleanup has already left the authoritative manager state dispatchable.
	modelRegistry.SuspendClientModel(authID, model, "payment_required")
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("test setup did not apply the late registry transition")
	}

	manager.reconcileCoveredCooldownAfterResult(context.Background(), authID)
	if !registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("late registry transition remained after current-state reconciliation")
	}
}

func TestClearDisabledCooldownStatesClearsStackedQuotaAndPreservesUnauthorized(t *testing.T) {
	const authID = "auth-clear-disabled-stacked-quota"
	const model = "gpt-5"
	now := time.Now().UTC()
	retryAt := now.Add(time.Minute)
	unauthorized := &Error{Message: "expired access token", HTTPStatus: http.StatusUnauthorized}
	quota := QuotaState{
		Exceeded:      true,
		Scope:         quotaScopeModel,
		Reason:        "quota",
		NextRecoverAt: now.Add(10 * time.Minute),
	}
	store := &fakeMutatorStore{persisted: &Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: retryAt,
		LastError:      cloneError(unauthorized),
		Quota:          quota,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      cloneError(unauthorized),
				Quota:          quota,
				UpdatedAt:      now,
			},
		},
	}}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
	}
	if got := store.mutationCount(); got != 1 {
		t.Fatalf("database mutation count = %d, want 1", got)
	}

	persisted := store.persistedSnapshot()
	state := persisted.ModelStates[model]
	if state == nil || state.Quota.Exceeded || state.QuotaResetAt.IsZero() {
		t.Fatalf("persisted model state = %#v, want quota cleared", state)
	}
	if !state.Unavailable || !state.NextRetryAfter.Equal(retryAt) || state.LastError == nil || state.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("persisted model state = %#v, want unauthorized cooldown preserved", state)
	}
	if persisted.Quota.Exceeded {
		t.Fatalf("persisted auth quota = %#v, want cleared", persisted.Quota)
	}
	if !persisted.Unavailable || !persisted.NextRetryAfter.Equal(retryAt) || persisted.LastError == nil || persisted.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("persisted auth state = %#v, want unauthorized cooldown preserved", persisted)
	}
}

func TestClearDisabledCooldownStatesClearsStackedQuotaAndRequestError(t *testing.T) {
	const authID = "auth-clear-disabled-stacked-request-error"
	const model = "gpt-5"
	now := time.Now().UTC()
	requestRetryAt := now.Add(time.Minute)
	quotaRetryAt := now.Add(10 * time.Minute)
	quota := QuotaState{
		Exceeded:      true,
		Scope:         quotaScopeModel,
		Reason:        "quota",
		NextRecoverAt: quotaRetryAt,
		BackoffLevel:  2,
	}
	requestError := &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable}
	store := &fakeMutatorStore{persisted: &Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: requestRetryAt,
		LastError:      cloneError(requestError),
		Quota:          quota,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: requestRetryAt,
				LastError:      cloneError(requestError),
				Quota:          quota,
				UpdatedAt:      now,
			},
		},
	}}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: true})
	if _, errRegister := manager.Register(context.Background(), store.persistedSnapshot()); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("ClearDisabledCooldownStates() error = %v", errClear)
	}
	persisted := store.persistedSnapshot()
	state := persisted.ModelStates[model]
	if state == nil || state.Quota.Exceeded || state.Unavailable || !state.NextRetryAfter.IsZero() || state.QuotaResetAt.IsZero() || state.LastError == nil || state.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("persisted model state = %#v, want quota and request cooldown cleared", state)
	}
	if persisted.Quota.Exceeded || persisted.Unavailable || !persisted.NextRetryAfter.IsZero() {
		t.Fatalf("persisted auth state = %#v, want dispatchable aggregate", persisted)
	}
	if errClear := manager.ClearDisabledCooldownStates(context.Background()); errClear != nil {
		t.Fatalf("second ClearDisabledCooldownStates() error = %v", errClear)
	}
	if got := store.mutationCount(); got != 1 {
		t.Fatalf("database mutation count after cleared cooldown = %d, want 1", got)
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
	// Cross one coarse Windows clock tick so the ordering assertion is stable.
	time.Sleep(20 * time.Millisecond)
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

func TestAdoptPersistedCooldownStateClearsStaleRuntimeRefreshBlock(t *testing.T) {
	now := time.Now().UTC()
	const authID = "auth-stale-runtime-refresh-block"
	quotaState := quotaCooldownModelState(now, 10*time.Minute)
	quotaState.Quota.Scope = quotaScopeModel
	persisted := &Auth{
		ID:             authID,
		Index:          authID,
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  quotaState.StatusMessage,
		LastError:      cloneError(quotaState.LastError),
		Unavailable:    true,
		NextRetryAfter: quotaState.NextRetryAfter,
		Quota:          quotaState.Quota,
		ModelStates:    map[string]*ModelState{"model-a": quotaState},
	}
	store := &fakeMutatorStore{persisted: persisted}
	manager := NewManager(store, nil, nil)

	local := persisted.Clone()
	local.StatusMessage = "refresh temporarily unavailable"
	local.LastError = &Error{
		Code:       refreshTransientErrorCode,
		Message:    "refresh temporarily unavailable",
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
	local.NextRetryAfter = now.Add(time.Minute)
	local.RuntimeRefreshBlocked = true
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), local); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	result, errClear := manager.ClearQuotaCooldown(context.Background(), authID, "missing-model")
	if errClear != nil {
		t.Fatalf("ClearQuotaCooldown() error = %v", errClear)
	}
	if result.Cleared {
		t.Fatalf("ClearQuotaCooldown() result = %#v, want no-op mutation with authoritative state adoption", result)
	}
	if got := store.mutationCount(); got != 0 {
		t.Fatalf("database mutation count = %d, want zero", got)
	}

	got, ok := manager.GetByID(authID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) = %#v/%v", authID, got, ok)
	}
	if got.RuntimeRefreshBlocked || RefreshBlocksDispatch(got) {
		t.Fatalf("adopted refresh block = runtime:%v effective:%v, want cleared", got.RuntimeRefreshBlocked, RefreshBlocksDispatch(got))
	}
	if blocked, reason, next := isAuthBlockedForModel(got, "model-a", now); !blocked || reason != blockReasonCooldown || !next.Equal(quotaState.NextRetryAfter) {
		t.Fatalf("model-a blocked/reason/next = %v/%v/%v, want quota cooldown until %v", blocked, reason, next, quotaState.NextRetryAfter)
	}
	if blocked, reason, next := isAuthBlockedForModel(got, "model-b", now); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("model-b blocked/reason/next = %v/%v/%v, want available", blocked, reason, next)
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

func TestReconcileRegistryModelStatesIgnoresDisabledUnregisteredCredential(t *testing.T) {
	const disabledAuthID = "auth-registry-disabled"
	const activePeerID = "auth-registry-disabled-peer"
	modelIDs := []string{
		"registry-disabled-model-1",
		"registry-disabled-model-2",
		"registry-disabled-model-3",
		"registry-disabled-model-4",
		"registry-disabled-model-5",
		"registry-disabled-model-6",
		"registry-disabled-model-7",
		"registry-disabled-model-8",
		"registry-disabled-model-9",
	}
	models := make([]*registry.ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		models = append(models, &registry.ModelInfo{ID: modelID, Object: "model", Type: "gemini"})
	}

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(activePeerID, "gemini", models)
	modelRegistry.RegisterClient(disabledAuthID, "gemini", models)
	modelRegistry.UnregisterClient(disabledAuthID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(disabledAuthID)
		modelRegistry.UnregisterClient(activePeerID)
	})

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       disabledAuthID,
		Index:    disabledAuthID,
		Provider: "gemini",
		Status:   StatusDisabled,
		Disabled: true,
		ModelStates: map[string]*ModelState{
			modelIDs[0]: {Status: StatusActive},
			modelIDs[1]: {Status: StatusActive},
		},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.ReconcileRegistryModelStates(context.Background(), disabledAuthID)
	manager.ReconcileRegistryModelStates(context.Background(), disabledAuthID)

	for _, modelID := range modelIDs {
		if !registryHasAvailableModel(modelRegistry, modelID) {
			t.Fatalf("registry hid active peer model %q because a disabled credential retained sparse model state", modelID)
		}
	}
}

func TestClearQuotaCooldownRequiresAtomicStore(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	_, errClear := manager.ClearQuotaCooldown(context.Background(), "auth-no-store", "")
	if !errors.Is(errClear, ErrCooldownMutationUnsupported) {
		t.Fatalf("ClearQuotaCooldown() error = %v, want ErrCooldownMutationUnsupported", errClear)
	}
}
