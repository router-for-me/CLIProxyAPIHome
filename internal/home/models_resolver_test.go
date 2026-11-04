package home

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestDispatchModelInfoForAuthUsesSelectedHomeCapabilities(t *testing.T) {
	const (
		authID  = "dispatch-model-info-antigravity-auth"
		modelID = "gemini-3.8-flash-high"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "antigravity", []*registry.ModelInfo{{
		ID:                         modelID,
		Type:                       "gemini",
		ContextLength:              1048576,
		SupportConfigurationUpdate: true,
		Thinking: &registry.ThinkingSupport{
			Levels: []string{"low", "medium", "high"},
		},
	}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	got := dispatchModelInfoForAuth(authID, modelID+"(medium)", modelID)
	if got == nil {
		t.Fatal("dispatch model info = nil")
	}
	if got.ID != modelID || got.ContextLength != 1048576 {
		t.Fatalf("dispatch model info = %#v, want %s with 1048576 context", got, modelID)
	}
	if got.Thinking == nil || !reflect.DeepEqual(got.Thinking.Levels, []string{"low", "medium", "high"}) {
		t.Fatalf("dispatch thinking levels = %#v, want low/medium/high", got.Thinking)
	}
	if !got.SupportConfigurationUpdate {
		t.Fatalf("selected registry capability lost in dispatch: %+v", got)
	}
}

func TestResolveConfigGeminiKeyEntryUsesConfigIdentity(t *testing.T) {
	entries := []config.GeminiKey{
		{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "one", ProxyURL: "http://proxy-one.test", Models: []config.GeminiModel{{Name: "upstream-one"}}},
		{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "two", ProxyURL: "http://proxy-two.test", Models: []config.GeminiModel{{Name: "upstream-two"}}},
		{BaseURL: "https://header-auth.example.test", Prefix: "header", Models: []config.GeminiModel{{Name: "upstream-header"}}},
		{APIKey: "shared-key", BaseURL: "https://other.example.test", Prefix: "two", ProxyURL: "http://proxy-two.test", Models: []config.GeminiModel{{Name: "upstream-other"}}},
	}
	tests := []struct {
		name      string
		auth      *coreauth.Auth
		wantModel string
	}{
		{
			name: "config index",
			auth: &coreauth.Auth{Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "1",
			}},
			wantModel: "upstream-two",
		},
		{
			name: "prefix and proxy fallback",
			auth: &coreauth.Auth{Prefix: "two", ProxyURL: "http://proxy-two.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test",
			}},
			wantModel: "upstream-two",
		},
		{
			name: "indexed credential mismatch",
			auth: &coreauth.Auth{Prefix: "header", Attributes: map[string]string{
				"base_url": "https://header-auth.example.test", "config_index": "1",
			}},
			wantModel: "upstream-header",
		},
		{
			name: "indexed base URL mismatch",
			auth: &coreauth.Auth{Prefix: "two", ProxyURL: "http://proxy-two.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "3",
			}},
			wantModel: "upstream-two",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := resolveConfigGeminiKeyEntry(tc.auth, entries)
			if entry == nil || len(entry.Models) != 1 || entry.Models[0].Name != tc.wantModel {
				t.Fatalf("resolved entry = %#v, want model %q", entry, tc.wantModel)
			}
		})
	}
}

type fakeAntigravityClusterAdapter struct {
	mu      sync.Mutex
	minimal map[string]*coreauth.Auth
	full    map[string]*coreauth.Auth
	mutates int
}

func (f *fakeAntigravityClusterAdapter) Enabled() bool { return true }

func (f *fakeAntigravityClusterAdapter) LoadAuthIndex(context.Context) error { return nil }

func (f *fakeAntigravityClusterAdapter) ListMinimalAuths() []*coreauth.Auth {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(f.minimal))
	for _, a := range f.minimal {
		out = append(out, a.Clone())
	}
	return out
}

func (f *fakeAntigravityClusterAdapter) GetFullAuth(_ context.Context, uuid string) (*coreauth.Auth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.full[uuid]; ok && a != nil {
		return a.Clone(), nil
	}
	return nil, nil
}

func (f *fakeAntigravityClusterAdapter) LoadConfigYAML(context.Context) ([]byte, error) {
	return nil, nil
}

func (f *fakeAntigravityClusterAdapter) MutateAuthState(_ context.Context, id string, mutate func(*coreauth.Auth) bool) (*coreauth.Auth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.full[id]
	if !ok || current == nil {
		return nil, nil
	}
	clone := current.Clone()
	if !mutate(clone) {
		return nil, nil
	}
	f.mutates++
	f.full[id] = clone.Clone()
	if minAuth, exists := f.minimal[id]; exists && minAuth != nil {
		minClone := minAuth.Clone()
		if minClone.Metadata == nil {
			minClone.Metadata = make(map[string]any)
		}
		minClone.Metadata[antigravityPersistedHintsMetaKey] = clone.Metadata[antigravityPersistedHintsMetaKey]
		f.minimal[id] = minClone
	}
	return f.minimal[id].Clone(), nil
}

func TestAntigravityDynamicModelFetchIntersectionAndPersistence(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)

	const wantDefaultUA = "antigravity/hub/2.29.0 (aidev_client; os_type=darwin; arch=arm64; cl=1013060670)"

	var (
		gotUA      string
		gotAuth    string
		gotProject string
		requests   atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != antigravityModelsPath {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotProject = payload["project"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": {
				"claude-sonnet-4-6": {},
				"gemini-3-flash": {},
				"unknown-upstream-only-model": {}
			},
			"webSearchModelIds": ["claude-sonnet-4-6", "gemini-3-flash"]
		}`))
	}))
	defer srv.Close()

	const authID = "ag-dynamic-auth-1"
	fullAuth := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "tok-live-123",
			"project_id":   "proj-abc",
			"base_url":     srv.URL,
		},
	}
	minAuth := &coreauth.Auth{
		ID:                authID,
		Index:             authID,
		Provider:          "antigravity",
		Status:            coreauth.StatusActive,
		RuntimeProjection: true,
		Metadata: map[string]any{
			"project_id": "proj-abc",
			"base_url":   srv.URL,
		},
	}
	adapter := &fakeAntigravityClusterAdapter{
		minimal: map[string]*coreauth.Auth{authID: minAuth},
		full:    map[string]*coreauth.Auth{authID: fullAuth},
	}

	rt, errRuntime := NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	rt.SetClusterAdapter(adapter)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
		rt.WaitAntigravityProbes()
	})

	rt.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(context.Background()), minAuth.Clone())
	rt.WaitAntigravityProbes()

	if requests.Load() != 1 {
		t.Fatalf("probe requests = %d, want 1", requests.Load())
	}
	if gotUA != wantDefaultUA {
		t.Fatalf("User-Agent = %q, want %q", gotUA, wantDefaultUA)
	}
	if gotAuth != "Bearer tok-live-123" {
		t.Fatalf("Authorization = %q, want Bearer tok-live-123", gotAuth)
	}
	if gotProject != "proj-abc" {
		t.Fatalf("project = %q, want proj-abc", gotProject)
	}

	registered := registry.GetGlobalRegistry().GetModelsForClient(authID)
	gotIDs := make([]string, 0, len(registered))
	webSearchByID := make(map[string]bool, len(registered))
	for _, m := range registered {
		gotIDs = append(gotIDs, m.ID)
		webSearchByID[m.ID] = m.NativeCapabilities != nil && m.NativeCapabilities.WebSearch != nil && *m.NativeCapabilities.WebSearch
	}
	slices.Sort(gotIDs)
	wantIDs := []string{"claude-sonnet-4-6", "gemini-3-flash"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("registered intersected models = %v, want %v", gotIDs, wantIDs)
	}
	if !webSearchByID["claude-sonnet-4-6"] || !webSearchByID["gemini-3-flash"] {
		t.Fatalf("expected web search enabled on intersected models, got %#v", webSearchByID)
	}

	adapter.mu.Lock()
	persistedRaw := adapter.full[authID].Metadata[antigravityPersistedHintsMetaKey]
	mutates := adapter.mutates
	persistedMinimal := adapter.minimal[authID].Clone()
	adapter.mu.Unlock()
	if mutates != 1 || persistedRaw == nil {
		t.Fatalf("expected persisted hints mutation (mutates=%d, raw=%#v)", mutates, persistedRaw)
	}

	// Simulate process restart with cold in-memory cache but persisted hints on the minimal projection.
	resetAntigravityCapabilityCache()
	registry.GetGlobalRegistry().UnregisterClient(authID)
	now := time.Now()
	prevNow := antigravityNowFunc
	antigravityNowFunc = func() time.Time { return now }
	t.Cleanup(func() { antigravityNowFunc = prevNow })

	// Pre-populate cache expiry or verify synchronous registration from persisted hints before any new probe runs.
	hints := rt.cachedAntigravityHints(persistedMinimal)
	if hints.ModelIDs == nil || len(hints.ModelIDs) != 3 {
		t.Fatalf("expected persisted hints to seed cache from minimal projection, got %#v", hints)
	}
	seededModels := rt.antigravityModelsForHints(persistedMinimal, hints)
	if len(seededModels) != 2 {
		t.Fatalf("expected 2 intersected models from seeded hints, got %#v", seededModels)
	}
}

func TestAntigravityAuthoritativeEmptyAndTransientFailureRetention(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)

	now := time.Unix(1_700_000_000, 0)
	prevNow := antigravityNowFunc
	antigravityNowFunc = func() time.Time { return now }
	t.Cleanup(func() { antigravityNowFunc = prevNow })

	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 0:
			_, _ = w.Write([]byte(`{"models":{"gemini-3-flash":{}},"webSearchModelIds":["gemini-3-flash"]}`))
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"unavailable"}`))
		case 2:
			_, _ = w.Write([]byte(`{"models":{}}`))
		}
	}))
	defer srv.Close()

	const authID = "ag-empty-or-fail-auth"
	auth := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "tok-state",
			"project_id":   "proj-state",
			"base_url":     srv.URL,
		},
	}

	rt, errRuntime := NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
		rt.WaitAntigravityProbes()
	})

	rt.applyCoreAuthAddOrUpdate(coreauth.WithSkipPersist(context.Background()), auth.Clone())
	rt.WaitAntigravityProbes()
	if got := registry.GetGlobalRegistry().GetModelsForClient(authID); len(got) != 1 || got[0].ID != "gemini-3-flash" {
		t.Fatalf("initial models = %#v, want [gemini-3-flash]", got)
	}

	// Expire TTL and return 503: previous catalog must be retained.
	now = now.Add(antigravityCapabilityCacheTTL + time.Minute)
	mode.Store(1)
	rt.probeAndRegisterAntigravityModels(context.Background(), auth, "antigravity")
	if got := registry.GetGlobalRegistry().GetModelsForClient(authID); len(got) != 1 || got[0].ID != "gemini-3-flash" {
		t.Fatalf("models after transient 503 = %#v, want retained [gemini-3-flash]", got)
	}

	// Advance past failure backoff and return authoritative empty {"models":{}}: models must be cleared while candidate list is non-nil empty.
	now = now.Add(antigravityCapabilityRetryMax + time.Minute)
	mode.Store(2)
	rt.probeAndRegisterAntigravityModels(context.Background(), auth, "antigravity")
	if got := registry.GetGlobalRegistry().GetModelsForClient(authID); len(got) != 0 {
		t.Fatalf("models after authoritative empty = %#v, want empty", got)
	}
	if candidates := registry.GetGlobalRegistry().GetModelCandidatesForClient(authID); candidates == nil || len(candidates) != 0 {
		t.Fatalf("candidates after authoritative empty = %#v, want non-nil empty slice", candidates)
	}
}
