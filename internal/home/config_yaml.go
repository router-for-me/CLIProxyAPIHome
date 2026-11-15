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
		filtered, errFilter := sanitizeConfigYAMLForDownstream(data)
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
	filtered, errFilter := sanitizeConfigYAMLForDownstream(data)
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

	filtered, errFilter := sanitizeConfigYAMLForDownstream(payload)
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

// sanitizeConfigYAMLForDownstream sanitizes a config yaml for downstream.
func sanitizeConfigYAMLForDownstream(payload []byte) ([]byte, error) {
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
	var errMarshal error
	payload, errMarshal = yaml.Marshal(normalized)
	if errMarshal != nil {
		return nil, fmt.Errorf("home runtime: marshal config: %w", errMarshal)
	}

	var yamlRoot yaml.Node
	if errUnmarshal := yaml.Unmarshal(payload, &yamlRoot); errUnmarshal != nil {
		return nil, fmt.Errorf("home runtime: unmarshal config: %w", errUnmarshal)
	}
	if yamlRoot.Kind != yaml.DocumentNode || len(yamlRoot.Content) == 0 || yamlRoot.Content[0] == nil {
		return nil, fmt.Errorf("home runtime: invalid config yaml document")
	}

	doc := yamlRoot.Content[0]
	removeConfigKeysForDownstream(doc, []string{
		"remote-management",
		"user-email",
		"api-keys",
		"auth-dir",
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
		"oauth-excluded-models",
	})
	stripYAMLComments(&yamlRoot)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if errEncode := enc.Encode(&yamlRoot); errEncode != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("home runtime: marshal config: %w", errEncode)
	}
	if errClose := enc.Close(); errClose != nil {
		return nil, fmt.Errorf("home runtime: marshal config: %w", errClose)
	}
	out := bytes.TrimSpace(buf.Bytes())
	if len(out) == 0 {
		return nil, fmt.Errorf("home runtime: config is empty")
	}
	out = append(out, '\n')

	var root map[string]any
	if errUnmarshalRoot := yaml.Unmarshal(out, &root); errUnmarshalRoot != nil {
		return nil, fmt.Errorf("home runtime: unmarshal downstream config root: %w", errUnmarshalRoot)
	}
	if root == nil {
		root = make(map[string]any)
	}
	appconfig.ApplyDownstreamHomeModeScalars(root)
	forced, errMarshalRoot := yaml.Marshal(root)
	if errMarshalRoot != nil {
		return nil, fmt.Errorf("home runtime: marshal downstream config root: %w", errMarshalRoot)
	}
	forced = bytes.TrimSpace(forced)
	if len(forced) == 0 {
		return nil, fmt.Errorf("home runtime: config is empty")
	}
	forced = append(forced, '\n')
	return forced, nil
}

// removeConfigKeysForDownstream removes a config keys for downstream.
func removeConfigKeysForDownstream(node *yaml.Node, keys []string) {
	// Normalize source data before building the derived payload.
	if node == nil || node.Kind != yaml.MappingNode || len(keys) == 0 {
		return
	}

	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		k := strings.TrimSpace(key)
		if k == "" {
			continue
		}
		keySet[k] = struct{}{}
	}
	if len(keySet) == 0 {
		return
	}

	next := make([]*yaml.Node, 0, len(node.Content))
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		v := node.Content[i+1]
		if k == nil || strings.TrimSpace(k.Value) == "" {
			next = append(next, k, v)
			continue
		}
		if _, ok := keySet[k.Value]; ok {
			continue
		}
		next = append(next, k, v)
	}
	node.Content = next
}

// stripYAMLComments handles a strip yaml comments.
func stripYAMLComments(node *yaml.Node) {
	if node == nil {
		return
	}
	node.HeadComment = ""
	node.LineComment = ""
	node.FootComment = ""

	for _, child := range node.Content {
		stripYAMLComments(child)
	}
	if node.Alias != nil {
		stripYAMLComments(node.Alias)
	}
}
