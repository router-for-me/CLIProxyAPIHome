package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestCodexDeviceSuccessfulFlushStartsWorkerAfterRequestCancellation(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	counts := make(map[string]int)
	client := codexDeviceHandoffTestClient(counts, "issuance")
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = client
	h.codexDeviceWorker = func(work func()) { work() }

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	writer := &codexDeviceHTTPResponseWriter{onFlush: func() error {
		cancelRequest()
		return nil
	}}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil).WithContext(requestCtx)
	h.StartOAuthV8(ctx)

	if errRequest := requestCtx.Err(); !errors.Is(errRequest, context.Canceled) {
		t.Fatalf("request context after successful flush = %v, want canceled", errRequest)
	}
	if writer.status != http.StatusOK {
		t.Fatalf("device auth response status = %d, want %d", writer.status, http.StatusOK)
	}
	if strings.Contains(writer.body.String(), "device_auth_id") {
		t.Fatalf("device auth response exposes private ID: %s", writer.body.String())
	}
	if counts["issuance:/api/accounts/deviceauth/token"] != 1 {
		t.Fatalf("device polls after successful flush = %d, want 1", counts["issuance:/api/accounts/deviceauth/token"])
	}
	if counts["issuance:/oauth/token"] != 1 {
		t.Fatalf("token exchanges after successful flush = %d, want 1", counts["issuance:/oauth/token"])
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "complete" {
		t.Fatalf("session after successful handoff = %+v", session)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 1 || auths[0].Metadata["access_token"] != "synthetic-access" {
		t.Fatalf("credentials after successful handoff = %+v, error=%v", auths, errAuths)
	}
}

func TestCodexDeviceUsesIssuanceClientForPollAndExchange(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	counts := make(map[string]int)
	issuanceClient := codexDeviceHandoffTestClient(counts, "issuance")
	replacementClient := codexDeviceHandoffTestClient(counts, "replacement")
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	h.oauthHTTPClientOverride = issuanceClient
	h.codexDeviceWorker = func(work func()) { work() }
	writer := &codexDeviceHTTPResponseWriter{onFlush: func() error {
		h.oauthHTTPClientOverride = replacementClient
		return nil
	}}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex&flow=device", nil)
	h.StartOAuthV8(ctx)

	for _, path := range []string{"/api/accounts/deviceauth/usercode", "/api/accounts/deviceauth/token", "/oauth/token"} {
		if got := counts["issuance:"+path]; got != 1 {
			t.Errorf("issuance client requests to %s = %d, want 1", path, got)
		}
		if got := counts["replacement:"+path]; got != 0 {
			t.Errorf("replacement client requests to %s = %d, want 0", path, got)
		}
	}
	session := onlyCodexOAuthSession(t, db)
	if session.Status != "complete" {
		t.Fatalf("session after client switch = %+v", session)
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil || len(auths) != 1 || auths[0].Metadata["access_token"] != "synthetic-access" {
		t.Fatalf("credentials after client switch = %+v, error=%v", auths, errAuths)
	}
}

func codexDeviceHandoffTestClient(counts map[string]int, name string) *http.Client {
	return &http.Client{Transport: codexDeviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		counts[name+":"+req.URL.Path]++
		switch req.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			return managementDeviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","user_code":"ABCD-EFGH","interval":1}`), nil
		case "/api/accounts/deviceauth/token":
			return managementDeviceResponse(http.StatusOK, `{"authorization_code":"auth-code","code_verifier":"verifier","code_challenge":"challenge"}`), nil
		case "/oauth/token":
			return managementDeviceResponse(http.StatusOK, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`), nil
		default:
			return nil, errors.New("unexpected Codex URL: " + req.URL.String())
		}
	})}
}
