package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOAuthStatusV8ReturnsPersistedCredentialID(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := cluster.NewRepository(db)
	session, errSession := cluster.NewOAuthSessionRecord("kimi-ai", "saved-login", map[string]any{"code_verifier": "private-verifier"}, time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(ctx, session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	storeCtx := context.WithValue(ctx, oauthSessionStateKey{}, session.State)
	if errStore := h.storeOAuthMetadataWithContext(storeCtx, map[string]any{
		"type": "kimi-ai", "access_token": "private-access", "refresh_token": "private-refresh",
	}, "fixture.json"); errStore != nil {
		t.Fatal(errStore)
	}
	auths, errAuths := repo.ListAuths(ctx)
	if errAuths != nil || len(auths) != 1 {
		t.Fatalf("saved credentials = %d, error = %v", len(auths), errAuths)
	}
	// Read through a new handler and repository, as another Home node would.
	reader := NewHandler(cluster.NewRepository(db), nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/v8/management/oauth/status", reader.GetAuthStatusV8)
	engine.GET("/v0/management/get-auth-status", reader.GetAuthStatus)
	for range 2 {
		body := oauthStatusV8Request(t, engine, "/v8/management/oauth/status?state="+session.State)
		want := map[string]any{"status": "ok", "auth_id": auths[0].ID, "auth_ids": []any{auths[0].ID}}
		if !reflect.DeepEqual(body, want) {
			t.Fatalf("status = %#v, want %#v", body, want)
		}
	}
	legacy := oauthStatusV8Request(t, engine, "/v0/management/get-auth-status?state="+session.State)
	if !reflect.DeepEqual(legacy, map[string]any{"status": "ok"}) {
		t.Fatalf("legacy status changed: %#v", legacy)
	}
	completed, errCompleted := repo.GetOAuthSession(ctx, session.State)
	if errCompleted != nil || completed == nil {
		t.Fatalf("completed session missing: %v", errCompleted)
	}
	if strings.Contains(string(completed.Data), "private-") {
		t.Fatal("completed session retained OAuth secrets")
	}
	group, errGroup := repo.CreateChannelGroup(ctx, "fixture", false)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	if _, errDetail := repo.CreateChannelGroupDetail(ctx, group.ID, auths[0].ID); errDetail != nil {
		t.Fatalf("returned ID cannot be assigned to a channel group: %v", errDetail)
	}
	if errExpire := db.Model(&cluster.OAuthSessionRecord{}).Where("state = ?", session.State).
		Update("expires_at", time.Now().Add(-time.Minute)).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	expired := oauthStatusV8Request(t, engine, "/v8/management/oauth/status?state="+session.State)
	if expired["status"] != "error" || expired["auth_id"] != nil || expired["auth_ids"] != nil {
		t.Fatalf("expired result = %#v", expired)
	}
}

func TestOAuthStatusV8MultipleCredentialsAndNonSuccess(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	ctx := context.Background()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/oauth/status", h.GetAuthStatusV8)
	for _, state := range []string{"multiple", "pending", "cancelled", "legacy"} {
		session, errSession := cluster.NewOAuthSessionRecord("codex", state, nil, time.Now())
		if errSession != nil {
			t.Fatal(errSession)
		}
		if errUpsert := repo.UpsertOAuthSession(ctx, session); errUpsert != nil {
			t.Fatal(errUpsert)
		}
	}
	first := &coreauth.Auth{ID: "fixture-first", Index: "fixture-first", Provider: "codex"}
	second := &coreauth.Auth{ID: "fixture-second", Index: "fixture-second", Provider: "codex"}
	if errComplete := repo.CompleteOAuthSessionWithAuths(ctx, "multiple", []*coreauth.Auth{first, second, first}); errComplete != nil {
		t.Fatal(errComplete)
	}
	multiple := oauthStatusV8Request(t, engine, "/oauth/status?state=multiple")
	if multiple["status"] != "ok" || multiple["auth_id"] != nil || !reflect.DeepEqual(multiple["auth_ids"], []any{first.ID, second.ID}) {
		t.Fatalf("multiple credential result = %#v", multiple)
	}
	if _, errCancel := repo.CancelOAuthSession(ctx, "cancelled"); errCancel != nil {
		t.Fatal(errCancel)
	}
	if errComplete := repo.CompleteOAuthSession(ctx, "legacy"); errComplete != nil {
		t.Fatal(errComplete)
	}
	for state, wantStatus := range map[string]string{"pending": "wait", "cancelled": "error", "unknown": "error", "legacy": "ok", "": "ok"} {
		body := oauthStatusV8Request(t, engine, "/oauth/status?state="+state)
		if body["status"] != wantStatus || body["auth_id"] != nil || body["auth_ids"] != nil {
			t.Fatalf("state %q result = %#v", state, body)
		}
	}
}

func oauthStatusV8Request(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status HTTP %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	return body
}

func TestOAuthStatusV8FirstPluginSuccessPollReturnsIDs(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	session, errSession := cluster.NewOAuthSessionRecord("fixture-plugin", "plugin-login", pluginOAuthSessionData(nil), time.Now())
	if errSession != nil {
		t.Fatal(errSession)
	}
	if errUpsert := repo.UpsertOAuthSession(t.Context(), session); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	plugin := &successfulOAuthStatusPlugin{auths: []*coreauth.Auth{
		{ID: "plugin-first", Index: "plugin-first", Provider: "fixture-plugin"},
		{ID: "plugin-second", Index: "plugin-second", Provider: "fixture-plugin"},
	}}
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.pluginOAuthStatus = plugin
	engine := gin.New()
	engine.GET("/oauth/status", h.GetAuthStatusV8)
	for range 2 {
		body := oauthStatusV8Request(t, engine, "/oauth/status?state="+session.State)
		want := map[string]any{"status": "ok", "auth_ids": []any{"plugin-first", "plugin-second"}}
		if !reflect.DeepEqual(body, want) {
			t.Fatalf("plugin completion response = %#v, want %#v", body, want)
		}
	}
	if plugin.polls != 1 {
		t.Fatalf("plugin polls = %d, want one poll followed by the persisted result", plugin.polls)
	}
}

type successfulOAuthStatusPlugin struct {
	auths []*coreauth.Auth
	polls int
}

func (p *successfulOAuthStatusPlugin) HasPluginAuthProvider(provider string) bool {
	return provider == "fixture-plugin"
}

func (p *successfulOAuthStatusPlugin) PollPluginLogin(context.Context, string, string, map[string]any) (pluginapi.AuthLoginPollResponse, bool, error) {
	p.polls++
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess}, true, nil
}

func (p *successfulOAuthStatusPlugin) PluginLoginAuths(pluginapi.AuthLoginPollResponse) ([]*coreauth.Auth, error) {
	return p.auths, nil
}
