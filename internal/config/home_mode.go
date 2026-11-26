package config

// ForceHomeRuntimeConfig applies invariants owned by the Home runtime.
func ForceHomeRuntimeConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.UsageStatisticsEnabled = true
}

// ApplyHomeRuntimeScalars applies scalar invariants owned by Home.
func ApplyHomeRuntimeScalars(root map[string]any) {
	if root == nil {
		return
	}
	root["usage-statistics-enabled"] = true
	if _, exists := root["disable-cooling"]; !exists {
		root["disable-cooling"] = false
	}
	root["ws-auth"] = false
	if oauth, changed, errScope := UpdateOAuthScope(root["oauth"], "ws-auth", false); errScope == nil && changed {
		root["oauth"] = oauth
	}
}

// ApplyDownstreamHomeModeScalars only applies scalar Home-mode overrides.
// It does not remove sensitive or Home-local roots such as api-keys,
// remote-management, auth-dir, tls, or credential roots. Callers that build
// downstream CPA YAML must filter those roots separately first.
func ApplyDownstreamHomeModeScalars(root map[string]any) {
	if root == nil {
		return
	}
	root["usage-statistics-enabled"] = true
	root["disable-cooling"] = true
	// CPA does not expose /v1/ws in Home mode, so its auth switch is not distributed.
	delete(root, "ws-auth")
	if oauth, changed, errScope := UpdateOAuthScope(root["oauth"], "ws-auth", nil); errScope == nil && changed {
		root["oauth"] = oauth
	}
}
