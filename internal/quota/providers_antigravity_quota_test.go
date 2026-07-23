package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
