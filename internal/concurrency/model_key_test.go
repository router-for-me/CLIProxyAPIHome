package concurrency

import (
	"strings"
	"testing"
)

func TestCanonicalConcurrencyModelKey(t *testing.T) {
	cases := map[string]string{
		" gpt(high) ":       "gpt",
		"gpt(8192)":         "gpt",
		"gpt(0001)":         "gpt",
		"gpt(-1)":           "gpt",
		"gpt(AUTO)":         "gpt",
		" GPT(HiGh) ":       "gpt",
		"model(custom)":     "model(custom)",
		" Model(Custom) ":   "model(custom)",
		"model(+1)":         "model(+1)",
		"model( 1)":         "model( 1)",
		"model(2147483648)": "model(2147483648)",
		"(high)":            "(high)",
		"":                  "",
		" \t\r\n ":          "",
	}
	for input, want := range cases {
		if got := CanonicalConcurrencyModelKey(input); got != want {
			t.Fatalf("CanonicalConcurrencyModelKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCanonicalConcurrencyModelKeyRejectsMalformedUTF8BeforeCaseNormalization(t *testing.T) {
	malformed := "GPT\xff(HIGH)"
	if got := CanonicalConcurrencyModelKey(malformed); got != "" {
		t.Fatalf("CanonicalConcurrencyModelKey(%q) = %q, want empty", malformed, got)
	}
	if key, valid := ValidCanonicalConcurrencyModelKey(malformed); valid || key != "" {
		t.Fatalf("ValidCanonicalConcurrencyModelKey(%q) = (%q, %t), want empty invalid key", malformed, key, valid)
	}
	if key, valid := ValidCanonicalConcurrencyModelKey("gpt�"); !valid || key != "gpt�" {
		t.Fatalf("ValidCanonicalConcurrencyModelKey legal replacement rune = (%q, %t)", key, valid)
	}
}

func TestValidCanonicalConcurrencyModelKeyEnforcesUTF8ByteLimit(t *testing.T) {
	cases := []struct {
		name  string
		model string
		valid bool
	}{
		{name: "ascii boundary", model: strings.Repeat("a", MaxCanonicalModelKeyBytes), valid: true},
		{name: "ascii over", model: strings.Repeat("a", MaxCanonicalModelKeyBytes+1)},
		{name: "multibyte boundary", model: strings.Repeat("界", 85) + "a", valid: true},
		{name: "multibyte over", model: strings.Repeat("界", 86)},
		{name: "suffix is removed before size validation", model: strings.Repeat("a", MaxCanonicalModelKeyBytes) + "(high)", valid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			key, valid := ValidCanonicalConcurrencyModelKey(testCase.model)
			if valid != testCase.valid {
				t.Fatalf("ValidCanonicalConcurrencyModelKey(%q) valid = %t, want %t (key bytes=%d)", testCase.model, valid, testCase.valid, len(key))
			}
		})
	}
}
