package config

import "gopkg.in/yaml.v3"

// captureConfigTree keeps the submitted document intact. Flattened credential
// subtrees only resolve group inheritance for credential storage and dispatch.
func (cfg *Config) captureConfigTree(source, flat *yaml.Node) error {
	cfg.RawConfig = nil
	if errDecode := source.Decode(&cfg.RawConfig); errDecode != nil {
		return errDecode
	}
	var effective map[string]any
	if errDecode := flat.Decode(&effective); errDecode != nil {
		return errDecode
	}
	entry := func(family string, index int) map[string]any {
		entries, _ := effective[family].([]any)
		if index >= len(entries) {
			return nil
		}
		value, _ := entries[index].(map[string]any)
		return value
	}
	for i := range cfg.GeminiKey {
		cfg.GeminiKey[i].RawConfig = entry("gemini-api-key", i)
	}
	for i := range cfg.InteractionsKey {
		cfg.InteractionsKey[i].RawConfig = entry("interactions-api-key", i)
	}
	for i := range cfg.VertexCompatAPIKey {
		cfg.VertexCompatAPIKey[i].RawConfig = entry("vertex-api-key", i)
	}
	for i := range cfg.CodexKey {
		cfg.CodexKey[i].RawConfig = entry("codex-api-key", i)
	}
	for i := range cfg.ClaudeKey {
		cfg.ClaudeKey[i].RawConfig = entry("claude-api-key", i)
	}
	for i := range cfg.XAIKey {
		cfg.XAIKey[i].RawConfig = entry("xai-api-key", i)
	}
	for i := range cfg.MetaKey {
		cfg.MetaKey[i].RawConfig = entry("meta-api-key", i)
	}
	for i := range cfg.OpenAICompatibility {
		group := &cfg.OpenAICompatibility[i]
		group.RawConfig = entry("openai-compatibility", i)
		keys, _ := group.RawConfig["api-key-entries"].([]any)
		for j := range group.APIKeyEntries {
			if j < len(keys) {
				group.APIKeyEntries[j].RawConfig, _ = keys[j].(map[string]any)
			}
		}
	}
	return nil
}
