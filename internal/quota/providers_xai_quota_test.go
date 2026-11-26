package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestCollectorXAIKeepsWeeklyQuotaAfterMalformedMonthlyResponse(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-invalid-monthly", "xai", map[string]any{"type": "xai", "access_token": "test"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Grok-Client-Version") != "1.0.70" || r.Header.Get("User-Agent") != "grok-pager/1.0.70 grok-shell/1.0.70 (macos; aarch64)" {
			t.Errorf("outdated Grok CLI identity headers")
		}
		if r.URL.Query().Get("format") == "credits" {
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":1,"currentPeriod":{"type":"weekly","end":"2026-11-12T12:15:00Z"},"productUsage":[{"product":"GrokBuild","usagePercent":1}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"config":`))
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "xai-invalid-monthly-test", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	item, errGet := repo.GetQuotaCredential(context.Background(), "xai-invalid-monthly", now)
	if errGet != nil || item.CollectionStatus != "partial" || len(item.Windows) != 2 || item.Windows[0].Remaining == nil || *item.Windows[0].Remaining != 99 {
		t.Fatalf("weekly quota was lost after malformed monthly response: %+v error=%v", item, errGet)
	}
	if item.Error == nil || item.Error.Code != "UPSTREAM_RESPONSE_INVALID" || item.Windows[1].ID != "xai-product-grokbuild" {
		t.Fatalf("partial error or product usage was lost: %+v", item)
	}
}

func TestQuotaHTTPProbeErrorReportsOutdatedClient(t *testing.T) {
	failure := quotaHTTPProbeError(http.StatusUpgradeRequired, "request-1")
	if failure.code != "UPSTREAM_CLIENT_OUTDATED" || failure.statusCode != 426 || failure.retryable {
		t.Fatalf("unexpected outdated-client error: %+v", failure)
	}
}

func TestParseXAIWeeklyQuotaRequiresObservedUsage(t *testing.T) {
	window, ok := parseXAIWeeklyWindow([]byte(`{"config":{"currentPeriod":{"type":"weekly","end":"2026-11-12T12:15:00Z"}}}`), time.Now())
	if ok {
		t.Fatalf("weekly allowance inferred without observed usage or subscription entitlement: %+v", window)
	}
}

func TestCollectorXAIKeepsMonthlyQuotaAfterInvalidWeeklyShape(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `[]`, `{"config":[]}`} {
		t.Run(body, func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			seedCollectorProviderAuth(t, repo, "xai-weekly-invalid-shape", "xai", map[string]any{"type": "xai", "access_token": "test"})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("format") == "credits" {
					_, _ = w.Write([]byte(body))
					return
				}
				_, _ = w.Write([]byte(`{"config":{"monthlyLimit":15000,"used":100}}`))
			}))
			defer server.Close()
			collector := NewCollector(repo, Options{Owner: "xai-weekly-shape-test", XAIBillingURL: server.URL})
			collector.collect(context.Background())
			item, errGet := repo.GetQuotaCredential(context.Background(), "xai-weekly-invalid-shape", time.Now())
			if errGet != nil || item.CollectionStatus != "partial" || item.Error == nil || item.Error.Code != "UPSTREAM_RESPONSE_INVALID" || len(item.Windows) != 1 || item.Plan == nil {
				t.Fatalf("weekly response shape failure discarded monthly quota or went unreported: item=%+v error=%v", item, errGet)
			}
		})
	}
}

func TestParseXAIUsageRejectsMissingBillingConfig(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"config":null}`} {
		if _, _, errParse := parseXAIUsageWindows([]byte(body), time.Now()); errParse == nil {
			t.Fatal("missing monthly config was accepted as an authoritative empty plan")
		}
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
	requestHeaders := make(chan http.Header, 3)
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

func TestCollectorClearsPaidXAIQuotaAfterConfirmedFreeProfile(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "xai-plan-clear", "xai", map[string]any{"type": "xai", "access_token": "xai-secret"})
	var round atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			if round.Load() == 0 {
				_, _ = w.Write([]byte(`{"subscriptionTier":"SuperGrokHeavy"}`))
			} else {
				_, _ = w.Write([]byte(`{"subscriptionTier":null}`))
			}
			return
		}
		if r.URL.Query().Get("format") == "credits" {
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":1,"currentPeriod":{"type":"weekly"}}}`))
			return
		}
		if round.Load() == 0 {
			_, _ = w.Write([]byte(`{"config":{"monthlyLimit":150000,"used":1000}}`))
			return
		}
		_, _ = w.Write([]byte(`{"config":{"monthlyLimit":0,"used":0}}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{Owner: "home-a", XAIBillingURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	first, errFirst := repo.GetQuotaCredential(context.Background(), "xai-plan-clear", now)
	if errFirst != nil || first.Plan == nil {
		t.Fatalf("initial plan = %+v, error = %v", first, errFirst)
	}

	now = now.Add(31 * time.Minute)
	round.Store(1)
	accepted, errTrigger := collector.TriggerCollection(context.Background(), map[string]struct{}{"xai-plan-clear": {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("TriggerCollection() = %d, %v, want 1, nil", accepted, errTrigger)
	}
	collector.Wait()
	second, errSecond := repo.GetQuotaCredential(context.Background(), "xai-plan-clear", now)
	if errSecond != nil {
		t.Fatalf("GetQuotaCredential() after plan change error = %v", errSecond)
	}
	if second.Plan == nil || second.Plan.Name != "Free" || len(second.Windows) != 0 || second.CollectionStatus != "success" || second.QuotaStatus != "unknown" {
		t.Fatalf("paid quota was retained after a confirmed downgrade: %+v", second)
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
		if request.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"subscriptionTier":"SuperGrokPlus"}`))
			return
		}
		if request.URL.Query().Get("format") == "credits" {
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":16,"currentPeriod":{"type":"weekly","start":"2026-07-09T20:46:00Z","end":"2026-07-16T20:46:00Z"},"productUsage":[{"product":"GrokBuild","usagePercent":1}]}}`))
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
	if len(item.Windows) != 2 || item.Windows[0].ID != "xai-weekly-usage" || item.Windows[1].ID != "xai-product-grokbuild" {
		t.Fatalf("weekly-only account got windows = %+v", item.Windows)
	}
	if item.Windows[1].Label == nil || *item.Windows[1].Label != "GrokBuild" || item.Windows[1].Used == nil || *item.Windows[1].Used != 1 {
		t.Fatalf("unexpected product usage window: %+v", item.Windows[1])
	}
	weekly := item.Windows[0]
	if weekly.Status != "healthy" || weekly.Remaining == nil || *weekly.Remaining != 84 {
		t.Fatalf("unexpected weekly window state: %+v", weekly)
	}
	if item.Plan == nil || item.Plan.Name != "SuperGrok Plus" {
		t.Fatalf("weekly-only account lost its confirmed subscription: %+v", item.Plan)
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
	if item.CollectionStatus != "partial" || item.Error == nil {
		t.Fatalf("weekly probe failure was hidden: %+v", item)
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
		if request.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
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
