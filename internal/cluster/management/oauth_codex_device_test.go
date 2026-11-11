package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestStartCodexDeviceFlowReturnsPublicMetadataAndStoresPrivateSession(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})
	client := &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH","interval":1}`), nil
		case "/api/accounts/deviceauth/token":
			close(pollStarted)
			<-req.Context().Done()
			close(pollCancelled)
			return nil, req.Context().Err()
		default:
			return nil, errors.New("unexpected Codex URL: " + req.URL.String())
		}
	})}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = client
	engine := gin.New()
	engine.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("device auth-url status = %d, body=%s", response.Code, response.Body.String())
	}

	var body map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(body) != 6 || body["status"] != "ok" || body["url"] != codex.DeviceVerificationURL || body["flow"] != "device" || body["user_code"] != "ABCD-EFGH" {
		t.Fatalf("device response = %v", body)
	}
	if _, exists := body["device_auth_id"]; exists || strings.Contains(response.Body.String(), "private-device-id") {
		t.Fatalf("private device auth id leaked in response: %s", response.Body.String())
	}
	state, okState := body["state"].(string)
	if !okState || validateOAuthState(state) != nil {
		t.Fatalf("response state = %v", body["state"])
	}
	expiresIn, okExpiresIn := body["expires_in"].(float64)
	if !okExpiresIn || expiresIn <= 0 || expiresIn > codex.DeviceFlowTimeout.Seconds() {
		t.Fatalf("response expires_in = %v", body["expires_in"])
	}

	session, errSession := repo.GetOAuthSession(context.Background(), state)
	if errSession != nil || session == nil || session.Provider != "codex" || session.Status != "" {
		t.Fatalf("device session = %+v, error=%v", session, errSession)
	}
	data, errData := cluster.OAuthSessionData(session)
	if errData != nil || data["flow"] != "device" || data["device_auth_id"] != "private-device-id" || data["user_code"] != "ABCD-EFGH" {
		t.Fatalf("stored device session data = %v, error=%v", data, errData)
	}
	select {
	case <-pollStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("device worker did not poll after the response was written")
	}

	secondNode := cluster.NewRepository(db)
	cancelled, errCancel := secondNode.CancelOAuthSession(context.Background(), state)
	if errCancel != nil || !cancelled {
		t.Fatalf("second-node cancellation = %v, error=%v", cancelled, errCancel)
	}
	select {
	case <-pollCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("device worker did not stop after second-node cancellation")
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 0 {
		t.Fatalf("credentials after cancelled device flow = %d, error=%v", len(auths), errAuths)
	}
}

func TestCodexDeviceStartupFailureMarksSessionError(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(*http.Request) (*http.Response, error) {
		return managementDeviceResponse(http.StatusOK, `{"user_code":"ABCD-EFGH"}`), nil
	})}
	engine := gin.New()
	engine.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("device startup status = %d, want %d: %s", response.Code, http.StatusBadGateway, response.Body.String())
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "error" || session.Error != "Authentication failed" {
		t.Fatalf("startup failure session = %+v", session)
	}
}

func TestCodexDeviceWriteFailureCancelsSessionWithoutPolling(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	polls := 0
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/accounts/deviceauth/usercode" {
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH"}`), nil
		}
		polls++
		return managementDeviceResponse(http.StatusOK, `{"authorization_code":"unused"}`), nil
	})}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil)
	ctx.Writer = failingCodexDeviceWriter{ResponseWriter: ctx.Writer, err: errors.New("response write failed")}
	h.StartOAuthV8(ctx)
	if polls != 0 {
		t.Fatalf("device polls after failed response write = %d, want 0", polls)
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "error" || session.Error != "Authentication cancelled" {
		t.Fatalf("undelivered response session = %+v", session)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 0 {
		t.Fatalf("credentials after failed response write = %d, error=%v", len(auths), errAuths)
	}
}

func TestCodexDeviceFlushFailureCancelsSessionWithoutPolling(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	polls := 0
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/accounts/deviceauth/usercode" {
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH"}`), nil
		}
		polls++
		return managementDeviceResponse(http.StatusOK, `{}`), nil
	})}
	h.codexDeviceWorker = func(work func()) { work() }
	responseWriter := &codexDeviceHTTPResponseWriter{flushErr: errors.New("transport flush failed")}
	ctx, _ := gin.CreateTestContext(responseWriter)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil)
	h.StartOAuthV8(ctx)
	if polls != 0 {
		t.Fatalf("device polls after failed transport flush = %d, want 0", polls)
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "error" || session.Error != "Authentication cancelled" {
		t.Fatalf("transport-flush failure session = %+v", session)
	}
}

func TestCodexDeviceStartupDisconnectCancelsSession(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	requestStarted := make(chan struct{})
	polls := 0
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/accounts/deviceauth/usercode" {
			close(requestStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		polls++
		return managementDeviceResponse(http.StatusOK, `{}`), nil
	})}
	engine := gin.New()
	engine.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil).WithContext(requestCtx)
	handlerDone := make(chan struct{})
	go func() {
		engine.ServeHTTP(httptest.NewRecorder(), request)
		close(handlerDone)
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		cancelRequest()
		t.Fatal("device code request did not start")
	}
	cancelRequest()
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("device startup did not stop after disconnect")
	}
	if polls != 0 {
		t.Fatalf("device polls after startup disconnect = %d, want 0", polls)
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "error" || session.Error != "Authentication cancelled" {
		t.Fatalf("disconnected device session = %+v", session)
	}
}

func TestCodexDeviceSessionMergeFailureDoesNotLeavePendingSession(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	polls := 0
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/accounts/deviceauth/usercode" {
			session := onlyCodexOAuthSession(t, db)
			if cancelled, errCancel := cluster.NewRepository(db).CancelOAuthSession(context.Background(), session.State); errCancel != nil || !cancelled {
				return nil, fmt.Errorf("fixture cancellation failed: %v", errCancel)
			}
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH"}`), nil
		}
		polls++
		return managementDeviceResponse(http.StatusOK, `{}`), nil
	})}
	engine := gin.New()
	engine.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("device session merge status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if polls != 0 {
		t.Fatalf("device polls after session merge failure = %d, want 0", polls)
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "error" || session.Error != "Authentication cancelled" {
		t.Fatalf("session after merge failure = %+v", session)
	}
}

func TestCodexDeviceFastCompletionWaitsForResponseHandoff(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeCompleted := make(chan struct{})
	pollStarted := make(chan struct{})
	var deviceDeadline time.Time
	var pollDeadline time.Time
	var exchangeDeadline time.Time
	client := &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			deviceDeadline, _ = req.Context().Deadline()
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH"}`), nil
		case "/api/accounts/deviceauth/token":
			select {
			case <-writeCompleted:
			default:
				return nil, errors.New("device poll began before the response write completed")
			}
			pollDeadline, _ = req.Context().Deadline()
			close(pollStarted)
			return managementDeviceResponse(http.StatusOK, `{"authorization_code":"auth-code","code_verifier":"verifier","code_challenge":"challenge"}`), nil
		case "/oauth/token":
			exchangeDeadline, _ = req.Context().Deadline()
			return managementDeviceResponse(http.StatusOK, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`), nil
		default:
			return nil, errors.New("unexpected Codex URL: " + req.URL.String())
		}
	})}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = client
	h.codexDeviceWorker = func(work func()) { work() }
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	responseWriter := ctx.Writer
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil)
	ctx.Writer = gatedCodexDeviceWriter{
		ResponseWriter: responseWriter,
		started:        writeStarted,
		release:        releaseWrite,
		completed:      writeCompleted,
	}
	handlerDone := make(chan struct{})
	go func() {
		h.StartOAuthV8(ctx)
		close(handlerDone)
	}()
	select {
	case <-writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("device auth response did not reach the response writer")
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "" {
		t.Fatalf("session changed before response handoff: %+v", session)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 0 {
		t.Fatalf("credentials before response handoff = %d, error=%v", len(auths), errAuths)
	}
	select {
	case <-pollStarted:
		t.Fatal("device worker polled before response handoff")
	default:
	}
	close(releaseWrite)
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("fast device authorization did not finish")
	}
	select {
	case <-pollStarted:
	default:
		t.Fatal("device worker did not poll after response handoff")
	}
	current, errCurrent := repo.GetOAuthSession(context.Background(), session.State)
	if errCurrent != nil || current == nil || current.Status != "complete" {
		t.Fatalf("session after fast completion = %+v, error=%v", current, errCurrent)
	}
	auths, errAuths = repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 1 {
		t.Fatalf("credentials after fast completion = %d, error=%v", len(auths), errAuths)
	}
	if deviceDeadline.IsZero() || pollDeadline.IsZero() || exchangeDeadline.IsZero() || pollDeadline.After(deviceDeadline) || exchangeDeadline.After(deviceDeadline) {
		t.Fatalf("device deadlines were reset: issuance=%s poll=%s exchange=%s", deviceDeadline, pollDeadline, exchangeDeadline)
	}
	if remaining := time.Until(deviceDeadline); remaining <= 0 || remaining > codex.DeviceFlowTimeout {
		t.Fatalf("shared device deadline remaining = %s", remaining)
	}
}
