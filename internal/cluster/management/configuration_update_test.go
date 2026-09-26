package management

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestManagementCodexCredentialKeepsConfigurationUpdateSupport(t *testing.T) {
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"home_config_models": []any{map[string]any{
		"id": "public", "name": "private", "user_defined": true, "support_configuration_update": true,
	}}}}
	models := credentialAPIKeyModels(auth)
	if len(models) != 1 || models[0]["support-configuration-update"] != true {
		t.Fatalf("management codex models = %+v, want enabled", models)
	}
}
