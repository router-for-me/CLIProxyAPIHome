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

func TestCollectorXAIProfileOutageKeepsConfirmedPlan(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "review-plan-quality"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	fixture := xaiPaidFixture(now)
	fixture.monthly = `{"config":{"monthlyLimit":15000,"used":1000}}`
	fixture.weekly = strings.Replace(fixture.weekly, `"currentPeriod":`, `"creditUsagePercent":1,"currentPeriod":`, 1)
	var round atomic.Int32
	server := serveXAIProbeFixture(t, func() xaiProbeFixture {
		value := fixture
		if round.Load() > 0 {
			value.profileStatus = 503
		}
		return value
	})
	collector := NewCollector(repo, Options{Owner: "review-plan", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	auth := &coreauth.Auth{ID: id, Provider: "xai"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), id, now)
	if errFirst != nil || first.Plan == nil || first.Plan.Name != "SuperGrok Plus" {
		t.Fatalf("invalid fixture: item=%+v err=%v", first, errFirst)
	}
	now = now.Add(time.Minute)
	round.Store(1)
	collector.collectCredential(context.Background(), auth, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if item.Plan == nil || item.Plan.Name != first.Plan.Name {
		t.Fatalf("unchanged account was relabeled after profile outage: before=%s after=%+v collection=%s", first.Plan.Name, item.Plan, item.CollectionStatus)
	}
}

func TestCollectorResetDuringProbeDoesNotReviveEndedCycle(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "review-reset-boundary"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	base := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	reset := base.Add(time.Minute)
	fixture := xaiPaidFixture(base)
	fixture.weekly = fmt.Sprintf(`{"config":{"creditUsagePercent":100,"currentPeriod":{"type":"weekly","start":%q,"end":%q}}}`, base.Add(-time.Hour).Format(time.RFC3339), reset.Format(time.RFC3339))
	var round, calls atomic.Int32
	clock := func() time.Time {
		if round.Load() > 0 {
			return base.Add(2 * time.Minute)
		}
		if calls.Add(1) <= 2 {
			return reset.Add(-time.Second)
		}
		return reset.Add(time.Second)
	}
	server := serveXAIProbeFixture(t, func() xaiProbeFixture {
		value := fixture
		if round.Load() > 0 {
			value.monthly = `{"config":{"monthlyLimit":15000,"used":1000}}`
			value.weeklyStatus = 503
		}
		return value
	})
	collector := NewCollector(repo, Options{Owner: "review-reset", XAIBillingURL: server.URL + "/v1/billing", Now: clock})
	auth := &coreauth.Auth{ID: id, Provider: "xai"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), id, reset.Add(time.Second))
	if errFirst != nil || len(first.Windows) != 1 {
		t.Fatalf("invalid fixture: item=%+v err=%v", first, errFirst)
	}
	t.Logf("ended reset=%s; observation=%s; stored window expiry=%s; snapshot freshness=%s", reset, first.Windows[0].ObservedAt, first.Windows[0].ExpiresAt, first.Freshness)
	round.Store(1)
	collector.collectCredential(context.Background(), auth, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), id, base.Add(2*time.Minute))
	if errGet != nil {
		t.Fatal(errGet)
	}
	for _, window := range item.Windows {
		if window.ID == "xai-weekly-usage" && item.Freshness == "fresh" {
			t.Fatalf("ended weekly quota was revived by a later partial merge: reset=%s expiry=%s status=%s collection=%s", window.ResetAt, window.ExpiresAt, item.QuotaStatus, item.CollectionStatus)
		}
	}
}

func TestCollectorXAIOldProductUsageCannotOverrideFreshSharedQuota(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "review-shared-quota"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	fixture := xaiPaidFixture(now)
	fixture.weekly = strings.Replace(fixture.weekly, `"currentPeriod":`, `"creditUsagePercent":100,"productUsage":[{"product":"GrokBuild","usagePercent":100}],"currentPeriod":`, 1)
	var round atomic.Int32
	server := serveXAIProbeFixture(t, func() xaiProbeFixture {
		value := fixture
		if round.Load() > 0 {
			value.profile = `{"subscriptionTier":"SuperGrokHeavy"}`
			value.weekly = strings.Replace(value.weekly, `"creditUsagePercent":100`, `"creditUsagePercent":10`, 1)
			value.weekly = strings.Replace(value.weekly, `"usagePercent":100`, `"usagePercent":null`, 1)
		}
		return value
	})
	collector := NewCollector(repo, Options{Owner: "review-shared", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
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
	if len(item.Windows) != 2 || item.Windows[0].Remaining == nil || *item.Windows[0].Remaining != 90 {
		t.Fatalf("fresh total was not collected: %+v", item)
	}
	if item.QuotaStatus != "healthy" || item.CollectionStatus != "partial" || item.Freshness != "fresh" {
		t.Fatalf("historical product breakdown overrode current shared pool: shared_remaining=%v plan=%+v status=%s collection=%s", *item.Windows[0].Remaining, item.Plan, item.QuotaStatus, item.CollectionStatus)
	}
	if !item.Windows[1].ObservedAt.Equal(first.Windows[1].ObservedAt) || item.Windows[1].Used == nil || *item.Windows[1].Used != 100 {
		t.Fatal("the shared-pool fix discarded the last-known product detail")
	}
	listed, errList := repo.ListQuotaCredentials(context.Background(), cluster.QuotaListQuery{Now: now, QuotaStatuses: map[string]struct{}{"healthy": {}}})
	if errList != nil || listed.Total != 1 || listed.Summary.Healthy != 1 || listed.GlobalSummary.Exhausted != 0 {
		t.Fatalf("list filters or summary disagree with shared quota: result=%+v error=%v", listed, errList)
	}
	stale, errStale := repo.GetQuotaCredential(context.Background(), id, now.Add(31*time.Minute))
	if errStale != nil || stale.Freshness != "stale" || stale.QuotaStatus != "healthy" {
		t.Fatalf("stored aggregate still depends on product detail: item=%+v error=%v", stale, errStale)
	}
}

func TestCollectorXAIPlanMetadataLifecycle(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "xai-plan-lifecycle"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	stages := []struct {
		name, profile, monthly, wantPlan string
		profileStatus                    int
		premium, partial                 bool
	}{
		{name: "first fallback", profileStatus: 503, wantPlan: "SuperGrok", partial: true},
		{name: "confirmed plan replaces fallback", profile: `{"subscriptionTier":"SuperGrokPlus"}`, wantPlan: "SuperGrok Plus", premium: true},
		{name: "missing tier preserves plan", profile: `{}`, wantPlan: "SuperGrok Plus", premium: true, partial: true},
		{name: "malformed tier cannot upgrade plan", profile: `{"subscriptionTier":true}`, monthly: `{"config":{"monthlyLimit":150000,"used":1000}}`, wantPlan: "SuperGrok Plus", premium: true, partial: true},
		{name: "unknown explicit tier remains visible", profile: `{"subscriptionTier":"FutureEnterprise"}`, wantPlan: "FutureEnterprise", partial: true},
		{name: "confirmed upgrade", profile: `{"subscriptionTier":"SuperGrokHeavy"}`, wantPlan: "SuperGrok Heavy", premium: true},
		{name: "authorization error cannot downgrade plan", profileStatus: 401, wantPlan: "SuperGrok Heavy", premium: true, partial: true},
		{name: "confirmed free replaces plan", profile: `{"subscriptionTier":null}`, wantPlan: "Free"},
		{name: "heuristic cannot replace confirmed free", profileStatus: 503, monthly: `{"config":{"monthlyLimit":150000,"used":1000}}`, wantPlan: "Free", partial: true},
		{name: "confirmed paid recovery", profile: `{"subscriptionTier":"SuperGrokPro"}`, wantPlan: "SuperGrok Pro", premium: true},
	}
	var phase atomic.Int32
	server := serveXAIProbeFixture(t, func() xaiProbeFixture {
		stage := stages[phase.Load()]
		fixture := xaiPaidFixture(now)
		fixture.profile, fixture.profileStatus = stage.profile, stage.profileStatus
		fixture.monthly = stage.monthly
		if fixture.monthly == "" {
			fixture.monthly = `{"config":{"monthlyLimit":15000,"used":1000}}`
		}
		fixture.weekly = strings.Replace(fixture.weekly, `"currentPeriod":`, `"creditUsagePercent":10,"currentPeriod":`, 1)
		return fixture
	})
	collector := NewCollector(repo, Options{Owner: "plan-lifecycle", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	for index, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			now = now.Add(time.Minute)
			phase.Store(int32(index))
			collector.collectCredential(context.Background(), &coreauth.Auth{ID: id, Provider: "xai"}, true)
			item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
			if errGet != nil || item.Plan == nil || item.Plan.Name != stage.wantPlan || item.Plan.Premium != stage.premium || item.ObservedAt == nil || !item.ObservedAt.Equal(now) {
				t.Fatalf("incorrect plan transition: item=%+v error=%v", item, errGet)
			}
			if (item.CollectionStatus == "partial") != stage.partial || (item.Error != nil) != stage.partial {
				t.Fatalf("incorrect collection metadata: status=%s error=%+v", item.CollectionStatus, item.Error)
			}
		})
	}
}

func TestCollectorWindowLifetimeAtResetBoundary(t *testing.T) {
	for _, provider := range []string{"devin", "xai"} {
		for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			t.Run(provider+"/"+offset.String(), func(t *testing.T) {
				repo := newCollectorTestRepository(t)
				id := "reset-boundary-" + provider
				seedCollectorProviderAuth(t, repo, id, provider, map[string]any{"type": provider, "api_key": "synthetic", "access_token": "synthetic"})
				base := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
				reset := base.Add(time.Minute)
				parsedAt, persistedAt := reset.Add(-time.Second), reset.Add(offset)
				fixture := xaiPaidFixture(base)
				fixture.weekly = fmt.Sprintf(`{"config":{"creditUsagePercent":100,"currentPeriod":{"type":"weekly","start":%q,"end":%q}}}`, base.Add(-time.Hour).Format(time.RFC3339), reset.Format(time.RFC3339))
				var phase, clockReads atomic.Int32
				clock := func() time.Time {
					if phase.Load() > 0 {
						return reset.Add(time.Minute)
					}
					if clockReads.Add(1) <= 2 {
						return parsedAt
					}
					return persistedAt
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body string
					if provider == "devin" {
						body = fmt.Sprintf(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":0,"dailyQuotaResetAtUnix":%d,"weeklyQuotaRemainingPercent":80}}}`, reset.Unix())
						if phase.Load() > 0 {
							body = `{"userStatus":{"planStatus":{"weeklyQuotaRemainingPercent":80}}}`
						}
					} else if r.URL.Path == "/v1/user" {
						body = fixture.profile
					} else if r.URL.Query().Get("format") == "credits" {
						if phase.Load() > 0 {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						body = fixture.weekly
					} else {
						body = `{"config":{"monthlyLimit":15000,"used":1000}}`
					}
					_, _ = io.WriteString(w, body)
				}))
				t.Cleanup(server.Close)
				collector := NewCollector(repo, Options{Owner: "reset-boundary", DevinQuotaURL: server.URL, XAIBillingURL: server.URL + "/v1/billing", Now: clock})
				auth := &coreauth.Auth{ID: id, Provider: provider}
				collector.collectCredential(context.Background(), auth, true)
				first, errFirst := repo.GetQuotaCredential(context.Background(), id, persistedAt)
				if errFirst != nil || first.ExpiresAt == nil || !first.ExpiresAt.Equal(reset) || (first.Freshness == "fresh") != (offset < 0) {
					t.Fatalf("snapshot crosses a reset: item=%+v error=%v", first, errFirst)
				}
				endedID := "xai-weekly-usage"
				if provider == "devin" {
					endedID = "devin-daily"
				}
				for _, window := range first.Windows {
					wantExpiry := parsedAt.Add(defaultSnapshotFreshness)
					if window.ID == endedID {
						wantExpiry = reset
					}
					if window.ExpiresAt == nil || !window.ExpiresAt.Equal(wantExpiry) {
						t.Fatalf("incorrect independent deadline for %s: %v, want %v", window.ID, window.ExpiresAt, wantExpiry)
					}
				}
				phase.Store(1)
				collector.collectCredential(context.Background(), auth, true)
				item, errGet := repo.GetQuotaCredential(context.Background(), id, clock())
				if errGet != nil || item.Freshness != "fresh" || item.QuotaStatus != "healthy" || item.CollectionStatus != "partial" {
					t.Fatalf("ended cycle polluted a later merge: item=%+v error=%v", item, errGet)
				}
				for _, window := range item.Windows {
					if window.ID == endedID {
						t.Fatalf("ended cycle was retained: %+v", window)
					}
				}
			})
		}
	}
}

func TestCollectorXAIAlreadyEndedMonthlyWindowCannotGainFreshness(t *testing.T) {
	repo := newCollectorTestRepository(t)
	id := "xai-ended-monthly"
	seedCollectorProviderAuth(t, repo, id, "xai", map[string]any{"type": "xai", "access_token": "synthetic"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	reset := now.Add(-time.Minute)
	fixture := xaiPaidFixture(now)
	fixture.monthly = fmt.Sprintf(`{"config":{"monthlyLimit":15000,"used":15000,"billingPeriodEnd":%q}}`, reset.Format(time.RFC3339))
	fixture.weeklyStatus = http.StatusServiceUnavailable
	server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
	collector := NewCollector(repo, Options{Owner: "ended-monthly", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	collector.collectCredential(context.Background(), &coreauth.Auth{ID: id, Provider: "xai"}, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), id, now)
	if errGet != nil || len(item.Windows) != 1 || item.Freshness != "stale" || item.ExpiresAt == nil || !item.ExpiresAt.Equal(reset) || item.Windows[0].ExpiresAt == nil || !item.Windows[0].ExpiresAt.Equal(reset) {
		t.Fatalf("an ended monthly cycle gained freshness: item=%+v error=%v", item, errGet)
	}
}
