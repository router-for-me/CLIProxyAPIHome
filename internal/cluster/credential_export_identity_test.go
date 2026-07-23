package cluster

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"gopkg.in/yaml.v3"
)

func TestApplyCredentialConfigToRootPreservesDistinctOpenAICompatibilityCredentialIDs(t *testing.T) {
	idOne := "77777777-7777-4777-8777-777777777777"
	idTwo := "88888888-8888-4888-8888-888888888888"
	root := map[string]any{}
	ApplyCredentialConfigToRoot(root, []*coreauth.Auth{
		{
			ID:       idOne,
			Index:    idOne,
			Provider: "openai",
			Attributes: map[string]string{
				"source":      "config:openai-compatibility[one]",
				"compat_name": "compat",
				"api_key":     "same-key",
				"base_url":    "https://example.test",
			},
		},
		{
			ID:       idTwo,
			Index:    idTwo,
			Provider: "openai",
			Attributes: map[string]string{
				"source":      "config:openai-compatibility[two]",
				"compat_name": "compat",
				"api_key":     "same-key",
				"base_url":    "https://example.test",
			},
		},
	})
	entries, okEntries := root["openai-compatibility"].([]appconfig.OpenAICompatibility)
	if !okEntries || len(entries) != 1 {
		t.Fatalf("openai-compatibility = %#v", root["openai-compatibility"])
	}
	if len(entries[0].APIKeyEntries) != 2 {
		t.Fatalf("api-key-entries = %#v, want both credential identities", entries[0].APIKeyEntries)
	}
	ids := map[string]bool{}
	for _, entry := range entries[0].APIKeyEntries {
		ids[entry.ID] = true
	}
	if !ids[idOne] || !ids[idTwo] {
		t.Fatalf("exported entry IDs = %#v, want %s and %s", ids, idOne, idTwo)
	}
}

func TestApplyCredentialConfigToRootRoundTripsDistinctOpenAICompatibilityFallbackIdentities(t *testing.T) {
	fallbackID := "12121212-1212-4121-8121-121212121212"
	nestedID := "34343434-3434-4434-8434-343434343434"
	newAuth := func(id string, apiKey string) *coreauth.Auth {
		attributes := map[string]string{
			"source":      "config:compat[token-" + id + "]",
			"compat_name": "compat",
			"base_url":    "https://example.test",
		}
		if apiKey != "" {
			attributes["api_key"] = apiKey
		}
		return &coreauth.Auth{ID: id, Index: id, Provider: "compat", Attributes: attributes}
	}
	tests := []struct {
		name  string
		auths []*coreauth.Auth
	}{
		{name: "fallbacks", auths: []*coreauth.Auth{newAuth(fallbackID, ""), newAuth(nestedID, "")}},
		{name: "fallback and nested", auths: []*coreauth.Auth{newAuth(fallbackID, ""), newAuth(nestedID, "key")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := map[string]any{}
			ApplyCredentialConfigToRoot(root, tc.auths)
			entries, okEntries := root["openai-compatibility"].([]appconfig.OpenAICompatibility)
			if !okEntries || len(entries) != 2 {
				t.Fatalf("openai-compatibility = %#v, want two separately representable entries", root["openai-compatibility"])
			}
			cfg, _, errConfig := RuntimeConfigFromRoot(root)
			if errConfig != nil {
				t.Fatal(errConfig)
			}
			roundTripped, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now().UTC(), IDGenerator: synthesizer.NewStableIDGenerator()})
			if errSynthesize != nil {
				t.Fatal(errSynthesize)
			}
			ids := make(map[string]bool, len(roundTripped))
			for _, auth := range roundTripped {
				ids[auth.ID] = true
			}
			if !ids[fallbackID] || !ids[nestedID] || len(ids) != 2 {
				t.Fatalf("round-trip IDs = %#v, want %s and %s", ids, fallbackID, nestedID)
			}
		})
	}
}

func TestExportLocalStatePreservesProviderCredentialIDs(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "66666666-6666-4666-8666-666666666666"
	auth := &coreauth.Auth{ID: id, Index: id, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[token]", "api_key": "key"}}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	outputDir := t.TempDir()
	if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	data, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var root map[string]any
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	entries, okEntries := root["gemini-api-key"].([]any)
	if !okEntries || len(entries) != 1 {
		t.Fatalf("gemini-api-key = %#v", root["gemini-api-key"])
	}
	entry, okEntry := entries[0].(map[string]any)
	if !okEntry || entry["id"] != id {
		t.Fatalf("exported entry = %#v", entries[0])
	}
	if _, hasUUID := entry["uuid"]; hasUUID {
		t.Fatalf("exported entry retained legacy uuid: %#v", entry)
	}
}
