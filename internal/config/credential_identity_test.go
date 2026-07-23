package config

import "testing"

func TestNormalizeProviderCredentialIDsRejectsInvalidAndNonCanonicalUUIDs(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "invalid", id: "not-a-uuid"},
		{name: "non-canonical", id: "11111111-1111-4111-8111-111111111111 "},
		{name: "uppercase", id: "11111111-1111-4111-8111-11111111111A"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{GeminiKey: []GeminiKey{{ID: tc.id, APIKey: "key"}}}
			if errNormalize := cfg.NormalizeProviderCredentialIDs(); errNormalize == nil {
				t.Fatalf("NormalizeProviderCredentialIDs() accepted %q", tc.id)
			}
		})
	}
}
