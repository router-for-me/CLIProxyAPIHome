package codex

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type outcomeRoundTripper func(*http.Request) (*http.Response, error)

func (f outcomeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type lostRefreshBody struct{}

func (*lostRefreshBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*lostRefreshBody) Close() error             { return nil }

func TestRefreshTokensUnknownOutcomeDoesNotReplay(t *testing.T) {
	unresolvedRefreshOutcomes.Clear()
	t.Cleanup(unresolvedRefreshOutcomes.Clear)
	partition := "outer"
	for _, tc := range []struct {
		name   string
		stage  string
		status int
		body   string
	}{
		{"transport", "transport", 0, ""},
		{"read", "response_read", http.StatusOK, ""},
		{"decode", "response_decode", http.StatusOK, `{"access_token":`},
		{"validate", "response_validate", http.StatusOK, `{}`},
		{"wrong_type", "response_decode", http.StatusOK, `{"access_token":123,"expires_in":3600}`},
		{"server_error", "response_status", http.StatusServiceUnavailable, `{"error":"server_error"}`},
		{"rate_limit", "response_status", http.StatusTooManyRequests, `{"error":"rate_limited"}`},
		{"redirect", "response_status", http.StatusTemporaryRedirect, `{}`},
	} {
		t.Run(partition+"/"+tc.name, func(t *testing.T) {
			var calls atomic.Int32
			transportErr := errors.New("synthetic acceptance unknown: secret-refresh-material")
			transport := outcomeRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if tc.status == 0 {
					return nil, transportErr
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(tc.body))
				if tc.name == "read" {
					body = &lostRefreshBody{}
				}
				header := make(http.Header)
				header.Set("Location", "https://example.invalid/replay")
				return &http.Response{StatusCode: tc.status, Body: body, Header: header, Request: req}, nil
			})
			token := t.Name()
			refresh := func(auth *CodexAuth, token string) error {
				_, errRefresh := auth.RefreshTokensWithRetry(context.Background(), token, 3)
				return errRefresh
			}
			for attempt := 0; attempt < 2; attempt++ {
				errRefresh := refresh(&CodexAuth{httpClient: &http.Client{Transport: transport}}, token)
				var outcome *RefreshOutcomeUnknownError
				if !errors.As(errRefresh, &outcome) || outcome.Stage() != tc.stage {
					t.Fatalf("outcome = %v, want stage %s", errRefresh, tc.stage)
				}
				if strings.Contains(errRefresh.Error(), "secret-refresh-material") {
					t.Fatal("refresh outcome exposed transport error text")
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("transport calls = %d, want 1 including subsequent invocations", got)
				}
			}
			refresh(&CodexAuth{httpClient: &http.Client{Transport: transport}}, token+"-replacement")
			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls after replacement = %d, want 2", got)
			}
		})
	}
}

func TestRefreshTokensRetriesOnlyBeforeSubmission(t *testing.T) {
	var calls atomic.Int32
	auth := &CodexAuth{httpClient: &http.Client{Transport: outcomeRoundTripper(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
			Body: io.NopCloser(strings.NewReader(`{"access_token":"access","refresh_token":"replacement","expires_in":3600}`)),
		}, nil
	})}}
	data, errRefresh := auth.RefreshTokensWithRetry(context.Background(), t.Name(), 2)
	if errRefresh != nil || data == nil || data.RefreshToken != "replacement" || calls.Load() != 2 {
		t.Fatalf("pre-submission retry: data=%v err=%v calls=%d", data, errRefresh, calls.Load())
	}
}

func TestRefreshOutcomeMetadataTracksPartitionsAndGrantVersions(t *testing.T) {
	metadata := map[string]any{
		"refresh_token": "outer-original",
		"siwc":          map[string]any{"refresh_token": "siwc-original", "client_id": "siwc-client"},
	}
	outerErr := NewRefreshOutcomeUnknownError(ClientID, "outer-original", "response_read", io.ErrUnexpectedEOF)
	siwcErr := NewRefreshOutcomeUnknownError("siwc-client", "siwc-original", "transport", context.DeadlineExceeded)
	if !RecordRefreshOutcomeUnknown(metadata, errors.Join(outerErr, siwcErr)) {
		t.Fatal("did not record unknown outcomes")
	}
	if RefreshOutcomeUnknown(metadata, false) == nil || RefreshOutcomeUnknown(metadata, true) == nil {
		t.Fatal("missing partition fence")
	}
	metadata["refresh_token"] = "outer-replacement"
	if RefreshOutcomeUnknown(metadata, false) != nil || RefreshOutcomeUnknown(metadata, true) == nil {
		t.Fatal("replacement did not release only the outer fence")
	}
	if RecordRefreshOutcomeUnknown(metadata, outerErr) {
		t.Fatal("stale failure quarantined a replacement grant")
	}
	metadata["siwc"].(map[string]any)["client_id"] = "new-client"
	if RefreshOutcomeUnknown(metadata, true) != nil {
		t.Fatal("old client fence blocked a different grant")
	}
}

func TestPersistedOutcomeAlsoProtectsOtherCallersOfSameGrant(t *testing.T) {
	token := t.Name()
	t.Cleanup(func() { unresolvedRefreshOutcomes.Delete(refreshGrantFingerprint(ClientID, token)) })
	metadata := map[string]any{"refresh_token": token}
	RecordRefreshOutcomeUnknown(metadata, NewRefreshOutcomeUnknownError(ClientID, token, "response_read", io.ErrUnexpectedEOF))
	if RefreshOutcomeUnknown(metadata, false) == nil {
		t.Fatal("missing persisted outcome")
	}
	var calls atomic.Int32
	auth := &CodexAuth{httpClient: &http.Client{Transport: outcomeRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected replay")
	})}}
	_, errRefresh := auth.RefreshTokens(context.Background(), token)
	if calls.Load() != 0 || !IsRefreshOutcomeUnknown(errRefresh) {
		t.Fatalf("persisted unresolved grant was replayed by another caller: calls=%d err=%v", calls.Load(), errRefresh)
	}
}
