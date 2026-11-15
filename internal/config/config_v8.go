package config

import (
	"fmt"
	"reflect"
	"strings"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Config fields are runtime read projections. Complete documents remain in
// RawConfig; layout translation stays at YAML boundaries for compatibility.
type legacyConfig Config

type configPath struct{ old, current string }

var v8FieldIndexes = make(map[string][]int)
var v8StructPaths []configPath
var v8Paths = buildV8Paths()

// Client paths win by presence. Historical OAuth paths precede legacy globals.
var v8ClientPaths = []configPath{
	{"oauth.providers.codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
	{"providers.codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
}

// Canonical upstream fields win by presence; historical OAuth fields precede globals.
var v8SharedPaths = []configPath{
	{"oauth.providers.codex.disable-codex-cloaking", "upstream.codex.disable-codex-cloaking"},
	{"oauth.providers.codex.stream-bootstrap-buffering", "upstream.codex.stream-bootstrap-buffering"},
	{"oauth.providers.codex.stream-bootstrap-timeout", "upstream.codex.stream-bootstrap-timeout"},
	{"oauth.providers.codex.orphan-delegation-compatibility", "upstream.codex.orphan-delegation-compatibility"},
	{"oauth.providers.codex.model-level-cooling", "upstream.codex.model-level-cooling"},
	{"oauth.providers.codex.response-steering", "upstream.codex.response-steering"},
	{"oauth.providers.claude.model-level-cooling", "upstream.claude.model-level-cooling"},
	{"oauth.providers.claude.claude-code.disable-cloaking-model-list", "upstream.claude.disable-cloaking-model-list"},
	{"oauth.providers.claude.disable-claude-cloak-mode", "upstream.claude.disable-claude-cloak-mode"},
	{"oauth.providers.claude.header-defaults.user-agent", "upstream.claude.header-defaults.user-agent"},
	{"oauth.providers.claude.header-defaults.package-version", "upstream.claude.header-defaults.package-version"},
	{"oauth.providers.claude.header-defaults.runtime-version", "upstream.claude.header-defaults.runtime-version"},
	{"oauth.providers.claude.header-defaults.os", "upstream.claude.header-defaults.os"},
	{"oauth.providers.claude.header-defaults.arch", "upstream.claude.header-defaults.arch"},
	{"oauth.providers.claude.header-defaults.timeout", "upstream.claude.header-defaults.timeout"},
	{"oauth.providers.claude.header-defaults.timezone", "upstream.claude.header-defaults.timezone"},
	{"oauth.providers.claude.header-defaults.stabilize-device-profile", "upstream.claude.header-defaults.stabilize-device-profile"},
	{"oauth.providers.xai.inject-x-search", "upstream.xai.inject-x-search"},
}

var v8Aliases = buildV8Aliases()

func buildV8Aliases() []configPath {
	aliases := append(append([]configPath(nil), v8ClientPaths...), v8SharedPaths...)
	seen := make(map[string]bool)
	for _, alias := range aliases {
		seen[alias.old] = true
	}
	for _, path := range v8Paths {
		if !strings.HasPrefix(path.current, "upstream.") {
			continue
		}
		old := "oauth.providers." + strings.TrimPrefix(path.current, "upstream.")
		if !seen[old] {
			aliases = append(aliases, configPath{old, path.current})
			seen[old] = true
		}
	}
	return aliases
}

var v8SharedStructPaths = []configPath{
	{"oauth.providers.claude.header-defaults", "upstream.claude.header-defaults"},
	{"oauth.providers.claude.claude-code", "upstream.claude"},
	{"oauth.providers.claude", "upstream.claude"},
	{"oauth.providers.xai", "upstream.xai"},
}

var v8KeyFamilies = []configPath{
	{"gemini-api-key", "gemini"}, {"interactions-api-key", "interactions"},
	{"vertex-api-key", "vertex"}, {"codex-api-key", "codex"},
	{"claude-api-key", "claude"}, {"xai-api-key", "xai"}, {"meta-api-key", "meta"},
	{"openai-compatibility", "openai-compatibility"},
}

func buildV8Paths() []configPath {
	prefixes := []configPath{
		{"allow-host", "server.allow-host"}, {"host", "server.host"}, {"port", "server.port"}, {"trusted-proxies", "server.trusted-proxies"},
		{"max-request-body-bytes", "server.max-request-body-bytes"}, {"response-compression", "server.response-compression"},
		{"github-token", "server.github-token"}, {"shutdown-timeout", "server.shutdown-timeout"}, {"kv-store", "server.kv-store"},
		{"tls", "server.tls"}, {"commercial-mode", "server.commercial-mode"}, {"discovery", "server.discovery"},
		{"remote-management", "management"}, {"api-keys", "access.api-keys"},
		{"credential-concurrency", "credentials.concurrency"}, {"credential-in-flight", "credentials.in-flight"},
		{"force-model-prefix", "routing.force-model-prefix"},
		{"request-retry", "routing.retry.request-retry"}, {"max-retry-credentials", "routing.retry.max-retry-credentials"},
		{"max-retry-interval", "routing.retry.max-retry-interval"},
		{"disable-cooling", "routing.cooldown.disable-cooling"}, {"save-cooldown-status", "routing.cooldown.save-cooldown-status"},
		{"transient-error-cooldown-seconds", "routing.cooldown.transient-error-cooldown-seconds"},
		{"proxy-url", "requests.proxy-url"}, {"tls-handshake-timeout-seconds", "requests.tls-handshake-timeout-seconds"}, {"passthrough-headers", "requests.passthrough-headers"},
		{"nonstream-keepalive-interval", "requests.nonstream-keepalive-interval"}, {"streaming", "requests.streaming"}, {"payload", "requests.payload"},
		{"auth-dir", "oauth.auth-dir"}, {"auth-auto-refresh-workers", "oauth.auth-auto-refresh-workers"},
		{"oauth-model-alias", "oauth.model-alias"}, {"oauth-excluded-models", "oauth.excluded-models"}, {"oauth-settings", "oauth.settings"},
		{"oauth-request-scoped-errors", "oauth.request-scoped-errors"}, {"ws-auth", "oauth.providers.aistudio.ws-auth"},
		{"codex.disable-codex-cloaking", "upstream.codex.disable-codex-cloaking"},
		{"codex.stream-bootstrap-buffering", "upstream.codex.stream-bootstrap-buffering"},
		{"codex.stream-bootstrap-timeout", "upstream.codex.stream-bootstrap-timeout"},
		{"codex.optimize-multi-agent-v2", "client.codex.optimize-multi-agent-v2"},
		{"codex.orphan-delegation-compatibility", "upstream.codex.orphan-delegation-compatibility"},
		{"codex.model-level-cooling", "upstream.codex.model-level-cooling"},
		{"codex.response-steering", "upstream.codex.response-steering"},
		{"codex.upstream-request-compression", "upstream.codex.upstream-request-compression"},
		{"codex.upstream-request-compression-min-bytes", "upstream.codex.upstream-request-compression-min-bytes"},
		{"codex.websocket-compression", "upstream.codex.websocket-compression"},
		{"codex", "oauth.providers.codex"}, {"codex-header-defaults", "oauth.providers.codex.header-defaults"},
		{"claude", "upstream.claude"}, {"claude-code", "upstream.claude"},
		{"disable-claude-cloak-mode", "upstream.claude.disable-claude-cloak-mode"},
		{"claude-header-defaults", "upstream.claude.header-defaults"},
		{"antigravity", "oauth.providers.antigravity"},
		{"antigravity-signature-cache-enabled", "oauth.providers.antigravity.signature-cache-enabled"},
		{"antigravity-signature-bypass-strict", "oauth.providers.antigravity.signature-bypass-strict"},
		{"quota-exceeded.antigravity-credits", "oauth.providers.antigravity.antigravity-credits"},
		{"xai", "upstream.xai"}, {"devin", "oauth.providers.devin"},
		{"disable-image-generation", "multimedia.disable-image-generation"}, {"gpt-image-2-base-model", "multimedia.gpt-image-2-base-model"},
		{"video-result-auth-cache-ttl", "multimedia.video-result-auth-cache-ttl"},
		{"debug", "observability.logs.debug"}, {"logging-to-file", "observability.logs.logging-to-file"},
		{"logs-max-total-size-mb", "observability.logs.logs-max-total-size-mb"}, {"request-log", "observability.logs.request-log"},
		{"error-logs-max-files", "observability.logs.error-logs-max-files"},
		{"usage-statistics-enabled", "observability.usage.usage-statistics-enabled"},
		{"redis-usage-queue-retention-seconds", "observability.usage.redis-usage-queue-retention-seconds"}, {"pprof", "observability.pprof"},
	}
	var out []configPath
	seen := make(map[string]bool)
	var walk func(reflect.Type, string, []int, bool)
	walk = func(t reflect.Type, path string, indexes []int, home bool) {
		if t.Kind() == reflect.Struct {
			for _, prefix := range prefixes {
				if path == prefix.old || strings.HasPrefix(path, prefix.old+".") {
					v8StructPaths = append(v8StructPaths, configPath{path, prefix.current + strings.TrimPrefix(path, prefix.old)})
					break
				}
			}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				tag, flags, _ := strings.Cut(field.Tag.Get("yaml"), ",")
				if tag == "-" || field.PkgPath != "" || (strings.Contains(flags, "inline") && field.Type.Kind() == reflect.Map) {
					continue
				}
				fieldIndexes := append(append([]int(nil), indexes...), i)
				if field.Anonymous {
					walk(field.Type, path, fieldIndexes, home)
					continue
				}
				child := tag
				if path != "" {
					child = path + "." + tag
				}
				walk(field.Type, child, fieldIndexes, home)
			}
			return
		}
		for _, prefix := range prefixes {
			if path == prefix.old || strings.HasPrefix(path, prefix.old+".") {
				if !seen[path] {
					out = append(out, configPath{path, prefix.current + strings.TrimPrefix(path, prefix.old)})
					seen[path] = true
				}
				if home {
					v8FieldIndexes[path] = indexes
				}
				return
			}
		}
	}
	// CPA owns execution fields. An SDK update extends this schema automatically.
	walk(reflect.TypeOf(Config{}), "", nil, true)
	walk(reflect.TypeOf(cpaconfig.Config{}), "", nil, false)
	return out
}

func yamlPath(root *yaml.Node, path string) *yaml.Node {
	for _, key := range strings.Split(path, ".") {
		idx := findMapKeyIndex(root, key)
		if idx < 0 {
			return nil
		}
		root = root.Content[idx+1]
	}
	return root
}

func setYAMLPath(root *yaml.Node, path string, value *yaml.Node) {
	parts := strings.Split(path, ".")
	for _, key := range parts[:len(parts)-1] {
		root = getOrCreateMapValue(root, key)
	}
	dst := getOrCreateMapValue(root, parts[len(parts)-1])
	*dst = *deepCopyYAMLNode(value)
}

func deleteYAMLPath(root *yaml.Node, path string) bool {
	parts := strings.SplitN(path, ".", 2)
	idx := findMapKeyIndex(root, parts[0])
	if idx < 0 {
		return false
	}
	if len(parts) == 2 {
		child := root.Content[idx+1]
		if !deleteYAMLPath(child, parts[1]) {
			return false
		}
		if len(child.Content) != 0 {
			return true
		}
	}
	root.Content = append(root.Content[:idx], root.Content[idx+2:]...)
	return true
}

func legacyPath(root *yaml.Node, path string) *yaml.Node {
	node := yamlPath(root, path)
	if path == "api-keys" && node != nil && node.Kind == yaml.MappingNode {
		return nil
	}
	return node
}

// UnmarshalYAML accepts both layouts, including partially migrated documents.
// Presence, rather than Go zero values, determines which setting wins.
func (cfg *Config) UnmarshalYAML(node *yaml.Node) error {
	root, err := flattenV8(node)
	if err != nil {
		return err
	}
	for _, family := range v8KeyFamilies {
		keys := yamlPath(root, family.old)
		if family.old == "openai-compatibility" {
			if errValidate := validateOpenAICompatibilityWeightNodes(keys); errValidate != nil {
				return errValidate
			}
		} else if errValidate := validateWeightSequenceNode(keys, family.old); errValidate != nil {
			return errValidate
		}
	}
	decoded := legacyConfig(*cfg)
	if err = root.Decode(&decoded); err != nil {
		return err
	}
	if errValidate := decoded.Models.Validate(); errValidate != nil {
		return errValidate
	}
	*cfg = Config(decoded)
	if errCapture := cfg.captureConfigTree(node, root); errCapture != nil {
		return errCapture
	}
	if errValidate := cfg.ValidateHomePolicies(); errValidate != nil {
		return errValidate
	}
	if errValidate := validateCPAExecution(root); errValidate != nil {
		return errValidate
	}
	if errValidate := cfg.ValidateCredentialWeights(); errValidate != nil {
		return errValidate
	}
	cfg.OAuthOnlyFields = nil
	source := expandConfigAliases(node)
	for _, path := range v8Paths {
		if strings.HasPrefix(path.current, "oauth.providers.") && yamlPath(source, path.current) != nil {
			if cfg.OAuthOnlyFields == nil {
				cfg.OAuthOnlyFields = make(map[string]bool)
			}
			cfg.OAuthOnlyFields[path.old] = true
		}
	}
	return nil
}

func flattenV8(node *yaml.Node) (*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config must be a mapping")
	}
	// Decode once before transformation to reject duplicate keys even when a
	// winning v8 value would otherwise hide the malformed legacy subtree.
	var shape map[string]any
	if err := node.Decode(&shape); err != nil {
		return nil, err
	}
	node = expandConfigAliases(node)
	if _, err := normalizeLegacyConfigFields(node, true); err != nil {
		return nil, err
	}
	for _, path := range append(append([]configPath(nil), v8Paths...), v8Aliases...) {
		parts := strings.Split(path.current, ".")
		for i := 1; i < len(parts); i++ {
			parent := yamlPath(node, strings.Join(parts[:i], "."))
			if parent == nil {
				break
			}
			// Routing is shared with the legacy layout, where null means defaults.
			if i == 1 && parts[0] == "routing" && parent.Tag == "!!null" {
				break
			}
			if parent.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("%s must be a mapping", strings.Join(parts[:i], "."))
			}
		}
	}
	root := deepCopyYAMLNode(node)
	for _, path := range v8Aliases {
		if value := yamlPath(root, path.old); value != nil {
			if yamlPath(root, path.current) == nil {
				setYAMLPath(root, path.current, value)
			}
			deleteYAMLPath(root, path.old)
		}
	}
	for _, path := range v8SharedStructPaths {
		value := yamlPath(root, path.old)
		if value == nil {
			continue
		}
		if value.Tag != "!!null" && value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", path.old)
		}
		if value.Tag != "!!null" && len(value.Content) != 0 {
			continue
		}
		if yamlPath(root, path.current) == nil {
			copy := copyYAMLPathValue(root, path.old)
			copy.Kind, copy.Tag, copy.Value = yaml.MappingNode, "!!map", ""
			setYAMLPathWithComments(root, path.current, copy)
		}
		deleteYAMLPath(root, path.old)
	}
	if version := yamlPath(root, "config-version"); version != nil && (version.Tag != "!!int" || version.Value != "8") {
		return nil, fmt.Errorf("unsupported config-version (expected 8)")
	}
	// The v8 upstream map reuses the legacy client-key field name.
	if keys := yamlPath(root, "api-keys"); keys != nil && keys.Kind == yaml.MappingNode {
		deleteYAMLPath(root, "api-keys")
	}
	for _, path := range v8Paths {
		if value := yamlPath(root, path.current); value != nil {
			copy := copyYAMLPathValue(root, path.current)
			deleteYAMLPath(root, path.current)
			setYAMLPathWithComments(root, path.old, copy)
		}
	}
	for _, family := range v8KeyFamilies {
		if groups := yamlPath(node, "api-keys."+family.current); groups != nil {
			keys, err := expandV8Groups(groups, family.current)
			if err != nil {
				return nil, err
			}
			setYAMLPath(root, family.old, keys)
		}
	}
	pruneEmptyV8Containers(root)
	return root, nil
}

func pruneEmptyV8Containers(root *yaml.Node) {
	for _, path := range v8Paths {
		parts := strings.Split(path.current, ".")
		for end := len(parts) - 1; end > 0; end-- {
			container := strings.Join(parts[:end], ".")
			if value := yamlPath(root, container); value != nil && value.Kind == yaml.MappingNode && len(value.Content) == 0 {
				deleteYAMLPath(root, container)
			}
		}
	}
}

var sharedKeyFields = buildSharedKeyFields()

func buildSharedKeyFields() map[string]bool {
	shared := map[string]bool{
		"priority": true, "prefix": true, "proxy-url": true, "headers": true,
		"models": true, "excluded-models": true, "disable-cooling": true,
		"request-retry": true, "request-scoped-errors": true,
	}
	home := configFields(reflect.TypeOf(Config{}))
	sdk := configFields(reflect.TypeOf(cpaconfig.Config{}))
	for _, family := range v8KeyFamilies {
		homeFields := configFields(home[family.old].Elem())
		sdkFields := configFields(sdk[family.old].Elem())
		for name := range sdkFields {
			if _, owned := homeFields[name]; !owned && name != "api-key" && name != "id" && name != "uuid" && name != "name" && name != "base-url" && name != "weight" {
				shared[name] = true
			}
		}
	}
	return shared
}

func expandV8Groups(groups *yaml.Node, provider string) (*yaml.Node, error) {
	if groups.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("api-keys.%s must be a list", provider)
	}
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index, group := range groups.Content {
		if group.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("api-keys.%s[%d] must be a mapping", provider, index)
		}
		keys := yamlPath(group, "keys")
		if keys == nil || keys.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("api-keys.%s[%d].keys must be a list", provider, index)
		}
		if err := validateWeightSequenceNode(keys, "api-keys."+provider+".keys"); err != nil {
			return nil, err
		}
		if provider == "openai-compatibility" {
			item := deepCopyYAMLNode(group)
			deleteYAMLPath(item, "keys")
			setYAMLPath(item, "api-key-entries", keys)
			out.Content = append(out.Content, item)
			continue
		}
		for i := 0; i < len(group.Content); i += 2 {
			field := group.Content[i].Value
			if field != "name" && field != "base-url" && field != "keys" && !sharedKeyFields[field] {
				return nil, fmt.Errorf("api-keys.%s: unsupported group field %s", provider, field)
			}
		}
		for _, key := range keys.Content {
			if key.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("api-keys.%s key must be a mapping", provider)
			}
			if yamlPath(key, "base-url") != nil {
				return nil, fmt.Errorf("api-keys.%s: base-url belongs to the group", provider)
			}
			item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for i := 0; i < len(group.Content); i += 2 {
				field := group.Content[i].Value
				if field == "base-url" || sharedKeyFields[field] {
					setYAMLPath(item, field, group.Content[i+1])
				}
			}
			for i := 0; i < len(key.Content); i += 2 {
				if key.Content[i+1].Tag != "!!null" {
					field := key.Content[i].Value
					if field == "connection-pool" {
						setYAMLPath(item, field, mergeConnectionPoolNode(yamlPath(group, field), key.Content[i+1]))
					} else {
						setYAMLPath(item, field, key.Content[i+1])
					}
				}
			}
			out.Content = append(out.Content, item)
		}
	}
	return out, nil
}

// mergeConnectionPoolNode preserves group defaults for omitted or null per-key fields.
func mergeConnectionPoolNode(base, override *yaml.Node) *yaml.Node {
	merged := deepCopyYAMLNode(base)
	if merged == nil || merged.Kind != yaml.MappingNode || override == nil || override.Kind != yaml.MappingNode {
		return deepCopyYAMLNode(override)
	}
	for i := 0; i < len(override.Content); i += 2 {
		if override.Content[i+1].Tag != "!!null" {
			setYAMLPath(merged, override.Content[i].Value, override.Content[i+1])
		}
	}
	return merged
}

// groupLegacyKeys deliberately keeps one group per legacy entry: equal endpoints
// do not imply equal routing, headers, models, or credential policies.
func groupLegacyKeys(keys *yaml.Node, provider string) *yaml.Node {
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for index, entry := range keys.Content {
		group := deepCopyYAMLNode(entry)
		if provider == "openai-compatibility" {
			entries := yamlPath(group, "api-key-entries")
			if entries == nil {
				entries = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			}
			setYAMLPath(group, "keys", entries)
			deleteYAMLPath(group, "api-key-entries")
		} else {
			group = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setYAMLPath(group, "name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%s-%d", provider, index+1)})
			key := deepCopyYAMLNode(entry)
			for i := 0; i < len(entry.Content); i += 2 {
				field := entry.Content[i].Value
				if field == "base-url" || sharedKeyFields[field] {
					setYAMLPath(group, field, entry.Content[i+1])
					deleteYAMLPath(key, field)
				}
			}
			setYAMLPath(group, "keys", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{key}})
		}
		out.Content = append(out.Content, group)
	}
	return out
}

// NormalizeConfigLayout removes conflicting legacy fields. migrate also moves
// legacy-only fields; callers use it only for an explicit v8 configuration write.
func NormalizeConfigLayout(data []byte, migrate bool) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if len(doc.Content) == 0 {
		if !migrate {
			return data, false, nil
		}
		return nil, false, fmt.Errorf("empty config")
	}
	root := doc.Content[0]
	if _, err := flattenV8(root); err != nil {
		return nil, false, err
	}
	root = expandConfigAliases(root)
	doc.Content[0] = root
	changed, err := normalizeLegacyConfigFields(root, migrate)
	if err != nil {
		return nil, false, err
	}
	// Empty legacy structs have no leaf fields to move. Preserve them as empty v8
	// mappings; null structs also mean defaults. User-owned maps are not included.
	paths := append(append([]configPath(nil), v8Aliases...), v8Paths...)
	for _, path := range append(append([]configPath(nil), v8StructPaths...), v8SharedStructPaths...) {
		old := yamlPath(root, path.old)
		if old == nil || (!migrate && yamlPath(root, path.current) == nil) {
			continue
		}
		if old.Tag == "!!null" {
			old.Kind, old.Tag, old.Value = yaml.MappingNode, "!!map", ""
		} else if old.Kind != yaml.MappingNode || len(old.Content) != 0 {
			continue
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		old := legacyPath(root, path.old)
		if old == nil {
			continue
		}
		current := yamlPath(root, path.current)
		if current == nil && !migrate {
			continue
		}
		copy := copyYAMLPathValue(root, path.old)
		deleteYAMLPath(root, path.old)
		if current == nil {
			setYAMLPathWithComments(root, path.current, copy)
		}
		changed = true
	}
	for _, family := range v8KeyFamilies {
		old := yamlPath(root, family.old)
		if old == nil {
			continue
		}
		path := "api-keys." + family.current
		if yamlPath(root, path) == nil {
			if !migrate {
				continue
			}
			setYAMLPath(root, path, groupLegacyKeys(old, family.current))
		}
		deleteYAMLPath(root, family.old)
		changed = true
	}
	if migrate {
		// Home persists structured database values, so ignored legacy sections
		// are removed from the v8 view instead of being retained as YAML comments.
		removeUnknownV8Sections(root)
		setYAMLPath(root, "config-version", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "8"})
		changed = true
	}
	if !changed {
		return data, false, nil
	}
	out, err := yaml.Marshal(&doc)
	return out, true, err
}

func v8AllowedRoots() map[string]bool {
	allowed := map[string]bool{"models": true, "client": true, "config-version": true, "api-keys": true, "plugins": true, "quota-exceeded": true, "user-email": true, "credential-concurrency-policies": true}
	for _, path := range v8Paths {
		section, _, _ := strings.Cut(path.current, ".")
		allowed[section] = true
	}
	return allowed
}

func removeUnknownV8Sections(root *yaml.Node) {
	allowed := v8AllowedRoots()
	for i := 0; i+1 < len(root.Content); {
		if allowed[root.Content[i].Value] {
			i += 2
			continue
		}
		key := root.Content[i].Value
		log.Warnf("unrecognized configuration section %q ignored during v8 migration", key)
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
	}
}

// Normalize removed fields at YAML boundaries without retaining runtime aliases.
// Legacy-only files are left untouched until an explicit migration/save.
func normalizeLegacyConfigFields(root *yaml.Node, migrate bool) (bool, error) {
	changed := false
	if migrate {
		for _, path := range []string{"codex.identity-confuse", "oauth.providers.codex.identity-confuse"} {
			changed = deleteYAMLPath(root, path) || changed
		}
		const legacyAffinity = "routing.claude-code-session-affinity"
		const currentAffinity = "routing.session-affinity"
		if value := yamlPath(root, legacyAffinity); value != nil {
			var legacyEnabled, currentEnabled bool
			if errDecode := value.Decode(&legacyEnabled); errDecode != nil {
				return false, fmt.Errorf("decode %s: %w", legacyAffinity, errDecode)
			}
			if current := yamlPath(root, currentAffinity); current != nil {
				if errDecode := current.Decode(&currentEnabled); errDecode != nil {
					return false, fmt.Errorf("decode %s: %w", currentAffinity, errDecode)
				}
			}
			// Home previously enabled affinity when either spelling was true.
			replacement := &yaml.Node{}
			if errEncode := replacement.Encode(legacyEnabled || currentEnabled); errEncode != nil {
				return false, errEncode
			}
			setYAMLPath(root, currentAffinity, replacement)
			deleteYAMLPath(root, legacyAffinity)
			changed = true
		}
	}
	// The deprecated allow flag is the inverse of disable-private-remote-ips.
	// Resolve it before applying v8 precedence so both spellings cannot coexist.
	const old = "codex.live-media-relay.allow-private-remote-ips"
	const canonical = "codex.live-media-relay.disable-private-remote-ips"
	value := yamlPath(root, old)
	if value == nil {
		return changed, nil
	}
	if yamlPath(root, "oauth.providers."+canonical) != nil {
		return deleteYAMLPath(root, old) || changed, nil
	}
	if !migrate || yamlPath(root, canonical) != nil {
		return changed, nil
	}
	var allow bool
	if err := value.Decode(&allow); err != nil {
		return false, fmt.Errorf("decode %s: %w", old, err)
	}
	replacement := deepCopyYAMLNode(value)
	if err := replacement.Encode(!allow); err != nil {
		return false, err
	}
	setYAMLPath(root, canonical, replacement)
	deleteYAMLPath(root, old)
	return true, nil
}

// Expand aliases and YAML merge keys before moving paths. Keeping an alias to a
// removed legacy anchor would produce an unreadable file, and updating a shared
// anchor in place could unintentionally change an unrelated setting.
// Callers first decode the document so yaml.v3 rejects cyclic/invalid aliases.
func expandConfigAliases(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		return expandConfigAliases(node.Alias)
	}
	copy := *node
	copy.Anchor = ""
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = expandConfigAliases(child)
	}
	if copy.Kind != yaml.MappingNode {
		return &copy
	}
	for i := 0; i < len(copy.Content); i += 2 {
		if copy.Content[i].Tag != "!!merge" {
			continue
		}
		merge := copy.Content[i+1]
		copy.Content = append(copy.Content[:i], copy.Content[i+2:]...)
		mappings := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			mappings = merge.Content
		}
		for _, mapping := range mappings {
			for j := 0; j < len(mapping.Content); j += 2 {
				if findMapKeyIndex(&copy, mapping.Content[j].Value) < 0 {
					copy.Content = append(copy.Content, mapping.Content[j], mapping.Content[j+1])
				}
			}
		}
		i -= 2
	}
	return &copy
}

// ValidateV8Config accepts only the v8 layout in management writes.
// Plugin-owned configuration remains extensible through its existing decoder.
func ValidateV8Config(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("empty config")
	}
	root := doc.Content[0]
	flat, err := flattenV8(root)
	if err != nil {
		return err
	}
	root = expandConfigAliases(root)
	allowedRoots := v8AllowedRoots()
	for _, path := range []string{"codex.identity-confuse", "oauth.providers.codex.identity-confuse", "routing.claude-code-session-affinity"} {
		if yamlPath(root, path) != nil {
			return fmt.Errorf("removed configuration field %s is not accepted by v8", path)
		}
	}
	for _, path := range append(append(append([]configPath(nil), v8Paths...), v8Aliases...), v8SharedStructPaths...) {
		if legacyPath(root, path.old) != nil {
			return fmt.Errorf("legacy field %s is not accepted by v8; use %s", path.old, path.current)
		}
	}
	for i := 0; i < len(root.Content); i += 2 {
		if key := root.Content[i].Value; !allowedRoots[key] {
			return fmt.Errorf("unknown v8 configuration section %s", key)
		}
	}
	if groups := yamlPath(root, "api-keys"); groups != nil && groups.Kind == yaml.MappingNode {
		for i := 0; i < len(groups.Content); i += 2 {
			found := false
			for _, family := range v8KeyFamilies {
				if groups.Content[i].Value == family.current {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("unknown API-key provider %s", groups.Content[i].Value)
			}
		}
	}
	deleteYAMLPath(flat, "config-version")
	// Empty struct containers are valid replacements. Only strip known structural
	// paths; empty user maps (headers, aliases, plugin options) carry real values.
	pruneEmptyV8Containers(flat)
	encoded, err := yaml.Marshal(flat)
	if err != nil {
		return err
	}
	if errValidate := validateExecutionSchema(flat, []reflect.Type{reflect.TypeOf(legacyConfig{}), reflect.TypeOf(cpaconfig.Config{})}, ""); errValidate != nil {
		return errValidate
	}
	var cfg legacyConfig
	if errDecode := flat.Decode(&cfg); errDecode != nil {
		return errDecode
	}
	if errValidate := (*Config)(&cfg).ValidateHomePolicies(); errValidate != nil {
		return errValidate
	}
	// Validate the compatible runtime projection with the bundled CPA SDK.
	_, errParse := cpaconfig.ParseConfigBytes(encoded)
	return errParse
}
