package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gorm.io/gorm"
)

var _ interface {
	AllowedDispatchIDsForAPIKeyModel(context.Context, string, string) ([]string, []string, error)
} = (*RuntimeAdapter)(nil)

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

	_, errZeroCreate := repo.CreateModelGroupDetail(ctx, modelGroup.ID, "gpt-invalid", []uint{firstChannel.ID, 0})
	if !errors.Is(errZeroCreate, ErrInvalidModelChannelGroupID) {
		t.Fatalf("CreateModelGroupDetail(zero channel) error = %v, want ErrInvalidModelChannelGroupID", errZeroCreate)
	}
	invalidDetails, errInvalidDetails := repo.ListModelGroupDetails(ctx, ModelGroupDetailFilter{ModelID: "gpt-invalid"})
	if errInvalidDetails != nil {
		t.Fatalf("ListModelGroupDetails(invalid) error = %v", errInvalidDetails)
	}
	if len(invalidDetails) != 0 {
		t.Fatalf("invalid model details = %#v, want none", invalidDetails)
	}

	detail, errCreate := repo.CreateModelGroupDetail(ctx, modelGroup.ID, "gpt-5.4(high)", []uint{secondChannel.ID, firstChannel.ID, secondChannel.ID})
	if errCreate != nil {
		t.Fatalf("CreateModelGroupDetail() error = %v", errCreate)
	}
	if detail.ModelID != "gpt-5.4" {
		t.Fatalf("created model ID = %q, want canonical gpt-5.4", detail.ModelID)
	}
	channels, errChannels := ModelGroupDetailChannelIDs(detail)
	if errChannels != nil {
		t.Fatalf("ModelGroupDetailChannelIDs(create) error = %v", errChannels)
	}
	if want := []uint{firstChannel.ID, secondChannel.ID}; !reflect.DeepEqual(channels, want) {
		t.Fatalf("created channels = %v, want %v", channels, want)
	}

	updatedChannels := []uint{secondChannel.ID}
	updatedModelID := "gpt-5.5(low)"
	updated, errUpdate := repo.UpdateModelGroupDetail(ctx, detail.ID, ModelGroupDetailUpdate{ModelID: &updatedModelID, Channels: &updatedChannels})
	if errUpdate != nil {
		t.Fatalf("UpdateModelGroupDetail() error = %v", errUpdate)
	}
	if updated.ModelID != "gpt-5.5" {
		t.Fatalf("updated model ID = %q, want canonical gpt-5.5", updated.ModelID)
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
	zeroChannels := []uint{secondChannel.ID, 0}
	_, errZeroUpdate := repo.UpdateModelGroupDetail(ctx, detail.ID, ModelGroupDetailUpdate{Channels: &zeroChannels})
	if !errors.Is(errZeroUpdate, ErrInvalidModelChannelGroupID) {
		t.Fatalf("UpdateModelGroupDetail(zero channel) error = %v, want ErrInvalidModelChannelGroupID", errZeroUpdate)
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

func TestAllowedDispatchIDsForAPIKeyModelIntersectsModelChannels(t *testing.T) {
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
	for _, authID := range []string{"auth-a", "auth-b", "auth-c"} {
		auth := &coreauth.Auth{ID: authID, Index: authID, Provider: "codex"}
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", authID, errUpsert)
		}
	}
	allChannel, errAllChannel := repo.CreateChannelGroup(ctx, "codex-all", false)
	if errAllChannel != nil {
		t.Fatalf("CreateChannelGroup(all) error = %v", errAllChannel)
	}
	subsetChannel, errSubsetChannel := repo.CreateChannelGroup(ctx, "codex-subset", false)
	if errSubsetChannel != nil {
		t.Fatalf("CreateChannelGroup(subset) error = %v", errSubsetChannel)
	}
	emptyChannel, errEmptyChannel := repo.CreateChannelGroup(ctx, "codex-empty", false)
	if errEmptyChannel != nil {
		t.Fatalf("CreateChannelGroup(empty) error = %v", errEmptyChannel)
	}
	for _, authID := range []string{"auth-a", "auth-b"} {
		if _, errDetail := repo.CreateChannelGroupDetail(ctx, allChannel.ID, authID); errDetail != nil {
			t.Fatalf("CreateChannelGroupDetail(all, %s) error = %v", authID, errDetail)
		}
	}
	for _, authID := range []string{"auth-b", "auth-c"} {
		if _, errDetail := repo.CreateChannelGroupDetail(ctx, subsetChannel.ID, authID); errDetail != nil {
			t.Fatalf("CreateChannelGroupDetail(subset, %s) error = %v", authID, errDetail)
		}
	}

	explicitGroup, errExplicitGroup := repo.CreateModelGroup(ctx, "explicit", false)
	if errExplicitGroup != nil {
		t.Fatalf("CreateModelGroup(explicit) error = %v", errExplicitGroup)
	}
	inheritedGroup, errInheritedGroup := repo.CreateModelGroup(ctx, "inherited", false)
	if errInheritedGroup != nil {
		t.Fatalf("CreateModelGroup(inherited) error = %v", errInheritedGroup)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, explicitGroup.ID, "gpt-5.4", []uint{subsetChannel.ID}); errDetail != nil {
		t.Fatalf("CreateModelGroupDetail(explicit) error = %v", errDetail)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, inheritedGroup.ID, "gpt-5.4", nil); errDetail != nil {
		t.Fatalf("CreateModelGroupDetail(inherited duplicate) error = %v", errDetail)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, inheritedGroup.ID, "gpt-5.5", nil); errDetail != nil {
		t.Fatalf("CreateModelGroupDetail(inherited model) error = %v", errDetail)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, explicitGroup.ID, "gpt-empty", []uint{emptyChannel.ID}); errDetail != nil {
		t.Fatalf("CreateModelGroupDetail(empty) error = %v", errDetail)
	}
	legacyChannels, errLegacyChannels := modelGroupDetailChannelsJSON([]uint{subsetChannel.ID})
	if errLegacyChannels != nil {
		t.Fatalf("modelGroupDetailChannelsJSON(legacy) error = %v", errLegacyChannels)
	}
	legacyDetail := &ModelGroupDetailRecord{ModelGroupID: explicitGroup.ID, ModelID: "gpt-legacy(high)", Channels: legacyChannels}
	if errCreateLegacy := db.Create(legacyDetail).Error; errCreateLegacy != nil {
		t.Fatalf("create legacy suffixed detail: %v", errCreateLegacy)
	}
	for _, filterModelID := range []string{"gpt-legacy", "gpt-legacy(high)"} {
		filteredDetails, errFilteredDetails := repo.ListModelGroupDetails(ctx, ModelGroupDetailFilter{ModelID: filterModelID})
		if errFilteredDetails != nil {
			t.Fatalf("ListModelGroupDetails(%q) error = %v", filterModelID, errFilteredDetails)
		}
		if len(filteredDetails) != 1 || filteredDetails[0].ID != legacyDetail.ID {
			t.Fatalf("ListModelGroupDetails(%q) = %#v, want legacy detail %d", filterModelID, filteredDetails, legacyDetail.ID)
		}
	}

	clientKey := "client-key"
	keyChannels := []uint{allChannel.ID}
	keyModelGroups := []uint{explicitGroup.ID, inheritedGroup.ID}
	if _, errCreateKey := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: clientKey, Channels: &keyChannels, ModelGroups: &keyModelGroups}); errCreateKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreateKey)
	}

	modelDetailQueries := 0
	channelDetailQueries := 0
	callbackName := "test:count-policy-detail-queries"
	if errCallback := db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx == nil || tx.Statement == nil {
			return
		}
		switch tx.Statement.Table {
		case "model_group_detail":
			modelDetailQueries++
		case "channel_group_detail":
			channelDetailQueries++
		}
	}); errCallback != nil {
		t.Fatalf("register query callback: %v", errCallback)
	}
	authIDs, modelIDs, errAllowed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, clientKey, "GPT-5.4")
	if errRemove := db.Callback().Query().Remove(callbackName); errRemove != nil {
		t.Fatalf("remove query callback: %v", errRemove)
	}
	if errAllowed != nil {
		t.Fatalf("AllowedDispatchIDsForAPIKeyModel(explicit) error = %v", errAllowed)
	}
	if modelDetailQueries != 1 {
		t.Fatalf("model detail query count = %d, want one coherent load", modelDetailQueries)
	}
	if channelDetailQueries != 1 {
		t.Fatalf("channel detail query count = %d, want one coherent load", channelDetailQueries)
	}
	if want := []string{"auth-b"}; !reflect.DeepEqual(authIDs, want) {
		t.Fatalf("explicit auth IDs = %v, want %v", authIDs, want)
	}
	if want := []string{"gpt-5.4", "gpt-empty", "gpt-legacy", "gpt-5.5"}; !reflect.DeepEqual(modelIDs, want) {
		t.Fatalf("model IDs = %v, want %v", modelIDs, want)
	}

	authIDs, _, errAllowed = repo.AllowedDispatchIDsForAPIKeyModel(ctx, clientKey, "gpt-legacy")
	if errAllowed != nil {
		t.Fatalf("AllowedDispatchIDsForAPIKeyModel(legacy suffix) error = %v", errAllowed)
	}
	if want := []string{"auth-b"}; !reflect.DeepEqual(authIDs, want) {
		t.Fatalf("legacy suffix auth IDs = %v, want %v", authIDs, want)
	}

	authIDs, _, errAllowed = repo.AllowedDispatchIDsForAPIKeyModel(ctx, clientKey, "gpt-5.5")
	if errAllowed != nil {
		t.Fatalf("AllowedDispatchIDsForAPIKeyModel(inherited) error = %v", errAllowed)
	}
	if want := []string{"auth-a", "auth-b"}; !reflect.DeepEqual(authIDs, want) {
		t.Fatalf("inherited auth IDs = %v, want %v", authIDs, want)
	}

	authIDs, _, errAllowed = repo.AllowedDispatchIDsForAPIKeyModel(ctx, clientKey, "gpt-empty")
	if errAllowed != nil {
		t.Fatalf("AllowedDispatchIDsForAPIKeyModel(empty) error = %v", errAllowed)
	}
	if authIDs == nil || len(authIDs) != 0 {
		t.Fatalf("empty explicit auth IDs = %#v, want non-nil empty list", authIDs)
	}

	unrestrictedKey := "unrestricted-client-key"
	unrestrictedModelGroups := []uint{explicitGroup.ID}
	if _, errCreateKey := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: unrestrictedKey, ModelGroups: &unrestrictedModelGroups}); errCreateKey != nil {
		t.Fatalf("CreateAPIKey(unrestricted channels) error = %v", errCreateKey)
	}
	authIDs, _, errAllowed = repo.AllowedDispatchIDsForAPIKeyModel(ctx, unrestrictedKey, "gpt-5.4")
	if errAllowed != nil {
		t.Fatalf("AllowedDispatchIDsForAPIKeyModel(unrestricted channels) error = %v", errAllowed)
	}
	if want := []string{"auth-b", "auth-c"}; !reflect.DeepEqual(authIDs, want) {
		t.Fatalf("unrestricted base auth IDs = %v, want %v", authIDs, want)
	}

	disabled := true
	if _, errDisable := repo.UpdateChannelGroup(ctx, subsetChannel.ID, ChannelGroupUpdate{Disabled: &disabled}); errDisable != nil {
		t.Fatalf("UpdateChannelGroup(disable) error = %v", errDisable)
	}
	authIDs, _, errAllowed = repo.AllowedDispatchIDsForAPIKeyModel(ctx, unrestrictedKey, "gpt-5.4")
	if errAllowed != nil {
		t.Fatalf("AllowedDispatchIDsForAPIKeyModel(disabled channel) error = %v", errAllowed)
	}
	if authIDs == nil || len(authIDs) != 0 {
		t.Fatalf("disabled model channel auth IDs = %#v, want non-nil empty list", authIDs)
	}
}

func TestRuntimeAdapterEnforcesModelChannelBindingsInHomeDispatch(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)

	modelID := "adapter-model"
	auths := []*coreauth.Auth{
		{ID: "adapter-auth-a-wide", Index: "adapter-auth-a-wide", Provider: "codex", Status: coreauth.StatusActive},
		{ID: "adapter-auth-z-subset", Index: "adapter-auth-z-subset", Provider: "codex", Status: coreauth.StatusActive},
	}
	for _, auth := range auths {
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
		}
	}

	allChannel, errAllChannel := repo.CreateChannelGroup(ctx, "adapter-all", false)
	if errAllChannel != nil {
		t.Fatalf("CreateChannelGroup(all) error = %v", errAllChannel)
	}
	subsetChannel, errSubsetChannel := repo.CreateChannelGroup(ctx, "adapter-subset", false)
	if errSubsetChannel != nil {
		t.Fatalf("CreateChannelGroup(subset) error = %v", errSubsetChannel)
	}
	for _, auth := range auths {
		if _, errDetail := repo.CreateChannelGroupDetail(ctx, allChannel.ID, auth.ID); errDetail != nil {
			t.Fatalf("CreateChannelGroupDetail(all, %s) error = %v", auth.ID, errDetail)
		}
	}
	if _, errDetail := repo.CreateChannelGroupDetail(ctx, subsetChannel.ID, auths[1].ID); errDetail != nil {
		t.Fatalf("CreateChannelGroupDetail(subset) error = %v", errDetail)
	}

	modelGroup, errModelGroup := repo.CreateModelGroup(ctx, "adapter-models", false)
	if errModelGroup != nil {
		t.Fatalf("CreateModelGroup() error = %v", errModelGroup)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, modelGroup.ID, modelID, []uint{subsetChannel.ID}); errDetail != nil {
		t.Fatalf("CreateModelGroupDetail() error = %v", errDetail)
	}
	clientKey := "adapter-client-key"
	keyChannels := []uint{allChannel.ID}
	keyModelGroups := []uint{modelGroup.ID}
	if _, errKey := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: clientKey, Channels: &keyChannels, ModelGroups: &keyModelGroups}); errKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errKey)
	}

	runtime, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("home.NewRuntime() error = %v", errRuntime)
	}
	runtime.SetClusterAdapter(NewRuntimeAdapter(repo, ""))
	t.Cleanup(runtime.Stop)
	for _, auth := range auths {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "openai"}})
		if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth.Clone()); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
		authID := auth.ID
		t.Cleanup(func() {
			registry.GetGlobalRegistry().UnregisterClient(authID)
		})
	}

	for attempt := 1; attempt <= 2; attempt++ {
		result, errDispatch := runtime.DispatchForAPIKey(ctx, modelID, nil, clientKey)
		if errDispatch != nil {
			t.Fatalf("DispatchForAPIKey(attempt %d) error = %v", attempt, errDispatch)
		}
		if result == nil || result.AuthID != auths[1].ID {
			t.Fatalf("DispatchForAPIKey(attempt %d) result = %#v, want auth %q", attempt, result, auths[1].ID)
		}
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
