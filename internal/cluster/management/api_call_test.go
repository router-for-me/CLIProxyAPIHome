package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestAPICallResolvesDevinTokenInBody(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	auth := &coreauth.Auth{ID: "devin-body", Index: "devin-body", Provider: "devin", Metadata: map[string]any{"type": "devin", "auth_kind": "oauth", "session_token": "devin-session-token$test"}}
	if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("seed Devin credential: %v", errUpsert)
	}
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil || string(body) != `{"metadata":{"apiKey":"devin-session-token$test"}}` {
			t.Errorf("Devin token was not substituted into the request body")
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	engine := gin.New()
	engine.POST("/requests/api-call", handler.APICall)
	for _, selector := range []string{"devin-body", "missing"} {
		payload, errMarshal := json.Marshal(map[string]any{
			"authIndex": selector, "method": "POST", "url": upstream.URL,
			"header": map[string]string{"Content-Type": "application/json"},
			"data":   `{"metadata":{"apiKey":"$TOKEN$"}}`,
		})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		req := httptest.NewRequest(http.MethodPost, "/requests/api-call", strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		wantStatus := http.StatusOK
		if selector == "missing" {
			wantStatus = http.StatusBadRequest
		}
		if response.Code != wantStatus {
			t.Fatalf("API call status=%d, want %d", response.Code, wantStatus)
		}
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("unresolved token reached upstream: requests=%d", upstreamCalls.Load())
	}
}

func TestAPICallBodyTokenEscapesJSON(t *testing.T) {
	const token = "session\"\\\n\tvalue"
	data := apiCallBodyWithToken(`{"metadata":{"apiKey":"$TOKEN$"}}`, token)
	var parsed struct {
		Metadata struct {
			APIKey string `json:"apiKey"`
		} `json:"metadata"`
	}
	if errDecode := json.Unmarshal([]byte(data), &parsed); errDecode != nil || parsed.Metadata.APIKey != token {
		t.Fatal("body token did not preserve JSON validity and token contents")
	}
	if apiCallBodyWithToken("plain:$TOKEN$", token) != "plain:"+token {
		t.Fatal("non-JSON request body was unexpectedly escaped")
	}
}

func TestAPICallTokenBodyRedirectPolicy(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	auth := &coreauth.Auth{ID: "devin-redirect", Index: "devin-redirect", Provider: "devin", Metadata: map[string]any{"type": "devin", "session_token": "synthetic-session"}}
	if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	engine := gin.New()
	engine.POST("/requests/api-call", handler.APICall)
	for _, test := range []struct {
		name                  string
		status                int
		sameOrigin, withToken bool
		wantStatus            int
	}{
		{"same origin 307", http.StatusTemporaryRedirect, true, true, http.StatusOK},
		{"same origin 308", http.StatusPermanentRedirect, true, true, http.StatusOK},
		{"cross origin 307", http.StatusTemporaryRedirect, false, true, http.StatusBadGateway},
		{"cross origin 308", http.StatusPermanentRedirect, false, true, http.StatusBadGateway},
		{"public redirect", http.StatusTemporaryRedirect, false, false, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			receive := func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil || test.withToken && !strings.Contains(string(body), "synthetic-session") {
					t.Error("allowed redirect lost the request body")
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}
			destination := httptest.NewServer(http.HandlerFunc(receive))
			defer destination.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/final" {
					receive(w, r)
					return
				}
				target := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)
				if test.sameOrigin {
					target = "/final"
				}
				http.Redirect(w, r, target, test.status)
			}))
			defer origin.Close()
			data := "public"
			if test.withToken {
				data = `{"apiKey":"$TOKEN$"}`
			}
			payload, errMarshal := json.Marshal(map[string]any{"auth_index": auth.ID, "method": "POST", "url": origin.URL, "data": data})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			request := httptest.NewRequest(http.MethodPost, "/requests/api-call", strings.NewReader(string(payload)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			wantCalls := int32(0)
			if test.wantStatus == http.StatusOK {
				wantCalls = 1
			}
			if response.Code != test.wantStatus || calls.Load() != wantCalls {
				t.Fatalf("redirect status=%d calls=%d, want status=%d calls=%d", response.Code, calls.Load(), test.wantStatus, wantCalls)
			}
		})
	}
}

func TestAPICallAuthMatchesOAuthFileName(t *testing.T) {
	t.Parallel()

	auth := &coreauth.Auth{
		ID:       "7f2f0f1d-40d1-4f9a-9045-4b83412c8188",
		Index:    "7f2f0f1d-40d1-4f9a-9045-4b83412c8188",
		Provider: "codex",
		Label:    "codex-user",
		Metadata: map[string]any{
			"type":     "codex",
			"filename": "codex-user.json",
		},
	}

	for _, selector := range []string{
		auth.ID,
		auth.Index,
		"codex-user.json",
		"codex-user",
	} {
		selector := selector
		t.Run(selector, func(t *testing.T) {
			t.Parallel()

			if !apiCallAuthMatches(auth.Clone(), selector) {
				t.Fatalf("apiCallAuthMatches returned false for selector %q", selector)
			}
		})
	}
}

func TestAPICallTransportUsesOAuthMetadataProxyURL(t *testing.T) {
	t.Parallel()

	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"type":      "codex",
			"proxy_url": "http://codex-proxy.example.com:8080",
		},
	}
	transport := (&Handler{}).apiCallTransport(auth)
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}
	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://codex-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://codex-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTokenValueUsesDevinSessionMetadata(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "devin",
		Metadata: map[string]any{
			"type":          "devin",
			"auth_kind":     "oauth",
			"api_key":       "devin-session-token$secret",
			"session_token": "devin-session-token$secret",
		},
	}
	if got := apiCallTokenValueForAuth(auth); got != "devin-session-token$secret" {
		t.Fatalf("apiCallTokenValueForAuth() = %q, want Devin session token", got)
	}

	accessFirst := &coreauth.Auth{
		Provider: "meta",
		Metadata: map[string]any{
			"access_token": "LLM|preferred",
			"api_key":      "LLM|secondary",
		},
	}
	if got := apiCallTokenValueForAuth(accessFirst); got != "LLM|preferred" {
		t.Fatalf("apiCallTokenValueForAuth() = %q, want access_token first", got)
	}
}
