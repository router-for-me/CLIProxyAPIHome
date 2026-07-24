package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
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

func TestGetInFlightDetailsStableSnapshotCursorPinsPages(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	publishInFlightManagementSnapshot(t, handler.repo, 2, []cluster.InFlightRequestDetail{
		{RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-2 * time.Second)},
		{RequestID: "req-2", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "websocket", StartedAt: inFlightManagementConnectedAt().Add(-time.Second)},
	})

	first := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&offset=0&stable_snapshot=true")
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	var firstPayload struct {
		Items []struct {
			RequestID string `json:"request_id"`
		} `json:"items"`
		Total                int     `json:"total"`
		NextOffset           *int    `json:"next_offset"`
		SnapshotCursor       *string `json:"snapshot_cursor"`
		SnapshotCursorReadAt *string `json:"snapshot_cursor_read_at"`
		SnapshotExpiresAt    *string `json:"snapshot_expires_at"`
	}
	if errDecode := json.Unmarshal(first.Body.Bytes(), &firstPayload); errDecode != nil {
		t.Fatalf("decode first response: %v", errDecode)
	}
	if len(firstPayload.Items) != 1 || firstPayload.Items[0].RequestID != "req-1" || firstPayload.Total != 2 || firstPayload.NextOffset == nil || *firstPayload.NextOffset != 1 || firstPayload.SnapshotCursor == nil || *firstPayload.SnapshotCursor == "" || firstPayload.SnapshotCursorReadAt == nil || firstPayload.SnapshotExpiresAt == nil {
		t.Fatalf("first payload = %#v", firstPayload)
	}
	firstReadAt, errFirstReadAt := time.Parse(time.RFC3339Nano, *firstPayload.SnapshotCursorReadAt)
	firstExpiresAt, errFirstExpiresAt := time.Parse(time.RFC3339Nano, *firstPayload.SnapshotExpiresAt)
	if errFirstReadAt != nil || errFirstExpiresAt != nil || !firstExpiresAt.After(firstReadAt) || firstExpiresAt.Sub(firstReadAt) > inFlightSnapshotCursorTTL {
		t.Fatalf("first cursor times read_at=%q expires_at=%q errors=(%v, %v)", *firstPayload.SnapshotCursorReadAt, *firstPayload.SnapshotExpiresAt, errFirstReadAt, errFirstExpiresAt)
	}

	publishInFlightManagementSnapshot(t, handler.repo, 3, []cluster.InFlightRequestDetail{
		{RequestID: "req-new", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt()},
	})
	secondPath := "/credentials/in-flight?credential_id=cred-a&limit=1&offset=1&snapshot_cursor=" + *firstPayload.SnapshotCursor
	second := performManagementGET(t, handler.GetInFlightDetails, secondPath)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
	var secondPayload struct {
		Items []struct {
			RequestID string `json:"request_id"`
		} `json:"items"`
		Total                int     `json:"total"`
		NextOffset           *int    `json:"next_offset"`
		SnapshotCursor       *string `json:"snapshot_cursor"`
		SnapshotCursorReadAt *string `json:"snapshot_cursor_read_at"`
		SnapshotExpiresAt    *string `json:"snapshot_expires_at"`
	}
	if errDecode := json.Unmarshal(second.Body.Bytes(), &secondPayload); errDecode != nil {
		t.Fatalf("decode second response: %v", errDecode)
	}
	if len(secondPayload.Items) != 1 || secondPayload.Items[0].RequestID != "req-2" || secondPayload.Total != 2 || secondPayload.NextOffset != nil || secondPayload.SnapshotCursor == nil || *secondPayload.SnapshotCursor != *firstPayload.SnapshotCursor || secondPayload.SnapshotCursorReadAt == nil || secondPayload.SnapshotExpiresAt == nil || *secondPayload.SnapshotExpiresAt != *firstPayload.SnapshotExpiresAt {
		t.Fatalf("second payload = %#v", secondPayload)
	}
	secondReadAt, errSecondReadAt := time.Parse(time.RFC3339Nano, *secondPayload.SnapshotCursorReadAt)
	if errSecondReadAt != nil || secondReadAt.Before(firstReadAt) || !firstExpiresAt.After(secondReadAt) {
		t.Fatalf("second cursor read_at=%q error=%v first_read_at=%s expires_at=%s", *secondPayload.SnapshotCursorReadAt, errSecondReadAt, firstReadAt, firstExpiresAt)
	}

	latest := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&stable_snapshot=true")
	if latest.Code != http.StatusOK || !strings.Contains(latest.Body.String(), `"request_id":"req-new"`) {
		t.Fatalf("latest response = %d %s", latest.Code, latest.Body.String())
	}
}

func TestGetInFlightDetailsSnapshotCursorRejectsMismatchAndExpiry(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	publishInFlightManagementSnapshot(t, handler.repo, 2, []cluster.InFlightRequestDetail{
		{RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-2 * time.Second)},
		{RequestID: "req-2", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-time.Second)},
	})
	first := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&stable_snapshot=true")
	var payload struct {
		SnapshotCursor string `json:"snapshot_cursor"`
	}
	if errDecode := json.Unmarshal(first.Body.Bytes(), &payload); errDecode != nil || payload.SnapshotCursor == "" {
		t.Fatalf("decode cursor response: %v payload=%#v", errDecode, payload)
	}

	mismatch := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-b&limit=1&offset=1&snapshot_cursor="+payload.SnapshotCursor)
	assertInFlightCursorError(t, mismatch, http.StatusConflict, "in_flight_snapshot_cursor_expired")
	modelMismatch := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&model=gpt-5.5&limit=1&offset=1&snapshot_cursor="+payload.SnapshotCursor)
	assertInFlightCursorError(t, modelMismatch, http.StatusConflict, "in_flight_snapshot_cursor_expired")

	past := time.Now().UTC().Add(-time.Minute)
	if errExpire := db.Model(&cluster.ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", payload.SnapshotCursor).
		Update("expires_at", past).Error; errExpire != nil {
		t.Fatalf("expire snapshot cursor: %v", errExpire)
	}
	expired := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&offset=1&snapshot_cursor="+payload.SnapshotCursor)
	assertInFlightCursorError(t, expired, http.StatusConflict, "in_flight_snapshot_cursor_expired")
}

func TestGetInFlightDetailsSnapshotCursorReportsRelationalLoadFailure(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	publishInFlightManagementSnapshot(t, handler.repo, 2, []cluster.InFlightRequestDetail{
		{RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-2 * time.Second)},
		{RequestID: "req-2", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-time.Second)},
	})
	first := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&stable_snapshot=true")
	var payload struct {
		SnapshotCursor string `json:"snapshot_cursor"`
	}
	if errDecode := json.Unmarshal(first.Body.Bytes(), &payload); errDecode != nil || payload.SnapshotCursor == "" {
		t.Fatalf("decode cursor response: %v payload=%#v", errDecode, payload)
	}
	if errDelete := db.Where("cursor = ? AND ordinal = ?", payload.SnapshotCursor, 1).
		Delete(&cluster.ManagementInFlightSnapshotCursorItemRecord{}).Error; errDelete != nil {
		t.Fatalf("delete cursor item: %v", errDelete)
	}

	second := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&offset=1&snapshot_cursor="+payload.SnapshotCursor)
	assertInFlightCursorError(t, second, http.StatusInternalServerError, "in_flight_snapshot_cursor_load_failed")
}

func TestGetInFlightDetailsSnapshotCursorFreshnessDegradesUsingDatabaseTime(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	publishInFlightManagementSnapshot(t, handler.repo, 2, []cluster.InFlightRequestDetail{
		{RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-2 * time.Second)},
		{RequestID: "req-2", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-time.Second)},
	})
	first := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&stable_snapshot=true")
	var firstPayload struct {
		SnapshotCursor string `json:"snapshot_cursor"`
	}
	if errDecode := json.Unmarshal(first.Body.Bytes(), &firstPayload); errDecode != nil || firstPayload.SnapshotCursor == "" {
		t.Fatalf("decode cursor response: %v payload=%#v", errDecode, firstPayload)
	}
	var cursorRecord cluster.ManagementInFlightSnapshotCursorRecord
	if errCursor := db.Where("cursor = ?", firstPayload.SnapshotCursor).First(&cursorRecord).Error; errCursor != nil {
		t.Fatalf("read snapshot cursor: %v", errCursor)
	}
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Minute)
	if errAge := db.Model(&cluster.ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", firstPayload.SnapshotCursor).
		Updates(map[string]any{"fresh_until": past, "expires_at": future}).Error; errAge != nil {
		t.Fatalf("update snapshot cursor freshness: %v", errAge)
	}
	second := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&offset=1&snapshot_cursor="+firstPayload.SnapshotCursor)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
	var secondPayload struct {
		Stale            bool `json:"stale"`
		CoverageComplete bool `json:"coverage_complete"`
	}
	if errDecode := json.Unmarshal(second.Body.Bytes(), &secondPayload); errDecode != nil {
		t.Fatalf("decode second response: %v", errDecode)
	}
	if !secondPayload.Stale || secondPayload.CoverageComplete {
		t.Fatalf("second payload = %#v", secondPayload)
	}
}

func TestGetInFlightDetailsFirstStablePageUsesCursorReadTimeForFreshness(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	publishInFlightManagementSnapshot(t, handler.repo, 2, []cluster.InFlightRequestDetail{
		{RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-2 * time.Second)},
		{RequestID: "req-2", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: inFlightManagementConnectedAt().Add(-time.Second)},
	})

	databaseNow, errNow := cluster.DatabaseNow(t.Context(), db)
	if errNow != nil {
		t.Fatalf("DatabaseNow() error = %v", errNow)
	}
	freshUntil := databaseNow.Add(2 * time.Second)
	updatedAt := freshUntil.Add(-handler.inFlightStaleAfter())
	if errUpdate := db.Model(&cluster.CPAInFlightSnapshotRecord{}).
		Where("certificate_fingerprint = ?", "in-flight-management-fingerprint").
		Update("updated_at", updatedAt).Error; errUpdate != nil {
		t.Fatalf("update snapshot freshness: %v", errUpdate)
	}
	preflight, errRead := handler.repo.ReadInFlightObservation(t.Context(), handler.inFlightStaleAfter())
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if preflight.Stale {
		t.Fatalf("preflight observation = %#v", preflight)
	}

	if errCallback := db.Callback().Create().Before("gorm:create").Register("in_flight_cursor_wait_until_stale", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != (cluster.ManagementInFlightSnapshotCursorRecord{}).TableName() {
			return
		}
		for {
			readAt, errReadAt := cluster.DatabaseNow(tx.Statement.Context, tx)
			if errReadAt != nil {
				tx.AddError(errReadAt)
				return
			}
			if readAt.After(freshUntil) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}); errCallback != nil {
		t.Fatalf("register create callback: %v", errCallback)
	}

	response := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight?credential_id=cred-a&limit=1&stable_snapshot=true")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Stale                bool   `json:"stale"`
		CoverageComplete     bool   `json:"coverage_complete"`
		SnapshotCursorReadAt string `json:"snapshot_cursor_read_at"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	readAt, errParse := time.Parse(time.RFC3339Nano, payload.SnapshotCursorReadAt)
	if errParse != nil {
		t.Fatalf("parse snapshot cursor read time %q: %v", payload.SnapshotCursorReadAt, errParse)
	}
	if !readAt.After(freshUntil) || !payload.Stale || payload.CoverageComplete {
		t.Fatalf("first stable page read_at=%s fresh_until=%s payload=%#v", readAt, freshUntil, payload)
	}
}

func TestUpdateInFlightDetailsFreshnessUsesCursorReadTime(t *testing.T) {
	freshUntil := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	read := cluster.InFlightObservationReadModel{
		FreshUntil:       &freshUntil,
		CoverageComplete: true,
	}

	fresh := updateInFlightDetailsFreshness(read, freshUntil)
	if fresh.Stale || !fresh.CoverageComplete {
		t.Fatalf("fresh read = %#v", fresh)
	}

	stale := updateInFlightDetailsFreshness(read, freshUntil.Add(time.Nanosecond))
	if !stale.Stale || stale.CoverageComplete {
		t.Fatalf("stale read = %#v", stale)
	}
}

func TestFilterInFlightDetailsStatesUsesDetailCredentialIDs(t *testing.T) {
	read := cluster.InFlightObservationReadModel{
		Details: []cluster.InFlightRequestDetail{{CredentialID: "cred-a"}},
	}
	states := []cluster.CredentialConcurrencyState{
		{CredentialID: "cred-a"},
		{CredentialID: "cred-b"},
	}
	filtered := filterInFlightDetailsStates(states, read)
	if len(filtered) != 1 || filtered[0].CredentialID != "cred-a" {
		t.Fatalf("filtered states = %#v", filtered)
	}
}

func assertInFlightCursorError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), status)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode error response: %v", errDecode)
	}
	if payload.Error != code {
		t.Fatalf("error = %q, want %q", payload.Error, code)
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
	if !payload.Capabilities["credential_in_flight_snapshot_cursor"] {
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

	connectedAt := inFlightManagementConnectedAt()
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
	publishInFlightManagementSnapshotWithAggregates(t, repo, 1, []cluster.InFlightAggregate{
		{CredentialID: "cred-a", Model: "gpt-5", Status: cluster.InFlightAccounted, Count: 2},
		{CredentialID: "cred-a", Model: "gpt-5", Status: cluster.InFlightUnaccounted, Count: 1},
	}, []cluster.InFlightRequestDetail{{
		RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: connectedAt.Add(-2 * time.Second),
	}})
}

func inFlightManagementConnectedAt() time.Time {
	return time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
}

func publishInFlightManagementSnapshot(t *testing.T, repo *cluster.Repository, revision int64, details []cluster.InFlightRequestDetail) {
	t.Helper()
	publishInFlightManagementSnapshotWithAggregates(t, repo, revision, []cluster.InFlightAggregate{
		{CredentialID: "cred-a", Model: "gpt-5", Status: cluster.InFlightAccounted, Count: int64(len(details))},
	}, details)
}

func publishInFlightManagementSnapshotWithAggregates(t *testing.T, repo *cluster.Repository, revision int64, aggregates []cluster.InFlightAggregate, details []cluster.InFlightRequestDetail) {
	t.Helper()
	connectedAt := inFlightManagementConnectedAt()
	partIndex := 0
	partCount := 1
	frame := cluster.InFlightSnapshotFrame{
		Kind:            cluster.InFlightFramePart,
		Revision:        revision,
		ObservedAt:      connectedAt.Add(time.Duration(revision-1) * time.Second),
		BarrierRevision: 0,
		PartIndex:       &partIndex,
		PartCount:       &partCount,
		Aggregates:      aggregates,
		Details:         details,
	}
	raw, errMarshal := json.Marshal(frame)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	result, errIngest := repo.IngestInFlightFrame(context.Background(), cluster.InFlightIngestIdentity{
		CertificateFingerprint: "in-flight-management-fingerprint",
		NodeID:                 "in-flight-management-node",
		MembershipConnectedAt:  connectedAt,
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
