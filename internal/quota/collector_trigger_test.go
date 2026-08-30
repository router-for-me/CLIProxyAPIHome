package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestTriggerCollectionAcceptsEligibleCredentials(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "codex-a", "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	seedCollectorProviderAuth(t, repo, "kimi-a", "kimi", map[string]any{"type": "kimi", "access_token": "probe-secret"})
	seedCollectorProviderAuth(t, repo, "gemini-a", "gemini", map[string]any{"type": "gemini"})

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if strings.Contains(request.URL.String(), "kimi") {
			_, _ = w.Write([]byte(`{"usage":{"used":10,"limit":100,"remaining":90},"limits":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600}}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", CodexUsageURL: server.URL + "/codex", KimiUsageURL: server.URL + "/kimi", Now: func() time.Time { return now }})
	accepted, errTrigger := collector.TriggerCollection(context.Background(), nil, nil)
	if errTrigger != nil {
		t.Fatalf("TriggerCollection() error = %v", errTrigger)
	}
	if accepted != 2 {
		t.Fatalf("TriggerCollection() accepted = %d, want 2 (gemini unsupported)", accepted)
	}
	waitForProbeRequests(t, &requests, 2)
	collector.Wait()
}

func TestCoolingCredentialCollectionUsesActivityAndAllowsForce(t *testing.T) {
	ctx := context.Background()
	repo := newCollectorTestRepository(t)
	const credentialID = "codex-cooling"
	seedCollectorProviderAuth(t, repo, credentialID, "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	now := time.Now().UTC().Add(time.Hour)

	auth, _, errAuth := repo.GetAuth(ctx, credentialID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	cooldownUntil := now.Add(time.Hour)
	auth.Status = coreauth.StatusError
	auth.Unavailable = true
	auth.NextRetryAfter = cooldownUntil
	auth.UpdatedAt = now
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test-cooldown"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	var usageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/usage":
			usageRequests.Add(1)
			_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600}}}`))
		case "/reset":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner:                "home-a",
		CodexUsageURL:        server.URL + "/usage",
		CodexResetCreditsURL: server.URL + "/reset",
		Now:                  func() time.Time { return now },
	})
	collector.collect(ctx)
	if got := usageRequests.Load(); got != 0 {
		t.Fatalf("inactive scheduled quota probe requests = %d, want 0", got)
	}

	usagePayload := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","auth_type":"oauth","auth_index":"codex-cooling"}`, now.Format(time.RFC3339Nano))
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, usagePayload, cluster.UsageRuntimeMetadata{ReceivedAt: now}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	collector.collect(ctx)
	if got := usageRequests.Load(); got != 1 {
		t.Fatalf("active scheduled quota probe requests = %d, want 1", got)
	}
	collector.collect(ctx)
	if got := usageRequests.Load(); got != 1 {
		t.Fatalf("repeated scheduled quota probe requests = %d, want 1", got)
	}

	accepted, errTrigger := collector.TriggerCollection(ctx, map[string]struct{}{credentialID: {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("TriggerCollection() = %d, %v, want 1, nil", accepted, errTrigger)
	}
	collector.Wait()
	if got := usageRequests.Load(); got != 2 {
		t.Fatalf("quota probe requests after force = %d, want 2", got)
	}

	item, errItem := repo.GetQuotaCredential(ctx, credentialID, now)
	if errItem != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errItem)
	}
	if item.CredentialStatus != "cooldown" || item.CollectionStatus != "success" {
		t.Fatalf("credential/collection status = %s/%s, want cooldown/success", item.CredentialStatus, item.CollectionStatus)
	}
}

func TestRefreshBlockedCredentialSkipsScheduledAndTriggerCollection(t *testing.T) {
	ctx := context.Background()
	repo := newCollectorTestRepository(t)
	const credentialID = "codex-refresh-blocked"
	seedCollectorProviderAuth(t, repo, credentialID, "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	now := time.Now().UTC().Add(time.Hour)

	auth, _, errAuth := repo.GetAuth(ctx, credentialID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	auth.Status = coreauth.StatusError
	auth.Unavailable = true
	auth.LastError = &coreauth.Error{Code: "refresh_temporarily_unavailable"}
	auth.UpdatedAt = now
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test-refresh-block"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}

	var usageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		usageRequests.Add(1)
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner:         "home-a",
		CodexUsageURL: server.URL + "/usage",
		Now:           func() time.Time { return now },
	})

	usagePayload := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","auth_type":"oauth","auth_index":"codex-refresh-blocked"}`, now.Format(time.RFC3339Nano))
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, usagePayload, cluster.UsageRuntimeMetadata{ReceivedAt: now}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	collector.collect(ctx)
	if got := usageRequests.Load(); got != 0 {
		t.Fatalf("scheduled quota probe requests for refresh-blocked credential = %d, want 0", got)
	}

	accepted, errTrigger := collector.TriggerCollection(ctx, map[string]struct{}{credentialID: {}}, nil)
	if errTrigger != nil || accepted != 0 {
		t.Fatalf("TriggerCollection() = %d, %v, want 0, nil", accepted, errTrigger)
	}
	collector.Wait()
	if got := usageRequests.Load(); got != 0 {
		t.Fatalf("quota probe requests after trigger = %d, want 0", got)
	}
}

func TestTriggerCollectionFiltersByIDsAndProviders(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "codex-a", "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	seedCollectorProviderAuth(t, repo, "codex-b", "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	seedCollectorProviderAuth(t, repo, "kimi-a", "kimi", map[string]any{"type": "kimi", "access_token": "probe-secret"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600}}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", CodexUsageURL: server.URL, KimiUsageURL: server.URL, Now: func() time.Time { return now }})

	accepted, errTrigger := collector.TriggerCollection(context.Background(), map[string]struct{}{"codex-b": {}}, nil)
	if errTrigger != nil {
		t.Fatalf("TriggerCollection(ids) error = %v", errTrigger)
	}
	if accepted != 1 {
		t.Fatalf("TriggerCollection(ids) accepted = %d, want 1", accepted)
	}

	accepted, errTrigger = collector.TriggerCollection(context.Background(), nil, map[string]struct{}{"kimi": {}})
	if errTrigger != nil {
		t.Fatalf("TriggerCollection(providers) error = %v", errTrigger)
	}
	if accepted != 1 {
		t.Fatalf("TriggerCollection(providers) accepted = %d, want 1", accepted)
	}

	accepted, errTrigger = collector.TriggerCollection(context.Background(), map[string]struct{}{"codex-a": {}}, map[string]struct{}{"kimi": {}})
	if errTrigger != nil {
		t.Fatalf("TriggerCollection(ids+providers) error = %v", errTrigger)
	}
	if accepted != 0 {
		t.Fatalf("TriggerCollection(ids+providers) accepted = %d, want 0 (intersection empty)", accepted)
	}
	collector.Wait()
}

func TestTriggerCollectionForcesFreshSnapshotAndDeduplicates(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "codex-fresh", "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	expiresAt := now.Add(30 * time.Minute)
	if _, errSeed := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: "codex-fresh", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, NextProbeAt: &expiresAt, LastSuccessAt: &now,
	}); errSeed != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSeed)
	}

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProbe()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600}}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", CodexUsageURL: server.URL, Now: func() time.Time { return now }})
	accepted, errTrigger := collector.TriggerCollection(context.Background(), map[string]struct{}{"codex-fresh": {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("first TriggerCollection() = %d, %v, want 1, nil", accepted, errTrigger)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("forced quota probe did not start")
	}
	accepted, errTrigger = collector.TriggerCollection(context.Background(), map[string]struct{}{"codex-fresh": {}}, nil)
	if errTrigger != nil || accepted != 0 {
		t.Fatalf("duplicate TriggerCollection() = %d, %v, want 0, nil", accepted, errTrigger)
	}
	releaseProbe()
	collector.Wait()
	if requests.Load() != 1 {
		t.Fatalf("probe requests = %d, want 1", requests.Load())
	}
}

func TestTriggerCollectionSharesConcurrencyAcrossRounds(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	for _, id := range []string{"codex-a", "codex-b", "codex-c", "codex-d"} {
		seedCollectorProviderAuth(t, repo, id, "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbes := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProbes()
	var requests atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		<-release
		active.Add(-1)
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600}}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", ProviderConcurrency: 1, CodexUsageURL: server.URL, Now: func() time.Time { return now }})
	acceptedFirst, errFirst := collector.TriggerCollection(context.Background(), map[string]struct{}{"codex-a": {}, "codex-b": {}}, nil)
	acceptedSecond, errSecond := collector.TriggerCollection(context.Background(), map[string]struct{}{"codex-c": {}, "codex-d": {}}, nil)
	if errFirst != nil || errSecond != nil || acceptedFirst != 2 || acceptedSecond != 2 {
		t.Fatalf("TriggerCollection() results = (%d, %v), (%d, %v)", acceptedFirst, errFirst, acceptedSecond, errSecond)
	}
	waitForProbeRequests(t, &requests, 1)
	time.Sleep(50 * time.Millisecond)
	if requests.Load() != 1 || maximum.Load() > 1 {
		t.Fatalf("concurrency before release: requests=%d maximum=%d", requests.Load(), maximum.Load())
	}
	releaseProbes()
	collector.Wait()
	if requests.Load() != 4 || maximum.Load() > 1 {
		t.Fatalf("concurrency after completion: requests=%d maximum=%d", requests.Load(), maximum.Load())
	}
}

func waitForProbeRequests(t *testing.T, requests *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if requests.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("probe requests = %d, want at least %d", requests.Load(), want)
}
