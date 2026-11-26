package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

const devinQuotaFixture = `{"userStatus":{"planStatus":{"planInfo":{"planName":"pro"},"planEnd":"2026-11-11T08:42:00Z","dailyQuotaRemainingPercent":65,"weeklyQuotaRemainingPercent":"83","dailyQuotaResetAtUnix":"1793980800","weeklyQuotaResetAtUnix":1794153600}}}`

func TestParseDevinQuotaDailyWeeklyAndPlan(t *testing.T) {
	now := time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)
	windows, plan, errParse := parseDevinQuota([]byte(devinQuotaFixture), now)
	if errParse != nil || len(windows) != 2 {
		t.Fatalf("parseDevinQuota(): windows=%+v error=%v", windows, errParse)
	}
	for index, remaining := range []float64{65, 83} {
		window := windows[index]
		if window.Remaining == nil || *window.Remaining != remaining || window.RemainingRatio == nil || *window.RemainingRatio != remaining/100 || window.ResetAt == nil || !window.ObservedAt.Equal(now) {
			t.Fatalf("unexpected quota window: %+v", window)
		}
	}
	if windows[0].ID != "devin-daily" || windows[0].PeriodUnit != "day" || windows[1].ID != "devin-weekly" || windows[1].PeriodUnit != "week" {
		t.Fatalf("unstable quota window identities: %+v", windows)
	}
	if plan == nil || plan.Name != "Pro" || plan.ExpiresAt == nil || plan.ExpiresAt.Format(time.RFC3339) != "2026-11-11T08:42:00Z" {
		t.Fatalf("unexpected subscription: %+v", plan)
	}
}

func TestParseDevinQuotaDoesNotInventMissingRemaining(t *testing.T) {
	now := time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)
	for _, body := range []string{
		`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":0,"weeklyQuotaResetAtUnix":1794153600}}}`,
		`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":"0","weeklyQuotaRemainingPercent":101,"weeklyQuotaResetAtUnix":"1794153600"}}}`,
	} {
		windows, _, errParse := parseDevinQuota([]byte(body), now)
		if errParse != nil || len(windows) != 2 {
			t.Fatalf("parse partial quota: windows=%+v error=%v", windows, errParse)
		}
		if windows[0].Status != "exhausted" || windows[0].Remaining == nil || *windows[0].Remaining != 0 {
			t.Fatalf("valid zero remaining was lost: %+v", windows[0])
		}
		if windows[1].Remaining != nil || windows[1].RemainingRatio != nil || windows[1].Status != "unknown" {
			t.Fatalf("missing remaining was fabricated: %+v", windows[1])
		}
	}
	windows, _, errParse := parseDevinQuota([]byte(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":-1,"dailyQuotaResetAtUnix":1.5,"weeklyQuotaResetAtUnix":"99999999999999999"}}}`), now)
	if errParse != nil || len(windows) != 0 {
		t.Fatalf("invalid quota facts were accepted: windows=%+v error=%v", windows, errParse)
	}
}

func TestParseDevinQuotaRejectsExpiredCycle(t *testing.T) {
	now := time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)
	body := strings.Replace(devinQuotaFixture, `"dailyQuotaResetAtUnix":"1793980800"`, `"dailyQuotaResetAtUnix":1`, 1)
	windows, _, errParse := parseDevinQuota([]byte(body), now)
	if errParse != nil || len(windows) != 1 || windows[0].ID != "devin-weekly" || windows[0].Remaining == nil || *windows[0].Remaining != 83 {
		t.Fatalf("expired daily cycle survived or current weekly cycle was lost: windows=%+v error=%v", windows, errParse)
	}
}

func TestCollectorPersistsDevinBodyAuthenticatedQuota(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "devin-quota", "devin", map[string]any{"type": "devin", "auth_kind": "oauth", "api_key": "devin-session-token$test"})
	now := time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Devin request method or headers")
		}
		var body struct {
			Metadata struct {
				APIKey string `json:"apiKey"`
				IDE    string `json:"ideName"`
			} `json:"metadata"`
		}
		if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil || body.Metadata.APIKey != "devin-session-token$test" || body.Metadata.IDE != "chisel" {
			t.Errorf("Devin body authentication was not resolved")
		}
		_, _ = w.Write([]byte(devinQuotaFixture))
	}))
	defer server.Close()
	collector := NewCollector(repo, Options{Owner: "devin-test", DevinQuotaURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	item, errGet := repo.GetQuotaCredential(context.Background(), "devin-quota", now)
	if errGet != nil || requests.Load() != 1 || item.CollectionStatus != "success" || len(item.Windows) != 2 || len(item.PrimaryWindows) != 2 {
		t.Fatalf("Devin quota was not collected: item=%+v requests=%d error=%v", item, requests.Load(), errGet)
	}
	if item.Plan == nil || item.Plan.ExpiresAt == nil || item.Plan.Name != "Pro" {
		t.Fatalf("Devin subscription did not survive persistence: %+v", item.Plan)
	}
}

func TestCollectorDevinRetainsPlanWhenOptionalMetadataIsInvalid(t *testing.T) {
	repo := newCollectorTestRepository(t)
	seedCollectorProviderAuth(t, repo, "devin-plan-retained", "devin", map[string]any{"type": "devin", "api_key": "test"})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := devinQuotaFixture
		if requests.Add(1) > 1 {
			body = strings.Replace(body, `"planInfo":{"planName":"pro"}`, `"planInfo":[]`, 1)
			body = strings.Replace(body, `"planEnd":"2026-11-11T08:42:00Z"`, `"planEnd":123`, 1)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)
	collector := NewCollector(repo, Options{Owner: "devin-plan-retained-test", DevinQuotaURL: server.URL, Now: func() time.Time { return now }})
	collector.collect(context.Background())
	now = now.Add(time.Minute)
	collector.collectCredential(context.Background(), &coreauth.Auth{ID: "devin-plan-retained", Provider: "devin"}, true)
	item, errGet := repo.GetQuotaCredential(context.Background(), "devin-plan-retained", now)
	if errGet != nil || len(item.Windows) != 2 || item.CollectionStatus != "success" || item.Plan == nil || item.Plan.Name != "Pro" || item.Plan.ExpiresAt == nil {
		t.Fatalf("optional metadata corrupted valid quota or cleared the plan: item=%+v error=%v", item, errGet)
	}
}

func TestParseDevinQuotaIgnoresInvalidSubscriptionExpiry(t *testing.T) {
	for _, expiry := range []string{`null`, `123`, `"0001-01-01T00:00:00Z"`, `"not-a-date"`} {
		body := strings.Replace(devinQuotaFixture, `"2026-11-11T08:42:00Z"`, expiry, 1)
		windows, plan, errParse := parseDevinQuota([]byte(body), time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC))
		if errParse != nil || len(windows) != 2 || plan == nil || plan.ExpiresAt != nil {
			t.Fatalf("invalid optional plan expiry discarded quota: windows=%d plan=%+v error=%v", len(windows), plan, errParse)
		}
	}
}

func TestCollectorDevinMissingTokenAndPartialQuota(t *testing.T) {
	repo := newCollectorTestRepository(t)
	collector := NewCollector(repo, Options{Owner: "devin-missing"})
	_, errMissing := collector.probeDevin(context.Background(), &coreauth.Auth{Provider: "devin"})
	if errMissing == nil || errMissing.code != "AUTH_TOKEN_UNAVAILABLE" {
		t.Fatalf("missing session token was not rejected")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":65}}}`))
	}))
	defer server.Close()
	collector.options.DevinQuotaURL = server.URL
	result, errProbe := collector.probeDevin(context.Background(), &coreauth.Auth{Metadata: map[string]any{"session_token": "test"}})
	if errProbe != nil || !result.partial || result.collectionError == nil || len(result.windows) != 1 {
		t.Fatalf("partial Devin response was not preserved: %+v error=%v", result, errProbe)
	}
	for _, auth := range []*coreauth.Auth{
		{ID: "disabled", Provider: "devin", Disabled: true},
		{ID: "api-key", Provider: "devin", Attributes: map[string]string{"auth_kind": "provider_api_key"}},
	} {
		if quotaProbeEligible(auth) {
			t.Fatalf("ineligible Devin credential was accepted")
		}
	}
	_, _, errInvalid := parseDevinQuota([]byte(strings.Repeat("x", 5)), time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC))
	if errInvalid == nil {
		t.Fatal("malformed upstream JSON was accepted")
	}
}
