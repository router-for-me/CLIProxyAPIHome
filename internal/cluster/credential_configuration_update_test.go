package cluster

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestExportCodexCredentialKeepsConfigurationUpdateSupport(t *testing.T) {
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"home_config_models": []any{map[string]any{
		"id": "public", "name": "private", "user_defined": true, "support_configuration_update": true,
	}}}}
	models := credentialCodexModels(auth)
	if len(models) != 1 || !models[0].SupportConfigurationUpdate {
		t.Fatalf("exported codex models = %+v, want enabled", models)
	}
}
