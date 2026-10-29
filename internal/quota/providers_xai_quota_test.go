package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseXAIUsageWindowsWithPlanAndOnDemand(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	body := []byte(`{
		"config": {
			"monthlyLimit": {"val": "150000"},
			"used": {"val": 151200},
			"onDemandCap": {"val": 5000},
			"onDemandUsed": "1200",
			"billingPeriodEnd": "2026-08-01T00:00:00Z"
		}
	}`)
	windows, plan, errParse := parseXAIUsageWindows(body, now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2 (monthly + on-demand)", len(windows))
	}
	monthly := windows[0]
	if monthly.ID != "xai-monthly-spend" || monthly.Limit == nil || *monthly.Limit != 1500 || monthly.Used == nil || *monthly.Used != 1500 || monthly.Remaining == nil || *monthly.Remaining != 0 {
		t.Fatalf("unexpected monthly window: %+v", monthly)
	}
	onDemand := windows[1]
	if onDemand.ID != "xai-on-demand" || onDemand.Unit != "currency" || onDemand.Limit == nil || *onDemand.Limit != 50 || onDemand.Used == nil || *onDemand.Used != 12 || onDemand.Remaining == nil || *onDemand.Remaining != 38 {
		t.Fatalf("unexpected on-demand window: %+v", onDemand)
	}
	if onDemand.PeriodUnit != "month" || onDemand.PeriodValue == nil || *onDemand.PeriodValue != 1 || onDemand.ResetAt == nil {
		t.Fatalf("unexpected on-demand period: %+v", onDemand)
	}
	if plan == nil || plan.Name != "SuperGrok Heavy" || !plan.Premium {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestParseXAIUsageWindowsAcceptsSnakeCaseAndScalarValues(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	body := []byte(`{
		"config": {
			"monthly_limit": 15000,
			"used": "4200",
			"on_demand_cap": "5000",
			"billing_period_end": "2026-08-01T00:00:00Z"
		}
	}`)
	windows, plan, errParse := parseXAIUsageWindows(body, now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 2 || windows[0].Limit == nil || *windows[0].Limit != 150 || windows[1].Remaining == nil || *windows[1].Remaining != 50 {
		t.Fatalf("unexpected scalar/snake_case windows: %+v", windows)
	}
	if plan == nil || plan.Name != "SuperGrok" || plan.Premium {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestParseXAIUsageWindowsDoesNotRecursivelyUnwrapValues(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	windows, plan, errParse := parseXAIUsageWindows([]byte(`{"config":{"monthlyLimit":{"val":{"val":15000}}}}`), now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 0 || plan != nil {
		t.Fatalf("nested value wrappers were accepted: windows=%+v plan=%+v", windows, plan)
	}
}

func TestParseXAIUsageWindowsUnknownMonthlyLimitHasNoPlan(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	windows, plan, errParse := parseXAIUsageWindows([]byte(`{"config":{"monthlyLimit":20000,"used":167,"onDemandCap":0}}`), now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil for an unknown monthly limit", plan)
	}
}

func TestParseXAIUsageWindowsDoesNotInventMissingUsage(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	windows, plan, errParse := parseXAIUsageWindows([]byte(`{"config":{"monthlyLimit":15000,"onDemandCap":5000}}`), now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 2 || plan == nil {
		t.Fatalf("unexpected windows or plan: windows=%+v plan=%+v", windows, plan)
	}
	for _, window := range windows {
		if window.Used != nil || window.Remaining != nil || window.UsedRatio != nil || window.RemainingRatio != nil || window.Status != "unknown" {
			t.Fatalf("missing usage was invented for window: %+v", window)
		}
	}
	if status := quotaWindowAggregateStatus(windows); status != "unknown" {
		t.Fatalf("aggregate status = %q, want unknown when usage is missing", status)
	}
}

func TestCollectorPersistsXAIPlanAndSendsCLIHeaders(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-plan-probe", "xai", map[string]any{"type": "xai", "access_token": "xai-secret", "sub": "xai-user"})
	requestHeaders := make(chan http.Header, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestHeaders <- request.Header.Clone()
		_, _ = w.Write([]byte(`{"config":{"monthlyLimit":{"val":150000},"used":{"val":15000},"onDemandCap":{"val":5000},"onDemandUsed":{"val":1200},"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-plan-probe", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.Plan == nil || item.Plan.Name != "SuperGrok Heavy" || !item.Plan.Premium {
		t.Fatalf("unexpected persisted plan: %+v", item.Plan)
	}
	if len(item.Windows) != 2 || item.Windows[0].ID != "xai-monthly-spend" || item.Windows[1].ID != "xai-on-demand" {
		t.Fatalf("unexpected persisted windows: %+v", item.Windows)
	}

	select {
	case headers := <-requestHeaders:
		if headers.Get("Authorization") != "Bearer xai-secret" || headers.Get("Accept") != "*/*" || headers.Get(xaiTokenAuthHeader) != xaiTokenAuthValue || headers.Get(xaiClientVersionHeader) != xaiClientVersionValue || headers.Get("User-Agent") != xaiGrokUserAgent || headers.Get("x-userid") != "xai-user" {
			t.Fatalf("unexpected xAI request headers: %v", headers)
		}
	case <-time.After(time.Second):
		t.Fatal("xAI probe request was not observed")
	}
}

func TestCollectorClearsStaleXAIPlan(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-plan-clear", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"config":{"monthlyLimit":150000,"used":1000}}`))
			return
		}
		_, _ = w.Write([]byte(`{"config":{"monthlyLimit":20000,"used":1000}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	first, errFirst := repo.GetQuotaCredential(context.Background(), "xai-plan-clear", now)
	if errFirst != nil || first.Plan == nil {
		t.Fatalf("initial plan = %+v, error = %v", first, errFirst)
	}

	now = now.Add(31 * time.Minute)
	accepted, errTrigger := collector.TriggerCollection(context.Background(), map[string]struct{}{"xai-plan-clear": {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("TriggerCollection() = %d, %v, want 1, nil", accepted, errTrigger)
	}
	collector.Wait()
	second, errSecond := repo.GetQuotaCredential(context.Background(), "xai-plan-clear", now)
	if errSecond != nil {
		t.Fatalf("GetQuotaCredential() after plan change error = %v", errSecond)
	}
	if second.Plan != nil {
		t.Fatalf("stale plan was retained: %+v", second.Plan)
	}
}

func TestParseXAIWeeklyWindowReadsCreditsPayload(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	body := []byte(`{
		"config": {
			"creditUsagePercent": {"val": "16"},
			"currentPeriod": {"type": "weekly", "start": "2026-07-09T20:46:00Z", "end": "2026-07-16T20:46:00Z"},
			"monthlyLimit": {"val": "0"},
			"used": {"val": "0"}
		}
	}`)
	window, ok := parseXAIWeeklyWindow(body, now)
	if !ok {
		t.Fatal("parseXAIWeeklyWindow() did not accept the credits payload")
	}
	if window.ID != "xai-weekly-usage" || window.Unit != "percentage" || window.Used == nil || *window.Used != 16 || window.Limit == nil || *window.Limit != 100 || window.Remaining == nil || *window.Remaining != 84 {
		t.Fatalf("unexpected weekly window: %+v", window)
	}
	if window.Status != "healthy" || window.RemainingRatio == nil || *window.RemainingRatio != 0.84 {
		t.Fatalf("unexpected weekly status: %+v", window)
	}
	if window.ResetAt == nil || !window.ResetAt.Equal(time.Date(2026, 7, 16, 20, 46, 0, 0, time.UTC)) {
		t.Fatalf("unexpected weekly reset: %+v", window.ResetAt)
	}
	if window.PeriodUnit != "week" || window.PeriodValue == nil || *window.PeriodValue != 1 || window.Priority >= 0 {
		t.Fatalf("unexpected weekly period/priority: %+v", window)
	}
}

func TestParseXAIWeeklyWindowRejectsMonthlyPeriodAndMissingUsage(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	monthly := []byte(`{"config":{"creditUsagePercent":42,"currentPeriod":{"type":"monthly","end":"2026-08-01T00:00:00Z"}}}`)
	if window, ok := parseXAIWeeklyWindow(monthly, now); ok {
		t.Fatalf("monthly period must not become a weekly window: %+v", window)
	}
	missing := []byte(`{"config":{"monthlyLimit":15000,"used":100}}`)
	if window, ok := parseXAIWeeklyWindow(missing, now); ok {
		t.Fatalf("payload without credit usage must not become a weekly window: %+v", window)
	}
}

func TestParseXAIWeeklyWindowAcceptsMissingUsageWithWeeklyPeriod(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	body := []byte(`{
		"config": {
			"currentPeriod": {"type": "USAGE_PERIOD_TYPE_WEEKLY", "start": "2026-07-16T00:00:00Z", "end": "2026-07-23T00:00:00Z"},
			"onDemandCap": {"val": 0},
			"isUnifiedBillingUser": true
		}
	}`)
	window, ok := parseXAIWeeklyWindow(body, now)
	if !ok {
		t.Fatal("parseXAIWeeklyWindow() rejected a weekly period without creditUsagePercent")
	}
	if window.ID != "xai-weekly-usage" || window.Limit == nil || *window.Limit != 100 || window.Used == nil || *window.Used != 0 || window.Status != "healthy" {
		t.Fatalf("unexpected weekly window: %+v", window)
	}
	if window.ResetAt == nil || !window.ResetAt.Equal(time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected weekly reset: %+v", window.ResetAt)
	}
}

func TestXAIProfileRequestURLAddsSubscriptionInclude(t *testing.T) {
	got := xaiProfileRequestURL(xaiProfileURL)
	if got != xaiProfileURL+"?include=subscription" {
		t.Fatalf("xaiProfileRequestURL() = %q", got)
	}
	derived := xaiProfileRequestURL(xaiProfileURLFromBillingURL("http://127.0.0.1:8327/v1/billing"))
	if !strings.HasPrefix(derived, "http://127.0.0.1:8327/v1/user?") || !strings.Contains(derived, "include=subscription") {
		t.Fatalf("derived profile URL = %q", derived)
	}
}

func TestParseXAISubscriptionTier(t *testing.T) {
	tests := []struct {
		body        string
		wantTier    string
		wantKnown   bool
		wantPaid    bool
		wantPlan    string
		wantPremium bool
	}{
		{body: `{"subscriptionTier":"SuperGrokPlus"}`, wantTier: "SuperGrokPlus", wantKnown: true, wantPaid: true, wantPlan: "SuperGrok Plus"},
		{body: `{"subscriptionTier":"SuperGrokHeavy"}`, wantTier: "SuperGrokHeavy", wantKnown: true, wantPaid: true, wantPlan: "SuperGrok Heavy", wantPremium: true},
		{body: `{"subscriptionTier":null}`, wantTier: "", wantKnown: true, wantPaid: false},
		{body: `{"subscriptionTier":"Free"}`, wantTier: "Free", wantKnown: true, wantPaid: false},
		{body: `{"email":"a@b.c"}`, wantTier: "", wantKnown: false, wantPaid: false},
		{body: `not-json`, wantTier: "", wantKnown: false, wantPaid: false},
	}
	for _, test := range tests {
		tier, known := parseXAISubscriptionTier([]byte(test.body))
		if tier != test.wantTier || known != test.wantKnown {
			t.Fatalf("parseXAISubscriptionTier(%s) = %q, %v; want %q, %v", test.body, tier, known, test.wantTier, test.wantKnown)
		}
		if paid := xaiTierPaid(tier); paid != test.wantPaid {
			t.Fatalf("xaiTierPaid(%q) = %v; want %v", tier, paid, test.wantPaid)
		}
		plan := xaiPlanFromSubscriptionTier(tier)
		if test.wantPlan == "" {
			if plan != nil {
				t.Fatalf("xaiPlanFromSubscriptionTier(%q) = %+v; want nil", tier, plan)
			}
			continue
		}
		if plan == nil || plan.Name != test.wantPlan || plan.Premium != test.wantPremium {
			t.Fatalf("xaiPlanFromSubscriptionTier(%q) = %+v; want %q premium=%v", tier, plan, test.wantPlan, test.wantPremium)
		}
	}
}

func TestCollectorXAIFreeAccountHasNoWeeklyQuota(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-free", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/v1/user"):
			if request.URL.Query().Get("include") != "subscription" {
				t.Errorf("profile include = %q, want subscription", request.URL.Query().Get("include"))
			}
			_, _ = w.Write([]byte(`{"email":"free@example.com","subscriptionTier":null}`))
		case request.URL.Query().Get("format") == "credits":
			_, _ = w.Write([]byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-22T00:00:00Z","end":"2026-10-29T00:00:00Z"}}}`))
		default:
			_, _ = w.Write([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`))
		}
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-free", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if len(item.Windows) != 0 || item.Plan != nil {
		t.Fatalf("free account got windows=%+v plan=%+v", item.Windows, item.Plan)
	}
	if item.CollectionStatus != "failed" || item.Error == nil || item.Error.Code != "UPSTREAM_RESPONSE_INVALID" {
		t.Fatalf("free account collection = %q error=%+v; want failed without usable quota", item.CollectionStatus, item.Error)
	}
}

func TestCollectorSuperGrokPlusZeroUsageWeeklyQuota(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-plus", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/v1/user"):
			if request.URL.Query().Get("include") != "subscription" {
				t.Errorf("profile include = %q, want subscription", request.URL.Query().Get("include"))
			}
			_, _ = w.Write([]byte(`{"email":"plus@example.com","subscriptionTier":"SuperGrokPlus"}`))
		case request.URL.Query().Get("format") == "credits":
			_, _ = w.Write([]byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-22T11:26:03Z","end":"2026-10-29T11:26:03Z"},"isUnifiedBillingUser":true}}`))
		default:
			_, _ = w.Write([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`))
		}
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-plus", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "success" || item.QuotaStatus != "healthy" {
		t.Fatalf("paid account collection = %q/%q error=%+v", item.QuotaStatus, item.CollectionStatus, item.Error)
	}
	if len(item.Windows) != 1 || item.Windows[0].ID != "xai-weekly-usage" || item.Windows[0].Used == nil || *item.Windows[0].Used != 0 || item.Windows[0].Limit == nil || *item.Windows[0].Limit != 100 {
		t.Fatalf("paid account windows = %+v", item.Windows)
	}
	if item.Plan == nil || item.Plan.Name != "SuperGrok Plus" || item.Plan.Premium {
		t.Fatalf("paid account plan = %+v", item.Plan)
	}
}

func TestParseXAIUsageWindowsSkipsZeroMonthlyLimit(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	windows, plan, errParse := parseXAIUsageWindows([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`), now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 0 || plan != nil {
		t.Fatalf("zero monthly limit produced windows=%+v plan=%+v", windows, plan)
	}

	windows, _, errParse = parseXAIUsageWindows([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":5000}}`), now)
	if errParse != nil {
		t.Fatalf("parseXAIUsageWindows() error = %v", errParse)
	}
	if len(windows) != 1 || windows[0].ID != "xai-on-demand" {
		t.Fatalf("on-demand-only account got windows = %+v", windows)
	}
}

func TestCollectorCollectsXAIWeeklyQuota(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-weekly-probe", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("format") == "credits" {
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":16,"currentPeriod":{"type":"weekly","start":"2026-07-09T20:46:00Z","end":"2026-07-16T20:46:00Z"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-weekly-probe", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if len(item.Windows) != 1 || item.Windows[0].ID != "xai-weekly-usage" {
		t.Fatalf("weekly-only account got windows = %+v", item.Windows)
	}
	weekly := item.Windows[0]
	if weekly.Status != "healthy" || weekly.Remaining == nil || *weekly.Remaining != 84 {
		t.Fatalf("unexpected weekly window state: %+v", weekly)
	}
	if item.Plan != nil {
		t.Fatalf("weekly-only account must not get a subscription plan: %+v", item.Plan)
	}
}

func TestCollectorToleratesXAIWeeklyProbeFailure(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-weekly-fail", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("format") == "credits" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"config":{"monthlyLimit":15000,"used":4200,"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-weekly-fail", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if len(item.Windows) != 1 || item.Windows[0].ID != "xai-monthly-spend" {
		t.Fatalf("monthly windows lost after weekly probe failure: %+v", item.Windows)
	}
	if item.Plan == nil || item.Plan.Name != "SuperGrok" {
		t.Fatalf("plan lost after weekly probe failure: %+v", item.Plan)
	}
}

func TestCollectorReportsXAIWeeklyProbeErrorWhenMonthlyYieldsNoWindows(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-weekly-error", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("format") == "credits" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"config":{"monthlyLimit":0,"used":0,"onDemandCap":0}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-weekly-error", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "failed" || item.Error == nil || item.Error.Code != "UPSTREAM_UNAVAILABLE" {
		t.Fatalf("weekly probe failure must surface the upstream error, got status=%q error=%+v", item.CollectionStatus, item.Error)
	}
}

func TestCollectorKeepsXAIPlanWhenMonthlyProbeFails(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-monthly-fail", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	var monthlyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("format") == "credits" {
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":16,"currentPeriod":{"type":"weekly","end":"2026-07-16T20:46:00Z"}}}`))
			return
		}
		if monthlyRequests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"config":{"monthlyLimit":15000,"used":4200,"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	first, errFirst := repo.GetQuotaCredential(context.Background(), "xai-monthly-fail", now)
	if errFirst != nil || first.Plan == nil || first.Plan.Name != "SuperGrok" {
		t.Fatalf("initial plan = %+v, error = %v", first, errFirst)
	}

	now = now.Add(31 * time.Minute)
	accepted, errTrigger := collector.TriggerCollection(context.Background(), map[string]struct{}{"xai-monthly-fail": {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("TriggerCollection() = %d, %v, want 1, nil", accepted, errTrigger)
	}
	collector.Wait()
	second, errSecond := repo.GetQuotaCredential(context.Background(), "xai-monthly-fail", now)
	if errSecond != nil {
		t.Fatalf("GetQuotaCredential() after monthly outage error = %v", errSecond)
	}
	if second.CollectionStatus != "partial" || second.Error == nil || second.Error.Code != "UPSTREAM_UNAVAILABLE" {
		t.Fatalf("monthly outage must mark the collection partial, got status=%q error=%+v", second.CollectionStatus, second.Error)
	}
	if second.Plan == nil || second.Plan.Name != "SuperGrok" {
		t.Fatalf("stored plan cleared by a monthly outage: %+v", second.Plan)
	}
	if len(second.Windows) != 1 || second.Windows[0].ID != "xai-weekly-usage" {
		t.Fatalf("weekly window lost after monthly outage: %+v", second.Windows)
	}
}
