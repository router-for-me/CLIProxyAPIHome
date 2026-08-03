package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestBuildConfigChangeDetailsIncludesCodexAlphaSearch(t *testing.T) {
	oldCfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "key"}}}
	newCfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "key", AlphaSearch: true}}}
	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	want := "codex[0].alpha-search: false -> true"
	for _, change := range changes {
		if change == want {
			return
		}
	}
	t.Fatalf("changes = %#v, want %q", changes, want)
}
