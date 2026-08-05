package config

// ForceHomeRuntimeConfig applies invariants owned by the Home runtime.
func ForceHomeRuntimeConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.APIKeys = nil
	cfg.UsageStatisticsEnabled = true
	cfg.DisableCooling = false
	cfg.WebsocketAuth = false
}

// ApplyHomeRuntimeScalars applies scalar invariants owned by Home.
func ApplyHomeRuntimeScalars(root map[string]any) {
	if len(root) == 0 {
		return
	}
	root["usage-statistics-enabled"] = true
	root["disable-cooling"] = false
	root["ws-auth"] = false
}

// ApplyDownstreamHomeModeScalars only applies scalar Home-mode overrides.
// It does not remove sensitive or Home-local roots such as api-keys,
// remote-management, auth-dir, tls, or credential roots. Callers that build
// downstream CPA YAML must filter those roots separately first.
func ApplyDownstreamHomeModeScalars(root map[string]any) {
	ApplyHomeRuntimeScalars(root)
	if len(root) == 0 {
		return
	}
	root["disable-cooling"] = true
}
