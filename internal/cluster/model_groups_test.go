package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"gorm.io/gorm"
)

func TestModelGroupDetailChannelsCreateAndUpdate(t *testing.T) {
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
	firstChannel, errFirstChannel := repo.CreateChannelGroup(ctx, "codex-all", false)
	if errFirstChannel != nil {
		t.Fatalf("CreateChannelGroup(first) error = %v", errFirstChannel)
	}
	secondChannel, errSecondChannel := repo.CreateChannelGroup(ctx, "codex-subset", false)
	if errSecondChannel != nil {
		t.Fatalf("CreateChannelGroup(second) error = %v", errSecondChannel)
	}
	modelGroup, errModelGroup := repo.CreateModelGroup(ctx, "codex-models", false)
	if errModelGroup != nil {
		t.Fatalf("CreateModelGroup() error = %v", errModelGroup)
	}

	detail, errCreate := repo.CreateModelGroupDetail(ctx, modelGroup.ID, "gpt-5.4", []uint{secondChannel.ID, firstChannel.ID, secondChannel.ID})
	if errCreate != nil {
		t.Fatalf("CreateModelGroupDetail() error = %v", errCreate)
	}
	channels, errChannels := ModelGroupDetailChannelIDs(detail)
	if errChannels != nil {
		t.Fatalf("ModelGroupDetailChannelIDs(create) error = %v", errChannels)
	}
	if want := []uint{firstChannel.ID, secondChannel.ID}; !reflect.DeepEqual(channels, want) {
		t.Fatalf("created channels = %v, want %v", channels, want)
	}

	updatedChannels := []uint{secondChannel.ID}
	updated, errUpdate := repo.UpdateModelGroupDetail(ctx, detail.ID, ModelGroupDetailUpdate{Channels: &updatedChannels})
	if errUpdate != nil {
		t.Fatalf("UpdateModelGroupDetail() error = %v", errUpdate)
	}
	channels, errChannels = ModelGroupDetailChannelIDs(updated)
	if errChannels != nil {
		t.Fatalf("ModelGroupDetailChannelIDs(update) error = %v", errChannels)
	}
	if !reflect.DeepEqual(channels, updatedChannels) {
		t.Fatalf("updated channels = %v, want %v", channels, updatedChannels)
	}

	missingChannels := []uint{secondChannel.ID + 1000}
	_, errMissing := repo.UpdateModelGroupDetail(ctx, detail.ID, ModelGroupDetailUpdate{Channels: &missingChannels})
	if !errors.Is(errMissing, gorm.ErrRecordNotFound) {
		t.Fatalf("UpdateModelGroupDetail(missing channel) error = %v, want record not found", errMissing)
	}

	reloaded, errReload := repo.GetModelGroupDetail(ctx, detail.ID)
	if errReload != nil {
		t.Fatalf("GetModelGroupDetail() error = %v", errReload)
	}
	channels, errChannels = ModelGroupDetailChannelIDs(reloaded)
	if errChannels != nil {
		t.Fatalf("ModelGroupDetailChannelIDs(reload) error = %v", errChannels)
	}
	if !reflect.DeepEqual(channels, updatedChannels) {
		t.Fatalf("channels after rejected update = %v, want %v", channels, updatedChannels)
	}
}

func TestMigrateModelGroupDetailChannelsBackfillsEmptyList(t *testing.T) {
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
	if errMigrate := db.AutoMigrate(&ModelGroupRecord{}, &ModelGroupDetailRecord{}); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	group := ModelGroupRecord{GroupName: "legacy"}
	if errCreateGroup := db.Create(&group).Error; errCreateGroup != nil {
		t.Fatalf("create model group: %v", errCreateGroup)
	}
	detail := ModelGroupDetailRecord{ModelGroupID: group.ID, ModelID: "legacy-model"}
	if errCreateDetail := db.Create(&detail).Error; errCreateDetail != nil {
		t.Fatalf("create model group detail: %v", errCreateDetail)
	}
	if errMigrate := migrateModelGroupDetailChannels(db); errMigrate != nil {
		t.Fatalf("migrateModelGroupDetailChannels() error = %v", errMigrate)
	}

	var reloaded ModelGroupDetailRecord
	if errFirst := db.First(&reloaded, detail.ID).Error; errFirst != nil {
		t.Fatalf("reload model group detail: %v", errFirst)
	}
	channels, errChannels := ModelGroupDetailChannelIDs(&reloaded)
	if errChannels != nil {
		t.Fatalf("ModelGroupDetailChannelIDs() error = %v", errChannels)
	}
	if len(channels) != 0 || string(reloaded.Channels) != "[]" {
		t.Fatalf("migrated channels = %v raw=%q, want []", channels, string(reloaded.Channels))
	}
}
