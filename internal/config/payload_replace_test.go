package config

import (
	"reflect"
	"strings"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

const payloadReplaceConfigFixture = `config-version: 8
requests:
  payload:
    override:
      - models: [{name: "*"}]
        params: {max_tokens: 100}
    replace:
      - models:
          - name: "*"
            protocol: claude
            from-protocol: openai
            headers: {X-Tier: "prod-*"}
            match: [{stage: ready}]
            not-match: [{blocked: true}]
            exist: [messages]
            not-exist: [disabled]
        target: {role: user, positions: [1, 2, 2]}
        replacements:
          - "you are hero": "you are fool"
          - "you are fool": "you are friend"
          - "remove": ""
      - models: [{name: "gemini-*", protocol: gemini}]
        target: {role: user, all: true}
        replacements:
          - "1": "false"
    filter: []
`

func TestPayloadReplaceConfigTreeRoundTrip(t *testing.T) {
	if errValidate := ValidateV8Config([]byte(payloadReplaceConfigFixture)); errValidate != nil {
		t.Fatal(errValidate)
	}
	cfg := Config{CredentialConcurrency: DefaultCredentialConcurrencyConfig(), CredentialInFlight: DefaultCredentialInFlightConfig()}
	if errDecode := yaml.Unmarshal([]byte(payloadReplaceConfigFixture), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	var submitted map[string]any
	if errDecode := yaml.Unmarshal([]byte(payloadReplaceConfigFixture), &submitted); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !reflect.DeepEqual(cfg.RawConfig, submitted) {
		t.Fatal("submitted replacement tree was changed")
	}
	original, errOriginal := NormalizeConfigRoot(submitted)
	if errOriginal != nil {
		t.Fatal(errOriginal)
	}
	legacy, errLegacy := LegacyConfigRoot(&cfg)
	if errLegacy != nil {
		t.Fatal(errLegacy)
	}
	for _, document := range []any{cfg, legacy} {
		encoded, errMarshal := yaml.Marshal(document)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		canonical, _, errNormalize := NormalizeConfigLayout(encoded, true)
		if errNormalize != nil {
			t.Fatal(errNormalize)
		}
		if errValidate := ValidateV8Config(canonical); errValidate != nil {
			t.Fatal(errValidate)
		}
		var restored Config
		if errDecode := yaml.Unmarshal(canonical, &restored); errDecode != nil {
			t.Fatal(errDecode)
		}
		root, errRoot := NormalizeConfigRoot(restored.RawConfig)
		if errRoot != nil {
			t.Fatal(errRoot)
		}
		if !reflect.DeepEqual(root["payload"], original["payload"]) {
			t.Fatalf("layout round trip changed payload: %s", canonical)
		}
		cpa, errParse := cpaconfig.ParseConfigBytes(canonical)
		if errParse != nil {
			t.Fatal(errParse)
		}
		rules := cpa.Payload.Replace
		if len(rules) != 2 || !reflect.DeepEqual(rules[0].Target.Positions, []int{1, 2, 2}) || !rules[1].Target.All {
			t.Fatalf("SDK lost replacement selectors: %+v", rules)
		}
		want := []cpaconfig.PayloadReplacement{
			{"you are hero": "you are fool"},
			{"you are fool": "you are friend"},
			{"remove": ""},
		}
		if !reflect.DeepEqual(rules[0].Replacements, want) || !reflect.DeepEqual(rules[1].Replacements, []cpaconfig.PayloadReplacement{{"1": "false"}}) {
			t.Fatalf("SDK changed replacement order or operands: %+v", rules)
		}
	}
}

func TestPayloadReplaceValidation(t *testing.T) {
	valid := `[{models: [{name: "*"}], target: {role: user, positions: [1, 2]}, replacements: [{"hero": "fool"}]}]`
	for _, tt := range []struct{ name, value string }{
		{"not list", `{}`},
		{"not object", `[null]`},
		{"missing role", strings.Replace(valid, "role: user, ", "", 1)},
		{"other role", strings.Replace(valid, "role: user", "role: assistant", 1)},
		{"implicit target", strings.Replace(valid, "positions: [1, 2]", "positions: []", 1)},
		{"zero", strings.Replace(valid, "[1, 2]", "[0]", 1)},
		{"negative", strings.Replace(valid, "[1, 2]", "[-1]", 1)},
		{"fractional", strings.Replace(valid, "[1, 2]", "[1.5]", 1)},
		{"string position", strings.Replace(valid, "[1, 2]", `["1"]`, 1)},
		{"overflow", strings.Replace(valid, "[1, 2]", "[99999999999999999999999999]", 1)},
		{"both selectors", strings.Replace(valid, "role: user", "role: user, all: true", 1)},
		{"nonboolean all", strings.Replace(valid, "role: user", `role: user, all: "true"`, 1)},
		{"no replacements", strings.Replace(valid, `[{"hero": "fool"}]`, `[]`, 1)},
		{"empty source", strings.Replace(valid, `"hero"`, `""`, 1)},
		{"numeric source", strings.Replace(valid, `"hero"`, `1`, 1)},
		{"numeric destination", strings.Replace(valid, `"fool"`, `1`, 1)},
		{"null destination", strings.Replace(valid, `"fool"`, `null`, 1)},
		{"empty mapping", strings.Replace(valid, `{"hero": "fool"}`, `{}`, 1)},
		{"multiple mappings", strings.Replace(valid, `{"hero": "fool"}`, `{"hero": "fool", "other": "text"}`, 1)},
		{"unknown rule field", strings.Replace(valid, "models:", "typo:", 1)},
		{"unknown target field", strings.Replace(valid, "positions:", "position:", 1)},
		{"unknown model field", strings.Replace(valid, "name:", "model:", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte("config-version: 8\nrequests:\n  payload:\n    replace: " + tt.value + "\n")
			if errValidate := ValidateV8Config(source); errValidate == nil {
				t.Fatalf("invalid replacement accepted: %s", source)
			}
		})
	}
	for _, value := range []string{valid, `[]`, `null`, strings.Replace(valid, "positions: [1, 2]", "all: true", 1), strings.Replace(valid, `"fool"`, `""`, 1)} {
		source := []byte("config-version: 8\nrequests:\n  payload:\n    replace: " + value + "\n")
		if errValidate := ValidateV8Config(source); errValidate != nil {
			t.Fatalf("valid replacement rejected: %v", errValidate)
		}
	}
}

func TestPayloadReplaceLegacyImportValidation(t *testing.T) {
	for _, value := range []string{`[{target: {role: user, positions: [0]}, replacements: [{"a": "b"}]}]`, `[{target: {role: user, all: true}, replacements: [{"a": null}]}]`} {
		var cfg Config
		if errDecode := yaml.Unmarshal([]byte("payload:\n  replace: "+value), &cfg); errDecode == nil {
			t.Fatal("legacy import accepted invalid replacement")
		}
	}
}
