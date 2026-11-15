package config

import (
	"os"
	"strings"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"gopkg.in/yaml.v3"
)

func TestV8ConfigLayoutCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, source string
	}{
		{"legacy", "port: 8327\nrequest-retry: 0\ndisable-cooling: false\ncodex-api-key: [{api-key: test-key, base-url: 'https://example.test', request-retry: 0, disable-cooling: false}]\n"},
		{"v8", "config-version: 8\nserver: {port: 8327}\nrouting: {retry: {request-retry: 0}, cooldown: {disable-cooling: false}}\napi-keys: {codex: [{name: example, base-url: 'https://example.test', request-retry: 2, disable-cooling: true, keys: [{api-key: test-key, request-retry: 0, disable-cooling: false}]}]}\n"},
		{"mixed", "request-retry: 9\nrouting: {retry: {request-retry: 0}}\nserver: {port: 8327}\napi-keys: {codex: [{base-url: 'https://example.test', request-retry: 0, disable-cooling: false, keys: [{api-key: test-key, disable-cooling: null}]}]}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.source), &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Port != 8327 || cfg.RequestRetry != 0 || len(cfg.CodexKey) != 1 {
				t.Fatalf("unexpected effective config: port=%d retry=%d keys=%d", cfg.Port, cfg.RequestRetry, len(cfg.CodexKey))
			}
			key := cfg.CodexKey[0]
			if key.DisableCooling == nil || *key.DisableCooling || key.RequestRetry == nil || *key.RequestRetry != 0 {
				t.Fatal("explicit false/zero or null inheritance lost")
			}
			migrated, _, err := NormalizeConfigLayout([]byte(tc.source), true)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(migrated); err != nil {
				t.Fatal(err)
			}
			cpa, err := cpaconfig.ParseConfigBytes(migrated)
			if err != nil {
				t.Fatal(err)
			}
			if cpa.Port != cfg.Port || cpa.RequestRetry != cfg.RequestRetry || len(cpa.CodexKey) != 1 || cpa.CodexKey[0].APIKey != key.APIKey {
				t.Fatal("Home/CPA effective config differs")
			}
		})
	}
}

func TestV8ConfigOAuthScopeSurvivesSnapshots(t *testing.T) {
	var root map[string]any
	if err := yaml.Unmarshal([]byte("oauth: {providers: {codex: {disable-codex-cloaking: true}}}\n"), &root); err != nil {
		t.Fatal(err)
	}
	root, err := NormalizeConfigRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	cfg.CredentialConcurrency = DefaultCredentialConcurrencyConfig()
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OAuthOnlyFields["codex.disable-codex-cloaking"] {
		t.Fatal("shared Codex setting retained OAuth-only scope in storage")
	}
	data, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cpa, err := cpaconfig.ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !cpa.Codex.DisableCodexCloaking || !cpa.ForAPIKey().Codex.DisableCodexCloaking {
		t.Fatal("shared settings did not reach API-key execution")
	}
}

func TestV8ConfigRejectsInvalidLayouts(t *testing.T) {
	for _, source := range []string{
		"config-version: 9", "server: false", "routing: {retry: {typo: 1}}",
		"home: {enabled: true}", "unknown-root: true", "server: {unknown-field: true}",
		"debug: true", "api-keys: {unknown: []}", "api-keys: {codex: [{keys: [{api-key: key, base-url: x}]}]}",
		"api-keys: {codex: [{keys: [{api-key: key, weight: 1.5}]}]}", "server: {port: 1, port: 2}",
	} {
		t.Run(source, func(t *testing.T) {
			if err := ValidateV8Config([]byte(source)); err == nil {
				t.Fatal("invalid v8 layout accepted")
			}
		})
	}
}

func TestV8ConfigExample(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if version := yamlPath(doc.Content[0], "config-version"); version == nil || version.Value != "8" {
		t.Fatal("missing V8 configuration version")
	}
	if yamlPath(doc.Content[0], "quota-exceeded") != nil || strings.Contains(string(data), "quota-exceeded:") {
		t.Fatal("obsolete quota section returned to the V8 example")
	}
	for _, path := range []string{"server", "management", "access", "credentials", "routing", "requests", "oauth", "multimedia", "observability", "plugins", "user-email"} {
		if yamlPath(doc.Content[0], path) == nil {
			t.Fatalf("missing configuration section %s", path)
		}
	}
	active, errParse := cpaconfig.ParseConfigBytes(data)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if active.Port != 8317 || active.RequestRetry != 3 || !active.QuotaExceeded.AntigravityCredits || active.QuotaExceeded.SwitchProject || active.QuotaExceeded.SwitchPreviewModel || !active.UsageStatisticsEnabled {
		t.Fatal("template defaults do not match the Home configuration contract")
	}
	for _, count := range []int{len(active.GeminiKey), len(active.InteractionsKey), len(active.VertexCompatAPIKey), len(active.CodexKey), len(active.ClaudeKey), len(active.XAIKey), len(active.MetaKey), len(active.OpenAICompatibility)} {
		if count != 0 {
			t.Fatal("placeholder credentials must remain commented")
		}
	}
}

func TestV8ConfigExampleProviderBlocks(t *testing.T) {
	data, errRead := os.ReadFile("../../config.example.yaml")
	if errRead != nil {
		t.Fatal(errRead)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	_, block, found := strings.Cut(text, "# BEGIN API KEY EXAMPLES\n")
	if !found {
		t.Fatal("provider examples are missing")
	}
	block, _, found = strings.Cut(block, "# END API KEY EXAMPLES")
	if !found {
		t.Fatal("provider example end marker is missing")
	}
	var uncommented strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(block, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			t.Fatal("placeholder credentials must remain commented")
		}
		uncommented.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "))
		uncommented.WriteByte('\n')
	}
	data = []byte(text + "\n" + uncommented.String())
	if errValidate := ValidateV8Config(data); errValidate != nil {
		t.Fatal(errValidate)
	}
	var cfg Config
	if errDecode := yaml.Unmarshal(data, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(cfg.GeminiKey) != 3 || len(cfg.InteractionsKey) != 1 || len(cfg.VertexCompatAPIKey) != 1 || len(cfg.CodexKey) != 1 || len(cfg.ClaudeKey) != 2 || len(cfg.XAIKey) != 1 || len(cfg.MetaKey) != 1 || len(cfg.OpenAICompatibility) != 1 {
		t.Fatal("provider examples did not populate all eight credential families")
	}
	key := cfg.GeminiKey[1]
	if key.RequestRetry == nil || *key.RequestRetry != 0 || key.DisableCooling == nil || *key.DisableCooling || key.ProxyURL != "direct" || len(key.ExcludedModels) != 0 || len(key.Models) != 1 {
		t.Fatal("provider example lost explicit overrides or inherited models")
	}
	if cfg.CodexKey[0].Weight == nil || *cfg.CodexKey[0].Weight != 5 || cfg.CodexKey[0].Models[0].Thinking == nil || cfg.ClaudeKey[1].Models[0].DisplayName == "" {
		t.Fatal("provider model capabilities or weight examples were lost")
	}
}

func TestV8MigrationRemovesUnknownLegacySections(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
	})
	raw := []byte(`home: {enabled: true, host: ignored.example}
enable-gemini-cli-endpoint: true
former-feature:
  token: fixture-secret
  items: [first, second]
proxy-url: direct
request-retry: 0
allow-host: [127.0.0.1]
user-email: {}
tls: {}
`)
	unchanged, changed, errNormalize := NormalizeConfigLayout(raw, false)
	if errNormalize != nil || changed || string(unchanged) != string(raw) || len(hook.AllEntries()) != 0 {
		t.Fatalf("legacy read changed configuration or emitted warnings: changed=%v error=%v", changed, errNormalize)
	}
	migrated, _, errMigrate := NormalizeConfigLayout(raw, true)
	if errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatal(errValidate)
	}
	var doc yaml.Node
	if errDecode := yaml.Unmarshal(migrated, &doc); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, key := range []string{"home", "enable-gemini-cli-endpoint", "former-feature"} {
		if yamlPath(doc.Content[0], key) != nil || strings.Contains(string(migrated), key+":") {
			t.Fatalf("unknown section %s remained in the migrated configuration", key)
		}
	}
	for _, path := range []string{"server.allow-host", "server.tls", "user-email", "requests.proxy-url", "routing.retry.request-retry"} {
		if yamlPath(doc.Content[0], path) == nil {
			t.Fatalf("known setting %s was removed", path)
		}
	}
	if strings.Contains(string(migrated), "fixture-secret") {
		t.Fatal("ignored values were retained as comments")
	}
	entries := hook.AllEntries()
	if len(entries) != 3 {
		t.Fatalf("migration warning count=%d, want 3", len(entries))
	}
	for _, entry := range entries {
		if entry.Level != log.WarnLevel || !strings.Contains(entry.Message, "unrecognized configuration section") || strings.Contains(entry.Message, "fixture-secret") {
			t.Fatalf("unexpected migration warning: %s", entry.Message)
		}
	}
	hook.Reset()
	remigrated, _, errRemigrate := NormalizeConfigLayout(migrated, true)
	if errRemigrate != nil || len(hook.AllEntries()) != 0 {
		t.Fatalf("repeated migration emitted warnings: %v", errRemigrate)
	}
	if string(remigrated) != string(migrated) {
		t.Fatal("repeated migration changed the normalized configuration")
	}
}

func TestV8MigrationRemovesRetiredFieldsAndPreservesSessionAffinity(t *testing.T) {
	for _, test := range []struct {
		name    string
		routing string
		want    bool
	}{
		{name: "legacy enabled", routing: "{claude-code-session-affinity: true}", want: true},
		{name: "legacy disabled", routing: "{claude-code-session-affinity: false}"},
		{name: "legacy enabled with canonical false", routing: "{claude-code-session-affinity: true, session-affinity: false}", want: true},
		{name: "canonical enabled", routing: "{claude-code-session-affinity: false, session-affinity: true}", want: true},
		{name: "both disabled", routing: "{claude-code-session-affinity: false, session-affinity: false}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []byte("routing: " + test.routing + "\ncodex: {identity-confuse: true}\noauth: {providers: {codex: {identity-confuse: false, disable-codex-cloaking: true}}}\n")
			var cfg Config
			if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
				t.Fatal(errDecode)
			}
			if cfg.Routing.SessionAffinity != test.want || !cfg.Codex.DisableCodexCloaking {
				t.Fatal("legacy config load changed effective settings")
			}
			unchanged, changed, errNormalize := NormalizeConfigLayout(source, false)
			if errNormalize != nil || changed || string(unchanged) != string(source) {
				t.Fatalf("read rewrote the source: changed=%v err=%v", changed, errNormalize)
			}
			migrated, _, errMigrate := NormalizeConfigLayout(source, true)
			if errMigrate != nil {
				t.Fatal(errMigrate)
			}
			if errValidate := ValidateV8Config(migrated); errValidate != nil {
				t.Fatal(errValidate)
			}
			var migratedRoot map[string]any
			if errDecode := yaml.Unmarshal(migrated, &migratedRoot); errDecode != nil {
				t.Fatal(errDecode)
			}
			compatible, errNormalize := NormalizeConfigRoot(migratedRoot)
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			compatibleData, errMarshal := yaml.Marshal(compatible)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			cpa, errParse := cpaconfig.ParseConfigBytes(compatibleData)
			if errParse != nil || cpa.Routing.SessionAffinity != test.want || !cpa.Codex.DisableCodexCloaking {
				t.Fatalf("migration changed CPA settings: %v", errParse)
			}
			var stored map[string]any
			if errDecode := yaml.Unmarshal(source, &stored); errDecode != nil {
				t.Fatal(errDecode)
			}
			normalized, errRoot := NormalizeConfigRoot(stored)
			if errRoot != nil {
				t.Fatal(errRoot)
			}
			storedData, errMarshal := yaml.Marshal(normalized)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			for _, data := range [][]byte{migrated, storedData} {
				for _, removed := range []string{"identity-confuse", "claude-code-session-affinity"} {
					if strings.Contains(string(data), removed) {
						t.Fatalf("removed field %s survived normalization", removed)
					}
				}
			}
			var restored Config
			if errDecode := yaml.Unmarshal(storedData, &restored); errDecode != nil || restored.Routing.SessionAffinity != test.want {
				t.Fatalf("database projection changed affinity: %v", errDecode)
			}
		})
	}
}

func TestV8ConfigRejectsRetiredFields(t *testing.T) {
	for _, source := range []string{
		"codex: {identity-confuse: true}",
		"oauth: {providers: {codex: {identity-confuse: false}}}",
		"routing: {claude-code-session-affinity: true}",
		"routing: {claude-code-session-affinity: false, session-affinity: true}",
	} {
		t.Run(source, func(t *testing.T) {
			if errValidate := ValidateV8Config([]byte(source)); errValidate == nil {
				t.Fatal("v8 write accepted a removed field")
			}
		})
	}
}
