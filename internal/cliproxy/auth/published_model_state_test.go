package auth

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestPublishedSourceStateCompatibility(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	legacy := &ModelState{
		Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Hour)},
		LastObservedAt: now, UpdatedAt: now,
	}
	for _, provider := range []string{"copilot", "devin"} {
		for _, tc := range []struct {
			name    string
			current *ModelState
			blocked bool
		}{
			{name: "legacy source", blocked: true},
			{name: "older canonical success", current: &ModelState{Status: StatusActive, LastObservedAt: now.Add(-time.Minute)}, blocked: true},
			{name: "newer canonical success", current: &ModelState{Status: StatusActive, LastObservedAt: now.Add(time.Minute)}},
			{name: "observation before receipt time", current: &ModelState{Status: StatusActive, LastObservedAt: now.Add(time.Minute), UpdatedAt: now.Add(-time.Minute)}},
			{name: "untimed newer canonical success", current: &ModelState{Status: StatusActive, UpdatedAt: now.Add(time.Minute)}},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				source := provider + "/model-a"
				auth := &Auth{Provider: provider, ModelStates: map[string]*ModelState{"model-a": legacy.Clone(), source: tc.current}}
				_, blocked, _, _ := dispatchModelBlockStatus(auth, source, "team/friendly(high)", now)
				if blocked != tc.blocked {
					t.Fatalf("blocked = %t, want %t", blocked, tc.blocked)
				}
				// A public alias that happens to equal an old source is not that source.
				if _, reboundBlocked, _, _ := dispatchModelBlockStatus(auth, provider+"/model-b", "model-a", now); reboundBlocked {
					t.Fatal("rebound alias inherited another source's cooldown")
				}
			})
		}
	}

	// Non-published providers do not treat bare keys as stripped provider-prefixed keys.
	otherAuth := &Auth{Provider: "claude", ModelStates: map[string]*ModelState{"model-a": legacy.Clone()}}
	if _, blocked, _, _ := dispatchModelBlockStatus(otherAuth, "claude/model-a", "claude/model-a", now); blocked {
		t.Fatal("non-published provider unexpectedly matched stripped provider prefix")
	}
}

func TestPublishedResultConsolidatesLegacySourceState(t *testing.T) {
	const native = "model-a"
	for _, provider := range []string{"copilot", "devin"} {
		source := provider + "/" + native
		for _, shared := range []bool{false, true} {
			for _, initialFeedbackModel := range []string{native, source} {
				t.Run(fmt.Sprintf("%s/shared=%t/model=%s", provider, shared, initialFeedbackModel), func(t *testing.T) {
					now := time.Now().UTC()
					observed := now.Add(-time.Minute)
					auth := &Auth{
						ID: t.Name(), Index: t.Name(), Provider: provider, Status: StatusActive,
						ModelStates: map[string]*ModelState{native: {
							Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
							Quota:          QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Hour), BackoffLevel: 2},
							LastObservedAt: observed, UpdatedAt: observed,
						}},
					}
					var store Store
					if shared {
						store = &fakeMutatorStore{persisted: auth.Clone()}
					}
					manager := NewManager(store, nil, nil)
					if _, errRegister := manager.Register(WithSkipPersist(t.Context()), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "friendly", SourceModelID: source, MetadataModelID: native}})
					t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

					checkState := func(blocked bool) {
						t.Helper()
						current, _ := manager.GetByID(auth.ID)
						snapshots := []*Auth{current}
						if shared {
							snapshots = append(snapshots, store.(*fakeMutatorStore).persistedSnapshot())
						}
						for _, snapshot := range snapshots {
							if _, exists := snapshot.ModelStates[native]; exists {
								t.Fatal("legacy source state was not consolidated")
							}
							state := snapshot.ModelStates[source]
							if state == nil || state.Unavailable != blocked || state.Quota.Exceeded != blocked {
								t.Fatalf("source state = %+v, want blocked=%t", state, blocked)
							}
						}
					}

					success := Result{AuthID: auth.ID, Model: initialFeedbackModel, Success: true, ObservedAt: observed.Add(time.Second)}
					if errResult := manager.MarkResult(t.Context(), success); errResult != nil {
						t.Fatal(errResult)
					}
					checkState(false)

					// Legacy unqualified feedback and canonical feedback share the observation fence.
					retry := time.Hour
					failure := Result{AuthID: auth.ID, Model: native + "(high)", ObservedAt: observed.Add(2 * time.Second), RetryAfter: &retry,
						Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}}
					if errResult := manager.MarkResult(t.Context(), failure); errResult != nil {
						t.Fatal(errResult)
					}
					checkState(true)
					success.Model = source
					if errResult := manager.MarkResult(t.Context(), success); errResult != nil {
						t.Fatal(errResult)
					}
					checkState(true)
					success.Model = native
					success.ObservedAt = observed.Add(3 * time.Second)
					if errResult := manager.MarkResult(t.Context(), success); errResult != nil {
						t.Fatal(errResult)
					}
					checkState(false)
				})
			}
		}
	}
}

func TestPublishedResultMigratesAuthoritativeLegacyState(t *testing.T) {
	const native = "model-a"
	for _, provider := range []string{"copilot", "devin"} {
		t.Run(provider, func(t *testing.T) {
			source := provider + "/" + native
			now := time.Now().UTC()
			observed := now.Add(-time.Minute)
			deadline := now.Add(time.Hour)
			auth := &Auth{ID: t.Name(), Provider: provider, Status: StatusActive}
			persisted := auth.Clone()
			persisted.ModelStates = map[string]*ModelState{native: {
				Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
				Quota:          QuotaState{Exceeded: true, NextRecoverAt: deadline, BackoffLevel: 3},
				LastObservedAt: observed, UpdatedAt: observed,
			}}
			store := &fakeMutatorStore{persisted: persisted}
			manager := NewManager(store, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(t.Context()), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			// The local projection has not seen the legacy state, and this result is stale.
			if errResult := manager.MarkResult(t.Context(), Result{AuthID: auth.ID, Model: source, ObservedAt: observed.Add(-time.Second),
				Error: &Error{HTTPStatus: http.StatusTooManyRequests}}); errResult != nil {
				t.Fatal(errResult)
			}
			current, _ := manager.GetByID(auth.ID)
			for _, snapshot := range []*Auth{current, store.persistedSnapshot()} {
				state := snapshot.ModelStates[source]
				if snapshot.ModelStates[native] != nil || state == nil || !state.NextRetryAfter.Equal(deadline) ||
					state.Quota.BackoffLevel != 3 || !state.LastObservedAt.Equal(observed) {
					t.Fatalf("migration changed the authoritative cooldown or fence: %+v", snapshot.ModelStates)
				}
			}
		})
	}
}

func TestPublishedClearQuotaCooldownIncludesLegacySource(t *testing.T) {
	const native = "model-a"
	for _, provider := range []string{"copilot", "devin"} {
		t.Run(provider, func(t *testing.T) {
			source := provider + "/" + native
			deadline := time.Now().Add(time.Hour)
			auth := &Auth{ID: t.Name(), Provider: provider, Status: StatusActive, ModelStates: map[string]*ModelState{native: {
				Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
				Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline},
			}}}
			store := &fakeMutatorStore{persisted: auth.Clone()}
			manager := NewManager(store, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(t.Context()), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: "friendly", SourceModelID: source, MetadataModelID: native}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			cleared, errClear := manager.ClearQuotaCooldown(t.Context(), auth.ID, "friendly")
			if errClear != nil || !cleared.Cleared {
				t.Fatalf("ClearQuotaCooldown = (%+v, %v)", cleared, errClear)
			}
			current, _ := manager.GetByID(auth.ID)
			for _, snapshot := range []*Auth{current, store.persistedSnapshot()} {
				if _, blocked, _, _ := dispatchModelBlockStatus(snapshot, source, "friendly", time.Now()); blocked {
					t.Fatal("manual reset left the legacy source blocked")
				}
			}
		})
	}
}

func TestPublishedClearQuotaCooldownPreservesReboundAliasTarget(t *testing.T) {
	for _, provider := range []string{"copilot", "devin"} {
		t.Run(provider, func(t *testing.T) {
			sourceA := provider + "/model-a"
			sourceB := provider + "/model-b"
			deadline := time.Now().Add(time.Hour)
			cooldownState := func() *ModelState {
				return &ModelState{
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: deadline,
					Quota:          QuotaState{Exceeded: true, NextRecoverAt: deadline},
				}
			}
			auth := &Auth{
				ID:       t.Name(),
				Provider: provider,
				Status:   StatusActive,
				ModelStates: map[string]*ModelState{
					sourceA: cooldownState(),
					sourceB: cooldownState(),
				},
			}
			store := &fakeMutatorStore{persisted: auth.Clone()}
			manager := NewManager(store, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(t.Context()), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{
				{ID: "friendly", SourceModelID: sourceA, MetadataModelID: "model-a"},
				{ID: sourceA, SourceModelID: sourceB, MetadataModelID: "model-b"},
			})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

			cleared, errClear := manager.ClearQuotaCooldown(t.Context(), auth.ID, sourceA)
			if errClear != nil || !cleared.Cleared || cleared.Model != sourceB || !reflect.DeepEqual(cleared.ClearedModels, []string{sourceB}) {
				t.Fatalf("ClearQuotaCooldown(%q) = (%+v, %v), want only %q cleared", sourceA, cleared, errClear, sourceB)
			}
			current, _ := manager.GetByID(auth.ID)
			for _, snapshot := range []*Auth{current, store.persistedSnapshot()} {
				if _, blocked, _, _ := dispatchModelBlockStatus(snapshot, sourceB, sourceA, time.Now()); blocked {
					t.Fatalf("target source %q remained blocked after reset", sourceB)
				}
				if _, blocked, _, _ := dispatchModelBlockStatus(snapshot, sourceA, "friendly", time.Now()); !blocked {
					t.Fatalf("unrelated source %q was cleared by resetting rebound route %q", sourceA, sourceA)
				}
			}

			// Resetting again when sourceB is no longer cooling must be a no-op and keep sourceA blocked.
			again, errAgain := manager.ClearQuotaCooldown(t.Context(), auth.ID, sourceA)
			if errAgain != nil || again.Cleared || len(again.ClearedModels) != 0 {
				t.Fatalf("second ClearQuotaCooldown(%q) = (%+v, %v), want no-op", sourceA, again, errAgain)
			}
			current, _ = manager.GetByID(auth.ID)
			for _, snapshot := range []*Auth{current, store.persistedSnapshot()} {
				if _, blocked, _, _ := dispatchModelBlockStatus(snapshot, sourceA, "friendly", time.Now()); !blocked {
					t.Fatalf("unrelated source %q was cleared by no-op reset of %q", sourceA, sourceA)
				}
			}
		})
	}
}
