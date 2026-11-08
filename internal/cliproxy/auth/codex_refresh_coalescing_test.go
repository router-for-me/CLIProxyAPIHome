package auth

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

func TestCodexCoalescedCredentialRefreshHonorsContext(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				original := http.DefaultTransport
				defer func() { http.DefaultTransport = original }()
				http.DefaultTransport = refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(started)
					}
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"renewed","refresh_token":"rotated","expires_in":3600}`))}, nil
				})
				manager := NewManager(nil, nil, nil)
				auth := &Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "original", "refresh_token": t.Name()}}
				follower := auth.Clone()
				follower.ID, follower.Index = t.Name()+"-follower", t.Name()+"-follower"
				type result struct {
					auth *Auth
					err  error
				}
				owner, waiter := make(chan result, 1), make(chan result, 1)
				go func() {
					updated, errRefresh := manager.RefreshAuthCredential(context.Background(), auth)
					owner <- result{updated, errRefresh}
				}()
				<-started
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
					updated, errRefresh := manager.RefreshAuthCredential(waiterCtx, follower)
					waiter <- result{updated, errRefresh}
				}()
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
				if len(owner) != 0 {
					t.Error("waiter cancellation ended the owner exchange")
				}
				close(release)
				completed := <-owner
				if !returned {
					stopped = <-waiter
				}
				if !returned || !errors.Is(stopped.err, expected) {
					t.Errorf("credential waiter ignored its context: returned=%t err=%v, want %v", returned, stopped.err, expected)
				}
				if stopped.auth == nil || stopped.auth.Metadata["access_token"] != "original" || stopped.auth.LastRefreshError != nil || stopped.auth.RuntimeRefreshBlocked {
					t.Error("canceled waiter changed credential state")
				}
				if completed.err != nil || completed.auth == nil || completed.auth.Metadata["access_token"] != "renewed" || completed.auth.Metadata["refresh_token"] != "rotated" {
					t.Errorf("owner did not complete rotation: %v", completed.err)
				}
				if calls.Load() != 1 {
					t.Errorf("made %d exchanges, want 1", calls.Load())
				}
			})
		})
	}
}
