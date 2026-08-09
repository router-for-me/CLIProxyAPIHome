package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCPANodeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		value     string
		want      string
		wantError bool
	}{
		{name: "empty", value: "", want: ""},
		{name: "trimmed", value: "  primary CPA  ", want: "primary CPA"},
		{name: "unicode", value: "上海 CPA 🚀", want: "上海 CPA 🚀"},
		{name: "maximum", value: strings.Repeat("a", CPANodeNameMaxLength), want: strings.Repeat("a", CPANodeNameMaxLength)},
		{name: "too long", value: strings.Repeat("a", CPANodeNameMaxLength+1), wantError: true},
		{name: "control character", value: "primary\nCPA", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, errNormalize := NormalizeCPANodeName(test.value)
			if (errNormalize != nil) != test.wantError {
				t.Fatalf("NormalizeCPANodeName() error = %v, want error %t", errNormalize, test.wantError)
			}
			if errNormalize == nil && got != test.want {
				t.Fatalf("NormalizeCPANodeName() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCPANodeMetadataCreateUpdateAndClear(t *testing.T) {
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
	if errCreate := db.Create(&[]CertificateRecord{
		{ID: "cpa-a", IsClient: true},
		{ID: "cpa-b", IsClient: true},
		{ID: "ca", IsCA: true},
	}).Error; errCreate != nil {
		t.Fatalf("create certificates: %v", errCreate)
	}
	repo := NewRepository(db)

	name, errUpdate := repo.UpdateCPANodeName(ctx, "cpa-a", "  Primary CPA  ")
	if errUpdate != nil || name != "Primary CPA" {
		t.Fatalf("UpdateCPANodeName() = %q, %v, want trimmed name", name, errUpdate)
	}
	if _, errUpdate = repo.UpdateCPANodeName(ctx, "cpa-b", "Primary CPA"); errUpdate != nil {
		t.Fatalf("duplicate UpdateCPANodeName() error = %v", errUpdate)
	}
	names, errNames := repo.ListCPANodeNames(ctx, []string{"cpa-a", "cpa-b", "missing", "cpa-a"})
	if errNames != nil {
		t.Fatalf("ListCPANodeNames() error = %v", errNames)
	}
	if names["cpa-a"] != "Primary CPA" || names["cpa-b"] != "Primary CPA" {
		t.Fatalf("names = %#v, want duplicate names preserved", names)
	}

	name, errUpdate = repo.UpdateCPANodeName(ctx, "cpa-a", " ")
	if errUpdate != nil || name != "" {
		t.Fatalf("clear UpdateCPANodeName() = %q, %v", name, errUpdate)
	}
	names, errNames = repo.ListCPANodeNames(ctx, []string{"cpa-a", "cpa-b"})
	if errNames != nil {
		t.Fatalf("ListCPANodeNames() after clear error = %v", errNames)
	}
	if _, exists := names["cpa-a"]; exists {
		t.Fatalf("cleared node name remains in %#v", names)
	}
	if _, errUpdate = repo.UpdateCPANodeName(ctx, "missing", "name"); !errors.Is(errUpdate, ErrCPANodeNotFound) {
		t.Fatalf("missing UpdateCPANodeName() error = %v, want ErrCPANodeNotFound", errUpdate)
	}
	if _, errUpdate = repo.UpdateCPANodeName(ctx, "ca", "name"); !errors.Is(errUpdate, ErrCPANodeNotFound) {
		t.Fatalf("CA UpdateCPANodeName() error = %v, want ErrCPANodeNotFound", errUpdate)
	}
}

func TestCreatePendingClientCertificateStoresNodeName(t *testing.T) {
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
	repo := NewRepository(db)
	if _, errEnsure := repo.EnsureClusterCertificates(ctx, "127.0.0.1"); errEnsure != nil {
		t.Fatalf("EnsureClusterCertificates() error = %v", errEnsure)
	}

	nodeID, _, errCreate := repo.CreatePendingClientCertificate(ctx, "  edge-1  ")
	if errCreate != nil {
		t.Fatalf("CreatePendingClientCertificate() error = %v", errCreate)
	}
	names, errNames := repo.ListCPANodeNames(ctx, []string{nodeID})
	if errNames != nil {
		t.Fatalf("ListCPANodeNames() error = %v", errNames)
	}
	if names[nodeID] != "edge-1" {
		t.Fatalf("created node name = %q, want edge-1", names[nodeID])
	}
}
