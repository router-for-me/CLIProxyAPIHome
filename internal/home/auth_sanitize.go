package home

import (
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// SanitizeAuthForDownstream removes refresh-capable credentials from the auth payload
// before it is returned to downstream CPA nodes.
func SanitizeAuthForDownstream(auth *coreauth.Auth) *coreauth.Auth {
	// Normalize auth state before updating runtime indexes.
	if auth == nil {
		return nil
	}

	out := auth.Clone()

	if out.Attributes != nil {
		delete(out.Attributes, "refresh_token")
		delete(out.Attributes, "service_account")
	}

	if out.Metadata != nil {
		delete(out.Metadata, "refresh_token")
		delete(out.Metadata, "service_account")
		delete(out.Metadata, homeConfigModelsMetadataKey)
		removeRefreshTokenFromNestedMap(out.Metadata, "token")
		removeRefreshTokenFromNestedMap(out.Metadata, "Token")
	}

	return out
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
