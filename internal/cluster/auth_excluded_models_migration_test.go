package cluster

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func newExcludedModelsMigrationTestRepository(t *testing.T) (*gorm.DB, *Repository) {
	t.Helper()
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return db, NewRepository(db)
}

func seedExcludedModelsMigrationAuth(t *testing.T, repo *Repository, auth *coreauth.Auth) {
	t.Helper()
	auth.Index = auth.ID
	auth.Status = coreauth.StatusActive
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
	}
}

func TestMigrateOAuthExcludedModelsAttributesDropsGlobalSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, repo := newExcludedModelsMigrationTestRepository(t)
	// Stale global snapshot without any per-credential exclusions.
	seedExcludedModelsMigrationAuth(t, repo, &coreauth.Auth{
		ID: "oauth-inherit", Provider: "antigravity",
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": "claude-,gpt-", "excluded_models_hash": "stale", "note": "keep"},
		Metadata:   map[string]any{"type": "antigravity", "expired": "2026-10-10T00:00:00Z"},
	})
	// Stale snapshot merged with the credential's own exclusions.
	seedExcludedModelsMigrationAuth(t, repo, &coreauth.Auth{
		ID: "oauth-own", Provider: "antigravity",
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": "claude-,gemini-x", "excluded_models_hash": "stale"},
		Metadata:   map[string]any{"type": "antigravity", "excluded_models": []any{"Gemini-X"}},
	})
	// API-key credentials own their exclusions in config and must stay untouched.
	seedExcludedModelsMigrationAuth(t, repo, &coreauth.Auth{
		ID: "apikey-config", Provider: "claude",
		Attributes: map[string]string{"auth_kind": "apikey", "api_key": "fixture", "excluded_models": "claude-a", "excluded_models_hash": "kept"},
	})

	// The migration is idempotent; SQLite reruns custom migrations on every startup.
	for run := 0; run < 2; run++ {
		if errMigrate := migrateOAuthExcludedModelsAttributes(db); errMigrate != nil {
			t.Fatalf("migrateOAuthExcludedModelsAttributes() run %d error = %v", run, errMigrate)
		}
	}
	load := func(id string) *coreauth.Auth {
		t.Helper()
		auth, _, errAuth := repo.GetAuth(ctx, id)
		if errAuth != nil {
			t.Fatalf("GetAuth(%s) error = %v", id, errAuth)
		}
		return auth
	}

	inherit := load("oauth-inherit")
	if value, ok := inherit.Attributes["excluded_models"]; ok {
		t.Fatalf("oauth-inherit excluded_models = %q, want removed", value)
	}
	if value, ok := inherit.Attributes["excluded_models_hash"]; ok {
		t.Fatalf("oauth-inherit excluded_models_hash = %q, want removed", value)
	}
	if inherit.Attributes["note"] != "keep" || inherit.Metadata["expired"] != "2026-10-10T00:00:00Z" {
		t.Fatalf("oauth-inherit unrelated fields changed: attributes=%v metadata=%v", inherit.Attributes, inherit.Metadata)
	}

	own := load("oauth-own")
	if got, want := own.Attributes["excluded_models"], "gemini-x"; got != want {
		t.Fatalf("oauth-own excluded_models = %q, want %q", got, want)
	}
	if hash := own.Attributes["excluded_models_hash"]; hash == "" || hash == "stale" {
		t.Fatalf("oauth-own excluded_models_hash = %q, want recomputed hash", hash)
	}

	apiKey := load("apikey-config")
	if apiKey.Attributes["excluded_models"] != "claude-a" || apiKey.Attributes["excluded_models_hash"] != "kept" {
		t.Fatalf("apikey-config attributes = %v, want untouched", apiKey.Attributes)
	}
}

func TestMigrateOAuthExcludedModelsAttributesCoversBatchesAndSkipsSoftDeletedRows(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, repo := newExcludedModelsMigrationTestRepository(t)
	total := oauthExcludedModelsMigrationBatchSize + 5
	for index := 0; index < total; index++ {
		seedExcludedModelsMigrationAuth(t, repo, &coreauth.Auth{
			ID: fmt.Sprintf("oauth-batch-%03d", index), Provider: "antigravity",
			Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": "claude-,gpt-", "excluded_models_hash": "stale"},
			Metadata:   map[string]any{"type": "antigravity"},
		})
	}
	if errDelete := repo.SoftDeleteAuth(ctx, "oauth-batch-000"); errDelete != nil {
		t.Fatalf("SoftDeleteAuth() error = %v", errDelete)
	}

	if errMigrate := migrateOAuthExcludedModelsAttributes(db); errMigrate != nil {
		t.Fatalf("migrateOAuthExcludedModelsAttributes() error = %v", errMigrate)
	}

	var records []AuthRecord
	if errFind := db.Unscoped().Find(&records).Error; errFind != nil {
		t.Fatalf("list auth records: %v", errFind)
	}
	if len(records) != total {
		t.Fatalf("auth record count = %d, want %d", len(records), total)
	}
	for _, record := range records {
		hasAttributes := strings.Contains(string(record.AuthJSON), "excluded_models")
		// Soft-deleted rows are skipped because restoring a credential writes a freshly synthesized auth.
		if record.DeletedAt.Valid != hasAttributes {
			t.Fatalf("%s (deleted=%t) has excluded_models attributes = %t: %s", record.UUID, record.DeletedAt.Valid, hasAttributes, record.AuthJSON)
		}
	}
}

func TestMigrateOAuthExcludedModelsAttributesFailsOnMalformedRecord(t *testing.T) {
	t.Parallel()

	db, _ := newExcludedModelsMigrationTestRepository(t)
	if errCreate := db.Create(&AuthRecord{
		UUID: "oauth-malformed", ID: "oauth-malformed", Index: "oauth-malformed", Provider: "antigravity", Version: 1,
		AuthJSON: JSONB(`{"id":"oauth-malformed","provider":"antigravity","created_at":"not-a-date","attributes":{"excluded_models":"old-global"}}`),
	}).Error; errCreate != nil {
		t.Fatalf("create malformed auth: %v", errCreate)
	}

	errMigrate := migrateOAuthExcludedModelsAttributes(db)
	if errMigrate == nil || !strings.Contains(errMigrate.Error(), "oauth-malformed") {
		t.Fatalf("migrateOAuthExcludedModelsAttributes() error = %v, want failure naming the malformed auth", errMigrate)
	}
}

func TestMigrateOAuthExcludedModelsAttributesSplitsLegacyStringMetadata(t *testing.T) {
	t.Parallel()

	db, repo := newExcludedModelsMigrationTestRepository(t)
	seedExcludedModelsMigrationAuth(t, repo, &coreauth.Auth{
		ID: "oauth-legacy-string", Provider: "antigravity",
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": "claude-,gpt-", "excluded_models_hash": "stale"},
		Metadata:   map[string]any{"type": "antigravity", "excluded_models": "Gemini-Pro-Agent, gemini-3-flash"},
	})

	if errMigrate := migrateOAuthExcludedModelsAttributes(db); errMigrate != nil {
		t.Fatalf("migrateOAuthExcludedModelsAttributes() error = %v", errMigrate)
	}
	auth, _, errAuth := repo.GetAuth(context.Background(), "oauth-legacy-string")
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if got, want := auth.Attributes["excluded_models"], "gemini-3-flash,gemini-pro-agent"; got != want {
		t.Fatalf("excluded_models = %q, want %q", got, want)
	}
}

func TestMigrateOAuthExcludedModelsAttributesFailsOnInvalidExclusionType(t *testing.T) {
	t.Parallel()

	db, repo := newExcludedModelsMigrationTestRepository(t)
	seedExcludedModelsMigrationAuth(t, repo, &coreauth.Auth{
		ID: "oauth-invalid-type", Provider: "antigravity",
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": "claude-,gpt-"},
		Metadata:   map[string]any{"type": "antigravity", "excluded_models": []any{"gemini-pro-agent", 123}},
	})

	errMigrate := migrateOAuthExcludedModelsAttributes(db)
	if errMigrate == nil || !strings.Contains(errMigrate.Error(), "oauth-invalid-type") {
		t.Fatalf("migrateOAuthExcludedModelsAttributes() error = %v, want failure naming the invalid auth", errMigrate)
	}
	auth, _, errAuth := repo.GetAuth(context.Background(), "oauth-invalid-type")
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if auth.Attributes["excluded_models"] != "claude-,gpt-" {
		t.Fatalf("excluded_models = %q, want untouched after failed migration", auth.Attributes["excluded_models"])
	}
}
