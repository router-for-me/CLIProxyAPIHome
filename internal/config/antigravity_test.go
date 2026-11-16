package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
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

	wire, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(wire)
	if errParse != nil {
		t.Fatal(errParse)
	}
	want := []string{" API ", "", "proxy", "   "}
	if !reflect.DeepEqual(cpa.Antigravity.SensitiveWords, want) {
		t.Fatalf("CPA Antigravity.SensitiveWords = %#v, want %#v", cpa.Antigravity.SensitiveWords, want)
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
	if !cfg.OAuthOnlyFields["antigravity.sensitive-words-replace"] || !cfg.OAuthOnlyFields["devin.sensitive-words-replace"] {
		t.Fatalf("OAuthOnlyFields missing sensitive-words-replace scope: %#v", cfg.OAuthOnlyFields)
	}
	wire, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(wire)
	if errParse != nil {
		t.Fatal(errParse)
	}
	wantAntigrav := map[string]string{
		"You are Codex, an agent based on GPT-5.": "You are a helpful coding assistant.",
		"DropMe":    "",
		"dup":       "lower",
		"DUP":       "upper",
		"exact-key": "exact-val",
	}
	if !reflect.DeepEqual(cpa.Antigravity.SensitiveWordsReplace, wantAntigrav) {
		t.Fatalf("CPA Antigravity.SensitiveWordsReplace = %#v, want %#v", cpa.Antigravity.SensitiveWordsReplace, wantAntigrav)
	}
	if !reflect.DeepEqual(cpa.Devin.SensitiveWords, []string{" Claude "}) {
		t.Fatalf("CPA Devin.SensitiveWords = %#v, want [\" Claude \"]", cpa.Devin.SensitiveWords)
	}
	if got := cpa.Devin.SensitiveWordsReplace["Claude Code"]; got != "Devin Assistant" {
		t.Fatalf("CPA Devin.SensitiveWordsReplace = %#v", cpa.Devin.SensitiveWordsReplace)
	}
	if len(cpa.ClaudeKey) != 1 || cpa.ClaudeKey[0].Cloak == nil || cpa.ClaudeKey[0].Cloak.SensitiveWordsReplace["Claude Code"] != "Claude" {
		t.Fatalf("CPA ClaudeKey[0].Cloak = %#v", cpa.ClaudeKey)
	}
}

func TestParseAndValidateV8_SensitivePatternsReplace(t *testing.T) {
	v8Content := []byte(`config-version: 8
oauth:
  providers:
    antigravity:
      sensitive-patterns-replace:
        - pattern: '(?m)^RFC 2119[^\n]*\n?'
          replacement: ""
        - pattern: '[invalid'
          replacement: "ignored"
    devin:
      sensitive-patterns-replace:
        - pattern: '(?m)^You are omp[^\n]*\n?'
          replacement: ""
api-keys:
  claude:
    - name: claude-1
      keys:
        - api-key: sk-ant-test
          cloak:
            mode: always
            sensitive-patterns-replace:
              - pattern: 'Claude\s+Code'
                replacement: "Claude"
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
	if !cfg.OAuthOnlyFields["antigravity.sensitive-patterns-replace"] || !cfg.OAuthOnlyFields["devin.sensitive-patterns-replace"] {
		t.Fatalf("OAuthOnlyFields missing sensitive-patterns-replace scope: %#v", cfg.OAuthOnlyFields)
	}
	wire, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(wire)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(cpa.Antigravity.SensitivePatternsReplace) != 1 || cpa.Antigravity.SensitivePatternsReplace[0].Pattern != `(?m)^RFC 2119[^\n]*\n?` || cpa.Antigravity.SensitivePatternsReplace[0].Replacement != "" {
		t.Fatalf("CPA Antigravity.SensitivePatternsReplace = %#v", cpa.Antigravity.SensitivePatternsReplace)
	}
	if len(cpa.Devin.SensitivePatternsReplace) != 1 || cpa.Devin.SensitivePatternsReplace[0].Pattern != `(?m)^You are omp[^\n]*\n?` {
		t.Fatalf("CPA Devin.SensitivePatternsReplace = %#v", cpa.Devin.SensitivePatternsReplace)
	}
	if len(cpa.ClaudeKey) != 1 || cpa.ClaudeKey[0].Cloak == nil || len(cpa.ClaudeKey[0].Cloak.SensitivePatternsReplace) != 1 || cpa.ClaudeKey[0].Cloak.SensitivePatternsReplace[0].Replacement != "Claude" {
		t.Fatalf("SDK Claude cloak patterns were lost: %#v", cpa.ClaudeKey)
	}
}
