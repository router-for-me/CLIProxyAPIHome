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
	requestHeaders := make(chan http.Header, 1)
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
	collector.collect(context.Background())
	second, errSecond := repo.GetQuotaCredential(context.Background(), "xai-plan-clear", now)
	if errSecond != nil {
		t.Fatalf("GetQuotaCredential() after plan change error = %v", errSecond)
	}
	if second.Plan != nil {
		t.Fatalf("stale plan was retained: %+v", second.Plan)
	}
}
