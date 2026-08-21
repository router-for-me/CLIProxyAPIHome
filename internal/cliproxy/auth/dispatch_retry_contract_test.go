package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func dispatchRetryCooldownAuth(id string, retryLimit int) *Auth {
	nextRetry := time.Now().Add(time.Minute)
	return &Auth{
		ID:         id,
		Provider:   "codex",
		Status:     StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"request_retry": retryLimit},
		ModelStates: map[string]*ModelState{
			"gpt": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: nextRetry,
				Quota:          QuotaState{Exceeded: true, NextRecoverAt: nextRetry},
			},
		},
	}
}

func dispatchRetryTransientCooldownAuth(id string, retryLimit int, status int) *Auth {
	nextRetry := time.Now().Add(time.Minute)
	return &Auth{
		ID:         id,
		Provider:   "codex",
		Status:     StatusError,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"request_retry": retryLimit},
		ModelStates: map[string]*ModelState{
			"gpt": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: nextRetry,
				LastError:      &Error{HTTPStatus: status, Message: http.StatusText(status)},
			},
		},
	}
}

func TestDispatchClassifiesExcludedCredentialsForNextRetryRound(t *testing.T) {
	selectors := []struct {
		name     string
		selector Selector
	}{
		{name: "scheduler fast path", selector: &RoundRobinSelector{}},
		{name: "custom selector path", selector: firstCandidateSelector{}},
	}
	for _, test := range selectors {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, test.selector, nil)
			cooldownZero := dispatchRetryCooldownAuth("retry-round-cooldown-zero-"+test.name, 0)
			cooldownTwo := dispatchRetryCooldownAuth("retry-round-cooldown-two-"+test.name, 2)
			registerDispatchTestAuth(t, manager, cooldownZero, "gpt")
			registerDispatchTestAuth(t, manager, cooldownTwo, "gpt")

			opts := Options{Metadata: map[string]any{
				ExcludedAuthIDsMetadataKey: []string{cooldownZero.ID, cooldownTwo.ID},
			}}
			decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", opts)
			if decision != nil {
				t.Fatalf("Dispatch() decision = %#v, want nil", decision)
			}
			var cooldownErr *modelCooldownError
			if !errors.As(errDispatch, &cooldownErr) || cooldownErr == nil {
				t.Fatalf("Dispatch() error = %T %v, want model cooldown", errDispatch, errDispatch)
			}
			if retryLimit, ok := cooldownErr.RequestRetryLimit(); !ok || retryLimit != 2 {
				t.Fatalf("RequestRetryLimit() = (%d, %t), want (2, true)", retryLimit, ok)
			}
			if retryAfter := cooldownErr.RetryAfter(); retryAfter == nil || *retryAfter <= 0 {
				t.Fatalf("RetryAfter() = %v, want positive next-round cooldown", retryAfter)
			}
		})
	}
}

func TestDispatchReportsEarliestEligibleCooldownForRetryInterval(t *testing.T) {
	selectors := []struct {
		name     string
		selector Selector
	}{
		{name: "scheduler fast path", selector: &RoundRobinSelector{}},
		{name: "custom selector path", selector: firstCandidateSelector{}},
	}
	for _, test := range selectors {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, test.selector, nil)
			now := time.Now()
			shortCooldown := dispatchRetryCooldownAuth("retry-interval-short-"+test.name, 1)
			longCooldown := dispatchRetryCooldownAuth("retry-interval-long-"+test.name, 1)
			shortCooldown.ModelStates["gpt"].NextRetryAfter = now.Add(10 * time.Second)
			shortCooldown.ModelStates["gpt"].Quota.NextRecoverAt = now.Add(10 * time.Second)
			longCooldown.ModelStates["gpt"].NextRetryAfter = now.Add(time.Minute)
			longCooldown.ModelStates["gpt"].Quota.NextRecoverAt = now.Add(time.Minute)
			registerDispatchTestAuth(t, manager, shortCooldown, "gpt")
			registerDispatchTestAuth(t, manager, longCooldown, "gpt")

			_, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
				ExcludedAuthIDsMetadataKey: []string{shortCooldown.ID, longCooldown.ID},
			}})
			var cooldownErr *modelCooldownError
			if !errors.As(errDispatch, &cooldownErr) || cooldownErr == nil {
				t.Fatalf("Dispatch() error = %T %v, want model cooldown", errDispatch, errDispatch)
			}
			retryAfter := cooldownErr.RetryAfter()
			if retryAfter == nil || *retryAfter <= 0 || *retryAfter > 30*time.Second {
				t.Fatalf("RetryAfter() = %v, want the 10s credential within the 30s retry interval", retryAfter)
			}
		})
	}
}

func TestDispatchClassifiesTransientCooldownForNextRetryRound(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{name: "internal server error", status: http.StatusInternalServerError},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
	}
	selectors := []struct {
		name     string
		selector Selector
	}{
		{name: "scheduler fast path", selector: &RoundRobinSelector{}},
		{name: "custom selector path", selector: firstCandidateSelector{}},
	}
	for _, status := range statuses {
		for _, selector := range selectors {
			t.Run(status.name+"/"+selector.name, func(t *testing.T) {
				manager := NewManager(nil, selector.selector, nil)
				cooldownOne := dispatchRetryTransientCooldownAuth("retry-transient-one-"+status.name+selector.name, 1, status.status)
				cooldownTwo := dispatchRetryTransientCooldownAuth("retry-transient-two-"+status.name+selector.name, 2, status.status)
				registerDispatchTestAuth(t, manager, cooldownOne, "gpt")
				registerDispatchTestAuth(t, manager, cooldownTwo, "gpt")

				_, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
					ExcludedAuthIDsMetadataKey: []string{cooldownOne.ID, cooldownTwo.ID},
				}})
				var cooldownErr *modelCooldownError
				if !errors.As(errDispatch, &cooldownErr) || cooldownErr == nil {
					t.Fatalf("Dispatch() error = %T %v, want transient model cooldown", errDispatch, errDispatch)
				}
				if retryLimit, ok := cooldownErr.RequestRetryLimit(); !ok || retryLimit != 2 {
					t.Fatalf("RequestRetryLimit() = (%d, %t), want (2, true)", retryLimit, ok)
				}
				if retryAfter := cooldownErr.RetryAfter(); retryAfter == nil || *retryAfter <= 0 {
					t.Fatalf("RetryAfter() = %v, want positive transient cooldown", retryAfter)
				}
			})
		}
	}
}

func TestDispatchDoesNotWaitWhenExcludedCredentialIsReadyNextRound(t *testing.T) {
	selectors := []struct {
		name     string
		selector Selector
	}{
		{name: "scheduler fast path", selector: &RoundRobinSelector{}},
		{name: "custom selector path", selector: firstCandidateSelector{}},
	}
	for _, test := range selectors {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, test.selector, nil)
			auth := &Auth{ID: "retry-round-ready-" + test.name, Provider: "codex", Status: StatusActive}
			registerDispatchTestAuth(t, manager, auth, "gpt")

			_, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
				ExcludedAuthIDsMetadataKey: []string{auth.ID},
			}})
			var authErr *Error
			if !errors.As(errDispatch, &authErr) || authErr == nil || authErr.Code != "auth_unavailable" {
				t.Fatalf("Dispatch() error = %T %v, want auth_unavailable", errDispatch, errDispatch)
			}
			var cooldownErr *modelCooldownError
			if errors.As(errDispatch, &cooldownErr) {
				t.Fatalf("Dispatch() returned cooldown for immediately reusable auth: %v", errDispatch)
			}
		})
	}
}

func TestDispatchSuccessIncludesEligibleRequestRetryAggregate(t *testing.T) {
	selectors := []struct {
		name     string
		suffix   string
		selector Selector
	}{
		{name: "scheduler fast path", suffix: "fast", selector: &RoundRobinSelector{}},
		{name: "custom selector path", suffix: "custom", selector: firstCandidateSelector{}},
	}
	for _, test := range selectors {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, test.selector, nil)
			manager.SetConfig(&internalconfig.Config{RequestRetry: 0})
			selected := &Auth{ID: "retry-aggregate-selected-" + test.suffix, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 0}}
			nextRound := &Auth{ID: "retry-aggregate-next-round-" + test.suffix, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 2}}
			filtered := &Auth{ID: "retry-aggregate-filtered-" + test.suffix, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 9}}
			registerDispatchTestAuth(t, manager, selected, "gpt")
			registerDispatchTestAuth(t, manager, nextRound, "gpt")
			registerDispatchTestAuth(t, manager, filtered, "gpt")

			decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
				AllowedAuthIDsMetadataKey:  []string{selected.ID, nextRound.ID},
				ExcludedAuthIDsMetadataKey: []string{nextRound.ID},
			}})
			if errDispatch != nil {
				t.Fatalf("Dispatch() error = %v", errDispatch)
			}
			if decision == nil || decision.Auth == nil || decision.Auth.ID != selected.ID {
				t.Fatalf("Dispatch() decision = %#v, want selected auth %q", decision, selected.ID)
			}
			if decision.RequestRetry != 2 {
				t.Fatalf("Dispatch() request retry = %d, want eligible next-round maximum 2", decision.RequestRetry)
			}
		})
	}
}

func TestDispatchRequestRetryAggregateIgnoresNonRoundCredentials(t *testing.T) {
	selectors := []struct {
		name     string
		suffix   string
		selector Selector
	}{
		{name: "scheduler fast path", suffix: "fast", selector: &RoundRobinSelector{}},
		{name: "custom selector path", suffix: "custom", selector: firstCandidateSelector{}},
	}
	for _, test := range selectors {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, test.selector, nil)
			manager.SetConfig(&internalconfig.Config{RequestRetry: 0})
			selected := &Auth{ID: "retry-round-selected-" + test.suffix, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 0}}
			transient := dispatchRetryTransientCooldownAuth("retry-round-transient-"+test.suffix, 1, http.StatusServiceUnavailable)
			disabledModel := &Auth{
				ID:       "retry-round-model-disabled-" + test.suffix,
				Provider: "codex",
				Status:   StatusError,
				Metadata: map[string]any{"request_retry": 9},
				ModelStates: map[string]*ModelState{
					"gpt": {Status: StatusDisabled},
				},
			}
			unauthorized := dispatchRetryTransientCooldownAuth("retry-round-unauthorized-"+test.suffix, 8, http.StatusUnauthorized)
			for _, auth := range []*Auth{selected, transient, disabledModel, unauthorized} {
				registerDispatchTestAuth(t, manager, auth, "gpt")
			}

			decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{})
			if errDispatch != nil {
				t.Fatalf("Dispatch() error = %v", errDispatch)
			}
			if decision == nil || decision.Auth == nil || decision.Auth.ID != selected.ID {
				t.Fatalf("Dispatch() decision = %#v, want selected auth %q", decision, selected.ID)
			}
			if decision.RequestRetry != 1 {
				t.Fatalf("Dispatch() request retry = %d, want only transient credential maximum 1", decision.RequestRetry)
			}
		})
	}
}

func TestDispatchStaleQuotaWithNonRetryableErrorDoesNotEnableRetryRound(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{name: "payment required", status: http.StatusPaymentRequired},
		{name: "not found", status: http.StatusNotFound},
	}
	selectors := []struct {
		name     string
		suffix   string
		selector Selector
	}{
		{name: "scheduler fast path", suffix: "fast", selector: &RoundRobinSelector{}},
		{name: "custom selector path", suffix: "custom", selector: firstCandidateSelector{}},
	}
	for _, status := range statuses {
		for _, selector := range selectors {
			t.Run(status.name+"/"+selector.name, func(t *testing.T) {
				manager := NewManager(nil, selector.selector, nil)
				manager.SetConfig(&internalconfig.Config{RequestRetry: 0})
				selected := &Auth{ID: "stale-quota-selected-" + status.name + selector.suffix, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 0}}
				staleQuota := dispatchRetryCooldownAuth("stale-quota-blocked-"+status.name+selector.suffix, 9)
				staleQuota.ModelStates["gpt"].LastError = &Error{HTTPStatus: status.status, Message: http.StatusText(status.status)}
				registerDispatchTestAuth(t, manager, selected, "gpt")
				registerDispatchTestAuth(t, manager, staleQuota, "gpt")

				decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{})
				if errDispatch != nil {
					t.Fatalf("Dispatch() error = %v", errDispatch)
				}
				if decision == nil || decision.Auth == nil || decision.Auth.ID != selected.ID {
					t.Fatalf("Dispatch() decision = %#v, want selected auth %q", decision, selected.ID)
				}
				if decision.RequestRetry != 0 {
					t.Fatalf("Dispatch() request retry = %d, want stale quota status excluded from aggregate", decision.RequestRetry)
				}

				_, errExhausted := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
					AllowedAuthIDsMetadataKey:  []string{staleQuota.ID},
					ExcludedAuthIDsMetadataKey: []string{staleQuota.ID},
				}})
				var authErr *Error
				if !errors.As(errExhausted, &authErr) || authErr == nil || authErr.Code != "auth_not_found" {
					t.Fatalf("Dispatch() exhausted error = %T %v, want auth_not_found", errExhausted, errExhausted)
				}
				var cooldownErr *modelCooldownError
				if errors.As(errExhausted, &cooldownErr) {
					t.Fatalf("Dispatch() returned retry cooldown for stale quota status: %v", errExhausted)
				}
			})
		}
	}
}

func TestDispatchNextRoundCooldownIgnoresFilteredCredentials(t *testing.T) {
	tests := []struct {
		name       string
		readyAuth  func(string) *Auth
		readyModel string
		metadata   func(string) map[string]any
	}{
		{
			name: "channel allowlist",
			readyAuth: func(id string) *Auth {
				return &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 9}}
			},
			readyModel: "gpt",
			metadata: func(cooldownID string) map[string]any {
				return map[string]any{AllowedAuthIDsMetadataKey: []string{cooldownID}}
			},
		},
		{
			name: "route model",
			readyAuth: func(id string) *Auth {
				return &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 9}}
			},
			readyModel: "other-model",
			metadata:   func(string) map[string]any { return nil },
		},
		{
			name: "disabled",
			readyAuth: func(id string) *Auth {
				return &Auth{ID: id, Provider: "codex", Status: StatusDisabled, Disabled: true, Metadata: map[string]any{"request_retry": 9}}
			},
			readyModel: "gpt",
			metadata:   func(string) map[string]any { return nil },
		},
		{
			name: "credential policy",
			readyAuth: func(id string) *Auth {
				return &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "ordinary"}, Metadata: map[string]any{"request_retry": 9}}
			},
			readyModel: "gpt",
			metadata: func(string) map[string]any {
				return map[string]any{CredentialPolicyMetadataKey: CredentialPolicyCodexAlphaSearchV1}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			cooldown := dispatchRetryCooldownAuth("retry-filter-cooldown-"+test.name, 1)
			ready := test.readyAuth("retry-filter-ready-" + test.name)
			registerDispatchTestAuth(t, manager, cooldown, "gpt")
			registerDispatchTestAuth(t, manager, ready, test.readyModel)
			metadata := test.metadata(cooldown.ID)
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata[ExcludedAuthIDsMetadataKey] = []string{cooldown.ID}

			_, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{Metadata: metadata})
			var cooldownErr *modelCooldownError
			if !errors.As(errDispatch, &cooldownErr) || cooldownErr == nil {
				t.Fatalf("Dispatch() error = %T %v, want eligible next-round cooldown", errDispatch, errDispatch)
			}
			if retryLimit, ok := cooldownErr.RequestRetryLimit(); !ok || retryLimit != 1 {
				t.Fatalf("RequestRetryLimit() = (%d, %t), want filtered maximum 1", retryLimit, ok)
			}
		})
	}
}

func assertDispatchAvailabilitySummaryEqual(t *testing.T, indexed, scanned dispatchAvailabilitySummary) {
	t.Helper()
	if indexed.total != scanned.total ||
		indexed.cooldownCount != scanned.cooldownCount ||
		!indexed.earliest.Equal(scanned.earliest) ||
		indexed.requestRetry != scanned.requestRetry ||
		indexed.hasRequestRetry != scanned.hasRequestRetry {
		t.Fatalf("indexed summary = %#v, scanned summary = %#v", indexed, scanned)
	}
}

func TestSchedulerDispatchAvailabilitySummaryMatchesScan(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{RequestRetry: 3})
	auths := []*Auth{
		{ID: "summary-ready-inherited", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}},
		{ID: "summary-ready-override", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"request_retry": 2}},
		dispatchRetryCooldownAuth("summary-quota-cooldown", 5),
		dispatchRetryTransientCooldownAuth("summary-transient-cooldown", 4, http.StatusServiceUnavailable),
		dispatchRetryTransientCooldownAuth("summary-unauthorized", 9, http.StatusUnauthorized),
		{
			ID:       "summary-ordinary-api-key",
			Provider: "codex",
			Status:   StatusActive,
			Attributes: map[string]string{
				"auth_kind":               "api_key",
				AttributeCodexAlphaSearch: "false",
			},
			Metadata: map[string]any{"request_retry": 8},
		},
		{ID: "summary-gemini", Provider: "gemini", Status: StatusActive, Metadata: map[string]any{"request_retry": 7}},
		{ID: "summary-disabled", Provider: "codex", Status: StatusDisabled, Disabled: true, Metadata: map[string]any{"request_retry": 10}},
	}
	for _, auth := range auths {
		registerDispatchTestAuth(t, manager, auth, "gpt")
	}
	registerDispatchTestAuth(t, manager, &Auth{
		ID:       "summary-suffixed-registry-model",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"request_retry": 11},
	}, "gpt(high)")

	tests := []struct {
		name      string
		providers []string
		model     string
		opts      Options
		excluded  map[string]struct{}
	}{
		{name: "single provider", providers: []string{"codex"}, model: "gpt"},
		{name: "mixed providers", providers: []string{"codex", "gemini"}, model: "gpt"},
		{
			name:      "allowlist",
			providers: []string{"codex"},
			model:     "gpt",
			opts: Options{Metadata: map[string]any{
				AllowedAuthIDsMetadataKey: []string{"summary-ready-override", "summary-transient-cooldown", "summary-unauthorized"},
			}},
		},
		{
			name:      "empty allowlist",
			providers: []string{"codex"},
			model:     "gpt",
			opts: Options{Metadata: map[string]any{
				AllowedAuthIDsMetadataKey: []string{},
			}},
		},
		{
			name:      "credential policy",
			providers: []string{"codex"},
			model:     "gpt",
			opts: Options{Metadata: map[string]any{
				CredentialPolicyMetadataKey: CredentialPolicyCodexAlphaSearchV1,
			}},
		},
		{
			name:      "slow path exclusions",
			providers: []string{"codex"},
			model:     "gpt",
			excluded: map[string]struct{}{
				"summary-ready-inherited": {},
				"summary-quota-cooldown":  {},
			},
		},
		{
			name:      "model allowlist rejection",
			providers: []string{"codex"},
			model:     "gpt",
			opts: Options{Metadata: map[string]any{
				AllowedModelIDsMetadataKey: []string{"other"},
			}},
		},
		{name: "canonical route requires registered base model", providers: []string{"codex"}, model: "gpt(high)"},
		{name: "registry model matching remains case insensitive", providers: []string{"codex"}, model: "GPT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			indexed := manager.scheduler.summarizeDispatchAvailability(test.providers, test.model, test.opts, test.excluded, 3)
			scanned := manager.scanDispatchAvailability(test.providers, test.model, test.opts, test.excluded)
			assertDispatchAvailabilitySummaryEqual(t, indexed, scanned)
		})
	}
}

func TestSchedulerDispatchAvailabilityAggregateRefreshesAfterMutation(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{RequestRetry: 2})
	inherited := &Auth{ID: "summary-mutation-inherited", Provider: "codex", Status: StatusActive}
	overridden := &Auth{ID: "summary-mutation-override", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 5}}
	registerDispatchTestAuth(t, manager, inherited, "gpt")
	registerDispatchTestAuth(t, manager, overridden, "gpt")

	assertSummary := func(wantRetry int) {
		t.Helper()
		indexed := manager.scheduler.summarizeDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil, 2)
		scanned := manager.scanDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil)
		assertDispatchAvailabilitySummaryEqual(t, indexed, scanned)
		if indexed.requestRetry != wantRetry {
			t.Fatalf("request retry = %d, want %d", indexed.requestRetry, wantRetry)
		}
	}

	assertSummary(5)
	updated, ok := manager.GetByID(overridden.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) did not return auth", overridden.ID)
	}
	updated.Metadata["request_retry"] = 1
	if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
		t.Fatalf("Update(%q) error = %v", overridden.ID, errUpdate)
	}
	assertSummary(2)

	if errDelete := manager.Delete(context.Background(), inherited.ID); errDelete != nil {
		t.Fatalf("Delete(%q) error = %v", inherited.ID, errDelete)
	}
	assertSummary(1)

	updated, ok = manager.GetByID(overridden.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID(%q) did not return auth after update", overridden.ID)
	}
	delete(updated.Metadata, "request_retry")
	if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
		t.Fatalf("Update(%q) error = %v", overridden.ID, errUpdate)
	}
	manager.SetConfig(&internalconfig.Config{RequestRetry: 6})
	indexed := manager.scheduler.summarizeDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil, 6)
	scanned := manager.scanDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil)
	assertDispatchAvailabilitySummaryEqual(t, indexed, scanned)
	if indexed.requestRetry != 6 {
		t.Fatalf("request retry after config reload = %d, want 6", indexed.requestRetry)
	}

	registry.GetGlobalRegistry().RegisterClient(overridden.ID, "codex", []*registry.ModelInfo{{ID: "other", Object: "model", Type: "openai"}})
	manager.RefreshSchedulerEntry(overridden.ID)
	indexed = manager.scheduler.summarizeDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil, 6)
	scanned = manager.scanDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil)
	assertDispatchAvailabilitySummaryEqual(t, indexed, scanned)
	if indexed.total != 0 {
		t.Fatalf("summary total after registry refresh = %d, want 0", indexed.total)
	}
}

func BenchmarkDispatchAvailabilitySummary(b *testing.B) {
	const credentialCount = 1000
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := &internalconfig.Config{
		RequestRetry: 2,
		CodexKey:     make([]internalconfig.CodexKey, credentialCount),
	}
	for i := range cfg.CodexKey {
		cfg.CodexKey[i] = internalconfig.CodexKey{
			APIKey:  fmt.Sprintf("benchmark-key-%04d", i),
			BaseURL: "https://benchmark.invalid",
		}
	}
	manager.SetConfig(cfg)
	modelRegistry := registry.GetGlobalRegistry()
	authIDs := make([]string, 0, credentialCount)
	for i := 0; i < credentialCount; i++ {
		authID := fmt.Sprintf("summary-benchmark-%04d", i)
		authIDs = append(authIDs, authID)
		modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt", Object: "model", Type: "openai"}})
		if _, errRegister := manager.Register(context.Background(), &Auth{
			ID:       authID,
			Provider: "codex",
			Status:   StatusActive,
			Attributes: map[string]string{
				"auth_kind": "api_key",
				"api_key":   cfg.CodexKey[i].APIKey,
				"base_url":  cfg.CodexKey[i].BaseURL,
			},
			Metadata: map[string]any{"request_retry": i % 4},
		}); errRegister != nil {
			b.Fatalf("Register(%q) error = %v", authID, errRegister)
		}
	}
	b.Cleanup(func() {
		for _, authID := range authIDs {
			modelRegistry.UnregisterClient(authID)
		}
	})
	manager.summarizeDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil)

	b.Run("scheduler-index", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(credentialCount, "credentials")
		for i := 0; i < b.N; i++ {
			manager.summarizeDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil)
		}
	})
	b.Run("full-scan", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(credentialCount, "credentials")
		for i := 0; i < b.N; i++ {
			manager.scanDispatchAvailability([]string{"codex"}, "gpt", Options{}, nil)
		}
	})

	allowedOpts := Options{Metadata: map[string]any{AllowedAuthIDsMetadataKey: authIDs}}
	b.Run("scheduler-index-allowlist", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(credentialCount, "credentials")
		for i := 0; i < b.N; i++ {
			manager.summarizeDispatchAvailability([]string{"codex"}, "gpt", allowedOpts, nil)
		}
	})
	b.Run("full-scan-allowlist", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(credentialCount, "credentials")
		for i := 0; i < b.N; i++ {
			manager.scanDispatchAvailability([]string{"codex"}, "gpt", allowedOpts, nil)
		}
	})
}
