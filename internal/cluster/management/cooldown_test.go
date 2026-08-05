package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

type clearCredentialCooldownResponse struct {
	Status        string   `json:"status"`
	CredentialID  string   `json:"credential_id"`
	Scope         string   `json:"scope"`
	Model         string   `json:"model"`
	Cleared       bool     `json:"cleared"`
	ClearedModels []string `json:"cleared_models"`
}

func newCooldownManagementHandler(t *testing.T, auth *coreauth.Auth) (*Handler, *cluster.Repository) {
	t.Helper()
	db, cleanup := openManagementLogTestDB(t)
	t.Cleanup(cleanup)
	repo := cluster.NewRepository(db)
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	runtime, errRuntime := home.NewRuntime(&appconfig.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatalf("home.NewRuntime() error = %v", errRuntime)
	}
	t.Cleanup(runtime.Stop)
	runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "127.0.0.1"))
	if errReload := runtime.ReloadAuths(context.Background()); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}
	return NewHandler(repo, runtime, "127.0.0.1", 0), repo
}

func managementQuotaState(now time.Time, delay time.Duration) *coreauth.ModelState {
	next := now.Add(delay)
	return &coreauth.ModelState{
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		UpdatedAt:      now,
	}
}

func TestClearCredentialCooldownForModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	modelA := managementQuotaState(now, 10*time.Minute)
	modelB := managementQuotaState(now, 20*time.Minute)
	auth := &coreauth.Auth{
		ID:             "credential-model-clear",
		Index:          "credential-model-clear",
		Provider:       "codex",
		Status:         coreauth.StatusError,
		Unavailable:    true,
		NextRetryAfter: modelA.NextRetryAfter,
		Quota:          modelA.Quota,
		Metadata:       map[string]any{"type": "codex"},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-a": modelA,
			"gpt-b": modelB,
		},
	}
	handler, repo := newCooldownManagementHandler(t, auth)
	engine := gin.New()
	engine.DELETE("/credentials/:credential_id/cooldown", handler.ClearCredentialCooldown)

	path := "/credentials/credential-model-clear/cooldown?model=" + url.QueryEscape("gpt-a(high)")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload clearCredentialCooldownResponse
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	var fields map[string]json.RawMessage
	if errDecode := json.Unmarshal(response.Body.Bytes(), &fields); errDecode != nil {
		t.Fatalf("decode response fields: %v", errDecode)
	}
	if _, exists := fields["credential_cooldown_cleared"]; exists {
		t.Fatalf("response contains removed credential-wide field: %s", response.Body.String())
	}
	if payload.Status != "ok" || payload.CredentialID != auth.ID || payload.Scope != "model" || payload.Model != "gpt-a" || !payload.Cleared || !reflect.DeepEqual(payload.ClearedModels, []string{"gpt-a"}) {
		t.Fatalf("response = %#v", payload)
	}

	persisted, _, errGet := repo.GetAuth(context.Background(), auth.ID)
	if errGet != nil {
		t.Fatalf("GetAuth() error = %v", errGet)
	}
	if persisted.ModelStates["gpt-a"].Quota.Exceeded || persisted.ModelStates["gpt-a"].Unavailable {
		t.Fatalf("gpt-a state = %#v, want cleared", persisted.ModelStates["gpt-a"])
	}
	if !persisted.ModelStates["gpt-b"].Quota.Exceeded || !persisted.ModelStates["gpt-b"].Unavailable {
		t.Fatalf("gpt-b state = %#v, want preserved", persisted.ModelStates["gpt-b"])
	}
}

func TestClearCredentialCooldownForAllModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	modelA := managementQuotaState(now, 10*time.Minute)
	modelB := managementQuotaState(now, 20*time.Minute)
	auth := &coreauth.Auth{
		ID:             "credential-all-clear",
		Index:          "credential-all-clear",
		Provider:       "codex",
		Status:         coreauth.StatusError,
		Unavailable:    true,
		NextRetryAfter: modelA.NextRetryAfter,
		Quota:          modelA.Quota,
		Metadata:       map[string]any{"type": "codex"},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-a": modelA,
			"gpt-b": modelB,
		},
	}
	handler, repo := newCooldownManagementHandler(t, auth)
	engine := gin.New()
	engine.DELETE("/credentials/:credential_id/cooldown", handler.ClearCredentialCooldown)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/credentials/credential-all-clear/cooldown", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload clearCredentialCooldownResponse
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.Scope != "all" || payload.Model != "" || !payload.Cleared || !reflect.DeepEqual(payload.ClearedModels, []string{"gpt-a", "gpt-b"}) {
		t.Fatalf("response = %#v", payload)
	}

	persisted, _, errGet := repo.GetAuth(context.Background(), auth.ID)
	if errGet != nil {
		t.Fatalf("GetAuth() error = %v", errGet)
	}
	for _, model := range []string{"gpt-a", "gpt-b"} {
		state := persisted.ModelStates[model]
		if state == nil || state.Quota.Exceeded || state.Unavailable {
			t.Fatalf("%s state = %#v, want cleared", model, state)
		}
	}
}

func TestClearCredentialCooldownValidationAndNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	auth := &coreauth.Auth{ID: "credential-validation", Index: "credential-validation", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	handler, _ := newCooldownManagementHandler(t, auth)
	engine := gin.New()
	engine.DELETE("/credentials/:credential_id/cooldown", handler.ClearCredentialCooldown)

	emptyModel := httptest.NewRecorder()
	engine.ServeHTTP(emptyModel, httptest.NewRequest(http.MethodDelete, "/credentials/credential-validation/cooldown?model=", nil))
	if emptyModel.Code != http.StatusBadRequest {
		t.Fatalf("empty model status = %d, body = %s", emptyModel.Code, emptyModel.Body.String())
	}

	missing := httptest.NewRecorder()
	engine.ServeHTTP(missing, httptest.NewRequest(http.MethodDelete, "/credentials/missing/cooldown", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing credential status = %d, body = %s", missing.Code, missing.Body.String())
	}

	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := httptest.NewRecorder()
	requestCanceled := httptest.NewRequest(http.MethodDelete, "/credentials/credential-validation/cooldown", nil).WithContext(ctxCanceled)
	engine.ServeHTTP(canceled, requestCanceled)
	if canceled.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled request status = %d, body = %s", canceled.Code, canceled.Body.String())
	}
}
