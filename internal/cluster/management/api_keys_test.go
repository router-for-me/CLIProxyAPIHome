package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

type apiKeyManagementEntryResponse struct {
	ID          uint    `json:"id"`
	APIKey      string  `json:"api_key"`
	DisplayName *string `json:"display_name"`
	UserID      *uint   `json:"user_id"`
	Channels    []uint  `json:"channels"`
	ModelGroups []uint  `json:"model_groups"`
}

func TestAPIKeyDisplayNameManagementCRUDPreservesKeyIdentityAndBindings(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	ctx := t.Context()
	username := "display-name-user"
	user, errUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username})
	if errUser != nil {
		t.Fatalf("CreateUser() error = %v", errUser)
	}
	channel, errChannel := handler.repo.CreateChannelGroup(ctx, "display-name-channel", false)
	if errChannel != nil {
		t.Fatalf("CreateChannelGroup() error = %v", errChannel)
	}
	modelGroup, errModelGroup := handler.repo.CreateModelGroup(ctx, "display-name-models", false)
	if errModelGroup != nil {
		t.Fatalf("CreateModelGroup() error = %v", errModelGroup)
	}

	createResponse := performAPIKeyManagementRequest(t, engine, http.MethodPost, "/api-keys", map[string]any{
		"api_key":      "client-key-1",
		"display_name": "  Production key  ",
		"user_id":      user.ID,
		"channels":     []uint{channel.ID},
		"model_groups": []uint{modelGroup.ID},
	})
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", createResponse.Code, createResponse.Body.String())
	}
	created := decodeAPIKeyMutationResponse(t, createResponse)
	assertAPIKeyManagementEntry(t, created, "client-key-1", "Production key", user.ID, []uint{channel.ID}, []uint{modelGroup.ID})

	patchResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id":           created.ID,
		"display_name": "  Primary production key  ",
	})
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", patchResponse.Code, patchResponse.Body.String())
	}
	patched := decodeAPIKeyMutationResponse(t, patchResponse)
	if patched.ID != created.ID {
		t.Fatalf("patched id = %d, want %d", patched.ID, created.ID)
	}
	assertAPIKeyManagementEntry(t, patched, "client-key-1", "Primary production key", user.ID, []uint{channel.ID}, []uint{modelGroup.ID})

	valid, errValidate := handler.repo.ValidateAPIKey(ctx, "client-key-1")
	if errValidate != nil {
		t.Fatalf("ValidateAPIKey() error = %v", errValidate)
	}
	if !valid {
		t.Fatal("original API key no longer authenticates after display-name-only update")
	}

	listResponse := performAPIKeyManagementRequest(t, engine, http.MethodGet, "/api-keys", nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var listPayload struct {
		Keys    []string                        `json:"api-keys"`
		Items   []apiKeyManagementEntryResponse `json:"items"`
		Entries []apiKeyManagementEntryResponse `json:"api_key_entries"`
	}
	decodeAPIKeyManagementResponse(t, listResponse, &listPayload)
	if !reflect.DeepEqual(listPayload.Keys, []string{"client-key-1"}) {
		t.Fatalf("compatibility keys = %#v, want raw key unchanged", listPayload.Keys)
	}
	if len(listPayload.Items) != 1 || len(listPayload.Entries) != 1 {
		t.Fatalf("list items/entries = %d/%d, want 1/1", len(listPayload.Items), len(listPayload.Entries))
	}
	assertAPIKeyManagementEntry(t, listPayload.Items[0], "client-key-1", "Primary production key", user.ID, []uint{channel.ID}, []uint{modelGroup.ID})

	clearResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id":           created.ID,
		"display_name": "   ",
	})
	if clearResponse.Code != http.StatusOK {
		t.Fatalf("clear status = %d body=%s", clearResponse.Code, clearResponse.Body.String())
	}
	cleared := decodeAPIKeyMutationResponse(t, clearResponse)
	if cleared.DisplayName != nil {
		t.Fatalf("cleared display_name = %q, want null", *cleared.DisplayName)
	}
	assertAPIKeyManagementEntryBindings(t, cleared, "client-key-1", user.ID, []uint{channel.ID}, []uint{modelGroup.ID})

	restoreResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id":           created.ID,
		"display_name": "Temporary name",
	})
	if restoreResponse.Code != http.StatusOK {
		t.Fatalf("restore-name status = %d body=%s", restoreResponse.Code, restoreResponse.Body.String())
	}
	nullResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id":           created.ID,
		"display_name": nil,
	})
	if nullResponse.Code != http.StatusOK {
		t.Fatalf("null-name status = %d body=%s", nullResponse.Code, nullResponse.Body.String())
	}
	nullCleared := decodeAPIKeyMutationResponse(t, nullResponse)
	if nullCleared.DisplayName != nil {
		t.Fatalf("null-cleared display_name = %q, want null", *nullCleared.DisplayName)
	}
	assertAPIKeyManagementEntryBindings(t, nullCleared, "client-key-1", user.ID, []uint{channel.ID}, []uint{modelGroup.ID})
}

func TestPutAPIKeysPreservesOmittedDisplayNameAndClearsExplicitNull(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	displayName := "Compatibility key"
	record, errCreate := handler.repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{
		APIKey:         "compat-client-key",
		DisplayName:    &displayName,
		DisplayNameSet: true,
	})
	if errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}

	omittedResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
		"api_key_entries": []map[string]any{{"api_key": "compat-client-key"}},
	})
	if omittedResponse.Code != http.StatusOK {
		t.Fatalf("omitted-name PUT status = %d body=%s", omittedResponse.Code, omittedResponse.Body.String())
	}
	omitted := loadSingleAPIKeyEntry(t, handler.repo)
	if omitted.ID != record.ID || omitted.DisplayName == nil || *omitted.DisplayName != displayName {
		t.Fatalf("omitted-name PUT entry = %#v, want id %d and display name %q", omitted, record.ID, displayName)
	}
	legacyResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", []string{"compat-client-key"})
	if legacyResponse.Code != http.StatusOK {
		t.Fatalf("legacy string PUT status = %d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}
	legacy := loadSingleAPIKeyEntry(t, handler.repo)
	if legacy.ID != record.ID || legacy.DisplayName == nil || *legacy.DisplayName != displayName {
		t.Fatalf("legacy string PUT entry = %#v, want id %d and display name %q", legacy, record.ID, displayName)
	}

	renamedDisplayName := "Renamed by PUT"
	renameResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
		"api_key_entries": []map[string]any{{"api_key": "compat-client-key", "display_name": renamedDisplayName}},
	})
	if renameResponse.Code != http.StatusOK {
		t.Fatalf("renamed-name PUT status = %d body=%s", renameResponse.Code, renameResponse.Body.String())
	}
	renamed := loadSingleAPIKeyEntry(t, handler.repo)
	if renamed.ID != record.ID || renamed.DisplayName == nil || *renamed.DisplayName != renamedDisplayName {
		t.Fatalf("renamed-name PUT entry = %#v, want same id and display name %q", renamed, renamedDisplayName)
	}

	nullResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
		"api_key_entries": []map[string]any{{"api_key": "compat-client-key", "display_name": nil}},
	})
	if nullResponse.Code != http.StatusOK {
		t.Fatalf("null-name PUT status = %d body=%s", nullResponse.Code, nullResponse.Body.String())
	}
	cleared := loadSingleAPIKeyEntry(t, handler.repo)
	if cleared.ID != record.ID || cleared.DisplayName != nil {
		t.Fatalf("null-name PUT entry = %#v, want same id and cleared display name", cleared)
	}

	invalidResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id":           record.ID,
		"display_name": 42,
	})
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid display_name status = %d body=%s, want 400", invalidResponse.Code, invalidResponse.Body.String())
	}

	for _, invalidName := range []string{strings.Repeat("名", cluster.APIKeyDisplayNameMaxLength+1), "invalid\nname", "\tinvalid"} {
		postResponse := performAPIKeyManagementRequest(t, engine, http.MethodPost, "/api-keys", map[string]any{
			"api_key": "invalid-name-key", "display_name": invalidName,
		})
		if postResponse.Code != http.StatusBadRequest {
			t.Fatalf("invalid POST display_name status = %d body=%s, want 400", postResponse.Code, postResponse.Body.String())
		}
		patchResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
			"id": record.ID, "display_name": invalidName,
		})
		if patchResponse.Code != http.StatusBadRequest {
			t.Fatalf("invalid PATCH display_name status = %d body=%s, want 400", patchResponse.Code, patchResponse.Body.String())
		}
		putResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
			"api_key_entries": []map[string]any{{"api_key": "compat-client-key", "display_name": invalidName}},
		})
		if putResponse.Code != http.StatusBadRequest {
			t.Fatalf("invalid PUT display_name status = %d body=%s, want 400", putResponse.Code, putResponse.Body.String())
		}
		entry := loadSingleAPIKeyEntry(t, handler.repo)
		if entry.ID != record.ID || entry.APIKey != record.APIKey || entry.DisplayName != nil {
			t.Fatalf("rejected display_name mutated API key entry: %#v", entry)
		}
	}
}

func TestPutAPIKeysPreservesOmittedRuntimeBindingsAndClearsExplicitUserID(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	ctx := t.Context()
	username := "put-presence-user"
	user, errUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username})
	if errUser != nil {
		t.Fatalf("CreateUser() error = %v", errUser)
	}
	channel, errChannel := handler.repo.CreateChannelGroup(ctx, "put-presence-channel", false)
	if errChannel != nil {
		t.Fatalf("CreateChannelGroup() error = %v", errChannel)
	}
	modelGroup, errModelGroup := handler.repo.CreateModelGroup(ctx, "put-presence-models", false)
	if errModelGroup != nil {
		t.Fatalf("CreateModelGroup() error = %v", errModelGroup)
	}
	displayName := "Before PUT"
	channels := []uint{channel.ID}
	modelGroups := []uint{modelGroup.ID}
	record, errCreate := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{
		APIKey:         "put-presence-key",
		DisplayName:    &displayName,
		DisplayNameSet: true,
		UserID:         &user.ID,
		UserIDSet:      true,
		Channels:       &channels,
		ModelGroups:    &modelGroups,
	})
	if errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}

	renameResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
		"api_key_entries": []map[string]any{{"api_key": record.APIKey, "display_name": "After PUT"}},
	})
	if renameResponse.Code != http.StatusOK {
		t.Fatalf("name-only PUT status = %d body=%s", renameResponse.Code, renameResponse.Body.String())
	}
	renamed := loadSingleAPIKeyEntry(t, handler.repo)
	if renamed.ID != record.ID || renamed.DisplayName == nil || *renamed.DisplayName != "After PUT" {
		t.Fatalf("name-only PUT entry = %#v, want stable ID and renamed display name", renamed)
	}
	if renamed.UserID == nil || *renamed.UserID != user.ID || !reflect.DeepEqual(renamed.Channels, channels) || !reflect.DeepEqual(renamed.ModelGroups, modelGroups) {
		t.Fatalf("name-only PUT changed runtime bindings: %#v", renamed)
	}
	valid, errValidate := handler.repo.ValidateAPIKey(ctx, record.APIKey)
	if errValidate != nil || !valid {
		t.Fatalf("ValidateAPIKey() = %t, %v; want original key accepted", valid, errValidate)
	}

	for _, explicitUserID := range []any{nil, float64(0)} {
		if _, errRebind := handler.repo.UpdateAPIKeyBindings(ctx, record.APIKey, &user.ID, nil, nil); errRebind != nil {
			t.Fatalf("rebind user before clearing with %v: %v", explicitUserID, errRebind)
		}
		clearResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
			"api_key_entries": []map[string]any{{"api_key": record.APIKey, "user_id": explicitUserID}},
		})
		if clearResponse.Code != http.StatusOK {
			t.Fatalf("clear user_id %v PUT status = %d body=%s", explicitUserID, clearResponse.Code, clearResponse.Body.String())
		}
		cleared := loadSingleAPIKeyEntry(t, handler.repo)
		if cleared.UserID != nil {
			t.Fatalf("cleared user_id = %v, want nil for %v", cleared.UserID, explicitUserID)
		}
		if !reflect.DeepEqual(cleared.Channels, channels) || !reflect.DeepEqual(cleared.ModelGroups, modelGroups) {
			t.Fatalf("clearing user_id changed group bindings: %#v", cleared)
		}
	}
}

func TestPatchAPIKeysNestedNullClearsUserID(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	ctx := t.Context()
	username := "nested-null-user"
	user, errUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username})
	if errUser != nil {
		t.Fatalf("CreateUser() error = %v", errUser)
	}
	record, errCreate := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: "nested-null-key", UserID: &user.ID, UserIDSet: true})
	if errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}

	response := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id":    record.ID,
		"value": map[string]any{"api_key": record.APIKey, "user_id": nil},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("nested null PATCH status = %d body=%s", response.Code, response.Body.String())
	}
	updated := decodeAPIKeyMutationResponse(t, response)
	if updated.UserID != nil || updated.ID != record.ID || updated.APIKey != record.APIKey {
		t.Fatalf("nested null PATCH entry = %#v, want same key with no user", updated)
	}
}

func TestAPIKeyRuntimeRefreshOnlyForRuntimeChanges(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementRuntimeTestServer(t)
	defer closeRepo()
	initialRuntimeConfig := handler.runtime.Config()

	createResponse := performAPIKeyManagementRequest(t, engine, http.MethodPost, "/api-keys", map[string]any{"api_key": "runtime-refresh-key"})
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", createResponse.Code, createResponse.Body.String())
	}
	created := decodeAPIKeyMutationResponse(t, createResponse)
	runtimeConfigAfterCreate := handler.runtime.Config()
	if runtimeConfigAfterCreate == initialRuntimeConfig {
		t.Fatal("runtime config was not refreshed after API key creation")
	}
	eventsAfterCreate := apiKeyManagementEventCount(t, handler.repo)

	nameResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id": created.ID, "display_name": "Runtime metadata only",
	})
	if nameResponse.Code != http.StatusOK {
		t.Fatalf("name-only PATCH status = %d body=%s", nameResponse.Code, nameResponse.Body.String())
	}
	if got := apiKeyManagementEventCount(t, handler.repo); got != eventsAfterCreate {
		t.Fatalf("config events after name-only PATCH = %d, want %d", got, eventsAfterCreate)
	}
	if got := handler.runtime.Config(); got != runtimeConfigAfterCreate {
		t.Fatal("runtime config was refreshed after display-name-only PATCH")
	}
	putNameResponse := performAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", map[string]any{
		"api_key_entries": []map[string]any{{"api_key": created.APIKey, "display_name": "PUT metadata only"}},
	})
	if putNameResponse.Code != http.StatusOK {
		t.Fatalf("name-only PUT status = %d body=%s", putNameResponse.Code, putNameResponse.Body.String())
	}
	if got := apiKeyManagementEventCount(t, handler.repo); got != eventsAfterCreate {
		t.Fatalf("config events after name-only PUT = %d, want %d", got, eventsAfterCreate)
	}
	if got := handler.runtime.Config(); got != runtimeConfigAfterCreate {
		t.Fatal("runtime config was refreshed after display-name-only PUT")
	}

	rotateResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id": created.ID, "new": "runtime-refresh-key-rotated",
	})
	if rotateResponse.Code != http.StatusOK {
		t.Fatalf("key rotation PATCH status = %d body=%s", rotateResponse.Code, rotateResponse.Body.String())
	}
	if got := apiKeyManagementEventCount(t, handler.repo); got != eventsAfterCreate+1 {
		t.Fatalf("config events after key rotation = %d, want %d", got, eventsAfterCreate+1)
	}
	runtimeConfigAfterRotation := handler.runtime.Config()
	if runtimeConfigAfterRotation == runtimeConfigAfterCreate {
		t.Fatal("runtime config was not refreshed after API key rotation")
	}
	validOld, errValidateOld := handler.repo.ValidateAPIKey(t.Context(), "runtime-refresh-key")
	validNew, errValidateNew := handler.repo.ValidateAPIKey(t.Context(), "runtime-refresh-key-rotated")
	if errValidateOld != nil || errValidateNew != nil || validOld || !validNew {
		t.Fatalf("rotated key validation old=%t/%v new=%t/%v", validOld, errValidateOld, validNew, errValidateNew)
	}

	bindingResponse := performAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", map[string]any{
		"id": created.ID, "channels": []uint{101},
	})
	if bindingResponse.Code != http.StatusOK {
		t.Fatalf("binding PATCH status = %d body=%s", bindingResponse.Code, bindingResponse.Body.String())
	}
	if got := apiKeyManagementEventCount(t, handler.repo); got != eventsAfterCreate+2 {
		t.Fatalf("config events after binding update = %d, want %d", got, eventsAfterCreate+2)
	}
	if got := handler.runtime.Config(); got == runtimeConfigAfterRotation {
		t.Fatal("runtime config was not refreshed after API key binding update")
	}
}

func TestPutAPIKeysRejectsMalformedReplacementWithoutDeletingExistingKeys(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	if _, errCreate := handler.repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{APIKey: "preserved-key"}); errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}
	malformedBodies := []string{
		`null`,
		`[null]`,
		`[{}]`,
		`[{"display_name":"name only"}]`,
		`[""]`,
		`{"items":null}`,
		`[42]`,
		`[true]`,
		`{"items":[],"api_key_entries":[]}`,
		`[{"api_key":"one","key":"two"}]`,
		`[{"api_key":""}]`,
		`[{"api_key":"","key":"fallback-key"}]`,
		`[{"api_key":"preserved-key","user_id":1,"user-id":2}]`,
		`[{"api_key":"preserved-key","model_groups":[1],"model-groups":[2]}]`,
	}
	for _, body := range malformedBodies {
		response := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", body, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("malformed PUT %s status = %d body=%s, want 400", body, response.Code, response.Body.String())
		}
		entry := loadSingleAPIKeyEntry(t, handler.repo)
		if entry.APIKey != "preserved-key" {
			t.Fatalf("malformed PUT %s changed entry to %#v", body, entry)
		}
	}
	compatibilityResponse := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", `[{"api_key":"preserved-key","future_metadata":"ignored"}]`, nil)
	if compatibilityResponse.Code != http.StatusOK {
		t.Fatalf("unknown metadata compatibility PUT status = %d body=%s", compatibilityResponse.Code, compatibilityResponse.Body.String())
	}

	for _, body := range []string{`[]`, `{"items":[]}`} {
		response := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", body, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("explicit empty PUT %s status = %d body=%s, want 200", body, response.Code, response.Body.String())
		}
		entries, errEntries := handler.repo.ListAPIKeyEntries(t.Context())
		if errEntries != nil || len(entries) != 0 {
			t.Fatalf("entries after explicit empty PUT = %#v, %v; want empty", entries, errEntries)
		}
		if _, errRestore := handler.repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{APIKey: "preserved-key"}); errRestore != nil {
			t.Fatalf("restore API key after explicit empty PUT: %v", errRestore)
		}
	}
}

func TestAPIKeyDisplayNameErrorsUseStableEnvelope(t *testing.T) {
	_, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	requests := []struct {
		method string
		body   string
	}{
		{method: http.MethodPost, body: `{"api_key":"post-key","display_name":42}`},
		{method: http.MethodPut, body: `[{"api_key":"put-key","display_name":42}]`},
		{method: http.MethodPatch, body: `{"api_key":"missing-key","display_name":42}`},
		{method: http.MethodPatch, body: `{"api_key":"missing-key","value":{"api_key":"missing-key","display_name":42}}`},
	}
	for _, request := range requests {
		response := performRawAPIKeyManagementRequest(t, engine, request.method, "/api-keys", request.body, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s display_name type error status = %d body=%s", request.method, response.Code, response.Body.String())
		}
		var envelope struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		decodeAPIKeyManagementResponse(t, response, &envelope)
		if envelope.Error != "invalid_display_name" || envelope.Message == "" {
			t.Fatalf("%s display_name error = %#v", request.method, envelope)
		}
	}
}

func TestPatchAPIKeysRejectsConflictingSelectorAliases(t *testing.T) {
	_, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	for _, body := range []string{
		`{"api_key":"one","key":"two","display_name":"name"}`,
		`{"id":1,"api_key_id":2,"display_name":"name"}`,
		`{"api_key":"one","user_id":1,"user-id":2}`,
		`{"api_key":"one","model_groups":[1],"model-groups":[2]}`,
	} {
		response := performRawAPIKeyManagementRequest(t, engine, http.MethodPatch, "/api-keys", body, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("conflicting PATCH %s status = %d body=%s, want 400", body, response.Code, response.Body.String())
		}
	}
}

func TestAPIKeyRequestBodyLimit(t *testing.T) {
	_, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	tooLargeName := strings.Repeat("a", int(apiKeyMaxRequestBodySize))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		body := fmt.Sprintf(`{"api_key":"limited-key","display_name":%q}`, tooLargeName)
		if method == http.MethodPut {
			body = "[" + body + "]"
		}
		response := performRawAPIKeyManagementRequest(t, engine, method, "/api-keys", body, nil)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s oversized body status = %d body=%s, want 413", method, response.Code, response.Body.String())
		}
	}
}

func TestAPIKeyCollectionETagGuardsFullReplacement(t *testing.T) {
	handler, engine, closeRepo := newAPIKeyManagementTestServer(t)
	defer closeRepo()

	if _, errCreate := handler.repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{APIKey: "first-key"}); errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}
	getResponse := performRawAPIKeyManagementRequest(t, engine, http.MethodGet, "/api-keys", "", nil)
	etag := getResponse.Header().Get("ETag")
	if getResponse.Code != http.StatusOK || etag == "" {
		t.Fatalf("GET status = %d ETag=%q body=%s", getResponse.Code, etag, getResponse.Body.String())
	}
	if cacheControl := getResponse.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("GET Cache-Control = %q, want no-store", cacheControl)
	}

	success := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", `["first-key"]`, map[string]string{"If-Match": etag})
	if success.Code != http.StatusOK {
		t.Fatalf("matching If-Match PUT status = %d body=%s", success.Code, success.Body.String())
	}
	for _, invalidIfMatch := range []string{"*", "W/" + etag, strings.Trim(etag, `"`)} {
		response := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", `["first-key"]`, map[string]string{"If-Match": invalidIfMatch})
		if response.Code != http.StatusPreconditionFailed {
			t.Fatalf("invalid If-Match %q status = %d body=%s, want 412", invalidIfMatch, response.Code, response.Body.String())
		}
	}
	emptyIfMatch := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", `["first-key"]`, map[string]string{"If-Match": ""})
	if emptyIfMatch.Code != http.StatusPreconditionFailed {
		t.Fatalf("empty If-Match status = %d body=%s, want 412", emptyIfMatch.Code, emptyIfMatch.Body.String())
	}
	multipleIfMatchRequest := httptest.NewRequest(http.MethodPut, "/api-keys", strings.NewReader(`["first-key"]`))
	multipleIfMatchRequest.Header.Set("Content-Type", "application/json")
	multipleIfMatchRequest.Header.Add("If-Match", `"stale"`)
	multipleIfMatchRequest.Header.Add("If-Match", etag)
	multipleIfMatch := httptest.NewRecorder()
	engine.ServeHTTP(multipleIfMatch, multipleIfMatchRequest)
	if multipleIfMatch.Code != http.StatusOK {
		t.Fatalf("multi-line matching If-Match status = %d body=%s, want 200", multipleIfMatch.Code, multipleIfMatch.Body.String())
	}
	concurrentResponse := performRawAPIKeyManagementRequest(t, engine, http.MethodPost, "/api-keys", `{"api_key":"concurrent-key"}`, nil)
	if concurrentResponse.Code != http.StatusCreated {
		t.Fatalf("concurrent POST status = %d body=%s", concurrentResponse.Code, concurrentResponse.Body.String())
	}

	stale := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", `["first-key"]`, map[string]string{"If-Match": etag})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match PUT status = %d body=%s, want 412", stale.Code, stale.Body.String())
	}
	entries, errEntries := handler.repo.ListAPIKeyEntries(t.Context())
	if errEntries != nil || len(entries) != 2 {
		t.Fatalf("entries after stale PUT = %#v, %v; want both keys", entries, errEntries)
	}

	legacy := performRawAPIKeyManagementRequest(t, engine, http.MethodPut, "/api-keys", `["first-key"]`, nil)
	if legacy.Code != http.StatusOK {
		t.Fatalf("unconditional legacy PUT status = %d body=%s", legacy.Code, legacy.Body.String())
	}
	entry := loadSingleAPIKeyEntry(t, handler.repo)
	if entry.APIKey != "first-key" {
		t.Fatalf("legacy PUT entry = %#v", entry)
	}
}

func newAPIKeyManagementTestServer(t *testing.T) (*Handler, *gin.Engine, func()) {
	return newAPIKeyManagementTestServerWithRuntime(t, nil)
}

func newAPIKeyManagementRuntimeTestServer(t *testing.T) (*Handler, *gin.Engine, func()) {
	t.Helper()
	runtime, errRuntime := home.NewRuntime(&appconfig.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatalf("home.NewRuntime() error = %v", errRuntime)
	}
	return newAPIKeyManagementTestServerWithRuntime(t, runtime)
}

func newAPIKeyManagementTestServerWithRuntime(t *testing.T, runtime *home.Runtime) (*Handler, *gin.Engine, func()) {
	t.Helper()
	db, errOpen := cluster.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	repo := cluster.NewRepository(db)
	if runtime != nil {
		if _, errEnsure := repo.EnsureLifecycleConfig(t.Context(), cluster.DefaultHeartbeatTimeout()); errEnsure != nil {
			closeRepo()
			t.Fatalf("EnsureLifecycleConfig() error = %v", errEnsure)
		}
	}
	handler := NewHandler(repo, runtime, "127.0.0.1", 0)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/api-keys", handler.GetAPIKeys)
	engine.POST("/api-keys", handler.PostAPIKeys)
	engine.PUT("/api-keys", handler.PutAPIKeys)
	engine.PATCH("/api-keys", handler.PatchAPIKeys)
	engine.DELETE("/api-keys", handler.DeleteAPIKeys)
	return handler, engine, closeRepo
}

func performRawAPIKeyManagementRequest(t *testing.T, engine *gin.Engine, method string, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func apiKeyManagementEventCount(t *testing.T, repo *cluster.Repository) int64 {
	t.Helper()
	maxID, errMaxID := repo.MaxEventID(t.Context())
	if errMaxID != nil {
		t.Fatalf("MaxEventID() error = %v", errMaxID)
	}
	return maxID
}

func performAPIKeyManagementRequest(t *testing.T, engine *gin.Engine, method string, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("")
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			t.Fatalf("marshal request body: %v", errMarshal)
		}
		body = strings.NewReader(string(raw))
	}
	request := httptest.NewRequest(method, path, body)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func decodeAPIKeyMutationResponse(t *testing.T, response *httptest.ResponseRecorder) apiKeyManagementEntryResponse {
	t.Helper()
	var payload struct {
		APIKey apiKeyManagementEntryResponse `json:"api_key"`
	}
	decodeAPIKeyManagementResponse(t, response, &payload)
	return payload.APIKey
}

func decodeAPIKeyManagementResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if errDecode := json.Unmarshal(response.Body.Bytes(), target); errDecode != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), errDecode)
	}
}

func loadSingleAPIKeyEntry(t *testing.T, repo *cluster.Repository) cluster.APIKeyEntry {
	t.Helper()
	entries, errList := repo.ListAPIKeyEntries(t.Context())
	if errList != nil {
		t.Fatalf("ListAPIKeyEntries() error = %v", errList)
	}
	if len(entries) != 1 {
		t.Fatalf("API key entries = %d, want 1", len(entries))
	}
	return entries[0]
}

func assertAPIKeyManagementEntry(t *testing.T, entry apiKeyManagementEntryResponse, apiKey string, displayName string, userID uint, channels []uint, modelGroups []uint) {
	t.Helper()
	if entry.DisplayName == nil || *entry.DisplayName != displayName {
		t.Fatalf("display_name = %v, want %q", entry.DisplayName, displayName)
	}
	assertAPIKeyManagementEntryBindings(t, entry, apiKey, userID, channels, modelGroups)
}

func assertAPIKeyManagementEntryBindings(t *testing.T, entry apiKeyManagementEntryResponse, apiKey string, userID uint, channels []uint, modelGroups []uint) {
	t.Helper()
	if entry.ID == 0 || entry.APIKey != apiKey {
		t.Fatalf("entry identity = id:%d key:%q, want non-zero id and %q", entry.ID, entry.APIKey, apiKey)
	}
	if entry.UserID == nil || *entry.UserID != userID {
		t.Fatalf("user_id = %v, want %d", entry.UserID, userID)
	}
	if !reflect.DeepEqual(entry.Channels, channels) {
		t.Fatalf("channels = %#v, want %#v", entry.Channels, channels)
	}
	if !reflect.DeepEqual(entry.ModelGroups, modelGroups) {
		t.Fatalf("model_groups = %#v, want %#v", entry.ModelGroups, modelGroups)
	}
}
