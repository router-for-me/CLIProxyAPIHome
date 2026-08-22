package home

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestResolveConfigGeminiKeyEntryUsesConfigIdentity(t *testing.T) {
	entries := []config.GeminiKey{
		{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "one", ProxyURL: "http://proxy-one.test", Models: []config.GeminiModel{{Name: "upstream-one"}}},
		{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "two", ProxyURL: "http://proxy-two.test", Models: []config.GeminiModel{{Name: "upstream-two"}}},
		{BaseURL: "https://header-auth.example.test", Prefix: "header", Models: []config.GeminiModel{{Name: "upstream-header"}}},
		{APIKey: "shared-key", BaseURL: "https://other.example.test", Prefix: "two", ProxyURL: "http://proxy-two.test", Models: []config.GeminiModel{{Name: "upstream-other"}}},
	}
	tests := []struct {
		name      string
		auth      *coreauth.Auth
		wantModel string
	}{
		{
			name: "config index",
			auth: &coreauth.Auth{Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "1",
			}},
			wantModel: "upstream-two",
		},
		{
			name: "prefix and proxy fallback",
			auth: &coreauth.Auth{Prefix: "two", ProxyURL: "http://proxy-two.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test",
			}},
			wantModel: "upstream-two",
		},
		{
			name: "indexed credential mismatch",
			auth: &coreauth.Auth{Prefix: "header", Attributes: map[string]string{
				"base_url": "https://header-auth.example.test", "config_index": "1",
			}},
			wantModel: "upstream-header",
		},
		{
			name: "indexed base URL mismatch",
			auth: &coreauth.Auth{Prefix: "two", ProxyURL: "http://proxy-two.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "3",
			}},
			wantModel: "upstream-two",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := resolveConfigGeminiKeyEntry(tc.auth, entries)
			if entry == nil || len(entry.Models) != 1 || entry.Models[0].Name != tc.wantModel {
				t.Fatalf("resolved entry = %#v, want model %q", entry, tc.wantModel)
			}
		})
	}
}
