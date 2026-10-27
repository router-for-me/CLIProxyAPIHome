package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type deviceRoundTripper func(*http.Request) (*http.Response, error)

func (f deviceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRequestDeviceCode(t *testing.T) {
	client := &http.Client{Transport: deviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != deviceUserCodeURL {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers = %v", req.Header)
		}
		var body map[string]string
		if errDecode := json.NewDecoder(req.Body).Decode(&body); errDecode != nil {
			t.Fatal(errDecode)
		}
		if body["client_id"] != ClientID {
			t.Fatalf("client_id = %q", body["client_id"])
		}
		return deviceResponse(http.StatusOK, `{"device_auth_id":"private-device-id","usercode":" AB12-CD34 ","interval":"3"}`), nil
	})}

	flow, errFlow := RequestDeviceCode(context.Background(), client)
	if errFlow != nil {
		t.Fatal(errFlow)
	}
	if flow.DeviceAuthID != "private-device-id" || flow.UserCode != "AB12-CD34" || flow.PollInterval != 3*time.Second {
		t.Fatalf("flow = %+v", flow)
	}
}

func TestRequestDeviceCodeRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantError  string
	}{
		{name: "missing device values", statusCode: http.StatusOK, body: `{"device_auth_id":"id"}`, wantError: "required fields"},
		{name: "endpoint unavailable", statusCode: http.StatusNotFound, body: `{"error":"missing"}`, wantError: "endpoint is unavailable"},
		{name: "provider error", statusCode: http.StatusBadGateway, body: "", wantError: "empty response body"},
	}
	for _, test := range tests {
		t.Run(test.name, func(tRun *testing.T) {
			client := &http.Client{Transport: deviceRoundTripper(func(*http.Request) (*http.Response, error) {
				return deviceResponse(test.statusCode, test.body), nil
			})}
			if _, errFlow := RequestDeviceCode(context.Background(), client); errFlow == nil || !strings.Contains(errFlow.Error(), test.wantError) {
				tRun.Fatalf("RequestDeviceCode() error = %v, want text %q", errFlow, test.wantError)
			}
		})
	}
}

func TestParseDevicePollInterval(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "missing", want: defaultDevicePollInterval},
		{name: "string seconds", raw: `"7"`, want: 7 * time.Second},
		{name: "numeric seconds", raw: `4`, want: 4 * time.Second},
		{name: "invalid", raw: `"0"`, want: defaultDevicePollInterval},
	}
	for _, test := range tests {
		t.Run(test.name, func(tRun *testing.T) {
			var raw json.RawMessage
			if test.raw != "" {
				raw = json.RawMessage(test.raw)
			}
			if got := parseDevicePollInterval(raw); got != test.want {
				tRun.Fatalf("parseDevicePollInterval(%s) = %s, want %s", test.raw, got, test.want)
			}
		})
	}
}

func TestWaitForDeviceAuthorizationPollsPendingStatuses(t *testing.T) {
	statuses := []int{http.StatusForbidden, http.StatusNotFound, http.StatusOK}
	requests := 0
	waits := 0
	client := &http.Client{Transport: deviceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != deviceTokenURL {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		var body map[string]string
		if errDecode := json.NewDecoder(req.Body).Decode(&body); errDecode != nil {
			t.Fatal(errDecode)
		}
		if body["device_auth_id"] != "private-device-id" || body["user_code"] != "AB12-CD34" {
			t.Fatalf("poll body = %v", body)
		}
		status := statuses[requests]
		requests++
		if status == http.StatusOK {
			return deviceResponse(status, `{"authorization_code":"auth-code","code_verifier":"verifier","code_challenge":"challenge"}`), nil
		}
		return deviceResponse(status, "{}"), nil
	})}
	flow := &DeviceFlow{DeviceAuthID: "private-device-id", UserCode: "AB12-CD34", PollInterval: 2 * time.Second}
	token, errToken := waitForDeviceAuthorization(context.Background(), client, flow, func(context.Context, time.Duration) error {
		waits++
		return nil
	})
	if errToken != nil {
		t.Fatal(errToken)
	}
	if token.AuthorizationCode != "auth-code" || token.CodeVerifier != "verifier" || token.CodeChallenge != "challenge" {
		t.Fatalf("token = %+v", token)
	}
	if requests != 3 || waits != 2 {
		t.Fatalf("requests=%d waits=%d, want 3 and 2", requests, waits)
	}
}

func TestWaitForDeviceAuthorizationCancellationStopsPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requests := 0
	client := &http.Client{Transport: deviceRoundTripper(func(*http.Request) (*http.Response, error) {
		requests++
		cancel()
		return deviceResponse(http.StatusForbidden, "{}"), nil
	})}
	_, errToken := waitForDeviceAuthorization(ctx, client, &DeviceFlow{
		DeviceAuthID: "private-device-id",
		UserCode:     "AB12-CD34",
		PollInterval: time.Minute,
	}, func(ctxWait context.Context, _ time.Duration) error { return ctxWait.Err() })
	if errToken != context.Canceled {
		t.Fatalf("poll error = %v, want context canceled", errToken)
	}
	if requests != 1 {
		t.Fatalf("poll requests = %d, want 1", requests)
	}
}

func TestRequestDeviceCodeHonorsExpiredDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	requests := 0
	client := &http.Client{Transport: deviceRoundTripper(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, nil
	})}
	if _, errFlow := RequestDeviceCode(ctx, client); errFlow != context.DeadlineExceeded {
		t.Fatalf("RequestDeviceCode() error = %v, want deadline exceeded", errFlow)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func deviceResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
