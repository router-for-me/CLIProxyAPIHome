package config

import (
	"strings"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

// TestValidateV8ConfigAcceptsRequestsTLSHandshakeTimeout verifies that
// Home's strict v8 management schema accepts the CPA opt-in TLS handshake
// bound under requests and decodes it as integer seconds.
func TestValidateV8ConfigAcceptsRequestsTLSHandshakeTimeout(t *testing.T) {
	source := []byte("requests:\n  tls-handshake-timeout-seconds: 12\n")
	if errValidate := ValidateV8Config(source); errValidate != nil {
		t.Fatalf("ValidateV8Config() error = %v", errValidate)
	}
	var cfg Config
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errDecode)
	}
	normalizedCfg, errNormalizeCfg := NormalizeConfigRoot(cfg.RawConfig)
	if errNormalizeCfg != nil {
		t.Fatalf("NormalizeConfigRoot(cfg) error = %v", errNormalizeCfg)
	}
	if normalizedCfg["tls-handshake-timeout-seconds"] != 12 {
		t.Fatalf("tls-handshake-timeout-seconds = %#v, want 12", normalizedCfg["tls-handshake-timeout-seconds"])
	}
	remigrated, _, errMigrate := NormalizeConfigLayout(source, true)
	if errMigrate != nil {
		t.Fatalf("NormalizeConfigLayout() error = %v", errMigrate)
	}
	if !strings.Contains(string(remigrated), "tls-handshake-timeout-seconds: 12") {
		t.Fatalf("remigrated config lost the TLS handshake timeout:\n%s", remigrated)
	}
	var restored Config
	if errDecode := yaml.Unmarshal(remigrated, &restored); errDecode != nil {
		t.Fatalf("yaml.Unmarshal(remigrated) error = %v", errDecode)
	}
	normalizedRestored, errNormalizeRestored := NormalizeConfigRoot(restored.RawConfig)
	if errNormalizeRestored != nil {
		t.Fatalf("NormalizeConfigRoot(restored) error = %v", errNormalizeRestored)
	}
	if normalizedRestored["tls-handshake-timeout-seconds"] != 12 {
		t.Fatalf("remigrated tls-handshake-timeout-seconds = %#v, want 12", normalizedRestored["tls-handshake-timeout-seconds"])
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
	// The bundled CPA release predates this field, so inspect the emitted
	// legacy projection directly; current CPA parses this flat spelling
	// into requests.tls-handshake-timeout-seconds.
	var downstream map[string]any
	if errDecode := yaml.Unmarshal(wire, &downstream); errDecode != nil {
		t.Fatalf("yaml.Unmarshal(downstream wire) error = %v", errDecode)
	}
	if downstream["tls-handshake-timeout-seconds"] != 12 {
		t.Fatalf("downstream wire tls-handshake-timeout-seconds = %#v, want 12", downstream["tls-handshake-timeout-seconds"])
	}
}

// TestRequestsTLSHandshakeTimeoutCanonicalPrecedence ensures the canonical
// requests value, including explicit zero, wins over the pre-v8 flat
// spelling in a partially migrated Home config.
func TestRequestsTLSHandshakeTimeoutCanonicalPrecedence(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   int
	}{
		{name: "canonical positive wins", source: "tls-handshake-timeout-seconds: 7\nrequests:\n  tls-handshake-timeout-seconds: 12\n", want: 12},
		{name: "canonical explicit zero wins", source: "tls-handshake-timeout-seconds: 7\nrequests:\n  tls-handshake-timeout-seconds: 0\n", want: 0},
		{name: "flat legacy maps to requests", source: "tls-handshake-timeout-seconds: 7\n", want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if errDecode := yaml.Unmarshal([]byte(tt.source), &cfg); errDecode != nil {
				t.Fatalf("yaml.Unmarshal() error = %v", errDecode)
			}
			normalized, errNormalize := NormalizeConfigRoot(cfg.RawConfig)
			if errNormalize != nil {
				t.Fatalf("NormalizeConfigRoot() error = %v", errNormalize)
			}
			if normalized["tls-handshake-timeout-seconds"] != tt.want {
				t.Fatalf("tls-handshake-timeout-seconds = %#v, want %d", normalized["tls-handshake-timeout-seconds"], tt.want)
			}
		})
	}
}
