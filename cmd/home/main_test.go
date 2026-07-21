package main

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestLoadInitialRuntimeConfigReadsEventHighWaterBeforeSnapshot(t *testing.T) {
	wantConfig := &config.Config{Debug: true}
	repo := &startupConfigRepositoryStub{cfg: wantConfig, maxEventID: 41}

	highWater, gotConfig, errLoad := loadInitialRuntimeConfig(context.Background(), repo)
	if errLoad != nil {
		t.Fatalf("loadInitialRuntimeConfig() error = %v", errLoad)
	}
	if highWater != 41 {
		t.Fatalf("high-water = %d, want 41", highWater)
	}
	if gotConfig != wantConfig {
		t.Fatalf("config = %p, want %p", gotConfig, wantConfig)
	}
	wantActions := []string{"max-event-id", "load-config"}
	if strings.Join(repo.actions, ",") != strings.Join(wantActions, ",") {
		t.Fatalf("repository actions = %#v, want %#v", repo.actions, wantActions)
	}
}

type startupConfigRepositoryStub struct {
	cfg        *config.Config
	payload    []byte
	maxEventID int64
	actions    []string
}

func (s *startupConfigRepositoryStub) LoadConfigAsRuntimeConfig(context.Context) (*config.Config, []byte, error) {
	s.actions = append(s.actions, "load-config")
	return s.cfg, append([]byte(nil), s.payload...), nil
}

func (s *startupConfigRepositoryStub) MaxEventID(context.Context) (int64, error) {
	s.actions = append(s.actions, "max-event-id")
	return s.maxEventID, nil
}

func TestResolveClusterAdvertisedPortUsesExternalPort(t *testing.T) {
	t.Parallel()

	cfg := &cluster.Config{
		Node: cluster.NodeConfig{
			ExternalPort: 443,
			Port:         8327,
		},
	}

	port, errPort := resolveClusterAdvertisedPort(cfg, 8327)
	if errPort != nil {
		t.Fatalf("resolveClusterAdvertisedPort failed: %v", errPort)
	}
	if port != 443 {
		t.Fatalf("advertised port = %d, want 443", port)
	}
}

func TestResolveClusterAdvertisedPortFallsBackToListenPort(t *testing.T) {
	t.Parallel()

	cfg := &cluster.Config{
		Node: cluster.NodeConfig{
			Port: 8327,
		},
	}

	port, errPort := resolveClusterAdvertisedPort(cfg, 18327)
	if errPort != nil {
		t.Fatalf("resolveClusterAdvertisedPort failed: %v", errPort)
	}
	if port != 18327 {
		t.Fatalf("advertised port = %d, want 18327", port)
	}
}

func TestResolveSQLitePath_UsesFlagOverride(t *testing.T) {
	t.Parallel()

	got := resolveSQLitePath("flag.db", "config.db")
	if got != "flag.db" {
		t.Fatalf("resolveSQLitePath() = %q, want flag.db", got)
	}
}

func TestResolveSQLitePath_UsesConfigFallback(t *testing.T) {
	t.Parallel()

	got := resolveSQLitePath("", "config.db")
	if got != "config.db" {
		t.Fatalf("resolveSQLitePath() = %q, want config.db", got)
	}
}

func TestResolveSQLitePath_UsesDefault(t *testing.T) {
	t.Parallel()

	got := resolveSQLitePath("", "")
	if got != "home.db" {
		t.Fatalf("resolveSQLitePath() = %q, want home.db", got)
	}
}

func TestDatabaseSnapshotOneTimeModeCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		importState        bool
		exportState        bool
		databaseImportPath string
		databaseExportPath string
		want               int
	}{
		{name: "none", want: 0},
		{name: "local import", importState: true, want: 1},
		{name: "local export", exportState: true, want: 1},
		{name: "database import", databaseImportPath: "snapshot.zip", want: 1},
		{name: "database export", databaseExportPath: "snapshot.zip", want: 1},
		{name: "all", importState: true, exportState: true, databaseImportPath: "in.zip", databaseExportPath: "out.zip", want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := oneTimeModeCount(test.importState, test.exportState, test.databaseImportPath, test.databaseExportPath)
			if got != test.want {
				t.Fatalf("oneTimeModeCount() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDatabaseSnapshotResolveRuntimeDatabaseBackend(t *testing.T) {
	t.Parallel()

	if got := resolveRuntimeDatabaseBackend(nil, false); got != cluster.DatabaseBackendSQLite {
		t.Fatalf("non-cluster backend = %q, want sqlite", got)
	}
	postgresConfig := &cluster.Config{PGSQL: cluster.PGSQLConfig{Host: "postgres"}}
	if got := resolveRuntimeDatabaseBackend(postgresConfig, true); got != cluster.DatabaseBackendPostgres {
		t.Fatalf("cluster backend = %q, want postgres", got)
	}
}

func TestExportOptionsForDir_UsesDefaultAuthDirWithoutOutputDir(t *testing.T) {
	t.Parallel()

	opts := exportOptionsForDir("", nil)
	if opts.OutputDir != "" {
		t.Fatalf("OutputDir = %q, want empty", opts.OutputDir)
	}
	if opts.AuthDirName != "" {
		t.Fatalf("AuthDirName = %q, want empty for backend default", opts.AuthDirName)
	}
}

func TestExportOptionsForDir_UsesAuthsForExplicitOutputDir(t *testing.T) {
	t.Parallel()

	opts := exportOptionsForDir("out", nil)
	if opts.OutputDir != "out" {
		t.Fatalf("OutputDir = %q, want out", opts.OutputDir)
	}
	if opts.AuthDirName != "auths" {
		t.Fatalf("AuthDirName = %q, want auths", opts.AuthDirName)
	}
}

func TestResolveDatabaseNodeIP_RejectsClusterSQLiteWithoutExternalIP(t *testing.T) {
	t.Parallel()

	cfg := &cluster.Config{
		SQLite: cluster.SQLiteConfig{Path: "home.db"},
	}
	got, errNodeIP := resolveDatabaseNodeIP(context.Background(), nil, cluster.DatabaseBackendSQLite, cfg, true)
	if errNodeIP == nil {
		t.Fatalf("resolveDatabaseNodeIP() error = nil, want node.external-ip error")
	}
	if got != "" {
		t.Fatalf("resolveDatabaseNodeIP() = %q, want empty ip on error", got)
	}
	if !strings.Contains(errNodeIP.Error(), "node.external-ip is required when cluster uses sqlite backend") {
		t.Fatalf("resolveDatabaseNodeIP() error = %v, want node.external-ip sqlite error", errNodeIP)
	}
}

func TestResolveDatabaseNodeIP_UsesLoopbackForNonClusterSQLite(t *testing.T) {
	t.Parallel()

	got, errNodeIP := resolveDatabaseNodeIP(context.Background(), nil, cluster.DatabaseBackendSQLite, nil, false)
	if errNodeIP != nil {
		t.Fatalf("resolveDatabaseNodeIP() error = %v", errNodeIP)
	}
	if got != "127.0.0.1" {
		t.Fatalf("resolveDatabaseNodeIP() = %q, want 127.0.0.1", got)
	}
}
