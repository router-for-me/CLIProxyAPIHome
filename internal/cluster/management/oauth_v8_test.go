package management

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOAuthV8DatabaseSessions(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	engine.GET("/v8/management/oauth/callback", h.PostOAuthCallbackV8)
	engine.GET("/v0/management/get-auth-status", h.GetAuthStatus)
	engine.DELETE("/v8/management/oauth/session", h.CancelAuthSession)
	request := func(method, path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != status {
			t.Fatalf("%s %s: %d want %d: %s", method, path, rec.Code, status, rec.Body.String())
		}
		return rec
	}
	request("GET", "/v8/management/oauth/auth-url", 400)
	request("GET", "/v8/management/oauth/auth-url?provider=unknown", 404)
	request("GET", "/v8/management/oauth/callback", 400)
	response := request("GET", "/v8/management/oauth/auth-url?provider=%20CODEX%20", 200)
	var login struct{ State, URL string }
	if errDecode := json.Unmarshal(response.Body.Bytes(), &login); errDecode != nil {
		t.Fatal(errDecode)
	}
	if login.State == "" || login.URL == "" {
		t.Fatal("missing OAuth login")
	}
	loginURL, errURL := url.Parse(login.URL)
	if errURL != nil || loginURL.Host != "auth.openai.com" || loginURL.Path != "/oauth/authorize" {
		t.Fatalf("default Codex browser flow URL = %q, error=%v", login.URL, errURL)
	}
	session, errSession := repo.GetOAuthSession(context.Background(), login.State)
	if errSession != nil || session == nil || session.Provider != "codex" || session.Status != "" {
		t.Fatalf("login was not persisted: %v", errSession)
	}
	data, errData := cluster.OAuthSessionData(session)
	if errData != nil || data["flow"] == "device" {
		t.Fatalf("default Codex flow session data = %v, error=%v", data, errData)
	}
	request("GET", "/v0/management/get-auth-status?state="+login.State, 200)
	request("DELETE", "/v8/management/oauth/session?state="+login.State, 200)
	request("GET", "/v8/management/oauth/callback?state="+login.State+"&code=unused", 409)
	session, errSession = repo.GetOAuthSession(context.Background(), login.State)
	if errSession != nil || session == nil || session.Status != "error" {
		t.Fatalf("cancel was not persisted: %v", errSession)
	}
}
