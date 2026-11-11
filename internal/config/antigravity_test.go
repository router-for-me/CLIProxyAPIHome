package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseConfig_AntigravitySensitiveWords(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`
antigravity:
  sensitive-words:
    - " API "
    - ""
    - "proxy"
    - "   "
`)
	if errWrite := os.WriteFile(configPath, content, 0o600); errWrite != nil {
		t.Fatalf("write config file: %v", errWrite)
	}

	cfg, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}

	want := []string{"API", "proxy"}
	if !reflect.DeepEqual(cfg.Antigravity.SensitiveWords, want) {
		t.Fatalf("Antigravity.SensitiveWords = %#v, want %#v", cfg.Antigravity.SensitiveWords, want)
	}
}

func TestSanitizeAntigravity(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "nil slice",
			in:   nil,
			want: nil,
		},
		{
			name: "empty slice",
			in:   []string{},
			want: []string{},
		},
		{
			name: "trims whitespace and drops empty",
			in:   []string{"  word1  ", "", "   ", "word2"},
			want: []string{"word1", "word2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Antigravity: AntigravityConfig{
					SensitiveWords: tt.in,
				},
			}
			cfg.SanitizeAntigravity()
			if len(tt.want) == 0 && len(cfg.Antigravity.SensitiveWords) == 0 {
				return
			}
			if !reflect.DeepEqual(cfg.Antigravity.SensitiveWords, tt.want) {
				t.Fatalf("SanitizeAntigravity() = %#v, want %#v", cfg.Antigravity.SensitiveWords, tt.want)
			}
		})
	}
}

func TestParseAndValidateV8_SensitiveWordsReplace(t *testing.T) {
	v8Content := []byte(`config-version: 8
oauth:
  providers:
    antigravity:
      sensitive-words:
        - " Codex "
      sensitive-words-replace:
        "  You are Codex, an agent based on GPT-5.  ": "You are a helpful coding assistant."
        "DropMe": ""
        "   ": "ignored"
        "dup": "lower"
        "DUP": "upper"
        "exact-key": "exact-val"
        " exact-key ": "padded-before"
        "exact-key ": "padded-after"
    devin:
      sensitive-words:
        - " Claude "
      sensitive-words-replace:
        "Claude Code": "Devin Assistant"
api-keys:
  claude:
    - name: claude-1
      keys:
        - api-key: sk-ant-test
          cloak:
            mode: always
            sensitive-words-replace:
              "Claude Code": "Claude"
`)
	if errValidate := ValidateV8Config(v8Content); errValidate != nil {
		t.Fatalf("ValidateV8Config() error = %v", errValidate)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, v8Content, 0o600); errWrite != nil {
		t.Fatalf("write config file: %v", errWrite)
	}
	cfg, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	wantAntigrav := map[string]string{
		"You are Codex, an agent based on GPT-5.": "You are a helpful coding assistant.",
		"DropMe":    "",
		"dup":       "lower",
		"DUP":       "upper",
		"exact-key": "exact-val",
	}
	if !reflect.DeepEqual(cfg.Antigravity.SensitiveWordsReplace, wantAntigrav) {
		t.Fatalf("Antigravity.SensitiveWordsReplace = %#v, want %#v", cfg.Antigravity.SensitiveWordsReplace, wantAntigrav)
	}
	if !reflect.DeepEqual(cfg.Devin.SensitiveWords, []string{"Claude"}) {
		t.Fatalf("Devin.SensitiveWords = %#v, want [Claude]", cfg.Devin.SensitiveWords)
	}
	if got := cfg.Devin.SensitiveWordsReplace["Claude Code"]; got != "Devin Assistant" {
		t.Fatalf("Devin.SensitiveWordsReplace = %#v", cfg.Devin.SensitiveWordsReplace)
	}
	if !cfg.OAuthOnlyFields["antigravity.sensitive-words-replace"] || !cfg.OAuthOnlyFields["devin.sensitive-words-replace"] {
		t.Fatalf("OAuthOnlyFields missing sensitive-words-replace scope: %#v", cfg.OAuthOnlyFields)
	}
	if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].Cloak == nil || cfg.ClaudeKey[0].Cloak.SensitiveWordsReplace["Claude Code"] != "Claude" {
		t.Fatalf("ClaudeKey[0].Cloak = %#v", cfg.ClaudeKey)
	}
}
