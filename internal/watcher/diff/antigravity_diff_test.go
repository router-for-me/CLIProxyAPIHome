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

func TestConfigDiff_SensitiveWordsReplace(t *testing.T) {
	oldCfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			SensitiveWordsReplace: map[string]string{"Codex": ""},
		},
		Devin: config.DevinConfig{
			SensitiveWords:        []string{"a"},
			SensitiveWordsReplace: map[string]string{"Codex": ""},
		},
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "sk-ant-1",
			Cloak:  &config.CloakConfig{SensitiveWordsReplace: map[string]string{"Codex": ""}},
		}},
	}
	newCfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			SensitiveWordsReplace: map[string]string{"GPT-5": ""},
		},
		Devin: config.DevinConfig{
			SensitiveWords:        []string{"a", "b"},
			SensitiveWordsReplace: map[string]string{"GPT-5": ""},
		},
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "sk-ant-1",
			Cloak:  &config.CloakConfig{SensitiveWordsReplace: map[string]string{"GPT-5": ""}},
		}},
	}
	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	joined := strings.Join(changes, "; ")
	for _, want := range []string{
		"antigravity.sensitive-words-replace: 1 -> 1",
		"devin.sensitive-words: 1 -> 2",
		"devin.sensitive-words-replace: 1 -> 1",
		"claude[0].cloak.sensitive-words-replace: 1 -> 1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("BuildConfigChangeDetails() = %q, missing %q", joined, want)
		}
	}

	emptyA := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "sk-ant-1", Cloak: &config.CloakConfig{}}}}
	emptyB := &config.Config{
		Antigravity: config.AntigravityConfig{SensitiveWordsReplace: map[string]string{}},
		Devin:       config.DevinConfig{SensitiveWordsReplace: map[string]string{}},
		ClaudeKey:   []config.ClaudeKey{{APIKey: "sk-ant-1", Cloak: &config.CloakConfig{SensitiveWordsReplace: map[string]string{}}}},
	}
	if noDiff := BuildConfigChangeDetails(emptyA, emptyB); len(noDiff) != 0 {
		t.Fatalf("expected no diff between nil and empty SensitiveWordsReplace maps, got %v", noDiff)
	}

	nilCloak := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "sk-ant-1"}}}
	withCloakA := &config.Config{ClaudeKey: []config.ClaudeKey{{
		APIKey: "sk-ant-1",
		Cloak:  &config.CloakConfig{SensitiveWords: []string{"API"}},
	}}}
	withCloakB := &config.Config{ClaudeKey: []config.ClaudeKey{{
		APIKey: "sk-ant-1",
		Cloak:  &config.CloakConfig{SensitiveWords: []string{"proxy"}},
	}}}
	if gotAdd := strings.Join(BuildConfigChangeDetails(nilCloak, withCloakA), "; "); !strings.Contains(gotAdd, "claude[0].cloak: added") {
		t.Fatalf("expected claude[0].cloak: added, got %q", gotAdd)
	}
	if gotRem := strings.Join(BuildConfigChangeDetails(withCloakA, nilCloak), "; "); !strings.Contains(gotRem, "claude[0].cloak: removed") {
		t.Fatalf("expected claude[0].cloak: removed, got %q", gotRem)
	}
	if gotWords := strings.Join(BuildConfigChangeDetails(withCloakA, withCloakB), "; "); !strings.Contains(gotWords, "claude[0].cloak.sensitive-words: 1 -> 1") {
		t.Fatalf("expected claude[0].cloak.sensitive-words: 1 -> 1, got %q", gotWords)
	}
}
