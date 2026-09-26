package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestCodexModelsHashChangesWithConfigurationUpdateCapability(t *testing.T) {
	models := []config.CodexModel{{Name: "opaque", Alias: "public"}}
	before := ComputeCodexModelsHash(models)
	models[0].SupportConfigurationUpdate = true
	if after := ComputeCodexModelsHash(models); after == before {
		t.Fatal("Codex capability change did not change the credential models hash")
	}
}
