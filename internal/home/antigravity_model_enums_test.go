package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type antigravityEnumTestAdapter struct {
	*fakeAntigravityClusterAdapter
	kvStore
	key          string
	raw          []byte
	writes       int
	renewed      int
	version      atomic.Int64
	failReserve  bool
	failWrite    bool
	conflictOnce bool
	readAttempts int
}

func (a *antigravityEnumTestAdapter) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	a.readAttempts++
	return a.raw, key == a.key, nil
}

func (a *antigravityEnumTestAdapter) KVIncrBy(_ context.Context, _ string, delta int64) (int64, error) {
	if a.failReserve {
		return 0, fmt.Errorf("sequence unavailable")
	}
	return a.version.Add(delta), nil
}

func (a *antigravityEnumTestAdapter) KVExpire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	if key != a.key || ttl <= 0 {
		return false, nil
	}
	a.renewed++
	return true, nil
}

func (a *antigravityEnumTestAdapter) KVCompareAndSwap(_ context.Context, key string, previous []byte, found bool, raw []byte, ttl time.Duration) (bool, error) {
	if a.failWrite {
		return false, fmt.Errorf("snapshot write unavailable")
	}
	if a.conflictOnce {
		a.conflictOnce = false
		return false, nil
	}
	if found != (key == a.key) || (found && !bytes.Equal(previous, a.raw)) {
		return false, nil
	}
	if ttl <= 0 {
		return false, fmt.Errorf("expected expiring enum snapshot")
	}
	a.key, a.raw = key, raw
	a.writes++
	return true, nil
}

func TestAntigravityModelEnumPublicationRecoversOnScan(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failReserve bool
		conflict    bool
		competing   bool
		wantCalls   int32
		wantModel   string
		wantVersion int64
	}{
		{name: "reservation failure reprobes after competing publication", failReserve: true, competing: true, wantCalls: 3, wantModel: "MODEL_3", wantVersion: 2},
		{name: "write failure retries the same version", wantCalls: 1, wantModel: "MODEL_1", wantVersion: 1},
		{name: "CAS conflict retries the same version", conflict: true, wantCalls: 1, wantModel: "MODEL_1", wantVersion: 1},
		{name: "write retry respects a competing publication", competing: true, wantCalls: 2, wantModel: "MODEL_2", wantVersion: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCapabilityCache()
			t.Cleanup(resetAntigravityCapabilityCache)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"models":{"gemini-2.5-pro":{"model":"MODEL_%d"}}}`, calls.Add(1))
			}))
			t.Cleanup(srv.Close)
			const id = "enum-publication-recovery"
			full := &coreauth.Auth{ID: id, Provider: "antigravity", Status: coreauth.StatusActive, CreatedAt: time.Unix(100, 0),
				Metadata: map[string]any{"access_token": "access", "refresh_token": "grant", "base_url": srv.URL}}
			minimal := full.Clone()
			minimal.RuntimeProjection = true
			delete(minimal.Metadata, "access_token")
			delete(minimal.Metadata, "refresh_token")
			adapter := &antigravityEnumTestAdapter{
				fakeAntigravityClusterAdapter: &fakeAntigravityClusterAdapter{minimal: map[string]*coreauth.Auth{id: minimal}, full: map[string]*coreauth.Auth{id: full}},
				kvStore:                       &runtimeKVTestAdapter{},
				failReserve:                   tc.failReserve,
				failWrite:                     !tc.failReserve && !tc.conflict,
				conflictOnce:                  tc.conflict,
			}
			rt, errRuntime := NewRuntime(&config.Config{})
			if errRuntime != nil {
				t.Fatal(errRuntime)
			}
			rt.SetClusterAdapter(adapter)
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			rt.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(t.Context()), minimal.Clone())
			rt.WaitAntigravityProbes()
			key := rt.antigravityCapabilityKey(full)
			antigravityCapabilityMu.RLock()
			entry := antigravityCapabilityCache[key]
			antigravityCapabilityMu.RUnlock()
			if calls.Load() != 1 || adapter.writes != 0 || entry.hints.revision == 0 || entry.appliedRevision != entry.hints.revision ||
				entry.persistedRevision != entry.hints.revision || !antigravityNowFunc().Before(entry.expiresAt) {
				t.Fatal("fixture did not leave fresh model hints with an unpublished enum snapshot")
			}
			adapter.failReserve, adapter.failWrite = false, false
			if tc.competing {
				// Use a distinct cache route but the same durable scope and KV store.
				other := full.Clone()
				other.ProxyURL = "direct"
				node := &Runtime{cfg: &config.Config{}, clusterAdapter: adapter}
				node.fetchAntigravityModelCapabilityHints(t.Context(), other)
				if adapter.writes != 1 || !strings.Contains(string(adapter.raw), "MODEL_2") {
					t.Fatalf("competing publication missing: %s", adapter.raw)
				}
			}
			beforeScan := adapter.readAttempts
			rt.refreshAntigravityModels(t.Context())
			rt.WaitAntigravityProbes()
			if adapter.readAttempts != beforeScan+1 {
				t.Fatalf("scan made %d snapshot read attempts, want 1", adapter.readAttempts-beforeScan)
			}
			gotVersion := ojson.GetBytes(adapter.raw, "version").Int()
			if gotVersion != tc.wantVersion || adapter.version.Load() != tc.wantVersion || calls.Load() != tc.wantCalls || !strings.Contains(string(adapter.raw), tc.wantModel) {
				t.Fatalf("after scan: snapshot = %s, reserved version = %d, probes = %d; want version %d, probes %d, %s",
					adapter.raw, adapter.version.Load(), calls.Load(), tc.wantVersion, tc.wantCalls, tc.wantModel)
			}
			reads := adapter.readAttempts
			rt.refreshAntigravityModels(t.Context())
			rt.WaitAntigravityProbes()
			if adapter.readAttempts != reads || calls.Load() != tc.wantCalls {
				t.Fatal("a completed or superseded publication was retried again")
			}
		})
	}
}

func TestAntigravityModelEnumsFromRefresh(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)
	var calls, response atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != antigravityModelsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch response.Load() {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			_, _ = fmt.Fprint(w, `{"models":{}}`)
		default:
			_, _ = fmt.Fprint(w, `{"models":{"gemini-2.5-pro":{"model":"MODEL_GOOGLE_GEMINI_2_5_PRO"},"gemini-3.1-flash-lite":{"model":"MODEL_PLACEHOLDER_M50"},"gemini-3-flash":{"model":42}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	const id = "antigravity-model-enums-home"
	full := &coreauth.Auth{ID: id, Index: id, Provider: "antigravity", Status: coreauth.StatusActive,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Metadata:  map[string]any{"access_token": "access-value", "refresh_token": "refresh-value", "project_id": "project", "base_url": srv.URL}}
	minimal := full.Clone()
	minimal.RuntimeProjection = true
	delete(minimal.Metadata, "access_token")
	delete(minimal.Metadata, "refresh_token")
	adapter := &antigravityEnumTestAdapter{
		fakeAntigravityClusterAdapter: &fakeAntigravityClusterAdapter{minimal: map[string]*coreauth.Auth{id: minimal}, full: map[string]*coreauth.Auth{id: full}},
		kvStore:                       &runtimeKVTestAdapter{},
	}
	rt, errRuntime := NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	rt.SetClusterAdapter(adapter)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	rt.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(t.Context()), minimal.Clone())
	rt.WaitAntigravityProbes()
	waitForSnapshot := func(want map[string]string) string {
		t.Helper()
		var state struct {
			Scope  string            `json:"scope"`
			Models map[string]string `json:"models"`
		}
		if errDecode := json.Unmarshal(adapter.raw, &state); errDecode != nil {
			t.Fatal(errDecode)
		}
		wantKey := fmt.Sprintf("antigravity:model-enums:v1:%x:%s", sha256.Sum256([]byte(id)), state.Scope)
		if adapter.key != wantKey {
			t.Fatalf("successful refresh wrote key %q, want %q", adapter.key, wantKey)
		}
		if len(state.Models) != len(want) || state.Scope == "" || state.Models == nil {
			t.Fatalf("bad snapshot: %s", adapter.raw)
		}
		for id, enum := range want {
			if state.Models[id] != enum {
				t.Fatalf("model %q = %q, want %q", id, state.Models[id], enum)
			}
		}
		if strings.Contains(string(adapter.raw), "access-value") || strings.Contains(string(adapter.raw), "refresh-value") {
			t.Fatal("snapshot contains a credential")
		}
		return state.Scope
	}
	want := map[string]string{"gemini-2.5-pro": "MODEL_GOOGLE_GEMINI_2_5_PRO", "gemini-3.1-flash-lite": "MODEL_PLACEHOLDER_M50"}
	scope := waitForSnapshot(want)
	rt.refreshAntigravityModels(t.Context())
	rt.WaitAntigravityProbes()
	if calls.Load() != 1 || adapter.writes != 1 {
		t.Fatalf("initial and cached refreshes: probes = %d, writes = %d", calls.Load(), adapter.writes)
	}
	dispatch, errSanitize := SanitizeAuthForDownstream(full, nil)
	if errSanitize != nil || dispatch.Attributes["antigravity_model_enum_scope"] != scope || dispatch.Metadata["refresh_token"] != nil {
		t.Fatalf("dispatch does not carry only the derived enum scope: error = %v", errSanitize)
	}
	rotated := full.Clone()
	rotated.Metadata["access_token"] = "access-rotated"
	dispatch, errSanitize = SanitizeAuthForDownstream(rotated, nil)
	if errSanitize != nil || dispatch.Attributes["antigravity_model_enum_scope"] != scope {
		t.Fatal("routine access token rotation changed the enum scope")
	}
	rotated.Metadata["refresh_token"] = "new-grant"
	dispatch, errSanitize = SanitizeAuthForDownstream(rotated, nil)
	if errSanitize != nil || dispatch.Attributes["antigravity_model_enum_scope"] == scope {
		t.Fatal("replacement grant reused the enum scope")
	}
	refresh := func(status int32) {
		response.Store(status)
		antigravityCapabilityMu.Lock()
		for key, entry := range antigravityCapabilityCache {
			entry.expiresAt = time.Time{}
			antigravityCapabilityCache[key] = entry
		}
		clear(antigravityAuthFailureCache)
		antigravityCapabilityMu.Unlock()
		rt.executeAntigravityModelRefresh(&antigravityModelRefreshRequest{ctx: t.Context(), auth: minimal, provider: "antigravity"})
		rt.WaitAntigravityProbes()
	}
	refresh(1)
	waitForSnapshot(want)
	if adapter.writes != 1 || adapter.renewed != 1 {
		t.Fatalf("failed refresh: %d writes, %d renewals; want 1 and 1", adapter.writes, adapter.renewed)
	}
	refresh(2)
	waitForSnapshot(map[string]string{})
	if calls.Load() != 3 || adapter.writes != 2 {
		t.Fatalf("probes = %d, writes = %d; want 3 and 2", calls.Load(), adapter.writes)
	}
	adapter.mu.Lock()
	adapter.full[id] = rotated
	adapter.mu.Unlock()
	response.Store(0)
	rt.refreshAntigravityModels(t.Context())
	rt.WaitAntigravityProbes()
	if replaced := waitForSnapshot(want); replaced == scope || calls.Load() != 4 || adapter.writes != 3 {
		t.Fatalf("replacement refresh: reused scope = %v, probes = %d, writes = %d", replaced == scope, calls.Load(), adapter.writes)
	}
}
