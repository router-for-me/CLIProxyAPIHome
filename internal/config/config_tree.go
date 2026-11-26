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
	groups, _ := cfg.RawConfig["api-keys"].(map[string]any)
	scopes, errScopes := credentialGroupScopes(groups)
	if errScopes != nil {
		return errScopes
	}
	entry := func(family string, index int) CredentialConfig {
		var result CredentialConfig
		entries, _ := effective[family].([]any)
		if index < len(entries) {
			result.RawConfig, _ = entries[index].(map[string]any)
		}
		if index < len(scopes[family]) {
			result.GroupScope = scopes[family][index]
		}
		return result
	}
	for i := range cfg.GeminiKey {
		cfg.GeminiKey[i].CredentialConfig = entry("gemini-api-key", i)
	}
	for i := range cfg.InteractionsKey {
		cfg.InteractionsKey[i].CredentialConfig = entry("interactions-api-key", i)
	}
	for i := range cfg.VertexCompatAPIKey {
		cfg.VertexCompatAPIKey[i].CredentialConfig = entry("vertex-api-key", i)
	}
	for i := range cfg.CodexKey {
		cfg.CodexKey[i].CredentialConfig = entry("codex-api-key", i)
	}
	for i := range cfg.ClaudeKey {
		cfg.ClaudeKey[i].CredentialConfig = entry("claude-api-key", i)
	}
	for i := range cfg.XAIKey {
		cfg.XAIKey[i].CredentialConfig = entry("xai-api-key", i)
	}
	for i := range cfg.MetaKey {
		cfg.MetaKey[i].CredentialConfig = entry("meta-api-key", i)
	}
	for i := range cfg.OpenAICompatibility {
		group := &cfg.OpenAICompatibility[i]
		group.CredentialConfig = entry("openai-compatibility", i)
		keys, _ := group.RawConfig["api-key-entries"].([]any)
		for j := range group.APIKeyEntries {
			if j < len(keys) {
				group.APIKeyEntries[j].RawConfig, _ = keys[j].(map[string]any)
			}
		}
	}
	return nil
}
