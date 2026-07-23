package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfigOptionalFallbacksOnlyApplyCredentialDefaults(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		write   bool
	}{
		{name: "missing"},
		{name: "empty", content: []byte{}, write: true},
		{name: "whitespace", content: []byte(" \t\r\n "), write: true},
		{name: "invalid", content: []byte("credential-in-flight: ["), write: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if test.write {
				if errWrite := os.WriteFile(configPath, test.content, 0o600); errWrite != nil {
					t.Fatalf("WriteFile() error = %v", errWrite)
				}
			}

			cfg, errLoad := LoadConfigOptional(configPath, true)
			if errLoad != nil {
				t.Fatalf("LoadConfigOptional() error = %v", errLoad)
			}
			if cfg.CredentialInFlight != DefaultCredentialInFlightConfig() {
				t.Fatalf("CredentialInFlight = %#v, want %#v", cfg.CredentialInFlight, DefaultCredentialInFlightConfig())
			}
			if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
				t.Fatalf("CredentialInFlight.Validate() error = %v", errValidate)
			}
			if cfg.CredentialConcurrency != DefaultCredentialConcurrencyConfig() {
				t.Fatalf("CredentialConcurrency = %#v, want %#v", cfg.CredentialConcurrency, DefaultCredentialConcurrencyConfig())
			}
			if errValidate := ValidateCredentialConcurrencyConfig(cfg.CredentialConcurrency); errValidate != nil {
				t.Fatalf("ValidateCredentialConcurrencyConfig() error = %v", errValidate)
			}
			want := &Config{
				CredentialConcurrency: DefaultCredentialConcurrencyConfig(),
				CredentialInFlight:    DefaultCredentialInFlightConfig(),
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("fallback config = %#v, want %#v", cfg, want)
			}
		})
	}
}

func TestLoadConfigOptionalParsesAndSanitizesXAIKeys(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	payload := []byte(`xai-api-key:
  - api-key: " xai-key "
    priority: 9
    prefix: "/grok/"
    base-url: " https://api.x.ai/v1 "
    websockets: true
    proxy-url: "socks5://proxy.example:1080"
    headers:
      X-Test: " value "
    models:
      - name: "grok-4.5"
        alias: "grok-latest"
        display-name: "Grok Latest"
        force-mapping: true
    excluded-models:
      - " GROK-3-* "
    disable-cooling: true
  - api-key: "dropped"
    base-url: " "
`)
	if errWrite := os.WriteFile(configPath, payload, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	cfg, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if len(cfg.XAIKey) != 1 {
		t.Fatalf("XAIKey count = %d, want 1", len(cfg.XAIKey))
	}
	entry := cfg.XAIKey[0]
	if entry.APIKey != " xai-key " || entry.BaseURL != "https://api.x.ai/v1" {
		t.Fatalf("xAI key/base URL = %q/%q", entry.APIKey, entry.BaseURL)
	}
	if entry.Prefix != "grok" || entry.Priority != 9 || !entry.Websockets || !entry.DisableCooling {
		t.Fatalf("xAI routing fields = %+v", entry)
	}
	if entry.Headers["X-Test"] != "value" || len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "grok-3-*" {
		t.Fatalf("xAI normalized fields = headers:%v excluded:%v", entry.Headers, entry.ExcludedModels)
	}
	if len(entry.Models) != 1 {
		t.Fatalf("xAI model count = %d, want 1", len(entry.Models))
	}
	model := entry.Models[0]
	if model.Name != "grok-4.5" || model.Alias != "grok-latest" || model.DisplayName != "Grok Latest" || !model.ForceMapping {
		t.Fatalf("xAI model = %+v", model)
	}
}
