package quota

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestParseAntigravityWindowsWeeklyOnly(t *testing.T) {
	now := time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC)
	windows, errParse := parseAntigravityWindows([]byte(`{
		"groups": [
			{"displayName":"Gemini models","buckets":[
				{"bucketId":"gemini-5h","disabled":true,"remainingFraction":0.5},
				{"bucketId":"gemini-weekly","remainingFraction":0.8,"resetTime":"2026-07-27T00:00:00Z"},
				{"bucketId":"gemini-image-5h","remainingFraction":0.2}
			]},
			{"displayName":"Claude and GPT models","buckets":[
				{"bucketId":"3p-5h"},
				{"bucketId":"3p-5h","remainingFraction":1.2},
				{"bucketId":"3p-weekly","remainingFraction":0.6,"resetTime":"2026-07-27T00:00:00Z"}
			]}
		],
		"models": {"chat_20706":{"displayName":"Internal completion model","quotaInfo":{"remainingFraction":0.01}}}
	}`), now)
	if errParse != nil {
		t.Fatalf("parseAntigravityWindows() error = %v", errParse)
	}
	if len(windows) != 2 || !antigravityQuotaGroupsComplete(windows) {
		t.Fatalf("weekly-only windows = %+v, want two complete weekly buckets", windows)
	}
	for index, expected := range []struct {
		id      string
		scopeID string
		label   string
		ratio   float64
	}{
		{id: "antigravity-gemini-weekly", scopeID: "gemini", label: "Gemini models", ratio: 0.8},
		{id: "antigravity-3p-weekly", scopeID: "third-party", label: "Claude and GPT models", ratio: 0.6},
	} {
		window := windows[index]
		if window.ID != expected.id || window.ScopeID == nil || *window.ScopeID != expected.scopeID || window.Label == nil || *window.Label != expected.label {
			t.Fatalf("weekly window %d identity = %+v", index, window)
		}
		if window.PeriodUnit != "week" || window.PeriodValue == nil || *window.PeriodValue != 1 || window.WindowSeconds == nil || *window.WindowSeconds != 7*24*60*60 {
			t.Fatalf("weekly window %d period = %+v", index, window)
		}
		if window.RemainingRatio == nil || *window.RemainingRatio != expected.ratio {
			t.Fatalf("weekly window %d ratio = %+v", index, window.RemainingRatio)
		}
	}
}

func TestParseAntigravityWindowsProUsesFourStableBuckets(t *testing.T) {
	now := time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC)
	windows, errParse := parseAntigravityWindows([]byte(`{
		"groups": [
			{"displayName":"Claude and GPT models","buckets":[
				{"bucketId":"3p-weekly","remainingFraction":0.9},
				{"bucketId":"3p-5h","remainingFraction":0.4}
			]},
			{"displayName":"Gemini models","buckets":[
				{"bucketId":"gemini-weekly","remainingFraction":0.7},
				{"bucketId":"gemini-5h","remainingFraction":0.3}
			]}
		]
	}`), now)
	if errParse != nil {
		t.Fatalf("parseAntigravityWindows() error = %v", errParse)
	}
	wantIDs := []string{
		"antigravity-gemini-5h",
		"antigravity-3p-5h",
		"antigravity-gemini-weekly",
		"antigravity-3p-weekly",
	}
	if len(windows) != len(wantIDs) || !antigravityQuotaGroupsComplete(windows) {
		t.Fatalf("pro windows = %+v, want four complete stable buckets", windows)
	}
	for index, window := range windows {
		if window.ID != wantIDs[index] || window.Priority != index {
			t.Fatalf("pro window %d = %+v", index, window)
		}
	}
}

func TestParseAntigravityWindowsAcceptsFlexibleFractionsAndSkipsMalformedBuckets(t *testing.T) {
	now := time.Date(2026, 7, 22, 3, 30, 0, 0, time.UTC)
	windows, errParse := parseAntigravityWindows([]byte(`{
		"groups": [
			{"displayName":"Gemini models","buckets":[
				"invalid-bucket",
				{"bucketId":"gemini-5h","remainingFraction":"40%"},
				{"bucketId":"gemini-weekly","remainingFraction":"0.8"},
				{"bucketId":"gemini-weekly","remainingFraction":{"unexpected":1}}
			]},
			{"displayName":"Claude and GPT models","buckets":[
				{"bucketId":"3p-5h","remainingFraction":"not-a-number"},
				{"bucketId":"3p-weekly","remainingFraction":0.6}
			]}
		]
	}`), now)
	if errParse != nil {
		t.Fatalf("parseAntigravityWindows() error = %v", errParse)
	}
	if len(windows) != 3 || !antigravityQuotaGroupsComplete(windows) {
		t.Fatalf("flexible fraction windows = %+v", windows)
	}
	if windows[0].ID != "antigravity-gemini-5h" || windows[0].RemainingRatio == nil || *windows[0].RemainingRatio != 0.4 {
		t.Fatalf("percentage fraction = %+v", windows[0])
	}
	if windows[1].ID != "antigravity-gemini-weekly" || windows[1].RemainingRatio == nil || *windows[1].RemainingRatio != 0.8 {
		t.Fatalf("numeric string fraction = %+v", windows[1])
	}
	if windows[2].ID != "antigravity-3p-weekly" {
		t.Fatalf("malformed bucket was not omitted independently: %+v", windows)
	}
}

func TestCollectorAntigravityPrimaryWindowsStayDistinctWhenFiveHourBucketsDiffer(t *testing.T) {
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 22, 4, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "antigravity-asymmetric", "antigravity", map[string]any{
		"type": "antigravity", "access_token": "ag-secret", "project_id": "project-123",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"groups": [
				{"displayName":"Gemini models","buckets":[
					{"bucketId":"gemini-5h","remainingFraction":0.4},
					{"bucketId":"gemini-weekly","remainingFraction":0.7}
				]},
				{"displayName":"Claude and GPT models","buckets":[
					{"bucketId":"3p-5h","disabled":true,"remainingFraction":0.5},
					{"bucketId":"3p-weekly","remainingFraction":0.8}
				]}
			]
		}`))
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner: "home-a", Now: func() time.Time { return now }, AntigravityURLs: []string{server.URL},
	})
	collector.collect(context.Background())

	item, errGet := repo.GetQuotaCredential(context.Background(), "antigravity-asymmetric", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "success" || len(item.Windows) != 3 || len(item.PrimaryWindows) != 2 {
		t.Fatalf("unexpected asymmetric quota snapshot: %+v", item)
	}
	if item.PrimaryWindows[0].ID != "antigravity-gemini-5h" || item.PrimaryWindows[1].ID != "antigravity-3p-weekly" {
		t.Fatalf("primary windows did not retain both groups: %+v", item.PrimaryWindows)
	}
}

func TestCollectorAntigravityIncompleteResponseRetainsLastKnownWindows(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "only five hour buckets",
			body: `{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.2}]},{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.3}]}]}`,
		},
		{
			name: "missing third party weekly",
			body: `{"groups":[{"buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.2}]},{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.3}]}]}`,
		},
		{
			name: "missing gemini weekly",
			body: `{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.2}]},{"buckets":[{"bucketId":"3p-weekly","remainingFraction":0.3}]}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newCollectorTestRepository(t)
			now := time.Date(2026, 7, 22, 5, 0, 0, 0, time.UTC)
			credentialID := "antigravity-incomplete-" + quotaIDSlug(test.name)
			seedCollectorProviderAuth(t, repo, credentialID, "antigravity", map[string]any{
				"type": "antigravity", "access_token": "ag-secret", "project_id": "project-123",
			})
			oldObservedAt := now.Add(-time.Hour)
			oldExpiresAt := now.Add(-time.Minute)
			oldWindows, errParse := parseAntigravityWindows([]byte(`{
				"groups": [
					{"buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.8}]},
					{"buckets":[{"bucketId":"3p-weekly","remainingFraction":0.6}]}
				]
			}`), oldObservedAt)
			if errParse != nil {
				t.Fatalf("parseAntigravityWindows() error = %v", errParse)
			}
			version := cluster.QuotaSnapshotVersion("antigravity")
			_, errSeed := repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
				CredentialID: credentialID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
				ObservedAt: &oldObservedAt, ExpiresAt: &oldExpiresAt, LastSuccessAt: &oldObservedAt, NextProbeAt: &oldExpiresAt,
				ParserVersion: version, CollectorVersion: version, ReplaceWindows: true, Windows: oldWindows,
			})
			if errSeed != nil {
				t.Fatalf("seed snapshot: %v", errSeed)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			collector := NewCollector(repo, Options{
				Owner: "home-a", Now: func() time.Time { return now }, AntigravityURLs: []string{server.URL},
			})
			collector.collect(context.Background())

			item, errGet := repo.GetQuotaCredential(context.Background(), credentialID, now)
			if errGet != nil {
				t.Fatalf("GetQuotaCredential() error = %v", errGet)
			}
			if item.QuotaStatus != "healthy" || item.CollectionStatus != "failed" || len(item.Windows) != 2 {
				t.Fatalf("incomplete response replaced last-known windows: %+v", item)
			}
			if item.Error == nil || item.Error.Code != "UPSTREAM_RESPONSE_INVALID" || !item.Error.Retryable {
				t.Fatalf("unexpected incomplete response error: %+v", item.Error)
			}
			if item.Windows[0].ID != "antigravity-gemini-weekly" || item.Windows[1].ID != "antigravity-3p-weekly" {
				t.Fatalf("last-known windows changed after incomplete response: %+v", item.Windows)
			}
		})
	}
}

func TestProbeAntigravityUsesRequestUserAgent(t *testing.T) {
	const body = `{"groups":[{"buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.9}]},{"buckets":[{"bucketId":"3p-weekly","remainingFraction":0.8}]}]}`
	tests := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{
			name: "default hub user agent",
			auth: &coreauth.Auth{Provider: "antigravity", Metadata: map[string]any{"access_token": "ag-secret", "project_id": "project-123"}},
			want: cpasdkauth.AntigravityUserAgent(),
		},
		{
			name: "configured metadata user agent",
			auth: &coreauth.Auth{Provider: "antigravity", Metadata: map[string]any{
				"access_token": "ag-secret", "project_id": "project-123",
				"user_agent": "antigravity/hub/1.23.2 windows/amd64 google-api-nodejs-client/10.3.0",
			}},
			want: "antigravity/hub/1.23.2 windows/amd64",
		},
		{
			name: "configured attribute user agent wins",
			auth: &coreauth.Auth{Provider: "antigravity", Attributes: map[string]string{"user_agent": "antigravity/hub/2.30.0 darwin/arm64"}, Metadata: map[string]any{
				"access_token": "ag-secret", "project_id": "project-123", "user_agent": "antigravity/1.11.5 windows/amd64",
			}},
			want: "antigravity/hub/2.30.0 darwin/arm64",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotUserAgent string
			collector := NewCollector(newCollectorTestRepository(t), Options{
				AntigravityURLs: []string{"https://antigravity.invalid/quota"},
				HTTPClient: func(*coreauth.Auth, time.Duration) (*http.Client, error) {
					return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
						gotUserAgent = request.Header.Get("User-Agent")
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
					})}, nil
				},
			})
			if _, failure := collector.probeAntigravity(context.Background(), test.auth); failure != nil {
				t.Fatalf("probeAntigravity() failure = %+v", failure)
			}
			if gotUserAgent != test.want {
				t.Fatalf("User-Agent = %q, want %q", gotUserAgent, test.want)
			}
		})
	}
}

func TestProbeAntigravityFallbacksShareDeadline(t *testing.T) {
	const probeTimeout = 80 * time.Millisecond
	var attempts atomic.Int32
	collector := NewCollector(newCollectorTestRepository(t), Options{
		ProbeTimeout: probeTimeout,
		AntigravityURLs: []string{
			"https://antigravity.invalid/one",
			"https://antigravity.invalid/two",
			"https://antigravity.invalid/three",
		},
		HTTPClient: func(*coreauth.Auth, time.Duration) (*http.Client, error) {
			return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				<-request.Context().Done()
				return nil, request.Context().Err()
			})}, nil
		},
	})
	auth := &coreauth.Auth{Provider: "antigravity", Metadata: map[string]any{
		"access_token": "ag-secret", "project_id": "project-123",
	}}
	startedAt := time.Now()
	_, failure := collector.probeAntigravity(context.Background(), auth)
	elapsed := time.Since(startedAt)
	if failure == nil || failure.code != "UPSTREAM_UNAVAILABLE" {
		t.Fatalf("probeAntigravity() failure = %+v", failure)
	}
	if attempts.Load() != 1 {
		t.Fatalf("fallback attempts after shared deadline = %d, want 1", attempts.Load())
	}
	if elapsed >= 2*probeTimeout {
		t.Fatalf("shared fallback deadline elapsed = %v, want less than %v", elapsed, 2*probeTimeout)
	}
}

func TestParseAntigravitySubscriptionPlans(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		plan    string
		premium bool
		invalid bool
	}{
		{
			name:    "paid ultra overrides free current tier",
			body:    `{"currentTier":{"id":"free-tier","name":"Antigravity"},"paidTier":{"id":"g1-ultra-tier","name":"Google AI Ultra"}}`,
			plan:    "Ultra",
			premium: true,
		},
		{
			name:    "paid ultra lite",
			body:    `{"current_tier":{"id":"free-tier"},"paid_tier":{"id":"g1-ultra-lite-tier","name":"Google AI Ultra Lite"}}`,
			plan:    "Ultra Lite",
			premium: true,
		},
		{
			name:    "paid pro",
			body:    `{"currentTier":{"id":"free-tier","name":"Antigravity"},"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`,
			plan:    "Pro",
			premium: false,
		},
		{
			name:    "free current tier without paid tier",
			body:    `{"currentTier":{"id":"free-tier","name":"Antigravity"}}`,
			plan:    "Free",
			premium: false,
		},
		{
			name:    "unknown tier falls back to tier name",
			body:    `{"paidTier":{"id":"g1-enterprise-tier","name":"Enterprise Plus"}}`,
			plan:    "Enterprise Plus",
			premium: false,
		},
		{
			name:    "unknown tier without name falls back to tier id",
			body:    `{"currentTier":{"id":"custom-tier"}}`,
			plan:    "custom-tier",
			premium: false,
		},
		{
			name: "missing tier metadata clears plan",
			body: `{"cloudaicompanionProject":"project-123"}`,
			plan: "",
		},
		{
			name:    "in-band error rejected",
			body:    `{"error":{"code":500,"message":"internal"}}`,
			invalid: true,
		},
		{
			name:    "invalid json rejected",
			body:    `invalid`,
			invalid: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, errParse := parseAntigravitySubscriptionPlan([]byte(test.body))
			if test.invalid {
				if errParse == nil {
					t.Fatalf("parseAntigravitySubscriptionPlan() succeeded with %+v, want error", plan)
				}
				return
			}
			if errParse != nil {
				t.Fatalf("parseAntigravitySubscriptionPlan() error = %v", errParse)
			}
			if test.plan == "" {
				if plan != nil {
					t.Fatalf("plan = %+v, want nil", plan)
				}
				return
			}
			if plan == nil || plan.Name != test.plan || plan.Premium != test.premium {
				t.Fatalf("plan = %+v, want %q premium=%v", plan, test.plan, test.premium)
			}
		})
	}
}

func TestCollectorAntigravityPersistsSubscriptionPlanAndPreservesOnPartialFailure(t *testing.T) {
	const quotaBody = `{
		"groups": [
			{"displayName":"Gemini models","buckets":[{"bucketId":"gemini-5h","remainingFraction":0.8},{"bucketId":"gemini-weekly","remainingFraction":0.9}]},
			{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","remainingFraction":0.7},{"bucketId":"3p-weekly","remainingFraction":0.85}]}
		]
	}`
	repo := newCollectorTestRepository(t)
	now := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	seedCollectorProviderAuth(t, repo, "antigravity-plan", "antigravity", map[string]any{
		"type": "antigravity", "access_token": "ag-secret", "project_id": "project-123",
	})

	var codeAssistStatus atomic.Int32
	codeAssistStatus.Store(http.StatusOK)
	var codeAssistBody atomic.Value
	codeAssistBody.Store(`{"currentTier":{"id":"free-tier","name":"Antigravity"},"paidTier":{"id":"g1-ultra-tier","name":"Google AI Ultra"}}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/quota":
			_, _ = w.Write([]byte(quotaBody))
		case "/loadCodeAssist":
			raw, _ := io.ReadAll(request.Body)
			if strings.TrimSpace(string(raw)) != string(antigravityCodeAssistRequestBody) {
				t.Errorf("loadCodeAssist body = %s, want %s", string(raw), string(antigravityCodeAssistRequestBody))
			}
			status := int(codeAssistStatus.Load())
			w.WriteHeader(status)
			if body, ok := codeAssistBody.Load().(string); ok {
				_, _ = w.Write([]byte(body))
			}
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner:                    "home-a",
		Now:                      func() time.Time { return now },
		AntigravityURLs:          []string{server.URL + "/quota"},
		AntigravityCodeAssistURL: server.URL + "/loadCodeAssist",
	})
	collector.collect(context.Background())

	first, errFirst := repo.GetQuotaCredential(context.Background(), "antigravity-plan", now.Add(time.Minute))
	if errFirst != nil {
		t.Fatalf("GetQuotaCredential(first) error = %v", errFirst)
	}
	if first.CollectionStatus != "success" || first.Plan == nil || first.Plan.Name != "Ultra" || !first.Plan.Premium {
		t.Fatalf("first snapshot = %+v, want success with Ultra premium plan", first)
	}

	now = now.Add(31 * time.Minute)
	codeAssistStatus.Store(http.StatusTooManyRequests)
	codeAssistBody.Store(`{"error":"rate limited"}`)
	accepted, errTrigger := collector.TriggerCollection(context.Background(), map[string]struct{}{"antigravity-plan": {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("TriggerCollection(partial) = %d, %v, want 1, nil", accepted, errTrigger)
	}
	collector.Wait()

	second, errSecond := repo.GetQuotaCredential(context.Background(), "antigravity-plan", now.Add(time.Minute))
	if errSecond != nil {
		t.Fatalf("GetQuotaCredential(second) error = %v", errSecond)
	}
	if second.CollectionStatus != "partial" || second.Plan == nil || second.Plan.Name != "Ultra" || !second.Plan.Premium || second.Error == nil || second.Error.Code != "UPSTREAM_RATE_LIMITED" {
		t.Fatalf("partial refresh snapshot = %+v, want partial with preserved Ultra plan", second)
	}

	now = now.Add(31 * time.Minute)
	codeAssistStatus.Store(http.StatusOK)
	codeAssistBody.Store(`{"currentTier":{"id":"free-tier","name":"Antigravity"},"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`)
	accepted, errTrigger = collector.TriggerCollection(context.Background(), map[string]struct{}{"antigravity-plan": {}}, nil)
	if errTrigger != nil || accepted != 1 {
		t.Fatalf("TriggerCollection(pro) = %d, %v, want 1, nil", accepted, errTrigger)
	}
	collector.Wait()

	third, errThird := repo.GetQuotaCredential(context.Background(), "antigravity-plan", now.Add(time.Minute))
	if errThird != nil {
		t.Fatalf("GetQuotaCredential(third) error = %v", errThird)
	}
	if third.CollectionStatus != "success" || third.Plan == nil || third.Plan.Name != "Pro" || third.Plan.Premium {
		t.Fatalf("pro refresh snapshot = %+v, want success with Pro non-premium plan", third)
	}
}
