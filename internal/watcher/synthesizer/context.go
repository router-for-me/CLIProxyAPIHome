package synthesizer

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// PluginAuthParser lets provider plugins parse auth file payloads.
type PluginAuthParser interface {
	ParseAuth(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error)
}

// PluginMultiAuthParser expands one auth file payload into multiple plugin auth records.
type PluginMultiAuthParser interface {
	ParseAuths(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error)
}

// SynthesisContext provides the context needed for auth synthesis.
type SynthesisContext struct {
	// Config is the current configuration
	Config *config.Config
	// AuthDir is the directory containing auth files
	AuthDir string
	// Now is the current time for timestamps
	Now time.Time
	// IDGenerator generates stable IDs for auth entries
	IDGenerator *StableIDGenerator
	// ClusterMode enables cluster UUID normalization for synthesized auth entries.
	ClusterMode bool
	// UUIDForAuth returns the cluster UUID for an auth entry.
	UUIDForAuth func(auth *coreauth.Auth) string
	// PluginAuthParser parses provider-specific plugin auth files.
	PluginAuthParser PluginAuthParser
}

// applyClusterUUID applies a cluster uuid.
func applyProviderCredentialID(ctx *SynthesisContext, auth *coreauth.Auth, explicitID string) {
	if auth == nil {
		return
	}
	credentialID := strings.TrimSpace(explicitID)
	if credentialID == "" && ctx != nil && ctx.UUIDForAuth != nil {
		credentialID = strings.TrimSpace(ctx.UUIDForAuth(auth))
	}
	generated := credentialID == ""
	if generated {
		credentialID = uuid.NewString()
	}
	auth.ID = credentialID
	auth.Index = credentialID
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["cluster_uuid"] = credentialID
	if generated {
		auth.Attributes["provider_credential_id_generated"] = "true"
	}
}

// applyClusterUUID applies a cluster uuid.
func applyClusterUUID(ctx *SynthesisContext, auth *coreauth.Auth) {
	if ctx == nil || !ctx.ClusterMode || ctx.UUIDForAuth == nil || auth == nil {
		return
	}
	uuidValue := strings.TrimSpace(ctx.UUIDForAuth(auth))
	if uuidValue == "" {
		return
	}
	auth.ID = uuidValue
	auth.Index = uuidValue
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["cluster_uuid"] = uuidValue
	if auth.Metadata != nil {
		auth.Metadata["uuid"] = uuidValue
	}
}
