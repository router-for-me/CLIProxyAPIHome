package home

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type tosPauseFailureAdapter struct {
	tosPauseTestAdapter
	readErr, writeErr error
}

func (a *tosPauseFailureAdapter) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	if a.readErr != nil {
		return nil, false, a.readErr
	}
	return a.tosPauseTestAdapter.KVGet(ctx, key)
}

func (a *tosPauseFailureAdapter) KVCompareAndSwap(ctx context.Context, key string, expected []byte, exists bool, value []byte, ttl time.Duration) (bool, error) {
	if a.writeErr != nil {
		return false, a.writeErr
	}
	return a.tosPauseTestAdapter.KVCompareAndSwap(ctx, key, expected, exists, value, ttl)
}

func TestAntigravityTOSPausePersistenceFailuresAndConcurrentReplay(t *testing.T) {
	ctx := t.Context()
	adapter := &tosPauseFailureAdapter{tosPauseTestAdapter: tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}}}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(&config.Config{})
	auth := &coreauth.Auth{ID: "pause-failures", Index: "pause-failures", Provider: "antigravity", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	runtime := &Runtime{cfg: &config.Config{Antigravity: config.AntigravityConfig{TOSPauseWindow: "6h"}}, coreManager: manager, clusterAdapter: adapter}
	result := coreauth.NewUsageResult(auth.ID, auth.Provider, "model", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`)
	result.ObservedAt = time.Now().Add(-time.Minute)
	failure := errors.New("injected KV failure")
	adapter.readErr = failure
	if _, errRead := runtime.antigravityTOSPauseDeadline(ctx); !errors.Is(errRead, failure) {
		t.Fatalf("read failure hidden: %v", errRead)
	}
	if errRecord := runtime.recordAntigravityTOSPause(ctx, result, false); !errors.Is(errRecord, failure) {
		t.Fatalf("feedback read failure hidden: %v", errRecord)
	}
	adapter.readErr, adapter.writeErr = nil, failure
	if errRecord := runtime.recordAntigravityTOSPause(ctx, result, false); !errors.Is(errRecord, failure) {
		t.Fatalf("feedback write failure hidden: %v", errRecord)
	}
	adapter.writeErr = nil
	adapter.value = []byte("invalid-deadline")
	var numberError *strconv.NumError
	if _, errRead := runtime.antigravityTOSPauseDeadline(ctx); !errors.As(errRead, &numberError) {
		t.Fatalf("malformed deadline hidden: %v", errRead)
	}
	if errRecord := runtime.recordAntigravityTOSPause(ctx, result, false); !errors.As(errRecord, &numberError) {
		t.Fatalf("malformed stored deadline overwritten: %v", errRecord)
	}
	adapter.value = nil
	var workers sync.WaitGroup
	failures := make(chan error, 16)
	for range cap(failures) {
		workers.Go(func() {
			failures <- runtime.recordAntigravityTOSPause(ctx, result, true)
		})
	}
	workers.Wait()
	close(failures)
	for errRecord := range failures {
		if errRecord != nil {
			t.Fatal(errRecord)
		}
	}
	deadline, errDeadline := runtime.antigravityTOSPauseDeadline(ctx)
	if errDeadline != nil || !deadline.Equal(result.ObservedAt.Add(6*time.Hour)) {
		t.Fatalf("concurrent replay changed the fence: %v, %v", deadline, errDeadline)
	}
	// Untimed legacy replay cannot rearm a cleared/expired pause.
	adapter.value = nil
	result.ObservedAt = time.Time{}
	if errReplay := runtime.recordAntigravityTOSPause(ctx, result, true); errReplay != nil || adapter.value != nil {
		t.Fatalf("untimed replay armed a pause: %v", errReplay)
	}
}
