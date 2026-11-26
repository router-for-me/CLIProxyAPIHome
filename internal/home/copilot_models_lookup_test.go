package home

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
)

type copilotLookupTestAdapter struct {
	fakeAntigravityClusterAdapter
	lookup func(context.Context, string) (*coreauth.Auth, error)
}

func (a *copilotLookupTestAdapter) GetFullAuth(ctx context.Context, id string) (*coreauth.Auth, error) {
	return a.lookup(ctx, id)
}

func TestCopilotDiscoveryLookupFailureBackoff(t *testing.T) {
	for _, probe := range []struct {
		name   string
		failAt int
		status int
	}{
		{"before fetch", 1, 200},
		{"after success", 2, 200},
		{"after unauthorized", 2, 401},
		{"after forbidden", 2, 403},
	} {
		for _, failure := range []string{"deadline", "probe timeout", "database error"} {
			t.Run(probe.name+"/"+failure, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
					rt := &Runtime{cfg: &config.Config{}, coreManager: m, copilotAccounts: make(map[string]copilotAccountModels)}
					auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "copilot", Status: coreauth.StatusActive,
						RuntimeProjection: true, Metadata: map[string]any{"base_url": copilotTestBase, "account_id": "account"}}
					rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					full := auth.Clone()
					full.RuntimeProjection = false
					full.Metadata["access_token"] = "secret-fixture"
					lookups, requests := 0, 0
					rt.clusterAdapter = &copilotLookupTestAdapter{lookup: func(ctx context.Context, _ string) (*coreauth.Auth, error) {
						lookups++
						if lookups != probe.failAt {
							return full.Clone(), nil
						}
						switch failure {
						case "probe timeout":
							<-ctx.Done()
							return nil, ctx.Err()
						case "database error":
							return nil, errors.New("database read failed: secret-fixture")
						default:
							return nil, fmt.Errorf("secret-fixture: %w", context.DeadlineExceeded)
						}
					}}
					m.SetRoundTripperProvider(copilotTestTransport(func(*http.Request) (*http.Response, error) {
						requests++
						return &http.Response{StatusCode: probe.status, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"data":[{"id":%q}]}`, copilotTestY)))}, nil
					}))
					snapshot := copilotAccountModels{identity: copilotIdentity(auth), ids: map[string]struct{}{copilotTestX: {}}, expiresAt: time.Now().Add(time.Hour)}
					rt.copilotAccounts[auth.ID] = snapshot
					rt.registerModelsForAuth(auth)
					published := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
					var logs bytes.Buffer
					logger := log.StandardLogger()
					previous := logger.Out
					logger.SetOutput(&logs)
					defer logger.SetOutput(previous)

					rt.discoverCopilotModels(t.Context(), auth.ID)
					next := rt.copilotAccounts[auth.ID]
					if next.nextProbeAt != time.Now().Add(copilotModelRetryInterval) {
						t.Fatalf("retry deadline = %v, want one minute from %v", next.nextProbeAt, time.Now())
					}
					next.nextProbeAt = snapshot.nextProbeAt
					if !reflect.DeepEqual(next, snapshot) || !reflect.DeepEqual(published, registry.GetGlobalRegistry().GetModelsForClient(auth.ID)) {
						t.Fatal("lookup failure changed the snapshot or published permissions")
					}
					rt.discoverCopilotModels(t.Context(), auth.ID)
					if lookups != probe.failAt || requests != probe.failAt-1 {
						t.Fatalf("lookups=%d requests=%d: failed lookup did not stop the probe or back off", lookups, requests)
					}
					stage := "lookup_before_fetch"
					if probe.failAt == 2 {
						stage = "lookup_after_fetch"
					}
					output := logs.String()
					if !strings.Contains(output, auth.ID) || !strings.Contains(output, "stage="+stage) || strings.Contains(output, "secret-fixture") {
						t.Fatalf("unsafe or missing lookup diagnostic: %s", output)
					}
					if failure != "database error" && !strings.Contains(output, "timeout") {
						t.Fatalf("missing timeout diagnostic: %s", output)
					}
				})
			})
		}
	}
}

func TestCopilotDiscoveryLookupDoesNotBackoffStaleAccount(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		for _, change := range []string{"missing", "not found", "full disabled", "full replaced", "removed", "disabled", "replaced", "shutdown"} {
			t.Run(fmt.Sprintf("lookup%d/%s", failAt, change), func(t *testing.T) {
				m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
				rt := &Runtime{cfg: &config.Config{}, coreManager: m, copilotAccounts: make(map[string]copilotAccountModels)}
				auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "copilot", Status: coreauth.StatusActive,
					RuntimeProjection: true, Metadata: map[string]any{"base_url": copilotTestBase, "account_id": "old"}}
				rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				full := auth.Clone()
				full.RuntimeProjection = false
				full.Metadata["access_token"] = "secret-fixture"
				rt.copilotAccounts[auth.ID] = copilotAccountModels{identity: copilotIdentity(auth), ids: map[string]struct{}{copilotTestX: {}}, expiresAt: time.Now().Add(time.Hour)}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				lookups, requests := 0, 0
				var expected map[string]copilotAccountModels
				rt.clusterAdapter = &copilotLookupTestAdapter{lookup: func(context.Context, string) (*coreauth.Auth, error) {
					lookups++
					if lookups != failAt {
						return full.Clone(), nil
					}
					current := auth.Clone()
					switch change {
					case "removed":
						rt.applyCoreAuthRemove(t.Context(), auth.ID)
					case "disabled":
						current.Disabled = true
						rt.applyCoreAuthAddOrUpdate(t.Context(), current)
					case "replaced":
						current.Metadata["account_id"] = "new"
						rt.applyCoreAuthAddOrUpdate(t.Context(), current)
					case "shutdown":
						cancel()
					}
					expected = make(map[string]copilotAccountModels)
					for id, snapshot := range rt.copilotAccounts {
						expected[id] = snapshot
					}
					switch change {
					case "missing":
						return nil, nil
					case "not found":
						return nil, fmt.Errorf("lookup: %w", coreauth.ErrFullAuthNotFound)
					case "full disabled":
						current.Disabled = true
						return current, nil
					case "full replaced":
						current.Metadata["account_id"] = "new"
						return current, nil
					default:
						return nil, context.DeadlineExceeded
					}
				}}
				m.SetRoundTripperProvider(copilotTestTransport(func(*http.Request) (*http.Response, error) {
					requests++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
				}))
				var logs bytes.Buffer
				logger := log.StandardLogger()
				previous := logger.Out
				logger.SetOutput(&logs)
				defer logger.SetOutput(previous)
				rt.discoverCopilotModels(ctx, auth.ID)
				if !reflect.DeepEqual(rt.copilotAccounts, expected) || requests != failAt-1 || lookups != failAt {
					t.Fatalf("stale lookup changed snapshot or continued probing: lookups=%d requests=%d snapshots=%+v", lookups, requests, rt.copilotAccounts)
				}
				if change == "shutdown" || change == "not found" || change == "missing" || strings.HasPrefix(change, "full ") {
					if logs.Len() != 0 {
						t.Fatalf("expected absence or shutdown to exit quietly: %s", logs.String())
					}
				}
			})
		}
	}
}

func TestCopilotDiscoveryInitialLookupFailureBackoff(t *testing.T) {
	m := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	rt := &Runtime{cfg: &config.Config{}, coreManager: m, copilotAccounts: make(map[string]copilotAccountModels)}
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "copilot", Status: coreauth.StatusActive,
		RuntimeProjection: true, Metadata: map[string]any{"base_url": copilotTestBase}}
	rt.applyCoreAuthAddOrUpdate(t.Context(), auth)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	lookups := 0
	rt.clusterAdapter = &copilotLookupTestAdapter{lookup: func(context.Context, string) (*coreauth.Auth, error) {
		lookups++
		return nil, context.DeadlineExceeded
	}}
	before := time.Now()
	rt.discoverCopilotModels(t.Context(), auth.ID)
	rt.discoverCopilotModels(t.Context(), auth.ID)
	snapshot := rt.copilotAccounts[auth.ID]
	if lookups != 1 || snapshot.identity != copilotIdentity(auth) || snapshot.ids != nil || !snapshot.expiresAt.IsZero() ||
		snapshot.nextProbeAt.Before(before.Add(copilotModelRetryInterval)) || snapshot.nextProbeAt.After(time.Now().Add(copilotModelRetryInterval)) {
		t.Fatalf("initial failure should only schedule a retry: lookups=%d snapshot=%+v", lookups, snapshot)
	}
}
