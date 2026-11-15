package config

import (
	"encoding/json"
	"fmt"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

// HomeCredentialConfigMetadataKey is private persistence data, never a CPA payload.
const HomeCredentialConfigMetadataKey = "home_config"

// These fields are stored authoritatively on auth records, not in CPA options.
var HomeCredentialFields = [...]string{"id", "uuid", "api-key", "base-url", "proxy-url", "prefix", "priority", "headers", "excluded-models", "disable-cooling", "request-retry", "name", "disabled"}

// CredentialConfig retains the complete credential subtree, including models and
// cloak settings. Home's typed credential fields are scheduling projections only.
type CredentialConfig struct {
	RawConfig map[string]any `yaml:"-" json:"-"`
}

func (c *CredentialConfig) SetConfigTree(tree map[string]any) { c.RawConfig = tree }

// SDK credential types define the CPA wire contract; Home does not mirror it.
func newSDKCredential(entry any) (any, error) {
	var sdk any
	switch entry.(type) {
	case GeminiKey, *GeminiKey:
		sdk = &cpaconfig.GeminiKey{}
	case VertexCompatKey, *VertexCompatKey:
		sdk = &cpaconfig.VertexCompatKey{}
	case CodexKey, *CodexKey:
		sdk = &cpaconfig.CodexKey{}
	case ClaudeKey, *ClaudeKey:
		sdk = &cpaconfig.ClaudeKey{}
	case OpenAICompatibility, *OpenAICompatibility:
		sdk = &cpaconfig.OpenAICompatibility{}
	case OpenAICompatibilityAPIKey, *OpenAICompatibilityAPIKey:
		sdk = &cpaconfig.OpenAICompatibilityAPIKey{}
	default:
		return nil, fmt.Errorf("unsupported credential configuration %T", entry)
	}
	return sdk, nil
}

// CredentialExecutionOptions translates YAML to CPA JSON using SDK types.
// This projection is not used to reconstruct newly stored configuration.
func CredentialExecutionOptions(entry any) (map[string]any, error) {
	sdk, errSDK := newSDKCredential(entry)
	if errSDK != nil {
		return nil, errSDK
	}
	data, errMarshal := yaml.Marshal(entry)
	if errMarshal != nil {
		return nil, errMarshal
	}
	if errDecode := yaml.Unmarshal(data, sdk); errDecode != nil {
		return nil, errDecode
	}
	wire, errWire := json.Marshal(sdk)
	if errWire != nil {
		return nil, errWire
	}
	var fields map[string]any
	if errDecode := json.Unmarshal(wire, &fields); errDecode != nil {
		return nil, errDecode
	}
	return fields, nil
}

// CredentialConfigFromOptions upgrades older records that only retained the CPA
// JSON projection. New records always export their original credential tree.
// Seed the SDK view with recoverable auth fields before applying retained options.
func CredentialConfigFromOptions(entry, options any) (map[string]any, error) {
	sdk, errSDK := newSDKCredential(entry)
	if errSDK != nil {
		return nil, errSDK
	}
	base, errBase := json.Marshal(entry)
	if errBase != nil {
		return nil, fmt.Errorf("encode legacy credential fields: %w", errBase)
	}
	if errDecode := json.Unmarshal(base, sdk); errDecode != nil {
		return nil, fmt.Errorf("decode legacy credential fields: %w", errDecode)
	}
	data, errMarshal := json.Marshal(options)
	if errMarshal != nil {
		return nil, errMarshal
	}
	if errDecode := json.Unmarshal(data, sdk); errDecode != nil {
		return nil, errDecode
	}
	encoded, errEncode := yaml.Marshal(sdk)
	if errEncode != nil {
		return nil, errEncode
	}
	var tree map[string]any
	if errDecode := yaml.Unmarshal(encoded, &tree); errDecode != nil {
		return nil, errDecode
	}
	return tree, nil
}

func (c GeminiKey) MarshalYAML() (any, error) {
	if c.RawConfig != nil {
		return c.RawConfig, nil
	}
	type plain GeminiKey
	return plain(c), nil
}

func (c VertexCompatKey) MarshalYAML() (any, error) {
	if c.RawConfig != nil {
		return c.RawConfig, nil
	}
	type plain VertexCompatKey
	return plain(c), nil
}

func (c CodexKey) MarshalYAML() (any, error) {
	if c.RawConfig != nil {
		return c.RawConfig, nil
	}
	type plain CodexKey
	return plain(c), nil
}

func (c ClaudeKey) MarshalYAML() (any, error) {
	if c.RawConfig != nil {
		return c.RawConfig, nil
	}
	type plain ClaudeKey
	return plain(c), nil
}

func (c OpenAICompatibilityAPIKey) MarshalYAML() (any, error) {
	if c.RawConfig != nil {
		return c.RawConfig, nil
	}
	type plain OpenAICompatibilityAPIKey
	return plain(c), nil
}

func (c OpenAICompatibility) MarshalYAML() (any, error) {
	if c.RawConfig == nil {
		type plain OpenAICompatibility
		return plain(c), nil
	}
	tree := make(map[string]any, len(c.RawConfig)+1)
	for key, value := range c.RawConfig {
		tree[key] = value
	}
	// Each stored auth owns only its own nested key; export assembles the group.
	if len(c.APIKeyEntries) > 0 {
		tree["api-key-entries"] = c.APIKeyEntries
	} else {
		delete(tree, "api-key-entries")
	}
	return tree, nil
}
