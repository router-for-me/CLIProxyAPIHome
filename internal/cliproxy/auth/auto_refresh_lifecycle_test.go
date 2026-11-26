package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// pausedRefreshParent intercepts reuse of the parent during a stale restart.
// WithCancel consults Value when linking or unlinking its cancellation child.
type pausedRefreshParent struct {
	context.Context
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *pausedRefreshParent) Value(key any) any {
	if p.armed.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.release
	}
	return p.Context.Value(key)
}

func TestManagerSetConfigCannotRestartAfterDemotion(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	t.Cleanup(manager.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	parent := &pausedRefreshParent{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(parent.release) })
	defer release()
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
	manager.StartAutoRefresh(parent, time.Minute)

	parent.armed.Store(true)
	reloaded := make(chan struct{})
	go func() {
		manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 2})
		close(reloaded)
	}()
	select {
	case <-parent.entered:
		// Reproduce demotion after a reload captured the old loop but before
		// it publishes a replacement. OnMasterChanged(false) calls this stop.
		manager.StopAutoRefresh()
		release()
	case <-reloaded:
		// In-place resizing must not consult the parent or restart the loop.
		parent.armed.Store(false)
		manager.StopAutoRefresh()
	case <-ctx.Done():
		t.Fatal("configuration reload did not finish")
	}
	select {
	case <-reloaded:
	case <-ctx.Done():
		t.Fatal("configuration reload remained blocked after demotion")
	}
	manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 3})
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.refreshLoop != nil || manager.refreshCancel != nil {
		t.Fatal("configuration reload restarted auto-refresh after demotion")
	}
}

func TestAutoRefreshResizeDrainsRetiringWorkers(t *testing.T) {
	for _, test := range []struct {
		name string
		stop bool
	}{
		{"finish in-flight requests", false},
		{"demotion cancels even retiring workers", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			t.Cleanup(manager.Shutdown)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 2})

			// Drive the workers directly so the scheduler cannot mark the test jobs
			// pending before the full-auth lookup, and completion is channel-controlled.
			workerCtx, cancelRefresh := context.WithCancel(ctx)
			defer cancelRefresh()
			loop := newAuthAutoRefreshLoop(manager, time.Minute, 2)
			loop.workerCtx = workerCtx
			manager.refreshLoop = loop
			manager.refreshCancel = cancelRefresh
			started := make(chan context.Context, 3)
			finished := make(chan error, 3)
			release := make(chan struct{})
			manager.SetAutoRefreshHandler(func(refreshCtx context.Context, _ *Auth) error {
				started <- refreshCtx
				select {
				case <-release:
				case <-refreshCtx.Done():
				}
				errRefresh := refreshCtx.Err()
				finished <- errRefresh
				return errRefresh
			})
			for _, id := range []string{"first", "second", "third"} {
				if _, errRegister := manager.Register(ctx, &Auth{
					ID: id, Index: id, Provider: "kimi", Status: StatusActive,
					Metadata: map[string]any{"refresh_token": "test-refresh"},
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			loop.resizeWorkers(2)
			loop.jobs <- "first"
			loop.jobs <- "second"
			for range 2 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("initial workers did not start")
				}
			}
			loop.mu.Lock()
			retired := loop.workerStops[1]
			loop.mu.Unlock()

			manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 1})
			select {
			case <-retired:
			default:
				t.Fatal("downsizing did not retire the excess worker")
			}
			select {
			case errRefresh := <-finished:
				t.Fatalf("downsizing aborted an active refresh: %v", errRefresh)
			default:
			}

			// Growth must start a new worker even while the retired one is draining.
			manager.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 2})
			loop.jobs <- "third"
			select {
			case refreshCtx := <-started:
				if refreshCtx.Err() != nil {
					t.Fatal("new worker inherited a canceled context")
				}
			case <-ctx.Done():
				t.Fatal("resizing did not start an additional worker")
			}
			if manager.refreshLoop != loop {
				t.Fatal("resizing replaced the scheduler and its queued jobs")
			}
			var wantErr error
			if test.stop {
				wantErr = context.Canceled
				manager.StopAutoRefresh()
			} else {
				close(release)
			}
			for range 3 {
				select {
				case errRefresh := <-finished:
					if !errors.Is(errRefresh, wantErr) {
						t.Fatalf("refresh result = %v, want %v", errRefresh, wantErr)
					}
				case <-ctx.Done():
					t.Fatal("workers did not drain")
				}
			}
		})
	}
}
