package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"gorm.io/gorm"
)

type staticCredentialReferenceChecker struct {
	ids map[string]bool
}

func (c staticCredentialReferenceChecker) HasCredentialReferences(_ context.Context, _ *gorm.DB, id string) (bool, error) {
	return c.ids[id], nil
}

func newCredentialFoundationTestRepository(t *testing.T) *Repository {
	t.Helper()
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	t.Cleanup(func() {
		sqlDB, errDB := db.DB()
		if errDB == nil {
			if errClose := sqlDB.Close(); errClose != nil {
				t.Errorf("close database: %v", errClose)
			}
		}
	})
	return NewRepository(db)
}

func TestReuseGeneratedProviderCredentialIDs(t *testing.T) {
	legacySource := legacyGeminiSourceForTest("shared-key", "https://gemini.example.test")
	newSource := geminiSourceForTest("shared-key", "https://gemini.example.test", "http://proxy-a.example.test", "team-a")
	existingID := "10101010-1010-4010-8010-101010101010"

	tests := []struct {
		name     string
		existing []*coreauth.Auth
		next     []*coreauth.Auth
		wantIDs  []string
	}{
		{
			name: "exact current source",
			existing: []*coreauth.Auth{{
				ID: existingID, Provider: "gemini", Attributes: map[string]string{"source": newSource, "api_key": "shared-key", "base_url": "https://gemini.example.test"},
			}},
			next: []*coreauth.Auth{{
				ID: "20202020-2020-4020-8020-202020202020", Provider: "gemini", Attributes: map[string]string{"source": newSource, "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"},
			}},
			wantIDs: []string{existingID},
		},
		{
			name: "legacy Gemini source",
			existing: []*coreauth.Auth{{
				ID: existingID, Provider: "gemini", Attributes: map[string]string{"source": legacySource, "api_key": "shared-key", "base_url": "https://gemini.example.test"},
			}},
			next: []*coreauth.Auth{{
				ID: "30303030-3030-4030-8030-303030303030", Provider: "gemini", Attributes: map[string]string{"source": newSource, "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"},
			}},
			wantIDs: []string{existingID},
		},
		{
			name: "legacy UUID is claimed once",
			existing: []*coreauth.Auth{{
				ID: existingID, Provider: "gemini", Attributes: map[string]string{"source": legacySource, "api_key": "shared-key", "base_url": "https://gemini.example.test"},
			}},
			next: []*coreauth.Auth{
				{ID: "40404040-4040-4040-8040-404040404040", Provider: "gemini", Attributes: map[string]string{"source": newSource, "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"}},
				{ID: "50505050-5050-4050-8050-505050505050", Provider: "gemini", Attributes: map[string]string{"source": geminiSourceForTest("shared-key", "https://gemini.example.test", "http://proxy-b.example.test", "team-b"), "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"}},
			},
			wantIDs: []string{existingID, "50505050-5050-4050-8050-505050505050"},
		},
		{
			name: "stored routing identity wins legacy fallback order",
			existing: []*coreauth.Auth{{
				ID: existingID, Provider: "gemini", Prefix: "team-b", ProxyURL: "http://proxy-b.example.test", Attributes: map[string]string{"source": legacySource, "api_key": "shared-key", "base_url": "https://gemini.example.test"},
			}},
			next: []*coreauth.Auth{
				{ID: "51515151-5151-4151-8151-515151515151", Provider: "gemini", Prefix: "team-a", ProxyURL: "http://proxy-a.example.test", Attributes: map[string]string{"source": newSource, "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"}},
				{ID: "52525252-5252-4252-8252-525252525252", Provider: "gemini", Prefix: "team-b", ProxyURL: "http://proxy-b.example.test", Attributes: map[string]string{"source": geminiSourceForTest("shared-key", "https://gemini.example.test", "http://proxy-b.example.test", "team-b"), "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"}},
			},
			wantIDs: []string{"51515151-5151-4151-8151-515151515151", existingID},
		},
		{
			name: "explicit UUID reserves existing credential",
			existing: []*coreauth.Auth{{
				ID: existingID, Provider: "gemini", Attributes: map[string]string{"source": legacySource, "api_key": "shared-key", "base_url": "https://gemini.example.test"},
			}},
			next: []*coreauth.Auth{
				{ID: existingID, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[explicit]", "api_key": "explicit"}},
				{ID: "60606060-6060-4060-8060-606060606060", Provider: "gemini", Attributes: map[string]string{"source": newSource, "api_key": "shared-key", "base_url": "https://gemini.example.test", "provider_credential_id_generated": "true"}},
			},
			wantIDs: []string{existingID, "60606060-6060-4060-8060-606060606060"},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			for _, auth := range testCase.next {
				if auth != nil && auth.Index == "" {
					auth.Index = auth.ID
				}
			}
			ReuseGeneratedProviderCredentialIDs(testCase.existing, testCase.next)
			for i, wantID := range testCase.wantIDs {
				if testCase.next[i].ID != wantID || testCase.next[i].Index != wantID {
					t.Fatalf("next[%d] identity = (%q, %q), want %q", i, testCase.next[i].ID, testCase.next[i].Index, wantID)
				}
				if testCase.next[i].Attributes["cluster_uuid"] != "" && testCase.next[i].Attributes["cluster_uuid"] != wantID {
					t.Fatalf("next[%d] cluster_uuid = %q, want %q", i, testCase.next[i].Attributes["cluster_uuid"], wantID)
				}
			}
		})
	}
}

func legacyGeminiSourceForTest(apiKey, baseURL string) string {
	_, token := synthesizer.NewStableIDGenerator().Next("gemini:apikey", apiKey, baseURL)
	return "config:gemini[" + token + "]"
}

func geminiSourceForTest(apiKey, baseURL, proxyURL, prefix string) string {
	_, token := synthesizer.NewStableIDGenerator().Next("gemini:apikey", apiKey, baseURL, proxyURL, prefix, "")
	return "config:gemini[" + token + "]"
}

func TestReconcileProviderAuthsPreservesExplicitUUIDAndRejectsReferencedOrphan(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	old := &coreauth.Auth{
		ID:       "11111111-1111-4111-8111-111111111111",
		Index:    "11111111-1111-4111-8111-111111111111",
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[old]",
			"api_key": "old",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, old, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	checker := staticCredentialReferenceChecker{ids: map[string]bool{old.ID: true}}
	rotated := old.Clone()
	rotated.Attributes["api_key"] = "new"
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{rotated}, checker); errReconcile != nil {
		t.Fatalf("rotate with explicit id: %v", errReconcile)
	}
	withoutID := rotated.Clone()
	withoutID.ID = "22222222-2222-4222-8222-222222222222"
	withoutID.Index = withoutID.ID
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{withoutID}, checker); !errors.Is(errReconcile, ErrCredentialUUIDMappingRequired) {
		t.Fatalf("error = %v, want ErrCredentialUUIDMappingRequired", errReconcile)
	}
}

func TestReconcileProviderAuthsUsesConcurrencyReferenceCheckerByDefault(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	old := &coreauth.Auth{
		ID:       "12121212-1212-4212-8212-121212121212",
		Index:    "12121212-1212-4212-8212-121212121212",
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[old]",
			"api_key": "old",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, old, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	setExchangePolicy(t, repo, old.ID, 2, nil)
	rotated := old.Clone()
	rotated.ID = "13131313-1313-4313-8313-131313131313"
	rotated.Index = rotated.ID
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{rotated}, nil); !errors.Is(errReconcile, ErrCredentialConcurrencyOrphan) {
		t.Fatalf("ReconcileProviderAuths() error = %v, want ErrCredentialConcurrencyOrphan", errReconcile)
	}
}

func TestReconcileProviderAuthsRejectsNonCanonicalUUID(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	auth := &coreauth.Auth{
		ID:       "55555555-5555-4555-8555-55555555555A",
		Index:    "55555555-5555-4555-8555-55555555555A",
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[new]",
			"api_key": "key",
		},
	}
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{auth}, nil); errReconcile == nil {
		t.Fatal("ReconcileProviderAuths accepted a non-canonical UUID")
	}
}

func TestReconcileProviderAuthsRejectsDuplicateAndCrossProviderUUID(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "33333333-3333-4333-8333-333333333333"
	auth := &coreauth.Auth{ID: id, Index: id, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[old]", "api_key": "old"}}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	duplicate := auth.Clone()
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{auth, duplicate}, nil); errReconcile == nil {
		t.Fatal("duplicate provider credential id was accepted")
	}
	crossProvider := auth.Clone()
	crossProvider.Provider = "codex"
	crossProvider.Attributes = map[string]string{"source": "config:codex[new]", "api_key": "new"}
	if errReconcile := repo.ReconcileProviderAuths(ctx, "codex-api-key", []*coreauth.Auth{crossProvider}, nil); errReconcile == nil {
		t.Fatal("cross-provider credential id was accepted")
	}
}

func TestReconcileProviderAuthsRejectsAuthOutsideRequestedProviderKey(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "45454545-4545-4454-8454-454545454545"
	auth := &coreauth.Auth{
		ID:       id,
		Index:    id,
		Provider: "codex",
		Attributes: map[string]string{
			"source":  "config:codex[new]",
			"api_key": "key",
		},
	}
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{auth}, nil); !errors.Is(errReconcile, ErrCredentialValidation) {
		t.Fatalf("ReconcileProviderAuths() error = %v, want ErrCredentialValidation", errReconcile)
	}
}

func TestReconcileProviderAuthsIsIdempotentAfterRetirement(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	first := &coreauth.Auth{ID: "56565656-5656-4565-8565-565656565656", Index: "56565656-5656-4565-8565-565656565656", Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[first]", "api_key": "first"}}
	retired := &coreauth.Auth{ID: "67676767-6767-4676-8676-676767676767", Index: "67676767-6767-4676-8676-676767676767", Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[retired]", "api_key": "retired"}}
	for _, auth := range []*coreauth.Auth{first, retired} {
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
	}
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{first}, nil); errReconcile != nil {
		t.Fatalf("first reconciliation: %v", errReconcile)
	}
	if errReconcile := repo.ReconcileProviderAuths(ctx, "gemini-api-key", []*coreauth.Auth{first}, nil); errReconcile != nil {
		t.Fatalf("repeated reconciliation: %v", errReconcile)
	}
}

func TestReconcileProviderAuthsRejectsRetiredUUIDFromDifferentLineage(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "44444444-4444-4444-8444-444444444444"
	retired := &coreauth.Auth{ID: id, Index: id, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[old]", "api_key": "old"}}
	if _, errUpsert := repo.UpsertAuth(ctx, retired, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if errRetire := repo.RetireProviderAuth(ctx, retired.ID); errRetire != nil {
		t.Fatal(errRetire)
	}
	wrongLineage := retired.Clone()
	wrongLineage.Provider = "codex"
	wrongLineage.Attributes = map[string]string{"source": "config:codex[new]", "api_key": "new"}
	if errReconcile := repo.ReconcileProviderAuths(ctx, "codex-api-key", []*coreauth.Auth{wrongLineage}, nil); errReconcile == nil {
		t.Fatal("retired credential id from another lineage was accepted")
	}
}
