package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestSynthesizeAPIKeyBodyRejectsInvalidIDBeforeSanitize(t *testing.T) {
	handler := &Handler{}
	_, errSynthesize := handler.synthesizeAPIKeyBody("gemini-api-key", []byte(`[{"id":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA","api-key":""}]`))
	if errSynthesize == nil {
		t.Fatal("synthesizeAPIKeyBody accepted a non-canonical ID on a sanitized entry")
	}
}

func TestPutAPIKeyMapsCredentialValidationAndCollisionErrors(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.PUT("/gemini-api-key", handler.PutGeminiKeys)

	invalid := httptest.NewRecorder()
	engine.ServeHTTP(invalid, httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(`[{"id":"invalid","api-key":"key"}]`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid UUID PUT status = %d body=%s", invalid.Code, invalid.Body.String())
	}

	id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	existing := &coreauth.Auth{ID: id, Index: id, Provider: "codex", Attributes: map[string]string{"source": "oauth:existing", "api_key": "existing"}}
	if _, errUpsert := repo.UpsertAuth(context.Background(), existing, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	collision := httptest.NewRecorder()
	engine.ServeHTTP(collision, httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(`[{"id":"`+id+`","api-key":"key"}]`)))
	if collision.Code != http.StatusConflict {
		t.Fatalf("colliding UUID PUT status = %d body=%s", collision.Code, collision.Body.String())
	}
}

func TestPatchAPIKeyPreservesCredentialUUID(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.PUT("/gemini-api-key", handler.PutGeminiKeys)
	engine.PATCH("/gemini-api-key", handler.PatchGeminiKey)

	put := httptest.NewRecorder()
	engine.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/gemini-api-key", strings.NewReader(`[{"api-key":"old","id":"11111111-1111-4111-8111-111111111111"}]`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", put.Code, put.Body.String())
	}
	patch := httptest.NewRecorder()
	engine.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/gemini-api-key", strings.NewReader(`{"id":"11111111-1111-4111-8111-111111111111","value":{"api-key":"new"}}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patch.Code, patch.Body.String())
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatal(errAuths)
	}
	if len(auths) != 1 || auths[0].ID != "11111111-1111-4111-8111-111111111111" || auths[0].Attributes["api_key"] != "new" {
		t.Fatalf("patched auth = %#v", auths)
	}
}

func TestPatchOpenAICompatibilityPreservesFallbackCredentialUUIDForEmptyEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries string
	}{
		{name: "empty array", entries: "[]"},
		{name: "null", entries: "null"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := openManagementLogTestDB(t)
			defer cleanup()

			repo := cluster.NewRepository(db)
			handler := NewHandler(repo, nil, "127.0.0.1", 0)
			engine := gin.New()
			engine.PUT("/openai-compatibility", handler.PutOpenAICompat)
			engine.PATCH("/openai-compatibility", handler.PatchOpenAICompat)
			id := "99999999-9999-4999-8999-999999999999"

			put := httptest.NewRecorder()
			engine.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/openai-compatibility", strings.NewReader(`[{"id":"`+id+`","name":"compat","base-url":"https://example.test"}]`)))
			if put.Code != http.StatusOK {
				t.Fatalf("PUT status = %d body=%s", put.Code, put.Body.String())
			}
			patch := httptest.NewRecorder()
			engine.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/openai-compatibility", strings.NewReader(`{"id":"`+id+`","value":{"api-key-entries":`+tc.entries+`}}`)))
			if patch.Code != http.StatusOK {
				t.Fatalf("PATCH status = %d body=%s", patch.Code, patch.Body.String())
			}
			auths, errAuths := repo.ListAuths(context.Background())
			if errAuths != nil {
				t.Fatal(errAuths)
			}
			if len(auths) != 1 || auths[0].ID != id {
				t.Fatalf("patched auth = %#v", auths)
			}
		})
	}
}

func TestPatchOpenAICompatibilityRejectsProviderRebind(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.PUT("/openai-compatibility", handler.PutOpenAICompat)
	engine.PATCH("/openai-compatibility", handler.PatchOpenAICompat)
	id := "31313131-3131-4131-8131-313131313131"

	put := httptest.NewRecorder()
	engine.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/openai-compatibility", strings.NewReader(`[{"id":"`+id+`","name":"original","base-url":"https://example.test"}]`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", put.Code, put.Body.String())
	}
	patch := httptest.NewRecorder()
	engine.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/openai-compatibility", strings.NewReader(`{"id":"`+id+`","value":{"name":"rebound"}}`)))
	if patch.Code != http.StatusBadRequest {
		t.Fatalf("PATCH status = %d body=%s", patch.Code, patch.Body.String())
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatal(errAuths)
	}
	if len(auths) != 1 || auths[0].ID != id || auths[0].Provider != "original" {
		t.Fatalf("auth after rejected rebind = %#v", auths)
	}
}

func TestPatchOpenAICompatibilityPreservesNestedCredentialUUID(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.PUT("/openai-compatibility", handler.PutOpenAICompat)
	engine.PATCH("/openai-compatibility", handler.PatchOpenAICompat)

	put := httptest.NewRecorder()
	engine.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/openai-compatibility", strings.NewReader(`[{"name":"compat","base-url":"https://example.test","api-key-entries":[{"id":"22222222-2222-4222-8222-222222222222","api-key":"old"}]}]`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", put.Code, put.Body.String())
	}
	patch := httptest.NewRecorder()
	engine.ServeHTTP(patch, httptest.NewRequest(http.MethodPatch, "/openai-compatibility", strings.NewReader(`{"id":"22222222-2222-4222-8222-222222222222","value":{"api-key-entries":[{"api-key":"new"}]}}`)))
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patch.Code, patch.Body.String())
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatal(errAuths)
	}
	if len(auths) != 1 || auths[0].ID != "22222222-2222-4222-8222-222222222222" || auths[0].Attributes["api_key"] != "new" {
		t.Fatalf("patched auth = %#v", auths)
	}
}
