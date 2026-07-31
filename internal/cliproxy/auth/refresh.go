package auth

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

	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/antigravity"
	claudeauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/claude"
	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	kimiauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/kimi"
	xaiauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	log "github.com/sirupsen/logrus"
)

// refreshCredential refreshes auth metadata when a refresh token is present.
// It is best-effort: providers that do not support refresh are treated as no-op.
func refreshCredential(ctx context.Context, cfg *config.Config, auth *Auth, rt http.RoundTripper) (*Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	if auth == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "codex":
		return refreshCodex(ctx, cfg, auth)
	case "claude":
		return refreshClaude(ctx, cfg, auth)
	case "kimi":
		return refreshKimi(ctx, cfg, auth)
	case "antigravity":
		return refreshAntigravity(ctx, cfg, auth, rt)
	case "xai":
		return refreshXAI(ctx, cfg, auth)
	default:
		return auth, nil
	}
}

// refreshCodex refreshes a codex.
func refreshCodex(ctx context.Context, cfg *config.Config, auth *Auth) (*Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	svc := codexauth.NewCodexAuthWithProxyURL(cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return auth, nil
}

// refreshClaude refreshes a claude.
func refreshClaude(ctx context.Context, cfg *config.Config, auth *Auth) (*Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return auth, nil
}

// refreshKimi refreshes a kimi.
func refreshKimi(ctx context.Context, cfg *config.Config, auth *Auth) (*Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if strings.TrimSpace(refreshToken) == "" {
		return auth, nil
	}
	client := kimiauth.NewDeviceFlowClientWithDeviceIDAndProxyURL(cfg, resolveKimiDeviceID(auth), auth.ProxyURL)
	td, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, kimiauth.ErrRefreshTokenRejected) {
			return nil, newUnauthorizedRefreshError()
		}
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ExpiresAt > 0 {
		auth.Metadata["expired"] = time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	auth.Metadata["type"] = "kimi"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return auth, nil
}

// resolveKimiDeviceID resolves a kimi device id.
func resolveKimiDeviceID(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata["device_id"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

type oauthRefreshErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// antigravityOAuthRefreshError classifies terminal OAuth failures without retaining
// the upstream response body, which may contain credential material.
func antigravityOAuthRefreshError(statusCode int, body []byte) error {
	var oauthErr oauthRefreshErrorResponse
	if errUnmarshal := json.Unmarshal(body, &oauthErr); errUnmarshal == nil &&
		(strings.EqualFold(strings.TrimSpace(oauthErr.Error), "invalid_grant") || isTerminalOAuthRefreshDescription(oauthErr.ErrorDescription)) {
		return newUnauthorizedRefreshError()
	}
	return fmt.Errorf("antigravity refresh: oauth refresh failed with status %d", statusCode)
}

// isTerminalOAuthRefreshDescription recognizes explicit expired or revoked
// refresh-token descriptions returned by OAuth providers.
func isTerminalOAuthRefreshDescription(description string) bool {
	normalized := strings.ToLower(strings.TrimSpace(description))
	normalized = strings.NewReplacer("_", " ", "-", " ").Replace(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return false
	}
	for _, signal := range []string{
		"token has been expired or revoked",
		"refresh token has expired",
		"refresh token is expired",
		"refresh token expired",
		"refresh token has been revoked",
		"refresh token is revoked",
		"refresh token revoked",
	} {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}

// refreshAntigravity refreshes an antigravity.
func refreshAntigravity(ctx context.Context, cfg *config.Config, auth *Auth, rt http.RoundTripper) (*Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	_ = cfg
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}

	form := url.Values{}
	form.Set("client_id", antigravity.ClientID)
	form.Set("client_secret", antigravity.ClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Host", "oauth2.googleapis.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	client := &http.Client{Timeout: 30 * time.Second}
	if rt != nil {
		client.Transport = rt
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity refresh: response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, antigravityOAuthRefreshError(resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(body, &tokenResp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenResp.RefreshToken
	}
	now := time.Now()
	auth.Metadata["expires_in"] = tokenResp.ExpiresIn
	auth.Metadata["timestamp"] = now.UnixMilli()
	auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = "antigravity"
	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)
	return auth, nil
}

// refreshXAI refreshes an xAI OAuth credential.
func refreshXAI(ctx context.Context, cfg *config.Config, auth *Auth) (*Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	tokenEndpoint := metaStringValue(auth.Metadata, "token_endpoint")
	svc := xaiauth.NewXAIAuthWithProxyURL(cfg, auth.ProxyURL)
	if tokenEndpoint == "" {
		discovery, errDiscover := svc.Discover(ctx)
		if errDiscover != nil {
			return nil, errDiscover
		}
		tokenEndpoint = discovery.TokenEndpoint
	}
	td, errRefresh := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if errRefresh != nil {
		return nil, errRefresh
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.IDToken != "" {
		auth.Metadata["id_token"] = td.IDToken
	}
	if td.TokenType != "" {
		auth.Metadata["token_type"] = td.TokenType
	}
	if td.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = td.ExpiresIn
	}
	if td.Expire != "" {
		auth.Metadata["expired"] = td.Expire
	}
	if td.Email != "" {
		auth.Metadata["email"] = td.Email
	}
	if td.Subject != "" {
		auth.Metadata["sub"] = td.Subject
	}
	auth.Metadata["token_endpoint"] = tokenEndpoint
	if _, ok := auth.Metadata["base_url"]; !ok {
		auth.Metadata["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	auth.Metadata["type"] = "xai"
	auth.Metadata["auth_kind"] = "oauth"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return auth, nil
}

// metaStringValue handles a meta string value.
func metaStringValue(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
