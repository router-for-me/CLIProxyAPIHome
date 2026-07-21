package config

import "testing"

func TestNormalizeAndValidateTrustedProxies(t *testing.T) {
	cfg := &Config{TrustedProxies: []string{" 127.0.0.1 ", "10.0.0.0/8", "::ffff:192.0.2.0/120", "127.0.0.1", ""}}
	cfg.NormalizeTrustedProxies()
	if len(cfg.TrustedProxies) != 3 || cfg.TrustedProxies[0] != "127.0.0.1" || cfg.TrustedProxies[1] != "10.0.0.0/8" || cfg.TrustedProxies[2] != "::ffff:192.0.2.0/120" {
		t.Fatalf("normalized trusted proxies = %#v", cfg.TrustedProxies)
	}
	if errValidate := ValidateTrustedProxies(cfg.TrustedProxies); errValidate != nil {
		t.Fatalf("ValidateTrustedProxies() error = %v", errValidate)
	}
}

func TestValidateTrustedProxiesRejectsInvalidAndTrustAllEntries(t *testing.T) {
	for _, value := range []string{"proxy.example.com", "10.0.0.0/99", "0.0.0.0/0", "::/0", "::ffff:0:0/96"} {
		t.Run(value, func(t *testing.T) {
			if errValidate := ValidateTrustedProxies([]string{value}); errValidate == nil {
				t.Fatalf("ValidateTrustedProxies(%q) succeeded", value)
			}
		})
	}
}
