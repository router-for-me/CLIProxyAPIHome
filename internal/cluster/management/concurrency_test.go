package management

import (
	"bytes"
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

func TestGetCredentialConcurrencyWithoutObservation(t *testing.T) {
	handler, engine, db, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedManagementPolicyAndCounter(t, handler.repo, db, "cred-1", "gpt", 3, 2)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/credentials/concurrency", nil)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	want := readConcurrencyTestFixture(t, "testdata/concurrency_state.json")
	assertConcurrencyJSONEqual(t, recorder.Body.Bytes(), want)
}

func TestGetCredentialConcurrencyIgnoresObservationFailure(t *testing.T) {
	handler, engine, db, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedManagementPolicyAndCounter(t, handler.repo, db, "cred-1", "gpt", 3, 2)
	if errDrop := db.Migrator().DropTable(&cluster.CPAInFlightSnapshotRecord{}); errDrop != nil {
		t.Fatalf("drop snapshots: %v", errDrop)
	}

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/credentials/concurrency", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []struct {
			FullyEnforced string `json:"fully_enforced"`
			Observed      any    `json:"observed"`
		} `json:"items"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Items) != 1 || payload.Items[0].FullyEnforced != "unknown" || payload.Items[0].Observed != nil {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestFullyEnforcedConcurrencyDiagnostic(t *testing.T) {
	barrier := int64(2)
	state := cluster.CredentialConcurrencyState{ObservationBarrier: barrier}
	observed := cluster.InFlightObservedCredentialItem{CredentialID: "cred"}
	fresh := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	minimumBarrier := int64(2)
	complete := cluster.InFlightObservationReadModel{
		ObservedAt:                      &fresh,
		CoverageComplete:                true,
		AggregatesComplete:              true,
		ProtocolCoverageComplete:        true,
		MinimumProcessedBarrierRevision: &minimumBarrier,
	}

	if got := fullyEnforcedConcurrency(state, &complete, observed, true); got != "true" {
		t.Fatalf("fullyEnforcedConcurrency() = %q, want true", got)
	}
	if got := fullyEnforcedConcurrency(state, &complete, observed, false); got != "unknown" {
		t.Fatalf("fullyEnforcedConcurrency() without credential observation = %q, want unknown", got)
	}
	observed.ObservedUnaccounted = 1
	if got := fullyEnforcedConcurrency(state, &complete, observed, true); got != "false" {
		t.Fatalf("fullyEnforcedConcurrency() = %q, want false", got)
	}

	for name, mutate := range map[string]func(*cluster.InFlightObservationReadModel){
		"missing":               func(read *cluster.InFlightObservationReadModel) { read.ObservedAt = nil },
		"stale":                 func(read *cluster.InFlightObservationReadModel) { read.Stale = true },
		"incomplete aggregates": func(read *cluster.InFlightObservationReadModel) { read.AggregatesComplete = false },
		"protocol":              func(read *cluster.InFlightObservationReadModel) { read.ProtocolCoverageComplete = false },
		"canceling":             func(read *cluster.InFlightObservationReadModel) { read.CoverageComplete = false },
		"old policy snapshot": func(read *cluster.InFlightObservationReadModel) {
			old := int64(1)
			read.MinimumProcessedBarrierRevision = &old
		},
	} {
		t.Run(name, func(t *testing.T) {
			read := complete
			mutate(&read)
			if got := fullyEnforcedConcurrency(state, &read, observed, true); got != "unknown" {
				t.Fatalf("fullyEnforcedConcurrency() = %q, want unknown", got)
			}
		})
	}

	acknowledged := complete
	if got := fullyEnforcedConcurrency(state, &acknowledged, observed, true); got != "false" {
		t.Fatalf("fullyEnforcedConcurrency() after barrier acknowledgement = %q, want false", got)
	}
}

func TestPatchAuthFileConcurrencyDelegatesPolicyService(t *testing.T) {
	handler, engine, _, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-1")

	body := strings.NewReader(`{"id":"oauth-1","max_in_flight":2,"max_in_flight_by_model":{"gpt(high)":1}}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/auth-files/fields", body)
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	policy := mustManagementPolicy(t, handler.repo, "oauth-1")
	if policy.MaxInFlight == nil || *policy.MaxInFlight != 2 || policy.MaxInFlightByModel["gpt"] != 1 {
		t.Fatalf("policy = %#v", policy)
	}
	assertAuthJSONHasNoField(t, handler.repo, "oauth-1", "max_in_flight")
	assertAuthJSONHasNoField(t, handler.repo, "oauth-1", "max_in_flight_by_model")
}

func TestPatchAuthFileRollbackPreservesPolicyOnAuthFailure(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedOAuthAuth(t, handler.repo, "oauth-rollback")

	limit := int64(1)
	errPatch := handler.repo.PatchAuthAndConcurrency(context.Background(), cluster.AuthConcurrencyPatchRequest{
		CredentialID: "oauth-rollback",
		Auth:         &coreauth.Auth{ID: "wrong-id", Index: "wrong-id"},
		AuthChanged:  true,
		PolicyPatch: cluster.ConcurrencyPolicyPatch{
			MaxInFlight: cluster.OptionalLimit{Set: true, Value: limit},
		},
	})
	if errPatch == nil {
		t.Fatal("PatchAuthAndConcurrency() error = nil, want auth failure")
	}
	policy := mustManagementPolicy(t, handler.repo, "oauth-rollback")
	if policy.MaxInFlight != nil || len(policy.MaxInFlightByModel) != 0 || policy.Version != 0 {
		t.Fatalf("policy after rollback = %#v", policy)
	}
}

func TestInFlightSummaryMergesAuthoritativeConcurrencyState(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	seedManagementPolicyAndCounter(t, handler.repo, db, "cred-a", "gpt-5", 3, 2)

	response := performManagementGET(t, handler.GetInFlightSummary, "/credentials/in-flight/summary")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []struct {
			MaxInFlight      *int64 `json:"max_in_flight"`
			AdmittedInFlight *int64 `json:"admitted_in_flight"`
			Limiter          any    `json:"limiter"`
		} `json:"items"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if len(payload.Items) != 1 || payload.Items[0].MaxInFlight == nil || *payload.Items[0].MaxInFlight != 3 || payload.Items[0].AdmittedInFlight == nil || *payload.Items[0].AdmittedInFlight != 2 || payload.Items[0].Limiter == nil {
		t.Fatalf("summary items = %#v", payload.Items)
	}
}

func TestInFlightDetailsMergesAuthoritativeConcurrencyState(t *testing.T) {
	handler, db, closeRepo := newInFlightManagementTestHandler(t)
	defer closeRepo()
	seedInFlightManagementSnapshot(t, handler.repo, db)
	seedManagementPolicyAndCounter(t, handler.repo, db, "cred-a", "gpt-5", 3, 2)

	response := performManagementGET(t, handler.GetInFlightDetails, "/credentials/in-flight")
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
	if len(payload.Items) != 1 || payload.Items[0].Limiter == nil {
		t.Fatalf("details items = %#v", payload.Items)
	}
}

func TestPatchCredentialConcurrencyPolicyRejectsVersionConflict(t *testing.T) {
	handler, engine, db, closeRepo := newConcurrencyManagementTestServer(t)
	defer closeRepo()
	seedManagementPolicyAndCounter(t, handler.repo, db, "cred-version", "gpt", 3, 0)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/credentials/cred-version/concurrency-policy", strings.NewReader(`{"version":0,"max_in_flight":4}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want %d", recorder.Code, recorder.Body.String(), http.StatusConflict)
	}
}

func newConcurrencyManagementTestServer(t *testing.T) (*Handler, *gin.Engine, *gorm.DB, func()) {
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
	handler := NewHandler(cluster.NewRepository(db), nil, "192.0.2.10", 0)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/credentials/concurrency-policies", handler.ListCredentialConcurrencyPolicies)
	engine.GET("/credentials/:credential_id/concurrency-policy", handler.GetCredentialConcurrencyPolicy)
	engine.PATCH("/credentials/:credential_id/concurrency-policy", handler.PatchCredentialConcurrencyPolicy)
	engine.GET("/credentials/concurrency", handler.GetCredentialConcurrency)
	engine.PATCH("/auth-files/fields", handler.PatchAuthFileFields)
	return handler, engine, db, closeRepo
}

func seedManagementPolicyAndCounter(t *testing.T, repo *cluster.Repository, db *gorm.DB, credentialID string, model string, maxInFlight int64, admitted int64) {
	t.Helper()
	seedOAuthAuth(t, repo, credentialID)
	effectiveAt := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	if errCreate := db.Create(&cluster.CredentialConcurrencyPolicyRecord{
		CredentialID: credentialID,
		MaxInFlight:  &maxInFlight,
		Version:      1,
		EffectiveAt:  effectiveAt,
	}).Error; errCreate != nil {
		t.Fatalf("create policy: %v", errCreate)
	}
	if errCreate := db.Create(&cluster.CredentialConcurrencyModelPolicyRecord{
		CredentialID: credentialID,
		Model:        model,
		MaxInFlight:  maxInFlight - 1,
	}).Error; errCreate != nil {
		t.Fatalf("create model policy: %v", errCreate)
	}
	if admitted == 0 {
		return
	}
	if errCreate := db.Create(&cluster.CredentialConcurrencyCounterRecord{
		CredentialID:           credentialID,
		Model:                  model,
		CertificateFingerprint: "management-test-node",
		ActiveCount:            admitted,
		UpdatedAt:              effectiveAt,
	}).Error; errCreate != nil {
		t.Fatalf("create counter: %v", errCreate)
	}
}

func seedOAuthAuth(t *testing.T, repo *cluster.Repository, credentialID string) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:       credentialID,
		Index:    credentialID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
}

func mustManagementPolicy(t *testing.T, repo *cluster.Repository, credentialID string) cluster.CredentialConcurrencyPolicy {
	t.Helper()
	policy, errPolicy := repo.GetCredentialConcurrencyPolicy(context.Background(), credentialID)
	if errPolicy != nil {
		t.Fatalf("GetCredentialConcurrencyPolicy() error = %v", errPolicy)
	}
	return policy
}

func assertAuthJSONHasNoField(t *testing.T, repo *cluster.Repository, credentialID string, field string) {
	t.Helper()
	_, record, errAuth := repo.GetAuth(context.Background(), credentialID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(record.AuthJSON, &payload); errDecode != nil {
		t.Fatalf("decode auth json: %v", errDecode)
	}
	if _, exists := payload[field]; exists {
		t.Fatalf("auth JSON unexpectedly contains %q: %#v", field, payload)
	}
}

func readConcurrencyTestFixture(t *testing.T, path string) []byte {
	t.Helper()
	fixture, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, errRead)
	}
	return fixture
}

func assertConcurrencyJSONEqual(t *testing.T, actual []byte, want []byte) {
	t.Helper()
	var gotValue any
	if errDecode := json.NewDecoder(bytes.NewReader(actual)).Decode(&gotValue); errDecode != nil {
		t.Fatalf("decode actual JSON: %v", errDecode)
	}
	var wantValue any
	if errDecode := json.NewDecoder(bytes.NewReader(want)).Decode(&wantValue); errDecode != nil {
		t.Fatalf("decode fixture JSON: %v", errDecode)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("response = %s, want fixture %s", actual, want)
	}
}
