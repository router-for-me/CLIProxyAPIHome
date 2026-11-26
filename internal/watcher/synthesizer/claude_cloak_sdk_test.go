package synthesizer

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestClaudeCloakPatternsUseSDKWithoutChangingSource(t *testing.T) {
	var cfg config.Config
	if errDecode := yaml.Unmarshal([]byte(`config-version: 8
api-keys:
  claude:
    - keys:
        - api-key: test-key
          cloak:
            mode: always
            sensitive-patterns-replace:
              - pattern: "^private"
                replacement: ""
              - pattern: "[invalid"
                replacement: ignored
              - pattern: "Claude[ ]+Code"
                replacement: Claude
`), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	before, errBefore := yaml.Marshal(&cfg)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: &cfg, Now: time.Now(), IDGenerator: NewStableIDGenerator()})
	if errSynthesize != nil || len(auths) != 1 {
		t.Fatalf("synthesis = %d credentials, %v", len(auths), errSynthesize)
	}
	var patterns []map[string]string
	if errDecode := json.Unmarshal([]byte(auths[0].Attributes["cloak_sensitive_patterns_replace"]), &patterns); errDecode != nil {
		t.Fatal(errDecode)
	}
	want := []map[string]string{{"pattern": "^private", "replacement": ""}, {"pattern": "Claude[ ]+Code", "replacement": "Claude"}}
	if !reflect.DeepEqual(patterns, want) {
		t.Fatalf("SDK pattern projection = %#v, want %#v", patterns, want)
	}
	if !reflect.DeepEqual(auths[0].Metadata["cloak_sensitive_patterns_replace"], want) {
		t.Fatalf("metadata patterns = %#v", auths[0].Metadata["cloak_sensitive_patterns_replace"])
	}
	after, errAfter := yaml.Marshal(&cfg)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("SDK normalization changed the saved source tree")
	}
}
