package cluster

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// TestApplyCredentialConfigToRootHydratesAPIKeyAuths verifies test apply credential config to root hydrates api key auths behavior.
func TestApplyCredentialConfigToRootHydratesAPIKeyAuths(t *testing.T) {
	// Normalize source data before building the derived payload.
	root := map[string]any{"debug": true}
	auths := []*coreauth.Auth{
		testConfigAPIKeyAuth("gemini-id", "gemini", "config:gemini[token]", "gemini-key"),
		testConfigAPIKeyAuth("vertex-id", "vertex", "config:vertex-apikey[token]", "vertex-key"),
		testConfigAPIKeyAuth("codex-id", "codex", "config:codex[token]", "codex-key"),
		testConfigAPIKeyAuth("xai-id", "xai", "config:xai[token]", "xai-key"),
		testConfigAPIKeyAuth("claude-id", "claude", "config:claude[token]", "claude-key"),
		testConfigAPIKeyAuth("codex-file-id", "codex", "auth-file.json", "ignored-key"),
	}

	counts := ApplyCredentialConfigToRoot(root, auths)
	if counts.GeminiKeys != 1 || counts.VertexKeys != 1 || counts.CodexKeys != 1 || counts.XAIKeys != 1 || counts.ClaudeKeys != 1 {
		t.Fatalf("unexpected credential counts: %#v", counts)
	}
	if got := root["debug"]; got != true {
		t.Fatalf("debug root value changed to %#v", got)
	}

	geminiKeys, ok := root["gemini-api-key"].([]appconfig.GeminiKey)
	if !ok || len(geminiKeys) != 1 || geminiKeys[0].APIKey != "gemini-key" {
		t.Fatalf("unexpected gemini-api-key root value: %#v", root["gemini-api-key"])
	}
	if geminiKeys[0].ID != "gemini-id" || geminiKeys[0].UUID != "" {
		t.Fatalf("gemini credential identity = %#v, want exported id only", geminiKeys[0])
	}
	vertexKeys, ok := root["vertex-api-key"].([]appconfig.VertexCompatKey)
	if !ok || len(vertexKeys) != 1 || vertexKeys[0].APIKey != "vertex-key" {
		t.Fatalf("unexpected vertex-api-key root value: %#v", root["vertex-api-key"])
	}
	codexKeys, ok := root["codex-api-key"].([]appconfig.CodexKey)
	if !ok || len(codexKeys) != 1 || codexKeys[0].APIKey != "codex-key" {
		t.Fatalf("unexpected codex-api-key root value: %#v", root["codex-api-key"])
	}
	xaiKeys, ok := root["xai-api-key"].([]appconfig.XAIKey)
	if !ok || len(xaiKeys) != 1 || xaiKeys[0].APIKey != "xai-key" {
		t.Fatalf("unexpected xai-api-key root value: %#v", root["xai-api-key"])
	}
	claudeKeys, ok := root["claude-api-key"].([]appconfig.ClaudeKey)
	if !ok || len(claudeKeys) != 1 || claudeKeys[0].APIKey != "claude-key" {
		t.Fatalf("unexpected claude-api-key root value: %#v", root["claude-api-key"])
	}
	if vertexKeys[0].ID != "vertex-id" || codexKeys[0].ID != "codex-id" || xaiKeys[0].ID != "xai-id" || claudeKeys[0].ID != "claude-id" {
		t.Fatalf("provider credential IDs were not exported")
	}
}

// testConfigAPIKeyAuth handles a test config api key auth.
func testConfigAPIKeyAuth(id, provider, source, apiKey string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Index:    id,
		Provider: provider,
		Prefix:   "test-prefix",
		ProxyURL: "http://proxy.example",
		Attributes: map[string]string{
			"source":        source,
			"api_key":       apiKey,
			"base_url":      "https://api.example",
			"priority":      "7",
			"header:X-Test": "test",
		},
	}
}
