package home

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type runtimeKVTestAdapter struct {
	enabled bool

	mu           sync.Mutex
	calls        []string
	purgeCalled  chan struct{}
	purgeSignals int
}

func (a *runtimeKVTestAdapter) Enabled() bool {
	return a != nil && a.enabled
}

func (a *runtimeKVTestAdapter) LoadAuthIndex(ctx context.Context) error {
	a.record("LoadAuthIndex")
	return nil
}

func (a *runtimeKVTestAdapter) ListMinimalAuths() []*coreauth.Auth {
	a.record("ListMinimalAuths")
	return nil
}

func (a *runtimeKVTestAdapter) GetFullAuth(ctx context.Context, uuid string) (*coreauth.Auth, error) {
	a.record("GetFullAuth")
	return nil, errors.New("not implemented")
}

func (a *runtimeKVTestAdapter) LoadConfigYAML(ctx context.Context) ([]byte, error) {
	a.record("LoadConfigYAML")
	return nil, errors.New("not implemented")
}

func (a *runtimeKVTestAdapter) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	a.record("KVGet:" + key)
	return []byte("value"), true, nil
}

func (a *runtimeKVTestAdapter) KVSet(ctx context.Context, key string, value []byte, ttl time.Duration, mode string) (bool, error) {
	a.record("KVSet:" + key + ":" + string(value) + ":" + mode)
	return true, nil
}

func (a *runtimeKVTestAdapter) KVCompareAndSwap(ctx context.Context, key string, expected []byte, expectedExists bool, value []byte, ttl time.Duration) (bool, error) {
	a.record("KVCompareAndSwap:" + key + ":" + string(expected) + ":" + strconv.FormatBool(expectedExists) + ":" + string(value))
	return true, nil
}

func (a *runtimeKVTestAdapter) KVDel(ctx context.Context, keys []string) (int64, error) {
	a.record("KVDel:" + strings.Join(keys, ","))
	return int64(len(keys)), nil
}

func (a *runtimeKVTestAdapter) KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	a.record("KVExpire:" + key)
	return true, nil
}

func (a *runtimeKVTestAdapter) KVTTL(ctx context.Context, key string) (int64, error) {
	a.record("KVTTL:" + key)
	return 30, nil
}

func (a *runtimeKVTestAdapter) KVIncrBy(ctx context.Context, key string, delta int64) (int64, error) {
	a.record("KVIncrBy:" + key)
	return delta + 1, nil
}

func (a *runtimeKVTestAdapter) KVMGet(ctx context.Context, keys []string) ([]KVGetResult, error) {
	a.record("KVMGet:" + strings.Join(keys, ","))
	return []KVGetResult{
		{Value: []byte("first"), Found: true},
		{Found: false},
	}, nil
}

func (a *runtimeKVTestAdapter) KVMSet(ctx context.Context, pairs map[string][]byte) error {
	a.record("KVMSet")
	return nil
}

func (a *runtimeKVTestAdapter) KVPurgeExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	a.record("KVPurgeExpired")
	a.mu.Lock()
	a.purgeSignals++
	ch := a.purgeCalled
	shouldSignal := a.purgeSignals == 1 && ch != nil
	a.mu.Unlock()
	if shouldSignal {
		close(ch)
	}
	return 1, nil
}

func (a *runtimeKVTestAdapter) record(call string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.calls = append(a.calls, call)
	a.mu.Unlock()
}

func (a *runtimeKVTestAdapter) hasCall(prefix string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, call := range a.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func TestRuntimeKVUnavailable(t *testing.T) {
	t.Parallel()

	runtime := &Runtime{}
	_, _, errGet := runtime.KVGet(context.Background(), "key")
	if errGet == nil || !strings.Contains(errGet.Error(), "kv store unavailable") {
		t.Fatalf("KVGet() error = %v, want kv store unavailable", errGet)
	}
	if _, errSet := runtime.KVSet(context.Background(), "key", []byte("value"), 0, ""); errSet == nil || !strings.Contains(errSet.Error(), "kv store unavailable") {
		t.Fatalf("KVSet() error = %v, want kv store unavailable", errSet)
	}
	if _, errCAS := runtime.KVCompareAndSwap(context.Background(), "key", nil, false, []byte("value"), 0); errCAS == nil || !strings.Contains(errCAS.Error(), "kv store unavailable") {
		t.Fatalf("KVCompareAndSwap() error = %v, want kv store unavailable", errCAS)
	}

	runtime.SetClusterAdapter(&runtimeKVTestAdapter{enabled: false})
	_, errDel := runtime.KVDel(context.Background(), []string{"key"})
	if errDel == nil || !strings.Contains(errDel.Error(), "kv store unavailable") {
		t.Fatalf("KVDel() error = %v, want kv store unavailable", errDel)
	}
}

func TestRuntimeKVDelegatesToAdapter(t *testing.T) {
	t.Parallel()

	adapter := &runtimeKVTestAdapter{enabled: true}
	runtime := &Runtime{clusterAdapter: adapter}
	ctx := context.Background()

	if value, found, errGet := runtime.KVGet(ctx, "key"); errGet != nil || !found || string(value) != "value" {
		t.Fatalf("KVGet() = %q, %v, %v, want value, true, nil", value, found, errGet)
	}
	if written, errSet := runtime.KVSet(ctx, "key", []byte("next"), time.Minute, "nx"); errSet != nil || !written {
		t.Fatalf("KVSet() = %v, %v, want true, nil", written, errSet)
	}
	if swapped, errCAS := runtime.KVCompareAndSwap(ctx, "key", []byte("value"), true, []byte("next"), time.Minute); errCAS != nil || !swapped {
		t.Fatalf("KVCompareAndSwap() = %v, %v, want true, nil", swapped, errCAS)
	}
	if deleted, errDel := runtime.KVDel(ctx, []string{"a", "b"}); errDel != nil || deleted != 2 {
		t.Fatalf("KVDel() = %d, %v, want 2, nil", deleted, errDel)
	}
	if ok, errExpire := runtime.KVExpire(ctx, "key", time.Minute); errExpire != nil || !ok {
		t.Fatalf("KVExpire() = %v, %v, want true, nil", ok, errExpire)
	}
	if ttl, errTTL := runtime.KVTTL(ctx, "key"); errTTL != nil || ttl != 30 {
		t.Fatalf("KVTTL() = %d, %v, want 30, nil", ttl, errTTL)
	}
	if value, errIncr := runtime.KVIncrBy(ctx, "counter", 2); errIncr != nil || value != 3 {
		t.Fatalf("KVIncrBy() = %d, %v, want 3, nil", value, errIncr)
	}
	results, errMGet := runtime.KVMGet(ctx, []string{"a", "b"})
	if errMGet != nil {
		t.Fatalf("KVMGet() error = %v", errMGet)
	}
	if len(results) != 2 || !results[0].Found || string(results[0].Value) != "first" || results[1].Found {
		t.Fatalf("KVMGet() = %#v, want first found and second missing", results)
	}
	if errMSet := runtime.KVMSet(ctx, map[string][]byte{"key": []byte("value")}); errMSet != nil {
		t.Fatalf("KVMSet() error = %v", errMSet)
	}

	for _, prefix := range []string{"KVGet:key", "KVSet:key", "KVCompareAndSwap:key:value:true:next", "KVDel:a,b", "KVExpire:key", "KVTTL:key", "KVIncrBy:counter", "KVMGet:a,b", "KVMSet"} {
		if !adapter.hasCall(prefix) {
			t.Fatalf("adapter call %q was not recorded", prefix)
		}
	}
}

func TestRuntimeKVStartPurgesExpiredRows(t *testing.T) {
	adapter := &runtimeKVTestAdapter{
		enabled:     true,
		purgeCalled: make(chan struct{}),
	}
	runtime, errNewRuntime := NewRuntime(&config.Config{})
	if errNewRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errNewRuntime)
	}
	runtime.SetClusterAdapter(adapter)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if errStart := runtime.Start(ctx, ""); errStart != nil {
		t.Fatalf("Start() error = %v", errStart)
	}
	defer runtime.Stop()

	select {
	case <-adapter.purgeCalled:
	case <-time.After(2 * time.Second):
		t.Fatalf("KVPurgeExpired was not called")
	}
}
