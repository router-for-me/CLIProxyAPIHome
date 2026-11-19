package home

import (
	"encoding/json"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

type vertexOpenAPIRule struct {
	Name    string `json:"name"`
	Alias   string `json:"alias"`
	OpenAPI *bool  `json:"openapi"`
}

// SanitizeAuthForDownstream removes refresh-capable credentials and projects
// execution-only settings without forwarding Home's alias resolution table.
func SanitizeAuthForDownstream(auth *coreauth.Auth, cfg *config.Config) (*coreauth.Auth, error) {
	// Normalize auth state before updating runtime indexes.
	if auth == nil {
		return nil, nil
	}

	out := auth.Clone()
	out.LastRefreshError = nil
	if raw := out.Metadata["credential_options"]; raw != nil {
		data, errMarshal := json.Marshal(raw)
		if errMarshal != nil {
			return nil, fmt.Errorf("encode credential execution options: %w", errMarshal)
		}
		var options map[string]json.RawMessage
		if errDecode := json.Unmarshal(data, &options); errDecode != nil {
			return nil, fmt.Errorf("decode credential execution options: %w", errDecode)
		}
		if pool, exists := options["connection-pool"]; exists {
			if errDecode := json.Unmarshal(pool, &out.ProxyConnectionPool); errDecode != nil {
				return nil, fmt.Errorf("decode credential connection pool: %w", errDecode)
			}
		}
		// CPA's Vertex router reads this attribute, not credential_options.models.
		// Project at dispatch time so existing DB credentials need no reimport.
		if models, configured := options["models"]; configured && strings.EqualFold(strings.TrimSpace(out.Provider), "vertex") {
			var entries []vertexOpenAPIRule
			if errDecode := json.Unmarshal(models, &entries); errDecode != nil {
				return nil, fmt.Errorf("decode Vertex model routing: %w", errDecode)
			}
			var rules []vertexOpenAPIRule
			for _, entry := range entries {
				if entry.OpenAPI != nil {
					rules = append(rules, entry)
				}
			}
			delete(out.Attributes, "vertex_model_openapi")
			if len(rules) > 0 {
				encoded, errEncode := json.Marshal(rules)
				if errEncode != nil {
					return nil, fmt.Errorf("encode Vertex model routing: %w", errEncode)
				}
				// Clone shares empty maps; allocate before adding derived fields.
				if len(out.Attributes) == 0 {
					out.Attributes = make(map[string]string)
				}
				out.Attributes["vertex_model_openapi"] = string(encoded)
			}
		}
	}
	if cfg != nil && coreauth.OAuthModelAliasChannel(out.Provider, out.AuthKind()) == "vertex" {
		var rules []vertexOpenAPIRule
		for _, alias := range cfg.OAuthModelAlias["vertex"] {
			if alias.OpenAPI != nil {
				rules = append(rules, vertexOpenAPIRule{Name: alias.Name, Alias: alias.Alias, OpenAPI: alias.OpenAPI})
			}
		}
		if len(rules) > 0 {
			// Credential-specific rules keep precedence over global OAuth settings.
			if raw := strings.TrimSpace(out.Attributes["vertex_model_openapi"]); raw != "" {
				var credentialRules []vertexOpenAPIRule
				if errDecode := json.Unmarshal([]byte(raw), &credentialRules); errDecode != nil {
					return nil, fmt.Errorf("decode Vertex credential routing: %w", errDecode)
				}
				rules = append(credentialRules, rules...)
			}
			encoded, errEncode := json.Marshal(rules)
			if errEncode != nil {
				return nil, fmt.Errorf("encode Vertex OAuth routing: %w", errEncode)
			}
			if len(out.Attributes) == 0 {
				out.Attributes = make(map[string]string)
			}
			out.Attributes["vertex_model_openapi"] = string(encoded)
		}
	}
	if out.LastError != nil {
		switch strings.ToLower(strings.TrimSpace(out.LastError.Code)) {
		case "refresh_temporarily_unavailable", "refresh_unsupported", coreauth.ErrorCodeRefreshOutcomeUnknown:
			out.LastError = nil
		}
	}

	if out.Attributes != nil {
		delete(out.Attributes, "refresh_token")
		delete(out.Attributes, "service_account")
		delete(out.Attributes, "__home_refresh_lease")
	}

	if out.Metadata != nil {
		delete(out.Metadata, "refresh_token")
		delete(out.Metadata, "service_account")
		delete(out.Metadata, homeConfigModelsMetadataKey)
		delete(out.Metadata, config.HomeCredentialConfigMetadataKey)
		delete(out.Metadata, config.HomeCredentialGroupMetadataKey)
		removeRefreshTokenFromNestedMap(out.Metadata, "token")
		removeRefreshTokenFromNestedMap(out.Metadata, "Token")
	}

	return out, nil
}

// removeRefreshTokenFromNestedMap converts remove refresh token from nested map.
func removeRefreshTokenFromNestedMap(meta map[string]any, key string) {
	// Resolve credential context before calling upstream OAuth services.
	if meta == nil {
		return
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return
	}

	switch typed := raw.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for k, v := range typed {
			cloned[k] = v
		}
		delete(cloned, "refresh_token")
		delete(cloned, "refreshToken")
		meta[key] = cloned
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for k, v := range typed {
			cloned[k] = v
		}
		delete(cloned, "refresh_token")
		delete(cloned, "refreshToken")
		meta[key] = cloned
	default:
		if s, okString := raw.(string); okString && strings.TrimSpace(s) == "" {
			return
		}
	}
}
