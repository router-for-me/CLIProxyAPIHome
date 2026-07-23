package quota

import (
	"math"
	"testing"
	"time"
)

func TestParseCodexUsageUsesStableLimitIdentityAndDeduplicatesPeriods(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	result, errParse := parseCodexUsage([]byte(`{
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
	}`), observedAt)
	if errParse != nil {
		t.Fatalf("parseCodexUsage() error = %v", errParse)
	}
	if result.plan == nil || result.plan.Name != "Pro 20x" || !result.plan.Premium {
		t.Fatalf("plan = %+v, want Pro 20x", result.plan)
	}
	if result.resetCreditsAvailableCount == nil || *result.resetCreditsAvailableCount != 3 {
		t.Fatalf("reset count = %v, want 3", result.resetCreditsAvailableCount)
	}
	if len(result.windows) != 2 {
		t.Fatalf("windows = %+v, want ordinary and Spark weekly windows", result.windows)
	}
	ordinary := result.windows[0]
	if ordinary.ID != "codex-1-week" || ordinary.Scope != "account" || ordinary.ScopeID != nil || ordinary.RemainingRatio == nil || math.Abs(*ordinary.RemainingRatio-0.91) > 1e-9 {
		t.Fatalf("ordinary window = %+v", ordinary)
	}
	spark := result.windows[1]
	if spark.ID != "codex-bengalfox-1-week" || spark.Label == nil || *spark.Label != "GPT-5.3-Codex-Spark" || spark.Scope != "model" || spark.ScopeID == nil || *spark.ScopeID != "codex_bengalfox" || spark.RemainingRatio == nil || *spark.RemainingRatio != 1 {
		t.Fatalf("Spark window = %+v", spark)
	}
}

func TestParseCodexUsageInvalidPrimaryDoesNotBlockValidSecondary(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	result, errParse := parseCodexUsage([]byte(`{
		"plan_type":"prolite",
		"rate_limit":{
			"primary_window":{"limit_window_seconds":604800,"reset_at":1785296520},
			"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":1785296520}
		}
	}`), observedAt)
	if errParse != nil {
		t.Fatalf("parseCodexUsage() error = %v", errParse)
	}
	if result.plan == nil || result.plan.Name != "Pro 5x" {
		t.Fatalf("plan = %+v, want Pro 5x", result.plan)
	}
	if len(result.windows) != 1 || result.windows[0].ID != "codex-1-week" || result.windows[0].RemainingRatio == nil || *result.windows[0].RemainingRatio != 0.75 {
		t.Fatalf("windows = %+v, want valid secondary weekly window", result.windows)
	}
}

func TestParseCodexUsageAlignsCodeReviewWithHeaderIdentity(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	result, errParse := parseCodexUsage([]byte(`{
		"plan_type":"pro",
		"rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800,"reset_at":1785296520}},
		"code_review_rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_at":1785301740}}
	}`), observedAt)
	if errParse != nil {
		t.Fatalf("parseCodexUsage() error = %v", errParse)
	}
	if len(result.windows) != 2 {
		t.Fatalf("windows = %+v, want ordinary and code-review windows", result.windows)
	}
	codeReview := result.windows[1]
	if codeReview.ID != "codex-code-review-5-hour" || codeReview.Scope != "model" || codeReview.ScopeID == nil || *codeReview.ScopeID != "codex_code_review" || codeReview.Label == nil || *codeReview.Label != "Code Review" {
		t.Fatalf("code-review window = %+v", codeReview)
	}
}

func TestParseCodexUsageRequiresMeteredFeatureForAdditionalIdentity(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	result, errParse := parseCodexUsage([]byte(`{
		"plan_type":"plus",
		"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1785296520}},
		"additional_rate_limits":[{
			"limit_name":"Display name is not an identity",
			"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1785301740}}
		}]
	}`), observedAt)
	if errParse != nil {
		t.Fatalf("parseCodexUsage() error = %v", errParse)
	}
	if len(result.windows) != 1 || result.windows[0].ID != "codex-5-hour" {
		t.Fatalf("windows = %+v, want only the stable default limit", result.windows)
	}
}

func TestParseCodexUsageRejectsDuplicateMeteredFeature(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	_, errParse := parseCodexUsage([]byte(`{
		"plan_type":"pro",
		"rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800,"reset_at":1785296520}},
		"additional_rate_limits":[
			{"limit_name":"Spark A","metered_feature":"codex_bengalfox","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1785301740}}},
			{"limit_name":"Spark B","metered_feature":"codex_bengalfox","rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":1785301740}}}
		]
	}`), observedAt)
	if errParse == nil {
		t.Fatal("parseCodexUsage() accepted duplicate metered_feature")
	}
}

func TestParseCodexUsageRejectsInvalidResetCreditCount(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	for _, body := range []string{
		`{"rate_limit_reset_credits":{"available_count":-1}}`,
		`{"rate_limit_reset_credits":{"available_count":1000001}}`,
	} {
		if _, errParse := parseCodexUsage([]byte(body), observedAt); errParse == nil {
			t.Fatalf("parseCodexUsage() accepted invalid reset-credit count in %s", body)
		}
	}
}

func TestParseCodexResetCreditsFiltersAndSortsAvailableCodexCredits(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	fallbackCount := 3
	result, errParse := parseCodexResetCredits([]byte(`{
		"credits":[
			{"id":"credit-3","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-07-22T00:00:00Z","expires_at":"2026-08-13T01:36:15Z"},
			{"id":"expired","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-07-20T00:00:00Z","expires_at":"2026-07-22T00:59:59Z"},
			{"id":"consumed","reset_type":"codex_rate_limits","status":"consumed","granted_at":"2026-07-22T00:00:00Z","expires_at":"2026-07-25T00:00:00Z"},
			{"id":"credit-1","reset_type":"codex_rate_limits","status":"AVAILABLE","granted_at":"2026-07-20T00:00:00Z","expires_at":"2026-07-27T07:40:51Z"},
			{"id":"other","reset_type":"other_reset","status":"available","granted_at":"2026-07-22T00:00:00Z","expires_at":"2026-07-24T00:00:00Z"},
			{"id":"credit-2","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-07-21T00:00:00Z","expires_at":"2026-08-01T03:52:39Z"}
		]
	}`), observedAt, &fallbackCount)
	if errParse != nil {
		t.Fatalf("parseCodexResetCredits() error = %v", errParse)
	}
	if result.AvailableCount == nil || *result.AvailableCount != 3 || !result.ObservedAt.Equal(observedAt) {
		t.Fatalf("reset summary = %+v", result)
	}
	if len(result.Credits) != 3 || result.Credits[0].ID != "credit-1" || result.Credits[1].ID != "credit-2" || result.Credits[2].ID != "credit-3" {
		t.Fatalf("reset details = %+v", result.Credits)
	}
}

func TestParseCodexResetCreditsRequiresGrantedAt(t *testing.T) {
	observedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	_, errParse := parseCodexResetCredits([]byte(`{
		"available_count":1,
		"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available"}]
	}`), observedAt, nil)
	if errParse == nil {
		t.Fatal("parseCodexResetCredits() accepted an available credit without granted_at")
	}
}
