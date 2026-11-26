package quota

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCollectorDevinRejectsCrossOriginBodyRedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var forwarded atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded.Add(1)
				_, _ = io.WriteString(w, devinQuotaFixture)
			}))
			defer destination.Close()
			redirectedURL := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, redirectedURL, status)
			}))
			defer origin.Close()
			collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "safety-test", DevinQuotaURL: origin.URL})
			_, failure := collector.probeDevin(context.Background(), &coreauth.Auth{Metadata: map[string]any{"api_key": "synthetic-session"}})
			if forwarded.Load() != 0 || failure == nil {
				t.Fatal("body-authenticated quota request followed a cross-origin redirect")
			}
		})
	}
}

func TestCollectorDevinAllowsSameOriginBodyRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/quota", http.StatusTemporaryRedirect)
			return
		}
		payload, errRead := io.ReadAll(r.Body)
		if errRead != nil || r.Method != http.MethodPost || !strings.Contains(string(payload), "synthetic-session") {
			t.Error("same-origin redirect lost body authentication")
		}
		_, _ = io.WriteString(w, devinQuotaFixture)
	}))
	defer server.Close()
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	client := server.Client()
	collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "safety-test", DevinQuotaURL: server.URL + "/start", Now: func() time.Time { return now }, HTTPClient: func(*coreauth.Auth, time.Duration) (*http.Client, error) { return client, nil }})
	result, failure := collector.probeDevin(context.Background(), &coreauth.Auth{Metadata: map[string]any{"api_key": "synthetic-session"}})
	if failure != nil || len(result.windows) != 2 || client.CheckRedirect != nil {
		t.Fatalf("same-origin quota request failed or mutated the shared client: failure=%v", failure)
	}
}

func TestCollectorDevinPartialKeepsUnobservedCycle(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "partial-devin-partial", "devin", map[string]any{"type": "devin", "api_key": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	var round atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := strings.Replace(devinQuotaFixture, `"weeklyQuotaRemainingPercent":"83"`, `"weeklyQuotaRemainingPercent":0`, 1)
		switch round.Load() {
		case 1:
			body = `{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":60,"weeklyQuotaResetAtUnix":1794153600}}}`
		case 2:
			body = strings.Replace(devinQuotaFixture, `"weeklyQuotaRemainingPercent":"83"`, `"weeklyQuotaRemainingPercent":75`, 1)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "safety-test", DevinQuotaURL: server.URL, Now: func() time.Time { return now }})
	auth := &coreauth.Auth{ID: "partial-devin-partial", Provider: "devin"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errFirst != nil || len(first.Windows) != 2 || first.QuotaStatus != "exhausted" || first.Windows[1].ExpiresAt == nil {
		t.Fatal("invalid fixture")
	}
	round.Store(1)
	now = now.Add(time.Minute)
	collector.collectCredential(context.Background(), auth, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if len(item.Windows) != 2 || item.QuotaStatus != "exhausted" || item.CollectionStatus != "partial" || item.Windows[0].Remaining == nil || *item.Windows[0].Remaining != 60 || item.Windows[1].Remaining == nil || *item.Windows[1].Remaining != 0 {
		t.Fatalf("missing weekly observation erased exhausted weekly quota: windows=%d status=%s collection=%s freshness=%s", len(item.Windows), item.QuotaStatus, item.CollectionStatus, item.Freshness)
	}
	if !item.Windows[1].ObservedAt.Equal(first.Windows[1].ObservedAt) || item.Windows[1].ExpiresAt == nil || !item.Windows[1].ExpiresAt.Equal(*first.Windows[1].ExpiresAt) {
		t.Fatal("unobserved weekly quota acquired a new observation time or freshness deadline")
	}
	round.Store(2)
	now = now.Add(time.Minute)
	collector.collectCredential(context.Background(), auth, true)
	recovered, errRecovered := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errRecovered != nil || recovered.CollectionStatus != "success" || recovered.Error != nil || recovered.QuotaStatus != "healthy" || len(recovered.Windows) != 2 || recovered.Windows[1].Remaining == nil || *recovered.Windows[1].Remaining != 75 || !recovered.Windows[1].ObservedAt.Equal(now) {
		t.Fatalf("complete Devin response did not recover: item=%+v error=%v", recovered, errRecovered)
	}
}

func TestCollectorDevinResetOnlyKeepsValidAmounts(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "partial-devin-reset-only", "devin", map[string]any{"type": "devin", "api_key": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	var round atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := devinQuotaFixture
		if round.Load() > 0 {
			body = `{"userStatus":{"planStatus":{"dailyQuotaResetAtUnix":1793980800,"weeklyQuotaResetAtUnix":1794153600}}}`
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "safety-test", DevinQuotaURL: server.URL, Now: func() time.Time { return now }})
	auth := &coreauth.Auth{ID: "partial-devin-reset-only", Provider: "devin"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errFirst != nil || first.ObservedAt == nil {
		t.Fatal("invalid fixture")
	}
	round.Store(1)
	now = now.Add(time.Minute)
	collector.collectCredential(context.Background(), auth, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if item.CollectionStatus != "failed" || item.Error == nil || len(item.Windows) != 2 || item.Windows[0].Remaining == nil || item.Windows[1].Remaining == nil || !item.ObservedAt.Equal(*first.ObservedAt) || item.LastAttemptAt == nil || !item.LastAttemptAt.Equal(now) {
		t.Fatalf("reset-only response erased valid amounts and refreshed observation: collection=%s old=%s new=%s", item.CollectionStatus, first.ObservedAt, item.ObservedAt)
	}
}

func TestXAIExplicitAmountsRejectInvalidPeriods(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct{ name, period string }{
		{"expired", `{"type":"weekly","end":"2026-11-04T00:00:00Z"}`},
		{"future start", `{"type":"weekly","start":"2026-11-07T00:00:00Z","end":"2026-11-13T00:00:00Z"}`},
		{"invalid end", `{"type":"weekly","end":"invalid"}`},
		{"wrong period", `{"type":"daily","end":"2026-11-13T00:00:00Z"}`},
		{"conflicting aliases", `{"type":"weekly","end":"2026-11-13T00:00:00Z"},"current_period":{"type":"weekly","end":"2026-11-04T00:00:00Z"}`},
	} {
		for _, percentage := range []string{"0", "42", "100"} {
			t.Run(test.name+"/"+percentage, func(t *testing.T) {
				fixture := xaiPaidFixture(now)
				fixture.weekly = `{"config":{"creditUsagePercent":` + percentage + `,"currentPeriod":` + test.period + `,"productUsage":[{"product":"GrokBuild","usagePercent":1}]}}`
				server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
				collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "safety-test", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
				if _, failure := collector.probeXAI(context.Background(), &coreauth.Auth{Metadata: map[string]any{"access_token": "synthetic"}}); failure == nil {
					t.Fatal("invalid weekly period was accepted as a new quota observation")
				}
			})
		}
	}
}

func TestQuotaSnapshotsExpireAtProviderReset(t *testing.T) {
	for _, provider := range []string{"devin", "xai"} {
		t.Run(provider, func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			id := "reset-" + provider
			seedCollectorProviderAuth(t, repo, id, provider, map[string]any{"type": provider, "api_key": "synthetic", "access_token": "synthetic"})
			now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
			resetAt := now.Add(5 * time.Minute)
			fixture := xaiPaidFixture(now)
			fixture.weekly = strings.Replace(fixture.weekly, now.Add(6*24*time.Hour).Format(time.RFC3339), resetAt.Format(time.RFC3339), 1)
			devinBody := fmt.Sprintf(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":65,"weeklyQuotaRemainingPercent":83,"dailyQuotaResetAtUnix":%d,"weeklyQuotaResetAtUnix":%d}}}`, resetAt.Unix(), now.Add(6*24*time.Hour).Unix())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := devinBody
				if provider == "xai" {
					body = fixture.monthly
					if r.URL.Path == "/v1/user" {
						body = fixture.profile
					} else if r.URL.Query().Get("format") == "credits" {
						body = fixture.weekly
					}
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			collector := NewCollector(repo, Options{Owner: "safety-test", DevinQuotaURL: server.URL, XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			collector.collectCredential(context.Background(), &coreauth.Auth{ID: id, Provider: provider}, true)
			item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
			if errGet != nil || item.ExpiresAt == nil || !item.ExpiresAt.Equal(resetAt) || item.NextProbeAt == nil || !item.NextProbeAt.Equal(resetAt) {
				t.Fatalf("quota freshness extended past reset: item=%+v error=%v", item, errGet)
			}
			now = resetAt
			stale, errStale := repo.GetQuotaCredential(context.Background(), id, now)
			if errStale != nil || stale.Freshness != "stale" {
				t.Fatalf("a reset cycle remained fresh: item=%+v error=%v", stale, errStale)
			}
		})
	}
}

func TestXAIPartialHonorsRetryAfter(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "partial-xai-retry", "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/user" {
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		body := `{"config":{"monthlyLimit":15000,"used":100}}`
		if r.URL.Query().Get("format") == "credits" {
			body = `{"config":{"creditUsagePercent":1,"currentPeriod":{"type":"weekly","end":"2026-11-06T00:05:00Z"}}}`
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "safety-test", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	collector.collectCredential(context.Background(), &coreauth.Auth{ID: "partial-xai-retry", Provider: "xai"}, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), "partial-xai-retry", now)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if item.CollectionStatus != "partial" || item.Error == nil || item.Error.Code != "UPSTREAM_RATE_LIMITED" || item.NextProbeAt == nil || item.NextProbeAt.Before(now.Add(2*time.Hour)) {
		t.Fatalf("profile Retry-After=7200 was ignored: collection=%s next=%s", item.CollectionStatus, item.NextProbeAt)
	}
	if item.ExpiresAt == nil || !item.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatal("quota freshness was incorrectly extended to the retry deadline")
	}
}

func TestXAIHonorsLongestRetryAfterAcrossIndependentFailures(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			id := "xai-longest-retry"
			seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
			now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/user" {
					w.WriteHeader(http.StatusUnauthorized)
				} else if r.URL.Query().Get("format") == "credits" {
					if partial {
						_, _ = io.WriteString(w, `{"config":{"creditUsagePercent":1,"currentPeriod":{"type":"weekly","end":"2026-11-13T00:00:00Z"}}}`)
					} else {
						w.Header().Set("Retry-After", "7200")
						w.WriteHeader(http.StatusTooManyRequests)
					}
				} else {
					w.Header().Set("Retry-After", now.Add(3*time.Hour).Format(http.TimeFormat))
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			collector := NewCollector(repo, Options{Owner: "safety-test", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			collector.collectCredential(context.Background(), &coreauth.Auth{ID: id, Provider: "xai"}, true)
			item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
			status := "failed"
			if partial {
				status = "partial"
			}
			if errGet != nil || item.CollectionStatus != status || item.Error == nil || item.Error.Code != "UPSTREAM_AUTH_REJECTED" || item.NextProbeAt == nil || item.NextProbeAt.Before(now.Add(3*time.Hour)) {
				t.Fatalf("selected error discarded another component's retry deadline: item=%+v error=%v", item, errGet)
			}
		})
	}
}

func TestXAIFreeProfileStillHonorsCreditsRetryAfter(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "xai-free-retry"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"config":{"monthlyLimit":0}}`
		if r.URL.Path == "/v1/user" {
			body = `{"subscriptionTier":null}`
		} else if r.URL.Query().Get("format") == "credits" {
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "safety-test", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	collector.collectCredential(context.Background(), &coreauth.Auth{ID: id, Provider: "xai"}, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
	if errGet != nil || item.CollectionStatus != "success" || item.Plan == nil || item.Plan.Name != "Free" || len(item.Windows) != 0 || item.NextProbeAt == nil || item.NextProbeAt.Before(now.Add(2*time.Hour)) {
		t.Fatalf("ignored free allowance also discarded upstream retry policy: item=%+v error=%v", item, errGet)
	}
}
