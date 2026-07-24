package cluster

import (
	"errors"
	"testing"
	"time"
)

func TestInFlightSnapshotCursorRoundTripAndExpiry(t *testing.T) {
	db, errOpen := OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
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
	if _, errInvalidPayload := repo.CreateInFlightSnapshotCursor(t.Context(), []byte(`not-json`), time.Minute); errInvalidPayload == nil {
		t.Fatal("CreateInFlightSnapshotCursor() accepted invalid JSON")
	}

	created, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), []byte(`{"safe":true}`), time.Minute)
	if errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	if created.Cursor == "" || !created.ExpiresAt.After(created.CreatedAt) {
		t.Fatalf("created cursor = %#v", created)
	}

	read, errRead := repo.ReadInFlightSnapshotCursor(t.Context(), created.Cursor)
	if errRead != nil {
		t.Fatalf("ReadInFlightSnapshotCursor() error = %v", errRead)
	}
	if string(read.Payload) != `{"safe":true}` || !read.ExpiresAt.Equal(created.ExpiresAt) {
		t.Fatalf("read cursor = %#v", read)
	}
	read.Payload[0] = 'x'
	readAgain, errReadAgain := repo.ReadInFlightSnapshotCursor(t.Context(), created.Cursor)
	if errReadAgain != nil {
		t.Fatalf("second ReadInFlightSnapshotCursor() error = %v", errReadAgain)
	}
	if string(readAgain.Payload) != `{"safe":true}` {
		t.Fatalf("stored payload mutated = %q", readAgain.Payload)
	}

	past := time.Now().UTC().Add(-time.Minute)
	if errExpire := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", created.Cursor).
		Update("expires_at", past).Error; errExpire != nil {
		t.Fatalf("expire cursor: %v", errExpire)
	}
	if _, errExpired := repo.ReadInFlightSnapshotCursor(t.Context(), created.Cursor); !errors.Is(errExpired, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("expired cursor error = %v, want %v", errExpired, ErrInFlightSnapshotCursorExpired)
	}
	if _, errInvalid := repo.ReadInFlightSnapshotCursor(t.Context(), "not-a-cursor"); !errors.Is(errInvalid, ErrInFlightSnapshotCursorExpired) {
		t.Fatalf("invalid cursor error = %v, want %v", errInvalid, ErrInFlightSnapshotCursorExpired)
	}
}

func TestValidateInFlightSnapshotCursorPayloadBounds(t *testing.T) {
	if errEmpty := validateInFlightSnapshotCursorPayload(nil, 16); errEmpty == nil {
		t.Fatal("validateInFlightSnapshotCursorPayload() accepted an empty payload")
	}
	if errInvalid := validateInFlightSnapshotCursorPayload([]byte(`not-json`), 16); errInvalid == nil {
		t.Fatal("validateInFlightSnapshotCursorPayload() accepted invalid JSON")
	}
	if errLarge := validateInFlightSnapshotCursorPayload([]byte(`{"value":"large"}`), 8); errLarge == nil {
		t.Fatal("validateInFlightSnapshotCursorPayload() accepted an oversized payload")
	}
	if errValid := validateInFlightSnapshotCursorPayload([]byte(`{"ok":true}`), 16); errValid != nil {
		t.Fatalf("validateInFlightSnapshotCursorPayload() error = %v", errValid)
	}
}

func TestCreateInFlightSnapshotCursorPurgesExpiredRows(t *testing.T) {
	db, errOpen := OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
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

	expiredToken, errToken := newInFlightSnapshotCursorToken()
	if errToken != nil {
		t.Fatalf("newInFlightSnapshotCursorToken() error = %v", errToken)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if errSeed := db.Create(&ManagementInFlightSnapshotCursorRecord{
		Cursor: expiredToken, Payload: JSONB(`{"old":true}`), CreatedAt: past.Add(-time.Minute), ExpiresAt: past,
	}).Error; errSeed != nil {
		t.Fatalf("seed expired cursor: %v", errSeed)
	}
	if _, errCreate := repo.CreateInFlightSnapshotCursor(t.Context(), []byte(`{"new":true}`), time.Minute); errCreate != nil {
		t.Fatalf("CreateInFlightSnapshotCursor() error = %v", errCreate)
	}
	var count int64
	if errCount := db.Model(&ManagementInFlightSnapshotCursorRecord{}).
		Where("cursor = ?", expiredToken).
		Count(&count).Error; errCount != nil {
		t.Fatalf("count expired cursor: %v", errCount)
	}
	if count != 0 {
		t.Fatalf("expired cursor count = %d, want 0", count)
	}
}
