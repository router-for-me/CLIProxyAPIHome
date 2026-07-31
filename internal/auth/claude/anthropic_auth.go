// Package claude provides token refresh functionality for Anthropic credentials used by the scheduler.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	TokenURL = "https://api.anthropic.com/v1/oauth/token"
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	claudeRefreshMinBackoff = 5 * time.Second
	claudeRefreshMaxBackoff = 5 * time.Minute
)

var (
	claudeRefreshGroup singleflight.Group
	claudeRefreshMu    sync.Mutex
	claudeRefreshBlock = make(map[string]time.Time)
)

type refreshHTTPError struct {
	status    int
	message   string
	retryable bool
}

// Error returns the error message.
func (e *refreshHTTPError) Error() string {
	return fmt.Sprintf("token refresh failed with status %d: %s", e.status, e.message)
}

// Retryable reports whether the error can be retried.
func (e *refreshHTTPError) Retryable() bool {
	return e != nil && e.retryable
}

// claudeRefreshBlockedUntil handles a claude refresh blocked until.
func claudeRefreshBlockedUntil(refreshToken string) time.Time {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	return claudeRefreshBlock[refreshToken]
}

// setClaudeRefreshBlockedUntil sets a claude refresh blocked until.
func setClaudeRefreshBlockedUntil(refreshToken string, until time.Time) {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	claudeRefreshBlock[refreshToken] = until
}

// clearClaudeRefreshBlockedUntil clears a claude refresh blocked until.
func clearClaudeRefreshBlockedUntil(refreshToken string) {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	delete(claudeRefreshBlock, refreshToken)
}

// clampClaudeRefreshBackoff handles a clamp claude refresh backoff.
func clampClaudeRefreshBackoff(d time.Duration) time.Duration {
	if d < claudeRefreshMinBackoff {
		return claudeRefreshMinBackoff
	}
	if d > claudeRefreshMaxBackoff {
		return claudeRefreshMaxBackoff
	}
	return d
}

// parseClaudeRetryAfter parses a claude retry after.
func parseClaudeRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return claudeRefreshMinBackoff
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After")); raw != "" {
		if seconds, err := time.ParseDuration(raw + "s"); err == nil {
			return clampClaudeRefreshBackoff(seconds)
		}
		if when, err := http.ParseTime(raw); err == nil {
			return clampClaudeRefreshBackoff(time.Until(when))
		}
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After-Ms")); raw != "" {
		if ms, err := time.ParseDuration(raw + "ms"); err == nil {
			return clampClaudeRefreshBackoff(ms)
		}
	}
	return claudeRefreshMinBackoff
}

// isClaudeRefreshRetryable reports whether claude refresh retryable.
func isClaudeRefreshRetryable(err error) bool {
	var httpErr *refreshHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	return true
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

type ClaudeAuth struct {
	httpClient *http.Client
}

// NewClaudeAuthWithProxyURL creates a new refresh client instance.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewClaudeAuthWithProxyURL(cfg *config.Config, proxyURL string) *ClaudeAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg *config.SDKConfig
	if cfg != nil {
		sdkCfgCopy := cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
		sdkCfgCopy.ProxyURL = effectiveProxyURL
		sdkCfg = &sdkCfgCopy
	} else if effectiveProxyURL != "" {
		sdkCfgCopy := config.SDKConfig{ProxyURL: effectiveProxyURL}
		sdkCfg = &sdkCfgCopy
	}

	return &ClaudeAuth{
		httpClient: NewAnthropicHttpClient(sdkCfg),
	}
}

// RefreshTokens refreshes access token via refresh token.
func (o *ClaudeAuth) RefreshTokens(ctx context.Context, refreshToken string) (*ClaudeTokenData, error) {
	// Resolve credential context before calling upstream OAuth services.
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if blockedUntil := claudeRefreshBlockedUntil(refreshToken); blockedUntil.After(time.Now()) {
		return nil, &refreshHTTPError{
			status:    http.StatusTooManyRequests,
			message:   fmt.Sprintf("refresh temporarily blocked until %s", blockedUntil.Format(time.RFC3339)),
			retryable: false,
		}
	}

	result, err, _ := claudeRefreshGroup.Do(refreshToken, func() (interface{}, error) {
		return o.refreshTokensSingleFlight(context.WithoutCancel(ctx), refreshToken)
	})
	if err != nil {
		return nil, err
	}
	tokenData, ok := result.(*ClaudeTokenData)
	if !ok || tokenData == nil {
		return nil, fmt.Errorf("token refresh failed: invalid single-flight result")
	}
	return tokenData, nil
}

// refreshTokensSingleFlight converts refresh tokens single flight.
func (o *ClaudeAuth) refreshTokensSingleFlight(ctx context.Context, refreshToken string) (*ClaudeTokenData, error) {
	// Resolve credential context before calling upstream OAuth services.
	if blockedUntil := claudeRefreshBlockedUntil(refreshToken); blockedUntil.After(time.Now()) {
		return nil, &refreshHTTPError{
			status:    http.StatusTooManyRequests,
			message:   fmt.Sprintf("refresh temporarily blocked until %s", blockedUntil.Format(time.RFC3339)),
			retryable: false,
		}
	}

	reqBody := map[string]interface{}{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	}

	jsonBody, errMarshal := json.Marshal(reqBody)
	if errMarshal != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", errMarshal)
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(string(jsonBody)))
	if errReq != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", errReq)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("claude refresh: response body close error: %v", errClose)
		}
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK {
		message := string(body)
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseClaudeRetryAfter(resp)
			setClaudeRefreshBlockedUntil(refreshToken, time.Now().Add(retryAfter))
			return nil, &refreshHTTPError{status: resp.StatusCode, message: message, retryable: false}
		}
		return nil, &refreshHTTPError{
			status:    resp.StatusCode,
			message:   message,
			retryable: resp.StatusCode >= http.StatusInternalServerError,
		}
	}

	var tokenResp tokenResponse
	if errUnmarshal := json.Unmarshal(body, &tokenResp); errUnmarshal != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", errUnmarshal)
	}

	clearClaudeRefreshBlockedUntil(refreshToken)

	return &ClaudeTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Email:        tokenResp.Account.EmailAddress,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// RefreshTokensWithRetry refreshes refresh tokens with retry.
func (o *ClaudeAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*ClaudeTokenData, error) {
	// Resolve credential context before calling upstream OAuth services.
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
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

		lastErr = err
		log.Warnf("Token refresh attempt %d failed: %v", attempt+1, err)
		if !isClaudeRefreshRetryable(err) {
			break
		}
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}
