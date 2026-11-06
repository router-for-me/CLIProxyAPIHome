package quota

import (
	"context"
	"fmt"
	"io"
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

type xaiProbeFixture struct {
	profile, monthly, weekly                   string
	profileStatus, monthlyStatus, weeklyStatus int
}

func xaiPaidFixture(now time.Time) xaiProbeFixture {
	return xaiProbeFixture{
		profile: `{"subscriptionTier":"SuperGrokPlus"}`,
		monthly: `{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`,
		weekly: fmt.Sprintf(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q}}}`,
			now.Add(-time.Hour).Format(time.RFC3339), now.Add(6*24*time.Hour).Format(time.RFC3339)),
	}
}

func serveXAIProbeFixture(t *testing.T, fixture func() xaiProbeFixture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := fixture()
		body, status := value.monthly, value.monthlyStatus
		if r.URL.Path == "/v1/user" {
			if r.URL.Query().Get("include") != "subscription" {
				t.Error("profile request did not select subscription metadata")
			}
			body, status = value.profile, value.profileStatus
		} else if r.URL.Query().Get("format") == "credits" {
			body, status = value.weekly, value.weeklyStatus
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestXAIWeeklySubscriptionAndPresenceMatrix(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	type scenario struct {
		name           string
		profile        string
		profileStatus  int
		percentage     string
		remaining      *float64
		plan           string
		partial, fails bool
	}
	for _, test := range []scenario{
		{name: "free null rejects weekly even with usage", profile: `{"subscriptionTier":null}`, percentage: `1`, plan: "Free"},
		{name: "free explicit rejects weekly", profile: `{"subscriptionTier":"Free"}`, percentage: `0`, plan: "Free"},
		{name: "none rejects weekly", profile: `{"subscription_tier":"none"}`, plan: "Free"},
		{name: "empty tier is free", profile: `{"subscriptionTier":""}`, plan: "Free"},
		{name: "paid omitted zero", remaining: quotaFloatPtr(100), plan: "SuperGrok Plus"},
		{name: "paid explicit zero", percentage: `0`, remaining: quotaFloatPtr(100), plan: "SuperGrok Plus"},
		{name: "paid numeric string zero", percentage: `"0"`, remaining: quotaFloatPtr(100), plan: "SuperGrok Plus"},
		{name: "paid exhausted", percentage: `100`, remaining: quotaFloatPtr(0), plan: "SuperGrok Plus"},
		{name: "paid null is unknown", percentage: `null`, fails: true},
		{name: "paid malformed is unknown", percentage: `"bad"`, fails: true},
		{name: "paid boolean is unknown", percentage: `false`, fails: true},
		{name: "paid negative is unknown", percentage: `-1`, fails: true},
		{name: "paid over range is unknown", percentage: `101`, fails: true},
		{name: "conflicting percentage aliases are unknown", percentage: `0,"credit_usage_percent":1`, fails: true},
		{name: "matching percentage aliases are usable", percentage: `1,"credit_usage_percent":"1"`, remaining: quotaFloatPtr(99), plan: "SuperGrok Plus"},
		{name: "missing tier keeps observed data partial", profile: `{}`, percentage: `1`, remaining: quotaFloatPtr(99), partial: true},
		{name: "missing tier cannot infer zero", profile: `{}`, fails: true},
		{name: "unknown tier keeps observed data partial", profile: `{"subscriptionTier":"FutureEnterprise"}`, percentage: `1`, remaining: quotaFloatPtr(99), plan: "FutureEnterprise", partial: true},
		{name: "unknown tier cannot infer zero", profile: `{"subscriptionTier":"FutureEnterprise"}`, fails: true},
		{name: "invalid tier cannot infer zero", profile: `{"subscriptionTier":true}`, fails: true},
		{name: "conflicting subscription aliases cannot infer zero", profile: `{"subscriptionTier":"SuperGrokPlus","subscription_tier":"Free"}`, fails: true},
		{name: "profile outage keeps observed data partial", profileStatus: http.StatusServiceUnavailable, percentage: `1`, remaining: quotaFloatPtr(99), partial: true},
		{name: "profile authorization error cannot infer zero", profileStatus: http.StatusUnauthorized, fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := xaiPaidFixture(now)
			if test.profile != "" {
				fixture.profile = test.profile
			}
			fixture.profileStatus = test.profileStatus
			if test.percentage != "" {
				fixture.weekly = strings.Replace(fixture.weekly, `"currentPeriod":`, `"creditUsagePercent":`+test.percentage+`,"currentPeriod":`, 1)
			}
			server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
			collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-matrix", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			result, errProbe := collector.probeXAI(context.Background(), &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}})
			if test.fails {
				if errProbe == nil {
					t.Fatal("unconfirmed or invalid quota was accepted as a successful observation")
				}
				return
			}
			if errProbe != nil || result.partial != test.partial {
				t.Fatalf("unexpected collection state: partial=%v error=%v", result.partial, errProbe)
			}
			if test.remaining == nil {
				if len(result.windows) != 0 {
					t.Fatalf("free profile acquired a weekly allowance: %+v", result.windows)
				}
			} else if len(result.windows) != 1 || result.windows[0].Remaining == nil || *result.windows[0].Remaining != *test.remaining {
				t.Fatalf("incorrect remaining quota: %+v", result.windows)
			}
			if test.plan == "" {
				if result.plan != nil {
					t.Fatalf("unknown subscription acquired a plan: %+v", result.plan)
				}
			} else if result.plan == nil || result.plan.Name != test.plan {
				t.Fatalf("subscription plan was lost: %+v", result.plan)
			}
		})
	}
}

func TestXAIOmittedZeroRejectsInvalidOrContradictoryPeriods(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct{ name, period string }{
		{"expired", `{"type":"weekly","end":"2026-11-05T00:00:00Z"}`},
		{"future start", `{"type":"weekly","start":"2026-11-07T00:00:00Z","end":"2026-11-13T00:00:00Z"}`},
		{"missing end", `{"type":"weekly"}`},
		{"invalid end", `{"type":"weekly","end":"bad"}`},
		{"unknown period", `{"type":"future_period","end":"2026-11-13T00:00:00Z"}`},
		{"monthly period", `{"type":"monthly","end":"2026-11-13T00:00:00Z"}`},
		{"conflicting period aliases", `{"type":"weekly","end":"2026-11-13T00:00:00Z"},"current_period":{"type":"monthly","end":"2026-11-13T00:00:00Z"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := xaiPaidFixture(now)
			fixture.weekly = `{"config":{"currentPeriod":` + test.period + `}}`
			server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
			collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-period", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			if result, errProbe := collector.probeXAI(context.Background(), &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}}); errProbe == nil {
				t.Fatalf("invalid period became a zero-usage quota: %+v", result.windows)
			}
		})
	}
}

func TestXAIOmittedTotalDoesNotContradictObservedProductUsage(t *testing.T) {
	now := time.Now().UTC()
	fixture := xaiPaidFixture(now)
	fixture.weekly = strings.Replace(fixture.weekly, `"currentPeriod":`, `"productUsage":[{"product":"GrokBuild","usagePercent":1}],"currentPeriod":`, 1)
	server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
	collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-products", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	result, errProbe := collector.probeXAI(context.Background(), &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}})
	if errProbe != nil || !result.partial || len(result.windows) != 2 || result.windows[0].Remaining != nil || result.windows[1].Used == nil || *result.windows[1].Used != 1 {
		t.Fatalf("product usage was discarded or an unused total was invented: result=%+v error=%v", result, errProbe)
	}
}

func TestXAISubscriptionPlanNamesAndAliases(t *testing.T) {
	for _, test := range []struct{ tier, name string }{
		{"SuperGrok", "SuperGrok"}, {"SuperGrokPlus", "SuperGrok Plus"},
		{"SuperGrokPro", "SuperGrok Pro"}, {"SuperGrokHeavy", "SuperGrok Heavy"},
		{"XPremiumPlus", "X Premium+"}, {"SUBSCRIPTION_TIER_SUPER_GROK_HEAVY", "SuperGrok Heavy"},
	} {
		subscription, errParse := parseXAISubscription([]byte(fmt.Sprintf(`{"subscription_tier":%q}`, test.tier)))
		if errParse != nil || subscription.entitlement != xaiEntitlementPaid || subscription.plan == nil || subscription.plan.Name != test.name {
			t.Fatalf("subscription mapping failed: subscription=%+v error=%v", subscription, errParse)
		}
	}
}

type xaiTestRoundTripper func(*http.Request) (*http.Response, error)

func (f xaiTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestXAIProbeUsesParallelRequestsWithOneDeadline(t *testing.T) {
	now := time.Now().UTC()
	fixture := xaiPaidFixture(now)
	started := make(chan time.Time, 3)
	release := make(chan struct{})
	var finish sync.Once
	defer finish.Do(func() { close(release) })
	client := &http.Client{Transport: xaiTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			return nil, fmt.Errorf("probe has no deadline")
		}
		started <- deadline
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		body := fixture.monthly
		if r.URL.Path == "/v1/user" {
			body = fixture.profile
		} else if r.URL.Query().Get("format") == "credits" {
			body = fixture.weekly
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-parallel", XAIBillingURL: "http://quota.test/v1/billing", Now: func() time.Time { return now }, HTTPClient: func(*coreauth.Auth, time.Duration) (*http.Client, error) { return client, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *probeError, 1)
	go func() {
		_, failure := collector.probeXAI(ctx, &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}})
		done <- failure
	}()
	var shared time.Time
	for range 3 {
		select {
		case deadline := <-started:
			if shared.IsZero() {
				shared = deadline
			} else if !shared.Equal(deadline) {
				t.Fatal("requests do not share a total probe deadline")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("independent requests were serialized")
		}
	}
	finish.Do(func() { close(release) })
	select {
	case failure := <-done:
		if failure != nil {
			t.Fatalf("parallel probe failed: %v", failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parallel probe did not finish")
	}
}

func TestXAIQuotaV3RecollectsFreshLegacyAllowance(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "xai-legacy-free", "xai", map[string]any{"type": "xai", "access_token": "fixture"})
	now := time.Now().UTC()
	expiresAt := now.Add(30 * time.Minute)
	_, errSeed := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: "xai-legacy-free", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ExpiresAt: &expiresAt, NextProbeAt: &expiresAt,
		ParserVersion: 2, CollectorVersion: 2, ReplaceWindows: true,
		Windows: []cluster.QuotaWindow{{ID: "xai-weekly-usage", Scope: "account", Mode: "fixed", Status: "healthy", Unit: "percentage", Used: quotaFloatPtr(1), Limit: quotaFloatPtr(100), PeriodUnit: "week", PeriodValue: quotaFloatPtr(1), Source: "active_probe", ObservedAt: now}},
	})
	if errSeed != nil {
		t.Fatal(errSeed)
	}
	fixture := xaiPaidFixture(now)
	fixture.profile = `{"subscriptionTier":null}`
	server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
	collector := NewCollector(repo, Options{Owner: "xai-upgrade", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	collector.collect(context.Background())
	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-legacy-free", now)
	if errGet != nil || item.Plan == nil || item.Plan.Name != "Free" || len(item.Windows) != 0 || item.CollectionStatus != "success" {
		t.Fatalf("fresh legacy allowance survived the eligibility upgrade: item=%+v error=%v", item, errGet)
	}
}

func TestXAIEligibilityFailureRetainsLastObservationAndRecovers(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "xai-recovery", "xai", map[string]any{"type": "xai", "access_token": "fixture"})
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	fixture := xaiPaidFixture(now)
	var round atomic.Int32
	server := serveXAIProbeFixture(t, func() xaiProbeFixture {
		value := fixture
		switch round.Load() {
		case 0:
			value.weekly = strings.Replace(value.weekly, `"currentPeriod":`, `"creditUsagePercent":1,"currentPeriod":`, 1)
		case 1:
			value.profileStatus = http.StatusServiceUnavailable
			value.profile = `{"error":"private upstream details"}`
		}
		return value
	})
	collector := NewCollector(repo, Options{Owner: "xai-recovery", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
	auth := &coreauth.Auth{ID: "xai-recovery", Provider: "xai"}
	collector.collectCredential(context.Background(), auth, true)
	first, errFirst := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errFirst != nil || first.CollectionStatus != "success" || first.ObservedAt == nil || len(first.Windows) != 1 {
		t.Fatalf("initial observation was not persisted: item=%+v error=%v", first, errFirst)
	}
	round.Store(1)
	now = now.Add(time.Minute)
	collector.collectCredential(context.Background(), auth, true)
	failed, errFailed := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errFailed != nil || failed.CollectionStatus != "failed" || failed.Error == nil || failed.Error.Code != "UPSTREAM_UNAVAILABLE" || strings.Contains(failed.Error.Message, "private") {
		t.Fatalf("eligibility failure was hidden or exposed upstream details: item=%+v error=%v", failed, errFailed)
	}
	if failed.ObservedAt == nil || !failed.ObservedAt.Equal(*first.ObservedAt) || failed.LastAttemptAt == nil || !failed.LastAttemptAt.Equal(now) || failed.ConsecutiveFailure != 1 || failed.NextProbeAt == nil || !failed.NextProbeAt.After(now) {
		t.Fatalf("failure overwrote observation time or bypassed retry backoff: %+v", failed)
	}
	if failed.Plan == nil || failed.Plan.Name != "SuperGrok Plus" || len(failed.Windows) != 1 || failed.Windows[0].Remaining == nil || *failed.Windows[0].Remaining != 99 {
		t.Fatalf("eligibility outage replaced valid historical quota: %+v", failed)
	}
	round.Store(2)
	now = now.Add(time.Minute)
	collector.collectCredential(context.Background(), auth, true)
	recovered, errRecovered := repo.GetQuotaCredential(context.Background(), auth.ID, now)
	if errRecovered != nil || recovered.CollectionStatus != "success" || recovered.Error != nil || recovered.ConsecutiveFailure != 0 || recovered.ObservedAt == nil || !recovered.ObservedAt.Equal(now) || len(recovered.Windows) != 1 || recovered.Windows[0].Remaining == nil || *recovered.Windows[0].Remaining != 100 {
		t.Fatalf("confirmed paid zero usage did not recover cleanly: item=%+v error=%v", recovered, errRecovered)
	}
}

func TestXAIIndependentFailuresPreserveUsefulQuota(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, monthly, weekly, code string
		monthlyStatus, weeklyStatus int
		remaining                   float64
	}{
		{name: "monthly outage", monthlyStatus: http.StatusServiceUnavailable, code: "UPSTREAM_UNAVAILABLE", remaining: 100},
		{name: "monthly malformed", monthly: `[]`, code: "UPSTREAM_RESPONSE_INVALID", remaining: 100},
		{name: "weekly outdated", monthly: `{"config":{"monthlyLimit":15000,"used":1500}}`, weeklyStatus: http.StatusUpgradeRequired, code: "UPSTREAM_CLIENT_OUTDATED", remaining: 135},
		{name: "weekly malformed", monthly: `{"config":{"monthlyLimit":15000,"used":1500}}`, weekly: `[]`, code: "UPSTREAM_RESPONSE_INVALID", remaining: 135},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := xaiPaidFixture(now)
			fixture.monthlyStatus, fixture.weeklyStatus = test.monthlyStatus, test.weeklyStatus
			if test.monthly != "" {
				fixture.monthly = test.monthly
			}
			if test.weekly != "" {
				fixture.weekly = test.weekly
			}
			server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
			collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-independent", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			result, failure := collector.probeXAI(context.Background(), &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}})
			if failure != nil || !result.partial || result.collectionError == nil || result.collectionError.Code != test.code || result.plan == nil || result.plan.Name != "SuperGrok Plus" || len(result.windows) != 1 || result.windows[0].Remaining == nil || *result.windows[0].Remaining != test.remaining {
				t.Fatalf("one component failure discarded confirmed quota or subscription: result=%+v failure=%v", result, failure)
			}
		})
	}
}

func TestXAIRequestFailuresTakePriorityOverMissingQuota(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusUpgradeRequired, "UPSTREAM_CLIENT_OUTDATED"},
		{http.StatusUnauthorized, "UPSTREAM_AUTH_REJECTED"},
		{http.StatusTooManyRequests, "UPSTREAM_RATE_LIMITED"},
	} {
		t.Run(test.code, func(t *testing.T) {
			fixture := xaiPaidFixture(now)
			fixture.monthly, fixture.profile = `{}`, `{}`
			fixture.weeklyStatus = test.status
			server := serveXAIProbeFixture(t, func() xaiProbeFixture { return fixture })
			collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-errors", XAIBillingURL: server.URL + "/v1/billing", Now: func() time.Time { return now }})
			_, failure := collector.probeXAI(context.Background(), &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}})
			if failure == nil || failure.code != test.code || failure.statusCode != test.status {
				t.Fatalf("actionable request failure was hidden: %v", failure)
			}
		})
	}
}

func TestXAIProbeCancellationStopsEveryRequest(t *testing.T) {
	started, stopped := make(chan struct{}, 3), make(chan struct{}, 3)
	client := &http.Client{Transport: xaiTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		stopped <- struct{}{}
		return nil, r.Context().Err()
	})}
	collector := NewCollector(newCollectorTestRepository(t), Options{Owner: "xai-cancel", XAIBillingURL: "http://quota.test/v1/billing", HTTPClient: func(*coreauth.Auth, time.Duration) (*http.Client, error) { return client, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *probeError, 1)
	go func() {
		_, failure := collector.probeXAI(ctx, &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture"}})
		done <- failure
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("requests did not start")
		}
	}
	cancel()
	for range 3 {
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("request survived cancellation")
		}
	}
	select {
	case failure := <-done:
		if failure == nil {
			t.Fatal("cancelled requests produced a successful quota observation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled probe did not return")
	}
}
