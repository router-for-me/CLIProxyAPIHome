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
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

func TestCredentialInFlightSummaryAndDetail(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:                 "auth-in-flight",
		Index:              "auth-in-flight",
		Provider:           "codex",
		Status:             coreauth.StatusActive,
		CreatedAt:          now,
		UpdatedAt:          now,
		MaxInFlight:        3,
		MaxInFlightByModel: map[string]int{"gpt-5": 1},
		Metadata:           map[string]any{"type": "codex"},
	}
	if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	lease, errReserve := handler.repo.ReserveInFlightLease(context.Background(), home.InFlightReserveInput{
		DispatchID:     "dispatch-management",
		RequestID:      "request-management",
		CredentialID:   auth.ID,
		Provider:       "codex",
		RequestedModel: "gpt-5-alias",
		Model:          "gpt-5",
		CPANodeID:      "node-a",
		CPAIP:          "192.0.2.20",
		CPALabel:       "node-a:8317",
		TTL:            time.Minute,
	})
	if errReserve != nil || lease == nil {
		t.Fatalf("ReserveInFlightLease() = %#v, %v", lease, errReserve)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/auth-files", handler.ListAuthFiles)
	engine.GET("/credentials/in-flight", handler.ListCredentialInFlight)
	engine.GET("/credentials/in-flight/summary", handler.GetCredentialInFlightSummary)
	engine.GET("/credentials/:credential_id/in-flight", handler.GetCredentialInFlight)

	authFilesResp := httptest.NewRecorder()
	engine.ServeHTTP(authFilesResp, httptest.NewRequest(http.MethodGet, "/auth-files", nil))
	if authFilesResp.Code != http.StatusOK {
		t.Fatalf("auth files status = %d body=%s", authFilesResp.Code, authFilesResp.Body.String())
	}
	var authFilesPayload struct {
		InFlightObservedAt time.Time `json:"in_flight_observed_at"`
		Files              []struct {
			ID                  string `json:"id"`
			InFlight            int64  `json:"in_flight"`
			Remaining           *int64 `json:"remaining"`
			TotalSaturated      bool   `json:"total_saturated"`
			SaturatedModelCount int    `json:"saturated_model_count"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(authFilesResp.Body.Bytes(), &authFilesPayload); errDecode != nil {
		t.Fatalf("decode auth files: %v", errDecode)
	}
	if authFilesPayload.InFlightObservedAt.IsZero() || len(authFilesPayload.Files) != 1 {
		t.Fatalf("auth files = %#v", authFilesPayload)
	}
	authFile := authFilesPayload.Files[0]
	if authFile.ID != auth.ID || authFile.InFlight != 1 || authFile.Remaining == nil || *authFile.Remaining != 2 || authFile.TotalSaturated || authFile.SaturatedModelCount != 1 {
		t.Fatalf("auth file in-flight fields = %#v", authFile)
	}

	summaryResp := httptest.NewRecorder()
	engine.ServeHTTP(summaryResp, httptest.NewRequest(http.MethodGet, "/credentials/in-flight/summary", nil))
	if summaryResp.Code != http.StatusOK {
		t.Fatalf("summary status = %d body=%s", summaryResp.Code, summaryResp.Body.String())
	}
	var summaryPayload struct {
		Items []struct {
			CredentialID string `json:"credential_id"`
			InFlight     int64  `json:"in_flight"`
			Models       []struct {
				Model     string `json:"model"`
				InFlight  int64  `json:"in_flight"`
				Saturated bool   `json:"saturated"`
			} `json:"models"`
		} `json:"items"`
	}
	if errDecode := json.Unmarshal(summaryResp.Body.Bytes(), &summaryPayload); errDecode != nil {
		t.Fatalf("decode summary: %v", errDecode)
	}
	if len(summaryPayload.Items) != 1 || summaryPayload.Items[0].CredentialID != auth.ID || summaryPayload.Items[0].InFlight != 1 {
		t.Fatalf("summary = %#v", summaryPayload)
	}
	if len(summaryPayload.Items[0].Models) != 1 || summaryPayload.Items[0].Models[0].Model != "gpt-5" || !summaryPayload.Items[0].Models[0].Saturated {
		t.Fatalf("summary models = %#v", summaryPayload.Items[0].Models)
	}

	listResp := httptest.NewRecorder()
	engine.ServeHTTP(listResp, httptest.NewRequest(http.MethodGet, "/credentials/in-flight?credential_id=auth-in-flight&limit=1", nil))
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResp.Code, listResp.Body.String())
	}
	var listPayload struct {
		Items []struct {
			LeaseID      string `json:"lease_id"`
			CredentialID string `json:"credential_id"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if errDecode := json.Unmarshal(listResp.Body.Bytes(), &listPayload); errDecode != nil {
		t.Fatalf("decode list: %v", errDecode)
	}
	if len(listPayload.Items) != 1 || listPayload.Items[0].LeaseID != lease.LeaseID || listPayload.Items[0].CredentialID != auth.ID || listPayload.NextCursor != nil {
		t.Fatalf("list = %#v", listPayload)
	}

	detailResp := httptest.NewRecorder()
	engine.ServeHTTP(detailResp, httptest.NewRequest(http.MethodGet, "/credentials/auth-in-flight/in-flight?limit=1", nil))
	if detailResp.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", detailResp.Code, detailResp.Body.String())
	}
	var detailPayload struct {
		Credential struct {
			CredentialID string `json:"credential_id"`
		} `json:"credential"`
		Requests struct {
			Items []struct {
				LeaseID        string `json:"lease_id"`
				RequestID      string `json:"request_id"`
				Model          string `json:"model"`
				RequestedModel string `json:"requested_model"`
				CPALabel       string `json:"cpa_label"`
			} `json:"items"`
		} `json:"requests"`
	}
	if errDecode := json.Unmarshal(detailResp.Body.Bytes(), &detailPayload); errDecode != nil {
		t.Fatalf("decode detail: %v", errDecode)
	}
	if detailPayload.Credential.CredentialID != auth.ID || len(detailPayload.Requests.Items) != 1 {
		t.Fatalf("detail = %#v", detailPayload)
	}
	request := detailPayload.Requests.Items[0]
	if request.LeaseID != lease.LeaseID || request.RequestID != "request-management" || request.Model != "gpt-5" || request.RequestedModel != "gpt-5-alias" || request.CPALabel != "node-a:8317" {
		t.Fatalf("request = %#v", request)
	}
}
