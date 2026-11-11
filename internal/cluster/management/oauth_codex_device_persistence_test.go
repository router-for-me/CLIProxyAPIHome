package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestCodexDeviceCallbackRejectsGetAndPostWithoutMutation(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	requests := 0
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(*http.Request) (*http.Response, error) {
		requests++
		return managementDeviceResponse(http.StatusOK, `{}`), nil
	})}
	state, errState := generateOAuthState("cdx")
	if errState != nil {
		t.Fatal(errState)
	}
	session, errSession := cluster.NewOAuthSessionRecord("codex", state, map[string]any{
		"flow":           "device",
		"device_auth_id": "private-device-id",
		"user_code":      "ABCD-EFGH",
	}, time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(context.Background(), session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	engine := gin.New()
	engine.GET("/v8/management/oauth/callback", h.PostOAuthCallbackV8)
	engine.POST("/v8/management/oauth/callback", h.PostOAuthCallbackV8)

	getResponse := httptest.NewRecorder()
	engine.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/callback?state="+url.QueryEscape(state), nil))
	missingCodePost := httptest.NewRecorder()
	engine.ServeHTTP(missingCodePost, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", strings.NewReader(`{"state":"`+state+`"}`)))
	postBody := strings.NewReader(`{"provider":"codex","state":"` + state + `","code":"unused"}`)
	postRequest := httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", postBody)
	postRequest.Header.Set("Content-Type", "application/json")
	postResponse := httptest.NewRecorder()
	engine.ServeHTTP(postResponse, postRequest)
	if getResponse.Code != http.StatusConflict || missingCodePost.Code != http.StatusConflict || postResponse.Code != http.StatusConflict {
		t.Fatalf("callback statuses = GET %d, missing-code POST %d, POST %d", getResponse.Code, missingCodePost.Code, postResponse.Code)
	}
	if requests != 0 {
		t.Fatalf("provider requests = %d, want 0", requests)
	}
	current, errCurrent := repo.GetOAuthSession(context.Background(), state)
	if errCurrent != nil || current == nil || current.Status != "" {
		t.Fatalf("callback changed session = %+v, error=%v", current, errCurrent)
	}
	data, errData := cluster.OAuthSessionData(current)
	if errData != nil || data["flow"] != "device" || data["device_auth_id"] != "private-device-id" || data["user_code"] != "ABCD-EFGH" {
		t.Fatalf("callback changed session data = %v, error=%v", data, errData)
	}
}

func TestCodexDeviceV0CallbackKeepsMissingCodeValidation(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	state, errState := generateOAuthState("cdx")
	if errState != nil {
		t.Fatal(errState)
	}
	session, errSession := cluster.NewOAuthSessionRecord("codex", state, map[string]any{
		"flow":           "device",
		"device_auth_id": "private-device-id",
		"user_code":      "ABCD-EFGH",
	}, time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(context.Background(), session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.POST("/v0/management/oauth-callback", h.PostOAuthCallback)
	request := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(`{"state":"`+state+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "code or error is required") {
		t.Fatalf("v0 device callback status = %d, body=%s", response.Code, response.Body.String())
	}
	current, errCurrent := repo.GetOAuthSession(context.Background(), state)
	if errCurrent != nil || current == nil || current.Status != "" {
		t.Fatalf("v0 callback changed session = %+v, error=%v", current, errCurrent)
	}
}

func TestCompletedClaudeCallbackWithoutCodeKeepsValidationResponse(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	state, errState := generateOAuthState("cla")
	if errState != nil {
		t.Fatal(errState)
	}
	session, errSession := cluster.NewOAuthSessionRecord("claude", state, nil, time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(context.Background(), session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if errComplete := repo.CompleteOAuthSession(context.Background(), state); errComplete != nil {
		t.Fatal(errComplete)
	}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/v8/management/oauth/callback", h.PostOAuthCallbackV8)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/callback?state="+url.QueryEscape(state), nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "code or error is required") {
		t.Fatalf("completed Claude callback status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestCodexDeviceWorkerExchangesAndPersistsCredential(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	state, errState := generateOAuthState("cdx")
	if errState != nil {
		t.Fatal(errState)
	}
	session, errSession := cluster.NewOAuthSessionRecord("codex", state, map[string]any{"flow": "device"}, time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(context.Background(), session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/accounts/deviceauth/token":
			return managementDeviceResponse(http.StatusOK, `{"authorization_code":"auth-code","code_verifier":"verifier","code_challenge":"challenge"}`), nil
		case "/oauth/token":
			body, errRead := io.ReadAll(req.Body)
			if errRead != nil {
				return nil, errRead
			}
			form, errParse := url.ParseQuery(string(body))
			if errParse != nil {
				return nil, errParse
			}
			if form.Get("code") != "auth-code" || form.Get("code_verifier") != "verifier" || form.Get("redirect_uri") != codex.DeviceTokenExchangeRedirectURI {
				return nil, errors.New("unexpected Codex exchange form")
			}
			return managementDeviceResponse(http.StatusOK, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`), nil
		default:
			return nil, errors.New("unexpected Codex URL: " + req.URL.String())
		}
	})}
	ctx, cancel := h.oauthSessionContext(context.Background(), state, codex.DeviceFlowTimeout)
	defer cancel()
	if errWait := h.waitForCodexDeviceAuthorization(ctx, h.oauthHTTPClient(), state, &codex.DeviceFlow{
		DeviceAuthID: "private-device-id",
		UserCode:     "ABCD-EFGH",
		PollInterval: time.Second,
	}); errWait != nil {
		t.Fatal(errWait)
	}
	current, errCurrent := repo.GetOAuthSession(context.Background(), state)
	if errCurrent != nil || current == nil || current.Status != "complete" {
		t.Fatalf("completed device session = %+v, error=%v", current, errCurrent)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 1 || auths[0].Provider != "codex" || auths[0].Metadata["access_token"] != "synthetic-access" {
		t.Fatalf("persisted device credentials = %+v, error=%v", auths, errAuths)
	}
	engine := gin.New()
	engine.GET("/oauth/status", h.GetAuthStatus)
	statusResponse := httptest.NewRecorder()
	engine.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/oauth/status?state="+url.QueryEscape(state), nil))
	var statusBody map[string]any
	if errDecode := json.Unmarshal(statusResponse.Body.Bytes(), &statusBody); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(statusBody) != 1 || statusBody["status"] != "ok" {
		t.Fatalf("completed OAuth status = %v", statusBody)
	}
}

func TestCodexDeviceWorkerRejectsIncompleteProviderToken(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/accounts/deviceauth/usercode" {
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH"}`), nil
		}
		if req.URL.Path == "/api/accounts/deviceauth/token" {
			return managementDeviceResponse(http.StatusOK, `{"authorization_code":"incomplete-code"}`), nil
		}
		return nil, errors.New("unexpected Codex URL: " + req.URL.String())
	})}
	h.codexDeviceWorker = func(work func()) { work() }
	engine := gin.New()
	engine.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("device auth-url status = %d, body=%s", response.Code, response.Body.String())
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "error" || session.Error != "Authentication failed" {
		t.Fatalf("incomplete provider token session = %+v", session)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 0 {
		t.Fatalf("credentials after incomplete provider token = %d, error=%v", len(auths), errAuths)
	}
}

func TestSecondHomeNodeCancellationPreventsCodexCredentialSave(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	state, errState := generateOAuthState("cdx")
	if errState != nil {
		t.Fatal(errState)
	}
	session, errSession := cluster.NewOAuthSessionRecord("codex", state, map[string]any{"flow": "device"}, time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(context.Background(), session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	sessionCtx, cancelSession := h.oauthSessionContext(context.Background(), state, codex.DeviceFlowTimeout)
	defer cancelSession()
	if errContext := sessionCtx.Err(); errContext != nil {
		t.Fatalf("new device session context = %v", errContext)
	}
	secondNode := cluster.NewRepository(db)
	if cancelled, errCancel := secondNode.CancelOAuthSession(context.Background(), state); errCancel != nil || !cancelled {
		t.Fatalf("second-node cancellation = %v, error=%v", cancelled, errCancel)
	}
	select {
	case <-sessionCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("device session context did not observe second-node cancellation")
	}

	storeCtx := context.WithValue(context.Background(), oauthSessionStateKey{}, state)
	errStore := h.storeOAuthMetadataWithContext(storeCtx, map[string]any{
		"type":          "codex",
		"access_token":  "synthetic-access",
		"refresh_token": "synthetic-refresh",
	}, "codex-device-fixture.json")
	if !errors.Is(errStore, cluster.ErrOAuthSessionNotPending) {
		t.Fatalf("late device credential save error = %v, want session not pending", errStore)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 0 {
		t.Fatalf("credentials after second-node cancellation = %d, error=%v", len(auths), errAuths)
	}
}
