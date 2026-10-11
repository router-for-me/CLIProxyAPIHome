package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestAddTokenRejectsInvalidExcludedModelsWithoutWritingFile(t *testing.T) {
	authDir := t.TempDir()
	runtime := &Runtime{cfg: &config.Config{}, authDir: authDir}

	_, errAdd := runtime.AddToken(t.Context(), `{"type":"antigravity","excluded_models":[1]}`)
	if errAdd == nil || !strings.Contains(errAdd.Error(), "excluded_models[0] must be a string") {
		t.Fatalf("AddToken() error = %v, want excluded_models validation error", errAdd)
	}
	entries, errRead := os.ReadDir(authDir)
	if errRead != nil {
		t.Fatalf("ReadDir() error = %v", errRead)
	}
	if len(entries) != 0 {
		t.Fatalf("auth dir has %d entries, want none after a rejected token", len(entries))
	}

	name, errValid := runtime.AddToken(t.Context(), `{"type":"antigravity","excluded_models":"gemini-3-flash,gemini-pro-agent"}`)
	if errValid != nil {
		t.Fatalf("AddToken(valid) error = %v", errValid)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, name)); errStat != nil {
		t.Fatalf("valid token file %s not written: %v", name, errStat)
	}
}
