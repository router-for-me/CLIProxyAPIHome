package cluster

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCodexLateReceiptPreservesGrantWhenRotationOmitsRefreshToken(t *testing.T) {
	for _, legacyReceipt := range []bool{false, true} {
		name := "current"
		if legacyReceipt {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo := newRefreshTestRepository(t)
			original, current := t.Name(), t.Name()+"-next"
			var calls atomic.Int32
			transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if errParse := req.ParseForm(); errParse != nil {
					return nil, errParse
				}
				if req.Form.Get("refresh_token") == original {
					return codexJournalTestResponse(req, current), nil
				}
				return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"access_token":"latest-access","expires_in":3600}`))}, nil
			})
			installCodexJournalTransport(t, transport)
			svc := codexauth.NewCodexAuthWithProxyURL(nil, "")
			for _, grant := range []string{original, current} {
				if _, errRefresh := svc.RefreshTokens(codexauth.WithRefreshJournal(ctx, repo), grant); errRefresh != nil {
					t.Fatal(errRefresh)
				}
			}
			if legacyReceipt {
				// Older receipts stored the provider response with no refresh token.
				receipt := JSONB(`{"tokens":{"access_token":"latest-access","expired":"2099-01-01T00:00:00Z"}}`)
				if errUpdate := repo.db.Model(&CodexRefreshRecord{}).Where("grant_sha256 = ?", codexJournalTestKey(current)).Update("receipt", receipt).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "old-access", "refresh_token": original},
			}
			runtime := newRefreshTestRuntime(t, repo, auth, transport)
			updated, errRefresh := runtime.CoreManager().RefreshAuthCredential(ctx, auth)
			if errRefresh != nil {
				t.Fatal(errRefresh)
			}
			if updated.Metadata["access_token"] != "latest-access" || updated.Metadata["refresh_token"] != current || calls.Load() != 2 {
				t.Fatalf("late caller did not adopt the grant of the latest receipt: calls=%d", calls.Load())
			}
		})
	}
}
