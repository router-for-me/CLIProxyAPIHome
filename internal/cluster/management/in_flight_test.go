package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

func TestGetInFlightSummaryObservationOnlyFixture(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)

	response := performManagementGET(t, handler.GetInFlightSummary, "/credentials/in-flight/summary")
	assertJSONFixture(t, response.Body.Bytes(), "testdata/in_flight_summary_observation_only.json")
}

func TestGetInFlightDetailsObservationOnlyFixture(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)

	response := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight")
	var payload struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items = %#v", payload.Items)
	}
	limiter, exists := payload.Items[0]["limiter"]
	if !exists || string(limiter) != "null" {
		t.Fatalf("limiter = %s, exists = %t; want explicit null", limiter, exists)
	}
	assertJSONFixture(t, response.Body.Bytes(), "testdata/in_flight_details.json")
}

func TestGetInFlightDetailsFiltersAndBoundsPagination(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)

	response := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&model=gpt-5&limit=1&offset=0")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), http.StatusOK)
	}
	var payload struct {
		Items []struct {
			RequestID string `json:"request_id"`
			Model     string `json:"model"`
		} `json:"items"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("json.Unmarshal() error = %v", errDecode)
	}
	if len(payload.Items) != 1 || payload.Items[0].RequestID != "req-1" || payload.Items[0].Model != "gpt-5" {
		t.Fatalf("items = %#v", payload.Items)
	}

	response = performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?limit=0")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d body=%s, want %d", response.Code, response.Body.String(), http.StatusBadRequest)
	}
}

func TestInFlightEndpointsIsolateLimiterFailure(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	if errDrop := db.Migrator().DropTable(&cluster.CredentialConcurrencyPolicyRecord{}); errDrop != nil {
		t.Fatalf("drop concurrency policies: %v", errDrop)
	}

	for _, endpoint := range []struct {
		name    string
		handler gin.HandlerFunc
		path    string
	}{
		{name: "summary", handler: handler.GetInFlightSummary, path: "/credentials/in-flight/summary"},
		{name: "details", handler: handler.GetInFlightDetails, path: "/credentials/in-flight"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			response := performManagementGET(t, endpoint.handler, endpoint.path)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			var payload struct {
				Items []struct {
					Limiter any `json:"limiter"`
				} `json:"items"`
			}
			if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			if len(payload.Items) != 1 || payload.Items[0].Limiter != nil {
				t.Fatalf("payload = %#v", payload)
			}
		})
	}
}

func TestListAuthFilesIsolatesObservationAndLimiterFailures(t *testing.T) {
	t.Run("observation", func(t *testing.T) {
		handler, db, closeRepo := newInFlightManagementTestHandler(t)
		defer closeRepo()
		seedOAuthAuth(t, handler.repo, "cred-a")
		if errDrop := db.Migrator().DropTable(&cluster.CPAInFlightSnapshotRecord{}); errDrop != nil {
			t.Fatalf("drop snapshots: %v", errDrop)
		}
		response := performManagementGET(t, handler.ListAuthFiles, "/auth-files")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		assertAuthFilesOptionalProjection(t, response.Body.Bytes(), false)
	})
	t.Run("limiter", func(t *testing.T) {
		handler, db, closeRepo := newInFlightManagementTestHandler(t)
		defer closeRepo()
		seedInFlightManagementSnapshot(t, handler.repo, db)
		if errDrop := db.Migrator().DropTable(&cluster.CredentialConcurrencyPolicyRecord{}); errDrop != nil {
			t.Fatalf("drop concurrency policies: %v", errDrop)
		}
		response := performManagementGET(t, handler.ListAuthFiles, "/auth-files")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		assertAuthFilesOptionalProjection(t, response.Body.Bytes(), true)
	})
}

func assertAuthFilesOptionalProjection(t *testing.T, body []byte, observed bool) {
	t.Helper()
	var payload struct {
		Files []struct {
			Observed any `json:"observed"`
			Limiter  any `json:"limiter"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Files) != 1 || payload.Files[0].Limiter != nil || (payload.Files[0].Observed != nil) != observed {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestListAuthFilesIncludesInFlightObservationFixture(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)

	response := performManagementGET(t, handler.ListAuthFiles, "/auth-files")
	assertJSONFixture(t, response.Body.Bytes(), "testdata/auth_files_in_flight_observation_only.json")
}

func TestGetCapabilitiesReturnsIndependentInFlightCapability(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/capabilities", handler.GetCapabilities)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/capabilities", nil))

	var payload struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("json.Unmarshal() error = %v", errDecode)
	}
	if !payload.Capabilities["credential_in_flight_snapshots"] {
		t.Fatalf("capabilities = %#v", payload.Capabilities)
	}
	if !payload.Capabilities["credential_concurrency_limits_v2"] {
		t.Fatalf("capabilities = %#v", payload.Capabilities)
	}
}

func newInFlightManagementTestHandler(t *testing.T) (*Handler, *gorm.DB, func()) {
	t.Helper()

	db, errOpen := cluster.OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewHandler(cluster.NewRepository(db), nil, "192.0.2.10", 0), db, closeRepo
}

func seedInFlightManagementSnapshot(t *testing.T, repo *cluster.Repository, db *gorm.DB) {
	t.Helper()

	connectedAt := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	member := cluster.CPANodeMembershipRecord{
		CertificateFingerprint: "in-flight-management-fingerprint",
		NodeID:                 "in-flight-management-node",
		ProtocolVersion:        1,
		State:                  cluster.MembershipStateActive,
		ConnectedAt:            connectedAt,
	}
	if errCreate := db.Create(&member).Error; errCreate != nil {
		t.Fatalf("create membership: %v", errCreate)
	}
	auth := &coreauth.Auth{
		ID:       "cred-a",
		Index:    "cred-a",
		Provider: "codex",
		Label:    "Primary",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	partIndex := 0
	partCount := 1
	frame := cluster.InFlightSnapshotFrame{
		Kind:            cluster.InFlightFramePart,
		Revision:        1,
		ObservedAt:      connectedAt,
		BarrierRevision: 0,
		PartIndex:       &partIndex,
		PartCount:       &partCount,
		Aggregates: []cluster.InFlightAggregate{
			{CredentialID: "cred-a", Model: "gpt-5", Status: cluster.InFlightAccounted, Count: 2},
			{CredentialID: "cred-a", Model: "gpt-5", Status: cluster.InFlightUnaccounted, Count: 1},
		},
		Details: []cluster.InFlightRequestDetail{{
			RequestID:    "req-1",
			CredentialID: "cred-a",
			Model:        "gpt-5",
			RequestKind:  "sse",
			StartedAt:    connectedAt.Add(-2 * time.Second),
		}},
	}
	raw, errMarshal := json.Marshal(frame)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	result, errIngest := repo.IngestInFlightFrame(context.Background(), cluster.InFlightIngestIdentity{
		CertificateFingerprint: member.CertificateFingerprint,
		NodeID:                 member.NodeID,
		MembershipConnectedAt:  member.ConnectedAt,
	}, raw, cluster.DefaultInFlightLimits())
	if errIngest != nil || !result.Published {
		t.Fatalf("IngestInFlightFrame() result = %#v error = %v", result, errIngest)
	}
}

func performManagementGET(t *testing.T, handler gin.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/", handler)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.URL.Path = "/"
	engine.ServeHTTP(response, request)
	return response
}

func assertJSONFixture(t *testing.T, actual []byte, fixturePath string) {
	t.Helper()

	expected, errRead := os.ReadFile(fixturePath)
	if errRead != nil {
		t.Fatalf("ReadFile(%q) error = %v", fixturePath, errRead)
	}
	var want any
	if errUnmarshal := json.Unmarshal(expected, &want); errUnmarshal != nil {
		t.Fatalf("unmarshal fixture %q: %v", fixturePath, errUnmarshal)
	}
	var got any
	if errUnmarshal := json.Unmarshal(actual, &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %s, want fixture %s", actual, expected)
	}
}
