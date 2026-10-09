package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestDescribeModelStatesAliases(t *testing.T) {
	const upstream = "grok-4.7-build-fast"
	const alias = "fast"
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"api-key-config", "api-key-metadata", "oauth"} {
		for _, prefix := range []string{"", "team"} {
			t.Run(kind+"/"+prefix, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				t.Cleanup(manager.Shutdown)
				auth := &Auth{
					ID: "state-alias-" + kind + "-" + prefix, Provider: "xai", Prefix: prefix,
					Status: StatusError, Attributes: map[string]string{"auth_kind": "oauth"},
					ModelStates: map[string]*ModelState{upstream: {
						Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
						LastError: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
						Quota:     QuotaState{Exceeded: true, NextRecoverAt: now.Add(2 * time.Hour)},
					}},
				}
				switch kind {
				case "api-key-config":
					auth.Attributes = map[string]string{"auth_kind": "apikey", "api_key": "test-key"}
					manager.SetConfig(&internalconfig.Config{XAIKey: []internalconfig.XAIKey{{
						APIKey: "test-key", Models: []internalconfig.CodexModel{{Name: upstream, Alias: alias}},
					}}})
				case "api-key-metadata":
					auth.Attributes["auth_kind"] = "apikey"
					auth.Metadata = map[string]any{homeConfigModelsMetadataKey: []map[string]any{{
						"id": alias, "name": upstream, "user_defined": true,
					}}}
				case "oauth":
					manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {{Name: upstream, Alias: alias}}})
				}
				route := alias
				if prefix != "" {
					route = prefix + "/" + alias
				}
				registerDispatchTestAuth(t, manager, auth, route)
				for _, filter := range []string{"", route, alias, upstream} {
					views := manager.DescribeModelStates(auth, filter, now)
					if len(views) != 1 {
						t.Fatalf("filter %q: views = %#v, want one route", filter, views)
					}
					view := views[0]
					if view.Model != route || view.UpstreamModel != upstream || view.StateKey != upstream || !view.Registered {
						t.Fatalf("filter %q: wrong mapping: %#v", filter, view)
					}
					if !view.Blocked || view.BlockReason != "cooldown" || view.State.LastError.HTTPStatus != 429 || !view.NextRetryAfter.Equal(now.Add(2*time.Hour)) {
						t.Fatalf("filter %q: wrong availability: %#v", filter, view)
					}
				}
			})
		}
	}
}

func TestDescribeModelStatesLegacyBlock(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	manager := NewManager(nil, nil, nil)
	t.Cleanup(manager.Shutdown)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {{Name: "upstream", Alias: "fast"}}})
	auth := &Auth{
		ID: "state-legacy", Provider: "xai", Status: StatusError,
		ModelStates: map[string]*ModelState{
			"upstream": {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(-time.Hour), LastError: &Error{HTTPStatus: 503}},
			"fast": {
				Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
				LastError: &Error{HTTPStatus: 429}, Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Hour)},
			},
		},
	}
	registerDispatchTestAuth(t, manager, auth, "fast")
	view := manager.DescribeModelStates(auth, "fast", now)[0]
	if view.UpstreamModel != "upstream" || view.StateKey != "fast" || view.State.LastError.HTTPStatus != 429 || !view.NextRetryAfter.Equal(now.Add(time.Hour)) {
		t.Fatalf("legacy block must supply the error and retry deadline together: %#v", view)
	}
	if !view.Blocked || view.BlockReason != "cooldown" {
		t.Fatalf("legacy block unavailable: %#v", view)
	}
}

func TestDescribeModelStatesCredentialGatePreservesLegacyHistory(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		gate            string
		status          int
		upstreamHistory bool
	}{
		{name: "refresh/499", gate: "refresh", status: 499},
		{name: "refresh/quota", gate: "refresh", status: 429},
		{name: "disabled/499", gate: "disabled", status: 499},
		{name: "refresh/upstream-preferred", gate: "refresh", status: 499, upstreamHistory: true},
		{name: "disabled/upstream-preferred", gate: "disabled", status: 499, upstreamHistory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			t.Cleanup(manager.Shutdown)
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {{Name: "upstream", Alias: "fast"}}})
			legacy := &ModelState{
				Status: StatusError, StatusMessage: "context canceled", Unavailable: true,
				LastError: &Error{HTTPStatus: tc.status, Message: "context canceled"},
				UpdatedAt: now.Add(-time.Minute),
			}
			if tc.status == 429 {
				legacy.StatusMessage = "quota"
				legacy.LastError.Message = "quota"
				legacy.NextRetryAfter = now.Add(2 * time.Hour)
				legacy.Quota = QuotaState{Exceeded: true, NextRecoverAt: legacy.NextRetryAfter, BackoffLevel: 2}
			}
			auth := &Auth{
				ID: "state-legacy-gate-" + tc.name, Provider: "xai", Status: StatusError,
				Unavailable: true, NextRetryAfter: now.Add(time.Hour),
				RuntimeRefreshBlocked: tc.gate == "refresh", Disabled: tc.gate == "disabled",
				ModelStates: map[string]*ModelState{"fast": legacy},
			}
			wantKey, wantState := "fast", legacy
			if tc.upstreamHistory {
				wantKey = "upstream"
				wantState = &ModelState{Status: StatusActive, UpdatedAt: now.Add(-2 * time.Minute)}
				auth.ModelStates[wantKey] = wantState
			}
			registerDispatchTestAuth(t, manager, auth, "fast")
			views := manager.DescribeModelStates(auth, "fast", now)
			if len(views) != 1 {
				t.Fatalf("credential gate must retain one registered route: %#v", views)
			}
			view := views[0]
			if view.UpstreamModel != "upstream" || view.StateKey != wantKey || !reflect.DeepEqual(view.State, wantState) {
				t.Fatalf("credential gate hid or replaced model history: %#v, want state_key=%q state=%#v", view, wantKey, wantState)
			}
			wantReason, wantRetry := "other", auth.NextRetryAfter
			if tc.gate == "disabled" {
				wantReason, wantRetry = "disabled", time.Time{}
			}
			if !view.Blocked || view.BlockReason != wantReason || !view.NextRetryAfter.Equal(wantRetry) {
				t.Fatalf("history selection changed credential blocking: %#v", view)
			}
		})
	}
}

func TestDescribeModelStatesPrefixedFilterCaseInsensitive(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"oauth", "apikey"} {
		for _, prefix := range []string{"team", "team/nested"} {
			t.Run(kind+"/"+prefix, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				t.Cleanup(manager.Shutdown)
				auth := &Auth{
					ID: "state-filter-case-" + kind + "-" + prefix, Provider: "xai", Prefix: prefix, Status: StatusError,
					Attributes:  map[string]string{"auth_kind": kind},
					ModelStates: map[string]*ModelState{"upstream": {Status: StatusError, LastError: &Error{HTTPStatus: 499}}},
				}
				if kind == "oauth" {
					manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {
						{Name: "upstream", Alias: "fast"}, {Name: "upstream", Alias: "other"},
					}})
				} else {
					auth.Metadata = map[string]any{homeConfigModelsMetadataKey: []map[string]any{
						{"id": "fast", "name": "upstream", "user_defined": true},
						{"id": "other", "name": "upstream", "user_defined": true},
					}}
				}
				route := prefix + "/fast"
				registerDispatchTestAuth(t, manager, auth, route, prefix+"/other")
				want := manager.DescribeModelStates(auth, route, now)
				if len(want) != 2 {
					t.Fatalf("both aliases must match the shared upstream: %#v", want)
				}
				for _, filter := range []string{
					strings.ToUpper(prefix) + "/fast", strings.ToUpper(route),
					strings.ToUpper(prefix) + "/UPSTREAM",
				} {
					if got := manager.DescribeModelStates(auth, filter, now); !reflect.DeepEqual(got, want) {
						t.Fatalf("filter %q differs from %q: got %#v, want %#v", filter, route, got, want)
					}
				}
				if got := manager.DescribeModelStates(auth, prefix+"-other/fast", now); len(got) != 0 {
					t.Fatalf("different prefixes must not match: %#v", got)
				}
				upperRoute := strings.ToUpper(route)
				if got := manager.resolveDispatchModel(auth, upperRoute).Key; got != upperRoute {
					t.Fatalf("diagnostic filtering must not change Dispatch prefix matching: got %q, want %q", got, upperRoute)
				}
			})
		}
	}
}

func TestDescribeModelStatesCoolingPolicy(t *testing.T) {
	const model = "grok-4.7"
	now := time.Now().UTC()
	disabled, enabled := true, false
	for _, tc := range []struct {
		name     string
		global   bool
		override *bool
		blocked  bool
	}{
		{name: "enabled", blocked: true},
		{name: "global disabled", global: true},
		{name: "credential disabled", override: &disabled},
		{name: "credential enabled", global: true, override: &enabled, blocked: true},
	} {
		for _, status := range []int{429, 503} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, status), func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				t.Cleanup(manager.Shutdown)
				manager.SetConfig(&internalconfig.Config{DisableCooling: tc.global})
				auth := &Auth{
					ID: fmt.Sprintf("state-policy-%s-%d", tc.name, status), Provider: "xai", Status: StatusError,
					RuntimeDisableCooling: tc.override,
					ModelStates: map[string]*ModelState{model: {
						Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
						LastError: &Error{HTTPStatus: status}, Quota: QuotaState{Exceeded: status == 429},
					}},
				}
				registerDispatchTestAuth(t, manager, auth, model)
				before := auth.Clone()
				view := manager.DescribeModelStates(auth, model, now)[0]
				if view.Blocked != tc.blocked || view.NextRetryAfter.IsZero() == tc.blocked {
					t.Fatalf("availability = %#v, want blocked=%v", view, tc.blocked)
				}
				if view.State.LastError.HTTPStatus != status || !reflect.DeepEqual(auth, before) {
					t.Fatal("describing effective availability must preserve recorded history and the source snapshot")
				}
				_, errDispatch := manager.Dispatch(context.Background(), []string{"xai"}, model, Options{})
				if (errDispatch != nil) != tc.blocked {
					t.Fatalf("query blocked=%v contradicts Dispatch: %v", view.Blocked, errDispatch)
				}
			})
		}
	}
}

func TestDescribeModelStatesCanceledRequest(t *testing.T) {
	const model = "grok-4.7"
	manager := NewManager(nil, nil, nil)
	t.Cleanup(manager.Shutdown)
	auth := &Auth{ID: "state-canceled", Index: "state-canceled", Provider: "xai", Status: StatusActive}
	registerDispatchTestAuth(t, manager, auth, model)
	manager.MarkResult(context.Background(), NewUsageResult(auth.Index, auth.Provider, model, 499, "context canceled"))
	after, _ := manager.GetByID(auth.ID)
	view := manager.DescribeModelStates(after, model, time.Now())[0]
	if view.Blocked || !view.NextRetryAfter.IsZero() || view.State.LastError.HTTPStatus != 499 {
		t.Fatalf("499 without a retry deadline must remain dispatchable: %#v", view)
	}
}

func TestDescribeModelStatesUnregisteredKeyIsNotAliased(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	manager := NewManager(nil, nil, nil)
	t.Cleanup(manager.Shutdown)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {{Name: "different-upstream", Alias: "orphan"}}})
	auth := &Auth{
		ID: "state-orphan", Provider: "xai", Status: StatusActive,
		ModelStates: map[string]*ModelState{"orphan": {
			Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), LastError: &Error{HTTPStatus: 503},
		}},
	}
	registerDispatchTestAuth(t, manager, auth, "Custom-Model")
	views := manager.DescribeModelStates(auth, "", now)
	if len(views) != 2 || views[0].Model != "Custom-Model" || views[1].Model != "orphan" {
		t.Fatalf("registered and orphan states must be sorted: %#v", views)
	}
	if views[0].State != nil || !views[0].Registered || views[0].Blocked {
		t.Fatalf("unobserved registered model: %#v", views[0])
	}
	orphan := views[1]
	if orphan.Registered || orphan.UpstreamModel != "orphan" || orphan.StateKey != "orphan" || !orphan.Blocked || orphan.State.LastError.HTTPStatus != 503 {
		t.Fatalf("orphan was aliased again: %#v", orphan)
	}
	if filtered := manager.DescribeModelStates(auth, "custom-model", now); len(filtered) != 1 || filtered[0].Model != "Custom-Model" {
		t.Fatalf("case-insensitive filtering must preserve model IDs: %#v", filtered)
	}
}

func TestDescribeModelStatesSharedUpstream(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	t.Cleanup(manager.Shutdown)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"xai": {
		{Name: "upstream", Alias: "alias-a"},
		{Name: "upstream", Alias: "alias-b"},
	}})
	auth := &Auth{
		ID: "state-shared", Provider: "xai", Status: StatusError,
		ModelStates: map[string]*ModelState{"upstream": {Status: StatusError, LastError: &Error{HTTPStatus: 499}}},
	}
	registerDispatchTestAuth(t, manager, auth, "alias-a", "alias-b")
	for _, filter := range []string{"alias-a", "alias-b", "upstream"} {
		views := manager.DescribeModelStates(auth, filter, time.Now())
		if len(views) != 2 || views[0].StateKey != "upstream" || views[1].StateKey != "upstream" {
			t.Fatalf("filter %q must expose both routes sharing upstream state: %#v", filter, views)
		}
		views[0].State.LastError.HTTPStatus = 503
		if auth.ModelStates["upstream"].LastError.HTTPStatus != 499 || views[1].State.LastError.HTTPStatus != 499 {
			t.Fatal("returned model histories must not share mutable state")
		}
	}
}

func TestDescribeModelStatesCredentialGates(t *testing.T) {
	for _, gate := range []string{"disabled", "refresh"} {
		t.Run(gate, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			t.Cleanup(manager.Shutdown)
			auth := &Auth{ID: "state-gate-" + gate, Provider: "xai", Status: StatusActive}
			if gate == "disabled" {
				auth.Disabled = true
			} else {
				auth.Unavailable = true
				auth.RuntimeRefreshBlocked = true
			}
			registerDispatchTestAuth(t, manager, auth, "grok-4.7")
			view := manager.DescribeModelStates(auth, "", time.Now())[0]
			if !view.Blocked || view.State != nil {
				t.Fatalf("credential gate must block even without model history: %#v", view)
			}
		})
	}
}
