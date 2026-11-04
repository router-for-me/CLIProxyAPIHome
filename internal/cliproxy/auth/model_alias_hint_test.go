package auth

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func configModels(entries ...map[string]any) map[string]any {
	items := make([]any, 0, len(entries))
	for _, entry := range entries {
		entry["user_defined"] = true
		items = append(items, entry)
	}
	return map[string]any{homeConfigModelsMetadataKey: items}
}

func registerAliasHintAuth(t *testing.T, manager *Manager, auth *Auth, models ...string) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.SetModelAliasHints(auth, models)
}

func TestAliasesForUpstreamModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	registerAliasHintAuth(t, manager, &Auth{
		ID: t.Name() + "-1", Provider: "codex", Attributes: map[string]string{"api_key": "test"},
		Metadata: configModels(
			map[string]any{"id": "GPT-6.1 Sol", "name": "gpt-6.1-sol"},
			map[string]any{"id": "gpt-6-astra", "name": "gpt-6-astra"},
		),
	}, "GPT-6.1 Sol", "gpt-6-astra")
	registerAliasHintAuth(t, manager, &Auth{
		ID: t.Name() + "-2", Provider: "codex", Attributes: map[string]string{"api_key": "test"},
		Metadata: configModels(
			map[string]any{"id": "sol", "name": "gpt-6.1-sol"},
			map[string]any{"id": "gpt-6.1 sol", "name": "gpt-6.1-sol"},
		),
	}, "sol", "gpt-6.1 sol")

	tests := []struct {
		name  string
		model string
		want  []string
	}{
		{name: "upstream name behind aliases", model: "gpt-6.1-sol", want: []string{"GPT-6.1 Sol", "sol"}},
		{name: "case and thinking suffix", model: "GPT-6.1-SOL(high)", want: []string{"GPT-6.1 Sol", "sol"}},
		{name: "alias same as name", model: "gpt-6-astra"},
		{name: "requested by alias", model: "sol"},
		{name: "unknown model", model: "gpt-unknown"},
		{name: "empty", model: "  "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := manager.AliasesForUpstreamModel(test.model, nil); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("AliasesForUpstreamModel(%q) = %v, want %v", test.model, got, test.want)
			}
		})
	}
}

func TestAliasesForUpstreamModelFiltersBeforeCap(t *testing.T) {
	var nilManager *Manager
	if got := nilManager.AliasesForUpstreamModel("up", nil); got != nil {
		t.Fatalf("nil manager = %v, want nil", got)
	}
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test"},
		Metadata: configModels(
			map[string]any{"id": "d", "name": "up"},
			map[string]any{"id": "a", "name": "up"},
			map[string]any{"id": "c", "name": "up"},
			map[string]any{"id": "b", "name": "up"},
		),
	}
	registerAliasHintAuth(t, manager, auth, "a", "b", "c", "d")
	if got, want := manager.AliasesForUpstreamModel("up", nil), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("capped aliases = %v, want %v", got, want)
	}
	tests := []struct {
		name   string
		access *ModelAccess
		want   []string
	}{
		{name: "model scope", access: NewModelAccess(nil, []string{"d"}, nil), want: []string{"d"}},
		{name: "credential scope", access: NewModelAccess([]string{"other"}, nil, nil)},
		{name: "empty model scope", access: NewModelAccess(nil, []string{}, nil)},
		{name: "alias channel denied", access: NewModelAccess(nil, []string{"d"}, map[string][]string{"d": {"other"}})},
		{name: "alias channel allowed", access: NewModelAccess(nil, []string{"d"}, map[string][]string{"d": {auth.ID}, "up": {}}), want: []string{"d"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := manager.AliasesForUpstreamModel("up", test.access); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("scoped aliases = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAliasesForUpstreamModelTracksPublicationAndRemoval(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test"},
		Metadata: configModels(map[string]any{"id": "friendly", "name": "up"}),
	}
	registerAliasHintAuth(t, manager, auth, "friendly")
	for _, status := range []Status{StatusDisabled, StatusActive} {
		auth.Status = status
		if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		got := manager.AliasesForUpstreamModel("up", nil)
		if (len(got) > 0) != (status == StatusActive) {
			t.Fatalf("aliases with status %s = %v", status, got)
		}
	}
	auth.Disabled = true
	if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if got := manager.AliasesForUpstreamModel("up", nil); len(got) != 0 {
		t.Fatalf("disabled credential exposes aliases: %v", got)
	}
	auth.Disabled = false
	if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	if got := manager.AliasesForUpstreamModel("up", nil); len(got) != 0 {
		t.Fatalf("unpublished alias exposed: %v", got)
	}
	if errDelete := manager.Delete(context.Background(), auth.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	if got := manager.modelAliasHints.lookup("up"); len(got) != 0 {
		t.Fatalf("deleted credential remains indexed: %v", got)
	}
}

type aliasHintMetadataProbe struct{ calls atomic.Int64 }

func (p *aliasHintMetadataProbe) MarshalJSON() ([]byte, error) {
	p.calls.Add(1)
	return []byte(`[{"id":"friendly","name":"up","user_defined":true}]`), nil
}

func TestAliasesForUpstreamModelDoesNotDecodeMetadataOnLookup(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	probe := &aliasHintMetadataProbe{}
	auth := &Auth{
		ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test"},
		Metadata: map[string]any{homeConfigModelsMetadataKey: probe},
	}
	registerAliasHintAuth(t, manager, auth, "friendly")
	probe.calls.Store(0)
	if got := manager.AliasesForUpstreamModel("up", nil); !reflect.DeepEqual(got, []string{"friendly"}) {
		t.Fatalf("indexed aliases = %v", got)
	}
	if got := manager.AliasesForUpstreamModel("missing", nil); len(got) != 0 {
		t.Fatalf("unknown model aliases = %v", got)
	}
	if got := probe.calls.Load(); got != 0 {
		t.Fatalf("lookup decoded model metadata %d times", got)
	}
}

func TestModelAliasHint(t *testing.T) {
	if got := ModelAliasHint("gpt-6.1-sol", nil); got != "" {
		t.Fatalf("no aliases = %q, want empty", got)
	}
	if got, want := ModelAliasHint("gpt-6.1-sol", []string{"GPT-6.1 Sol"}),
		`model "gpt-6.1-sol" is configured under the alias "GPT-6.1 Sol"; request that model ID or clear the alias`; got != want {
		t.Fatalf("single alias = %q, want %q", got, want)
	}
	if got, want := ModelAliasHint(" up ", []string{"a", "b"}),
		`model "up" is configured under the aliases "a", "b"; request that model ID or clear the alias`; got != want {
		t.Fatalf("multiple aliases = %q, want %q", got, want)
	}
}

func TestAliasesForUpstreamModelConcurrentPublication(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test"},
		Metadata: configModels(
			map[string]any{"id": "first", "name": "up"},
			map[string]any{"id": "second", "name": "up"},
		),
	}
	registerAliasHintAuth(t, manager, auth, "first", "second")
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for range 100 {
			manager.SetModelAliasHints(auth, []string{"first"})
			manager.SetModelAliasHints(auth, []string{"second"})
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		policy := NewModelAccess(nil, []string{"second"}, nil)
		for range 200 {
			for _, alias := range manager.AliasesForUpstreamModel("up", policy) {
				if alias != "second" {
					t.Errorf("concurrent publication exposed a forbidden alias: %q", alias)
				}
			}
		}
	}()
	close(start)
	workers.Wait()
}

func BenchmarkAliasesForUpstreamModelMiss(b *testing.B) {
	for _, credentials := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("credentials_%d_models_20", credentials), func(b *testing.B) {
			manager := &Manager{auths: make(map[string]*Auth, credentials)}
			for index := 0; index < credentials; index++ {
				authID := fmt.Sprintf("auth-%d", index)
				entries := make([]modelAliasHintEntry, 0, 20)
				for model := 0; model < 20; model++ {
					entries = append(entries, modelAliasHintEntry{upstream: fmt.Sprintf("up-%d", model), alias: fmt.Sprintf("alias-%d", model)})
				}
				manager.auths[authID] = &Auth{ID: authID}
				manager.modelAliasHints.replace(authID, entries)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				manager.AliasesForUpstreamModel("missing-model", nil)
			}
		})
	}
}
