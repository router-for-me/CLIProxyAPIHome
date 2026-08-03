package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCodexAlphaSearchImportDatabaseExportRoundTrip(t *testing.T) {
	inputDir := t.TempDir()
	configPath := filepath.Join(inputDir, "config.yaml")
	writeFile(t, configPath, `codex-api-key:
  - api-key: codex-key
    base-url: https://codex.example/v1
    alpha-search: true
`)
	repo := NewRepository(openImportTestSQLite(t))
	if _, errImport := ImportLocalState(context.Background(), ImportOptions{ConfigPath: configPath, Repository: repo}); errImport != nil {
		t.Fatalf("ImportLocalState() error = %v", errImport)
	}

	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() error = %v", errAuths)
	}
	if len(auths) != 1 || auths[0].Attributes[coreauth.AttributeCodexAlphaSearch] != "true" {
		t.Fatalf("persisted auths = %#v, want codex_alpha_search=true", auths)
	}

	outputDir := t.TempDir()
	if _, errExport := ExportLocalState(context.Background(), ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
		t.Fatalf("ExportLocalState() error = %v", errExport)
	}
	raw, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
	if errRead != nil {
		t.Fatalf("read exported config: %v", errRead)
	}
	if !strings.Contains(string(raw), "alpha-search: true") {
		t.Fatalf("exported config missing alpha-search: %s", raw)
	}
}
