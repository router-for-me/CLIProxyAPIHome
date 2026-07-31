// Package kimi provides token refresh for Kimi (Moonshot AI) credentials used by the scheduler.
package kimi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	kimiClientID    = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiOAuthHost   = "https://auth.kimi.com"
	kimiTokenURL    = kimiOAuthHost + "/api/oauth/token"
	kimiHTTPTimeout = 30 * time.Second
)

// DeviceFlowClient is a minimal Kimi OAuth client used for refresh-token exchange.
type DeviceFlowClient struct {
	httpClient *http.Client
	deviceID   string
}

// NewDeviceFlowClientWithDeviceIDAndProxyURL creates a new refresh client with proxy override.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewDeviceFlowClientWithDeviceIDAndProxyURL(cfg *config.Config, deviceID string, proxyURL string) *DeviceFlowClient {
	client := &http.Client{Timeout: kimiHTTPTimeout}
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	client = util.SetProxy(&sdkCfg, client)

	resolvedDeviceID := strings.TrimSpace(deviceID)
	if resolvedDeviceID == "" {
		resolvedDeviceID = newRandomID(16)
	}
	return &DeviceFlowClient{
		httpClient: client,
		deviceID:   resolvedDeviceID,
	}
}

// newRandomID creates a random id.
func newRandomID(nbytes int) string {
	if nbytes <= 0 {
		nbytes = 16
	}
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "device"
	}
	return hex.EncodeToString(buf)
}

// getDeviceModel returns a device model.
func getDeviceModel() string {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch osName {
	case "darwin":
		return fmt.Sprintf("macOS %s", arch)
	case "windows":
		return fmt.Sprintf("Windows %s", arch)
	case "linux":
		return fmt.Sprintf("Linux %s", arch)
	default:
		return fmt.Sprintf("%s %s", osName, arch)
	}
}

// getHostname returns a hostname.
func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// commonHeaders handles a common headers.
func (c *DeviceFlowClient) commonHeaders() map[string]string {
	if c == nil {
		return nil
	}
	return map[string]string{
		"X-Msh-Platform":     "cli-proxy-api-home",
		"X-Msh-Version":      "1.0.0",
		"X-Msh-Device-Name":  getHostname(),
		"X-Msh-Device-Model": getDeviceModel(),
		"X-Msh-Device-Id":    c.deviceID,
	}
}

// RefreshToken exchanges a refresh token for a new access token.
func (c *DeviceFlowClient) RefreshToken(ctx context.Context, refreshToken string) (*KimiTokenData, error) {
	// Resolve credential context before calling upstream OAuth services.
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("kimi: client not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("kimi: refresh token is required")
	}

	data := url.Values{}
	data.Set("client_id", kimiClientID)
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, kimiTokenURL, strings.NewReader(data.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("kimi: failed to create refresh request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.commonHeaders() {
		req.Header.Set(k, v)
	}

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("kimi: refresh request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kimi refresh token: response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("kimi: failed to read refresh response: %w", errRead)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("kimi: refresh token rejected (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kimi: refresh failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResp struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return nil, fmt.Errorf("kimi: failed to parse refresh response: %w", errUnmarshal)
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("kimi: empty access token in refresh response")
	}

	var expiresAt int64
	if tokenResp.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)
	}

	return &KimiTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        tokenResp.Scope,
	}, nil
}
