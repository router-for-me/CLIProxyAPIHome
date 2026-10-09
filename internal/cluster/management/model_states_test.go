package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type modelStatesResponse struct {
	Source      string                      `json:"source"`
	ObservedAt  time.Time                   `json:"observed_at"`
	Node        map[string]any              `json:"node"`
	Total       int                         `json:"total"`
	Credentials []CredentialModelStatesItem `json:"credentials"`
	Credential  *CredentialModelStatesItem  `json:"credential"`
}

func newModelStatesHandler(t *testing.T, auths ...*coreauth.Auth) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	runtime, errRuntime := home.NewRuntime(&appconfig.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(runtime.Stop)
	manager := runtime.CoreManager()
	manager.SetStore(nil)
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		models := make([]*registry.ModelInfo, 0, len(auth.ModelStates))
		for key := range auth.ModelStates {
			models = append(models, &registry.ModelInfo{ID: key})
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		manager.RefreshSchedulerEntry(auth.ID)
	}
	handler := NewHandler(nil, runtime, "127.0.0.1", 8317)
	engine := gin.New()
	engine.GET("/credentials/model-states", handler.GetCredentialModelStates)
	engine.GET("/credentials/:credential_id/model-states", handler.GetCredentialModelStates)
	return handler, engine
}

func TestGetCredentialModelStates(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "model-states-xai", Index: "model-states-xai", Provider: "xai", Status: coreauth.StatusActive,
		StateVersion: 7,
		ModelStates: map[string]*coreauth.ModelState{
			"grok-4.7":            {Status: coreauth.StatusActive},
			"grok-4.7-build-fast": {Status: coreauth.StatusActive},
			"grok-4.6":            {Status: coreauth.StatusActive},
		},
	}
	handler, engine := newModelStatesHandler(t, auth)
	for _, model := range []string{"grok-4.7", "grok-4.7-build-fast"} {
		handler.runtime.RecordUsagePayload(context.Background(), `{"auth_index":"model-states-xai","provider":"xai","model":"`+model+`","failed":true,"fail":{"status_code":499,"body":"context canceled"}}`)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/credentials/model-states?provider=xai", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response modelStatesResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Source != "runtime" || response.ObservedAt.IsZero() || response.Node["ip"] != "127.0.0.1" || response.Node["port"] != float64(8317) {
		t.Fatalf("missing snapshot provenance: %#v", response)
	}
	if response.Total != 1 || len(response.Credentials) != 1 || response.Credentials[0].StateVersion != 7 {
		t.Fatalf("wrong credential snapshot: %#v", response)
	}
	models := response.Credentials[0].Models
	if len(models) != 3 || models[0].Model != "grok-4.6" || models[0].Status != coreauth.StatusActive {
		t.Fatalf("registered models must be sorted: %#v", models)
	}
	for _, model := range models[1:] {
		if model.LastError == nil || model.LastError.HTTPStatus != 499 || model.Blocked || model.BlockReason != "none" {
			t.Fatalf("499 is history, not a scheduling gate: %#v", model)
		}
		if model.UpstreamModel != model.Model || model.StateKey != model.Model || !model.Registered {
			t.Fatalf("missing state mapping: %#v", model)
		}
	}
	if strings.Contains(recorder.Body.String(), `"next_retry_after"`) || strings.Contains(recorder.Body.String(), `"quota"`) || strings.Contains(recorder.Body.String(), `"last_error":null`) {
		t.Fatalf("unset optional fields must be omitted: %s", recorder.Body.String())
	}
}

func TestCredentialModelStatesFiltersAndResponseShape(t *testing.T) {
	_, engine := newModelStatesHandler(t,
		&coreauth.Auth{ID: "filter-xai", Provider: "xai", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{"grok-4.7": {Status: coreauth.StatusActive}}},
		&coreauth.Auth{ID: "filter-codex", Provider: "codex", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{"gpt-5": {Status: coreauth.StatusActive}}},
	)
	for _, tc := range []struct {
		url    string
		status int
		total  int
		single bool
	}{
		{url: "/credentials/model-states", status: 200, total: 2},
		{url: "/credentials/model-states?provider=XAI", status: 200, total: 1},
		{url: "/credentials/model-states?model=grok-4.7", status: 200, total: 1},
		{url: "/credentials/model-states?credential_id=filter-xai", status: 200, total: 1},
		{url: "/credentials/model-states?credential_id=filter-xai&provider=codex", status: 200},
		{url: "/credentials/model-states?credential_id=filter-xai&model=missing", status: 200},
		{url: "/credentials/model-states?credential_id=missing", status: 200},
		{url: "/credentials/filter-xai/model-states", status: 200, single: true},
		{url: "/credentials/filter-xai/model-states?model=missing", status: 200, single: true},
		{url: "/credentials/filter-xai/model-states?provider=codex", status: 404},
		{url: "/credentials/missing/model-states", status: 404},
	} {
		t.Run(tc.url, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if recorder.Code != tc.status {
				t.Fatalf("status=%d, want %d: %s", recorder.Code, tc.status, recorder.Body.String())
			}
			if tc.status != http.StatusOK {
				return
			}
			var response modelStatesResponse
			if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
				t.Fatal(errDecode)
			}
			if tc.single {
				if response.Credential == nil || response.Credential.CredentialID != "filter-xai" || response.Credentials != nil {
					t.Fatalf("single path must return one object: %#v", response)
				}
			} else if response.Credential != nil || response.Credentials == nil || response.Total != tc.total || len(response.Credentials) != tc.total {
				t.Fatalf("list response shape must not change with filters: %#v", response)
			}
		})
	}
}

func TestCredentialModelStatesWhitespacePathRejected(t *testing.T) {
	_, engine := newModelStatesHandler(t,
		&coreauth.Auth{ID: "whitespace-path-xai", Provider: "xai", Status: coreauth.StatusActive},
		&coreauth.Auth{ID: "whitespace-path-codex", Provider: "codex", Status: coreauth.StatusActive},
	)
	for _, path := range []string{
		"/credentials/%20/model-states",
		"/credentials/%20/model-states?credential_id=whitespace-path-xai",
		"/credentials/%20%09/model-states?credential_id=whitespace-path-codex",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("whitespace path ID must not list credentials or use a query ID: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Error       string                      `json:"error"`
				Credential  *CredentialModelStatesItem  `json:"credential"`
				Credentials []CredentialModelStatesItem `json:"credentials"`
			}
			if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if response.Error != "credential_not_found" || response.Credential != nil || response.Credentials != nil {
				t.Fatalf("invalid path must return only the not-found error envelope: %#v", response)
			}
		})
	}
}

func TestCredentialModelStatesRuntimeUnavailable(t *testing.T) {
	handler := NewHandler(nil, nil, "127.0.0.1", 8317)
	engine := gin.New()
	engine.GET("/model-states", handler.GetCredentialModelStates)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/model-states", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing runtime must not return persisted or guessed availability: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialModelStatesRuntimeOnly(t *testing.T) {
	const model = "grok-4.7"
	auth := &coreauth.Auth{
		ID: "runtime-only", Provider: "xai", Status: coreauth.StatusError, StateVersion: 2,
		ModelStates: map[string]*coreauth.ModelState{model: {Status: coreauth.StatusError, LastError: &coreauth.Error{HTTPStatus: 499}}},
	}
	handler, engine := newModelStatesHandler(t, auth)
	db, cleanup := openManagementLogTestDB(t)
	t.Cleanup(cleanup)
	repo := cluster.NewRepository(db)
	persisted := auth.Clone()
	persisted.Index = persisted.ID
	persisted.ModelStates[model].LastError.HTTPStatus = 503
	persisted.ModelStates[model].Unavailable = true
	persisted.ModelStates[model].NextRetryAfter = time.Now().Add(time.Hour)
	if _, errUpsert := repo.UpsertAuth(context.Background(), persisted, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	handler.repo = repo
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/credentials/runtime-only/model-states", nil))
	var response modelStatesResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if recorder.Code != 200 || response.Credential == nil || response.Credential.StateVersion != 2 || response.Credential.Models[0].LastError.HTTPStatus != 499 || response.Credential.Models[0].Blocked {
		t.Fatalf("persisted state was merged into runtime response: %s", recorder.Body.String())
	}
	// An unusable repository must not prevent reading the in-memory scheduler snapshot.
	handler.repo = cluster.NewRepository(nil)
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/credentials/model-states", nil))
	if recorder.Code != 200 {
		t.Fatalf("runtime query depended on database availability: %s", recorder.Body.String())
	}
}

func TestCredentialModelStatesClusterPrefixAndAlias(t *testing.T) {
	const upstream = "grok-4.7-build-fast"
	for _, kind := range []string{"apikey", "oauth"} {
		t.Run(kind, func(t *testing.T) {
			handler, engine := newModelStatesHandler(t)
			db, cleanup := openManagementLogTestDB(t)
			t.Cleanup(cleanup)
			repo := cluster.NewRepository(db)
			id := "cluster-alias-" + kind
			modelID := upstream
			if kind == "apikey" {
				modelID = "fast"
			}
			auth := &coreauth.Auth{
				ID: id, Index: id, Provider: "xai", Prefix: "team", Status: coreauth.StatusError,
				Attributes: map[string]string{"auth_kind": kind},
				Metadata: map[string]any{
					"type": "xai", "filename": "private-name.json", "access_token": "test-only-token",
					"home_config_models": []map[string]any{{"id": modelID, "name": upstream, "user_defined": kind == "apikey"}},
				},
				ModelStates: map[string]*coreauth.ModelState{upstream: {
					Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 499},
				}},
			}
			if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "register"); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			cfg := &appconfig.Config{
				SDKConfig:       appconfig.SDKConfig{ForceModelPrefix: true},
				OAuthModelAlias: map[string][]appconfig.OAuthModelAlias{"xai": {{Name: upstream, Alias: "fast"}}},
			}
			if errApply := handler.runtime.ApplyConfigFromCluster(context.Background(), cfg); errApply != nil {
				t.Fatal(errApply)
			}
			handler.runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "127.0.0.1"))
			if errReload := handler.runtime.ReloadAuths(context.Background()); errReload != nil {
				t.Fatal(errReload)
			}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			live, _ := handler.runtime.CoreManager().GetByID(id)
			if live.Metadata["access_token"] != nil || live.Metadata["filename"] != nil {
				t.Fatal("test requires the minimal cluster runtime projection")
			}
			for _, filter := range []string{"fast", "team/fast", upstream} {
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/credentials/model-states?model="+filter, nil))
				var response modelStatesResponse
				if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
					t.Fatal(errDecode)
				}
				if recorder.Code != 200 || response.Total != 1 || len(response.Credentials[0].Models) != 1 {
					t.Fatalf("filter %q: %s", filter, recorder.Body.String())
				}
				model := response.Credentials[0].Models[0]
				if model.Model != "team/fast" || model.UpstreamModel != upstream || model.StateKey != upstream || model.LastError.HTTPStatus != 499 || model.Blocked {
					t.Fatalf("filter %q: wrong prefixed alias state: %#v", filter, model)
				}
				if strings.Contains(recorder.Body.String(), "test-only-token") || strings.Contains(recorder.Body.String(), "private-name") {
					t.Fatal("model diagnostics must not fetch or serialize provider credential metadata")
				}
			}
		})
	}
}

func TestCredentialModelStatesReadDoesNotSelect(t *testing.T) {
	const model = "grok-4.6"
	handler, engine := newModelStatesHandler(t,
		&coreauth.Auth{ID: "read-a", Provider: "xai", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{model: {Status: coreauth.StatusActive}}},
		&coreauth.Auth{ID: "read-b", Provider: "xai", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{model: {Status: coreauth.StatusActive}}},
	)
	manager := handler.runtime.CoreManager()
	before, errBefore := manager.Dispatch(context.Background(), []string{"xai"}, model, coreauth.Options{})
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/credentials/model-states", nil))
	after, errAfter := manager.Dispatch(context.Background(), []string{"xai"}, model, coreauth.Options{})
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if recorder.Code != 200 || before.Auth.ID != "read-a" || after.Auth.ID != "read-b" {
		t.Fatalf("diagnostic read changed round-robin selection: before=%s after=%s", before.Auth.ID, after.Auth.ID)
	}
}

func TestCredentialModelStatesEffectiveRetryDeadline(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID: "effective-retry", Provider: "xai", Status: coreauth.StatusError,
		ModelStates: map[string]*coreauth.ModelState{"grok-4.7": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(100 * time.Millisecond),
			LastError: &coreauth.Error{HTTPStatus: 503},
		}},
	}
	handler, _ := newModelStatesHandler(t, auth)
	manager := handler.runtime.CoreManager()
	item := buildCredentialModelStatesItem(manager, auth, "grok-4.7", now)
	if item.Models[0].RemainingCooldown != "1s" || !item.Models[0].NextRetryAfter.Equal(now.Add(100*time.Millisecond)) {
		t.Fatalf("effective deadline must not round a live gate down to zero: %#v", item.Models[0])
	}
	manager.SetConfig(&appconfig.Config{DisableCooling: true})
	item = buildCredentialModelStatesItem(manager, auth, "grok-4.7", now)
	if item.Models[0].Blocked || !item.Models[0].NextRetryAfter.IsZero() || item.Models[0].RemainingCooldown != "" || item.Models[0].LastError.HTTPStatus != 503 {
		t.Fatalf("disabled cooling must preserve history but clear the effective retry deadline: %#v", item.Models[0])
	}
}
