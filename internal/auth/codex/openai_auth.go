// Package codex provides authentication and token management for OpenAI's Codex API.
// It handles token refreshing for scheduler-managed Codex credentials.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
)

// OAuth configuration constants for OpenAI Codex refresh.
const (
	TokenURL = "https://auth.openai.com/oauth/token"
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// CodexAuth handles the OpenAI OAuth2 authentication flow.
// It manages the HTTP client and provides methods for generating authorization URLs,
// exchanging authorization codes for tokens, and refreshing access tokens.
type CodexAuth struct {
	httpClient *http.Client
}

type codexRefreshResponseError struct {
	statusCode   int
	terminalCode string
}

func (e *codexRefreshResponseError) Error() string {
	if e != nil && e.terminalCode != "" {
		return "token refresh failed: " + e.terminalCode
	}
	if e == nil {
		return "token refresh failed"
	}
	return fmt.Sprintf("token refresh failed with status %d", e.statusCode)
}

// NewCodexAuthWithProxyURL creates a new CodexAuth service instance.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewCodexAuthWithProxyURL(cfg *config.Config, proxyURL string) *CodexAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &CodexAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{}),
	}
}

// RefreshTokens refreshes an access token using a refresh token.
// This method is called when an access token has expired. It makes a request to the
// token endpoint to obtain a new set of tokens.
func (o *CodexAuth) RefreshTokens(ctx context.Context, refreshToken string) (*CodexTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}

	data := url.Values{
		"client_id":     {ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex refresh: response body close error: %v", errClose)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, newCodexRefreshResponseError(resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse refreshed ID token: %v", err)
	}

	accountID := ""
	email := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.Email
	}

	return &CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// RefreshTokensWithRetry refreshes tokens with a built-in retry mechanism.
// It attempts to refresh the tokens up to a specified maximum number of retries,
// with an exponential backoff strategy to handle transient network errors.
func (o *CodexAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*CodexTokenData, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		tokenData, err := o.RefreshTokens(ctx, refreshToken)
		if err == nil {
			return tokenData, nil
		}
		if isNonRetryableRefreshErr(err) {
			log.Warnf("Token refresh attempt %d failed with a non-retryable provider response", attempt+1)
			return nil, err
		}

		lastErr = err
		log.Warnf("Token refresh attempt %d failed with a retryable provider response", attempt+1)
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}

func newCodexRefreshResponseError(statusCode int, body []byte) error {
	var oauthErr struct {
		Error            string `json:"error"`
		Code             string `json:"code"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	_ = json.Unmarshal(body, &oauthErr)
	code := strings.ToLower(strings.TrimSpace(oauthErr.Error))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(oauthErr.Code))
	}
	terminalCode := codexTerminalRefreshCode(code, strings.Join([]string{oauthErr.ErrorDescription, oauthErr.Message}, " "))
	return &codexRefreshResponseError{statusCode: statusCode, terminalCode: terminalCode}
}

func codexTerminalRefreshCode(code, description string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_grant", "refresh_token_expired", "refresh_token_revoked", "refresh_token_reused":
		return strings.ToLower(strings.TrimSpace(code))
	}
	normalized := strings.ToLower(strings.TrimSpace(description))
	normalized = strings.NewReplacer("_", " ", "-", " ").Replace(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")
	mentionsRefreshToken := strings.Contains(normalized, "refresh token")
	if !mentionsRefreshToken && !strings.Contains(normalized, "token has been expired or revoked") {
		return ""
	}
	switch {
	case strings.Contains(normalized, "reused"):
		return "refresh_token_reused"
	case strings.Contains(normalized, "revoked"):
		return "refresh_token_revoked"
	case strings.Contains(normalized, "expired"):
		return "refresh_token_expired"
	default:
		return ""
	}
}

// isNonRetryableRefreshErr reports whether non retryable refresh err.
func isNonRetryableRefreshErr(err error) bool {
	if err == nil {
		return false
	}
	var responseErr *codexRefreshResponseError
	if errors.As(err, &responseErr) && responseErr != nil {
		return responseErr.terminalCode != ""
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "invalid_grant") || strings.Contains(raw, "refresh_token_expired") ||
		strings.Contains(raw, "refresh_token_revoked") || strings.Contains(raw, "refresh_token_reused")
}
