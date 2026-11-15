package home

import (
	"fmt"
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestSanitizeConfigYAMLForDownstreamAntigravityTOSPauseWindow(t *testing.T) {
	type testCase struct {
		name   string
		source string
		want   string
	}
	tests := []testCase{
		{name: "empty", source: "{}", want: "0"},
		{name: "v8 without oauth", source: "config-version: 8", want: "0"},
		{name: "v8 without providers", source: "oauth: {settings: {}}", want: "0"},
		{name: "v8 without antigravity", source: "oauth: {providers: {codex: {signin-with-chatgpt-identifier: preserved}}}", want: "0"},
		{name: "legacy empty provider", source: "antigravity: {}", want: "0"},
		{name: "legacy null provider", source: "antigravity: null", want: "0"},
		{name: "v8 empty provider", source: "oauth: {providers: {antigravity: {}}}", want: "0"},
	}
	for _, layout := range []struct {
		name   string
		format string
	}{
		{name: "legacy", format: "antigravity: {%s}\ncodex: {signin-with-chatgpt-identifier: preserved}\n"},
		{name: "v8", format: "config-version: 8\noauth:\n  providers:\n    antigravity: {%s}\n    codex: {signin-with-chatgpt-identifier: preserved}\n"},
	} {
		for _, window := range []struct {
			name  string
			field string
			want  string
		}{
			{name: "omitted", want: "0"},
			{name: "null", field: ", tos-pause-window: null", want: "0"},
			{name: "empty", field: `, tos-pause-window: ""`, want: "0"},
			{name: "whitespace", field: `, tos-pause-window: " \t "`, want: "0"},
			{name: "custom", field: `, tos-pause-window: "3h"`, want: "3h"},
			{name: "zero", field: `, tos-pause-window: "0"`, want: "0"},
			{name: "off", field: `, tos-pause-window: "off"`, want: "off"},
			{name: "false", field: `, tos-pause-window: "false"`, want: "false"},
			{name: "none", field: `, tos-pause-window: "none"`, want: "none"},
			{name: "disabled", field: `, tos-pause-window: "disabled"`, want: "disabled"},
		} {
			tests = append(tests, testCase{
				name:   layout.name + "/" + window.name,
				source: fmt.Sprintf(layout.format, "sensitive-words: [preserved], connection-pool: {enabled: true}"+window.field),
				want:   window.want,
			})
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original, errParseOriginal := cpaconfig.ParseConfigBytes([]byte(tc.source))
			if errParseOriginal != nil {
				t.Fatal(errParseOriginal)
			}
			wire, errSanitize := sanitizeConfigYAMLForDownstream([]byte(tc.source))
			if errSanitize != nil {
				t.Fatal(errSanitize)
			}
			cpa, errParse := cpaconfig.ParseConfigBytes(wire)
			if errParse != nil {
				t.Fatal(errParse)
			}
			// CPA v8.0.70 uses a 6h executor default for an empty value;
			// decoding "0" must explicitly disable it instead.
			if cpa.Antigravity.TOSPauseWindow != tc.want {
				t.Fatalf("CPA TOSPauseWindow = %q, want %q", cpa.Antigravity.TOSPauseWindow, tc.want)
			}
			original.Antigravity.TOSPauseWindow = tc.want
			if !reflect.DeepEqual(cpa.Antigravity, original.Antigravity) || !reflect.DeepEqual(cpa.Codex, original.Codex) {
				t.Fatal("downstream projection changed other provider settings")
			}
			var root map[string]any
			if errDecode := yaml.Unmarshal(wire, &root); errDecode != nil {
				t.Fatal(errDecode)
			}
			legacy := root["antigravity"].(map[string]any)
			if legacy["tos-pause-window"] != tc.want {
				t.Fatalf("legacy tos-pause-window = %#v, want %q", legacy["tos-pause-window"], tc.want)
			}
			if oauth, ok := root["oauth"].(map[string]any); ok {
				if providers, okProviders := oauth["providers"].(map[string]any); okProviders {
					if scoped, okScoped := providers["antigravity"].(map[string]any); okScoped && !reflect.DeepEqual(scoped, legacy) {
						t.Fatalf("OAuth and legacy projections differ: %#v != %#v", scoped, legacy)
					}
				}
			}
		})
	}
}

func TestSanitizeConfigYAMLForDownstreamRejectsInvalidAntigravityMapping(t *testing.T) {
	for _, source := range []string{
		"antigravity: invalid",
		"antigravity: []",
		"oauth: {providers: {antigravity: invalid}}",
		"oauth: {providers: {antigravity: []}}",
	} {
		if wire, errSanitize := sanitizeConfigYAMLForDownstream([]byte(source)); errSanitize == nil || wire != nil {
			t.Fatalf("invalid provider mapping produced downstream config: %s (error: %v)", wire, errSanitize)
		}
	}
}
