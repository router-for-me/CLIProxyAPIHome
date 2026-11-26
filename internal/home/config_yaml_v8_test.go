package home

import (
	"strings"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestV8DownstreamConfigFiltersSecretsAndKeepsScope(t *testing.T) {
	source := `config-version: 8
server:
  port: 8427
  tls: {enable: true, cert: secret-cert, key: secret-private-key}
management: {secret-key: management-secret}
access: {api-keys: [client-secret]}
api-keys:
  codex: [{base-url: 'https://example.test', keys: [{api-key: upstream-secret}]}]
oauth:
  auth-dir: secret-auth-dir
  model-alias: {codex: [{name: hidden-model, alias: hidden-alias}]}
  providers:
    codex: {disable-codex-cloaking: true, header-defaults: {user-agent: oauth-agent}}
    aistudio: {ws-auth: true}
routing:
  cooldown: {disable-cooling: false}
requests:
  tls-handshake-timeout-seconds: 12
observability:
  usage: {usage-statistics-enabled: false}
`
	data, err := sanitizeConfigYAMLForDownstream([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-", "management-secret", "client-secret", "upstream-secret", "hidden-alias"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("downstream contains filtered value %q", secret)
		}
	}
	cpa, err := cpaconfig.ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if cpa.Port != 8427 || !cpa.DisableCooling || !cpa.UsageStatisticsEnabled {
		t.Fatal("Home invariants lost")
	}
	// The bundled CPA release predates this field, so verify the emitted
	// legacy projection that current CPA maps to
	// requests.tls-handshake-timeout-seconds.
	var downstream map[string]any
	if errDecode := yaml.Unmarshal(data, &downstream); errDecode != nil {
		t.Fatalf("decode downstream config: %v", errDecode)
	}
	if strings.Contains(string(data), "ws-auth:") {
		t.Fatalf("Home-only WebSocket auth switch reached CPA: %s", data)
	}
	if downstream["tls-handshake-timeout-seconds"] != 12 {
		t.Fatalf("downstream tls-handshake-timeout-seconds = %#v, want 12", downstream["tls-handshake-timeout-seconds"])
	}
	if !cpa.Codex.DisableCodexCloaking || !cpa.ForAPIKey().Codex.DisableCodexCloaking || cpa.CodexHeaderDefaults.UserAgent != "oauth-agent" || cpa.ForAPIKey().CodexHeaderDefaults.UserAgent != "" {
		t.Fatal("shared client settings or OAuth-only header scope lost")
	}
	if !strings.Contains(string(data), "disable-codex-cloaking: true") || !strings.Contains(string(data), "port: 8427") || !strings.Contains(string(data), "tls-handshake-timeout-seconds: 12") {
		t.Fatal("legacy projection missing")
	}
}
