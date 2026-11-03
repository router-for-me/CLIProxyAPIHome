package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestSessionAffinityDatabaseConditionalWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.db")
	nodeA := openRefreshTestRepositoryAt(t, path, true)
	nodeB := openRefreshTestRepositoryAt(t, path, false)
	key := strings.Repeat("a", 64)
	commit := coreauth.SessionAffinityCommit{Key: key, AuthID: "cred-a", TTL: time.Hour}
	if swapped, errSwap := nodeA.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("create=%t err=%v", swapped, errSwap)
	}
	if swapped, errSwap := nodeB.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("competing create=%t err=%v", swapped, errSwap)
	}
	original, errLoad := nodeB.LoadSessionBinding(t.Context(), key)
	if errLoad != nil || original.AuthID != "cred-a" || original.Revision == "" {
		t.Fatalf("shared load=%#v err=%v", original, errLoad)
	}
	commit.ExpectedRevision = original.Revision
	// Renewals retain the revision, so concurrent requests using an unchanged
	// credential need not serialize through repeated selection retries.
	for _, repo := range []*Repository{nodeA, nodeB} {
		if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
			t.Fatalf("renew=%t err=%v", swapped, errSwap)
		}
	}
	commit.AuthID = "cred-b"
	if swapped, errSwap := nodeB.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("rebind=%t err=%v", swapped, errSwap)
	}
	commit.AuthID = "cred-a"
	if swapped, errSwap := nodeA.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("stale renewal=%t err=%v", swapped, errSwap)
	}

	// Expire explicitly rather than waiting on wall-clock timer resolution.
	if errExpire := nodeA.db.Model(&SessionAffinityRecord{}).Where("key = ?", key).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	expired, errExpired := nodeB.LoadSessionBinding(t.Context(), key)
	if errExpired != nil {
		t.Fatal(errExpired)
	}
	commit.ExpectedRevision = expired.Revision
	commit.ExpiredAt = expired.ExpiresAt
	commit.AuthID = expired.AuthID
	// Model an in-flight renewal that extends the lease without changing its
	// credential revision. An expired snapshot must not overwrite that lease.
	if errRenew := nodeA.db.Model(&SessionAffinityRecord{}).Where("key = ?", key).Update("expires_at", time.Now().UTC().Add(time.Hour)).Error; errRenew != nil {
		t.Fatal(errRenew)
	}
	if swapped, errSwap := nodeB.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("stale expiration-based claim=%t err=%v", swapped, errSwap)
	}
	if errExpire := nodeA.db.Model(&SessionAffinityRecord{}).Where("key = ?", key).Update("expires_at", expired.ExpiresAt).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	if swapped, errSwap := nodeA.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("expired same-credential claim=%t err=%v", swapped, errSwap)
	}
	commit.AuthID = "cred-racing"
	if swapped, errSwap := nodeB.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("stale expired rebind=%t err=%v", swapped, errSwap)
	}
	if errExpire := nodeA.db.Model(&SessionAffinityRecord{}).Where("key = ?", key).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	if purged, errPurge := nodeA.PurgeExpiredSessionBindings(t.Context(), time.Now(), 1); errPurge != nil || purged != 1 {
		t.Fatalf("purged=%d err=%v", purged, errPurge)
	}
	commit.ExpectedRevision = ""
	commit.ExpiredAt = time.Time{}
	if swapped, errSwap := nodeB.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("recreate=%t err=%v", swapped, errSwap)
	}
	commit.ExpectedRevision = original.Revision
	commit.AuthID = "cred-stale"
	if swapped, errSwap := nodeA.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || swapped {
		t.Fatalf("ABA write=%t err=%v", swapped, errSwap)
	}
	if purged, errPurge := nodeB.PurgeExpiredSessionBindings(t.Context(), time.Now(), 100); errPurge != nil || purged != 0 {
		t.Fatalf("active binding purged=%d err=%v", purged, errPurge)
	}
}

func TestSessionAffinityDatabaseConcurrentFirstBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.db")
	nodeA := openRefreshTestRepositoryAt(t, path, true)
	nodeB := openRefreshTestRepositoryAt(t, path, false)
	testSessionAffinityConcurrentSelectors(t, nodeA, nodeB)
}

func TestPostgresSessionAffinityConcurrentFirstBinding(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	testSessionAffinityConcurrentSelectors(t, repo, NewRepository(repo.db))
}

func testSessionAffinityConcurrentSelectors(t *testing.T, nodeA, nodeB *Repository) {
	t.Helper()
	ctx, cancelCtx := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelCtx()
	auths := []*coreauth.Auth{{ID: "cred-a", Status: coreauth.StatusActive}, {ID: "cred-b", Status: coreauth.StatusActive}}
	selectors := []*coreauth.SessionAffinitySelector{
		coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Store: nodeA}),
		coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Store: nodeB}),
	}
	// Advance only node B's cursor so independent first selections disagree.
	if _, errPick := selectors[1].Pick(ctx, "openai", "gpt", coreauth.Options{}, auths); errPick != nil {
		t.Fatal(errPick)
	}
	start := make(chan struct{})
	results := make(chan string, 16)
	var wait sync.WaitGroup
	for index := range 16 {
		wait.Go(func() {
			<-start
			picked, errPick := selectors[index%2].Pick(ctx, "openai", "gpt", coreauth.Options{Headers: http.Header{"X-Session-ID": {"shared"}}}, auths)
			if errPick != nil || picked == nil {
				t.Errorf("pick=%v err=%v", picked, errPick)
				return
			}
			results <- picked.ID
		})
	}
	close(start)
	wait.Wait()
	close(results)
	var want string
	count := 0
	for id := range results {
		count++
		if want == "" {
			want = id
		}
		if id != want {
			t.Errorf("shared session split between %s and %s", want, id)
		}
	}
	if count != 16 {
		t.Fatalf("successful requests=%d, want 16", count)
	}
}

func TestSessionAffinityAdmissionAtomicCommit(t *testing.T) {
	testSessionAffinityAdmissionAtomicCommit(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresSessionAffinityAdmissionAtomicCommit(t *testing.T) {
	testSessionAffinityAdmissionAtomicCommit(t, newPostgresQuiescenceRepository(t))
}

func testSessionAffinityAdmissionAtomicCommit(t *testing.T, repo *Repository) {
	t.Helper()
	seedConcurrencyAdmissionLifetime(t, repo, "fp", "home")
	seedConcurrencyAdmissionAuth(t, repo, "cred-a")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-a", int64Pointer(1), nil)
	request := ConcurrencyAdmissionRequest{
		CredentialID: "cred-a", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp", "home"), ProtocolVersion: 1,
		SessionAffinity: &coreauth.SessionAffinityCommit{Key: strings.Repeat("b", 64), AuthID: "cred-a", TTL: time.Hour},
	}
	if result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); errAdmit != nil || !result.Accounted {
		t.Fatalf("first admission=%#v err=%v", result, errAdmit)
	}
	original, errLoad := repo.LoadSessionBinding(t.Context(), request.SessionAffinity.Key)
	if errLoad != nil || original.AuthID != "cred-a" {
		t.Fatalf("admitted binding=%#v err=%v", original, errLoad)
	}
	// A conflict must not consume an additional concurrency slot.
	if result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); !errors.Is(errAdmit, coreauth.ErrSessionAffinityConflict) || result.Accounted {
		t.Fatalf("conflicting admission=%#v err=%v", result, errAdmit)
	}
	// Saturation must roll back both new bindings and renewals.
	request.SessionAffinity.ExpectedRevision = original.Revision
	request.SessionAffinity.TTL = 2 * time.Hour
	if result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); !IsConcurrencySaturated(errAdmit) || result.Accounted {
		t.Fatalf("saturated renewal=%#v err=%v", result, errAdmit)
	}
	current, errCurrent := repo.LoadSessionBinding(t.Context(), request.SessionAffinity.Key)
	if errCurrent != nil || current.Revision != original.Revision || !current.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("failed renewal changed binding: %#v, original=%#v err=%v", current, original, errCurrent)
	}
	request.SessionAffinity = &coreauth.SessionAffinityCommit{Key: strings.Repeat("c", 64), AuthID: "cred-a", TTL: time.Hour}
	if _, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); !IsConcurrencySaturated(errAdmit) {
		t.Fatalf("saturated creation err=%v", errAdmit)
	}
	absent, errAbsent := repo.LoadSessionBinding(t.Context(), request.SessionAffinity.Key)
	if errAbsent != nil || absent.Revision != "" {
		t.Fatalf("failed creation left binding: %#v err=%v", absent, errAbsent)
	}
	if got := countConcurrencyAdmissionCounters(t, repo); got != 1 {
		t.Fatalf("active count=%d, want 1", got)
	}
	// An unlimited credential still commits its session despite not counting.
	seedConcurrencyAdmissionAuth(t, repo, "cred-free")
	request.CredentialID = "cred-free"
	request.SessionAffinity.AuthID = "cred-free"
	if result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); errAdmit != nil || result.Accounted {
		t.Fatalf("unlimited admission=%#v err=%v", result, errAdmit)
	}
	free, errFree := repo.LoadSessionBinding(t.Context(), request.SessionAffinity.Key)
	if errFree != nil || free.AuthID != "cred-free" {
		t.Fatalf("unlimited binding=%#v err=%v", free, errFree)
	}
}

func newSessionAffinityRuntime(t *testing.T, repo *Repository, node string) *home.Runtime {
	t.Helper()
	cfg := &config.Config{Routing: config.RoutingConfig{SessionAffinity: true}}
	runtime, errRuntime := home.NewRuntime(cfg)
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(runtime.Stop)
	runtime.SetClusterAdapter(NewRuntimeAdapter(repo, node))
	if errLoad := runtime.ReloadAuths(t.Context()); errLoad != nil {
		t.Fatal(errLoad)
	}
	return runtime
}

func TestSessionAffinityRuntimeAcrossNodesAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.db")
	repoA := openRefreshTestRepositoryAt(t, path, true)
	repoB := openRefreshTestRepositoryAt(t, path, false)
	for index := range 2 {
		id := fmt.Sprintf("11111111-1111-4111-8111-%012d", index+1)
		auth := &coreauth.Auth{
			ID: id, Index: id, Provider: "openai", Status: coreauth.StatusActive,
			Attributes: map[string]string{"api_key": "test-only"},
			Metadata:   map[string]any{"home_config_models": []map[string]any{{"id": "affinity-model", "name": "affinity-model", "user_defined": true}}},
		}
		if _, errUpsert := repoA.UpsertAuth(t.Context(), auth, "upsert"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	nodeA := newSessionAffinityRuntime(t, repoA, "a")
	nodeB := newSessionAffinityRuntime(t, repoB, "b")
	dispatch := func(runtime *home.Runtime, session, parent string) string {
		t.Helper()
		result, errDispatch := runtime.DispatchForAPIKey(t.Context(), "affinity-model", http.Header{"X-Session-ID": {session}, "X-Parent-Session-ID": {parent}}, "")
		if errDispatch != nil || result == nil {
			t.Fatalf("dispatch=%#v err=%v", result, errDispatch)
		}
		return result.AuthID
	}
	dispatch(nodeB, "warmup", "")
	want := dispatch(nodeA, "shared", "")
	for _, step := range []struct {
		node    *home.Runtime
		session string
		parent  string
	}{{nodeB, "shared", ""}, {nodeB, "child", "shared"}, {nodeA, "child", ""}} {
		if got := dispatch(step.node, step.session, step.parent); got != want {
			t.Fatalf("session=%s got=%s want=%s", step.session, got, want)
		}
	}
	// Seed a binding to the other credential so reinitializing round-robin
	// cannot accidentally make a discarded binding appear to survive reload.
	other := "11111111-1111-4111-8111-000000000002"
	if want == other {
		other = "11111111-1111-4111-8111-000000000001"
	}
	if _, errDispatch := nodeB.DispatchForAPIKeyWithConcurrency(t.Context(), "affinity-model", http.Header{"X-Session-ID": {"shared"}}, "", "", nil, other, home.DispatchConcurrencyContext{}); errDispatch != nil {
		t.Fatal(errDispatch)
	}
	cfg := *nodeA.Config()
	cfg.Routing.Strategy = "fill-first"
	if errApply := nodeA.ApplyConfigFromCluster(t.Context(), &cfg); errApply != nil {
		t.Fatal(errApply)
	}
	if got := dispatch(nodeA, "shared", ""); got != other {
		t.Fatalf("binding lost on config reload: got=%s want=%s", got, other)
	}
	// A fresh Home runtime uses the same database binding after restart.
	restarted := newSessionAffinityRuntime(t, repoA, "restarted")
	if got := dispatch(restarted, "shared", ""); got != other {
		t.Fatalf("binding lost on restart: got=%s want=%s", got, other)
	}

	// Response preparation errors must not establish a binding.
	failure := errors.New("response preparation failed")
	_, errDispatch := nodeA.DispatchForAPIKeyWithConcurrency(t.Context(), "affinity-model", http.Header{"X-Session-ID": {"failed"}}, "", "", nil, "", home.DispatchConcurrencyContext{
		PrepareResponse: func(*home.DispatchResult) ([]byte, []byte, error) { return nil, nil, failure },
	})
	if !errors.Is(errDispatch, failure) {
		t.Fatalf("dispatch preparation error=%v", errDispatch)
	}
	var count int64
	if errCount := repoA.db.Model(&SessionAffinityRecord{}).Count(&count).Error; errCount != nil || count != 3 {
		t.Fatalf("bindings=%d err=%v, want only warmup, shared and child", count, errCount)
	}

	seedConcurrencyAdmissionLifetime(t, repoA, "fp", "a")
	lifetime := concurrencyAdmissionLifetime(t, repoA, "fp", "a")
	credA := "11111111-1111-4111-8111-000000000001"
	credB := "11111111-1111-4111-8111-000000000002"
	for _, id := range []string{credA, credB} {
		seedConcurrencyAdmissionPolicy(t, repoA, id, int64Pointer(1), nil)
	}
	var admittedBindingKey string
	nodeA.SetConcurrencyAdmitter(home.ConcurrencyAdmitterFunc(func(ctx context.Context, request home.ConcurrencyAdmissionRequest) (home.ConcurrencyAdmissionResult, error) {
		result, errAdmit := repoA.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
			CredentialID: request.CredentialID, Model: request.Model, Lifetime: lifetime,
			ProtocolVersion: 1, SessionAffinity: request.SessionAffinity,
		})
		if errAdmit == nil && request.SessionAffinity != nil {
			admittedBindingKey = request.SessionAffinity.Key
		}
		return home.ConcurrencyAdmissionResult{CredentialID: result.CredentialID, Model: result.Model, Accounted: result.Accounted}, errAdmit
	}))
	// A competing Home wins after selection but before admission. The loser
	// must retry with the winner, without admitting its stale credential.
	var raced bool
	result, errRace := nodeA.DispatchForAPIKeyWithConcurrency(t.Context(), "affinity-model", http.Header{"X-Session-ID": {"racing"}}, "", "", nil, "", home.DispatchConcurrencyContext{
		PrepareResponse: func(*home.DispatchResult) ([]byte, []byte, error) {
			if !raced {
				raced = true
				_, errCompete := nodeB.DispatchForAPIKeyWithConcurrency(t.Context(), "affinity-model", http.Header{"X-Session-ID": {"racing"}}, "", "", nil, credB, home.DispatchConcurrencyContext{})
				return nil, nil, errCompete
			}
			return nil, nil, nil
		},
	})
	if errRace != nil || result == nil || result.AuthID != credB || !result.Concurrency.Accounted {
		t.Fatalf("racing dispatch=%#v err=%v", result, errRace)
	}
	if got := countConcurrencyAdmissionCounters(t, repoA); got != 1 {
		t.Fatalf("conflict consumed slots: count=%d", got)
	}
	// The bound credential is now full. Only a successful admission on the
	// alternative may rebind the session.
	if got := dispatch(nodeA, "racing", ""); got != credA {
		t.Fatalf("saturation failover=%s, want %s", got, credA)
	}
	var before SessionAffinityRecord
	if errFind := repoA.db.Where("key = ?", admittedBindingKey).Take(&before).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if before.CredentialID != credA {
		t.Fatalf("admitted binding=%#v, want credential %s", before, credA)
	}
	if result, errDispatch := nodeA.DispatchForAPIKey(t.Context(), "affinity-model", http.Header{"X-Session-ID": {"racing"}}, ""); errDispatch == nil || result != nil {
		t.Fatalf("all-saturated dispatch=%#v err=%v", result, errDispatch)
	}
	after, errAfter := repoA.LoadSessionBinding(t.Context(), before.Key)
	if errAfter != nil || after.AuthID != before.CredentialID || after.Revision != before.Revision || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("failed admission changed binding: before=%#v after=%#v err=%v", before, after, errAfter)
	}
	if got := countConcurrencyAdmissionCounters(t, repoA); got != 2 {
		t.Fatalf("saturation consumed slots: count=%d", got)
	}
}
