package home

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	homeauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func newPluginHostForRuntime(cfg *config.Config) *sdkpluginhost.Host {
	_ = cfg
	return sdkpluginhost.New()
}

func (r *Runtime) applyPluginConfig(ctx context.Context, cfg *config.Config) {
	if r == nil || r.pluginHost == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.pluginHost.ApplyConfig(ctx, pluginRuntimeConfig(cfg))
}

// HasScheduler reports whether a Home-loaded plugin supplies auth scheduling.
func (r *Runtime) HasScheduler() bool {
	return r != nil && r.pluginHost != nil && r.pluginHost.HasScheduler()
}

// ParseAuth lets Home auth synthesis use provider plugins.
func (r *Runtime) ParseAuth(ctx context.Context, req pluginapi.AuthParseRequest) (*homeauth.Auth, bool, error) {
	if r == nil || r.pluginHost == nil {
		return nil, false, nil
	}
	auth, handled, errParse := r.pluginHost.ParseAuth(ctx, req)
	if auth == nil {
		return nil, handled, errParse
	}
	return pluginAuthToHomeAuth(auth), handled, errParse
}

// ParseAuths lets Home auth synthesis use provider plugins that expand one file into multiple auths.
func (r *Runtime) ParseAuths(ctx context.Context, req pluginapi.AuthParseRequest) ([]*homeauth.Auth, bool, error) {
	if r == nil || r.pluginHost == nil {
		return nil, false, nil
	}
	auths, handled, errParse := r.pluginHost.ParseAuths(ctx, req)
	if errParse != nil || !handled {
		return nil, handled, errParse
	}
	return pluginAuthsToHomeAuths(auths), handled, nil
}

// RefreshAuth lets Home refresh provider plugin credentials.
func (r *Runtime) RefreshAuth(ctx context.Context, auth *homeauth.Auth) (*homeauth.Auth, bool, error) {
	if r == nil || r.pluginHost == nil || auth == nil {
		return nil, false, nil
	}
	refreshed, handled, errRefresh := r.pluginHost.RefreshAuth(ctx, homeAuthToPluginAuth(auth))
	if refreshed == nil {
		return nil, handled, errRefresh
	}
	next := pluginAuthToHomeAuth(refreshed)
	if next == nil {
		return nil, handled, errRefresh
	}
	if next.Runtime == nil {
		next.Runtime = auth.Runtime
	}
	return next, handled, errRefresh
}

// PickAuth lets Home dispatch use scheduler plugins.
func (r *Runtime) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	if r == nil || r.pluginHost == nil {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	return r.pluginHost.PickAuth(ctx, req)
}

// RegisteredPlugins returns Home-loaded plugin metadata for management clients.
func (r *Runtime) RegisteredPlugins() []sdkpluginhost.RegisteredPluginInfo {
	if r == nil || r.pluginHost == nil {
		return nil
	}
	return r.pluginHost.RegisteredPlugins()
}

// HasPluginAuthProvider reports whether a Home-loaded plugin owns provider login.
func (r *Runtime) HasPluginAuthProvider(provider string) bool {
	return r != nil && r.pluginHost != nil && r.pluginHost.HasAuthProvider(provider)
}

// StartPluginLogin starts an OAuth flow through a Home-loaded auth provider plugin.
func (r *Runtime) StartPluginLogin(ctx context.Context, provider, baseURL string) (pluginapi.AuthLoginStartResponse, bool, error) {
	if r == nil || r.pluginHost == nil {
		return pluginapi.AuthLoginStartResponse{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return r.pluginHost.StartLogin(ctx, provider, baseURL)
}

// PollPluginLogin polls an OAuth flow through a Home-loaded auth provider plugin.
func (r *Runtime) PollPluginLogin(ctx context.Context, provider, state string, metadata map[string]any) (pluginapi.AuthLoginPollResponse, bool, error) {
	if r == nil || r.pluginHost == nil {
		return pluginapi.AuthLoginPollResponse{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return r.pluginHost.PollLogin(ctx, provider, state, metadata)
}

// PluginLoginAuths converts auth records returned by a successful plugin login.
func (r *Runtime) PluginLoginAuths(resp pluginapi.AuthLoginPollResponse) ([]*homeauth.Auth, error) {
	if r == nil || r.pluginHost == nil {
		return nil, fmt.Errorf("plugin host is not available")
	}
	auths, errAuths := r.pluginLoginPollAuths(resp)
	if errAuths != nil {
		return nil, errAuths
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("plugin login returned no auth records")
	}
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			return nil, fmt.Errorf("plugin login returned auth without id")
		}
	}
	return auths, nil
}

func (r *Runtime) pluginLoginPollAuths(resp pluginapi.AuthLoginPollResponse) ([]*homeauth.Auth, error) {
	if r == nil || r.pluginHost == nil {
		return nil, fmt.Errorf("plugin host is not available")
	}
	authDatas := resp.Auths
	if len(authDatas) == 0 {
		authDatas = []pluginapi.AuthData{resp.Auth}
	}
	auths := make([]*homeauth.Auth, 0, len(authDatas))
	for _, authData := range authDatas {
		coreAuth := r.pluginHost.AuthDataToCoreAuth(authData, "", "")
		auth := pluginAuthToHomeAuth(coreAuth)
		if auth == nil {
			return nil, fmt.Errorf("plugin login returned invalid auth data")
		}
		auths = append(auths, auth)
	}
	return auths, nil
}

func (r *Runtime) tryRegisterPluginModelsForAuth(ctx context.Context, auth *homeauth.Auth, provider, authKind string, excluded []string) bool {
	if r == nil || r.pluginHost == nil || auth == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := r.pluginHost.ModelsForAuth(ctx, homeAuthToPluginAuth(auth))
	if !result.Handled {
		return false
	}
	if result.Err != nil {
		log.Warnf("plugin models for auth %s failed: %v", auth.ID, result.Err)
		return true
	}

	activeAuth := auth
	providerKey := strings.ToLower(strings.TrimSpace(result.Provider))
	if providerKey == "" {
		providerKey = strings.ToLower(strings.TrimSpace(provider))
	}
	if result.Auth != nil && r.coreManager != nil {
		updatedAuth := pluginAuthToHomeAuth(result.Auth)
		if updatedAuth != nil {
			updatedAuth.ID = auth.ID
			if updatedAuth.Provider == "" {
				updatedAuth.Provider = auth.Provider
			}
			if updatedAuth.FileName == "" {
				updatedAuth.FileName = auth.FileName
			}
			if updatedAuth.Attributes == nil {
				updatedAuth.Attributes = make(map[string]string)
			}
			for key, value := range auth.Attributes {
				if _, exists := updatedAuth.Attributes[key]; !exists {
					updatedAuth.Attributes[key] = value
				}
			}
			if updated, errUpdate := r.coreManager.Update(context.Background(), updatedAuth); errUpdate == nil && updated != nil {
				activeAuth = updated.Clone()
			}
		}
	}
	if activeAuth == nil {
		activeAuth = auth
	}
	if activeProvider := strings.ToLower(strings.TrimSpace(activeAuth.Provider)); activeProvider != "" {
		providerKey = activeProvider
	}
	if providerKey == "" {
		providerKey = strings.ToLower(strings.TrimSpace(provider))
	}
	activeAuthKind := strings.ToLower(strings.TrimSpace(activeAuth.Attributes["auth_kind"]))
	if activeAuthKind == "" {
		if kind, _ := activeAuth.AccountInfo(); strings.EqualFold(kind, "api_key") {
			activeAuthKind = "apikey"
		}
	}

	r.cfgMu.RLock()
	cfg := r.cfg
	r.cfgMu.RUnlock()
	activeExcluded := r.oauthExcludedModels(cfg, providerKey, activeAuthKind)
	if auth == activeAuth && len(activeExcluded) == 0 {
		activeExcluded = excluded
	}
	// Credential-level exclusions take priority over the global oauth-excluded-models config.
	if activeAuth.Attributes != nil {
		if val, ok := activeAuth.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			activeExcluded = strings.Split(val, ",")
		}
	}

	models := pluginModelsToHomeModels(result.Models)
	models = applyExcludedModels(models, activeExcluded)
	models = applyOAuthModelAlias(cfg, providerKey, activeAuthKind, models)
	if len(models) > 0 {
		forcePrefix := cfg != nil && cfg.ForceModelPrefix
		r.registerResolvedModelsForAuth(activeAuth, providerKey, applyModelPrefixes(models, activeAuth.Prefix, forcePrefix))
		return true
	}
	registry.GetGlobalRegistry().UnregisterClient(activeAuth.ID)
	return true
}

func (r *Runtime) pluginModelsForProvider(providerKey string) []*ModelInfo {
	if r == nil || r.pluginHost == nil {
		return nil
	}
	return pluginModelsToHomeModels(r.pluginHost.ModelsForProvider(providerKey))
}

func (r *Runtime) appendPluginModels(providerKey string, models []*ModelInfo) []*ModelInfo {
	pluginModels := r.pluginModelsForProvider(providerKey)
	if len(pluginModels) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models)+len(pluginModels))
	seen := make(map[string]struct{}, len(models)+len(pluginModels))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID != "" {
			seen[modelID] = struct{}{}
		}
		out = append(out, model)
	}
	for _, model := range pluginModels {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		out = append(out, model)
	}
	return out
}

func pluginRuntimeConfig(cfg *config.Config) sdkpluginhost.RuntimeConfig {
	if cfg == nil {
		return sdkpluginhost.RuntimeConfig{}
	}
	return sdkpluginhost.RuntimeConfig{
		Enabled:             cfg.Plugins.Enabled,
		Dir:                 pluginRootFromConfigDir(cfg.Plugins.Dir),
		AuthDir:             cfg.AuthDir,
		ProxyURL:            cfg.ProxyURL,
		ForceModelPrefix:    cfg.ForceModelPrefix,
		OAuthExcludedModels: cloneStringSliceMap(cfg.OAuthExcludedModels),
		OAuthModelAlias:     pluginOAuthModelAlias(cfg.OAuthModelAlias),
		Configs:             pluginInstanceConfigs(cfg.Plugins.Configs),
	}
}

func pluginInstanceConfigs(in map[string]config.PluginInstanceConfig) map[string]sdkpluginhost.PluginInstanceConfig {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]sdkpluginhost.PluginInstanceConfig, len(in))
	for id, item := range in {
		if !pluginInstanceLoadsInHome(id, item) {
			continue
		}
		out[id] = sdkpluginhost.PluginInstanceConfig{
			Enabled:  item.Enabled,
			Priority: item.Priority,
			Raw:      item.Raw,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func pluginInstanceLoadsInHome(id string, item config.PluginInstanceConfig) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if item.Raw.Kind == 0 || yamlMappingValue(&item.Raw, "store") == nil {
		return true
	}
	return pluginConfigHomeLoadEnabled(item)
}

func pluginConfigHomeLoadEnabled(item config.PluginInstanceConfig) bool {
	if boolFromYAMLNode(yamlMappingValue(&item.Raw, "load-in-home")) {
		return true
	}
	homeNode := yamlMappingValue(&item.Raw, "home")
	if boolFromYAMLNode(homeNode) {
		return true
	}
	if homeNode != nil && homeNode.Kind == yaml.MappingNode {
		return boolFromYAMLNode(yamlMappingValue(homeNode, "enabled")) || boolFromYAMLNode(yamlMappingValue(homeNode, "load"))
	}
	return false
}

func boolFromYAMLNode(node *yaml.Node) bool {
	if node == nil || node.Kind == 0 {
		return false
	}
	var value bool
	if errDecode := node.Decode(&value); errDecode != nil {
		return false
	}
	return value
}

func pluginOAuthModelAlias(in map[string][]config.OAuthModelAlias) map[string][]sdkpluginhost.OAuthModelAlias {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]sdkpluginhost.OAuthModelAlias, len(in))
	for provider, aliases := range in {
		items := make([]sdkpluginhost.OAuthModelAlias, 0, len(aliases))
		for _, alias := range aliases {
			items = append(items, sdkpluginhost.OAuthModelAlias{
				Name:  alias.Name,
				Alias: alias.Alias,
				Fork:  alias.Fork,
			})
		}
		out[provider] = items
	}
	return out
}

func homeAuthToPluginAuth(auth *homeauth.Auth) *cpaauth.Auth {
	if auth == nil {
		return nil
	}
	return &cpaauth.Auth{
		ID:               auth.ID,
		Index:            auth.Index,
		Provider:         auth.Provider,
		Prefix:           auth.Prefix,
		FileName:         auth.FileName,
		Label:            auth.Label,
		Status:           cpaauth.Status(auth.Status),
		StatusMessage:    auth.StatusMessage,
		Disabled:         auth.Disabled,
		Unavailable:      auth.Unavailable,
		ProxyURL:         auth.ProxyURL,
		Attributes:       cloneStringMap(auth.Attributes),
		Metadata:         cloneAnyMap(auth.Metadata),
		CreatedAt:        auth.CreatedAt,
		UpdatedAt:        auth.UpdatedAt,
		LastRefreshedAt:  auth.LastRefreshedAt,
		NextRefreshAfter: auth.NextRefreshAfter,
		NextRetryAfter:   auth.NextRetryAfter,
		Runtime:          auth.Runtime,
		Success:          auth.Success,
		Failed:           auth.Failed,
	}
}

func pluginAuthToHomeAuth(auth *cpaauth.Auth) *homeauth.Auth {
	if auth == nil {
		return nil
	}
	metadata := metadataFromPluginAuth(auth)
	return &homeauth.Auth{
		ID:               auth.ID,
		Index:            auth.Index,
		Provider:         auth.Provider,
		Prefix:           auth.Prefix,
		FileName:         auth.FileName,
		Label:            auth.Label,
		Status:           homeauth.Status(auth.Status),
		StatusMessage:    auth.StatusMessage,
		Disabled:         auth.Disabled,
		Unavailable:      auth.Unavailable,
		ProxyURL:         auth.ProxyURL,
		Attributes:       cloneStringMap(auth.Attributes),
		Metadata:         metadata,
		CreatedAt:        auth.CreatedAt,
		UpdatedAt:        auth.UpdatedAt,
		LastRefreshedAt:  auth.LastRefreshedAt,
		NextRefreshAfter: auth.NextRefreshAfter,
		NextRetryAfter:   auth.NextRetryAfter,
		Runtime:          auth.Runtime,
		Success:          auth.Success,
		Failed:           auth.Failed,
	}
}

func pluginAuthsToHomeAuths(auths []*cpaauth.Auth) []*homeauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := make([]*homeauth.Auth, 0, len(auths))
	for _, auth := range auths {
		converted := pluginAuthToHomeAuth(auth)
		if converted == nil {
			continue
		}
		out = append(out, converted)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func metadataFromPluginAuth(auth *cpaauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	out := map[string]any{}
	if rawProvider, ok := auth.Storage.(interface{ RawJSON() []byte }); ok {
		if raw := rawProvider.RawJSON(); len(raw) > 0 {
			var decoded map[string]any
			if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal == nil {
				for key, value := range decoded {
					out[key] = value
				}
			}
		}
	}
	for key, value := range auth.Metadata {
		out[key] = value
	}
	if provider := strings.TrimSpace(auth.Provider); provider != "" {
		out["type"] = provider
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func pluginModelsToHomeModels(models []sdkpluginhost.ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		out = append(out, pluginModelToHomeModel(model))
	}
	return out
}

func pluginModelToHomeModel(model sdkpluginhost.ModelInfo) *ModelInfo {
	return &ModelInfo{
		ID:                         model.ID,
		Object:                     model.Object,
		Created:                    model.Created,
		OwnedBy:                    model.OwnedBy,
		Type:                       model.Type,
		DisplayName:                model.DisplayName,
		Name:                       model.Name,
		Version:                    model.Version,
		Description:                model.Description,
		InputTokenLimit:            int(model.InputTokenLimit),
		OutputTokenLimit:           int(model.OutputTokenLimit),
		SupportedGenerationMethods: cloneStringSlice(model.SupportedGenerationMethods),
		ContextLength:              int(model.ContextLength),
		MaxCompletionTokens:        int(model.MaxCompletionTokens),
		SupportedParameters:        cloneStringSlice(model.SupportedParameters),
		SupportedInputModalities:   cloneStringSlice(model.SupportedInputModalities),
		SupportedOutputModalities:  cloneStringSlice(model.SupportedOutputModalities),
		Thinking:                   pluginThinkingToHomeThinking(model.Thinking),
		UserDefined:                model.UserDefined,
	}
}

func pluginThinkingToHomeThinking(thinking *sdkpluginhost.ThinkingSupport) *registry.ThinkingSupport {
	if thinking == nil {
		return nil
	}
	return &registry.ThinkingSupport{
		Min:            thinking.Min,
		Max:            thinking.Max,
		ZeroAllowed:    thinking.ZeroAllowed,
		DynamicAllowed: thinking.DynamicAllowed,
		Levels:         cloneStringSlice(thinking.Levels),
	}
}

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}

func cloneStringSliceMap(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for key, values := range in {
		out[key] = cloneStringSlice(values)
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// PluginHost exposes the Home-owned host to management operations. Credentials
// supplied to it are loaded from the Home repository, not an SDK file store.
func (r *Runtime) PluginHost() *sdkpluginhost.Host {
	if r == nil {
		return nil
	}
	return r.pluginHost
}
