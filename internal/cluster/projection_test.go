package cluster

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestProjectionPreservesCredentialAndExistingSharedTransitions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
	}{
		{name: "none"},
		{name: "weight", metadata: map[string]any{"weight": 5}},
		{name: "retry", metadata: map[string]any{"request_retry": 2}},
		{name: "both", metadata: map[string]any{"weight": 5, "request_retry": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newQuotaTestRepository(t)
			metadata := map[string]any{"type": "codex", "access_token": "access", "refresh_token": "refresh", "unknown": map[string]any{"field": "preserved"}}
			for key, value := range tc.metadata {
				metadata[key] = value
			}
			seed := &coreauth.Auth{ID: "projection", Index: "projection", Provider: "codex", Status: coreauth.StatusActive, Metadata: metadata}
			if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			before, recordBefore, errBefore := repo.GetAuth(ctx, seed.ID)
			if errBefore != nil {
				t.Fatal(errBefore)
			}
			adapter := NewRuntimeAdapter(repo, "127.0.0.1")
			if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
				t.Fatal(errLoad)
			}
			minimal := adapter.ListMinimalAuths()[0]
			if !minimal.RuntimeProjection || minimal.Metadata["access_token"] != nil {
				t.Fatal("index did not produce an explicitly marked secret-free projection")
			}
			if tc.metadata["weight"] != nil && minimal.Metadata["weight"] != float64(5) {
				t.Fatal("weight projection was lost")
			}
			if tc.metadata["request_retry"] != nil {
				if retry, ok := minimal.RequestRetryOverride(); !ok || retry != 2 {
					t.Fatal("request retry projection was lost")
				}
			}
			manager := coreauth.NewManager(adapter, nil, nil)
			t.Cleanup(manager.Shutdown)
			if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), minimal); errRegister != nil {
				t.Fatal(errRegister)
			}
			manager.MarkResult(ctx, coreauth.Result{AuthID: seed.ID, Model: "local", Error: &coreauth.Error{HTTPStatus: http.StatusInternalServerError}})
			manager.MarkResult(ctx, coreauth.Result{AuthID: seed.ID, Model: "clean", Success: true})
			for _, save := range []func() error{
				func() error { _, errSave := adapter.Save(ctx, minimal); return errSave },
				func() error { _, _, errSave := adapter.SaveWithStateVersion(ctx, minimal); return errSave },
				func() error { _, errSave := repo.UpsertAuth(ctx, minimal, "update"); return errSave },
			} {
				if errSave := save(); !errors.Is(errSave, coreauth.ErrRuntimeProjectionPersist) {
					t.Fatalf("full save error = %v, want projection rejection", errSave)
				}
			}
			after, recordAfter, errAfter := repo.GetAuth(ctx, seed.ID)
			if errAfter != nil || recordBefore.Version != recordAfter.Version || !reflect.DeepEqual(before, after) {
				t.Fatal("local projection state overwrote the complete database credential")
			}
			// Existing shared transitions still mutate authoritative records.
			for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
				manager.MarkResult(ctx, coreauth.Result{AuthID: seed.ID, Model: "shared", Error: &coreauth.Error{HTTPStatus: status}})
			}
			after, _, errAfter = repo.GetAuth(ctx, seed.ID)
			if errAfter != nil || after.ModelStates["shared"] == nil || !after.ModelStates["shared"].Quota.Exceeded || !reflect.DeepEqual(before.Metadata, after.Metadata) {
				t.Fatal("shared state mutation lost quota state or credential metadata")
			}
			// Complete saves must still permit token rotation and field deletion.
			after.Metadata["refresh_token"] = "rotated"
			delete(after.Metadata, "unknown")
			if _, errSave := adapter.Save(ctx, after); errSave != nil {
				t.Fatal(errSave)
			}
			full, errFull := adapter.GetFullAuth(ctx, seed.ID)
			if errFull != nil || full.RuntimeProjection || full.Metadata["refresh_token"] != "rotated" || full.Metadata["unknown"] != nil {
				t.Fatal("complete credential save/cache behavior regressed")
			}
		})
	}
}

func TestProjectionPreservesAntigravityCapabilityHintsAndRouteMetadata(t *testing.T) {
	ctx := context.Background()
	repo := newQuotaTestRepository(t)
	hints := map[string]any{
		"endpoint":             "https://daily-cloudcode-pa.googleapis.com",
		"project_id":           "proj-123",
		"model_ids":            []any{"claude-sonnet-4-6", "gemini-3-flash"},
		"web_search_model_ids": []any{"gemini-3-flash"},
	}
	seed := &coreauth.Auth{
		ID:       "antigravity-proj",
		Index:    "antigravity-proj",
		Provider: "antigravity",
		ProxyURL: "http://proxy.example.test:8080",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":                         "antigravity",
			"access_token":                 "secret-access",
			"refresh_token":                "secret-refresh",
			"project_id":                   "proj-123",
			"base_url":                     "https://daily-cloudcode-pa.googleapis.com",
			"user_agent":                   "antigravity/1.104.0 darwin/arm64",
			"antigravity_capability_hints": hints,
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	adapter := NewRuntimeAdapter(repo, "127.0.0.1")
	if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
		t.Fatal(errLoad)
	}
	minimal := adapter.ListMinimalAuths()[0]
	if !minimal.RuntimeProjection || minimal.Metadata["access_token"] != nil || minimal.Metadata["refresh_token"] != nil {
		t.Fatalf("minimal projection leaked secrets: %#v", minimal.Metadata)
	}
	if minimal.ProxyURL != "http://proxy.example.test:8080" {
		t.Fatalf("ProxyURL = %q, want http://proxy.example.test:8080", minimal.ProxyURL)
	}
	if minimal.Metadata["project_id"] != "proj-123" ||
		minimal.Metadata["base_url"] != "https://daily-cloudcode-pa.googleapis.com" ||
		minimal.Metadata["user_agent"] != "antigravity/1.104.0 darwin/arm64" {
		t.Fatalf("minimal projection lost Antigravity route metadata: %#v", minimal.Metadata)
	}
	if !reflect.DeepEqual(minimal.Metadata["antigravity_capability_hints"], hints) {
		t.Fatalf("minimal projection hints = %#v, want %#v", minimal.Metadata["antigravity_capability_hints"], hints)
	}
}
