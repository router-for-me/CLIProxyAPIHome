package config

import (
	"fmt"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

// Reject invalid rules before the SDK parser can silently drop them.
// The original tree remains authoritative for storage and distribution.
func validatePayloadReplace(root *yaml.Node) error {
	rules := yamlPath(root, "payload.replace")
	if rules == nil || rules.Tag == "!!null" {
		return nil
	}
	if rules.Kind != yaml.SequenceNode {
		return fmt.Errorf("requests.payload.replace must be a list")
	}
	for i, node := range rules.Content {
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("requests.payload.replace[%d]: rule must be an object", i)
		}
		var rule cpaconfig.PayloadReplaceRule
		if errDecode := node.Decode(&rule); errDecode != nil {
			return fmt.Errorf("requests.payload.replace[%d]: invalid replacement rule structure", i)
		}
		if errValidate := rule.Validate(); errValidate != nil {
			return fmt.Errorf("requests.payload.replace[%d]: %w", i, errValidate)
		}
	}
	return nil
}
