package auth

import "strings"

const (
	// CredentialPolicyMetadataKey stores the dispatch credential policy in Options.Metadata.
	CredentialPolicyMetadataKey = "credential_policy"
	// CredentialPolicyCodexAlphaSearchV1 selects credentials supported by Codex Alpha Search.
	CredentialPolicyCodexAlphaSearchV1 = "codex_alpha_search_v1"

	// AttributeCodexAlphaSearch marks an opted-in Codex API key.
	AttributeCodexAlphaSearch = "codex_alpha_search"

	AuthKindAPIKey = "apikey"
	AuthKindOAuth  = "oauth"
)

// NormalizeCredentialPolicy validates and canonicalizes a dispatch credential policy.
func NormalizeCredentialPolicy(policy string) (string, bool) {
	policy = strings.TrimSpace(policy)
	switch policy {
	case "", CredentialPolicyCodexAlphaSearchV1:
		return policy, true
	default:
		return policy, false
	}
}

// AuthKind returns the explicit credential kind or infers it from the credential shape.
func (a *Auth) AuthKind() string {
	if a == nil {
		return ""
	}
	if kind := normalizeCredentialAuthKind(authAttributeValue(a, "auth_kind")); kind != "" {
		return kind
	}
	if kind := normalizeCredentialAuthKind(authMetadataStringValue(a, "auth_kind")); kind != "" {
		return kind
	}
	if authAttributeValue(a, "api_key") != "" {
		return AuthKindAPIKey
	}
	if authHasOAuthMetadata(a) {
		return AuthKindOAuth
	}
	return ""
}

func credentialPolicyFromOptions(opts Options) string {
	if opts.Metadata == nil {
		return ""
	}
	policy, _ := opts.Metadata[CredentialPolicyMetadataKey].(string)
	return strings.TrimSpace(policy)
}

func credentialPolicyAllows(policy string, auth *Auth) bool {
	if policy == "" {
		return true
	}
	if policy != CredentialPolicyCodexAlphaSearchV1 || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	switch auth.AuthKind() {
	case AuthKindOAuth:
		return true
	case AuthKindAPIKey:
		return strings.EqualFold(authAttributeValue(auth, AttributeCodexAlphaSearch), "true")
	default:
		return false
	}
}

func normalizeCredentialAuthKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case AuthKindAPIKey, "api_key", "api-key":
		return AuthKindAPIKey
	case AuthKindOAuth, "oauth2":
		return AuthKindOAuth
	default:
		return ""
	}
}

func authHasOAuthMetadata(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "email", "token_type", "expires_at", "expired"} {
		if authMetadataStringValue(auth, key) != "" {
			return true
		}
	}
	if token, ok := auth.Metadata["token"].(map[string]any); ok && len(token) > 0 {
		return true
	}
	return false
}

func authAttributeValue(auth *Auth, key string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes[key])
}

func authMetadataStringValue(auth *Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}
