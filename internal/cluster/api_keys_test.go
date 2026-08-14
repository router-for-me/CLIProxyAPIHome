package cluster

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAutoMigrateAddsNullableAPIKeyDisplayName(t *testing.T) {
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

	if errCreateLegacy := db.Exec(`CREATE TABLE api_key (
		id integer PRIMARY KEY AUTOINCREMENT,
		api_key text NOT NULL UNIQUE,
		user_id integer,
		channels text,
		model_groups text,
		created_at datetime,
		updated_at datetime,
		deleted_at datetime
	)`).Error; errCreateLegacy != nil {
		t.Fatalf("create legacy api_key table: %v", errCreateLegacy)
	}
	if errSeed := db.Exec(`INSERT INTO api_key (api_key, channels, model_groups) VALUES (?, ?, ?)`, "legacy-key", "[]", "[]").Error; errSeed != nil {
		t.Fatalf("seed legacy api_key row: %v", errSeed)
	}

	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if !db.Migrator().HasColumn(&APIKeyRecord{}, "display_name") {
		t.Fatal("AutoMigrate() did not add api_key.display_name")
	}
	var record APIKeyRecord
	if errFirst := db.First(&record, "api_key = ?", "legacy-key").Error; errFirst != nil {
		t.Fatalf("load migrated API key: %v", errFirst)
	}
	if record.DisplayName != nil || record.APIKey != "legacy-key" {
		t.Fatalf("migrated record = %#v, want unchanged key and null display name", record)
	}
}

func TestAPIKeyMutationLocksUsePostgresAdvisoryLock(t *testing.T) {
	var logs bytes.Buffer
	db, errOpen := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 user=cliproxy dbname=cliproxy_home sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		DryRun:               true,
		Logger: logger.New(log.New(&logs, "", 0), logger.Config{
			LogLevel: logger.Info,
		}),
	})
	if errOpen != nil {
		t.Fatalf("open postgres dry-run db: %v", errOpen)
	}

	if errLock := lockAPIKeyMutationTransaction(db); errLock != nil {
		t.Fatalf("lockAPIKeyMutationTransaction() error = %v", errLock)
	}
	if errLock := lockAPIKeyReadTransaction(db); errLock != nil {
		t.Fatalf("lockAPIKeyReadTransaction() error = %v", errLock)
	}
	output := logs.String()
	if !strings.Contains(output, "pg_advisory_xact_lock") ||
		!strings.Contains(output, "pg_advisory_xact_lock_shared") ||
		!strings.Contains(output, strconv.FormatInt(apiKeyMutationAdvisoryLockKey, 10)) {
		t.Fatalf("postgres API key advisory lock SQL = %q", output)
	}
}

func TestAPIKeyETagMatchesRequiresStrongValidators(t *testing.T) {
	current := `"current"`
	if !apiKeyETagMatches(current, current) || !apiKeyETagMatches(`"stale", "current"`, current) {
		t.Fatal("strong matching ETag was rejected")
	}
	for _, candidate := range []string{"*", `W/"current"`, "current", `"current", W/"stale"`, ""} {
		if apiKeyETagMatches(candidate, current) {
			t.Fatalf("invalid or weak If-Match %q was accepted", candidate)
		}
	}
}

func TestCreateAPIKeyIgnoresDisplayNameWithoutPresenceFlag(t *testing.T) {
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
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	displayName := "should be ignored"
	record, errCreate := NewRepository(db).CreateAPIKey(ctx, APIKeyEntryUpdate{
		APIKey:      "presence-flag-key",
		DisplayName: &displayName,
	})
	if errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}
	if record.DisplayName != nil {
		t.Fatalf("created display name = %q, want null without DisplayNameSet", *record.DisplayName)
	}
}

func TestAPIKeyDisplayNameValidationAndRuntimeChangeEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, repo := openAPIKeyTestRepository(t)
	displayName := "Initial name"
	record, runtimeChanged, errCreate := repo.CreateAPIKeyWithRuntimeChange(ctx, APIKeyEntryUpdate{
		APIKey:         "runtime-change-key",
		DisplayName:    &displayName,
		DisplayNameSet: true,
	})
	if errCreate != nil || !runtimeChanged {
		t.Fatalf("CreateAPIKeyWithRuntimeChange() = %#v, %t, %v", record, runtimeChanged, errCreate)
	}
	eventsAfterCreate := apiKeyEventCount(t, db)

	renamed := "Renamed metadata"
	updated, runtimeChanged, errRename := repo.UpdateAPIKeyWithRuntimeChange(ctx, APIKeySelector{ID: record.ID}, APIKeyAdminUpdate{
		DisplayName:    &renamed,
		DisplayNameSet: true,
	})
	if errRename != nil || runtimeChanged {
		t.Fatalf("name-only UpdateAPIKeyWithRuntimeChange() = %#v, %t, %v", updated, runtimeChanged, errRename)
	}
	if updated.DisplayName == nil || *updated.DisplayName != renamed {
		t.Fatalf("updated display name = %v, want %q", updated.DisplayName, renamed)
	}
	if got := apiKeyEventCount(t, db); got != eventsAfterCreate {
		t.Fatalf("events after name-only update = %d, want %d", got, eventsAfterCreate)
	}

	rotatedKey := "runtime-change-key-rotated"
	_, runtimeChanged, errRotate := repo.UpdateAPIKeyWithRuntimeChange(ctx, APIKeySelector{ID: record.ID}, APIKeyAdminUpdate{APIKey: &rotatedKey})
	if errRotate != nil || !runtimeChanged {
		t.Fatalf("key rotation UpdateAPIKeyWithRuntimeChange() runtimeChanged=%t error=%v", runtimeChanged, errRotate)
	}
	if got := apiKeyEventCount(t, db); got != eventsAfterCreate+1 {
		t.Fatalf("events after key rotation = %d, want %d", got, eventsAfterCreate+1)
	}

	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "too long", value: strings.Repeat("名", APIKeyDisplayNameMaxLength+1)},
		{name: "control character", value: "invalid\nname"},
		{name: "leading control character", value: "\tinvalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			before := apiKeyEventCount(t, db)
			_, _, errUpdate := repo.UpdateAPIKeyWithRuntimeChange(ctx, APIKeySelector{ID: record.ID}, APIKeyAdminUpdate{
				DisplayName:    &testCase.value,
				DisplayNameSet: true,
			})
			if !errors.Is(errUpdate, ErrInvalidAPIKeyDisplayName) {
				t.Fatalf("UpdateAPIKeyWithRuntimeChange() error = %v, want ErrInvalidAPIKeyDisplayName", errUpdate)
			}
			if got := apiKeyEventCount(t, db); got != before {
				t.Fatalf("events after rejected name = %d, want %d", got, before)
			}
			entry, errEntry := repo.ListAPIKeyEntries(ctx)
			if errEntry != nil || len(entry) != 1 || entry[0].DisplayName == nil || *entry[0].DisplayName != renamed {
				t.Fatalf("entry after rejected name = %#v, %v", entry, errEntry)
			}
		})
	}
}

func TestAPIKeySparseWritePathsPreserveDisplayName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, repo := openAPIKeyTestRepository(t)
	username := "sparse-write-user"
	user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &username})
	if errUser != nil {
		t.Fatalf("CreateUser() error = %v", errUser)
	}
	displayName := "Must survive sparse writes"
	channels := []uint{11}
	modelGroups := []uint{21}
	record, errCreate := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{
		APIKey:         "sparse-write-key",
		DisplayName:    &displayName,
		DisplayNameSet: true,
		UserID:         &user.ID,
		UserIDSet:      true,
		Channels:       &channels,
		ModelGroups:    &modelGroups,
	})
	if errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}

	nextChannels := []uint{12}
	if _, errBindings := repo.UpdateAPIKeyBindings(ctx, record.APIKey, nil, &nextChannels, nil); errBindings != nil {
		t.Fatalf("UpdateAPIKeyBindings() error = %v", errBindings)
	}
	assertAPIKeyRecordDisplayName(t, repo, record.ID, displayName)

	nextModelGroups := []uint{22}
	if _, errExistingCreate := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{
		APIKey:      &record.APIKey,
		Channels:    &nextChannels,
		ModelGroups: &nextModelGroups,
	}); errExistingCreate != nil {
		t.Fatalf("CreateAPIKeyForUser(existing) error = %v", errExistingCreate)
	}
	assertAPIKeyRecordDisplayName(t, repo, record.ID, displayName)

	rotatedKey := "sparse-write-key-rotated"
	if _, errUpdate := repo.UpdateAPIKeyForUser(ctx, user.ID, record.ID, "", APIKeyUserUpdate{APIKey: &rotatedKey}); errUpdate != nil {
		t.Fatalf("UpdateAPIKeyForUser() error = %v", errUpdate)
	}
	assertAPIKeyRecordDisplayName(t, repo, record.ID, displayName)

	entries, errEntries := repo.ListAPIKeyEntries(ctx)
	if errEntries != nil || len(entries) != 1 || entries[0].APIKey != rotatedKey || !reflect.DeepEqual(entries[0].Channels, nextChannels) || !reflect.DeepEqual(entries[0].ModelGroups, nextModelGroups) {
		t.Fatalf("sparse write result = %#v, %v", entries, errEntries)
	}
}

func TestReplaceAPIKeyEntriesUsesLastDuplicateAndPresenceSemantics(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, repo := openAPIKeyTestRepository(t)
	firstName := "First duplicate"
	lastName := "Last duplicate"
	stats, errReplace := repo.ReplaceAPIKeyEntries(ctx, []APIKeyEntryUpdate{
		{APIKey: "duplicate-key", DisplayName: &firstName, DisplayNameSet: true},
		{APIKey: "duplicate-key", DisplayName: &lastName, DisplayNameSet: true},
	})
	if errReplace != nil {
		t.Fatalf("ReplaceAPIKeyEntries() error = %v", errReplace)
	}
	if !stats.RequiresRuntimeRefresh() {
		t.Fatal("new API key did not require runtime refresh")
	}
	entries, errEntries := repo.ListAPIKeyEntries(ctx)
	if errEntries != nil || len(entries) != 1 || entries[0].DisplayName == nil || *entries[0].DisplayName != lastName {
		t.Fatalf("last-write-wins entries = %#v, %v", entries, errEntries)
	}

	stats, errReplace = repo.ReplaceAPIKeyEntries(ctx, []APIKeyEntryUpdate{{APIKey: "duplicate-key"}})
	if errReplace != nil {
		t.Fatalf("ReplaceAPIKeyEntries(omitted fields) error = %v", errReplace)
	}
	if stats.RequiresRuntimeRefresh() {
		t.Fatal("omitted metadata and bindings unexpectedly required runtime refresh")
	}
	entries, errEntries = repo.ListAPIKeyEntries(ctx)
	if errEntries != nil || len(entries) != 1 || entries[0].DisplayName == nil || *entries[0].DisplayName != lastName {
		t.Fatalf("omitted display name entries = %#v, %v", entries, errEntries)
	}
}

func TestUpdateAPIKeyForUserRejectsDuplicateRename(t *testing.T) {
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
	firstUsername := "first-user"
	secondUsername := "second-user"
	firstUser, errCreateFirstUser := repo.CreateUser(ctx, UserUpdate{Username: &firstUsername})
	if errCreateFirstUser != nil {
		t.Fatalf("CreateUser(first) error = %v", errCreateFirstUser)
	}
	secondUser, errCreateSecondUser := repo.CreateUser(ctx, UserUpdate{Username: &secondUsername})
	if errCreateSecondUser != nil {
		t.Fatalf("CreateUser(second) error = %v", errCreateSecondUser)
	}

	firstKey := "first-client-key"
	secondKey := "second-client-key"
	if _, errCreateFirstKey := repo.CreateAPIKeyForUser(ctx, firstUser.ID, APIKeyUserUpdate{APIKey: &firstKey}); errCreateFirstKey != nil {
		t.Fatalf("CreateAPIKeyForUser(first) error = %v", errCreateFirstKey)
	}
	if _, errCreateSecondKey := repo.CreateAPIKeyForUser(ctx, secondUser.ID, APIKeyUserUpdate{APIKey: &secondKey}); errCreateSecondKey != nil {
		t.Fatalf("CreateAPIKeyForUser(second) error = %v", errCreateSecondKey)
	}

	_, errRenameActive := repo.UpdateAPIKeyForUser(ctx, firstUser.ID, 0, firstKey, APIKeyUserUpdate{APIKey: &secondKey})
	if !errors.Is(errRenameActive, ErrAPIKeyExists) {
		t.Fatalf("UpdateAPIKeyForUser(active duplicate) error = %v, want ErrAPIKeyExists", errRenameActive)
	}

	if errDeleteSecondKey := repo.DeleteAPIKeyForUser(ctx, secondUser.ID, 0, secondKey); errDeleteSecondKey != nil {
		t.Fatalf("DeleteAPIKeyForUser(second) error = %v", errDeleteSecondKey)
	}
	_, errRenameDeleted := repo.UpdateAPIKeyForUser(ctx, firstUser.ID, 0, firstKey, APIKeyUserUpdate{APIKey: &secondKey})
	if !errors.Is(errRenameDeleted, ErrAPIKeyExists) {
		t.Fatalf("UpdateAPIKeyForUser(deleted duplicate) error = %v, want ErrAPIKeyExists", errRenameDeleted)
	}

	records, errList := repo.ListAPIKeyRecordsForUser(ctx, firstUser.ID)
	if errList != nil {
		t.Fatalf("ListAPIKeyRecordsForUser() error = %v", errList)
	}
	if len(records) != 1 || records[0].APIKey != firstKey {
		t.Fatalf("first user API keys = %#v, want only %q", records, firstKey)
	}
}

func openAPIKeyTestRepository(t *testing.T) (*gorm.DB, *Repository) {
	t.Helper()
	db, errOpenSQLite := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
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

func apiKeyEventCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if errCount := db.Model(&ClusterEventRecord{}).Count(&count).Error; errCount != nil {
		t.Fatalf("count cluster events: %v", errCount)
	}
	return count
}

func assertAPIKeyRecordDisplayName(t *testing.T, repo *Repository, id uint, want string) {
	t.Helper()
	entries, errEntries := repo.ListAPIKeyEntries(t.Context())
	if errEntries != nil {
		t.Fatalf("ListAPIKeyEntries() error = %v", errEntries)
	}
	if len(entries) != 1 || entries[0].ID != id || entries[0].DisplayName == nil || *entries[0].DisplayName != want {
		t.Fatalf("API key entry = %#v, want id %d and display name %q", entries, id, want)
	}
}
