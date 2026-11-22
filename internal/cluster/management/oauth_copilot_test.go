package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type copilotDeviceTransport func(*http.Request) (*http.Response, error)

func (f copilotDeviceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCopilotOAuthV8CompletesDatabaseSession(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/v8/management/oauth/status", h.GetAuthStatus)

	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	http.DefaultTransport = copilotDeviceTransport(func(req *http.Request) (*http.Response, error) {
		var response string
		switch req.URL.String() {
		case "https://github.com/login/device/code":
			response = `{"device_code":"device-fixture","user_code":"TEST-KEY","verification_uri":"https://github.com/login/device","expires_in":600,"interval":1}`
		case "https://github.com/login/oauth/access_token":
			response = `{"access_token":"github-fixture"}`
		case "https://api.github.com/user":
			response = `{"id":123,"login":"example"}`
		case "https://api.github.com/copilot_internal/v2/token":
			if req.Header.Get("Authorization") != "Bearer github-fixture" {
				t.Error("token exchange did not use the GitHub credential")
			}
			response = fmt.Sprintf(`{"token":"short-fixture","expires_at":%d,"endpoints":{"api":"https://api.business.githubcopilot.com/"}}`, time.Now().Add(time.Hour).Unix())
		default:
			t.Error("unexpected Copilot OAuth endpoint")
			return nil, fmt.Errorf("unexpected OAuth request")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=copilot", nil))
	var login struct {
		State    string
		URL      string
		UserCode string `json:"user_code"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &login); errDecode != nil ||
		rec.Code != http.StatusOK || login.State == "" || login.URL != "https://github.com/login/device" || login.UserCode == "" {
		t.Fatal("device flow did not return a verification URL, code and state")
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
waitForCompletion:
	for {
		select {
		case <-timeout.C:
			t.Fatal("Copilot authorization did not complete")
		case <-ticker.C:
			session, errSession := repo.GetOAuthSession(t.Context(), login.State)
			if errSession != nil || session == nil || session.Provider != "copilot" || session.Status == "error" {
				t.Fatalf("Copilot session failed: %v", errSession)
			}
			if session.Status == "complete" {
				break waitForCompletion
			}
		}
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+login.State, nil))
	var status struct{ Status string }
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &status); errDecode != nil || rec.Code != http.StatusOK || status.Status != "ok" {
		t.Fatal("OAuth status did not report completion")
	}
	auths, err := repo.ListAuths(t.Context())
	if err != nil || len(auths) != 1 || auths[0].Provider != "copilot" {
		t.Fatal("missing Copilot record after transactional completion")
	}
	_, hasExpiry := auths[0].ExpirationTime()
	meta := auths[0].Metadata
	if meta["refresh_token"] != "github-fixture" || meta["access_token"] != "short-fixture" ||
		meta["base_url"] != "https://api.business.githubcopilot.com" || !hasExpiry {
		t.Fatal("Copilot credential metadata is incomplete")
	}
}
