package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func findMapKeyIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}
	return -1
}

// IsOAuthProviderRoot identifies legacy roots that contain OAuth-scoped fields.
func IsOAuthProviderRoot(key string) bool {
	for _, path := range v8Paths {
		if strings.HasPrefix(path.current, "oauth.providers.") && (path.old == key || strings.HasPrefix(path.old, key+".")) {
			return true
		}
	}
	return false
}

// NormalizeConfigRoot keeps the database's legacy roots while retaining OAuth
// provider paths as scope information. Both layouts describe the same values.
// Keeping the legacy projection also lets older CPA nodes consume the snapshot.
func NormalizeConfigRoot(root map[string]any) (map[string]any, error) {
	if root == nil {
		root = make(map[string]any)
	}
	var source yaml.Node
	if errEncode := source.Encode(root); errEncode != nil {
		return nil, errEncode
	}
	flat, errFlatten := flattenV8(&source)
	if errFlatten != nil {
		return nil, errFlatten
	}
	for _, path := range v8Paths {
		if strings.HasPrefix(path.current, "oauth.providers.") {
			if value := yamlPath(&source, path.current); value != nil {
				setYAMLPath(flat, path.current, value)
			}
		}
	}
	deleteYAMLPath(flat, "config-version")
	var result map[string]any
	if errDecode := flat.Decode(&result); errDecode != nil {
		return nil, errDecode
	}
	return result, nil
}

// UpdateOAuthScope applies a legacy root edit to any existing v8 OAuth paths.
// A nil value removes the corresponding scoped settings as well as their root.
func UpdateOAuthScope(oauth any, key string, value any) (any, bool, error) {
	var root yaml.Node
	if errEncode := root.Encode(map[string]any{"oauth": oauth, key: value}); errEncode != nil {
		return nil, false, errEncode
	}
	changed := false
	for _, path := range v8Paths {
		if !strings.HasPrefix(path.current, "oauth.providers.") || (path.old != key && !strings.HasPrefix(path.old, key+".")) {
			continue
		}
		if yamlPath(&root, path.current) == nil {
			continue
		}
		changed = true
		if next := yamlPath(&root, path.old); value != nil && next != nil {
			setYAMLPath(&root, path.current, next)
		} else {
			deleteYAMLPath(&root, path.current)
		}
	}
	if !changed {
		return oauth, false, nil
	}
	var result any = map[string]any{}
	if next := yamlPath(&root, "oauth"); next != nil {
		if errDecode := next.Decode(&result); errDecode != nil {
			return nil, false, errDecode
		}
	}
	return result, true, nil
}

// LegacyConfigRoot materializes effective runtime fields for v0 field updates.
// Scope is retained in the persisted document by UpdateOAuthScope.
func LegacyConfigRoot(cfg *Config) (map[string]any, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	var node yaml.Node
	if errEncode := node.Encode((*legacyConfig)(cfg)); errEncode != nil {
		return nil, errEncode
	}
	// Only the legacy field-update adapter serializes runtime fields. Normal
	// snapshots use RawConfig directly; unknown CPA fields remain in this tree.
	if cfg.RawConfig != nil {
		stored, errNormalize := NormalizeConfigRoot(cfg.RawConfig)
		if errNormalize != nil {
			return nil, errNormalize
		}
		var source yaml.Node
		if errEncode := source.Encode(stored); errEncode != nil {
			return nil, errEncode
		}
		mergeLegacyRuntimeFields(&source, &node)
		node = source
	}
	var root map[string]any
	if errDecode := node.Decode(&root); errDecode != nil {
		return nil, errDecode
	}
	return root, nil
}

func mergeLegacyRuntimeFields(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		*dst = *deepCopyYAMLNode(src)
		return
	}
	for i := 0; i < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		if index := findMapKeyIndex(dst, key.Value); index >= 0 {
			mergeLegacyRuntimeFields(dst.Content[index+1], value)
		} else {
			dst.Content = append(dst.Content, deepCopyYAMLNode(key), deepCopyYAMLNode(value))
		}
	}
}
