package cluster

import (
	"encoding/json"
	"fmt"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

const oauthExcludedModelsMigrationBatchSize = 200

var oauthExcludedModelsAttributeKeys = []string{"excluded_models", "excluded_models_hash"}

// migrateOAuthExcludedModelsAttributes rebuilds the excluded_models attributes of OAuth credentials
// from each credential's own metadata. Older versions persisted a snapshot of the global
// oauth-excluded-models config merged into these attributes, and that snapshot then overrode every
// later global config change at model registration time.
//
// The upgrade requires stopping every older Home node first, so no concurrent writer exists.
// Soft-deleted rows are skipped because restoring a credential writes a freshly synthesized auth.
// Any record that cannot be rebuilt fails the migration so the schema version is not advanced.
func migrateOAuthExcludedModelsAttributes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	var records []AuthRecord
	return db.
		Select("uuid", "auth_json").
		FindInBatches(&records, oauthExcludedModelsMigrationBatchSize, func(_ *gorm.DB, _ int) error {
			for _, record := range records {
				next, changed, errRebuild := rebuildOAuthExcludedModelsAuthJSON(record.AuthJSON)
				if errRebuild != nil {
					return fmt.Errorf("rebuild auth %s excluded models: %w", record.UUID, errRebuild)
				}
				if !changed {
					continue
				}
				if errUpdate := db.Model(&AuthRecord{}).
					Where("uuid = ?", record.UUID).
					UpdateColumn("auth_json", next).Error; errUpdate != nil {
					return fmt.Errorf("update auth %s excluded models: %w", record.UUID, errUpdate)
				}
			}
			return nil
		}).Error
}

// rebuildOAuthExcludedModelsAuthJSON recomputes the excluded_models attributes of a stored
// non-API-key auth from its metadata. Only those attribute keys are rewritten so the rest of the
// stored JSON stays byte-for-byte intact.
func rebuildOAuthExcludedModelsAuthJSON(raw JSONB) (JSONB, bool, error) {
	auth := &coreauth.Auth{}
	if errUnmarshal := json.Unmarshal([]byte(raw), auth); errUnmarshal != nil {
		return nil, false, errUnmarshal
	}
	if auth.AuthKind() == coreauth.AuthKindAPIKey {
		return raw, false, nil
	}

	before := make(map[string]string, len(oauthExcludedModelsAttributeKeys))
	for _, key := range oauthExcludedModelsAttributeKeys {
		if value, ok := auth.Attributes[key]; ok {
			before[key] = value
		}
	}
	if errSync := synthesizer.SyncOAuthExcludedModelsAttributes(auth); errSync != nil {
		return nil, false, errSync
	}

	next := []byte(raw)
	changed := false
	for _, key := range oauthExcludedModelsAttributeKeys {
		oldValue, hadOld := before[key]
		newValue, hasNew := auth.Attributes[key]
		if hadOld == hasNew && oldValue == newValue {
			continue
		}
		path := "attributes." + key
		var errSet error
		if hasNew {
			next, errSet = sjson.SetBytes(next, path, newValue)
		} else {
			next, errSet = sjson.DeleteBytes(next, path)
		}
		if errSet != nil {
			return nil, false, errSet
		}
		changed = true
	}
	return JSONB(next), changed, nil
}
