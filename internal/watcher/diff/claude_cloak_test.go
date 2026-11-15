package diff

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestClaudeCloakSDKPatternDiff(t *testing.T) {
	const first = "{pattern: private-first, replacement: private-one}"
	const second = "{pattern: private-second, replacement: private-two}"
	for _, tc := range []struct {
		name, oldCloak, newCloak, want string
	}{
		{"unchanged", "{sensitive-patterns-replace: [" + first + "]}", "{sensitive-patterns-replace: [" + first + "]}", ""},
		{"nil-empty", "{}", "{sensitive-patterns-replace: []}", ""},
		{"added-rule", "{sensitive-patterns-replace: [" + first + "]}", "{sensitive-patterns-replace: [" + first + "," + second + "]}", "claude[0].cloak.sensitive-patterns-replace: 1 -> 2"},
		{"removed-rule", "{sensitive-patterns-replace: [" + first + "," + second + "]}", "{sensitive-patterns-replace: [" + first + "]}", "claude[0].cloak.sensitive-patterns-replace: 2 -> 1"},
		{"changed-pattern", "{sensitive-patterns-replace: [" + first + "]}", "{sensitive-patterns-replace: [{pattern: private-new, replacement: private-one}]}", "claude[0].cloak.sensitive-patterns-replace: 1 -> 1"},
		{"changed-replacement", "{sensitive-patterns-replace: [" + first + "]}", "{sensitive-patterns-replace: [{pattern: private-first, replacement: private-new}]}", "claude[0].cloak.sensitive-patterns-replace: 1 -> 1"},
		{"changed-order", "{sensitive-patterns-replace: [" + first + "," + second + "]}", "{sensitive-patterns-replace: [" + second + "," + first + "]}", "claude[0].cloak.sensitive-patterns-replace: 2 -> 2"},
		{"cleared-rules", "{sensitive-patterns-replace: [" + first + "]}", "{sensitive-patterns-replace: []}", "claude[0].cloak.sensitive-patterns-replace: 1 -> 0"},
		{"added-cloak", "null", "{sensitive-patterns-replace: [" + first + "]}", "claude[0].cloak: added"},
		{"removed-cloak", "{sensitive-patterns-replace: [" + first + "]}", "null", "claude[0].cloak: removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configs [2]config.Config
			var before [2][]byte
			for i, cloak := range [2]string{tc.oldCloak, tc.newCloak} {
				source := fmt.Sprintf("claude-api-key: [{api-key: private-key, cloak: %s}]", cloak)
				if errDecode := yaml.Unmarshal([]byte(source), &configs[i]); errDecode != nil {
					t.Fatal(errDecode)
				}
				var errMarshal error
				before[i], errMarshal = yaml.Marshal(&configs[i])
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
			}
			changes := BuildConfigChangeDetails(&configs[0], &configs[1])
			if tc.want == "" {
				if len(changes) != 0 {
					t.Fatalf("unexpected changes: %v", changes)
				}
			} else if !reflect.DeepEqual(changes, []string{tc.want}) {
				t.Fatalf("changes = %v, want %q", changes, tc.want)
			}
			if strings.Contains(strings.Join(changes, ";"), "private-") {
				t.Fatal("diff leaked credential or rule content")
			}
			for i := range configs {
				after, errMarshal := yaml.Marshal(&configs[i])
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if !bytes.Equal(before[i], after) {
					t.Fatal("SDK diff mutated the source tree")
				}
			}
		})
	}
}

func TestClaudeCloakSDKDiffUsesAuthoritativeTree(t *testing.T) {
	oldCfg := &config.Config{ClaudeKey: []config.ClaudeKey{{
		Cloak:            &config.CloakConfig{Mode: "always"},
		CredentialConfig: config.CredentialConfig{RawConfig: map[string]any{"cloak": map[string]any{"mode": "never"}, "base-url": "https://old.example"}},
	}}}
	newCfg := &config.Config{ClaudeKey: []config.ClaudeKey{{
		Cloak:            &config.CloakConfig{Mode: "auto"},
		CredentialConfig: config.CredentialConfig{RawConfig: map[string]any{"cloak": map[string]any{"mode": "never"}, "base-url": "https://new.example"}},
	}}}
	if changes := BuildConfigChangeDetails(oldCfg, newCfg); len(changes) != 0 {
		t.Fatalf("diff compared stale projections or emitted a generic execution-options change: %v", changes)
	}
}

func TestClaudeCloakSDKDiffMalformedOptionsAreRedacted(t *testing.T) {
	oldCfg := &config.Config{ClaudeKey: []config.ClaudeKey{{}}}
	newCfg := &config.Config{ClaudeKey: []config.ClaudeKey{{
		CredentialConfig: config.CredentialConfig{RawConfig: map[string]any{"cloak": map[string]any{"sensitive-patterns-replace": "private-invalid-value"}}},
	}}}
	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	if !reflect.DeepEqual(changes, []string{"claude[0].cloak: invalid configuration (comparison skipped)"}) {
		t.Fatalf("malformed SDK option was silently ignored or leaked: %v", changes)
	}
}
