package synthesizer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/kimi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// FileSynthesizer generates Auth entries from OAuth JSON files.
// It handles file-based authentication.
type FileSynthesizer struct{}

// NewFileSynthesizer creates a new FileSynthesizer instance.
func NewFileSynthesizer() *FileSynthesizer {
	return &FileSynthesizer{}
}

// Synthesize generates Auth entries from auth files in the auth directory.
func (s *FileSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 16)
	if ctx == nil || ctx.AuthDir == "" {
		return out, nil
	}

	entries, err := os.ReadDir(ctx.AuthDir)
	if err != nil {
		// Not an error if directory doesn't exist
		return out, nil
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		full := filepath.Join(ctx.AuthDir, name)
		data, errRead := os.ReadFile(full)
		if errRead != nil || len(data) == 0 {
			continue
		}
		auths, errSynthesize := synthesizeFileAuths(ctx, full, data)
		if errSynthesize != nil {
			return nil, errSynthesize
		}
		if len(auths) == 0 {
			continue
		}
		out = append(out, auths...)
	}
	return out, nil
}

// SynthesizeAuthFile generates Auth entries for one auth JSON file payload.
// It shares exactly the same mapping behavior as FileSynthesizer.Synthesize.
// Unsupported payloads yield no auths and a nil error; a recognized credential
// with an invalid excluded_models field yields an error naming the file.
func SynthesizeAuthFile(ctx *SynthesisContext, fullPath string, data []byte) ([]*coreauth.Auth, error) {
	return synthesizeFileAuths(ctx, fullPath, data)
}

// synthesizeFileAuths handles a synthesize file auths.
func synthesizeFileAuths(ctx *SynthesisContext, fullPath string, data []byte) ([]*coreauth.Auth, error) {
	if ctx == nil || len(data) == 0 {
		return nil, nil
	}
	now := ctx.Now
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return nil, nil
	}
	t, _ := metadata["type"].(string)
	provider := strings.ToLower(strings.TrimSpace(t))
	if ctx.PluginAuthParser != nil {
		auths, handled, errParse := parsePluginFileAuths(ctx.PluginAuthParser, pluginapi.AuthParseRequest{
			Provider: provider,
			Path:     fullPath,
			FileName: filepath.Base(fullPath),
			RawJSON:  data,
		})
		if errParse == nil && handled {
			auths = compactPluginAuths(auths)
			if len(auths) == 0 {
				return nil, nil
			}
			perAccountExcluded, errExcluded := ExtractExcludedModelsFromMetadata(metadata)
			if errExcluded != nil {
				return nil, fmt.Errorf("auth file %s: %w", fullPath, errExcluded)
			}
			for _, auth := range auths {
				auth.CreatedAt = now
				auth.UpdatedAt = now
				if auth.Attributes == nil {
					auth.Attributes = make(map[string]string)
				}
				auth.Attributes["path"] = fullPath
				auth.Attributes["source"] = fullPath
				ApplyAuthExcludedModelsMeta(auth, perAccountExcluded, "oauth")
				coreauth.ApplyCustomHeadersFromMetadata(auth)
				applyClusterUUID(ctx, auth)
			}
			return auths, nil
		}
	}
	if provider == "" {
		return nil, nil
	}
	if provider == "kimi" || provider == "kimi-ai" {
		if kimi.ResolveKimiDomain(provider, nil, metadata) == kimi.KimiAIDomain {
			provider = "kimi-ai"
		} else {
			provider = "kimi"
		}
	}
	label := provider
	if email, _ := metadata["email"].(string); email != "" {
		label = email
	}
	// Use relative path under authDir as ID to stay consistent with the file-based token store.
	id := fullPath
	if strings.TrimSpace(ctx.AuthDir) != "" {
		if rel, errRel := filepath.Rel(ctx.AuthDir, fullPath); errRel == nil && rel != "" {
			id = rel
		}
	}
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}

	proxyURL := ""
	if p, ok := metadata["proxy_url"].(string); ok {
		proxyURL = p
	}

	prefix := ""
	if rawPrefix, ok := metadata["prefix"].(string); ok {
		trimmed := strings.TrimSpace(rawPrefix)
		trimmed = strings.Trim(trimmed, "/")
		if trimmed != "" && !strings.Contains(trimmed, "/") {
			prefix = trimmed
		}
	}

	disabled, _ := metadata["disabled"].(bool)
	status := coreauth.StatusActive
	if disabled {
		status = coreauth.StatusDisabled
	}

	// Read per-account excluded models from the OAuth JSON file.
	perAccountExcluded, errExcluded := ExtractExcludedModelsFromMetadata(metadata)
	if errExcluded != nil {
		return nil, fmt.Errorf("auth file %s: %w", fullPath, errExcluded)
	}

	a := &coreauth.Auth{
		ID:       id,
		Provider: provider,
		Label:    label,
		Prefix:   prefix,
		Status:   status,
		Disabled: disabled,
		Attributes: map[string]string{
			"source": fullPath,
			"path":   fullPath,
		},
		ProxyURL:  proxyURL,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Read priority from auth file.
	if rawPriority, ok := metadata["priority"]; ok {
		switch v := rawPriority.(type) {
		case float64:
			a.Attributes["priority"] = strconv.Itoa(int(v))
		case string:
			priority := strings.TrimSpace(v)
			if _, errAtoi := strconv.Atoi(priority); errAtoi == nil {
				a.Attributes["priority"] = priority
			}
		}
	}
	// Read note from auth file.
	if rawNote, ok := metadata["note"]; ok {
		if note, isStr := rawNote.(string); isStr {
			if trimmed := strings.TrimSpace(note); trimmed != "" {
				a.Attributes["note"] = trimmed
			}
		}
	}
	coreauth.ApplyCustomHeadersFromMetadata(a)
	ApplyAuthExcludedModelsMeta(a, perAccountExcluded, "oauth")
	// For codex auth files, extract plan_type from the JWT id_token.
	if provider == "codex" {
		if idTokenRaw, ok := metadata["id_token"].(string); ok && strings.TrimSpace(idTokenRaw) != "" {
			if claims, errParse := codex.ParseJWTToken(idTokenRaw); errParse == nil && claims != nil {
				if pt := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); pt != "" {
					a.Attributes["plan_type"] = pt
				}
			}
		}
	}
	applyClusterUUID(ctx, a)
	return []*coreauth.Auth{a}, nil
}

func parsePluginFileAuths(parser PluginAuthParser, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
	if parser == nil {
		return nil, false, nil
	}
	if multiParser, ok := parser.(PluginMultiAuthParser); ok {
		return multiParser.ParseAuths(context.Background(), req)
	}
	auth, handled, errParse := parser.ParseAuth(context.Background(), req)
	if errParse != nil || !handled || auth == nil {
		return nil, handled, errParse
	}
	return []*coreauth.Auth{auth}, true, nil
}

func compactPluginAuths(auths []*coreauth.Auth) []*coreauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := auths[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// ExtractExcludedModelsFromMetadata reads per-account excluded models from the OAuth JSON metadata.
// Supports both "excluded_models" and "excluded-models" keys. The value must be a string array;
// a single string is also accepted for older hand-written files and is split on commas, because
// model IDs never contain commas. Any other type, or a non-string array element, is an error.
func ExtractExcludedModelsFromMetadata(metadata map[string]any) ([]string, error) {
	if metadata == nil {
		return nil, nil
	}
	key := "excluded_models"
	raw, ok := metadata[key]
	if !ok {
		key = "excluded-models"
		raw, ok = metadata[key]
	}
	if !ok || raw == nil {
		return nil, nil
	}
	var stringSlice []string
	switch v := raw.(type) {
	case string:
		stringSlice = strings.Split(v, ",")
	case []string:
		stringSlice = v
	case []interface{}:
		stringSlice = make([]string, 0, len(v))
		for index, item := range v {
			s, okString := item.(string)
			if !okString {
				return nil, fmt.Errorf("metadata %s[%d] must be a string, got %T", key, index, item)
			}
			stringSlice = append(stringSlice, s)
		}
	default:
		return nil, fmt.Errorf("metadata %s must be a string array, got %T", key, raw)
	}
	result := make([]string, 0, len(stringSlice))
	for _, s := range stringSlice {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result, nil
}
