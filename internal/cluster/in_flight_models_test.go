package cluster

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestInFlightAutoMigrateCreatesObservationTables(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	migrator := db.Migrator()
	for _, table := range []string{
		"cpa_in_flight_snapshots",
		"cpa_in_flight_snapshot_attempts",
		"cpa_in_flight_snapshot_parts",
		"management_in_flight_snapshot_cursors",
		"management_in_flight_snapshot_cursor_items",
		"management_in_flight_snapshot_cursor_observed",
		"management_in_flight_snapshot_cursor_states",
		"management_in_flight_snapshot_cursor_state_models",
	} {
		if !migrator.HasTable(table) {
			t.Fatalf("missing table %s", table)
		}
	}
}

func TestInFlightPartPayloadPreservesRawBytes(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	payload := []byte(` {"a": 1, "b": [2, 3]}\n`)
	if errInsert := db.Exec(`INSERT INTO cpa_in_flight_snapshot_parts (
		certificate_fingerprint, membership_connected_at, revision, part_index,
		payload, encoded_bytes, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		"fingerprint", "2026-01-02 03:04:05", 1, 0, payload, len(payload)).Error; errInsert != nil {
		t.Fatalf("part insert error = %v", errInsert)
	}

	var stored struct {
		Payload []byte
	}
	if errQuery := db.Raw(`SELECT payload FROM cpa_in_flight_snapshot_parts WHERE certificate_fingerprint = ?`, "fingerprint").Scan(&stored).Error; errQuery != nil {
		t.Fatalf("part payload query error = %v", errQuery)
	}
	if !bytes.Equal(stored.Payload, payload) {
		t.Fatalf("stored payload = %q, want %q", stored.Payload, payload)
	}
}

func TestInFlightAttemptStateConstraint(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	errInsert := db.Exec(`INSERT INTO cpa_in_flight_snapshot_attempts (
		certificate_fingerprint, membership_connected_at, highest_seen_revision, state,
		observed_at, barrier_revision, part_count, received_part_count, encoded_bytes,
		aggregate_group_count, details_truncated, updated_at
	) VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		"fingerprint", "2026-01-02 03:04:05", 1, "invalid", 1, 1, 1, 10, 1, false).Error
	if errInsert == nil {
		t.Fatal("invalid attempt state insert error = nil")
	}
}

func TestInFlightPartUniqueKey(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	insert := func(connectedAt string, payload []byte) error {
		return db.Exec(`INSERT INTO cpa_in_flight_snapshot_parts (
            certificate_fingerprint, membership_connected_at, revision, part_index,
            payload, encoded_bytes, created_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			"fingerprint", connectedAt, 1, 0, payload, len(payload)).Error
	}

	if errInsert := insert("2026-01-02 03:04:05", []byte(` {"a": 1}`)); errInsert != nil {
		t.Fatalf("first insert error = %v", errInsert)
	}
	if errInsert := insert("2026-01-02 03:04:05", []byte(`{"a":1}`)); errInsert == nil {
		t.Fatal("duplicate part insert error = nil")
	}
	if errInsert := insert("2026-01-02 03:04:06", []byte(`{"a":1}`)); errInsert != nil {
		t.Fatalf("different membership lifetime insert error = %v", errInsert)
	}
}
