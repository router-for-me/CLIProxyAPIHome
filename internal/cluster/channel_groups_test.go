package cluster

import (
	"context"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCreateChannelGroupDetailFindsAuthByQuotedIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, errOpenSQLite := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	repo := NewRepository(db)
	authID := "auth-a"
	if _, errUpsert := repo.UpsertAuth(ctx, &coreauth.Auth{ID: authID, Index: authID, Provider: "codex"}, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	group, errGroup := repo.CreateChannelGroup(ctx, "codex", false)
	if errGroup != nil {
		t.Fatalf("CreateChannelGroup() error = %v", errGroup)
	}
	if _, errDetail := repo.CreateChannelGroupDetail(ctx, group.ID, authID); errDetail != nil {
		t.Fatalf("CreateChannelGroupDetail() error = %v", errDetail)
	}
}
