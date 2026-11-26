package synthesizer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

// addV8CredentialOptions keeps provider options lossless across the database
// credential projection and publishes the names expected by CPA execution.
func addV8CredentialOptions(entry any, attrs map[string]string, metadata map[string]any) error {
	fields, errOptions := appconfig.CredentialExecutionOptions(entry)
	if errOptions != nil {
		return fmt.Errorf("project credential execution options: %w", errOptions)
	}
	// Keep the original credential separately from the CPA execution projection.
	// It is stripped before dispatch and used directly for database exports.
	data, errMarshal := yaml.Marshal(entry)
	if errMarshal != nil {
		return fmt.Errorf("encode original credential configuration: %w", errMarshal)
	}
	var source map[string]any
	if errDecode := yaml.Unmarshal(data, &source); errDecode != nil {
		return fmt.Errorf("decode original credential configuration: %w", errDecode)
	}
	for _, field := range appconfig.HomeCredentialFields {
		delete(source, field)
	}
	if scoped, ok := entry.(interface{ V8GroupScope() map[string]any }); ok && scoped.V8GroupScope() != nil {
		metadata[appconfig.HomeCredentialGroupMetadataKey] = scoped.V8GroupScope()
	}
	switch entry.(type) {
	case appconfig.OpenAICompatibility, *appconfig.OpenAICompatibility:
		delete(source, "api-key-entries")
		metadata[appconfig.HomeCredentialConfigMetadataKey] = source
	case appconfig.OpenAICompatibilityAPIKey, *appconfig.OpenAICompatibilityAPIKey:
		group, ok := metadata[appconfig.HomeCredentialConfigMetadataKey].(map[string]any)
		if !ok || group == nil {
			return fmt.Errorf("compatibility key has no original provider configuration")
		}
		group["api-key-entries"] = []any{source}
	default:
		metadata[appconfig.HomeCredentialConfigMetadataKey] = source
	}
	options, _ := metadata["credential_options"].(map[string]any)
	if options == nil {
		options = make(map[string]any)
	}
	for key, value := range fields {
		// Home owns identity, endpoint, routing, refresh, and access policy. All
		// remaining SDK options are carried losslessly to CPA without a whitelist.
		switch key {
		case "id", "uuid", "api-key", "base-url", "proxy-url", "prefix", "priority", "headers", "excluded-models", "disable-cooling", "request-retry", "name", "disabled", "api-key-entries", "refresh-token", "refresh_token", "service-account", "service_account":
			continue
		}
		// Keep empty model configuration distinguishable from older payloads.
		if value == nil && key != "models" {
			continue
		}
		if key == "connection-pool" {
			if base, ok := options[key].(map[string]any); ok {
				if override, okOverride := value.(map[string]any); okOverride {
					merged := make(map[string]any, len(base)+len(override))
					for field, inherited := range base {
						merged[field] = inherited
					}
					for field, replacement := range override {
						merged[field] = replacement
					}
					value = merged
				}
			}
		}
		options[key] = value
		switch key {
		case "weight":
			weight, ok := value.(float64)
			if ok {
				attrs["weight"] = strconv.FormatInt(max(0, int64(weight)), 10)
			}
		case "request-scoped-errors":
			metadata["request_scoped_errors"] = value
		case "models", "connection-pool":
		case "cloak":
			if errCloak := projectClaudeCloakOptions(value, attrs, metadata); errCloak != nil {
				return errCloak
			}
		default:
			attribute := strings.ReplaceAll(key, "-", "_")
			if key == "disable-codex-cloaking" {
				attribute = "codex_disable_cloaking"
			}
			switch value.(type) {
			case string, bool, float64:
				attrs[attribute] = fmt.Sprint(value)
			}
		}
	}
	if len(options) > 0 {
		metadata["credential_options"] = options
	}
	return nil
}

func projectClaudeCloakOptions(raw any, attrs map[string]string, metadata map[string]any) error {
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return fmt.Errorf("encode Claude cloak options: %w", errMarshal)
	}
	sdkCfg := cpaconfig.Config{ClaudeKey: make([]cpaconfig.ClaudeKey, 1)}
	if errUnmarshal := json.Unmarshal(data, &sdkCfg.ClaudeKey[0].Cloak); errUnmarshal != nil {
		return fmt.Errorf("decode Claude cloak options: %w", errUnmarshal)
	}
	sdkCfg.SanitizeClaudeKeys()
	cloakCfg := sdkCfg.ClaudeKey[0].Cloak
	if cloakCfg == nil {
		return nil
	}

	if cloakCfg.Mode != "" {
		attrs["cloak_mode"] = cloakCfg.Mode
	}
	if cloakCfg.StrictMode {
		attrs["cloak_strict_mode"] = "true"
	}
	if len(cloakCfg.SensitiveWords) > 0 {
		wordsCopy := append([]string(nil), cloakCfg.SensitiveWords...)
		metadata["cloak_sensitive_words"] = wordsCopy
		hasComma := false
		for _, w := range wordsCopy {
			if strings.Contains(w, ",") {
				hasComma = true
				break
			}
		}
		if !hasComma {
			attrs["cloak_sensitive_words"] = strings.Join(wordsCopy, ",")
		}
	}
	if len(cloakCfg.SensitiveWordsReplace) > 0 {
		replBytes, errMarshalRepl := json.Marshal(cloakCfg.SensitiveWordsReplace)
		if errMarshalRepl != nil {
			return fmt.Errorf("encode Claude cloak replacements: %w", errMarshalRepl)
		}
		attrs["cloak_sensitive_words_replace"] = string(replBytes)
		copied := make(map[string]string, len(cloakCfg.SensitiveWordsReplace))
		for k, v := range cloakCfg.SensitiveWordsReplace {
			copied[k] = v
		}
		metadata["cloak_sensitive_words_replace"] = copied
	}
	if len(cloakCfg.SensitivePatternsReplace) > 0 {
		patBytes, errMarshalPat := json.Marshal(cloakCfg.SensitivePatternsReplace)
		if errMarshalPat != nil {
			return fmt.Errorf("encode Claude cloak pattern replacements: %w", errMarshalPat)
		}
		attrs["cloak_sensitive_patterns_replace"] = string(patBytes)
		copied := make([]map[string]string, 0, len(cloakCfg.SensitivePatternsReplace))
		for _, rule := range cloakCfg.SensitivePatternsReplace {
			copied = append(copied, map[string]string{
				"pattern":     rule.Pattern,
				"replacement": rule.Replacement,
			})
		}
		metadata["cloak_sensitive_patterns_replace"] = copied
	}
	if cloakCfg.CacheUserID != nil {
		attrs["cloak_cache_user_id"] = strconv.FormatBool(*cloakCfg.CacheUserID)
	}
	return nil
}
