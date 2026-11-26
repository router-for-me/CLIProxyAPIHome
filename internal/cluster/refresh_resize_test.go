package cluster

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestRefreshControllerWorkerResizePreservesRefreshLease(t *testing.T) {
	for _, test := range []struct {
		name          string
		initial, next int
	}{
		{"grow", 1, 2},
		{"shrink", 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			repo := newRefreshTestRepository(t)
			auth := newInvalidGrantRefreshAuth("resize-active-refresh")
			auth.Metadata["expires_at"] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
			if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
				t.Fatal(errUpsert)
			}

			started := make(chan context.Context, 1)
			release := make(chan struct{})
			var calls atomic.Int32
			transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					started <- req.Context()
					select {
					case <-release:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)),
				}, nil
			})
			runtime := newRefreshTestRuntime(t, repo, auth, transport)
			manager := runtime.CoreManager()
			manager.SetConfig(&config.Config{AuthAutoRefreshWorkers: test.initial})
			coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9310, Secret: "test-master"}, CoordinatorOptions{})
			markRefreshTestMaster(t, repo, coordinator)
			controller := NewRefreshController(coordinator, runtime, repo, nil)
			finished := make(chan error, 1)
			manager.SetAutoRefreshHandler(func(refreshCtx context.Context, selected *coreauth.Auth) error {
				_, errRefresh := controller.refreshLocalWithLock(refreshCtx, selected.Index, coreauth.AccessTokenSHA256(selected))
				finished <- errRefresh
				return errRefresh
			})
			controller.OnMasterChanged(true)
			defer controller.OnMasterChanged(false)
			var requestCtx context.Context
			select {
			case requestCtx = <-started:
			case <-ctx.Done():
				t.Fatal("background provider refresh did not start")
			}
			leased, _, errLease := repo.GetAuth(ctx, auth.ID)
			if errLease != nil {
				t.Fatal(errLease)
			}
			if !refreshLeaseActive(leased, time.Now().UTC()) {
				t.Fatal("background refresh did not claim a persisted lease")
			}

			manager.SetConfig(&config.Config{AuthAutoRefreshWorkers: test.next})
			if requestCtx.Err() != nil {
				t.Fatalf("worker resize canceled OAuth request: %v", requestCtx.Err())
			}
			close(release)
			select {
			case errRefresh := <-finished:
				if errRefresh != nil {
					t.Fatalf("background refresh failed after resize: %v", errRefresh)
				}
			case <-ctx.Done():
				t.Fatal("background refresh did not finish")
			}
			persisted, _, errAuth := repo.GetAuth(ctx, auth.ID)
			if errAuth != nil {
				t.Fatal(errAuth)
			}
			if !persisted.NextRefreshAfter.IsZero() || persisted.Attributes[refreshLeaseAttribute] != "" || persisted.LastRefreshError != nil {
				t.Fatal("successful refresh retained a lease or retry restriction after resize")
			}
			if _, errManual := controller.RefreshNow(ctx, auth.Index); errManual != nil {
				t.Fatalf("manual refresh was restricted after resize: %v", errManual)
			}
			if calls.Load() != 2 {
				t.Fatalf("provider calls = %d, want background and manual refresh", calls.Load())
			}
		})
	}
}
