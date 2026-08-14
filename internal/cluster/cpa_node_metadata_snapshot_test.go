package cluster

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseSnapshotV3IncludesCPANodeMetadata(t *testing.T) {
	t.Parallel()
	models, okModels := databaseSnapshotModels(3)
	if !okModels {
		t.Fatal("database snapshot v3 registry is unsupported")
	}
	if len(models) != len(databaseSnapshotV2Models)+1 {
		t.Fatalf("v3 model count = %d, want v2 count plus metadata", len(models))
	}
	last := models[len(models)-1]
	if last.name != "cpa_node_metadata" || last.restore == false {
		t.Fatalf("last v3 model = %#v, want restorable cpa_node_metadata", last)
	}
}

func TestDatabaseSnapshotRoundTripsCPANodeMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source, errSource := OpenSQLite(ctx, filepath.Join(t.TempDir(), "source.db"))
	if errSource != nil {
		t.Fatalf("OpenSQLite(source) error = %v", errSource)
	}
	sourceSQL, errSourceDB := source.DB()
	if errSourceDB != nil {
		t.Fatalf("source DB() error = %v", errSourceDB)
	}
	t.Cleanup(func() {
		if errClose := sourceSQL.Close(); errClose != nil {
			t.Errorf("close source db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(source); errMigrate != nil {
		t.Fatalf("AutoMigrate(source) error = %v", errMigrate)
	}
	if errCreate := source.Create(&[]CertificateRecord{
		{ID: "cpa-a", IsClient: true},
		{ID: "pending-cpa", EnrollmentSecretHash: "pending"},
	}).Error; errCreate != nil {
		t.Fatalf("create source certificate: %v", errCreate)
	}
	if errCreate := source.Create(&[]CPANodeMetadataRecord{
		{NodeID: "cpa-a", NodeName: "primary-cpa"},
		{NodeID: "pending-cpa", NodeName: "pending-cpa"},
	}).Error; errCreate != nil {
		t.Fatalf("create source node metadata: %v", errCreate)
	}

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.zip")
	manifest, errExport := ExportDatabaseSnapshot(ctx, source, DatabaseSnapshotExportOptions{Path: snapshotPath})
	if errExport != nil {
		t.Fatalf("ExportDatabaseSnapshot() error = %v", errExport)
	}
	if manifest.FormatVersion != databaseSnapshotFormatVersion {
		t.Fatalf("snapshot format version = %d, want %d", manifest.FormatVersion, databaseSnapshotFormatVersion)
	}
	snapshot, errOpenSnapshot := OpenDatabaseSnapshot(ctx, snapshotPath)
	if errOpenSnapshot != nil {
		t.Fatalf("OpenDatabaseSnapshot() error = %v", errOpenSnapshot)
	}
	t.Cleanup(func() {
		if errClose := snapshot.Close(); errClose != nil {
			t.Errorf("close snapshot: %v", errClose)
		}
	})

	target, errTarget := OpenSQLite(ctx, filepath.Join(t.TempDir(), "target.db"))
	if errTarget != nil {
		t.Fatalf("OpenSQLite(target) error = %v", errTarget)
	}
	targetSQL, errTargetDB := target.DB()
	if errTargetDB != nil {
		t.Fatalf("target DB() error = %v", errTargetDB)
	}
	t.Cleanup(func() {
		if errClose := targetSQL.Close(); errClose != nil {
			t.Errorf("close target db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(target); errMigrate != nil {
		t.Fatalf("AutoMigrate(target) error = %v", errMigrate)
	}
	if _, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil); errImport != nil {
		t.Fatalf("ImportDatabaseSnapshot() error = %v", errImport)
	}
	metadata := CPANodeMetadataRecord{}
	if errFind := target.Where("node_id = ?", "cpa-a").First(&metadata).Error; errFind != nil {
		t.Fatalf("load imported metadata: %v", errFind)
	}
	if metadata.NodeName != "primary-cpa" {
		t.Fatalf("imported node_name = %q, want primary-cpa", metadata.NodeName)
	}
	pendingMetadata := CPANodeMetadataRecord{}
	if errFind := target.Where("node_id = ?", "pending-cpa").First(&pendingMetadata).Error; errFind != nil {
		t.Fatalf("load imported pending metadata: %v", errFind)
	}
	if pendingMetadata.NodeName != "pending-cpa" {
		t.Fatalf("imported pending node_name = %q, want pending-cpa", pendingMetadata.NodeName)
	}
}

func TestDatabaseSnapshotRejectsInvalidCPANodeMetadata(t *testing.T) {
	ctx := context.Background()
	source, errSource := OpenSQLite(ctx, filepath.Join(t.TempDir(), "source.db"))
	if errSource != nil {
		t.Fatalf("OpenSQLite(source) error = %v", errSource)
	}
	sourceSQL, errSourceDB := source.DB()
	if errSourceDB != nil {
		t.Fatalf("source DB() error = %v", errSourceDB)
	}
	t.Cleanup(func() {
		if errClose := sourceSQL.Close(); errClose != nil {
			t.Errorf("close source db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(source); errMigrate != nil {
		t.Fatalf("AutoMigrate(source) error = %v", errMigrate)
	}
	if errCreate := source.Create(&CertificateRecord{ID: "cpa-a", IsClient: true}).Error; errCreate != nil {
		t.Fatalf("create source certificate: %v", errCreate)
	}
	if errCreate := source.Create(&CPANodeMetadataRecord{NodeID: "cpa-a", NodeName: "primary-cpa"}).Error; errCreate != nil {
		t.Fatalf("create source node metadata: %v", errCreate)
	}

	sourcePath := filepath.Join(t.TempDir(), "source.zip")
	if _, errExport := ExportDatabaseSnapshot(ctx, source, DatabaseSnapshotExportOptions{Path: sourcePath}); errExport != nil {
		t.Fatalf("ExportDatabaseSnapshot() error = %v", errExport)
	}
	invalidPath := filepath.Join(t.TempDir(), "invalid.zip")
	rewriteDatabaseSnapshotTable(t, sourcePath, invalidPath, "cpa_node_metadata", func(raw []byte) []byte {
		invalidName := strings.Repeat("a", CPANodeNameMaxLength+1)
		updated := bytes.Replace(raw, []byte("primary-cpa"), []byte(invalidName), 1)
		if bytes.Equal(updated, raw) {
			t.Fatal("metadata snapshot row was not updated")
		}
		return updated
	})
	if snapshot, errOpen := OpenDatabaseSnapshot(ctx, invalidPath); errOpen == nil {
		_ = snapshot.Close()
		t.Fatal("OpenDatabaseSnapshot() error = nil, want invalid node_name rejection")
	}
}

func TestDatabaseSnapshotRejectsOrphanedCPANodeMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("DB() error = %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if errCreate := db.Create(&CPANodeMetadataRecord{NodeID: "missing", NodeName: "orphan"}).Error; errCreate != nil {
		t.Fatalf("create orphan metadata: %v", errCreate)
	}
	if errValidate := validateImportedDatabaseSnapshotRelationships(db); errValidate == nil {
		t.Fatal("validateImportedDatabaseSnapshotRelationships() error = nil, want orphan rejection")
	}
}
