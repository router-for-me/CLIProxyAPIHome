package synthesizer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// addV8CredentialOptions keeps provider options lossless across the database
// credential projection and publishes the names expected by CPA execution.
func addV8CredentialOptions(entry any, attrs map[string]string, metadata map[string]any) {
	data, errMarshal := json.Marshal(entry)
	if errMarshal != nil {
		return
	}
	var fields map[string]any
	if errDecode := json.Unmarshal(data, &fields); errDecode != nil {
		return
	}
	options, _ := metadata["credential_options"].(map[string]any)
	if options == nil {
		options = make(map[string]any)
	}
	for _, key := range []string{"weight", "disable-codex-cloaking", "request-scoped-errors", "rebuild-mid-system-message", "fingerprint-profile", "native-tool-search", "support-prompt-cache-key", "models", "cloak"} {
		value, exists := fields[key]
		// Keep empty model configuration distinguishable from older payloads.
		if !exists || (value == nil && key != "models") {
			continue
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
		case "models":
		case "cloak":
			projectClaudeCloakOptions(value, attrs, metadata)
		default:
			attribute := strings.ReplaceAll(key, "-", "_")
			if key == "disable-codex-cloaking" {
				attribute = "codex_disable_cloaking"
			}
			attrs[attribute] = fmt.Sprint(value)
		}
	}
	if len(options) > 0 {
		metadata["credential_options"] = options
	}
}

func projectClaudeCloakOptions(raw any, attrs map[string]string, metadata map[string]any) {
	rawBytes, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return
	}
	var cloakCfg appconfig.CloakConfig
	if errUnmarshal := json.Unmarshal(rawBytes, &cloakCfg); errUnmarshal != nil {
		return
	}
	appconfig.NormalizeCloakConfig(&cloakCfg)

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
		if replBytes, errMarshalRepl := json.Marshal(cloakCfg.SensitiveWordsReplace); errMarshalRepl == nil {
			attrs["cloak_sensitive_words_replace"] = string(replBytes)
		}
		copied := make(map[string]string, len(cloakCfg.SensitiveWordsReplace))
		for k, v := range cloakCfg.SensitiveWordsReplace {
			copied[k] = v
		}
		metadata["cloak_sensitive_words_replace"] = copied
	}
	if cloakCfg.CacheUserID != nil {
		attrs["cloak_cache_user_id"] = strconv.FormatBool(*cloakCfg.CacheUserID)
	}
}
