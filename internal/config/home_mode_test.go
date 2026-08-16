package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForceHomeRuntimeConfigPreservesDisableCooling(t *testing.T) {
	cfg := &Config{}
	cfg.APIKeys = []string{"local-key"}
	cfg.UsageStatisticsEnabled = false
	cfg.DisableCooling = true
	cfg.WebsocketAuth = true
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.DisableControlPanel = false

	ForceHomeRuntimeConfig(cfg)

	if len(cfg.APIKeys) != 0 {
		t.Fatalf("APIKeys = %#v, want nil/empty", cfg.APIKeys)
	}
	if !cfg.UsageStatisticsEnabled {
		t.Fatal("UsageStatisticsEnabled = false, want true")
	}
	if !cfg.DisableCooling {
		t.Fatal("DisableCooling = false, want Home global cooling disabled")
	}
	if cfg.WebsocketAuth {
		t.Fatal("WebsocketAuth = true, want false")
	}
	if !cfg.RemoteManagement.AllowRemote {
		t.Fatal("RemoteManagement.AllowRemote = false, want preserved true")
	}
	if cfg.RemoteManagement.DisableControlPanel {
		t.Fatal("RemoteManagement.DisableControlPanel = true, want preserved false")
	}
}

func TestLoadConfigOptionalPreservesDisableCooling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(`api-keys:
  - local-key
usage-statistics-enabled: false
disable-cooling: true
ws-auth: true
`), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	cfg, errLoad := LoadConfigOptional(path, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if len(cfg.APIKeys) != 0 {
		t.Fatalf("APIKeys = %#v, want nil/empty", cfg.APIKeys)
	}
	if !cfg.UsageStatisticsEnabled {
		t.Fatal("UsageStatisticsEnabled = false, want true")
	}
	if !cfg.DisableCooling {
		t.Fatal("DisableCooling = false, want Home global cooling disabled")
	}
	if cfg.WebsocketAuth {
		t.Fatal("WebsocketAuth = true, want false")
	}
}

func TestApplyHomeRuntimeScalarsPreservesDisableCooling(t *testing.T) {
	root := map[string]any{
		"usage-statistics-enabled": false,
		"disable-cooling":          true,
		"ws-auth":                  true,
	}

	ApplyHomeRuntimeScalars(root)

	if root["usage-statistics-enabled"] != true {
		t.Fatalf("usage-statistics-enabled = %v, want true", root["usage-statistics-enabled"])
	}
	if root["disable-cooling"] != true {
		t.Fatalf("disable-cooling = %v, want Home global setting preserved", root["disable-cooling"])
	}
	if root["ws-auth"] != false {
		t.Fatalf("ws-auth = %v, want false", root["ws-auth"])
	}

	rootWithoutSetting := map[string]any{"usage-statistics-enabled": false}
	ApplyHomeRuntimeScalars(rootWithoutSetting)
	if rootWithoutSetting["disable-cooling"] != false {
		t.Fatalf("default disable-cooling = %v, want false", rootWithoutSetting["disable-cooling"])
	}

	emptyRoot := map[string]any{}
	ApplyHomeRuntimeScalars(emptyRoot)
	if emptyRoot["usage-statistics-enabled"] != true || emptyRoot["disable-cooling"] != false || emptyRoot["ws-auth"] != false {
		t.Fatalf("empty Home root invariants = %#v, want usage=true disable-cooling=false ws-auth=false", emptyRoot)
	}
}

func TestApplyDownstreamHomeModeScalarsPreservesRemoteManagement(t *testing.T) {
	root := map[string]any{
		"api-keys":                 []any{"local-key"},
		"usage-statistics-enabled": false,
		"disable-cooling":          false,
		"remote-management": map[string]any{
			"allow-remote":          true,
			"disable-control-panel": false,
		},
	}

	ApplyDownstreamHomeModeScalars(root)

	keys, ok := root["api-keys"].([]any)
	if !ok || len(keys) != 1 || keys[0] != "local-key" {
		t.Fatalf("api-keys = %v, want preserved local-key", root["api-keys"])
	}
	if root["usage-statistics-enabled"] != true {
		t.Fatalf("usage-statistics-enabled = %v, want true", root["usage-statistics-enabled"])
	}
	if root["disable-cooling"] != true {
		t.Fatalf("disable-cooling = %v, want true", root["disable-cooling"])
	}
	if root["ws-auth"] != false {
		t.Fatalf("ws-auth = %v, want false", root["ws-auth"])
	}
	remoteManagement, ok := root["remote-management"].(map[string]any)
	if !ok {
		t.Fatalf("remote-management = %#v, want map", root["remote-management"])
	}
	if remoteManagement["allow-remote"] != true {
		t.Fatalf("allow-remote = %v, want preserved true", remoteManagement["allow-remote"])
	}
	if remoteManagement["disable-control-panel"] != false {
		t.Fatalf("disable-control-panel = %v, want preserved false", remoteManagement["disable-control-panel"])
	}
}

func TestApplyDownstreamHomeModeScalarsDoesNotInjectRemoteManagement(t *testing.T) {
	root := map[string]any{
		"usage-statistics-enabled": false,
	}
	ApplyDownstreamHomeModeScalars(root)
	if root["usage-statistics-enabled"] != true {
		t.Fatalf("usage-statistics-enabled = %v, want true", root["usage-statistics-enabled"])
	}
	if _, ok := root["remote-management"]; ok {
		t.Fatal("remote-management should not be injected into downstream yaml root")
	}
}

func TestApplyDownstreamHomeModeScalarsAlwaysDisablesCPACooling(t *testing.T) {
	for _, homeDisableCooling := range []bool{false, true} {
		root := map[string]any{"disable-cooling": homeDisableCooling}

		ApplyDownstreamHomeModeScalars(root)

		if root["disable-cooling"] != true {
			t.Fatalf("Home disable-cooling=%t produced downstream value %v, want true", homeDisableCooling, root["disable-cooling"])
		}
	}

	emptyRoot := map[string]any{}
	ApplyDownstreamHomeModeScalars(emptyRoot)
	if emptyRoot["usage-statistics-enabled"] != true || emptyRoot["disable-cooling"] != true || emptyRoot["ws-auth"] != false {
		t.Fatalf("empty downstream root invariants = %#v, want usage=true disable-cooling=true ws-auth=false", emptyRoot)
	}
}
