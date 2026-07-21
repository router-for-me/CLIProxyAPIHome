package quota

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// Kimi weekly, duration, and aggregate usage limits must all be exposed as
// generic windows with structured periods, ratios, and reset timestamps.
func TestParseKimiUsageWindowsMultiLimitShapes(t *testing.T) {
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	body := []byte(`{
		"usage": {"limit": "500", "used": "360", "remaining": "140", "resetTime": "2026-07-23T00:00:00Z", "title": "Weekly quota"},
		"limits": [
			{
				"title": "Weekly limit",
				"window": {"duration": 1, "timeUnit": "TIME_UNIT_WEEK"},
				"detail": {"limit": "500", "used": "360", "remaining": "140", "resetTime": "2026-07-23T00:00:00Z"}
			},
			{
				"title": "5h limit",
				"window": {"duration": 5, "timeUnit": "TIME_UNIT_HOUR"},
				"detail": {"limit": "100", "used": "59", "remaining": "41", "resetTime": "2026-07-16T14:00:00Z"}
			},
			{
				"title": "Daily limit",
				"window": {"duration": 1, "timeUnit": "TIME_UNIT_DAY"},
				"detail": {"limit": "100", "used": "30", "remaining": "70", "resetTime": "2026-07-17T00:00:00Z"}
			}
		]
	}`)
	windows, errParse := parseKimiUsageWindows(body, now)
	if errParse != nil {
		t.Fatalf("parseKimiUsageWindows() error = %v", errParse)
	}
	if len(windows) != 4 {
		t.Fatalf("windows = %d, want 4 (summary + weekly + 5h + daily)", len(windows))
	}

	summary := windows[0]
	if summary.ID != "kimi-usage" || summary.Label == nil || *summary.Label != "Weekly quota" {
		t.Fatalf("unexpected summary window: %+v", summary)
	}
	if summary.ResetAt == nil {
		t.Fatalf("summary window missing reset_at: %+v", summary)
	}

	weekly := windows[1]
	if weekly.PeriodUnit != "week" || weekly.PeriodValue == nil || *weekly.PeriodValue != 1 {
		t.Fatalf("weekly limit period shape invalid: %+v", weekly)
	}
	if weekly.WindowSeconds == nil || *weekly.WindowSeconds != 604800 {
		t.Fatalf("weekly limit window_seconds invalid: %+v", weekly.WindowSeconds)
	}
	if weekly.Limit == nil || *weekly.Limit != 500 || weekly.Remaining == nil || *weekly.Remaining != 140 {
		t.Fatalf("weekly limit quantities invalid: %+v", weekly)
	}
	if weekly.ResetAt == nil {
		t.Fatalf("weekly limit missing reset_at")
	}

	fiveHour := windows[2]
	if fiveHour.PeriodUnit != "hour" || fiveHour.PeriodValue == nil || *fiveHour.PeriodValue != 5 {
		t.Fatalf("5h limit period shape invalid: %+v", fiveHour)
	}
	if fiveHour.WindowSeconds == nil || *fiveHour.WindowSeconds != 18000 {
		t.Fatalf("5h limit window_seconds invalid: %+v", fiveHour.WindowSeconds)
	}

	daily := windows[3]
	if daily.PeriodUnit != "day" || daily.PeriodValue == nil || *daily.PeriodValue != 1 {
		t.Fatalf("daily limit period shape invalid: %+v", daily)
	}
}

func TestKimiPrimaryWindowsPreferDistinctProviderLimits(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "kimi-primary", "kimi", map[string]any{"type": "kimi", "access_token": "probe-secret"})
	body := []byte(`{
		"usage": {"limit": 500, "used": 360, "remaining": 140, "title": "Weekly quota"},
		"limits": [
			{"title": "Weekly limit", "window": {"duration": 1, "timeUnit": "TIME_UNIT_WEEK"}, "detail": {"limit": 500, "used": 360, "remaining": 140}},
			{"title": "5h limit", "window": {"duration": 5, "timeUnit": "TIME_UNIT_HOUR"}, "detail": {"limit": 100, "used": 59, "remaining": 41}}
		]
	}`)
	windows, errParse := parseKimiUsageWindows(body, now)
	if errParse != nil {
		t.Fatalf("parseKimiUsageWindows() error = %v", errParse)
	}
	expiresAt := now.Add(30 * time.Minute)
	if _, errWrite := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: "kimi-primary", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, LastSuccessAt: &now, NextProbeAt: &expiresAt,
		ReplaceWindows: true, Windows: windows,
	}); errWrite != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errWrite)
	}

	result, errList := repo.ListQuotaCredentials(context.Background(), cluster.QuotaListQuery{
		Limit: 50, IDs: map[string]struct{}{"kimi-primary": {}}, Sort: "risk_desc", Now: now.Add(time.Minute),
	})
	if errList != nil {
		t.Fatalf("ListQuotaCredentials() error = %v", errList)
	}
	if len(result.Items) != 1 || len(result.Items[0].PrimaryWindows) != 2 {
		t.Fatalf("unexpected Kimi list item: %+v", result.Items)
	}
	primary := result.Items[0].PrimaryWindows
	if primary[0].PeriodUnit != "week" || primary[1].PeriodUnit != "hour" || primary[1].PeriodValue == nil || *primary[1].PeriodValue != 5 {
		t.Fatalf("Kimi primary windows lost provider limits: %+v", primary)
	}
	if result.Items[0].WindowCount != 3 {
		t.Fatalf("Kimi window_count = %d, want 3", result.Items[0].WindowCount)
	}
}
