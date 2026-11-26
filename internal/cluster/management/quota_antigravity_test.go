package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestQuotaManagementAntigravityListIncludesEveryCurrentWindow(t *testing.T) {
	for _, testCase := range []struct {
		name string
		ids  []string
	}{
		{name: "all four windows", ids: []string{"antigravity-gemini-5h", "antigravity-3p-5h", "antigravity-gemini-weekly", "antigravity-3p-weekly"}},
		{name: "weekly only", ids: []string{"antigravity-gemini-weekly", "antigravity-3p-weekly"}},
		{name: "without third-party five-hour window", ids: []string{"antigravity-gemini-5h", "antigravity-gemini-weekly", "antigravity-3p-weekly"}},
		{name: "without Gemini five-hour window", ids: []string{"antigravity-3p-5h", "antigravity-gemini-weekly", "antigravity-3p-weekly"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler, closeRepo := newUsageObservabilityTestHandler(t)
			defer closeRepo()

			now := time.Now().UTC().Truncate(time.Second)
			expiresAt := now.Add(time.Hour)
			auth := &coreauth.Auth{
				ID: "quota-antigravity", Index: "quota-antigravity", Provider: "antigravity", Status: coreauth.StatusActive,
				Metadata: map[string]any{"type": "antigravity"}, CreatedAt: now, UpdatedAt: now,
			}
			if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
				t.Fatalf("UpsertAuth() error = %v", errUpsert)
			}
			windows := make([]cluster.QuotaWindow, 0, len(testCase.ids))
			for _, id := range testCase.ids {
				scopeID, periodUnit, periodValue, priority := "gemini", "hour", float64(5), 0
				switch id {
				case "antigravity-3p-5h":
					scopeID, priority = "third-party", 1
				case "antigravity-gemini-weekly":
					periodUnit, periodValue, priority = "week", 1, 2
				case "antigravity-3p-weekly":
					scopeID, periodUnit, periodValue, priority = "third-party", "week", 1, 3
				}
				remaining, status := 0.8, "healthy"
				if id == "antigravity-3p-weekly" {
					remaining, status = 0.05, "low"
				}
				windows = append(windows, cluster.QuotaWindow{
					ID: id, Priority: priority, Scope: "model", ScopeID: &scopeID, Mode: "rolling", Status: status,
					Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: periodUnit, PeriodValue: &periodValue,
					Source: "active_probe", ObservedAt: now, ExpiresAt: &expiresAt,
				})
			}
			if _, errSnapshot := handler.repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
				CredentialID: auth.ID, QuotaStatus: "low", CollectionStatus: "success", Source: "active_probe",
				ObservedAt: &now, ExpiresAt: &expiresAt, LastAttemptAt: &now, LastSuccessAt: &now,
				ParserVersion: cluster.QuotaSnapshotVersion("antigravity"), CollectorVersion: cluster.QuotaSnapshotVersion("antigravity"),
				ReplaceWindows: true, Windows: windows,
			}); errSnapshot != nil {
				t.Fatalf("UpsertQuotaSnapshot() error = %v", errSnapshot)
			}

			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.GET("/quota/credentials", handler.ListQuotaCredentials)
			for _, path := range []string{"/quota/credentials?provider=antigravity", "/quota/credentials?ids=" + auth.ID} {
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d body=%s", path, response.Code, response.Body.String())
				}
				var payload struct {
					Items []struct {
						PrimaryWindows []cluster.QuotaWindow `json:"primary_windows"`
						WindowCount    int                   `json:"window_count"`
						QuotaStatus    string                `json:"quota_status"`
						Windows        json.RawMessage       `json:"windows"`
					} `json:"items"`
				}
				if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
					t.Fatalf("decode list: %v", errDecode)
				}
				if len(payload.Items) != 1 {
					t.Fatalf("GET %s returned %d items, want 1", path, len(payload.Items))
				}
				item := payload.Items[0]
				if item.WindowCount != len(testCase.ids) || len(item.PrimaryWindows) != len(testCase.ids) || item.QuotaStatus != "low" || item.Windows != nil {
					t.Fatalf("GET %s returned unexpected quota list item: %+v", path, item)
				}
				for index, id := range testCase.ids {
					if item.PrimaryWindows[index].ID != id {
						t.Fatalf("GET %s primary window %d = %q, want %q", path, index, item.PrimaryWindows[index].ID, id)
					}
				}
			}
		})
	}
}
