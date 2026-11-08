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
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/oautherror"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
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

var codexRefreshGroup singleflight.Group

type codexRefreshResponseError struct {
	statusCode   int
	terminalCode string
	upstream     *oautherror.ResponseError
}

func (e *codexRefreshResponseError) Error() string {
	if e == nil {
		return "token refresh failed"
	}
	if e.upstream != nil {
		return e.upstream.Error()
	}
	return fmt.Sprintf("token refresh failed with status %d", e.statusCode)
}

func (e *codexRefreshResponseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.upstream
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
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	var ownsRefresh atomic.Bool
	resultCh := codexRefreshGroup.DoChan(refreshToken, func() (any, error) {
		ownsRefresh.Store(true)
		return o.refreshTokens(ctx, refreshToken)
	})
	var result singleflight.Result
	select {
	case <-ctx.Done():
		if !ownsRefresh.Load() {
			return nil, ctx.Err()
		}
		// Keep the owner's exchange outcome, especially a known token rotation.
		// Only waiters may leave without waiting for that exchange to finish.
		result = <-resultCh
	case result = <-resultCh:
	}
	// Cancellation wins for waiters even when both channels were ready.
	if errContext := ctx.Err(); errContext != nil && !ownsRefresh.Load() {
		return nil, errContext
	}
	if result.Err != nil {
		return nil, result.Err
	}
	tokens, ok := result.Val.(*CodexTokenData)
	if !ok || tokens == nil {
		return nil, fmt.Errorf("token refresh failed: invalid single-flight result")
	}
	return tokens, nil
}

func (o *CodexAuth) refreshTokens(ctx context.Context, refreshToken string) (*CodexTokenData, error) {
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
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

	resp, err := doRefreshRequest(o.httpClient, req)
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
	if strings.TrimSpace(tokenResp.AccessToken) == "" || tokenResp.ExpiresIn <= 0 {
		return nil, fmt.Errorf("token refresh response is missing a usable access token or expiry")
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

// RefreshTokensWithRetry applies bounded retries to transient refresh failures.
// Explicit grant rejection and caller cancellation stop the retry loop.
func (o *CodexAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*CodexTokenData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maxRetries <= 0 {
		maxRetries = 1
	}
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
		if isNonRetryableRefreshErr(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Warnf("Token refresh attempt %d stopped", attempt+1)
			return nil, err
		}

		lastErr = err
		log.Warnf("Token refresh attempt %d failed; retrying within the configured limit", attempt+1)
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}

func newCodexRefreshResponseError(statusCode int, body []byte) error {
	upstream := oautherror.NewResponseError(statusCode, body)
	code := strings.ToLower(strings.TrimSpace(upstream.OAuthError()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(upstream.Code()))
	}
	terminalCode := codexTerminalRefreshCode(code, strings.Join([]string{upstream.ErrorDescription(), upstream.Message(), upstream.Detail()}, " "))
	return &codexRefreshResponseError{statusCode: statusCode, terminalCode: terminalCode, upstream: upstream}
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

// doRefreshRequest keeps OAuth refresh credentials on the configured endpoint.
func doRefreshRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	refreshClient := *client
	refreshClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return refreshClient.Do(req)
}

// RefreshOutcomeMetadataKey identifies legacy diagnostics retained by old data.
// These diagnostics no longer control refresh eligibility.
const RefreshOutcomeMetadataKey = "codex_refresh_outcomes"
