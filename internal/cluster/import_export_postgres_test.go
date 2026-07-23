package cluster

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestExportLocalStateUsesRepeatableReadSnapshotPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()

	credentialID := "74747474-7474-4747-8747-747474747474"
	oldAuth := &coreauth.Auth{
		ID:       credentialID,
		Index:    credentialID,
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[export]",
			"api_key": "old-key",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, oldAuth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	setExchangePolicy(t, repo, credentialID, 2, nil)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, configPath, "gemini-api-key:\n  - id: "+credentialID+"\n    api-key: new-key\ncredential-concurrency-policies:\n  "+credentialID+":\n    max-in-flight: 3\n")

	snapshotRead := make(chan struct{})
	importDone := make(chan error, 1)
	ctx = context.WithValue(ctx, exportSnapshotConfigReadContextKey{}, func() {
		close(snapshotRead)
		select {
		case errImport := <-importDone:
			if errImport != nil {
				t.Errorf("concurrent import error: %v", errImport)
			}
		case <-ctx.Done():
			t.Errorf("concurrent import did not complete: %v", ctx.Err())
		}
	})
	go func() {
		select {
		case <-snapshotRead:
		case <-ctx.Done():
			importDone <- ctx.Err()
			return
		}
		_, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo})
		importDone <- errImport
	}()

	outputDir := t.TempDir()
	if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	data, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	root := map[string]any{}
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	entries, okEntries := root["gemini-api-key"].([]any)
	if !okEntries || len(entries) != 1 {
		t.Fatalf("gemini-api-key = %#v", root["gemini-api-key"])
	}
	entry, okEntry := entries[0].(map[string]any)
	if !okEntry || entry["api-key"] != "old-key" {
		t.Fatalf("exported credential = %#v, want old snapshot", entries[0])
	}
	policies, okPolicies := root[credentialConcurrencyPoliciesRootKey].(map[string]any)
	if !okPolicies {
		t.Fatalf("%s = %#v", credentialConcurrencyPoliciesRootKey, root[credentialConcurrencyPoliciesRootKey])
	}
	policy, okPolicy := policies[credentialID].(map[string]any)
	if !okPolicy || policy["max-in-flight"] != 2 {
		t.Fatalf("exported policy = %#v, want old snapshot", policies[credentialID])
	}
}
