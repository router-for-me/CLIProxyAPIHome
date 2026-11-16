package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNormalizePluginOAuthProvider(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		valid bool
	}{
		{input: "gemini-cli", want: "gemini-cli", valid: true},
		{input: " Gemini ", want: "gemini", valid: true},
		{input: "gemini_cli"},
		{input: "gemini/cli"},
		{input: ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, errNormalize := normalizePluginOAuthProvider(tc.input)
			if (errNormalize == nil) != tc.valid || got != tc.want {
				t.Fatalf("normalizePluginOAuthProvider(%q) = %q, %v", tc.input, got, errNormalize)
			}
		})
	}
}

func TestPluginOAuthPollMetadataOmitsSessionSource(t *testing.T) {
	data := pluginOAuthSessionData(map[string]any{
		"poll_token": "token-1",
	})
	data["callback_code"] = "code-1"

	if !oauthSessionIsPlugin(data) {
		t.Fatal("oauthSessionIsPlugin() = false, want true")
	}
	pollMetadata := pluginOAuthPollMetadata(data)
	if _, exists := pollMetadata["source"]; exists {
		t.Fatalf("poll metadata contains source: %#v", pollMetadata)
	}
	if pollMetadata["poll_token"] != "token-1" || pollMetadata["callback_code"] != "code-1" {
		t.Fatalf("poll metadata = %#v, want plugin metadata and callback code", pollMetadata)
	}
}

func TestPluginManagementCallbackURLUsesForwardedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "http://internal.example/v8/management/oauth/auth-url?provider=gemini-cli", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "home.example.test")
	c.Request = req

	handler := NewHandler(nil, nil, "127.0.0.1", 8317)
	got, errURL := handler.pluginManagementCallbackURL(c)
	if errURL != nil {
		t.Fatalf("pluginManagementCallbackURL() error = %v", errURL)
	}
	if want := "https://home.example.test/v8/management/oauth/callback"; got != want {
		t.Fatalf("pluginManagementCallbackURL() = %q, want %q", got, want)
	}
}
