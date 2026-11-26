package cluster

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestMigrateOpenAICompatibleProviders(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	const source = "config:copilot[stable]"
	for _, id := range []string{"active", "retired", "native"} {
		auth := &coreauth.Auth{ID: id, Index: id, Provider: "copilot"}
		if id != "native" {
			auth.Attributes = map[string]string{"provider_key": "copilot", "compat_name": "Copilot", "source": source}
		}
		record, errRecord := AuthToRecord(auth)
		if errRecord != nil {
			t.Fatal(errRecord)
		}
		raw, errSet := sjson.SetBytes(record.AuthJSON, "extension.large_number", int64(9007199254740993))
		if errSet != nil {
			t.Fatal(errSet)
		}
		record.AuthJSON = JSONB(raw)
		if errCreate := repo.db.Create(record).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		if id == "retired" {
			if errDelete := repo.db.Delete(record).Error; errDelete != nil {
				t.Fatal(errDelete)
			}
		}
	}
	for range 2 {
		if errMigrate := migrateOpenAICompatibleProviders(repo.db); errMigrate != nil {
			t.Fatal(errMigrate)
		}
		for _, id := range []string{"active", "retired", "native"} {
			var row AuthRecord
			if errFind := repo.db.Unscoped().First(&row, "uuid = ?", id).Error; errFind != nil {
				t.Fatal(errFind)
			}
			auth, errAuth := RecordToAuth(&row)
			if errAuth != nil {
				t.Fatal(errAuth)
			}
			if gjson.GetBytes(row.AuthJSON, "extension.large_number").Raw != "9007199254740993" || row.DeletedAt.Valid != (id == "retired") || auth.ID != id {
				t.Fatalf("migration changed credential identity or unrelated data for %s", id)
			}
			if id == "native" {
				if auth.Provider != "copilot" || row.Provider != "copilot" || row.ProviderKey != "" {
					t.Fatal("native provider was migrated")
				}
				continue
			}
			const want = "openai-compatible-copilot"
			if auth.Provider != want || row.Provider != want || auth.Attributes["provider_key"] != want || row.ProviderKey != want || auth.Attributes["compat_name"] != "Copilot" || auth.Attributes["source"] != source {
				t.Fatalf("inconsistent migrated provider for %s", id)
			}
		}
	}
}
