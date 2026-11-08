package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestRefreshTokensCoalescedWaiterHonorsContext(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				auth := &CodexAuth{httpClient: &http.Client{Transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(started)
					}
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"renewed","refresh_token":"rotated","expires_in":3600}`))}, nil
				})}}
				type result struct {
					tokens *CodexTokenData
					err    error
				}
				owner, active, waiter := make(chan result, 1), make(chan result, 1), make(chan result, 1)
				go func() {
					tokens, errRefresh := auth.RefreshTokens(context.Background(), t.Name())
					owner <- result{tokens, errRefresh}
				}()
				<-started
				go func() {
					tokens, errRefresh := auth.RefreshTokens(context.Background(), t.Name())
					active <- result{tokens, errRefresh}
				}()
				var waiterCtx context.Context
				var cancel context.CancelFunc
				expected := error(context.Canceled)
				if mode == "cancel" {
					waiterCtx, cancel = context.WithCancel(context.Background())
				} else {
					waiterCtx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
					expected = context.DeadlineExceeded
				}
				defer cancel()
				go func() {
					tokens, errRefresh := auth.RefreshTokensWithRetry(waiterCtx, t.Name(), 3)
					waiter <- result{tokens, errRefresh}
				}()
				// All callers have joined the flight before canceling the waiter.
				synctest.Wait()
				if mode == "cancel" {
					cancel()
				} else {
					<-waiterCtx.Done()
				}
				synctest.Wait()
				returned := false
				var stopped result
				select {
				case stopped = <-waiter:
					returned = true
				default:
				}
				if len(owner) != 0 || len(active) != 0 {
					t.Error("waiter cancellation ended another caller")
				}
				close(release)
				for _, out := range []<-chan result{owner, active} {
					completed := <-out
					if completed.err != nil || completed.tokens == nil || completed.tokens.AccessToken != "renewed" || completed.tokens.RefreshToken != "rotated" {
						t.Errorf("shared refresh did not complete: %v", completed.err)
					}
				}
				if !returned {
					stopped = <-waiter
				}
				if !returned || stopped.tokens != nil || !errors.Is(stopped.err, expected) {
					t.Errorf("waiter ignored its context: returned=%t err=%v, want %v", returned, stopped.err, expected)
				}
				if calls.Load() != 1 {
					t.Errorf("made %d exchanges, want 1", calls.Load())
				}
			})
		})
	}
}

func TestRefreshTokensOwnerRetainsKnownOutcomeAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"success", http.StatusOK, `{"access_token":"renewed","refresh_token":"rotated","expires_in":3600}`},
		{"rejected", http.StatusBadRequest, `{"error":"invalid_grant"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			auth := &CodexAuth{httpClient: &http.Client{Transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				// Model a provider result arriving as the owner is canceled.
				cancel()
				return &http.Response{StatusCode: tc.status, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			tokens, errRefresh := auth.RefreshTokensWithRetry(ctx, t.Name(), 3)
			if tc.status == http.StatusOK {
				if errRefresh != nil || tokens == nil || tokens.AccessToken != "renewed" || tokens.RefreshToken != "rotated" {
					t.Fatalf("owner lost successful rotation: %v", errRefresh)
				}
			} else {
				var responseErr *codexRefreshResponseError
				if tokens != nil || !errors.As(errRefresh, &responseErr) || responseErr.terminalCode != "invalid_grant" {
					t.Fatalf("owner lost explicit rejection: %v", errRefresh)
				}
			}
			if calls != 1 {
				t.Fatalf("made %d exchanges, want 1", calls)
			}
		})
	}
}
