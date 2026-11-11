package home

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSanitizeConfigYAMLForDownstreamPreservesModelCatalogs(t *testing.T) {
	for name, version := range map[string]string{"legacy": "", "v8": "config-version: 8\n"} {
		t.Run(name, func(t *testing.T) {
			input := version + `models:
  catalog: "https://example.com/models.json?revision=1&format=json"
  codex-catalog: "http://localhost/codex.json"
  devin-catalog: "/tmp/devin models.json"
`
			output, errSanitize := sanitizeConfigYAMLForDownstream([]byte(input))
			if errSanitize != nil {
				t.Fatal(errSanitize)
			}
			var original, downstream map[string]any
			if errUnmarshal := yaml.Unmarshal([]byte(input), &original); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if errUnmarshal := yaml.Unmarshal(output, &downstream); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if !reflect.DeepEqual(downstream["models"], original["models"]) {
				t.Fatalf("downstream models = %#v, want %#v", downstream["models"], original["models"])
			}
		})
	}
}

// TestSanitizeConfigYAMLForDownstream_RemovesSensitiveKeys verifies test sanitize config yaml for downstream_ removes sensitive keys behavior.
func TestSanitizeConfigYAMLForDownstream_RemovesSensitiveKeys(t *testing.T) {
	// Normalize source data before building the derived payload.
	input := strings.TrimSpace(`
# head comment
host: ""
port: 8317
disable-cooling: false
tls:
  enable: false
remote-management:
  allow-remote: false
trusted-proxies:
  - "127.0.0.1"
user-email:
  enabled: true
  public-user-url: "https://home.example.com/user.html"
  from-address: "no-reply@example.com"
  sender:
    type: smtp
    smtp:
      host: "smtp.example.com"
      password-env: "HOME_USER_EMAIL_SMTP_PASSWORD"
auth-dir: "~/.cli-proxy-api-home"
api-keys:
  - "k1"
gemini-api-key:
  - api-key: "g1"
interactions-api-key:
  - api-key: "i1"
codex-api-key:
  - api-key: "c1"
xai-api-key:
  - api-key: "x1"
meta-api-key:
  - api-key: "m1"
claude-api-key:
  - api-key: "a1"
openai-compatibility:
  - name: "openrouter"
vertex-api-key:
  - api-key: "v1"
oauth-model-alias:
  codex:
    - name: "gpt-5"
      alias: "g5"
oauth-excluded-models:
  codex:
    - "gpt-5-codex-mini"
plugins:
  enabled: true
  dir: "plugins"
  configs:
    sample:
      enabled: true
      priority: 7
      mode: fast
      nested:
        value: keep
`) + "\n"

	out, err := sanitizeConfigYAMLForDownstream([]byte(input))
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}

	assertNotContains := func(needle string) {
		t.Helper()
		if strings.Contains(string(out), needle) {
			t.Fatalf("expected output to not contain %q, got:\n%s", needle, string(out))
		}
	}

	assertContains := func(needle string) {
		t.Helper()
		if !strings.Contains(string(out), needle) {
			t.Fatalf("expected output to contain %q, got:\n%s", needle, string(out))
		}
	}

	assertContains("host:")
	assertContains("port: 8317")
	assertContains("usage-statistics-enabled: true")
	assertContains("disable-cooling: true")
	assertContains("ws-auth: false")
	assertContains("plugins:")
	assertContains("sample:")
	assertContains("mode: fast")
	assertContains("value: keep")
	assertContains("trusted-proxies:")
	assertContains("127.0.0.1")

	assertNotContains("tls:")
	assertNotContains("remote-management:")
	assertNotContains("user-email:")
	assertNotContains("HOME_USER_EMAIL_SMTP_PASSWORD")
	assertNotContains("auth-dir:")
	assertNotContains("api-keys:")
	assertNotContains("gemini-api-key:")
	assertNotContains("interactions-api-key:")
	assertNotContains("codex-api-key:")
	assertNotContains("xai-api-key:")
	assertNotContains("meta-api-key:")
	assertNotContains("claude-api-key:")
	assertNotContains("openai-compatibility:")
	assertNotContains("vertex-api-key:")
	assertNotContains("oauth-model-alias:")
	assertNotContains("oauth-excluded-models:")

	assertNotContains("head comment")
}

func TestSanitizeConfigYAMLForDownstreamNullDocumentAppliesHomeModeScalars(t *testing.T) {
	out, errSanitize := sanitizeConfigYAMLForDownstream([]byte("null\n"))
	if errSanitize != nil {
		t.Fatalf("sanitizeConfigYAMLForDownstream() error = %v", errSanitize)
	}
	for _, expected := range []string{
		"usage-statistics-enabled: true",
		"disable-cooling: true",
		"ws-auth: false",
	} {
		if !strings.Contains(string(out), expected) {
			t.Fatalf("downstream config = %q, want %q", out, expected)
		}
	}
}

// TestSanitizeConfigYAMLForDownstreamForwardsClaudeToolSearch verifies the v8
// upstream.claude.tool-search switch reaches CPA in the flat layout it reads.
func TestSanitizeConfigYAMLForDownstreamForwardsClaudeToolSearch(t *testing.T) {
	out, errSanitize := sanitizeConfigYAMLForDownstream([]byte("config-version: 8\nupstream:\n  claude:\n    model-level-cooling: true\n    tool-search: true\n"))
	if errSanitize != nil {
		t.Fatalf("sanitizeConfigYAMLForDownstream() error = %v", errSanitize)
	}
	want := "claude:\n    model-level-cooling: true\n    tool-search: true\n"
	if !strings.Contains(string(out), want) {
		t.Fatalf("downstream config = %q, want %q", out, want)
	}
	if strings.Contains(string(out), "upstream:") {
		t.Fatalf("downstream config kept a v8 upstream block: %q", out)
	}
}

func TestSanitizeConfigYAMLForDownstreamForwardsSensitiveWordsReplace(t *testing.T) {
	input := `config-version: 8
oauth:
  providers:
    antigravity:
      sensitive-words-replace:
        "You are Codex, an agent based on GPT-5.": "You are a helpful coding assistant."
        "RemoveOnly": ""
    devin:
      sensitive-words-replace:
        "Claude Code": "Devin"
`
	out, errSanitize := sanitizeConfigYAMLForDownstream([]byte(input))
	if errSanitize != nil {
		t.Fatalf("sanitizeConfigYAMLForDownstream() error = %v", errSanitize)
	}
	var decoded struct {
		Antigravity struct {
			SensitiveWordsReplace map[string]string `yaml:"sensitive-words-replace"`
		} `yaml:"antigravity"`
		Devin struct {
			SensitiveWordsReplace map[string]string `yaml:"sensitive-words-replace"`
		} `yaml:"devin"`
	}
	if errUnmarshal := yaml.Unmarshal(out, &decoded); errUnmarshal != nil {
		t.Fatalf("yaml.Unmarshal(out) error = %v", errUnmarshal)
	}
	if decoded.Antigravity.SensitiveWordsReplace["You are Codex, an agent based on GPT-5."] != "You are a helpful coding assistant." {
		t.Fatalf("downstream antigravity replacements = %#v", decoded.Antigravity.SensitiveWordsReplace)
	}
	if val, ok := decoded.Antigravity.SensitiveWordsReplace["RemoveOnly"]; !ok || val != "" {
		t.Fatalf("downstream empty deletion lost: %#v", decoded.Antigravity.SensitiveWordsReplace)
	}
	if decoded.Devin.SensitiveWordsReplace["Claude Code"] != "Devin" {
		t.Fatalf("downstream devin replacements = %#v", decoded.Devin.SensitiveWordsReplace)
	}
}
