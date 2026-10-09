package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestCodexRefreshPersistsPlanTypeAndReloadsModels(t *testing.T) {
	// Keep this test serial because Codex uses the default HTTP transport.
	const authID = "codex-plan-upgrade"
	ctx := context.Background()
	idToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`)) + ".signature"
	body, errMarshal := json.Marshal(map[string]any{
		"id_token":      idToken,
		"access_token":  "new-access-token",
		"refresh_token": "new-refresh-token",
		"expires_in":    3600,
	})
	if errMarshal != nil {
		t.Fatalf("marshal token response: %v", errMarshal)
	}
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != codexauth.TokenURL {
			t.Fatalf("refresh request = %s %s", req.Method, req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}, nil
	})

	repo := newRefreshTestRepository(t)
	auth := &coreauth.Auth{
		ID:         authID,
		Index:      authID,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "free", "note": "keep"},
		Metadata: map[string]any{
			"id_token":      "old-id-token",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
		},
	}
	record, errUpsert := repo.UpsertAuth(ctx, auth, "register")
	if errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	runtime, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	t.Cleanup(runtime.Stop)
	adapter := NewRuntimeAdapter(repo, "")
	runtime.SetClusterAdapter(adapter)
	modelRegistry := registry.GetGlobalRegistry()
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	if errReload := runtime.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("initial ReloadAuths() error = %v", errReload)
	}
	if got := len(modelRegistry.GetModelsForClient(authID)); got != len(registry.GetCodexFreeModels()) {
		t.Fatalf("initial model count = %d, want Free models", got)
	}

	lastSeenID, errMaxEventID := repo.MaxEventID(ctx)
	if errMaxEventID != nil {
		t.Fatalf("MaxEventID() error = %v", errMaxEventID)
	}
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9310, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	if _, errRefresh := controller.RefreshNow(ctx, authID); errRefresh != nil {
		t.Fatalf("RefreshNow() error = %v", errRefresh)
	}

	persisted, refreshedRecord, errAuth := repo.GetAuth(ctx, authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if persisted.Attributes["plan_type"] != "plus" || persisted.Attributes["note"] != "keep" {
		t.Fatalf("persisted attributes = %#v, want Plus and preserved note", persisted.Attributes)
	}
	if persisted.Metadata["id_token"] != idToken || refreshedRecord.Version <= record.Version {
		t.Fatal("refreshed token and revision were not persisted")
	}
	inMemory, ok := runtime.CoreManager().GetByID(authID)
	if !ok || inMemory.Attributes["plan_type"] != "plus" {
		t.Fatal("in-memory plan was not synchronized")
	}

	// Process the same auth event callback as the Home entrypoint without a timer.
	processedAuthEvents := 0
	watcher := NewEventWatcherFrom(repo, 0, lastSeenID, func(eventCtx context.Context, event ClusterEventRecord) error {
		if event.Scope != "auth" {
			return nil
		}
		processedAuthEvents++
		if errApplyEvent := adapter.ApplyEvent(eventCtx, event); errApplyEvent != nil {
			return errApplyEvent
		}
		return runtime.ReloadAuths(eventCtx)
	})
	if errPoll := watcher.poll(ctx); errPoll != nil {
		t.Fatalf("poll auth events: %v", errPoll)
	}
	if processedAuthEvents == 0 {
		t.Fatal("refresh did not publish an auth change event")
	}
	gotModels := make(map[string]bool)
	for _, model := range modelRegistry.GetModelsForClient(authID) {
		gotModels[model.ID] = true
	}
	wantModels := make(map[string]bool)
	for _, model := range registry.GetCodexPlusModels() {
		wantModels[model.ID] = true
	}
	if !reflect.DeepEqual(gotModels, wantModels) {
		t.Fatalf("registered models = %v, want Plus models %v", gotModels, wantModels)
	}
}
