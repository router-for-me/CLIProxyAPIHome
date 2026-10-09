package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
)

func TestRefreshCodexSyncsPlanType(t *testing.T) {
	// Keep this test serial because Codex uses the default HTTP transport.
	for _, tc := range []struct {
		name          string
		claims        string
		oldPlan       string
		wantPlan      string
		nilAttributes bool
		invalidToken  bool
	}{
		{name: "free to plus", claims: `{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`, oldPlan: "free", wantPlan: "plus"},
		{name: "plus to free", claims: `{"https://api.openai.com/auth":{"chatgpt_plan_type":"free"}}`, oldPlan: "plus", wantPlan: "free"},
		{name: "trim plan", claims: `{"https://api.openai.com/auth":{"chatgpt_plan_type":" pro "}}`, oldPlan: "plus", wantPlan: "pro"},
		{name: "initialize attributes", claims: `{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`, nilAttributes: true, wantPlan: "plus"},
		{name: "missing plan", claims: `{}`, oldPlan: "plus", wantPlan: "plus"},
		{name: "empty plan", claims: `{"https://api.openai.com/auth":{"chatgpt_plan_type":""}}`, oldPlan: "plus", wantPlan: "plus"},
		{name: "blank plan", claims: `{"https://api.openai.com/auth":{"chatgpt_plan_type":"  "}}`, oldPlan: "plus", wantPlan: "plus"},
		{name: "invalid token", invalidToken: true, oldPlan: "plus", wantPlan: "plus"},
		{name: "missing token", oldPlan: "plus", wantPlan: "plus"},
		{name: "missing plan without attributes", claims: `{}`, nilAttributes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idToken := ""
			if tc.claims != "" {
				idToken = "e30." + base64.RawURLEncoding.EncodeToString([]byte(tc.claims)) + ".signature"
			}
			if tc.invalidToken {
				idToken = "invalid-token"
			}
			body, errMarshal := json.Marshal(map[string]any{
				"id_token":      idToken,
				"access_token":  "new-access-token",
				"refresh_token": "new-refresh-token",
				"expires_in":    3600,
			})
			if errMarshal != nil {
				t.Fatalf("marshal token response: %v", errMarshal)
			}

			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			calls := 0
			http.DefaultTransport = refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || req.URL.String() != codexauth.TokenURL {
					t.Fatalf("refresh request = %s %s", req.Method, req.URL)
				}
				if errParseForm := req.ParseForm(); errParseForm != nil {
					t.Fatalf("parse refresh form: %v", errParseForm)
				}
				if req.Form.Get("grant_type") != "refresh_token" || req.Form.Get("refresh_token") != "old-refresh-token" {
					t.Fatal("unexpected refresh form")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(string(body))),
				}, nil
			})

			auth := &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"id_token":      "old-id-token",
					"access_token":  "old-access-token",
					"refresh_token": "old-refresh-token",
				},
			}
			if !tc.nilAttributes {
				auth.Attributes = map[string]string{"plan_type": tc.oldPlan, "note": "keep"}
			}
			updated, errRefresh := refreshCredential(context.Background(), nil, auth, nil)
			if errRefresh != nil {
				t.Fatalf("refreshCredential() error = %v", errRefresh)
			}
			if calls != 1 {
				t.Fatalf("refresh calls = %d, want 1", calls)
			}
			if got := updated.Attributes["plan_type"]; got != tc.wantPlan {
				t.Fatalf("plan_type = %q, want %q", got, tc.wantPlan)
			}
			if !tc.nilAttributes && updated.Attributes["note"] != "keep" {
				t.Fatal("unrelated attribute was lost")
			}
			if tc.nilAttributes && tc.wantPlan == "" && updated.Attributes != nil {
				t.Fatalf("attributes = %#v, want nil without a plan", updated.Attributes)
			}
			if updated.Metadata["id_token"] != idToken || updated.Metadata["access_token"] != "new-access-token" || updated.Metadata["refresh_token"] != "new-refresh-token" {
				t.Fatal("refreshed token metadata was not updated")
			}
		})
	}
}
