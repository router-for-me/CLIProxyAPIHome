package config

import (
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestNormalizeLegacyCodexOptimization(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   bool
	}{
		{"legacy enabled", "codex: {optimize-multi-agent-v2: true}\n", true},
		{"legacy disabled", "codex: {optimize-multi-agent-v2: false}\n", false},
		{"historical providers", "providers: {codex: {optimize-multi-agent-v2: true}}\n", true},
		{"historical OAuth", "oauth: {providers: {codex: {optimize-multi-agent-v2: true}}}\n", true},
		{"client false wins", "client: {codex: {optimize-multi-agent-v2: false}}\ncodex: {optimize-multi-agent-v2: true}\n", false},
		{"client null wins", "client: {codex: {optimize-multi-agent-v2: null}}\ncodex: {optimize-multi-agent-v2: true}\n", false},
		{"OAuth wins globals", "oauth: {providers: {codex: {optimize-multi-agent-v2: false}}}\nproviders: {codex: {optimize-multi-agent-v2: true}}\ncodex: {optimize-multi-agent-v2: true}\n", false},
		{"providers wins global", "providers: {codex: {optimize-multi-agent-v2: false}}\ncodex: {optimize-multi-agent-v2: true}\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, migrate := range []bool{false, true} {
				normalized, _, errNormalize := NormalizeConfigLayout([]byte(test.source), migrate)
				if errNormalize != nil {
					t.Fatal(errNormalize)
				}
				if migrate {
					if errValidate := ValidateV8Config(normalized); errValidate != nil {
						t.Fatal(errValidate)
					}
					var doc yaml.Node
					if errDecode := yaml.Unmarshal(normalized, &doc); errDecode != nil {
						t.Fatal(errDecode)
					}
					if yamlPath(doc.Content[0], "client.codex.optimize-multi-agent-v2") == nil {
						t.Fatalf("migration omitted canonical client setting: %s", normalized)
					}
				}
				cpa, errParse := cpaconfig.ParseConfigBytes(normalized)
				if errParse != nil {
					t.Fatal(errParse)
				}
				if cpa.Client.Codex.OptimizeMultiAgentV2 != test.want {
					t.Fatalf("migrate=%t: optimize-multi-agent-v2 = %t, want %t", migrate, cpa.Client.Codex.OptimizeMultiAgentV2, test.want)
				}
			}
		})
	}
}
