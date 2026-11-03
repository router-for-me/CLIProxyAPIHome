package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigOptionalPostgresPoolLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		settings  string
		wantOpen  int
		wantIdle  int
		idleSet   bool
		wantError string
	}{
		{name: "defaults"},
		{name: "total only", settings: "  max-open-conns: 1\n", wantOpen: 1},
		{name: "custom", settings: "  max-open-conns: 32\n  max-idle-conns: 16\n", wantOpen: 32, wantIdle: 16, idleSet: true},
		{name: "idle only", settings: "  max-idle-conns: 16\n", wantIdle: 16, idleSet: true},
		{name: "explicit zero", settings: "  max-open-conns: 0\n  max-idle-conns: 0\n", idleSet: true},
		{name: "equal limits", settings: "  max-open-conns: 8\n  max-idle-conns: 8\n", wantOpen: 8, wantIdle: 8, idleSet: true},
		{name: "negative total", settings: "  max-open-conns: -1\n", wantError: "pgsql.max-open-conns must not be negative"},
		{name: "negative idle", settings: "  max-idle-conns: -1\n", wantError: "pgsql.max-idle-conns must not be negative"},
		{name: "idle exceeds total", settings: "  max-open-conns: 4\n  max-idle-conns: 8\n", wantError: "pgsql.max-idle-conns must not exceed pgsql.max-open-conns"},
	}
	for _, connection := range []struct {
		name   string
		fields string
	}{
		{name: "uri", fields: "  postgres_uri: postgresql://cliproxy:secret@db.example.com/cliproxy_home\n"},
		{name: "individual fields", fields: "  host: db.example.com\n  user: cliproxy\n  database: cliproxy_home\n"},
	} {
		for _, test := range tests {
			t.Run(connection.name+"/"+test.name, func(t *testing.T) {
				t.Parallel()

				configPath := filepath.Join(t.TempDir(), "cluster.yaml")
				content := "pgsql:\n" + connection.fields + test.settings + "node:\n  port: 8327\n"
				if errWrite := os.WriteFile(configPath, []byte(content), 0o600); errWrite != nil {
					t.Fatalf("write config: %v", errWrite)
				}
				cfg, exists, errLoad := LoadConfigOptional(configPath)
				if !exists {
					t.Fatal("expected config to exist")
				}
				if test.wantError != "" {
					if errLoad == nil || !strings.Contains(errLoad.Error(), test.wantError) {
						t.Fatalf("LoadConfigOptional error = %v, want %q", errLoad, test.wantError)
					}
					return
				}
				if errLoad != nil {
					t.Fatalf("LoadConfigOptional: %v", errLoad)
				}
				if cfg.PGSQL.MaxOpenConns != test.wantOpen {
					t.Fatalf("max-open-conns = %d, want %d", cfg.PGSQL.MaxOpenConns, test.wantOpen)
				}
				if (cfg.PGSQL.MaxIdleConns != nil) != test.idleSet {
					t.Fatalf("max-idle-conns configured = %t, want %t", cfg.PGSQL.MaxIdleConns != nil, test.idleSet)
				}
				if test.idleSet && *cfg.PGSQL.MaxIdleConns != test.wantIdle {
					t.Fatalf("max-idle-conns = %d, want %d", *cfg.PGSQL.MaxIdleConns, test.wantIdle)
				}
				if _, errDSN := cfg.PGSQL.DSN(); errDSN != nil {
					t.Fatalf("pool limits must work with either connection format: %v", errDSN)
				}
			})
		}
	}
}

func TestOpenRejectsInvalidPostgresPoolLimitsBeforeConnecting(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		open int
		idle int
	}{
		{name: "negative total", open: -1},
		{name: "negative idle", idle: -1},
		{name: "idle exceeds total", open: 1, idle: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := PGSQLConfig{
				PostgresURI:  "postgresql://cliproxy:secret@db.invalid/cliproxy_home",
				MaxOpenConns: test.open,
				MaxIdleConns: &test.idle,
			}
			_, errOpen := Open(t.Context(), cfg)
			if errOpen == nil || !strings.Contains(errOpen.Error(), "pgsql.max-") {
				t.Fatalf("Open error = %v, want pool validation error", errOpen)
			}
		})
	}
}
