package auth

import (
	"time"
)

var (
	codexRefreshLead       = 5 * 24 * time.Hour
	claudeRefreshLead      = 4 * time.Hour
	antigravityRefreshLead = 30 * time.Minute
	kimiRefreshLead        = 5 * time.Minute
	xaiRefreshLead         = 5 * time.Minute
	copilotRefreshLead     = 5 * time.Minute
)

// init prepares package-level state.
func init() {
	registerRefreshLead("codex", &codexRefreshLead)
	registerRefreshLead("claude", &claudeRefreshLead)
	registerRefreshLead("gemini", nil)
	registerRefreshLead("antigravity", &antigravityRefreshLead)
	registerRefreshLead("kimi", &kimiRefreshLead)
	registerRefreshLead("kimi-ai", &kimiRefreshLead)
	registerRefreshLead("xai", &xaiRefreshLead)
	registerRefreshLead("copilot", &copilotRefreshLead)
	registerRefreshLead("meta", nil)
}

// registerRefreshLead handles a register refresh lead.
func registerRefreshLead(provider string, lead *time.Duration) {
	RegisterRefreshLeadProvider(provider, func() *time.Duration {
		if lead == nil || *lead <= 0 {
			return nil
		}
		return lead
	})
}
