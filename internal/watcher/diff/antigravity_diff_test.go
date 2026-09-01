package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigDiff_AntigravitySensitiveWords(t *testing.T) {
	oldCfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			SensitiveWords: []string{"API"},
		},
	}
	newCfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			SensitiveWords: []string{"API", "proxy"},
		},
	}

	diff := BuildConfigChangeDetails(oldCfg, newCfg)
	joined := strings.Join(diff, "; ")
	if !strings.Contains(joined, "antigravity.sensitive-words: 1 -> 2") {
		t.Fatalf("BuildConfigChangeDetails() = %q, want antigravity.sensitive-words change", joined)
	}
}
