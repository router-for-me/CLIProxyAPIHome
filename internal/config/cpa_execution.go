package config

import (
	"fmt"
	"strings"
	"time"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// OAuthModelSetting remains typed because Home publishes the authoritative model catalog.
type OAuthModelSetting = cpaconfig.OAuthModelSetting

// ValidateHomePolicies validates only policies that Home actually executes.
func (cfg *Config) ValidateHomePolicies() error {
	mode := strings.ToLower(strings.TrimSpace(cfg.Routing.SessionAffinityRebindOn))
	if mode != "" && mode != "any" && mode != "quota" && mode != "never" {
		return fmt.Errorf("routing.session-affinity-rebind-on must be any, quota, or never")
	}
	cfg.Routing.SessionAffinityRebindOn = mode
	window := strings.TrimSpace(cfg.Antigravity.TOSPauseWindow)
	switch strings.ToLower(window) {
	case "", "0", "off", "false", "none", "disabled":
	default:
		if duration, errParse := time.ParseDuration(window); errParse != nil || duration <= 0 {
			return fmt.Errorf("oauth.providers.antigravity.tos-pause-window must be a positive duration or a disabling value")
		}
	}
	settings := cpaconfig.Config{OAuthSettings: cfg.OAuthSettings}
	settings.SanitizeOAuthSettings()
	cfg.OAuthSettings = settings.OAuthSettings
	return nil
}
