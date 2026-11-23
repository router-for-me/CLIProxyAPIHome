package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
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
	if key != a.key || ttl != antigravityModelEnumTTL {
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
	if ttl != antigravityModelEnumTTL {
		return false, fmt.Errorf("unexpected enum snapshot TTL: %v", ttl)
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
				call := calls.Add(1)
				_, _ = fmt.Fprintf(w, `{
					"models":{"gemini-test-high":{"model":"MODEL_%d","maxOutputTokens":%d}},
					"groupedModels":{"gemini-test":{"supportedThinkingLevels":[{"thinkingSuffix":"high"}]}}
				}`, call, 500+call)
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
			if limit := ojson.GetBytes(adapter.raw, "thinkingGroups.gemini-test.high").Int(); limit != 500+int64(tc.wantCalls) {
				t.Fatalf("retried or superseded publication has wrong thinking limit: %s", adapter.raw)
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
	const (
		legacyResponse int32 = iota
		groupedResponse
		failedResponse
		groupedResponseWithoutSearch
		emptyResponse
	)
	const modelCatalog = `{
		"gemini-9.9-test-low":{"model":"MODEL_LOW","maxOutputTokens":512},
		"gemini-9.9-test-high":{"model":"MODEL_HIGH"},
		"gemini-9.9-test-very-high":{"model":"MODEL_VERY_HIGH","maxOutputTokens":1024},
		"gemini-9.9-test-tiered":{"model":"MODEL_TIERED","maxOutputTokens":2048},
		"gemini-9.9-test-neighbor-high":{"maxOutputTokens":768},
		"gemini-9.9-test--leading":{"maxOutputTokens":512},
		"gemini-9.9-test-trailing-":{"maxOutputTokens":512},
		"gemini-9.9-test-double--dash":{"maxOutputTokens":512},
		"gemini-9.9-test-under_score":{"maxOutputTokens":512},
		"gemini-9.9-test-padded":{"maxOutputTokens":512},
		"gemini-9.9-test-élevé":{"maxOutputTokens":512},
		"gemini-9.9-test-scalar":42,
		"gemini-9.9-test-zero":{"maxOutputTokens":0},
		"gemini-9.9-test-negative":{"maxOutputTokens":-1},
		"gemini-9.9-test-decimal":{"maxOutputTokens":1.5},
		"gemini-9.9-test-exponent":{"maxOutputTokens":1e3},
		"gemini-9.9-test-quoted":{"maxOutputTokens":"512"},
		"gemini-9.9-test-overflow":{"maxOutputTokens":9223372036854775808},
		"gemini-9.9-test-nolimit":{"model":42},
		"gemini-9.9-test-CASE":{"maxOutputTokens":512},
		"gemini-9.9-test-incomplete-high":{"maxOutputTokens":256}
	}`
	const groupCatalog = `{
		"gemini-9.9-test":{"supportedThinkingLevels":[
			{"thinkingSuffix":"low"},{"thinkingSuffix":"HIGH"},{"thinkingSuffix":"very-high"},
			{"thinkingSuffix":"-leading"},{"thinkingSuffix":"trailing-"},{"thinkingSuffix":"double--dash"},
			{"thinkingSuffix":"under_score"},{"thinkingSuffix":" padded "},{"thinkingSuffix":"élevé"},{"thinkingSuffix":"scalar"},
			{"thinkingSuffix":"zero"},{"thinkingSuffix":"negative"},{"thinkingSuffix":"decimal"},
			{"thinkingSuffix":"exponent"},{"thinkingSuffix":"quoted"},{"thinkingSuffix":"overflow"},
			{"thinkingSuffix":"nolimit"},{"thinkingSuffix":"absent"},{"thinkingSuffix":"case"},
			{"thinkingSuffix":42},"tiered",{},null
		]},
		"gemini-9.9-test-neighbor":{"supportedThinkingLevels":[{"thinkingSuffix":"high"}]},
		"gemini-9.9-test-incomplete":{"supportedThinkingLevels":{"thinkingSuffix":"high"}},
		"gemini-9.9-unknown":{"supportedThinkingLevels":[{"thinkingSuffix":"high","maxOutputTokens":512}]}
	}`
	const webSearchCatalog = `["gemini-9.9-test-low","gemini-9.9-test-high","gemini-9.9-test-tiered","gemini-9.9-test-absent","Gemini-9.9-test-very-high"]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != antigravityModelsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch response.Load() {
		case groupedResponse:
			_, _ = fmt.Fprintf(w, `{"models":%s,"groupedModels":%s,"webSearchModelIds":%s}`, modelCatalog, groupCatalog, webSearchCatalog)
		case failedResponse:
			w.WriteHeader(http.StatusServiceUnavailable)
		case groupedResponseWithoutSearch:
			_, _ = fmt.Fprintf(w, `{"models":%s,"groupedModels":%s}`, modelCatalog, groupCatalog)
		case emptyResponse:
			_, _ = fmt.Fprint(w, `{"models":{}}`)
		default:
			_, _ = fmt.Fprintf(w, `{"models":%s,"webSearchModelIds":%s}`, modelCatalog, webSearchCatalog)
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
	checkSnapshot := func(models map[string]string, groups map[string]map[string]int, version int64) string {
		t.Helper()
		// A legacy reader only decodes the original fields.
		var state struct {
			Scope   string            `json:"scope"`
			Models  map[string]string `json:"models"`
			Version int64             `json:"version"`
		}
		if errDecode := json.Unmarshal(adapter.raw, &state); errDecode != nil {
			t.Fatal(errDecode)
		}
		wantKey := fmt.Sprintf("antigravity:model-enums:v1:%x:%s", sha256.Sum256([]byte(id)), state.Scope)
		if adapter.key != wantKey {
			t.Fatalf("successful refresh wrote key %q, want %q", adapter.key, wantKey)
		}
		if state.Scope == "" || state.Version != version || state.Models == nil || !maps.Equal(state.Models, models) {
			t.Fatalf("bad v1 snapshot: %s", adapter.raw)
		}
		groupField := ojson.GetBytes(adapter.raw, "thinkingGroups")
		if groups == nil {
			if groupField.Exists() {
				t.Fatalf("expected absent thinkingGroups, got %s", adapter.raw)
			}
		} else {
			var got map[string]map[string]int
			if errDecode := json.Unmarshal([]byte(groupField.Raw), &got); errDecode != nil || !maps.EqualFunc(got, groups, maps.Equal) {
				t.Fatalf("thinkingGroups = %s, want %v (error = %v)", groupField.Raw, groups, errDecode)
			}
		}
		if strings.Contains(string(adapter.raw), "access-value") || strings.Contains(string(adapter.raw), "refresh-value") {
			t.Fatal("snapshot contains a credential")
		}
		return state.Scope
	}
	checkWebSearch := func(want map[string]bool) {
		t.Helper()
		field := ojson.GetBytes(adapter.raw, "thinkingWebSearch")
		if want == nil {
			if field.Exists() {
				t.Fatalf("expected absent thinkingWebSearch, got %s", field.Raw)
			}
			return
		}
		var got map[string]bool
		if errDecode := json.Unmarshal([]byte(field.Raw), &got); errDecode != nil || !maps.Equal(got, want) {
			t.Fatalf("thinkingWebSearch = %s, want %v (error = %v)", field.Raw, want, errDecode)
		}
	}
	wantModels := map[string]string{
		"gemini-9.9-test-low":       "MODEL_LOW",
		"gemini-9.9-test-high":      "MODEL_HIGH",
		"gemini-9.9-test-very-high": "MODEL_VERY_HIGH",
		"gemini-9.9-test-tiered":    "MODEL_TIERED",
	}
	wantGroups := map[string]map[string]int{
		"gemini-9.9-test": {
			"low": 512, "high": 0, "very-high": 1024,
			"zero": 0, "negative": 0, "decimal": 0, "exponent": 0, "quoted": 0, "overflow": 0, "nolimit": 0,
		},
		"gemini-9.9-test-neighbor": {"high": 768},
	}
	scope := checkSnapshot(wantModels, nil, 1)
	checkWebSearch(nil)
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
	refresh(groupedResponse)
	if checkSnapshot(wantModels, wantGroups, 2) != scope {
		t.Fatal("adding groups changed the credential scope")
	}
	checkWebSearch(map[string]bool{"gemini-9.9-test-low": true, "gemini-9.9-test-high": true})
	previous := bytes.Clone(adapter.raw)
	refresh(failedResponse)
	if !bytes.Equal(previous, adapter.raw) || adapter.writes != 2 || adapter.renewed != 1 {
		t.Fatalf("failed refresh changed the snapshot or failed to renew it: %d writes, %d renewals", adapter.writes, adapter.renewed)
	}
	refresh(groupedResponseWithoutSearch)
	checkSnapshot(wantModels, wantGroups, 4)
	checkWebSearch(nil)
	refresh(legacyResponse)
	if checkSnapshot(wantModels, nil, 5) != scope {
		t.Fatal("removing groups changed the credential scope")
	}
	checkWebSearch(nil)
	refresh(emptyResponse)
	checkSnapshot(map[string]string{}, nil, 6)
	if calls.Load() != 6 || adapter.writes != 5 {
		t.Fatalf("probes = %d, writes = %d; want 6 and 5", calls.Load(), adapter.writes)
	}
	adapter.mu.Lock()
	adapter.full[id] = rotated
	adapter.mu.Unlock()
	response.Store(legacyResponse)
	rt.refreshAntigravityModels(t.Context())
	rt.WaitAntigravityProbes()
	if replaced := checkSnapshot(wantModels, nil, 7); replaced == scope || calls.Load() != 7 || adapter.writes != 6 {
		t.Fatalf("replacement refresh: reused scope = %v, probes = %d, writes = %d", replaced == scope, calls.Load(), adapter.writes)
	}
}
