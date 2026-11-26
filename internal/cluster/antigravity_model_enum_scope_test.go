package cluster

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const antigravityGroupedModelCatalog = `{
	"models":{"gemini-test-high":{"model":"MODEL_TEST_HIGH","maxOutputTokens":512}},
	"groupedModels":{"gemini-test":{"supportedThinkingLevels":[{"thinkingSuffix":"high"}]}}
}`

func TestAntigravityModelEnumScopeFencesRestoredCredential(t *testing.T) {
	ctx := t.Context()
	repo := newCredentialFoundationTestRepository(t)
	adapter := NewRuntimeAdapter(repo, "127.0.0.1")
	const id = "48d2e093-0c78-40d3-afbe-dd777be83d49"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, antigravityGroupedModelCatalog)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	auth := &coreauth.Auth{ID: id, Index: id, Provider: "antigravity", Status: coreauth.StatusActive,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Metadata:  map[string]any{"access_token": "access", "refresh_token": "refresh", "project_id": "project", "base_url": srv.URL}}
	if _, errInsert := repo.UpsertAuth(ctx, auth, "create"); errInsert != nil {
		t.Fatal(errInsert)
	}
	dispatchScope := func() string {
		t.Helper()
		full, errGet := adapter.GetFullAuth(ctx, id)
		if errGet != nil {
			t.Fatal(errGet)
		}
		dispatch, errSanitize := home.SanitizeAuthForDownstream(full, nil)
		if errSanitize != nil || dispatch == nil || dispatch.Attributes["antigravity_model_enum_scope"] == "" {
			t.Fatalf("missing dispatch scope (error = %v)", errSanitize)
		}
		return dispatch.Attributes["antigravity_model_enum_scope"]
	}
	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	rt.SetClusterAdapter(adapter)
	if errReload := rt.ReloadAuths(ctx); errReload != nil {
		t.Fatal(errReload)
	}
	rt.WaitAntigravityProbes()
	old := dispatchScope()
	readSnapshot := func(scope string) ([]byte, bool) {
		t.Helper()
		key := fmt.Sprintf("antigravity:model-enums:v1:%x:%s", sha256.Sum256([]byte(id)), scope)
		value, found, errKV := repo.KVGet(ctx, key)
		if errKV != nil {
			t.Fatal(errKV)
		}
		return value, found
	}
	if snapshot, found := readSnapshot(old); !found || ojson.GetBytes(snapshot, "thinkingGroups.gemini-test.high").Int() != 512 {
		t.Fatalf("original credential has no thinking group snapshot: %s", snapshot)
	}
	if errDelete := adapter.Delete(ctx, id); errDelete != nil {
		t.Fatal(errDelete)
	}
	if _, errRestore := repo.UpsertAuth(ctx, auth, "create"); errRestore != nil {
		t.Fatal(errRestore)
	}
	if errIndex := adapter.RefreshAuthIndex(ctx, id); errIndex != nil {
		t.Fatal(errIndex)
	}
	restored := dispatchScope()
	if restored == old {
		t.Fatal("same-ID restoration inherited the original enum scope")
	}
	if _, found := readSnapshot(restored); found {
		t.Fatal("restored credential can read a snapshot before its first probe")
	}
	rotated := auth.Clone()
	rotated.Metadata["access_token"] = "rotated"
	if _, errSave := adapter.Save(ctx, rotated); errSave != nil {
		t.Fatal(errSave)
	}
	if dispatchScope() != restored {
		t.Fatal("saving a new access token lost the restored credential identity")
	}
}

func TestAntigravityModelEnumsWithProjectAliasesInDatabase(t *testing.T) {
	for _, alias := range []string{"projectId", "project"} {
		t.Run(alias, func(t *testing.T) {
			ctx := t.Context()
			repo := newCredentialFoundationTestRepository(t)
			adapter := NewRuntimeAdapter(repo, "127.0.0.1")
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = fmt.Fprint(w, antigravityGroupedModelCatalog)
			}))
			t.Cleanup(srv.Close)
			id := "project-alias-" + alias
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			credential := &coreauth.Auth{ID: id, Index: id, Provider: "antigravity", Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "access", "refresh_token": "refresh", alias: "my-project", "base_url": srv.URL}}
			if _, errInsert := repo.UpsertAuth(ctx, credential, "create"); errInsert != nil {
				t.Fatal(errInsert)
			}
			rt, errRuntime := home.NewRuntime(&config.Config{})
			if errRuntime != nil {
				t.Fatal(errRuntime)
			}
			rt.SetClusterAdapter(adapter)
			if errReload := rt.ReloadAuths(ctx); errReload != nil {
				t.Fatal(errReload)
			}
			rt.WaitAntigravityProbes()
			full, errFull := adapter.GetFullAuth(ctx, id)
			if errFull != nil {
				t.Fatal(errFull)
			}
			dispatch, errSanitize := home.SanitizeAuthForDownstream(full, nil)
			if errSanitize != nil {
				t.Fatal(errSanitize)
			}
			key := fmt.Sprintf("antigravity:model-enums:v1:%x:%s", sha256.Sum256([]byte(id)), dispatch.Attributes["antigravity_model_enum_scope"])
			value, found, errKV := repo.KVGet(ctx, key)
			if errKV != nil || !found || ojson.GetBytes(value, "models.gemini-test-high").Str != "MODEL_TEST_HIGH" || requests.Load() != 1 {
				t.Fatalf("alias %s: mapping found = %v, probes = %d, error = %v", alias, found, requests.Load(), errKV)
			}
			if limit := ojson.GetBytes(value, "thinkingGroups.gemini-test.high").Int(); limit != 512 {
				t.Fatalf("alias %s: missing thinking group in database snapshot: %s", alias, value)
			}
		})
	}
}
