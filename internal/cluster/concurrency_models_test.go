package cluster

import (
	"context"
	"path/filepath"
	"testing"
)

func TestConcurrencyObservationBarrierUpdatedAtIsRequired(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
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

	errInsert := db.Exec(`INSERT INTO credential_concurrency_observation_barrier (id, revision, updated_at) VALUES (?, ?, NULL)`, 1, 0).Error
	if errInsert == nil {
		t.Fatal("barrier insert with null updated_at error = nil")
	}
}
