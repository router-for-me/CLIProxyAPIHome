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
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestCollectorDevinDailyResetDoesNotExpireWeeklyObservation(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "cross-cycle-devin"
	seedCollectorProviderAuth(t, repo, id, "devin", map[string]any{"type": "devin", "api_key": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	base := now
	dailyReset := base.Add(5 * time.Minute)
	weeklyReset := base.Add(6 * 24 * time.Hour)
	var round atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":60,"weeklyQuotaRemainingPercent":0,"dailyQuotaResetAtUnix":%d,"weeklyQuotaResetAtUnix":%d}}}`, dailyReset.Unix(), weeklyReset.Unix())
		switch round.Load() {
		case 1:
			body = fmt.Sprintf(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":80,"dailyQuotaResetAtUnix":%d}}}`, base.Add(24*time.Hour).Unix())
		case 2:
			body = fmt.Sprintf(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":80,"weeklyQuotaRemainingPercent":75,"dailyQuotaResetAtUnix":%d,"weeklyQuotaResetAtUnix":%d}}}`, base.Add(24*time.Hour).Unix(), weeklyReset.Unix())
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "cross-cycle-test", DevinQuotaURL: server.URL, Now: func() time.Time { return now }})
	auth := &coreauth.Auth{ID: id, Provider: "devin"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), id, now)
	if errFirst != nil || len(first.Windows) != 2 || first.QuotaStatus != "exhausted" {
		t.Fatalf("invalid fixture: item=%+v err=%v", first, errFirst)
	}
	if first.Windows[0].ExpiresAt == nil || !first.Windows[0].ExpiresAt.Equal(dailyReset) || first.Windows[1].ExpiresAt == nil || !first.Windows[1].ExpiresAt.Equal(base.Add(30*time.Minute)) {
		t.Fatal("daily and weekly windows do not have independent freshness deadlines")
	}
	now = base.Add(6 * time.Minute)
	round.Store(1)
	collector.collectCredential(context.Background(), auth, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
	if errGet != nil {
		t.Fatal(errGet)
	}
	var weeklyFound bool
	for _, window := range item.Windows {
		if window.ID == "devin-weekly" && window.Remaining != nil && *window.Remaining == 0 {
			weeklyFound = true
		}
	}
	if !weeklyFound || item.QuotaStatus != "exhausted" {
		t.Fatalf("6-minute-old unreset weekly exhaustion was pruned at the daily boundary: windows=%d status=%s collection=%s freshness=%s", len(item.Windows), item.QuotaStatus, item.CollectionStatus, item.Freshness)
	}
	if item.ExpiresAt == nil || !item.ExpiresAt.Equal(base.Add(30*time.Minute)) || item.NextProbeAt == nil || !item.NextProbeAt.Equal(*item.ExpiresAt) {
		t.Fatal("partial collection extended a retained window's freshness or deferred its scheduled refresh")
	}
	now = now.Add(time.Minute)
	round.Store(2)
	collector.collectCredential(context.Background(), auth, true)
	recovered, errRecovered := repo.GetQuotaCredential(context.Background(), id, now)
	if errRecovered != nil || recovered.CollectionStatus != "success" || recovered.Error != nil || recovered.QuotaStatus != "healthy" || len(recovered.Windows) != 2 || recovered.Windows[1].Remaining == nil || *recovered.Windows[1].Remaining != 75 || !recovered.Windows[1].ObservedAt.Equal(now) {
		t.Fatalf("complete response did not replace the retained cycle: item=%+v error=%v", recovered, errRecovered)
	}
}

func TestCollectorXAIMalformedMonthlyKeepsValidMonthlyObservation(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "cross-component-xai"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	fixture := xaiPaidFixture(now)
	fixture.monthly = `{"config":{"monthlyLimit":15000,"used":15000,"billingPeriodEnd":"2026-12-01T00:00:00Z"}}`
	fixture.weekly = strings.Replace(fixture.weekly, `"currentPeriod":`, `"creditUsagePercent":1,"currentPeriod":`, 1)
	var round atomic.Int32
	server := serveXAIProbeFixture(t, func() xaiProbeFixture {
		value := fixture
		if round.Load() > 0 {
			value.monthly = `{"config":`
		}
		return value
	})
	collector := NewCollector(repo, Options{Owner: "cross-component-test", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	auth := &coreauth.Auth{ID: id, Provider: "xai"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), id, now)
	if errFirst != nil || len(first.Windows) != 2 || first.QuotaStatus != "exhausted" {
		t.Fatalf("invalid fixture: item=%+v err=%v", first, errFirst)
	}
	now = now.Add(time.Minute)
	round.Store(1)
	collector.collectCredential(context.Background(), auth, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
	if errGet != nil {
		t.Fatal(errGet)
	}
	var monthlyFound bool
	for _, window := range item.Windows {
		if window.ID == "xai-monthly-spend" && window.Remaining != nil && *window.Remaining == 0 {
			monthlyFound = true
		}
	}
	if !monthlyFound || item.QuotaStatus != "exhausted" {
		t.Fatalf("a malformed response deleted a 1-minute-old monthly exhaustion: windows=%d status=%s collection=%s freshness=%s", len(item.Windows), item.QuotaStatus, item.CollectionStatus, item.Freshness)
	}
}

func TestCollectorXAIPartialComponentReplacement(t *testing.T) {
	for _, test := range []struct {
		name                                       string
		monthly, weekly, profile                   string
		monthlyStatus, weeklyStatus, profileStatus int
		keepMonthly, keepWeekly, keepProduct       bool
		clearMonthly, clearWeekly, failed, partial bool
	}{
		{name: "malformed monthly", monthly: `{"config":`, keepMonthly: true, partial: true},
		{name: "monthly unavailable", monthlyStatus: 503, keepMonthly: true, partial: true},
		{name: "monthly amount unknown", monthly: `{"config":{"monthlyLimit":15000,"used":null}}`, keepMonthly: true, partial: true},
		{name: "weekly unavailable", weeklyStatus: 503, keepWeekly: true, keepProduct: true, partial: true},
		{name: "malformed weekly", weekly: `[]`, keepWeekly: true, keepProduct: true, partial: true},
		{name: "weekly aggregate unknown", weekly: `{"config":{"creditUsagePercent":null,"currentPeriod":{"type":"weekly","end":"2026-11-12T00:00:00Z"},"productUsage":[{"product":"GrokBuild","usagePercent":40}]}}`, keepWeekly: true, partial: true},
		{name: "weekly product unknown", weekly: `{"config":{"creditUsagePercent":50,"currentPeriod":{"type":"weekly","end":"2026-11-12T00:00:00Z"},"productUsage":[{"product":"GrokBuild","usagePercent":null}]}}`, keepProduct: true, partial: true},
		{name: "monthly caps disabled", monthly: `{"config":{"monthlyLimit":0,"onDemandCap":0}}`, clearMonthly: true},
		{name: "free profile removes weekly group", profile: `{"subscriptionTier":null}`, weeklyStatus: 503, clearWeekly: true},
		{name: "profile failure keeps amounts authoritative", profileStatus: 503, partial: true},
		{name: "all amount queries failed", monthlyStatus: 503, weeklyStatus: 503, keepMonthly: true, keepWeekly: true, keepProduct: true, failed: true},
		{name: "no new amounts keeps last observation", monthly: `{"config":{"monthlyLimit":0,"onDemandCap":0}}`, weeklyStatus: 503, keepMonthly: true, keepWeekly: true, keepProduct: true, failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			id := "xai-groups"
			seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
			now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
			initial := xaiPaidFixture(now)
			initial.monthly = `{"config":{"monthlyLimit":15000,"used":15000,"onDemandCap":5000,"onDemandUsed":1000}}`
			initial.weekly = strings.Replace(initial.weekly, `"currentPeriod":`, `"creditUsagePercent":100,"productUsage":[{"product":"GrokBuild","usagePercent":20}],"currentPeriod":`, 1)
			next := initial
			next.monthly = `{"config":{"monthlyLimit":15000,"used":5000,"onDemandCap":0}}`
			next.weekly = strings.Replace(strings.Replace(initial.weekly, `"creditUsagePercent":100`, `"creditUsagePercent":50`, 1), `"usagePercent":20`, `"usagePercent":40`, 1)
			if test.monthly != "" {
				next.monthly = test.monthly
			}
			if test.weekly != "" {
				next.weekly = test.weekly
			}
			if test.profile != "" {
				next.profile = test.profile
			}
			next.monthlyStatus, next.weeklyStatus, next.profileStatus = test.monthlyStatus, test.weeklyStatus, test.profileStatus
			var round atomic.Int32
			server := serveXAIProbeFixture(t, func() xaiProbeFixture {
				if round.Load() == 0 {
					return initial
				}
				return next
			})
			collector := NewCollector(repo, Options{Owner: "component-test", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			auth := &coreauth.Auth{ID: id, Provider: "xai"}
			collector.collectCredential(context.Background(), auth, true)
			first, errFirst := repo.GetQuotaCredential(context.Background(), id, now)
			if errFirst != nil || first.CollectionStatus != "success" || len(first.Windows) != 4 {
				t.Fatalf("invalid fixture: item=%+v error=%v", first, errFirst)
			}
			old := make(map[string]cluster.QuotaWindow)
			for _, window := range first.Windows {
				old[window.ID] = window
			}
			now = now.Add(time.Minute)
			round.Store(1)
			collector.collectCredential(context.Background(), auth, true)
			item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
			if errGet != nil {
				t.Fatal(errGet)
			}
			status := "success"
			if test.failed {
				status = "failed"
			} else if test.partial {
				status = "partial"
			}
			if item.CollectionStatus != status || (item.Error != nil) != (test.partial || test.failed) {
				t.Fatalf("unexpected collection status: %+v", item)
			}
			current := make(map[string]cluster.QuotaWindow)
			for _, window := range item.Windows {
				current[window.ID] = window
			}
			assertWindow := func(id string, keep, remove bool, remaining float64) {
				t.Helper()
				window, exists := current[id]
				if remove {
					if exists {
						t.Fatalf("authoritatively removed window survived: %s", id)
					}
					return
				}
				observation := now
				if keep {
					previous := old[id]
					remaining = *previous.Remaining
					observation = previous.ObservedAt
					if window.ExpiresAt == nil || !window.ExpiresAt.Equal(*previous.ExpiresAt) {
						t.Fatalf("retained window gained freshness: %s", id)
					}
				}
				if !exists || window.Remaining == nil || *window.Remaining != remaining || !window.ObservedAt.Equal(observation) {
					t.Fatalf("window lost its expected amount or observation: %s %+v", id, window)
				}
			}
			assertWindow("xai-monthly-spend", test.keepMonthly, test.clearMonthly, 100)
			assertWindow("xai-on-demand", test.keepMonthly, !test.keepMonthly, 40)
			assertWindow("xai-weekly-usage", test.keepWeekly, test.clearWeekly, 50)
			assertWindow("xai-product-grokbuild", test.keepProduct, test.clearWeekly, 60)
			if (test.keepMonthly || test.keepWeekly) && item.QuotaStatus != "exhausted" {
				t.Fatal("a retained exhausted window stopped contributing to aggregate status")
			}
			if test.failed && (item.ObservedAt == nil || !item.ObservedAt.Equal(*first.ObservedAt)) {
				t.Fatal("failed collection advanced the last valid observation")
			}
		})
	}
}
