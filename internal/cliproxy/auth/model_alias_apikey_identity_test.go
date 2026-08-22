package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestResolveInteractionsAPIKeyConfigUsesConfigIdentity(t *testing.T) {
	entries := []internalconfig.GeminiKey{
		{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "one", ProxyURL: "http://proxy-one.test", Models: []internalconfig.GeminiModel{{Alias: "alias", Name: "upstream-one"}}},
		{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "two", ProxyURL: "http://proxy-two.test", Models: []internalconfig.GeminiModel{{Alias: "alias", Name: "upstream-two"}}},
		{BaseURL: "https://header-auth.example.test", Prefix: "header", Models: []internalconfig.GeminiModel{{Alias: "alias", Name: "upstream-header"}}},
		{APIKey: "shared-key", BaseURL: "https://other.example.test", Prefix: "two", ProxyURL: "http://proxy-two.test", Models: []internalconfig.GeminiModel{{Alias: "alias", Name: "upstream-other"}}},
	}
	tests := []struct {
		name      string
		auth      *Auth
		wantModel string
	}{
		{
			name: "config index",
			auth: &Auth{Prefix: "one", ProxyURL: "http://proxy-one.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "1",
			}},
			wantModel: "upstream-two",
		},
		{
			name: "prefix and proxy fallback",
			auth: &Auth{Prefix: "two", ProxyURL: "http://proxy-two.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "99",
			}},
			wantModel: "upstream-two",
		},
		{
			name: "indexed credential mismatch",
			auth: &Auth{Prefix: "header", Attributes: map[string]string{
				"base_url": "https://header-auth.example.test", "config_index": "1",
			}},
			wantModel: "upstream-header",
		},
		{
			name: "header authenticated config index",
			auth: &Auth{Attributes: map[string]string{
				"base_url": "https://header-auth.example.test", "config_index": "2",
			}},
			wantModel: "upstream-header",
		},
		{
			name: "indexed base URL mismatch",
			auth: &Auth{Prefix: "two", ProxyURL: "http://proxy-two.test", Attributes: map[string]string{
				"api_key": "shared-key", "base_url": "https://api.example.test", "config_index": "3",
			}},
			wantModel: "upstream-two",
		},
	}
	cfg := &internalconfig.Config{InteractionsKey: entries}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := resolveInteractionsAPIKeyConfig(cfg, tc.auth)
			if entry == nil || len(entry.Models) != 1 || entry.Models[0].Name != tc.wantModel {
				t.Fatalf("resolved entry = %#v, want model %q", entry, tc.wantModel)
			}
		})
	}
}
