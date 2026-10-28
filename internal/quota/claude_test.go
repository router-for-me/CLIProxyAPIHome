package quota

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestParseClaudeProfilePlans(t *testing.T) {
	for _, test := range []struct {
		body    string
		name    string
		premium bool
	}{
		{`{"organization":{"rate_limit_tier":"default_claude_max_20x"}}`, "Max 20x", true},
		{`{"organization":{"rate_limit_tier":"default_claude_max_5x"}}`, "Max 5x", true},
		{`{"organization":{"organization_type":"claude_team","subscription_status":"active"}}`, "Team", false},
		{`{"organization":{"organization_type":"claude_enterprise"}}`, "Enterprise", true},
		{`{"organization":{"organization_type":"claude_pro"}}`, "Pro", true},
		{`{"account":{"has_claude_max":true}}`, "Max", true},
		{`{"account":{"has_claude_pro":"true"}}`, "Pro", true},
		{`{"account":{"has_claude_pro":false,"has_claude_max":false}}`, "Free", false},
		{`{"account":{"email":"test@example.test"}}`, "", false},
		{`{"organization":{"organization_type":"claude_pro","subscription_status":"cancelled"}}`, "", false},
		{`invalid`, "", false},
	} {
		t.Run(test.body, func(t *testing.T) {
			plan := parseClaudeProfilePlan([]byte(test.body))
			if test.name == "" {
				if plan != nil {
					t.Fatalf("unrecognized profile produced plan: %+v", plan)
				}
			} else if plan == nil || plan.Name != test.name || plan.Premium != test.premium {
				t.Fatalf("plan = %+v, want %s, premium=%v", plan, test.name, test.premium)
			}
		})
	}
}

func TestParseClaudeResetCredits(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		body    string
		count   int
		unknown bool
		invalid bool
	}{
		{"not reported", `{"five_hour":{"utilization":10}}`, 0, true, false},
		{"explicit null", `{"five_hour":null,"cedar_ember":null}`, 0, true, false},
		{"empty response", `{}`, 0, false, true},
		{"in-band error", `{"error":"unavailable"}`, 0, false, true},
		{"not eligible", `{"cedar_ember":{"eligible":false}}`, 0, true, false},
		{"zero", `{"cedar_ember":{"eligible":true,"grants":[]}}`, 0, false, false},
		{"remaining before limit", `{"cedar_ember":{"eligible":true,"grants":[{"id":"full","resets_left":2,"usable_now":false,"use_requires_limit":true},{"id":"session","resets_left":1}]}}`, 3, false, false},
		{"expired paused and future", `{"cedar_ember":{"eligible":true,"grants":[{"id":"expired","resets_left":3,"ends_at":"2026-10-20T00:00:00Z"},{"id":"paused","resets_left":2,"paused":true},{"id":"future","resets_left":4,"starts_at":"2026-10-21T00:00:00Z"},{"id":"valid","resets_left":1,"ends_at":"2026-11-01T00:00:00Z"}]}}`, 1, false, false},
		{"missing grants", `{"cedar_ember":{"eligible":true}}`, 0, false, true},
		{"missing eligibility", `{"cedar_ember":{"grants":[]}}`, 0, false, true},
		{"missing count", `{"cedar_ember":{"eligible":true,"grants":[{"id":"a"}]}}`, 0, false, true},
		{"negative count", `{"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_left":-1}]}}`, 0, false, true},
		{"invalid expiry", `{"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_left":1,"ends_at":"bad"}]}}`, 0, false, true},
		{"duplicate", `{"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_left":1},{"id":"a","resets_left":1}]}}`, 0, false, true},
		{"overflow", `{"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_left":1000001}]}}`, 0, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credits, errParse := parseClaudeResetCredits([]byte(test.body), now)
			if test.invalid {
				if errParse == nil {
					t.Fatal("malformed grants were accepted")
				}
				return
			}
			if errParse != nil {
				t.Fatal(errParse)
			}
			if test.unknown {
				if credits != nil {
					t.Fatalf("unknown grants presented as a count: %+v", credits)
				}
				return
			}
			if credits == nil || credits.AvailableCount == nil || *credits.AvailableCount != test.count || !credits.ObservedAt.Equal(now) || len(credits.Credits) != 0 {
				t.Fatalf("unexpected reset grants: %+v", credits)
			}
		})
	}
}

func TestClaudePartialRefreshPreservesPlanAndReplacesResetObservation(t *testing.T) {
	for _, test := range []struct {
		name        string
		profile     string
		profileCode int
		resetCode   int
		resetBody   string
		wantResets  bool
		wantPartial bool
	}{
		{"profile rate limited", "", 429, 200, `{"cedar_ember":{"eligible":true,"grants":[{"id":"grant","resets_left":2}]}}`, true, true},
		{"profile invalid", `{"error":"invalid"}`, 200, 200, `{"cedar_ember":{"eligible":true,"grants":[{"id":"grant","resets_left":2}]}}`, true, true},
		{"profile unknown and reset fails", `{"account":{}}`, 200, 429, "", false, true},
		{"reset not reported", `{"account":{}}`, 200, 200, `{"five_hour":null}`, false, false},
		{"reset response invalid", `{"account":{}}`, 200, 200, `{"error":"unavailable"}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
			old := now.Add(-time.Hour)
			seedCollectorProviderAuth(t, repo, "claude", "claude", map[string]any{"type": "claude", "access_token": "synthetic-token"})
			count := 5
			_, errSeed := repo.UpsertQuotaSnapshot(t.Context(), cluster.QuotaSnapshotWrite{
				CredentialID: "claude", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
				ObservedAt: &old, ExpiresAt: &old, LastSuccessAt: &old, NextProbeAt: &old,
				Plan: &cluster.QuotaPlan{Name: "Max 5x", Premium: true}, ReplacePlan: true,
				ResetCredits: &cluster.QuotaResetCredits{AvailableCount: &count, ObservedAt: old}, ReplaceResetCredits: true,
			})
			if errSeed != nil {
				t.Fatal(errSeed)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Method != http.MethodGet {
					t.Errorf("collector must not consume grants: %s", request.Method)
				}
				if request.URL.Path == "/profile" {
					w.WriteHeader(test.profileCode)
					_, _ = w.Write([]byte(test.profile))
				} else if request.URL.Query().Get("cedar_ember") == "1" {
					if request.Header.Get("User-Agent") != claudeResetCreditsUserAgent {
						t.Error("reset observation must identify the supported Claude CLI protocol")
					}
					if request.URL.Query().Get("skip_spend") != "1" {
						t.Error("reset observation must use the read-only usage request")
					}
					w.WriteHeader(test.resetCode)
					_, _ = w.Write([]byte(test.resetBody))
				} else {
					_, _ = w.Write([]byte(`{"five_hour":{"utilization":20,"resets_at":"2026-10-20T05:00:00Z"}}`))
				}
			}))
			defer server.Close()
			collector := NewCollector(repo, Options{Owner: "home-a", Now: func() time.Time { return now }, ClaudeUsageURL: server.URL + "/usage", ClaudeProfileURL: server.URL + "/profile"})
			collector.collect(context.Background())
			item, errGet := repo.GetQuotaCredential(t.Context(), "claude", now)
			if errGet != nil {
				t.Fatal(errGet)
			}
			if requests.Load() != 3 || item.Plan == nil || item.Plan.Name != "Max 5x" || len(item.Windows) != 1 || (item.CollectionStatus == "partial") != test.wantPartial {
				t.Fatalf("refresh lost known data or status: requests=%d snapshot=%+v", requests.Load(), item)
			}
			if test.wantResets {
				if item.ResetCredits == nil || *item.ResetCredits.AvailableCount != 2 {
					t.Fatalf("reset grant observation missing: %+v", item.ResetCredits)
				}
			} else if item.ResetCredits != nil {
				t.Fatalf("old reset count still presented as current: %+v", item.ResetCredits)
			}
		})
	}
}

func TestParseClaudeUsageWindowsTreatsUtilizationAsPercent(t *testing.T) {
	observedAt := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		utilization string
		usedRatio   float64
		status      string
	}{
		{"0", 0, "healthy"},
		{"0.5", 0.005, "healthy"},
		{"1", 0.01, "healthy"},
		{"1.0", 0.01, "healthy"},
		{"17", 0.17, "healthy"},
		{"100", 1, "exhausted"},
		{"150", 1, "exhausted"},
	} {
		t.Run(test.utilization, func(t *testing.T) {
			body := `{"five_hour":{"utilization":` + test.utilization + `,"resets_at":"2026-10-28T12:40:00Z"}}`
			windows, errParse := parseClaudeUsageWindows([]byte(body), observedAt)
			if errParse != nil {
				t.Fatalf("parse: %v", errParse)
			}
			if len(windows) != 1 || windows[0].UsedRatio == nil {
				t.Fatalf("windows = %+v, want one window with a used ratio", windows)
			}
			if got := *windows[0].UsedRatio; math.Abs(got-test.usedRatio) > 1e-9 {
				t.Fatalf("used ratio = %v, want %v", got, test.usedRatio)
			}
			if windows[0].Status != test.status {
				t.Fatalf("status = %q, want %q", windows[0].Status, test.status)
			}
		})
	}
}
