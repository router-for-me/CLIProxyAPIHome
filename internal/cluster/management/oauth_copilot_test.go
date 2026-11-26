package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestRecordOAuthSessionErrorRetriesTransientFailures(t *testing.T) {
	oldSleep := oauthSessionErrorSleep
	oldDelays := oauthSessionErrorRetryDelays
	t.Cleanup(func() {
		oauthSessionErrorSleep = oldSleep
		oauthSessionErrorRetryDelays = oldDelays
	})

	var sleeps []time.Duration
	oauthSessionErrorRetryDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	oauthSessionErrorSleep = func(d time.Duration) { sleeps = append(sleeps, d) }

	attempts := 0
	errTransient := errors.New("temporary db failure")
	err := recordOAuthSessionErrorWith(func(ctx context.Context, state, message string) error {
		attempts++
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			t.Fatal("expected bounded timeout context")
		}
		if state != "cpl-state" || message != "Authentication failed" {
			t.Fatalf("unexpected args: state=%q message=%q", state, message)
		}
		if attempts < 3 {
			return errTransient
		}
		return nil
	}, "cpl-state", "Authentication failed")
	if err != nil {
		t.Fatalf("expected retry success, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if !reflect.DeepEqual(sleeps, oauthSessionErrorRetryDelays) {
		t.Fatalf("sleeps = %v, want %v", sleeps, oauthSessionErrorRetryDelays)
	}

	sleeps = nil
	attempts = 0
	errFinal := errors.New("db unavailable")
	err = recordOAuthSessionErrorWith(func(ctx context.Context, _, _ string) error {
		attempts++
		return errFinal
	}, "cpl-state", "Failed to save authentication tokens")
	if !errors.Is(err, errFinal) {
		t.Fatalf("error = %v, want %v", err, errFinal)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if !reflect.DeepEqual(sleeps, oauthSessionErrorRetryDelays) {
		t.Fatalf("sleeps = %v, want %v", sleeps, oauthSessionErrorRetryDelays)
	}
}

func TestCopilotOAuthV8MarksSessionErrorOnLoginFailure(t *testing.T) {
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
		switch req.URL.String() {
		case "https://github.com/login/device/code":
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"device_code":"device-fixture","user_code":"TEST-KEY","verification_uri":"https://github.com/login/device","expires_in":600,"interval":1}`)),
			}, nil
		case "https://github.com/login/oauth/access_token":
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"error":"access_denied"}`)),
			}, nil
		default:
			return nil, fmt.Errorf("unexpected OAuth request: %s", req.URL.String())
		}
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=copilot", nil))
	var login struct{ State string }
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &login); errDecode != nil || rec.Code != http.StatusOK || login.State == "" {
		t.Fatalf("unexpected auth-url response: code=%d body=%s", rec.Code, rec.Body.String())
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-timeout.C:
			t.Fatal("Copilot session did not transition to error")
		case <-ticker.C:
			session, errSession := repo.GetOAuthSession(t.Context(), login.State)
			if errSession != nil || session == nil {
				t.Fatalf("GetOAuthSession failed: %v", errSession)
			}
			if session.Status == "error" {
				rec = httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+login.State, nil))
				var status struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				}
				if errDecode := json.Unmarshal(rec.Body.Bytes(), &status); errDecode != nil || status.Status != "error" || status.Error != "Authentication failed" {
					t.Fatalf("unexpected status response: %s", rec.Body.String())
				}
				return
			}
		}
	}
}
