package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCollectorPersistsCodexActiveProbeSnapshot(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-probe", map[string]any{"type": "codex", "access_token": "probe-secret", "account_id": "acct-123"})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Authorization") != "Bearer probe-secret" || request.Header.Get("Chatgpt-Account-Id") != "acct-123" {
			t.Errorf("unexpected probe headers: %#v", request.Header)
		}
		w.Header().Set("X-Request-ID", "probe-request-1")
		_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":82,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1784160600},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":6000,"reset_at":1784166000}}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", HomeID: "home-a", CodexUsageURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	if requests.Load() != 1 {
		t.Fatalf("probe requests = %d, want 1", requests.Load())
	}
	item, errGet := repo.GetQuotaCredential(context.Background(), "codex-probe", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "low" || item.CollectionStatus != "success" || item.Source == nil || *item.Source != "active_probe" {
		t.Fatalf("unexpected probe state: %+v", item)
	}
	if len(item.Windows) != 2 || item.Windows[0].PeriodUnit != "hour" || item.Windows[1].PeriodUnit != "week" {
		t.Fatalf("unexpected probe windows: %+v", item.Windows)
	}
	if item.Runtime == nil || item.Runtime.HomeID != "home-a" {
		t.Fatalf("unexpected probe runtime: %+v", item.Runtime)
	}
}

func TestCollectorFailureRetainsLastKnownWindows(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-failed-probe", map[string]any{"type": "codex", "access_token": "probe-secret"})
	oldObservedAt := now.Add(-time.Hour)
	oldExpiresAt := now.Add(-time.Minute)
	period := float64(5)
	remaining := 0.5
	_, errSeed := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: "codex-failed-probe", QuotaStatus: "healthy", CollectionStatus: "success", Source: "response_header",
		ObservedAt: &oldObservedAt, ExpiresAt: &oldExpiresAt, LastSuccessAt: &oldObservedAt, NextProbeAt: &oldExpiresAt,
		ParserVersion: cluster.QuotaSnapshotVersion("codex"), CollectorVersion: cluster.QuotaSnapshotVersion("codex"), ReplaceWindows: true,
		Windows: []cluster.QuotaWindow{{ID: "codex-5-hour", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: "hour", PeriodValue: &period, Source: "response_header", ObservedAt: oldObservedAt}},
	})
	if errSeed != nil {
		t.Fatalf("seed snapshot: %v", errSeed)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "rate-limited-request")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"secret should not be persisted"}}`))
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "home-a", CodexUsageURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "codex-failed-probe", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "healthy" || item.CollectionStatus != "failed" || len(item.Windows) != 1 {
		t.Fatalf("failure did not retain last snapshot: %+v", item)
	}
	if item.Error == nil || item.Error.Code != "UPSTREAM_RATE_LIMITED" || item.Error.Message == "" || item.Error.RequestID == nil || *item.Error.RequestID != "rate-limited-request" {
		t.Fatalf("unexpected failure metadata: %+v", item.Error)
	}
	if item.Error.Message == "secret should not be persisted" {
		t.Fatalf("upstream body leaked into failure metadata: %+v", item.Error)
	}
}

func TestCollectorPersistenceFailureClearsCollectingState(t *testing.T) {
	repo, db := newCollectorTestRepositoryAndDB(t)
	now := time.Date(2026, 7, 16, 5, 30, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-persist-failure", map[string]any{"type": "codex", "access_token": "probe-secret"})
	if errTrigger := db.Exec(`CREATE TRIGGER fail_quota_window_insert BEFORE INSERT ON quota_window BEGIN SELECT RAISE(ABORT, 'forced quota window failure'); END`).Error; errTrigger != nil {
		t.Fatalf("create failure trigger: %v", errTrigger)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600}}}`))
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "home-a", CodexUsageURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	item, errGet := repo.GetQuotaCredential(context.Background(), "codex-persist-failure", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "failed" || item.QuotaStatus != "error" || item.Error == nil || item.Error.Code != "SNAPSHOT_PERSIST_FAILED" {
		t.Fatalf("persistence failure state = %+v", item)
	}
}

func TestCollectorSupportsClaudeAntigravityKimiAndXAI(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "claude-probe", "anthropic", map[string]any{"type": "claude", "access_token": "claude-secret"})
	seedCollectorProviderAuth(t, repo, "antigravity-probe", "antigravity", map[string]any{"type": "antigravity", "access_token": "ag-secret", "project_id": "project-123"})
	seedCollectorProviderAuth(t, repo, "kimi-probe", "kimi", map[string]any{"type": "kimi", "access_token": "kimi-secret"})
	seedCollectorProviderAuth(t, repo, "xai-probe", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	var antigravityAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/claude/usage":
			if request.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || request.Header.Get("Authorization") != "Bearer claude-secret" {
				t.Errorf("unexpected claude headers: %#v", request.Header)
			}
			_, _ = w.Write([]byte(`{"five_hour":{"utilization":36,"resets_at":"2026-07-16T10:00:00Z"},"seven_day":{"utilization":72,"resets_at":"2026-07-20T00:00:00Z"},"extra_usage":{"is_enabled":true,"monthly_limit":1000,"used_credits":250,"utilization":25}}`))
		case "/claude/profile":
			_, _ = w.Write([]byte(`{"account":{"email":"user@example.com"},"organization":{"organization_type":"claude_team"}}`))
		case "/antigravity/fail":
			antigravityAttempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/antigravity/ok":
			antigravityAttempts.Add(1)
			var body map[string]string
			if errDecode := json.NewDecoder(request.Body).Decode(&body); errDecode != nil || body["project"] != "project-123" {
				t.Errorf("unexpected antigravity body: %#v error=%v", body, errDecode)
			}
			_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini models","buckets":[{"bucketId":"gemini-5h","remainingFraction":0.4,"resetTime":"2026-07-16T11:00:00Z"},{"bucketId":"gemini-weekly","remainingFraction":0.7,"resetTime":"2026-07-20T00:00:00Z"}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","remainingFraction":0.5,"resetTime":"2026-07-16T11:30:00Z"},{"bucketId":"3p-weekly","remainingFraction":0.8,"resetTime":"2026-07-20T00:00:00Z"}]}],"models":{"chat_20706":{"quotaInfo":{"remainingFraction":0.01}}}}`))
		case "/kimi":
			_, _ = w.Write([]byte(`{"user":{"userId":"user-1","region":"REGION_OVERSEA"},"usage":{"limit":"100","used":"44","remaining":"56","resetTime":"2026-07-21T13:09:11Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","used":"10","remaining":"90","resetTime":"2026-07-19T14:09:11Z"}}],"totalQuota":{"limit":"100","remaining":"99"}}`))
		case "/xai":
			_, _ = w.Write([]byte(`{"config":{"monthlyLimit":{"val":20000},"used":{"val":167},"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{
		Owner: "home-a", HomeID: "home-a", Now: func() time.Time { return now },
		ClaudeUsageURL: server.URL + "/claude/usage", ClaudeProfileURL: server.URL + "/claude/profile",
		AntigravityURLs: []string{server.URL + "/antigravity/fail", server.URL + "/antigravity/ok"},
		KimiUsageURL:    server.URL + "/kimi", XAIBillingURL: server.URL + "/xai",
	})
	collector.collect(context.Background())
	if antigravityAttempts.Load() != 2 {
		t.Fatalf("antigravity attempts = %d, want 2", antigravityAttempts.Load())
	}

	tests := []struct {
		id       string
		provider string
		status   string
		windows  int
		assert   func(*testing.T, *cluster.QuotaCredentialSnapshot)
	}{
		{id: "claude-probe", provider: "claude", status: "healthy", windows: 3},
		{id: "antigravity-probe", provider: "antigravity", status: "healthy", windows: 4, assert: func(t *testing.T, item *cluster.QuotaCredentialSnapshot) {
			if len(item.PrimaryWindows) != 2 || item.PrimaryWindows[0].ScopeID == nil || item.PrimaryWindows[1].ScopeID == nil || *item.PrimaryWindows[0].ScopeID != "gemini" || *item.PrimaryWindows[1].ScopeID != "third-party" {
				t.Fatalf("unexpected antigravity primary windows: %+v", item.PrimaryWindows)
			}
		}},
		{id: "kimi-probe", provider: "kimi", status: "healthy", windows: 2, assert: func(t *testing.T, item *cluster.QuotaCredentialSnapshot) {
			var summary, limit *cluster.QuotaWindow
			for index := range item.Windows {
				switch item.Windows[index].ID {
				case "kimi-usage":
					summary = &item.Windows[index]
				case "kimi-limit-1":
					limit = &item.Windows[index]
				}
			}
			if summary == nil || limit == nil {
				t.Fatalf("missing Kimi windows: %+v", item.Windows)
			}
			if summary.ID != "kimi-usage" || summary.Used == nil || *summary.Used != 44 || summary.Limit == nil || *summary.Limit != 100 || summary.Remaining == nil || *summary.Remaining != 56 {
				t.Fatalf("unexpected kimi summary window: %+v", summary)
			}
			if summary.ResetAt == nil || !summary.ResetAt.Equal(time.Date(2026, 7, 21, 13, 9, 11, 0, time.UTC)) {
				t.Fatalf("unexpected kimi summary reset time: %+v", summary.ResetAt)
			}
			if limit.ID != "kimi-limit-1" || limit.Used == nil || *limit.Used != 10 || limit.Limit == nil || *limit.Limit != 100 || limit.Remaining == nil || *limit.Remaining != 90 {
				t.Fatalf("unexpected kimi limit window: %+v", limit)
			}
			if limit.PeriodUnit != "minute" || limit.PeriodValue == nil || *limit.PeriodValue != 300 || limit.WindowSeconds == nil || *limit.WindowSeconds != 18000 {
				t.Fatalf("unexpected kimi limit period: %+v", limit)
			}
			if limit.ResetAt == nil || !limit.ResetAt.Equal(time.Date(2026, 7, 19, 14, 9, 11, 0, time.UTC)) {
				t.Fatalf("unexpected kimi limit reset time: %+v", limit.ResetAt)
			}
		}},
		{id: "xai-probe", provider: "xai", status: "healthy", windows: 1, assert: func(t *testing.T, item *cluster.QuotaCredentialSnapshot) {
			window := item.Windows[0]
			if window.Unit != "currency" || window.Currency == nil || *window.Currency != "USD" || window.Limit == nil || *window.Limit != 200 {
				t.Fatalf("unexpected xAI window: %+v", window)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			item, errGet := repo.GetQuotaCredential(context.Background(), test.id, now.Add(time.Minute))
			if errGet != nil {
				t.Fatalf("GetQuotaCredential() error = %v", errGet)
			}
			if item.Provider != test.provider || item.QuotaStatus != test.status || item.CollectionStatus != "success" || len(item.Windows) != test.windows {
				t.Fatalf("unexpected snapshot: %+v", item)
			}
			if test.assert != nil {
				test.assert(t, item)
			}
		})
	}
}

func TestClaudeProfileFailureProducesPartialSnapshot(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "claude-partial", "claude", map[string]any{"type": "claude", "access_token": "claude-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/usage" {
			_, _ = w.Write([]byte(`{"five_hour":{"utilization":90,"resets_at":"2026-07-16T10:00:00Z"}}`))
			return
		}
		w.Header().Set("X-Request-ID", "profile-failed")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "home-a", Now: func() time.Time { return now }, ClaudeUsageURL: server.URL + "/usage", ClaudeProfileURL: server.URL + "/profile"})
	collector.collect(context.Background())
	item, errGet := repo.GetQuotaCredential(context.Background(), "claude-partial", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "partial" || item.QuotaStatus != "low" || len(item.Windows) != 1 || item.Error == nil || item.Error.Code != "UPSTREAM_RATE_LIMITED" {
		t.Fatalf("unexpected Claude partial snapshot: %+v", item)
	}
}

func TestAntigravityMissingProjectFailsWithoutUpstreamRequest(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "antigravity-no-project", "antigravity", map[string]any{"type": "antigravity", "access_token": "ag-secret"})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "home-a", Now: func() time.Time { return now }, AntigravityURLs: []string{server.URL}})
	collector.collect(context.Background())
	if requests.Load() != 0 {
		t.Fatalf("upstream requests = %d, want 0", requests.Load())
	}
	item, errGet := repo.GetQuotaCredential(context.Background(), "antigravity-no-project", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "error" || item.CollectionStatus != "failed" || item.Error == nil || item.Error.Code != "PROJECT_ID_UNAVAILABLE" || item.Error.Retryable {
		t.Fatalf("unexpected missing project state: %+v", item)
	}
}

func TestQuotaProbeEligibilitySkipsDisabledCooldownAndUnavailableCredentials(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		auth *coreauth.Auth
		want bool
	}{
		{name: "active", auth: &coreauth.Auth{ID: "active", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex"}}, want: true},
		{name: "disabled", auth: &coreauth.Auth{ID: "disabled", Provider: "codex", Status: coreauth.StatusDisabled, Disabled: true}, want: false},
		{name: "cooldown", auth: &coreauth.Auth{ID: "cooldown", Provider: "codex", Status: coreauth.StatusActive, NextRetryAfter: now.Add(time.Minute)}, want: false},
		{name: "expired cooldown", auth: &coreauth.Auth{ID: "expired-cooldown", Provider: "codex", Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(-time.Minute)}, want: true},
		{name: "unavailable without future cooldown", auth: &coreauth.Auth{ID: "unavailable", Provider: "codex", Status: coreauth.StatusError, Unavailable: true}, want: true},
		{name: "provider api key", auth: &coreauth.Auth{ID: "api-key", Provider: "xai", Status: coreauth.StatusActive, Attributes: map[string]string{"source": "config:xai", "api_key": "secret"}}, want: false},
		{name: "explicit api key wins legacy oauth metadata", auth: &coreauth.Auth{ID: "explicit-api-key", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "apikey", "api_key": "secret"}, Metadata: map[string]any{"type": "codex"}}, want: false},
		{name: "explicit oauth wins api key shape", auth: &coreauth.Auth{ID: "explicit-oauth", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth", "api_key": "secret"}, Metadata: map[string]any{"type": "codex"}}, want: true},
		{name: "unsupported provider", auth: &coreauth.Auth{ID: "vertex", Provider: "vertex", Status: coreauth.StatusActive}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := quotaProbeEligible(test.auth, now); got != test.want {
				t.Fatalf("quotaProbeEligible() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestQuotaHTTPProbeErrorClassification(t *testing.T) {
	for _, test := range []struct {
		status    int
		code      string
		retryable bool
	}{
		{status: http.StatusUnauthorized, code: "UPSTREAM_AUTH_REJECTED", retryable: false},
		{status: http.StatusForbidden, code: "UPSTREAM_AUTH_REJECTED", retryable: false},
		{status: http.StatusTooManyRequests, code: "UPSTREAM_RATE_LIMITED", retryable: true},
		{status: http.StatusInternalServerError, code: "UPSTREAM_UNAVAILABLE", retryable: true},
		{status: http.StatusBadRequest, code: "UPSTREAM_UNAVAILABLE", retryable: false},
	} {
		failure := quotaHTTPProbeError(test.status, "request-id")
		if failure.code != test.code || failure.retryable != test.retryable || failure.statusCode != test.status || failure.requestID != "request-id" {
			t.Fatalf("quotaHTTPProbeError(%d) = %+v", test.status, failure)
		}
	}
}

func TestQuotaRetryAfterSupportsSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{value: "120", want: 2 * time.Minute},
		{value: now.Add(3 * time.Minute).Format(http.TimeFormat), want: 3 * time.Minute},
		{value: "invalid", want: 0},
	} {
		headers := http.Header{"Retry-After": []string{test.value}}
		if got := quotaRetryAfter(headers, now); got != test.want {
			t.Fatalf("quotaRetryAfter(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestCollectorResolvesFreshAuthBeforeProbe(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 9, 30, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-refresh", map[string]any{"type": "codex", "access_token": "stale-token"})
	var resolved atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Errorf("Authorization = %q, want refreshed token", request.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600}}}`))
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{
		Owner: "home-a", CodexUsageURL: server.URL, Now: func() time.Time { return now },
		ResolveAuth: func(_ context.Context, candidate *coreauth.Auth) (*coreauth.Auth, error) {
			resolved.Add(1)
			fresh := candidate.Clone()
			fresh.Metadata["access_token"] = "fresh-token"
			return fresh, nil
		},
	})
	collector.collect(context.Background())
	if resolved.Load() != 1 {
		t.Fatalf("ResolveAuth calls = %d, want 1", resolved.Load())
	}
}

func TestCollectorSkipsStaleCandidateWhenDBCredentialBecameAPIKey(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 9, 45, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-became-api-key", map[string]any{"type": "codex", "access_token": "old-token"})
	candidate, _, errCandidate := repo.GetAuth(context.Background(), "codex-became-api-key")
	if errCandidate != nil {
		t.Fatalf("GetAuth(candidate) error = %v", errCandidate)
	}
	apiKeyAuth := &coreauth.Auth{
		ID: "codex-became-api-key", Index: "codex-became-api-key", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "apikey", "source": "config:codex", "api_key": "secret"}, Metadata: map[string]any{"type": "codex"}, CreatedAt: now, UpdatedAt: now,
	}
	if _, errUpsert := repo.UpsertAuth(context.Background(), apiKeyAuth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth(API key) error = %v", errUpsert)
	}
	var resolved atomic.Int32
	collector := NewCollector(repo, Options{
		Owner: "home-a", Now: func() time.Time { return now },
		ResolveAuth: func(ctx context.Context, _ *coreauth.Auth) (*coreauth.Auth, error) {
			resolved.Add(1)
			current, _, errGet := repo.GetAuth(ctx, apiKeyAuth.ID)
			return current, errGet
		},
	})
	collector.collectCredential(context.Background(), candidate, false)
	if resolved.Load() != 0 {
		t.Fatalf("ResolveAuth calls = %d, want 0", resolved.Load())
	}
	item, errGet := repo.GetQuotaCredential(context.Background(), apiKeyAuth.ID, now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CredentialType != "provider_api_key" || item.QuotaStatus != "unsupported" || item.CollectionStatus != "unsupported" || item.ObservedAt != nil {
		t.Fatalf("ineligible resolved credential retained probe state: %+v", item)
	}
}

func TestCollectorReadsDynamicGlobalProxyForEachClient(t *testing.T) {
	firstProxyRequests := atomic.Int32{}
	secondProxyRequests := atomic.Int32{}
	proxyHandler := func(counter *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			counter.Add(1)
			_, _ = w.Write([]byte("ok"))
		}
	}
	firstProxy := httptest.NewServer(proxyHandler(&firstProxyRequests))
	defer firstProxy.Close()
	secondProxy := httptest.NewServer(proxyHandler(&secondProxyRequests))
	defer secondProxy.Close()
	currentProxy := firstProxy.URL
	collector := NewCollector(newCollectorTestRepository(t), Options{GlobalProxyURLProvider: func() string { return currentProxy }})
	for _, wantProxy := range []string{firstProxy.URL, secondProxy.URL} {
		currentProxy = wantProxy
		client, errClient := collector.options.HTTPClient(&coreauth.Auth{}, time.Second)
		if errClient != nil {
			t.Fatalf("HTTPClient() error = %v", errClient)
		}
		response, errGet := client.Get("http://quota.invalid/test")
		if errGet != nil {
			t.Fatalf("proxy request error = %v", errGet)
		}
		_ = response.Body.Close()
	}
	if firstProxyRequests.Load() != 1 || secondProxyRequests.Load() != 1 {
		t.Fatalf("proxy requests = %d/%d, want 1/1", firstProxyRequests.Load(), secondProxyRequests.Load())
	}
}

func TestProviderWindowNormalizationReconcilesContradictoryValues(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 45, 0, 0, time.UTC)
	windows, errParse := parseKimiUsageWindows([]byte(`{"limits":[{"name":"daily","used":8,"remaining":9,"limit":10,"duration":1,"timeUnit":"day"}]}`), now)
	if errParse != nil || len(windows) != 1 {
		t.Fatalf("parseKimiUsageWindows() = %+v, %v", windows, errParse)
	}
	window := windows[0]
	if window.Used == nil || *window.Used != 8 || window.Remaining == nil || *window.Remaining != 2 || window.UsedRatio == nil || *window.UsedRatio != 0.8 || window.RemainingRatio == nil || math.Abs(*window.RemainingRatio-0.2) > 1e-9 || window.Status != "low" {
		t.Fatalf("reconciled Kimi window = %+v", window)
	}
	antigravity, errAntigravity := parseAntigravityWindows([]byte(`{"groups":[{"buckets":[{"bucketId":"gemini-weekly","remainingFraction":0}]}]}`), now)
	if errAntigravity != nil || len(antigravity) != 1 || antigravity[0].Status != "exhausted" {
		t.Fatalf("remaining-only zero window = %+v, %v", antigravity, errAntigravity)
	}
}

func TestParseKimiUsageWindowsAcceptsFlexibleNumericAndResetFields(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	windows, errParse := parseKimiUsageWindows([]byte(`{"usage":{"limit":"100","used":"44","remaining":"56","reset_time":"2026-07-21T13:09:11.699501Z"},"limits":[{"window":{"duration":"300","timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":100,"used":"10","remaining":90,"reset_in":"120"}}]}`), now)
	if errParse != nil || len(windows) != 2 {
		t.Fatalf("parseKimiUsageWindows() = %+v, %v", windows, errParse)
	}
	summary := windows[0]
	if summary.Used == nil || *summary.Used != 44 || summary.Limit == nil || *summary.Limit != 100 || summary.Remaining == nil || *summary.Remaining != 56 {
		t.Fatalf("unexpected kimi summary window: %+v", summary)
	}
	if summary.ResetAt == nil || !summary.ResetAt.Equal(time.Date(2026, 7, 21, 13, 9, 11, 699501000, time.UTC)) {
		t.Fatalf("unexpected kimi summary reset time: %+v", summary.ResetAt)
	}
	limit := windows[1]
	if limit.PeriodUnit != "minute" || limit.PeriodValue == nil || *limit.PeriodValue != 300 || limit.WindowSeconds == nil || *limit.WindowSeconds != 18000 {
		t.Fatalf("unexpected kimi string duration: %+v", limit)
	}
	if limit.ResetAt == nil || !limit.ResetAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("unexpected kimi relative reset time: %+v", limit.ResetAt)
	}
}

func TestParseKimiUsageWindowsRejectsNonFiniteNumbers(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	windows, errParse := parseKimiUsageWindows([]byte(`{"usage":{"limit":"NaN","used":"Infinity","remaining":"-Inf"},"limits":[{"window":{"duration":"300","timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"NaN","used":"Inf","remaining":"Infinity"}}]}`), now)
	if errParse != nil || len(windows) != 0 {
		t.Fatalf("parseKimiUsageWindows() = %+v, %v, want no usable windows", windows, errParse)
	}
}

func TestQuotaBackoffJitterIsBoundedAndStable(t *testing.T) {
	base := 5 * time.Minute
	first := quotaBackoffWithJitter(base, "credential-a")
	second := quotaBackoffWithJitter(base, "credential-a")
	if first != second || first < base || first > base+base/5 {
		t.Fatalf("quotaBackoffWithJitter() = %v and %v, want stable value in [%v,%v]", first, second, base, base+base/5)
	}
}

func TestCollectorProbeTimeoutBecomesRetryableFailure(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-timeout", map[string]any{"type": "codex", "access_token": "probe-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{
		Owner: "home-a", CodexUsageURL: server.URL, ProbeTimeout: 20 * time.Millisecond,
		Now: func() time.Time { return now },
	})
	collector.collect(context.Background())
	item, errGet := repo.GetQuotaCredential(context.Background(), "codex-timeout", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "error" || item.CollectionStatus != "failed" || item.Error == nil || item.Error.Code != "UPSTREAM_UNAVAILABLE" || !item.Error.Retryable {
		t.Fatalf("unexpected timeout state: %+v", item)
	}
}

func TestCollectorEnforcesSQLiteSingleWriter(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC)
	for index := 0; index < 4; index++ {
		seedCollectorAuth(t, repo, fmt.Sprintf("codex-concurrency-%d", index), map[string]any{"type": "codex", "access_token": "probe-secret"})
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbes := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseProbes()
	// Codex performs an independent reset-credit detail request after usage, so
	// leave enough signal capacity for both requests per credential. Concurrency
	// is still measured by current/maximum below.
	started := make(chan struct{}, 16)
	var current atomic.Int32
	var maximum atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		active := current.Add(1)
		for {
			observed := maximum.Load()
			if active <= observed || maximum.CompareAndSwap(observed, active) {
				break
			}
		}
		started <- struct{}{}
		<-release
		current.Add(-1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600}}}`,
			)),
		}, nil
	})}
	collector := NewCollector(repo, Options{
		Owner: "home-a", ProviderConcurrency: 2, Now: func() time.Time { return now },
		HTTPClient: func(*coreauth.Auth, time.Duration) (*http.Client, error) { return client, nil },
	})
	done := make(chan struct{})
	go func() {
		collector.collect(context.Background())
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("collector did not start a probe")
	}
	select {
	case <-started:
		t.Fatal("collector exceeded the configured concurrency limit before a slot was released")
	case <-time.After(50 * time.Millisecond):
	}
	releaseProbes()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector did not complete after releasing probe slots")
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent probes = %d, want 1", maximum.Load())
	}
}

func TestCollectorDefaultsSQLiteConcurrencyToOne(t *testing.T) {
	collector := NewCollector(newCollectorTestRepository(t), Options{})
	if collector == nil || collector.options.ProviderConcurrency != 1 {
		t.Fatalf("SQLite collector concurrency = %v, want 1", collector.options.ProviderConcurrency)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func newCollectorTestRepository(t *testing.T) *cluster.Repository {
	repo, _ := newCollectorTestRepositoryAndDB(t)
	return repo
}

func newCollectorTestRepositoryAndDB(t *testing.T) (*cluster.Repository, *gorm.DB) {
	t.Helper()
	db, errOpen := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "quota.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if errOpen != nil {
		t.Fatalf("open sqlite: %v", errOpen)
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate: %v", errMigrate)
	}
	return cluster.NewRepository(db), db
}

func seedCollectorAuth(t *testing.T, repo *cluster.Repository, id string, metadata map[string]any) {
	seedCollectorProviderAuth(t, repo, id, "codex", metadata)
}

func seedCollectorProviderAuth(t *testing.T, repo *cluster.Repository, id string, provider string, metadata map[string]any) {
	t.Helper()
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{ID: id, Index: id, Provider: provider, Label: id, Status: coreauth.StatusActive, Metadata: metadata, CreatedAt: now, UpdatedAt: now}
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
}
