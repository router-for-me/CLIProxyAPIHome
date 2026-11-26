package home

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// ConfigPath handles a config path.
func (r *Runtime) ConfigPath() string {
	if r == nil {
		return ""
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return strings.TrimSpace(r.configPath)
}

// ReadConfigYAMLContext loads read config yaml context.
func (r *Runtime) ReadConfigYAMLContext(ctx context.Context) ([]byte, error) {
	// Normalize source data before building the derived payload.
	if r == nil {
		return nil, fmt.Errorf("home runtime: runtime is nil")
	}
	if r.clusterAdapter != nil && r.clusterAdapter.Enabled() {
		data, errRead := r.clusterAdapter.LoadConfigYAML(ctx)
		if errRead != nil {
			return nil, errRead
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("home runtime: config is empty")
		}
		filtered, errFilter := sanitizeConfigYAMLForDownstream(data, r.pluginHost != nil && r.pluginHost.HasSchedulerContinuity())
		if errFilter != nil {
			return nil, errFilter
		}
		return filtered, nil
	}
	path := r.ConfigPath()
	if path == "" {
		return nil, fmt.Errorf("home runtime: config path is empty")
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("home runtime: config is empty")
	}
	filtered, errFilter := sanitizeConfigYAMLForDownstream(data, r.pluginHost != nil && r.pluginHost.HasSchedulerContinuity())
	if errFilter != nil {
		return nil, errFilter
	}
	return filtered, nil
}

// SubscribeConfigYAML handles a subscribe config yaml.
func (r *Runtime) SubscribeConfigYAML(subscriber func(payload []byte) error) (unsubscribe func()) {
	// Normalize source data before building the derived payload.
	if r == nil || subscriber == nil {
		return func() {}
	}

	r.configSubsMu.Lock()
	if r.configSubs == nil {
		r.configSubs = make(map[uint64]func(payload []byte) error)
	}
	r.nextConfigSubID++
	id := r.nextConfigSubID
	r.configSubs[id] = subscriber
	r.configSubsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.configSubsMu.Lock()
			delete(r.configSubs, id)
			r.configSubsMu.Unlock()
		})
	}
}

// PublishConfigYAML handles a publish config yaml.
func (r *Runtime) PublishConfigYAML(payload []byte) {
	// Normalize source data before building the derived payload.
	if r == nil || len(payload) == 0 {
		return
	}

	filtered, errFilter := sanitizeConfigYAMLForDownstream(payload, r.pluginHost != nil && r.pluginHost.HasSchedulerContinuity())
	if errFilter != nil || len(filtered) == 0 {
		return
	}

	r.configSubsMu.Lock()
	snapshot := make(map[uint64]func(payload []byte) error, len(r.configSubs))
	for id, sub := range r.configSubs {
		snapshot[id] = sub
	}
	r.configSubsMu.Unlock()

	for id, sub := range snapshot {
		if sub == nil {
			continue
		}
		if errSend := sub(filtered); errSend != nil {
			r.configSubsMu.Lock()
			delete(r.configSubs, id)
			r.configSubsMu.Unlock()
		}
	}
}

var downstreamExcludedConfigKeys = []string{
	"remote-management",
	"user-email",
	"api-keys",
	"auth-dir",
	"allow-host",
	"tls",
	"gemini-api-key",
	"interactions-api-key",
	"codex-api-key",
	"xai-api-key",
	"meta-api-key",
	"claude-api-key",
	"openai-compatibility",
	"vertex-api-key",
	"oauth-model-alias",
}

// sanitizeConfigYAMLForDownstream sanitizes a config yaml for downstream.
func sanitizeConfigYAMLForDownstream(payload []byte, continuity ...bool) ([]byte, error) {
	// Normalize source data before building the derived payload.
	if len(payload) == 0 {
		return nil, fmt.Errorf("home runtime: config is empty")
	}

	var source map[string]any
	if errDecode := yaml.Unmarshal(payload, &source); errDecode != nil {
		return nil, fmt.Errorf("home runtime: unmarshal config: %w", errDecode)
	}
	normalized, errNormalize := appconfig.NormalizeConfigRoot(source)
	if errNormalize != nil {
		return nil, fmt.Errorf("home runtime: normalize config: %w", errNormalize)
	}
	// Older CPA executors default an omitted or blank TOS pause window to 6h.
	// Project Home's disabled default explicitly without changing stored config.
	antigravity, ok := normalized["antigravity"].(map[string]any)
	if !ok {
		if normalized["antigravity"] != nil {
			return nil, fmt.Errorf("home runtime: antigravity must be a mapping")
		}
		antigravity = make(map[string]any)
		normalized["antigravity"] = antigravity
	}
	window := antigravity["tos-pause-window"]
	if raw, isString := window.(string); window == nil || (isString && strings.TrimSpace(raw) == "") {
		antigravity["tos-pause-window"] = "0"
		// NormalizeConfigRoot has already validated these optional containers.
		if oauth, okOAuth := normalized["oauth"].(map[string]any); okOAuth {
			if providers, okProviders := oauth["providers"].(map[string]any); okProviders {
				if scoped, okScoped := providers["antigravity"].(map[string]any); okScoped {
					scoped["tos-pause-window"] = "0"
				}
			}
		}
	}
	for _, key := range downstreamExcludedConfigKeys {
		delete(normalized, key)
	}
	// CPA needs Antigravity exclusions for variants selected after Home dispatch.
	// Keep the other providers' rules Home-only and preserve each pattern intact.
	const exclusionsKey = "oauth-excluded-models"
	exclusions, _ := normalized[exclusionsKey].(map[string]any)
	for provider := range exclusions {
		if !strings.EqualFold(strings.TrimSpace(provider), "antigravity") {
			delete(exclusions, provider)
		}
	}
	if len(exclusions) == 0 {
		delete(normalized, exclusionsKey)
	}
	// Advertise the optional read-only scheduler RPC separately from user config.
	normalized["home-scheduler-protocol"] = 0
	if len(continuity) > 0 && continuity[0] {
		normalized["home-scheduler-protocol"] = 1
	}
	appconfig.ApplyDownstreamHomeModeScalars(normalized)
	forced, errMarshal := yaml.Marshal(normalized)
	if errMarshal != nil {
		return nil, fmt.Errorf("home runtime: marshal downstream config root: %w", errMarshal)
	}
	forced = bytes.TrimSpace(forced)
	if len(forced) == 0 {
		return nil, fmt.Errorf("home runtime: config is empty")
	}
	forced = append(forced, '\n')
	return forced, nil
}
