package cluster

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type sessionPolicyAdmissionAdapter struct {
	*RuntimeAdapter
	rejectID string
	lifetime ConnectionLifetime
}

func (a *sessionPolicyAdmissionAdapter) AdmitCredentialConcurrency(ctx context.Context, req home.ConcurrencyAdmissionRequest) (home.ConcurrencyAdmissionResult, error) {
	if req.CredentialID == a.rejectID {
		return home.ConcurrencyAdmissionResult{}, &ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded"}
	}
	result, errAdmit := a.repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
		CredentialID: req.CredentialID, Model: req.Model, ProtocolVersion: 1, SessionAffinity: req.SessionAffinity,
		Lifetime: a.lifetime,
	})
	return home.ConcurrencyAdmissionResult{CredentialID: result.CredentialID, Model: result.Model, Accounted: result.Accounted}, errAdmit
}

func TestSessionAffinityPoliciesAcrossHomesAndExpiration(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, mode := range []string{"any", "quota", "never"} {
			for _, failure := range []string{"transient", "quota", "expired-quota-admission"} {
				t.Run(strategy+"/"+mode+"/"+failure, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					defer cancel()
					path := filepath.Join(t.TempDir(), "home.db")
					repoA := openRefreshTestRepositoryAt(t, path, true)
					repoB := openRefreshTestRepositoryAt(t, path, false)
					auths := []*coreauth.Auth{
						{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "fixture-a", "priority": "10"}},
						{ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "fixture-b"}},
					}
					for _, auth := range auths {
						auth.Index = auth.ID
						auth.Metadata = map[string]any{"home_config_models": []map[string]any{{"id": "policy-model", "name": "policy-model"}}}
						if _, errUpsert := repoA.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
							t.Fatal(errUpsert)
						}
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					}
					seedConcurrencyAdmissionLifetime(t, repoA, "fp-policy", "home-policy")
					lifetime := concurrencyAdmissionLifetime(t, repoA, "fp-policy", "home-policy")
					var nodes []*home.Runtime
					var adapters []*sessionPolicyAdmissionAdapter
					for _, repo := range []*Repository{repoA, repoB} {
						cfg := &config.Config{Routing: config.RoutingConfig{Strategy: strategy, SessionAffinity: true, SessionAffinityRebindOn: mode}}
						runtime, errRuntime := home.NewRuntime(cfg)
						if errRuntime != nil {
							t.Fatal(errRuntime)
						}
						t.Cleanup(runtime.Stop)
						adapter := &sessionPolicyAdmissionAdapter{RuntimeAdapter: NewRuntimeAdapter(repo, "policy-test"), lifetime: lifetime}
						runtime.SetClusterAdapter(adapter)
						if errReload := runtime.ReloadAuths(ctx); errReload != nil {
							t.Fatal(errReload)
						}
						nodes = append(nodes, runtime)
						adapters = append(adapters, adapter)
					}
					headers := http.Header{"X-Session-Id": {"shared-policy-session"}}
					first, errFirst := nodes[0].DispatchForAPIKey(ctx, "policy-model", headers, "")
					if errFirst != nil || first == nil || first.AuthID != auths[0].ID {
						t.Fatalf("initial=%+v error=%v", first, errFirst)
					}
					blocked := auths[0].Clone()
					state := &coreauth.ModelState{Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), LastError: &coreauth.Error{HTTPStatus: 503}}
					if failure != "transient" {
						state.LastError.HTTPStatus = 429
						if failure == "expired-quota-admission" {
							state.NextRetryAfter = time.Now().Add(-time.Hour)
							adapters[1].rejectID = auths[0].ID
						}
						state.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: state.NextRetryAfter}
					}
					blocked.ModelStates = map[string]*coreauth.ModelState{"policy-model": state}
					if _, errUpsert := repoA.UpsertAuth(ctx, blocked, "test"); errUpsert != nil {
						t.Fatal(errUpsert)
					}
					if errReload := nodes[1].ReloadAuths(ctx); errReload != nil {
						t.Fatal(errReload)
					}
					failover, errFailover := nodes[1].DispatchForAPIKey(ctx, "policy-model", headers, "")
					if mode == "never" {
						var authErr *coreauth.Error
						var admissionErr *ConcurrencyAdmissionError
						valid := errors.As(errFailover, &authErr) && authErr.Code == "session_auth_unavailable"
						valid = valid || (errors.As(errFailover, &admissionErr) && admissionErr.Type == "credential_concurrency_exceeded")
						if !valid || failover != nil {
							t.Fatalf("never failed over: %+v error=%v", failover, errFailover)
						}
					} else if errFailover != nil || failover == nil || failover.AuthID != auths[1].ID {
						t.Fatalf("failover=%+v error=%v", failover, errFailover)
					}
					adapters[1].rejectID = ""
					if errSuccess := nodes[1].CoreManager().MarkResult(ctx, coreauth.Result{AuthID: auths[0].ID, Provider: "codex", Model: "policy-model", Success: true, ObservedAt: time.Now().UTC()}); errSuccess != nil {
						t.Fatal(errSuccess)
					}
					want := auths[0].ID
					if mode == "any" || (mode == "quota" && failure == "quota") {
						want = auths[1].ID
					}
					for _, node := range nodes {
						if errReload := node.ReloadAuths(ctx); errReload != nil {
							t.Fatal(errReload)
						}
						recovered, errRecovered := node.DispatchForAPIKey(ctx, "policy-model", headers, "")
						if errRecovered != nil || recovered == nil || recovered.AuthID != want {
							t.Fatalf("shared recovery=%+v want=%s error=%v", recovered, want, errRecovered)
						}
					}
					if errExpire := repoA.db.Model(&SessionAffinityRecord{}).Where("credential_id <> ?", "").Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; errExpire != nil {
						t.Fatal(errExpire)
					}
					// An expired binding, even in never mode, must permit a fresh choice.
					blocked.Disabled = true
					if _, errUpsert := repoA.UpsertAuth(ctx, blocked, "test"); errUpsert != nil {
						t.Fatal(errUpsert)
					}
					adapters[1].rejectID = auths[0].ID
					if errReload := nodes[1].ReloadAuths(ctx); errReload != nil {
						t.Fatal(errReload)
					}
					fresh, errFresh := nodes[1].DispatchForAPIKey(ctx, "policy-model", headers, "")
					if errFresh != nil || fresh == nil || fresh.AuthID != auths[1].ID {
						t.Fatalf("expired binding prevented fresh selection: %+v %v", fresh, errFresh)
					}
				})
			}
		}
	}
}
