package concurrency

import (
	"strings"
	"unicode/utf8"
)

const (
	asciiWhitespace = " \t\r\n\v\f"

	// MaxCanonicalModelKeyBytes bounds canonical model keys stored by the limiter.
	MaxCanonicalModelKeyBytes = 256
)

// CanonicalConcurrencyModelKey removes recognized reasoning suffixes from a model key.
func CanonicalConcurrencyModelKey(model string) string {
	if !utf8.ValidString(model) {
		return ""
	}
	trimmed := strings.ToLower(strings.Trim(model, asciiWhitespace))
	if !strings.HasSuffix(trimmed, ")") {
		return trimmed
	}
	open := strings.LastIndexByte(trimmed, '(')
	if open < 0 {
		return trimmed
	}
	suffix := trimmed[open+1 : len(trimmed)-1]
	if !recognizedConcurrencySuffix(suffix) {
		return trimmed
	}
	base := strings.Trim(trimmed[:open], asciiWhitespace)
	if base == "" {
		return trimmed
	}
	return base
}

// ValidCanonicalConcurrencyModelKey canonicalizes a model key and verifies its UTF-8 byte size.
func ValidCanonicalConcurrencyModelKey(model string) (string, bool) {
	key := CanonicalConcurrencyModelKey(model)
	return key, key != "" && utf8.ValidString(key) && len(key) <= MaxCanonicalModelKeyBytes
}

func recognizedConcurrencySuffix(value string) bool {
	if value == "-1" {
		return true
	}
	switch strings.ToLower(value) {
	case "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	if value == "" || len(value) > 10 {
		return false
	}
	var parsed int64
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
		parsed = parsed*10 + int64(value[index]-'0')
		if parsed > 2_147_483_647 {
			return false
		}
	}
	return true
}
