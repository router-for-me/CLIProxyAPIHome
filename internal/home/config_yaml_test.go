package home

import (
	"strings"
	"testing"
)

// TestSanitizeConfigYAMLForDownstream_RemovesSensitiveKeys verifies test sanitize config yaml for downstream_ removes sensitive keys behavior.
func TestSanitizeConfigYAMLForDownstream_RemovesSensitiveKeys(t *testing.T) {
	// Normalize source data before building the derived payload.
	input := strings.TrimSpace(`
# head comment
host: ""
port: 8327
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
codex-api-key:
  - api-key: "c1"
xai-api-key:
  - api-key: "x1"
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
	assertContains("port:")
	assertContains("usage-statistics-enabled: true")
	assertContains("disable-cooling: true")
	assertContains("ws-auth: false")
	assertContains("plugins:")
	assertContains("sample:")
	assertContains("mode: fast")
	assertContains("value: keep")

	assertNotContains("tls:")
	assertNotContains("remote-management:")
	assertNotContains("trusted-proxies:")
	assertNotContains("user-email:")
	assertNotContains("HOME_USER_EMAIL_SMTP_PASSWORD")
	assertNotContains("auth-dir:")
	assertNotContains("api-keys:")
	assertNotContains("gemini-api-key:")
	assertNotContains("codex-api-key:")
	assertNotContains("xai-api-key:")
	assertNotContains("claude-api-key:")
	assertNotContains("openai-compatibility:")
	assertNotContains("vertex-api-key:")
	assertNotContains("oauth-model-alias:")
	assertNotContains("oauth-excluded-models:")

	assertNotContains("head comment")
}
