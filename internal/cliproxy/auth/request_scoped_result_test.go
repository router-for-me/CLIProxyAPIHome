package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestMarkResultRequestScopedDoesNotPersistOrAdoptSharedState(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const authID = "request-scoped-cluster-auth"
			const model = "gpt-6-sol"
			local := &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive, StateVersion: 1}
			persisted := local.Clone()
			persisted.StateVersion = 2
			persisted.Status = StatusError
			persisted.Unavailable = true
			persisted.NextRetryAfter = time.Now().Add(time.Hour)
			persisted.ModelStates = map[string]*ModelState{
				model: {Status: StatusError, Unavailable: true, NextRetryAfter: persisted.NextRetryAfter},
			}
			store := &fakeMutatorStore{persisted: persisted.Clone()}
			manager := NewManager(store, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), local); errRegister != nil {
				t.Fatal(errRegister)
			}
			before, _ := manager.GetByID(authID)
			// Keep queued writes observable without racing a background worker.
			manager.resultPersistActive = map[string]struct{}{authID: {}}
			manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: "codex", Model: model, Error: &Error{Code: ErrorCodeRequestScoped, HTTPStatus: status, Message: "request-scoped failure"}})
			after, _ := manager.GetByID(authID)
			if !reflect.DeepEqual(availabilityFingerprint(before, model), availabilityFingerprint(after, model)) || before.StateVersion != after.StateVersion {
				t.Errorf("request fault adopted shared availability: before=%#v after=%#v", before, after)
			}
			if after.Failed != before.Failed+1 {
				t.Error("request failure was not counted")
			}
			if store.mutationAttemptCount() != 0 || store.saveCount() != 0 || len(manager.resultPersistPending) != 0 {
				t.Errorf("request fault reached persistence: mutate=%d save=%d queued=%d", store.mutationAttemptCount(), store.saveCount(), len(manager.resultPersistPending))
			}
			if !reflect.DeepEqual(store.persistedSnapshot(), persisted) {
				t.Error("request fault changed shared credential state")
			}
		})
	}
}

func TestMarkResultForceCooldownPersistsFromMinimalProjectionAndSuspendsRegistry(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			authID := "force-cooldown-projection-" + http.StatusText(status)
			model := "force-cooldown-model-" + http.StatusText(status)

			modelRegistry := registry.GetGlobalRegistry()
			modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			disableCooling := true
			persisted := &Auth{
				ID:                    authID,
				Index:                 authID,
				Provider:              "codex",
				Status:                StatusActive,
				StateVersion:          1,
				RuntimeDisableCooling: &disableCooling,
				Metadata: map[string]any{
					"refresh_token": "secret-refresh-token",
				},
			}
			store := &fakeMutatorStore{persisted: persisted.Clone()}

			// Register a minimal runtime projection (RuntimeProjection: true) on node A.
			nodeA := NewManager(store, nil, nil)
			projection := &Auth{
				ID:                    authID,
				Index:                 authID,
				Provider:              "codex",
				Status:                StatusActive,
				StateVersion:          1,
				RuntimeProjection:     true,
				RuntimeDisableCooling: &disableCooling,
			}
			if _, errRegister := nodeA.Register(WithSkipPersist(context.Background()), projection); errRegister != nil {
				t.Fatal(errRegister)
			}

			observedAt := time.Now().UTC().Add(-time.Second)
			if errMark := nodeA.MarkResult(context.Background(), Result{
				AuthID:     authID,
				Provider:   "codex",
				Model:      model,
				ObservedAt: observedAt,
				Error: &Error{
					Code:       ErrorCodeForceCooldown,
					HTTPStatus: status,
					Message:    "forced cooldown error",
				},
			}); errMark != nil {
				t.Fatalf("MarkResult() error = %v", errMark)
			}

			if store.mutationCount() != 1 {
				t.Fatalf("expected StateMutator to persist non-429 force_cooldown from minimal projection, mutations=%d", store.mutationCount())
			}
			saved := store.persistedSnapshot()
			if saved.Metadata["refresh_token"] != "secret-refresh-token" {
				t.Fatalf("persisted token metadata was lost: %#v", saved.Metadata)
			}
			savedState := saved.ModelStates[model]
			if savedState == nil || !savedState.Unavailable || !savedState.NextRetryAfter.After(time.Now()) || !savedState.LastObservedAt.Equal(observedAt) {
				t.Fatalf("persisted model state missing forced cooldown or observation fence: %#v", savedState)
			}
			if registryHasAvailableModel(modelRegistry, model) {
				t.Fatalf("expected registry model %q to be suspended for status %d force_cooldown", model, status)
			}

			// Subsequent success clears the forced cooldown and resumes the registry model.
			if errSuccess := nodeA.MarkResult(context.Background(), Result{
				AuthID:     authID,
				Provider:   "codex",
				Model:      model,
				Success:    true,
				ObservedAt: observedAt.Add(time.Second),
			}); errSuccess != nil {
				t.Fatalf("MarkResult(success) error = %v", errSuccess)
			}
			if !registryHasAvailableModel(modelRegistry, model) {
				t.Fatalf("expected registry model %q to resume after success", model)
			}
		})
	}
}

func TestHasDisabledCooldownStateIgnoresAggregatedForceQuotaAfterSiblingUpdate(t *testing.T) {
	disableCooling := true
	store := &fakeMutatorStore{
		persisted: &Auth{
			ID:                    "force-quota-sibling-auth",
			Index:                 "force-quota-sibling-auth",
			Provider:              "codex",
			Status:                StatusActive,
			StateVersion:          1,
			RuntimeDisableCooling: &disableCooling,
		},
	}
	manager := NewManager(store, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), store.persistedSnapshot()); errRegister != nil {
		t.Fatal(errRegister)
	}

	// Step 1: Model A receives a forced 429 cooldown.
	if errMark := manager.MarkResult(context.Background(), Result{
		AuthID:   "force-quota-sibling-auth",
		Provider: "codex",
		Model:    "model-a",
		Error:    &Error{Code: ErrorCodeForceCooldown, HTTPStatus: http.StatusTooManyRequests, Message: "forced 429"},
	}); errMark != nil {
		t.Fatal(errMark)
	}
	mutationsAfterForced429 := store.mutationCount()

	// Step 2: Sibling Model B succeeds, updating auth.LastError while auth.Quota remains aggregated from Model A.
	if errMark := manager.MarkResult(context.Background(), Result{
		AuthID:   "force-quota-sibling-auth",
		Provider: "codex",
		Model:    "model-b",
		Success:  true,
	}); errMark != nil {
		t.Fatal(errMark)
	}
	afterB, _ := manager.GetByID("force-quota-sibling-auth")
	if hasDisabledCooldownState(afterB) {
		t.Fatalf("hasDisabledCooldownState returned true when only forced 429 quota remained: %#v", afterB)
	}
	if afterB.ModelStates["model-a"] == nil || !afterB.ModelStates["model-a"].Unavailable || !afterB.ModelStates["model-a"].Quota.Exceeded {
		t.Fatalf("model-a forced 429 cooldown was lost after sibling success: %#v", afterB.ModelStates["model-a"])
	}
	_ = mutationsAfterForced429
}
