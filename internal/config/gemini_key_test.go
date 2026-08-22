package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGeminiModelSerializationPreservesCatalogFields(t *testing.T) {
	want := GeminiKey{
		APIKey: "key",
		Models: []GeminiModel{{
			Name:         "gemini-upstream",
			Alias:        "gemini-alias",
			DisplayName:  "Gemini Catalog Name",
			ForceMapping: true,
		}},
	}
	tests := []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{name: "yaml", marshal: yaml.Marshal, unmarshal: yaml.Unmarshal},
		{name: "json", marshal: json.Marshal, unmarshal: json.Unmarshal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, errMarshal := tc.marshal(want)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var got GeminiKey
			if errUnmarshal := tc.unmarshal(raw, &got); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if len(got.Models) != 1 || got.Models[0].DisplayName != "Gemini Catalog Name" || !got.Models[0].ForceMapping {
				t.Fatalf("round-tripped models = %#v", got.Models)
			}
		})
	}
}

func TestSanitizeInteractionsKeysPreservesDistinctRoutingIdentity(t *testing.T) {
	base := GeminiKey{
		APIKey:   " shared-key ",
		BaseURL:  " https://api.example.test ",
		Prefix:   " team ",
		ProxyURL: " http://proxy-one.test ",
		Headers: map[string]string{
			"X-One": "1",
			"X-Two": "2",
		},
	}
	cfg := &Config{InteractionsKey: []GeminiKey{
		base,
		{
			APIKey:   "shared-key",
			BaseURL:  "https://api.example.test",
			Prefix:   "team",
			ProxyURL: "http://proxy-one.test",
			Headers:  map[string]string{"X-One": "1", "X-Two": "changed"},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://api.example.test",
			Prefix:   "team",
			ProxyURL: "http://proxy-two.test",
			Headers:  map[string]string{"X-One": "1", "X-Two": "2"},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://api.example.test",
			Prefix:   "other-team",
			ProxyURL: "http://proxy-one.test",
			Headers:  map[string]string{"X-One": "1", "X-Two": "2"},
		},
		{
			APIKey:   "shared-key",
			BaseURL:  "https://api.example.test",
			Prefix:   "team",
			ProxyURL: "http://proxy-one.test",
			Headers:  map[string]string{"X-Two": "2", "X-One": "1"},
		},
		{
			BaseURL: " https://header-auth.example.test ",
			Headers: map[string]string{"Authorization": "Bearer token"},
		},
		{Headers: map[string]string{"Authorization": "Bearer ignored"}},
	}}

	cfg.SanitizeInteractionsKeys()

	if len(cfg.InteractionsKey) != 5 {
		t.Fatalf("sanitized interactions count = %d, want 5: %#v", len(cfg.InteractionsKey), cfg.InteractionsKey)
	}
	if cfg.InteractionsKey[0].APIKey != "shared-key" || cfg.InteractionsKey[0].BaseURL != "https://api.example.test" {
		t.Fatalf("first entry was not trimmed: %#v", cfg.InteractionsKey[0])
	}
	keyless := cfg.InteractionsKey[4]
	if keyless.APIKey != "" || keyless.BaseURL != "https://header-auth.example.test" {
		t.Fatalf("header-authenticated entry = %#v", keyless)
	}
	if got, wantHeaders := FormatSortedHeaders(cfg.InteractionsKey[0].Headers), "X-One\x001\x00X-Two\x002\x00"; got != wantHeaders {
		t.Fatalf("FormatSortedHeaders() = %q, want %q", got, wantHeaders)
	}
}
