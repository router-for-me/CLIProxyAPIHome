package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestInFlightSnapshotCursorRoundTripReadsOnlyRequestedPage(t *testing.T) {
	repo, db := newInFlightCursorTestRepository(t)
	input := inFlightCursorTestInput()

	created, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute)
	if errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	if created.Cursor == "" || created.Total != len(input.Observation.Details) || created.ReadAt.Before(created.CreatedAt) || !created.ExpiresAt.After(created.ReadAt) || created.ExpiresAt.Sub(created.ReadAt) > time.Minute {
		t.Fatalf("created cursor = %#v", created)
	}

	var header ManagementInFlightSnapshotCursorRecord
	if errHeader := db.Where("cursor = ?", created.Cursor).First(&header).Error; errHeader != nil {
		t.Fatalf("read cursor header: %v", errHeader)
	}
	if header.SchemaVersion != inFlightSnapshotCursorSchemaVersion || len(header.Payload) > 64 || strings.Contains(string(header.Payload), "req-") {
		t.Fatalf("cursor header = %#v", header)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorItemRecord{}, created.Cursor, 3)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorObservedRecord{}, created.Cursor, 2)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorStateRecord{}, created.Cursor, 2)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorStateModelRecord{}, created.Cursor, 2)

	page, errRead := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, input.CredentialID, input.Model, 1, 1)
	if errRead != nil {
		t.Fatalf("ReadInFlightSnapshotCursorPage() error = %v", errRead)
	}
	if page.Total != 3 || len(page.Observation.Details) != 1 || page.Observation.Details[0].RequestID != "req-2" {
		t.Fatalf("page = %#v", page)
	}
	if len(page.Observation.Credentials) != 1 || page.Observation.Credentials[0].CredentialID != "cred-b" {
		t.Fatalf("page credentials = %#v", page.Observation.Credentials)
	}
	if page.Observation.Credentials[0].ObservedInFlight != 1 || page.Observation.Credentials[0].ObservedAccounted != 1 || page.Observation.Credentials[0].ObservedUnaccounted != 0 {
		t.Fatalf("page observed projection = %#v", page.Observation.Credentials[0])
	}
	if len(page.States) != 1 || page.States[0].CredentialID != "cred-b" || page.States[0].AdmittedInFlight != 1 || page.States[0].PolicyVersion != 2 || page.States[0].ObservationBarrier != 5 || len(page.States[0].Models) != 1 || page.States[0].Models[0].Model != "claude" || page.States[0].Models[0].MaxInFlight != 1 || page.States[0].Models[0].AdmittedInFlight != 1 {
		t.Fatalf("page states = %#v", page.States)
	}
	if page.Observation.ObservedAt == nil || input.Observation.ObservedAt == nil || !page.Observation.ObservedAt.Equal(*input.Observation.ObservedAt) || page.Observation.FreshUntil == nil || input.Observation.FreshUntil == nil || !page.Observation.FreshUntil.Equal(*input.Observation.FreshUntil) {
		t.Fatalf("page freshness = %#v", page.Observation)
	}

	lastPage, errLast := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, input.CredentialID, input.Model, 2, 10)
	if errLast != nil {
		t.Fatalf("last ReadInFlightSnapshotCursorPage() error = %v", errLast)
	}
	if len(lastPage.Observation.Details) != 1 || lastPage.Observation.Details[0].RequestID != "req-3" || len(lastPage.Observation.Credentials) != 1 || lastPage.Observation.Credentials[0].CredentialID != "cred-a" {
		t.Fatalf("last page = %#v", lastPage)
	}

	past := time.Now().UTC().Add(-time.Minute)
	if errExpire := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", created.Cursor).
		Update("expires_at", past).Error; errExpire != nil {
		t.Fatalf("expire cursor: %v", errExpire)
	}
	if _, errExpired := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, input.CredentialID, input.Model, 0, 1); !errors.Is(errExpired, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("expired cursor error = %v, want %v", errExpired, ErrInFlightSnapshotCursorExpired)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorRecord{}, created.Cursor, 1)
	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute); errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() after expiry error = %v", errCreate)
	}
	assertInFlightCursorDeleted(t, db, created.Cursor)
	if _, errInvalid := repo.ReadInFlightSnapshotCursorPage(t.Context(), "not-a-cursor", "", "", 0, 1); !errors.Is(errInvalid, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("invalid cursor error = %v, want %v", errInvalid, ErrInFlightSnapshotCursorExpired)
	}
}

func TestInFlightSnapshotCursorRejectsFilterAndLegacySchema(t *testing.T) {
	repo, db := newInFlightCursorTestRepository(t)
	input := inFlightCursorTestInput()
	created, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute)
	if errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}

	if _, errMismatch := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, "wrong", input.Model, 0, 1); !errors.Is(errMismatch, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("filter mismatch error = %v, want %v", errMismatch, ErrInFlightSnapshotCursorExpired)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorRecord{}, created.Cursor, 1)
	if _, errRead := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, input.CredentialID, input.Model, 0, 1); errRead != nil {
		t.Fatalf("cursor read after filter mismatch error = %v", errRead)
	}

	if errLegacy := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", created.Cursor).
		Update("schema_version", 0).Error; errLegacy != nil {
		t.Fatalf("mark cursor legacy: %v", errLegacy)
	}
	if _, errLegacy := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, input.CredentialID, input.Model, 0, 1); !errors.Is(errLegacy, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("legacy cursor error = %v, want %v", errLegacy, ErrInFlightSnapshotCursorExpired)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorRecord{}, created.Cursor, 1)
	if errExpire := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", created.Cursor).
		Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; errExpire != nil {
		t.Fatalf("expire legacy cursor: %v", errExpire)
	}
	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute); errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() after legacy cursor error = %v", errCreate)
	}
	assertInFlightCursorDeleted(t, db, created.Cursor)
}

func TestInFlightSnapshotCursorConcurrentLegacyReadsDoNotDeadlockPostgres(t *testing.T) {
	repo, db := newInFlightCursorPostgresTestRepository(t)
	input := inFlightCursorTestInput()
	created, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute)
	if errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	if errLegacy := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", created.Cursor).
		Update("schema_version", 0).Error; errLegacy != nil {
		t.Fatalf("mark cursor legacy: %v", errLegacy)
	}

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var queryCount atomic.Int32
	if errCallback := db.Callback().Query().After("gorm:query").Register("in_flight_cursor_legacy_read_barrier", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != (ManagementInFlightSnapshotCursorRecord{}).TableName() || queryCount.Add(1) > 2 {
			return
		}
		arrived <- struct{}{}
		<-release
	}); errCallback != nil {
		t.Fatalf("register query callback: %v", errCallback)
	}

	ctx, cancelCtx := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCtx()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, errRead := repo.ReadInFlightSnapshotCursorPage(ctx, created.Cursor, input.CredentialID, input.Model, 0, 1)
			results <- errRead
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatalf("concurrent cursor reads did not acquire shared locks: %v", ctx.Err())
		}
	}
	updateStarted := make(chan struct{})
	updateDone := make(chan error, 1)
	go func() {
		close(updateStarted)
		errUpdate := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			header := ManagementInFlightSnapshotCursorRecord{}
			return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("cursor = ?", created.Cursor).First(&header).Error
		})
		updateDone <- errUpdate
	}()
	<-updateStarted
	select {
	case errUpdate := <-updateDone:
		close(release)
		t.Fatalf("update lock completed while shared locks were held: %v", errUpdate)
	case <-time.After(200 * time.Millisecond):
	case <-ctx.Done():
		close(release)
		t.Fatalf("waiting for blocked update lock: %v", ctx.Err())
	}
	close(release)
	for range 2 {
		select {
		case errRead := <-results:
			if !errors.Is(errRead, ErrInFlightSnapshotCursorExpired) {
				t.Fatalf("concurrent legacy cursor error = %v, want %v", errRead, ErrInFlightSnapshotCursorExpired)
			}
		case <-ctx.Done():
			t.Fatalf("concurrent cursor reads did not finish: %v", ctx.Err())
		}
	}
	select {
	case errUpdate := <-updateDone:
		if errUpdate != nil {
			t.Fatalf("update lock after shared reads error = %v", errUpdate)
		}
	case <-ctx.Done():
		t.Fatalf("update lock did not finish after shared reads: %v", ctx.Err())
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorRecord{}, created.Cursor, 1)
}

func TestInFlightSnapshotCursorMigratesAndExpiresLegacyBlobRows(t *testing.T) {
	db, errOpen := OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
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
	if errTable := db.Exec(`CREATE TABLE management_in_flight_snapshot_cursors (
		cursor text PRIMARY KEY,
		payload blob NOT NULL,
		expires_at datetime NOT NULL,
		created_at datetime NOT NULL
	)`).Error; errTable != nil {
		t.Fatalf("create legacy cursor table: %v", errTable)
	}
	legacyCursor, errToken := newInFlightSnapshotCursorToken()
	if errToken != nil {
		t.Fatalf("newInFlightSnapshotCursorToken() error = %v", errToken)
	}
	now := time.Now().UTC()
	if errSeed := db.Exec(
		"INSERT INTO management_in_flight_snapshot_cursors (cursor, payload, expires_at, created_at) VALUES (?, ?, ?, ?)",
		legacyCursor,
		[]byte(`{"observation":{"details":[]}}`),
		now.Add(time.Minute),
		now,
	).Error; errSeed != nil {
		t.Fatalf("seed legacy cursor: %v", errSeed)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	repo := NewRepository(db)
	if _, errRead := repo.ReadInFlightSnapshotCursorPage(t.Context(), legacyCursor, "", "", 0, 1); !errors.Is(errRead, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("legacy cursor error = %v, want %v", errRead, ErrInFlightSnapshotCursorExpired)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorRecord{}, legacyCursor, 1)
	if errExpire := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", legacyCursor).
		Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; errExpire != nil {
		t.Fatalf("expire legacy cursor: %v", errExpire)
	}
	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), inFlightCursorTestInput(), time.Minute); errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() after migration error = %v", errCreate)
	}
	assertInFlightCursorDeleted(t, db, legacyCursor)
}

func TestInFlightSnapshotCursorDetectsIncompleteRelations(t *testing.T) {
	testCases := []struct {
		name      string
		remove    func(*gorm.DB, string) error
		wantError string
	}{
		{
			name: "item",
			remove: func(db *gorm.DB, cursor string) error {
				return db.Where("cursor = ? AND ordinal = ?", cursor, 0).Delete(&ManagementInFlightSnapshotCursorItemRecord{}).Error
			},
			wantError: "page ordinal is invalid",
		},
		{
			name: "observed",
			remove: func(db *gorm.DB, cursor string) error {
				return db.Where("cursor = ? AND credential_id = ?", cursor, "cred-a").Delete(&ManagementInFlightSnapshotCursorObservedRecord{}).Error
			},
			wantError: "observed state is incomplete",
		},
		{
			name: "state",
			remove: func(db *gorm.DB, cursor string) error {
				return db.Where("cursor = ? AND credential_id = ?", cursor, "cred-a").Delete(&ManagementInFlightSnapshotCursorStateRecord{}).Error
			},
			wantError: "limiter state is incomplete",
		},
		{
			name: "state model",
			remove: func(db *gorm.DB, cursor string) error {
				return db.Where("cursor = ? AND credential_id = ?", cursor, "cred-a").Delete(&ManagementInFlightSnapshotCursorStateModelRecord{}).Error
			},
			wantError: "limiter models are incomplete",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			repo, db := newInFlightCursorTestRepository(t)
			input := inFlightCursorTestInput()
			created, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute)
			if errCreate != nil {
				t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
			}
			if errRemove := testCase.remove(db, created.Cursor); errRemove != nil {
				t.Fatalf("remove relation row: %v", errRemove)
			}
			_, errRead := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, input.CredentialID, input.Model, 0, 1)
			if errRead == nil || !strings.Contains(errRead.Error(), testCase.wantError) {
				t.Fatalf("ReadInFlightSnapshotCursorPage() error = %v, want %q", errRead, testCase.wantError)
			}
		})
	}
}

func TestCreateInFlightSnapshotCursorCleansOneExpiredBatch(t *testing.T) {
	repo, db := newInFlightCursorTestRepository(t)
	past := time.Now().UTC().Add(-time.Hour)
	headers := make([]ManagementInFlightSnapshotCursorRecord, 0, inFlightSnapshotCursorCleanupBatchSize+1)
	items := make([]ManagementInFlightSnapshotCursorItemRecord, 0, cap(headers))
	for index := 0; index < cap(headers); index++ {
		cursor, errToken := newInFlightSnapshotCursorToken()
		if errToken != nil {
			t.Fatalf("newInFlightSnapshotCursorToken() error = %v", errToken)
		}
		headers = append(headers, ManagementInFlightSnapshotCursorRecord{
			Cursor: cursor, SchemaVersion: inFlightSnapshotCursorSchemaVersion, Payload: JSONB(`{"schema_version":1}`), ExpiresAt: past, CreatedAt: past.Add(-time.Minute),
		})
		items = append(items, ManagementInFlightSnapshotCursorItemRecord{
			Cursor: cursor, Ordinal: 0, RequestID: "old", CredentialID: "cred", Model: "model", RequestKind: "sse", StartedAt: past,
		})
	}
	if errHeaders := db.CreateInBatches(headers, 100).Error; errHeaders != nil {
		t.Fatalf("seed expired cursor headers: %v", errHeaders)
	}
	if errItems := db.CreateInBatches(items, 100).Error; errItems != nil {
		t.Fatalf("seed expired cursor items: %v", errItems)
	}
	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), inFlightCursorTestInput(), time.Minute); errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}

	var expiredHeaders int64
	if errCount := db.Model(&ManagementInFlightSnapshotCursorRecord{}).Where("expires_at <= ?", time.Now().UTC()).Count(&expiredHeaders).Error; errCount != nil {
		t.Fatalf("count expired headers: %v", errCount)
	}
	if expiredHeaders != 1 {
		t.Fatalf("expired header count = %d, want 1", expiredHeaders)
	}
	var expiredItems int64
	if errCount := db.Model(&ManagementInFlightSnapshotCursorItemRecord{}).Where("request_id = ?", "old").Count(&expiredItems).Error; errCount != nil {
		t.Fatalf("count expired items: %v", errCount)
	}
	if expiredItems != 1 {
		t.Fatalf("expired item count = %d, want 1", expiredItems)
	}
}

func TestInFlightSnapshotCursorCleanupBoundsRelationRows(t *testing.T) {
	repo, db := newInFlightCursorTestRepository(t)
	cursor, errToken := newInFlightSnapshotCursorToken()
	if errToken != nil {
		t.Fatalf("newInFlightSnapshotCursorToken() error = %v", errToken)
	}
	past := time.Now().UTC().Add(-time.Hour)
	header := ManagementInFlightSnapshotCursorRecord{
		Cursor: cursor, SchemaVersion: inFlightSnapshotCursorSchemaVersion, Payload: JSONB(`{"schema_version":1}`), ExpiresAt: past, CreatedAt: past.Add(-time.Minute),
	}
	if errHeader := db.Create(&header).Error; errHeader != nil {
		t.Fatalf("seed expired cursor header: %v", errHeader)
	}
	items := make([]ManagementInFlightSnapshotCursorItemRecord, 0, inFlightSnapshotCursorCleanupRowBatchSize+1)
	for index := 0; index < cap(items); index++ {
		items = append(items, ManagementInFlightSnapshotCursorItemRecord{
			Cursor: cursor, Ordinal: int64(index), RequestID: fmt.Sprintf("old-%04d", index), CredentialID: "cred", Model: "model", RequestKind: "sse", StartedAt: past,
		})
	}
	if errItems := db.CreateInBatches(items, inFlightSnapshotCursorWriteBatchSize).Error; errItems != nil {
		t.Fatalf("seed expired cursor items: %v", errItems)
	}

	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), inFlightCursorTestInput(), time.Minute); errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorRecord{}, cursor, 1)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorItemRecord{}, cursor, 1)

	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), inFlightCursorTestInput(), time.Minute); errCreate != nil {
		t.Fatalf("second CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	assertInFlightCursorDeleted(t, db, cursor)
}

func TestInFlightSnapshotCursorBatchesLargeRelationalView(t *testing.T) {
	repo, db := newInFlightCursorTestRepository(t)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	input := InFlightSnapshotCursorInput{
		Observation: InFlightObservationReadModel{
			ObservedAt:       &now,
			CoverageComplete: true,
			Credentials:      make([]InFlightObservedCredentialItem, 0, 1001),
			Details:          make([]InFlightRequestDetail, 0, 1001),
		},
		States: make([]CredentialConcurrencyState, 0, 1001),
	}
	for index := 0; index < 1001; index++ {
		credentialID := fmt.Sprintf("cred-%04d", index)
		model := fmt.Sprintf("model-%04d", index)
		input.Observation.Credentials = append(input.Observation.Credentials, InFlightObservedCredentialItem{
			CredentialID: credentialID, ObservedInFlight: 1, ObservedAccounted: 1,
		})
		input.Observation.Details = append(input.Observation.Details, InFlightRequestDetail{
			RequestID: fmt.Sprintf("req-%04d-%s", index, strings.Repeat("x", 240)), CredentialID: credentialID, Model: model, RequestKind: "sse", StartedAt: now,
		})
		input.States = append(input.States, CredentialConcurrencyState{
			CredentialID: credentialID, PolicyVersion: 1, EffectiveAt: now, Models: []CredentialConcurrencyModelState{{Model: model, MaxInFlight: 2, AdmittedInFlight: 1}},
		})
	}

	created, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), input, time.Minute)
	if errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorItemRecord{}, created.Cursor, 1001)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorObservedRecord{}, created.Cursor, 1001)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorStateRecord{}, created.Cursor, 1001)
	assertInFlightCursorRowCount(t, db, &ManagementInFlightSnapshotCursorStateModelRecord{}, created.Cursor, 1001)
	page, errRead := repo.ReadInFlightSnapshotCursorPage(t.Context(), created.Cursor, "", "", 1, 1000)
	if errRead != nil {
		t.Fatalf("ReadInFlightSnapshotCursorPage() error = %v", errRead)
	}
	if page.Total != 1001 || len(page.Observation.Details) != 1000 || len(page.Observation.Credentials) != 1000 || len(page.States) != 1000 || len(page.States[0].Models) != 1 {
		t.Fatalf("large cursor page = total %d details %d credentials %d states %d", page.Total, len(page.Observation.Details), len(page.Observation.Credentials), len(page.States))
	}
	if page.Observation.Details[0].CredentialID != "cred-0001" || page.Observation.Details[999].CredentialID != "cred-1000" {
		t.Fatalf("large cursor detail bounds = %#v ... %#v", page.Observation.Details[0], page.Observation.Details[999])
	}
}

func TestCreateInFlightSnapshotCursorRejectsInvalidTTL(t *testing.T) {
	repo, _ := newInFlightCursorTestRepository(t)
	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), inFlightCursorTestInput(), 0); errCreate == nil {
		t.Fatal("CreateInFlightSnapshotCursor() accepted a non-positive ttl")
	}
}

func newInFlightCursorPostgresTestRepository(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not configured")
	}
	adminDB, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig())
	if errOpen != nil {
		t.Fatalf("open postgres admin database: %v", errOpen)
	}
	adminSQLDB, errAdminDB := adminDB.DB()
	if errAdminDB != nil {
		t.Fatalf("get postgres admin database: %v", errAdminDB)
	}
	schema := "in_flight_cursor_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := adminDB.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		t.Fatalf("create postgres schema: %v", errCreate)
	}
	db, errSchemaOpen := gorm.Open(postgres.Open(postgresDSNWithSearchPath(dsn, schema)), databaseGORMConfig())
	if errSchemaOpen != nil {
		if errDrop := adminDB.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres schema after open failure: %v", errDrop)
		}
		if errClose := adminSQLDB.Close(); errClose != nil {
			t.Errorf("close postgres admin database: %v", errClose)
		}
		t.Fatalf("open postgres schema database: %v", errSchemaOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get postgres database: %v", errDB)
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres database: %v", errClose)
		}
		if errDrop := adminDB.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres schema: %v", errDrop)
		}
		if errClose := adminSQLDB.Close(); errClose != nil {
			t.Errorf("close postgres admin database: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewRepository(db), db
}

func newInFlightCursorTestRepository(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	db, errOpen := OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
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
	return NewRepository(db), db
}

func inFlightCursorTestInput() InFlightSnapshotCursorInput {
	observedAt := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	freshUntil := observedAt.Add(10 * time.Minute)
	barrier := int64(7)
	maxA := int64(3)
	return InFlightSnapshotCursorInput{
		CredentialID: "filter-credential",
		Model:        "filter-model",
		Observation: InFlightObservationReadModel{
			ObservedAt:                      &observedAt,
			FreshUntil:                      &freshUntil,
			CoverageComplete:                true,
			AggregatesComplete:              true,
			ProtocolCoverageComplete:        true,
			MinimumProcessedBarrierRevision: &barrier,
			Credentials: []InFlightObservedCredentialItem{
				{CredentialID: "cred-a", ObservedInFlight: 2, ObservedAccounted: 1, ObservedUnaccounted: 1},
				{CredentialID: "cred-b", ObservedInFlight: 1, ObservedAccounted: 1},
			},
			Details: []InFlightRequestDetail{
				{RequestID: "req-1", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: observedAt.Add(-3 * time.Second)},
				{RequestID: "req-2", CredentialID: "cred-b", Model: "claude", RequestKind: "websocket", StartedAt: observedAt.Add(-2 * time.Second)},
				{RequestID: "req-3", CredentialID: "cred-a", Model: "gpt-5", RequestKind: "sse", StartedAt: observedAt.Add(-time.Second)},
			},
		},
		States: []CredentialConcurrencyState{
			{CredentialID: "cred-a", MaxInFlight: &maxA, AdmittedInFlight: 2, PolicyVersion: 4, EffectiveAt: observedAt, ObservationBarrier: 7, Models: []CredentialConcurrencyModelState{{Model: "gpt-5", MaxInFlight: 2, AdmittedInFlight: 1}}},
			{CredentialID: "cred-b", AdmittedInFlight: 1, PolicyVersion: 2, EffectiveAt: observedAt, ObservationBarrier: 5, Models: []CredentialConcurrencyModelState{{Model: "claude", MaxInFlight: 1, AdmittedInFlight: 1}}},
		},
	}
}

func assertInFlightCursorRowCount(t *testing.T, db *gorm.DB, model any, cursor string, want int64) {
	t.Helper()
	var count int64
	if errCount := db.Model(model).Where("cursor = ?", cursor).Count(&count).Error; errCount != nil {
		t.Fatalf("count cursor rows: %v", errCount)
	}
	if count != want {
		t.Fatalf("cursor row count = %d, want %d", count, want)
	}
}

func assertInFlightCursorDeleted(t *testing.T, db *gorm.DB, cursor string) {
	t.Helper()
	for _, model := range []any{
		&ManagementInFlightSnapshotCursorRecord{},
		&ManagementInFlightSnapshotCursorItemRecord{},
		&ManagementInFlightSnapshotCursorObservedRecord{},
		&ManagementInFlightSnapshotCursorStateRecord{},
		&ManagementInFlightSnapshotCursorStateModelRecord{},
	} {
		assertInFlightCursorRowCount(t, db, model, cursor, 0)
	}
}
