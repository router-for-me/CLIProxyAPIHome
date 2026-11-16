package config

import (
	"fmt"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

// This schema bridges payload.replace until Home's bundled SDK includes it.
// It is validation-only: the authoritative tree is stored and distributed intact,
// and downstream CPA nodes perform all message selection and text replacement.
type payloadReplaceSchema struct {
	Replace []payloadReplaceRule `yaml:"replace"`
}

type payloadReplaceRule struct {
	Models []cpaconfig.PayloadModelRule `yaml:"models"`
	Target struct {
		Role      string `yaml:"role"`
		Positions []int  `yaml:"positions"`
		All       bool   `yaml:"all"`
	} `yaml:"target"`
	Replacements []map[string]string `yaml:"replacements"`
}

func validatePayloadReplace(root *yaml.Node) error {
	rules := yamlPath(root, "payload.replace")
	if rules == nil || rules.Tag == "!!null" {
		return nil
	}
	if rules.Kind != yaml.SequenceNode {
		return fmt.Errorf("requests.payload.replace must be a list")
	}
	for i, node := range rules.Content {
		if errValidate := validatePayloadReplaceRule(node); errValidate != nil {
			return fmt.Errorf("requests.payload.replace[%d]: %w", i, errValidate)
		}
	}
	return nil
}

func validatePayloadReplaceRule(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("rule must be an object")
	}
	var rule payloadReplaceRule
	if errDecode := node.Decode(&rule); errDecode != nil {
		return fmt.Errorf("invalid replacement rule structure")
	}
	if role := yamlPath(node, "target.role"); role == nil || role.Tag != "!!str" || rule.Target.Role != "user" {
		return fmt.Errorf("target.role must be user")
	}
	if all := yamlPath(node, "target.all"); all != nil && all.Tag != "!!null" && all.Tag != "!!bool" {
		return fmt.Errorf("target.all must be a boolean")
	}
	if rule.Target.All {
		if len(rule.Target.Positions) != 0 {
			return fmt.Errorf("target.all and target.positions are mutually exclusive")
		}
	} else if len(rule.Target.Positions) == 0 {
		return fmt.Errorf("target.positions must not be empty unless target.all is true")
	}
	if positions := yamlPath(node, "target.positions"); positions != nil && positions.Tag != "!!null" {
		if positions.Kind != yaml.SequenceNode {
			return fmt.Errorf("target.positions must be a list")
		}
		for i, position := range positions.Content {
			if position.Tag != "!!int" || rule.Target.Positions[i] < 1 {
				return fmt.Errorf("target.positions must contain positive one-based integers")
			}
		}
	}
	if len(rule.Replacements) == 0 {
		return fmt.Errorf("replacements must not be empty")
	}
	replacements := yamlPath(node, "replacements")
	for _, replacement := range replacements.Content {
		if replacement.Kind != yaml.MappingNode || len(replacement.Content) != 2 {
			return fmt.Errorf("each replacement must contain exactly one string mapping")
		}
		from, to := replacement.Content[0], replacement.Content[1]
		if from.Kind != yaml.ScalarNode || from.Tag != "!!str" || from.Value == "" || to.Kind != yaml.ScalarNode || to.Tag != "!!str" {
			return fmt.Errorf("replacement source must be a non-empty string and destination must be a string")
		}
	}
	return nil
}
