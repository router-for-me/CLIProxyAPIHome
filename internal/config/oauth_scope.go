package config

import (
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// MarshalYAML returns the complete source tree without rebuilding it from runtime
// fields. Programmatically constructed configs retain the legacy fallback below.
// Legacy field updates explicitly use LegacyConfigRoot instead.
func (cfg Config) MarshalYAML() (any, error) {
	if cfg.RawConfig != nil {
		return cfg.RawConfig, nil
	}
	var root yaml.Node
	if err := root.Encode(legacyConfig(cfg)); err != nil {
		return nil, err
	}
	value := reflect.ValueOf(cfg)
	for _, path := range v8Paths {
		if !strings.HasPrefix(path.current, "oauth.providers.") || !cfg.OAuthOnlyFields[path.old] {
			continue
		}
		field := yamlPath(&root, path.old)
		if field == nil {
			field = new(yaml.Node)
			indexes, owned := v8FieldIndexes[path.old]
			if !owned {
				continue
			}
			if err := field.Encode(value.FieldByIndex(indexes).Interface()); err != nil {
				return nil, err
			}
		}
		copy := deepCopyYAMLNode(field)
		deleteYAMLPath(&root, path.old)
		setYAMLPath(&root, path.current, copy)
	}
	return &root, nil
}
