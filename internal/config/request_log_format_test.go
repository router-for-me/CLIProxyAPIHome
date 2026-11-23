package config

import (
	"strings"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestValidateV8ConfigAcceptsObservabilityLogsRequestLogFormat(t *testing.T) {
	source := []byte("observability:\n  logs:\n    request-log: true\n    request-log-format: json\n")
	if errValidate := ValidateV8Config(source); errValidate != nil {
		t.Fatalf("ValidateV8Config() error = %v", errValidate)
	}
	var cfg Config
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errDecode)
	}
	if !cfg.RequestLog || cfg.RequestLogFormat != "json" {
		t.Fatalf("request-log=%t request-log-format=%q, want true / json", cfg.RequestLog, cfg.RequestLogFormat)
	}
	remigrated, _, errMigrate := NormalizeConfigLayout(source, true)
	if errMigrate != nil {
		t.Fatalf("NormalizeConfigLayout() error = %v", errMigrate)
	}
	if !strings.Contains(string(remigrated), "request-log-format: json") {
		t.Fatalf("remigrated config lost request-log-format:\n%s", remigrated)
	}
	var root map[string]any
	if errDecode := yaml.Unmarshal(source, &root); errDecode != nil {
		t.Fatalf("yaml.Unmarshal(root) error = %v", errDecode)
	}
	projected, errNormalize := NormalizeConfigRoot(root)
	if errNormalize != nil {
		t.Fatalf("NormalizeConfigRoot() error = %v", errNormalize)
	}
	wire, errMarshal := yaml.Marshal(projected)
	if errMarshal != nil {
		t.Fatalf("yaml.Marshal(projected) error = %v", errMarshal)
	}
	if _, errParse := cpaconfig.ParseConfigBytes(wire); errParse != nil {
		t.Fatalf("bundled CPA ParseConfigBytes() error = %v", errParse)
	}
	var downstream map[string]any
	if errDecode := yaml.Unmarshal(wire, &downstream); errDecode != nil {
		t.Fatalf("yaml.Unmarshal(downstream) error = %v", errDecode)
	}
	if downstream["request-log-format"] != "json" {
		t.Fatalf("downstream wire request-log-format = %#v, want json", downstream["request-log-format"])
	}
}

func TestValidateV8ConfigRejectsInvalidAndLegacyRequestLogFormat(t *testing.T) {
	for _, source := range []string{
		"request-log-format: json\n",
		"observability:\n  logs:\n    request-log-format: ndjson\n",
	} {
		if errValidate := ValidateV8Config([]byte(source)); errValidate == nil {
			t.Fatalf("ValidateV8Config(%q) succeeded, want error", source)
		}
	}
}

func TestRequestLogFormatCanonicalPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "canonical json wins", source: "request-log-format: text\nobservability:\n  logs:\n    request-log-format: json\n", want: "json"},
		{name: "canonical text wins", source: "request-log-format: json\nobservability:\n  logs:\n    request-log-format: text\n", want: "text"},
		{name: "legacy flat maps", source: "request-log-format: JSON\n", want: "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if errDecode := yaml.Unmarshal([]byte(tt.source), &cfg); errDecode != nil {
				t.Fatalf("yaml.Unmarshal() error = %v", errDecode)
			}
			if cfg.RequestLogFormat != tt.want {
				t.Fatalf("request-log-format = %q, want %q", cfg.RequestLogFormat, tt.want)
			}
		})
	}
}
