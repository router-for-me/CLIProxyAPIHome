package management

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// configV8AuthIndexFields are runtime identifiers, never configuration: JSON
// reads add one to every API key credential and writes drop either spelling.
var configV8AuthIndexFields = []string{"auth_index", "auth-index"}

// injectConfigV8APIKeyAuthIndexes adds the credential selector used by
// /requests/api-call, matching CPA's JSON view. Home's credential ID is already
// authoritative; keyless compatibility credentials carry that ID on the group.
func injectConfigV8APIKeyAuthIndexes(root *yaml.Node) {
	apiKeys := configV8Node(root, []string{"api-keys"})
	if apiKeys == nil || apiKeys.Kind != yaml.MappingNode {
		return
	}
	for i := 1; i < len(apiKeys.Content); i += 2 {
		groups := apiKeys.Content[i]
		if groups == nil || groups.Kind != yaml.SequenceNode {
			continue
		}
		for _, group := range groups.Content {
			if group == nil || group.Kind != yaml.MappingNode {
				continue
			}
			keys := configV8Node(group, []string{"keys"})
			if keys == nil || keys.Kind != yaml.SequenceNode || len(keys.Content) == 0 {
				setConfigV8AuthIndexFromID(group)
				continue
			}
			for _, key := range keys.Content {
				setConfigV8AuthIndexFromID(key)
			}
		}
	}
}

func setConfigV8AuthIndexFromID(node *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	deleteConfigV8AuthIndexFields(node)
	idNode := configV8Node(node, []string{"id"})
	if idNode == nil || idNode.Kind != yaml.ScalarNode || idNode.Tag != "!!str" {
		return
	}
	id := strings.TrimSpace(idNode.Value)
	if id == "" {
		return
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "auth_index"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id},
	)
}

// stripConfigV8AuthIndexes removes the read-only auth index from the api-keys
// part of a write, so a client can send back exactly what it read. parts is the
// write's path below /config. Only group and key mappings are touched, the two
// levels a read adds the field to; a header that happens to share the name is
// configuration and survives.
func stripConfigV8AuthIndexes(parts []string, update *yaml.Node) {
	if update == nil {
		return
	}
	switch {
	case len(parts) == 0:
		stripConfigV8ProviderMap(configV8Node(update, []string{"api-keys"}))
	case parts[0] != "api-keys":
	case len(parts) == 1:
		stripConfigV8ProviderMap(update)
	default:
		stripConfigV8Groups(update)
	}
}

func stripConfigV8ProviderMap(providers *yaml.Node) {
	if providers == nil || providers.Kind != yaml.MappingNode {
		return
	}
	for i := 1; i < len(providers.Content); i += 2 {
		stripConfigV8Groups(providers.Content[i])
	}
}

func stripConfigV8Groups(groups *yaml.Node) {
	if groups == nil {
		return
	}
	if groups.Kind == yaml.MappingNode {
		stripConfigV8Group(groups)
		return
	}
	if groups.Kind != yaml.SequenceNode {
		return
	}
	for _, group := range groups.Content {
		stripConfigV8Group(group)
	}
}

func stripConfigV8Group(group *yaml.Node) {
	deleteConfigV8AuthIndexFields(group)
	keys := configV8Node(group, []string{"keys"})
	if keys == nil || keys.Kind != yaml.SequenceNode {
		return
	}
	for _, key := range keys.Content {
		deleteConfigV8AuthIndexFields(key)
	}
}

func deleteConfigV8AuthIndexFields(node *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	kept := node.Content[:0]
	for i := 0; i+1 < len(node.Content); i += 2 {
		if isConfigV8AuthIndexField(node.Content[i].Value) {
			continue
		}
		kept = append(kept, node.Content[i], node.Content[i+1])
	}
	node.Content = kept
}

func isConfigV8AuthIndexField(name string) bool {
	for _, field := range configV8AuthIndexFields {
		if name == field {
			return true
		}
	}
	return false
}
