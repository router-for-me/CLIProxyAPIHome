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
