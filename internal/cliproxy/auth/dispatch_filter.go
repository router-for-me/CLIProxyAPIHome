package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
)

func allowedAuthIDsFromOptions(opts Options) map[string]struct{} {
	if opts.Metadata == nil {
		return nil
	}
	raw, ok := opts.Metadata[AllowedAuthIDsMetadataKey]
	if !ok {
		return nil
	}

	allowed := make(map[string]struct{})
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			authID := strings.TrimSpace(value)
			if authID != "" {
				allowed[authID] = struct{}{}
			}
		}
	case []any:
		for _, value := range values {
			authID := strings.TrimSpace(toString(value))
			if authID != "" {
				allowed[authID] = struct{}{}
			}
		}
	case map[string]struct{}:
		for value := range values {
			authID := strings.TrimSpace(value)
			if authID != "" {
				allowed[authID] = struct{}{}
			}
		}
	case map[string]bool:
		for value, enabled := range values {
			if !enabled {
				continue
			}
			authID := strings.TrimSpace(value)
			if authID != "" {
				allowed[authID] = struct{}{}
			}
		}
	}
	return allowed
}

func allowedModelIDsFromOptions(opts Options) map[string]struct{} {
	if opts.Metadata == nil {
		return nil
	}
	raw, ok := opts.Metadata[AllowedModelIDsMetadataKey]
	if !ok {
		return nil
	}

	allowed := make(map[string]struct{})
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			modelID := normalizedAllowedModelID(value)
			if modelID != "" {
				allowed[modelID] = struct{}{}
			}
		}
	case []any:
		for _, value := range values {
			modelID := normalizedAllowedModelID(toString(value))
			if modelID != "" {
				allowed[modelID] = struct{}{}
			}
		}
	case map[string]struct{}:
		for value := range values {
			modelID := normalizedAllowedModelID(value)
			if modelID != "" {
				allowed[modelID] = struct{}{}
			}
		}
	case map[string]bool:
		for value, enabled := range values {
			if !enabled {
				continue
			}
			modelID := normalizedAllowedModelID(value)
			if modelID != "" {
				allowed[modelID] = struct{}{}
			}
		}
	}
	return allowed
}

func excludedConcurrencyCandidatesFromOptions(opts Options) []ExcludedConcurrencyCandidate {
	if opts.Metadata == nil {
		return nil
	}
	raw, ok := opts.Metadata[ExcludedConcurrencyCandidatesMetadataKey]
	if !ok {
		return nil
	}
	candidates, ok := raw.([]ExcludedConcurrencyCandidate)
	if !ok {
		return nil
	}
	return candidates
}

func concurrencyCandidateExcluded(opts Options, credentialID string, model string) bool {
	credentialID = strings.TrimSpace(credentialID)
	model, validModel := concurrency.ValidCanonicalConcurrencyModelKey(model)
	if !validModel {
		return false
	}
	for _, candidate := range excludedConcurrencyCandidatesFromOptions(opts) {
		if strings.TrimSpace(candidate.CredentialID) != credentialID {
			continue
		}
		rawModel := strings.TrimSpace(candidate.Model)
		if rawModel == "" {
			return true
		}
		excludedModel, validExcludedModel := concurrency.ValidCanonicalConcurrencyModelKey(rawModel)
		if validExcludedModel && excludedModel == model {
			return true
		}
	}
	return false
}

func authAllowedByID(authID string, allowed map[string]struct{}) bool {
	if allowed == nil {
		return true
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	_, ok := allowed[authID]
	return ok
}

func modelAllowedByID(modelID string, allowed map[string]struct{}) bool {
	if allowed == nil {
		return true
	}
	modelID = normalizedAllowedModelID(modelID)
	if modelID == "" {
		return false
	}
	_, ok := allowed[modelID]
	return ok
}

func normalizedAllowedModelID(modelID string) string {
	modelID = canonicalModelKey(modelID)
	if modelID == "" {
		return ""
	}
	return strings.ToLower(modelID)
}

func schedulerPredicate(tried map[string]struct{}, allowed map[string]struct{}, credentialPolicy string) func(*scheduledAuth) bool {
	return func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if len(tried) > 0 {
			if _, ok := tried[entry.auth.ID]; ok {
				return false
			}
		}
		return authAllowedByID(entry.auth.ID, allowed) && credentialPolicyAllows(credentialPolicy, entry.auth)
	}
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}
