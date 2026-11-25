package cluster

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

// migrateOpenAICompatibleProviders updates both the scheduling index and the full
// credential. Include retired rows: reconciliation checks their provider before
// restoring a UUID. A bare provider name alone is never evidence of a compat group.
func migrateOpenAICompatibleProviders(db *gorm.DB) error {
	var records []AuthRecord
	return db.Unscoped().Select("uuid", "provider", "provider_key", "compat_name", "auth_json").
		Where("compat_name <> ''").FindInBatches(&records, 200, func(_ *gorm.DB, _ int) error {
		for _, record := range records {
			name := strings.TrimSpace(record.CompatName)
			if name == "" {
				continue
			}
			key := config.OpenAICompatibleProviderKey(name)
			if record.Provider == key && record.ProviderKey == key {
				continue
			}
			// Patch only the two routing fields; auth JSON can contain extension data.
			if !gjson.ValidBytes(record.AuthJSON) {
				return fmt.Errorf("invalid auth JSON for credential %s", record.UUID)
			}
			raw, errSet := sjson.SetBytes(record.AuthJSON, "provider", key)
			if errSet != nil {
				return errSet
			}
			raw, errSet = sjson.SetBytes(raw, "attributes.provider_key", key)
			if errSet != nil {
				return errSet
			}
			if errUpdate := db.Unscoped().Model(&AuthRecord{}).Where("uuid = ?", record.UUID).
				UpdateColumns(map[string]any{"auth_json": JSONB(raw), "provider": key, "provider_key": key}).Error; errUpdate != nil {
				return errUpdate
			}
		}
		return nil
	}).Error
}
