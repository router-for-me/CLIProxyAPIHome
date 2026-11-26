package config

import "testing"

func TestOpenAICompatibleProviderKey(t *testing.T) {
	for input, want := range map[string]string{
		"": "openai-compatibility", " OpenAI-Compatibility ": "openai-compatibility",
		" Copilot ": "openai-compatible-copilot", "OpenAI-Compatible-Copilot": "openai-compatible-copilot",
	} {
		if got := OpenAICompatibleProviderKey(input); got != want {
			t.Errorf("OpenAICompatibleProviderKey(%q) = %q, want %q", input, got, want)
		}
	}
}
