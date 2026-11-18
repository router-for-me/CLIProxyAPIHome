package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DeviceFlowTimeout              = 15 * time.Minute
	DeviceVerificationURL          = "https://auth.openai.com/codex/device"
	DeviceTokenExchangeRedirectURI = "https://auth.openai.com/deviceauth/callback"
	deviceUserCodeURL              = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL                 = "https://auth.openai.com/api/accounts/deviceauth/token"
	defaultDevicePollInterval      = 5 * time.Second
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type DeviceFlow struct {
	DeviceAuthID string
	UserCode     string
	PollInterval time.Duration
}

type DeviceToken struct {
	AuthorizationCode string
	CodeVerifier      string
	CodeChallenge     string
}

type deviceUserCodeResponse struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	UserCodeAlt  string          `json:"usercode"`
	Interval     json.RawMessage `json:"interval"`
}

type deviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

type devicePollWaitFunc func(context.Context, time.Duration) error

func RequestDeviceCode(ctx context.Context, client HTTPDoer) (*DeviceFlow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return nil, fmt.Errorf("codex device HTTP client is required")
	}
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}

	body, errMarshal := json.Marshal(struct {
		ClientID string `json:"client_id"`
	}{ClientID})
	if errMarshal != nil {
		return nil, fmt.Errorf("failed to encode codex device request: %w", errMarshal)
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, deviceUserCodeURL, bytes.NewReader(body))
	if errRequest != nil {
		return nil, fmt.Errorf("failed to create codex device request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to request codex device code: %w", errDo)
	}
	respBody, errRead := readDeviceResponseBody(resp)
	if errRead != nil {
		return nil, fmt.Errorf("failed to read codex device code response: %w", errRead)
	}
	if !deviceSuccessStatus(resp.StatusCode) {
		trimmed := strings.TrimSpace(string(respBody))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("codex device endpoint is unavailable (status %d)", resp.StatusCode)
		}
		if trimmed == "" {
			trimmed = "empty response body"
		}
		return nil, fmt.Errorf("codex device code request failed with status %d: %s", resp.StatusCode, trimmed)
	}

	var parsed deviceUserCodeResponse
	if errDecode := json.Unmarshal(respBody, &parsed); errDecode != nil {
		return nil, fmt.Errorf("failed to decode codex device code response: %w", errDecode)
	}
	userCode := strings.TrimSpace(parsed.UserCode)
	if userCode == "" {
		userCode = strings.TrimSpace(parsed.UserCodeAlt)
	}
	deviceAuthID := strings.TrimSpace(parsed.DeviceAuthID)
	if userCode == "" || deviceAuthID == "" {
		return nil, fmt.Errorf("codex device flow did not return required fields")
	}

	return &DeviceFlow{
		DeviceAuthID: deviceAuthID,
		UserCode:     userCode,
		PollInterval: parseDevicePollInterval(parsed.Interval),
	}, nil
}

func WaitForDeviceAuthorization(ctx context.Context, client HTTPDoer, flow *DeviceFlow) (*DeviceToken, error) {
	return waitForDeviceAuthorization(ctx, client, flow, waitForDevicePollInterval)
}

func waitForDeviceAuthorization(ctx context.Context, client HTTPDoer, flow *DeviceFlow, wait devicePollWaitFunc) (*DeviceToken, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return nil, fmt.Errorf("codex device HTTP client is required")
	}
	if flow == nil || strings.TrimSpace(flow.DeviceAuthID) == "" || strings.TrimSpace(flow.UserCode) == "" {
		return nil, fmt.Errorf("codex device flow is missing required fields")
	}
	if wait == nil {
		return nil, fmt.Errorf("codex device poll wait function is required")
	}

	for {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		body, errMarshal := json.Marshal(struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}{DeviceAuthID: flow.DeviceAuthID, UserCode: flow.UserCode})
		if errMarshal != nil {
			return nil, fmt.Errorf("failed to encode codex device poll request: %w", errMarshal)
		}

		req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, deviceTokenURL, bytes.NewReader(body))
		if errRequest != nil {
			return nil, fmt.Errorf("failed to create codex device poll request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, errDo := client.Do(req)
		if errDo != nil {
			return nil, fmt.Errorf("failed to poll codex device token: %w", errDo)
		}
		respBody, errRead := readDeviceResponseBody(resp)
		if errRead != nil {
			return nil, fmt.Errorf("failed to read codex device poll response: %w", errRead)
		}

		switch {
		case deviceSuccessStatus(resp.StatusCode):
			var parsed deviceTokenResponse
			if errDecode := json.Unmarshal(respBody, &parsed); errDecode != nil {
				return nil, fmt.Errorf("failed to decode codex device token response: %w", errDecode)
			}
			return &DeviceToken{
				AuthorizationCode: strings.TrimSpace(parsed.AuthorizationCode),
				CodeVerifier:      strings.TrimSpace(parsed.CodeVerifier),
				CodeChallenge:     strings.TrimSpace(parsed.CodeChallenge),
			}, nil
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
			if errWait := wait(ctx, flow.PollInterval); errWait != nil {
				return nil, errWait
			}
		default:
			trimmed := strings.TrimSpace(string(respBody))
			if trimmed == "" {
				trimmed = "empty response body"
			}
			return nil, fmt.Errorf("codex device token polling failed with status %d: %s", resp.StatusCode, trimmed)
		}
	}
}

func parseDevicePollInterval(raw json.RawMessage) time.Duration {
	if len(raw) == 0 {
		return defaultDevicePollInterval
	}

	var asString string
	if errUnmarshal := json.Unmarshal(raw, &asString); errUnmarshal == nil {
		if seconds, errConvert := strconv.Atoi(strings.TrimSpace(asString)); errConvert == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}

	var asInt int
	if errUnmarshal := json.Unmarshal(raw, &asInt); errUnmarshal == nil && asInt > 0 {
		return time.Duration(asInt) * time.Second
	}
	return defaultDevicePollInterval
}

func deviceSuccessStatus(code int) bool {
	return code >= http.StatusOK && code < http.StatusMultipleChoices
}

func readDeviceResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("codex device response body is missing")
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		_ = resp.Body.Close()
		return nil, errRead
	}
	if errClose := resp.Body.Close(); errClose != nil {
		return nil, fmt.Errorf("failed to close codex device response body: %w", errClose)
	}
	return body, nil
}

func waitForDevicePollInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
