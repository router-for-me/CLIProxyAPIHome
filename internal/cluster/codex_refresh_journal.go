package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CodexRefreshRecord survives auth deletion and lease expiry. It deliberately has
// no auth foreign key or expiry: neither event proves a grant was not consumed.
type CodexRefreshRecord struct {
	Grant     string    `gorm:"column:grant_sha256;primaryKey;size:64"`
	Owner     string    `gorm:"column:owner;not null;size:36"`
	Receipt   JSONB     `gorm:"column:receipt;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (CodexRefreshRecord) TableName() string { return "codex_refresh_journal" }

var _ codexauth.RefreshJournal = (*Repository)(nil)

// CodexRefreshJournal keeps manual, scheduled, and controller refreshes on the
// same database protocol, including across separate RuntimeAdapter instances.
func (a *RuntimeAdapter) CodexRefreshJournal() codexauth.RefreshJournal {
	if a == nil || a.repo == nil {
		return nil
	}
	return a.repo
}

func (r *Repository) RefreshClaim(ctx context.Context, key string) (string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return "", errDB
	}
	record := CodexRefreshRecord{Grant: key, Owner: uuid.NewString(), Receipt: JSONB(`{"stage":"pending"}`)}
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
	if result.Error != nil {
		return "", fmt.Errorf("claim Codex refresh: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return "", nil
	}
	return record.Owner, nil
}

func (r *Repository) RefreshRead(ctx context.Context, key string) ([]byte, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var record CodexRefreshRecord
	errRead := db.WithContext(ctx).Where("grant_sha256 = ?", key).Take(&record).Error
	if errors.Is(errRead, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read Codex refresh receipt: %w", errRead)
	}
	return []byte(record.Receipt), nil
}

func (r *Repository) RefreshComplete(ctx context.Context, key, owner string, receipt []byte) error {
	if owner == "" {
		return fmt.Errorf("Codex refresh claim owner is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	result := db.WithContext(ctx).Model(&CodexRefreshRecord{}).
		Where("grant_sha256 = ? AND owner = ?", key, owner).
		Updates(map[string]any{"receipt": JSONB(receipt), "owner": "", "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return fmt.Errorf("save Codex refresh receipt: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("Codex refresh claim is no longer owned")
	}
	return nil
}

func (r *Repository) RefreshRelease(ctx context.Context, key, owner string) error {
	if owner == "" {
		return fmt.Errorf("Codex refresh claim owner is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	result := db.WithContext(ctx).Where("grant_sha256 = ? AND owner = ?", key, owner).Delete(&CodexRefreshRecord{})
	if result.Error != nil {
		return fmt.Errorf("release unsubmitted Codex refresh: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("Codex refresh claim is no longer owned")
	}
	return nil
}

// Seed grant-matched metadata fences before refresh workers start.
// Otherwise another auth record containing the same grant could claim it first.
func migrateCodexRefreshOutcomes(db *gorm.DB) error {
	ctx := codexauth.WithRefreshJournal(db.Statement.Context, NewRepository(db))
	var cursor string
	for {
		var records []AuthRecord
		query := db.Unscoped().Where("LOWER(TRIM(provider)) = ?", "codex").Order("uuid").Limit(100)
		if cursor != "" {
			query = query.Where("uuid > ?", cursor)
		}
		if errFind := query.Find(&records).Error; errFind != nil {
			return fmt.Errorf("load legacy Codex refresh outcomes: %w", errFind)
		}
		for i := range records {
			// Snapshot imports may still contain legacy identity columns. Read
			// only refresh evidence without imposing runtime identity validation.
			var auth struct {
				Metadata map[string]any `json:"metadata"`
			}
			if errAuth := json.Unmarshal([]byte(records[i].AuthJSON), &auth); errAuth != nil {
				return fmt.Errorf("decode legacy Codex refresh outcome: %w", errAuth)
			}
			token, _ := auth.Metadata["refresh_token"].(string)
			if strings.TrimSpace(token) == "" {
				continue
			}
			outcome := codexauth.RefreshOutcomeUnknown(auth.Metadata, false)
			if outcome != nil {
				if errPreserve := codexauth.PreserveRefreshOutcome(ctx, token, outcome); errPreserve != nil {
					return fmt.Errorf("preserve legacy Codex refresh outcome: %w", errPreserve)
				}
			}
		}
		if len(records) < 100 {
			return nil
		}
		cursor = records[len(records)-1].UUID
	}
}
