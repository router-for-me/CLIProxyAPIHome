package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestAccessGroupDeletionClearsAPIKeyBindings(t *testing.T) {
	for _, kind := range []string{"channel", "model"} {
		t.Run(kind, func(t *testing.T) {
			handler, engine, closeRepo := newAPIKeyManagementRuntimeTestServer(t)
			defer closeRepo()
			ctx := t.Context()
			channel, errChannel := handler.repo.CreateChannelGroup(ctx, "channel", false)
			if errChannel != nil {
				t.Fatal(errChannel)
			}
			model, errModel := handler.repo.CreateModelGroup(ctx, "model", false)
			if errModel != nil {
				t.Fatal(errModel)
			}
			channels, models := []uint{channel.ID}, []uint{model.ID}
			key, errKey := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: "client-key", Channels: &channels, ModelGroups: &models})
			if errKey != nil {
				t.Fatal(errKey)
			}
			engine.DELETE("/channel-groups/:id", handler.DeleteChannelGroup)
			engine.DELETE("/model-groups/:id", handler.DeleteModelGroup)
			groupID, field := channel.ID, "channels"
			if kind == "model" {
				groupID, field = model.ID, "model_groups"
			}
			before := performAPIKeyManagementRequest(t, engine, http.MethodGet, "/api-keys", nil)
			response := performAPIKeyManagementRequest(t, engine, http.MethodDelete, fmt.Sprintf("/%s-groups/%d", kind, groupID), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
			}
			after := performAPIKeyManagementRequest(t, engine, http.MethodGet, "/api-keys", nil)
			var collection struct {
				Entries []apiKeyManagementEntryResponse `json:"api_key_entries"`
			}
			decodeAPIKeyManagementResponse(t, after, &collection)
			if len(collection.Entries) != 1 {
				t.Fatalf("keys = %+v", collection.Entries)
			}
			entry := collection.Entries[0]
			if entry.ID != key.ID || (kind == "channel" && (len(entry.Channels) != 0 || len(entry.ModelGroups) != 1)) || (kind == "model" && (len(entry.ModelGroups) != 0 || len(entry.Channels) != 1)) {
				t.Fatalf("unexpected bindings after deletion: %+v", entry)
			}
			if before.Header().Get("ETag") == after.Header().Get("ETag") {
				t.Fatal("group deletion did not invalidate API key collection ETag")
			}
			for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut} {
				payload := map[string]any{"id": key.ID, "api_key": key.APIKey, field: []uint{groupID}}
				var body any = payload
				if method == http.MethodPost {
					payload["api_key"] = "new-key"
				}
				if method == http.MethodPut {
					body = map[string]any{"api_key_entries": []any{payload}}
				}
				rejected := performAPIKeyManagementRequest(t, engine, method, "/api-keys", body)
				if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "invalid_access_group") {
					t.Fatalf("%s stale binding status=%d body=%s", method, rejected.Code, rejected.Body.String())
				}
			}
		})
	}
}

func TestListChannelGroupDetailsIncludesAuthMetadata(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementRuntimeTestServer(t)
	defer closeRepo()
	ctx := t.Context()

	activeAuth := &coreauth.Auth{
		ID:       "auth-active-1",
		Index:    "auth-active-1",
		Provider: "antigravity",
		Label:    "active@example.com",
		Status:   coreauth.StatusActive,
	}
	deletedAuth := &coreauth.Auth{
		ID:          "auth-deleted-2",
		Index:       "auth-deleted-2",
		Provider:    "codex",
		Status:      coreauth.StatusDisabled,
		Disabled:    true,
		Unavailable: true,
		Metadata:    map[string]any{"email": "deleted@example.com"},
	}
	for _, auth := range []*coreauth.Auth{activeAuth, deletedAuth} {
		if _, errUpsert := handler.repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
		}
	}

	group, errGroup := handler.repo.CreateChannelGroup(ctx, "default", false)
	if errGroup != nil {
		t.Fatalf("CreateChannelGroup() error = %v", errGroup)
	}
	if _, errCreate := handler.repo.CreateChannelGroupDetail(ctx, group.ID, activeAuth.ID); errCreate != nil {
		t.Fatalf("CreateChannelGroupDetail(active) error = %v", errCreate)
	}
	if _, errCreate := handler.repo.CreateChannelGroupDetail(ctx, group.ID, deletedAuth.ID); errCreate != nil {
		t.Fatalf("CreateChannelGroupDetail(deleted) error = %v", errCreate)
	}
	if errRetire := handler.repo.RetireProviderAuth(ctx, deletedAuth.ID); errRetire != nil {
		t.Fatalf("RetireProviderAuth() error = %v", errRetire)
	}

	engine.GET("/channel-group-details", handler.ListChannelGroupDetails)
	resp := performAPIKeyManagementRequest(t, engine, http.MethodGet, "/channel-group-details", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /channel-group-details status = %d body=%s", resp.Code, resp.Body.String())
	}

	var payload struct {
		Details []struct {
			AuthID          string `json:"auth_id"`
			AuthLabel       string `json:"auth_label"`
			AuthProvider    string `json:"auth_provider"`
			AuthStatus      string `json:"auth_status"`
			AuthDisabled    bool   `json:"auth_disabled"`
			AuthUnavailable bool   `json:"auth_unavailable"`
			AuthDeleted     bool   `json:"auth_deleted"`
		} `json:"channel_group_details"`
	}
	decodeAPIKeyManagementResponse(t, resp, &payload)
	if len(payload.Details) != 2 {
		t.Fatalf("details len = %d, want 2 (%+v)", len(payload.Details), payload.Details)
	}
	byID := make(map[string]int, len(payload.Details))
	for i, d := range payload.Details {
		byID[d.AuthID] = i
	}
	activeGot := payload.Details[byID[activeAuth.ID]]
	if activeGot.AuthLabel != "active@example.com" || activeGot.AuthProvider != "antigravity" || activeGot.AuthStatus != "active" || activeGot.AuthDisabled || activeGot.AuthDeleted {
		t.Fatalf("unexpected active detail metadata: %+v", activeGot)
	}
	deletedGot := payload.Details[byID[deletedAuth.ID]]
	if deletedGot.AuthLabel != "deleted@example.com" || deletedGot.AuthProvider != "codex" || deletedGot.AuthStatus != "disabled" || !deletedGot.AuthDisabled || !deletedGot.AuthUnavailable || !deletedGot.AuthDeleted {
		t.Fatalf("unexpected deleted detail metadata: %+v", deletedGot)
	}
}
