package cluster

import "time"

// CodexRefreshRecord is retained for v17 database/snapshot compatibility.
// Historical journal entries no longer control runtime refresh attempts.
type CodexRefreshRecord struct {
	Grant     string    `gorm:"column:grant_sha256;primaryKey;size:64"`
	Owner     string    `gorm:"column:owner;not null;size:36"`
	Receipt   JSONB     `gorm:"column:receipt;not null"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (CodexRefreshRecord) TableName() string { return "codex_refresh_journal" }
