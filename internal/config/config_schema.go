package config

import (
	"fmt"
	"reflect"
	"strings"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

// configFields is used only for strict field validation and V8 layout discovery.
// Configuration storage and credential round trips do not use reflection.
func configFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	var walk func(reflect.Type)
	walk = func(current reflect.Type) {
		for current.Kind() == reflect.Pointer {
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < current.NumField(); i++ {
			field := current.Field(i)
			name, flags, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if field.PkgPath != "" || name == "-" {
				continue
			}
			if strings.Contains(flags, "inline") {
				walk(field.Type)
				continue
			}
			if name == "" {
				name = strings.ToLower(field.Name)
			}
			fields[name] = field.Type
		}
	}
	walk(t)
	return fields
}

// validateExecutionSchema checks known fields against Home and SDK YAML schemas.
// Semantic validation is delegated to their parsers, without rebuilding the tree.
func validateExecutionSchema(node *yaml.Node, types []reflect.Type, path string) error {
	if node == nil || node.Tag == "!!null" || path == "plugins.configs" {
		return nil
	}
	for i, t := range types {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		types[i] = t
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			var children []reflect.Type
			for _, t := range types {
				switch t.Kind() {
				case reflect.Interface:
					return nil
				case reflect.Map:
					children = append(children, t.Elem())
				case reflect.Struct:
					fields := configFields(t)
					if field, exists := fields[key]; exists {
						children = append(children, field)
					}
				}
			}
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if len(children) == 0 {
				return fmt.Errorf("unknown configuration field %s", childPath)
			}
			if errValidate := validateExecutionSchema(node.Content[i+1], children, childPath); errValidate != nil {
				return errValidate
			}
		}
	}
	if node.Kind == yaml.SequenceNode {
		var elements []reflect.Type
		for _, t := range types {
			if t.Kind() == reflect.Interface {
				return nil
			}
			if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
				elements = append(elements, t.Elem())
			}
		}
		if len(elements) == 0 {
			return fmt.Errorf("%s must not be a list", path)
		}
		for i, item := range node.Content {
			if errValidate := validateExecutionSchema(item, append([]reflect.Type(nil), elements...), fmt.Sprintf("%s[%d]", path, i)); errValidate != nil {
				return errValidate
			}
		}
	}
	return nil
}

// Validate imports/reloads against the SDK without persisting its defaults.
// Home validates its lifecycle, normalizes trusted proxies, and owns the secret hash.
func validateCPAExecution(root *yaml.Node) error {
	if errValidate := validatePayloadReplace(root); errValidate != nil {
		return errValidate
	}
	execution := deepCopyYAMLNode(root)
	for _, path := range []string{"remote-management.secret-key", "credential-concurrency", "credential-in-flight", "trusted-proxies"} {
		deleteYAMLPath(execution, path)
	}
	encoded, errMarshal := yaml.Marshal(execution)
	if errMarshal != nil {
		return errMarshal
	}
	_, errParse := cpaconfig.ParseConfigBytes(encoded)
	return errParse
}
