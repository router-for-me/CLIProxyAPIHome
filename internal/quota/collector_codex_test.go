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

func TestCollectorPersistsCodexPlanWindowsAndResetCredits(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-full", map[string]any{"type": "codex", "access_token": "probe-secret", "account_id": "acct-123"})

	var usageRequests atomic.Int32
	var resetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer probe-secret" || request.Header.Get("Chatgpt-Account-Id") != "acct-123" {
			t.Errorf("unexpected Codex headers: %#v", request.Header)
		}
		switch request.URL.Path {
		case "/usage":
			usageRequests.Add(1)
			_, _ = w.Write([]byte(`{
				"plan_type":"pro",
				"rate_limit_reset_credits":{"available_count":3},
				"rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800,"reset_at":1785296520}},
				"additional_rate_limits":[{
					"limit_name":"GPT-5.3-Codex-Spark",
					"metered_feature":"codex_bengalfox",
					"rate_limit":{
						"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1785301740},
						"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1785301740}
					}
				}]
			}`))
		case "/resets":
			resetRequests.Add(1)
			_, _ = w.Write([]byte(`{
				"available_count":3,
				"credits":[
					{"id":"credit-3","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-07-22T00:00:00Z","expires_at":"2026-08-13T01:36:15Z"},
					{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-07-20T00:00:00Z","expires_at":"2026-07-27T07:40:51Z"},
					{"id":"credit-2","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-07-21T00:00:00Z","expires_at":"2026-08-01T03:52:39Z"}
				]
			}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner: "home-a", HomeID: "home-a", CodexUsageURL: server.URL + "/usage",
		CodexResetCreditsURL: server.URL + "/resets", Now: func() time.Time { return now },
	})
	collector.collect(context.Background())

	if usageRequests.Load() != 1 || resetRequests.Load() != 1 {
		t.Fatalf("request counts = usage %d reset %d, want 1/1", usageRequests.Load(), resetRequests.Load())
	}
	item, errGet := repo.GetQuotaCredential(context.Background(), "codex-full", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "success" || item.Source == nil || *item.Source != "active_probe" || item.Plan == nil || item.Plan.Name != "Pro 20x" || !item.Plan.Premium {
		t.Fatalf("Codex snapshot metadata = %+v", item)
	}
	if len(item.Windows) != 2 {
		t.Fatalf("Codex windows = %+v, want two", item.Windows)
	}
	windows := make(map[string]cluster.QuotaWindow, len(item.Windows))
	for _, window := range item.Windows {
		windows[window.ID] = window
	}
	ordinary, ordinaryOK := windows["codex-1-week"]
	spark, sparkOK := windows["codex-bengalfox-1-week"]
	if !ordinaryOK || ordinary.RemainingRatio == nil || math.Abs(*ordinary.RemainingRatio-0.91) > 1e-9 {
		t.Fatalf("ordinary weekly window = %+v", ordinary)
	}
	if !sparkOK || spark.RemainingRatio == nil || *spark.RemainingRatio != 1 || spark.ScopeID == nil || *spark.ScopeID != "codex_bengalfox" {
		t.Fatalf("Spark weekly window = %+v", spark)
	}
	if item.ResetCredits == nil || item.ResetCredits.AvailableCount == nil || *item.ResetCredits.AvailableCount != 3 || len(item.ResetCredits.Credits) != 3 {
		t.Fatalf("reset credits = %+v", item.ResetCredits)
	}
	if item.ResetCredits.Credits[0].ID != "credit-1" || item.ResetCredits.Credits[1].ID != "credit-2" || item.ResetCredits.Credits[2].ID != "credit-3" {
		t.Fatalf("sorted reset credits = %+v", item.ResetCredits.Credits)
	}
}

func TestCollectorCodexResetDetailsFailurePreservesLastKnownDetails(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	seedCollectorAuth(t, repo, "codex-reset-failure", map[string]any{"type": "codex", "access_token": "probe-secret"})
	oldObservedAt := now.Add(-time.Hour)
	oldExpiresAt := now.Add(-time.Minute)
	oldResetExpiry := now.Add(72 * time.Hour)
	availableCount := 3
	remaining := 0.5
	period := float64(1)
	_, errSeed := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: "codex-reset-failure", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &oldObservedAt, ExpiresAt: &oldExpiresAt, LastSuccessAt: &oldObservedAt, NextProbeAt: &oldExpiresAt,
		Plan: &cluster.QuotaPlan{Name: "Pro 20x", Premium: true}, ReplacePlan: true,
		ResetCredits: &cluster.QuotaResetCredits{
			AvailableCount: &availableCount, ObservedAt: oldObservedAt,
			Credits: []cluster.QuotaResetCredit{{ID: "old-credit", Status: "available", GrantedAt: oldObservedAt.Add(-time.Hour), ExpiresAt: &oldResetExpiry}},
		},
		ReplaceResetCredits: true, ReplaceWindows: true,
		Windows: []cluster.QuotaWindow{{
			ID: "codex-1-week", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage",
			RemainingRatio: &remaining, PeriodUnit: "week", PeriodValue: &period, Source: "active_probe", ObservedAt: oldObservedAt,
		}},
	})
	if errSeed != nil {
		t.Fatalf("seed quota snapshot: %v", errSeed)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/usage":
			_, _ = w.Write([]byte(`{
				"plan_type":"pro",
				"rate_limit_reset_credits":{"available_count":3},
				"rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800,"reset_at":1785296520}}
			}`))
		case "/resets":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner: "home-a", CodexUsageURL: server.URL + "/usage",
		CodexResetCreditsURL: server.URL + "/resets", Now: func() time.Time { return now },
	})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "codex-reset-failure", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "partial" || item.Error == nil || item.Error.Code != "UPSTREAM_UNAVAILABLE" || len(item.Windows) != 1 || item.Windows[0].ID != "codex-1-week" || item.Windows[0].RemainingRatio == nil || math.Abs(*item.Windows[0].RemainingRatio-0.91) > 1e-9 {
		t.Fatalf("partial Codex result = %+v", item)
	}
	if item.ResetCredits == nil || item.ResetCredits.AvailableCount == nil || *item.ResetCredits.AvailableCount != 3 || len(item.ResetCredits.Credits) != 1 || item.ResetCredits.Credits[0].ID != "old-credit" || !item.ResetCredits.ObservedAt.Equal(oldObservedAt) {
		t.Fatalf("last-known reset details were not preserved: %+v", item.ResetCredits)
	}
}

func TestCollectorCodexResetCountControlsDetailsRequest(t *testing.T) {
	for _, test := range []struct {
		name           string
		resetSummary   string
		wantCount      int
		wantOldDetails bool
	}{
		{name: "zero clears details without request", resetSummary: `,"rate_limit_reset_credits":{"available_count":0}`, wantCount: 0},
		{name: "missing preserves details without request", resetSummary: "", wantCount: 2, wantOldDetails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			now := time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC)
			credentialID := "codex-reset-count-" + test.name
			seedCollectorAuth(t, repo, credentialID, map[string]any{"type": "codex", "access_token": "probe-secret"})
			oldObservedAt := now.Add(-time.Hour)
			oldExpiresAt := now.Add(-time.Minute)
			oldResetExpiry := now.Add(48 * time.Hour)
			oldCount := 2
			remaining := 0.5
			period := float64(1)
			_, errSeed := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
				CredentialID: credentialID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
				ObservedAt: &oldObservedAt, ExpiresAt: &oldExpiresAt, NextProbeAt: &oldExpiresAt,
				ResetCredits: &cluster.QuotaResetCredits{
					AvailableCount: &oldCount, ObservedAt: oldObservedAt,
					Credits: []cluster.QuotaResetCredit{{ID: "old-credit", Status: "available", GrantedAt: oldObservedAt.Add(-time.Hour), ExpiresAt: &oldResetExpiry}},
				},
				ReplaceResetCredits: true, ReplaceWindows: true,
				Windows: []cluster.QuotaWindow{{ID: "codex-1-week", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: "week", PeriodValue: &period, Source: "active_probe", ObservedAt: oldObservedAt}},
			})
			if errSeed != nil {
				t.Fatalf("seed quota snapshot: %v", errSeed)
			}

			var resetRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/usage":
					_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1785296520}}` + test.resetSummary + `}`))
				case "/resets":
					resetRequests.Add(1)
					http.Error(w, "unexpected reset details request", http.StatusInternalServerError)
				default:
					http.NotFound(w, request)
				}
			}))
			defer server.Close()

			collector := NewCollector(repo, Options{
				Owner: "home-a", CodexUsageURL: server.URL + "/usage",
				CodexResetCreditsURL: server.URL + "/resets", Now: func() time.Time { return now },
			})
			collector.collect(context.Background())
			if resetRequests.Load() != 0 {
				t.Fatalf("reset detail requests = %d, want 0", resetRequests.Load())
			}
			item, errGet := repo.GetQuotaCredential(context.Background(), credentialID, now.Add(time.Minute))
			if errGet != nil {
				t.Fatalf("GetQuotaCredential() error = %v", errGet)
			}
			if item.CollectionStatus != "success" || item.ResetCredits == nil || item.ResetCredits.AvailableCount == nil || *item.ResetCredits.AvailableCount != test.wantCount {
				t.Fatalf("reset count snapshot = %+v", item)
			}
			if test.wantOldDetails {
				if len(item.ResetCredits.Credits) != 1 || item.ResetCredits.Credits[0].ID != "old-credit" {
					t.Fatalf("missing summary did not preserve old details: %+v", item.ResetCredits)
				}
			} else if len(item.ResetCredits.Credits) != 0 {
				t.Fatalf("zero summary did not clear details: %+v", item.ResetCredits)
			}
		})
	}
}
