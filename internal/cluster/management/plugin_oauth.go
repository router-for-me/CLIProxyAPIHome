package management

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	log "github.com/sirupsen/logrus"
)

const oauthSessionSourcePlugin = "plugin"

type pluginManagementListResponse struct {
	PluginsEnabled bool                        `json:"plugins_enabled"`
	PluginsDir     string                      `json:"plugins_dir"`
	Plugins        []pluginManagementListEntry `json:"plugins"`
}

type pluginManagementListEntry struct {
	ID               string                        `json:"id"`
	Path             string                        `json:"path"`
	Configured       bool                          `json:"configured"`
	Registered       bool                          `json:"registered"`
	Enabled          bool                          `json:"enabled"`
	EffectiveEnabled bool                          `json:"effective_enabled"`
	SupportsOAuth    bool                          `json:"supports_oauth"`
	OAuthProvider    string                        `json:"oauth_provider"`
	Logo             string                        `json:"logo"`
	ConfigFields     []pluginManagementConfigField `json:"config_fields"`
	Menus            []pluginManagementMenu        `json:"menus"`
	Metadata         *pluginManagementMetadata     `json:"metadata"`
}

type pluginManagementMetadata struct {
	Name             string                        `json:"name"`
	Version          string                        `json:"version"`
	Author           string                        `json:"author"`
	GitHubRepository string                        `json:"github_repository"`
	Logo             string                        `json:"logo"`
	ConfigFields     []pluginManagementConfigField `json:"config_fields"`
}

type pluginManagementConfigField struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	EnumValues  []string `json:"enum_values"`
	Description string   `json:"description"`
}

type pluginManagementMenu struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

func (h *Handler) ListPlugins(c *gin.Context) {
	if h == nil || h.runtime == nil {
		c.JSON(http.StatusOK, pluginManagementListResponse{
			PluginsDir: "plugins",
			Plugins:    []pluginManagementListEntry{},
		})
		return
	}

	cfg := h.runtime.Config()
	if cfg == nil {
		c.JSON(http.StatusOK, pluginManagementListResponse{
			PluginsDir: "plugins",
			Plugins:    []pluginManagementListEntry{},
		})
		return
	}

	entries := make(map[string]pluginManagementListEntry)
	for id, item := range cfg.Plugins.Configs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		entry := entries[id]
		entry.ID = pluginManagementString(id)
		entry.Configured = true
		entry.Enabled = pluginInstanceEnabled(item)
		if entry.ConfigFields == nil {
			entry.ConfigFields = []pluginManagementConfigField{}
		}
		if entry.Menus == nil {
			entry.Menus = []pluginManagementMenu{}
		}
		entries[id] = entry
	}

	for _, info := range h.runtime.RegisteredPlugins() {
		id := strings.TrimSpace(info.ID)
		if id == "" {
			continue
		}
		entry := entries[id]
		entry.ID = pluginManagementString(id)
		entry.Registered = true
		entry.SupportsOAuth = info.SupportsOAuth
		entry.OAuthProvider = pluginManagementString(info.OAuthProvider)
		entry.Logo = pluginManagementString(info.Metadata.Logo)
		entry.ConfigFields = pluginManagementConfigFields(info.Metadata.ConfigFields)
		entry.Menus = []pluginManagementMenu{}
		entry.Metadata = pluginManagementMetadataFromPlugin(info.Metadata)
		entries[id] = entry
	}

	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]pluginManagementListEntry, 0, len(ids))
	for _, id := range ids {
		entry := entries[id]
		entry.EffectiveEnabled = cfg.Plugins.Enabled && entry.Enabled && entry.Registered
		if entry.ConfigFields == nil {
			entry.ConfigFields = []pluginManagementConfigField{}
		}
		if entry.Menus == nil {
			entry.Menus = []pluginManagementMenu{}
		}
		out = append(out, entry)
	}

	c.JSON(http.StatusOK, pluginManagementListResponse{
		PluginsEnabled: cfg.Plugins.Enabled,
		PluginsDir:     pluginManagementString(pluginStoreDir(cfg)),
		Plugins:        out,
	})
}

func (h *Handler) ServePluginAuthURL(c *gin.Context) bool {
	if h == nil || h.runtime == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	provider, okProvider := pluginAuthProviderFromManagementPath(c.Request.URL.Path)
	callbackPath := "/v0/management/oauth-callback"
	if c.Request.URL.Path == "/v8/management/oauth/auth-url" {
		var errProvider error
		provider, errProvider = normalizePluginOAuthProvider(c.Query("provider"))
		okProvider = errProvider == nil
		callbackPath = "/v8/management/oauth/callback"
	}
	if !okProvider || !h.runtime.HasPluginAuthProvider(provider) {
		return false
	}

	ctx := pluginAuthRequestContext(requestContextOrBackground(c), c)
	baseURL, errBaseURL := h.pluginManagementCallbackURL(c, callbackPath)
	if errBaseURL != nil {
		log.WithError(errBaseURL).WithField("provider", provider).Error("cluster plugin oauth: callback URL failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	resp, handled, errStart := h.runtime.StartPluginLogin(ctx, provider, baseURL)
	if !handled {
		return false
	}
	if errStart != nil {
		log.WithError(errStart).WithField("provider", provider).Error("cluster plugin oauth: start login failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	state := strings.TrimSpace(resp.State)
	if errState := validateOAuthState(state); errState != nil {
		log.WithError(errState).WithField("provider", provider).Error("cluster plugin oauth: plugin returned invalid state")
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth state"})
		return true
	}
	if strings.TrimSpace(resp.URL) == "" {
		log.WithField("provider", provider).Error("cluster plugin oauth: plugin returned empty auth URL")
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	if errRegister := h.registerPluginOAuthSession(c, provider, state, resp.Metadata); errRegister != nil {
		log.WithError(errRegister).WithField("provider", provider).Error("cluster plugin oauth: session register failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return true
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": resp.URL, "state": state})
	return true
}

func (h *Handler) respondPluginAuthStatus(c *gin.Context, ctx context.Context, session *cluster.OAuthSessionRecord, data map[string]any) bool {
	if session == nil || !oauthSessionIsPlugin(data) {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(session.Provider))
	if h == nil || h.runtime == nil || provider == "" || !h.runtime.HasPluginAuthProvider(provider) {
		c.JSON(http.StatusOK, gin.H{"status": "wait"})
		return true
	}

	pollCtx, cancelPoll := h.oauthSessionContext(ctx, session.State, cluster.OAuthSessionTTL)
	defer cancelPoll()
	resp, handled, errPoll := h.runtime.PollPluginLogin(pluginAuthRequestContext(pollCtx, c), provider, session.State, pluginOAuthPollMetadata(data))
	// A remote cancellation may have won while the plugin was polling.
	if current, errCurrent := h.repo.GetOAuthSession(ctx, session.State); errCurrent == nil && (current == nil || current.Status != "") {
		h.GetAuthStatus(c)
		return true
	}
	if !handled {
		c.JSON(http.StatusOK, gin.H{"status": "wait"})
		return true
	}
	if errPoll != nil {
		message := pluginOAuthErrorMessage(errPoll.Error())
		_ = h.repo.SetOAuthSessionError(ctx, session.State, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": message})
		return true
	}

	switch resp.Status {
	case "", pluginapi.AuthLoginStatusPending:
		c.JSON(http.StatusOK, gin.H{"status": "wait"})
	case pluginapi.AuthLoginStatusError:
		message := pluginOAuthErrorMessage(resp.Message)
		_ = h.repo.SetOAuthSessionError(ctx, session.State, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": message})
	case pluginapi.AuthLoginStatusSuccess:
		auths, errAuths := h.runtime.PluginLoginAuths(resp)
		if errAuths != nil {
			log.WithError(errAuths).WithField("provider", provider).Error("cluster plugin oauth: decode auth failed")
			_ = h.repo.SetOAuthSessionError(ctx, session.State, "Authentication failed")
			c.JSON(http.StatusOK, gin.H{"status": "error", "error": "Authentication failed"})
			return true
		}
		for _, auth := range auths {
			cluster.EnsurePluginAuthIdentity(auth)
		}
		if errComplete := h.repo.CompleteOAuthSessionWithAuths(ctx, session.State, auths); errComplete != nil {
			if errors.Is(errComplete, cluster.ErrOAuthSessionNotPending) {
				h.GetAuthStatus(c)
				return true
			}
			log.WithError(errComplete).WithField("provider", provider).Error("cluster plugin oauth: save auth failed")
			_ = h.repo.SetOAuthSessionError(ctx, session.State, "Failed to save authentication tokens")
			c.JSON(http.StatusOK, gin.H{"status": "error", "error": "Failed to save authentication tokens"})
			return true
		}
		if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
			log.WithError(errRefresh).WithField("provider", provider).Error("cluster plugin oauth: refresh auth failed")
			_ = h.repo.SetOAuthSessionError(ctx, session.State, "Failed to save authentication tokens")
			c.JSON(http.StatusOK, gin.H{"status": "error", "error": "Failed to save authentication tokens"})
			return true
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	default:
		c.JSON(http.StatusOK, gin.H{"status": "wait"})
	}
	return true
}

func (h *Handler) registerPluginOAuthSession(c *gin.Context, provider, state string, metadata map[string]any) error {
	data := pluginOAuthSessionData(metadata)
	return h.registerOAuthSession(c, provider, state, data)
}

func pluginOAuthSessionData(metadata map[string]any) map[string]any {
	data := cloneAnyMapForPluginOAuth(metadata)
	if data == nil {
		data = make(map[string]any)
	}
	data["source"] = oauthSessionSourcePlugin
	return data
}

func oauthSessionIsPlugin(data map[string]any) bool {
	return strings.EqualFold(stringFromAny(data["source"]), oauthSessionSourcePlugin)
}

func pluginOAuthPollMetadata(data map[string]any) map[string]any {
	out := cloneAnyMapForPluginOAuth(data)
	delete(out, "source")
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizePluginOAuthProvider(provider string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "", errUnsupportedProvider
	}
	for _, char := range provider {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '-':
		default:
			return "", errUnsupportedProvider
		}
	}
	return provider, nil
}

func pluginAuthProviderFromManagementPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	const prefix = "/v0/management/"
	const suffix = "-auth-url"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	provider, errProvider := normalizePluginOAuthProvider(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return provider, errProvider == nil
}

func (h *Handler) pluginManagementCallbackURL(c *gin.Context, callbackPath string) (string, error) {
	callbackPath = strings.TrimSpace(callbackPath)
	if callbackPath == "" {
		callbackPath = "/v0/management/oauth-callback"
	}
	if !strings.HasPrefix(callbackPath, "/") {
		callbackPath = "/" + callbackPath
	}
	scheme := managementRequestScheme(c)
	host := managementRequestHost(c)
	if host == "" && h != nil && h.nodeIP != "" && h.nodePort > 0 {
		host = fmt.Sprintf("%s:%d", h.nodeIP, h.nodePort)
	}
	if host == "" {
		return "", fmt.Errorf("management request host is empty")
	}
	return (&url.URL{Scheme: scheme, Host: host, Path: callbackPath}).String(), nil
}

func managementRequestScheme(c *gin.Context) string {
	if c != nil {
		if forwardedProto := firstHeaderValue(c.GetHeader("X-Forwarded-Proto")); forwardedProto != "" {
			switch strings.ToLower(forwardedProto) {
			case "http", "https":
				return strings.ToLower(forwardedProto)
			}
		}
		if c.Request != nil && c.Request.TLS != nil {
			return "https"
		}
	}
	return "http"
}

func managementRequestHost(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if forwardedHost := firstHeaderValue(c.GetHeader("X-Forwarded-Host")); forwardedHost != "" {
		return forwardedHost
	}
	if c.Request != nil {
		return strings.TrimSpace(c.Request.Host)
	}
	return ""
}

func firstHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, ","); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return value
}

func pluginAuthRequestContext(ctx context.Context, c *gin.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ctx
	}
	return cpaauth.WithRequestInfo(ctx, &cpaauth.RequestInfo{
		Query:   c.Request.URL.Query(),
		Headers: c.Request.Header,
	})
}

func pluginOAuthErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "Authentication failed"
	}
	return message
}

func pluginManagementConfigFields(fields []pluginapi.ConfigField) []pluginManagementConfigField {
	out := make([]pluginManagementConfigField, 0, len(fields))
	for _, field := range fields {
		out = append(out, pluginManagementConfigField{
			Name:        pluginManagementString(field.Name),
			Type:        pluginManagementString(string(field.Type)),
			EnumValues:  pluginManagementStrings(field.EnumValues),
			Description: pluginManagementString(field.Description),
		})
	}
	return out
}

func pluginManagementMenus(menus []sdkpluginhost.RegisteredPluginMenu) []pluginManagementMenu {
	out := make([]pluginManagementMenu, 0, len(menus))
	for _, menu := range menus {
		out = append(out, pluginManagementMenu{
			Path:        pluginManagementString(menu.Path),
			Menu:        pluginManagementString(menu.Menu),
			Description: pluginManagementString(menu.Description),
		})
	}
	return out
}

func pluginManagementMetadataFromPlugin(meta pluginapi.Metadata) *pluginManagementMetadata {
	return &pluginManagementMetadata{
		Name:             pluginManagementString(meta.Name),
		Version:          pluginManagementString(meta.Version),
		Author:           pluginManagementString(meta.Author),
		GitHubRepository: pluginManagementString(meta.GitHubRepository),
		Logo:             pluginManagementString(meta.Logo),
		ConfigFields:     pluginManagementConfigFields(meta.ConfigFields),
	}
}

func pluginManagementString(value string) string {
	return html.EscapeString(value)
}

func pluginManagementStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, pluginManagementString(value))
	}
	return out
}

func cloneAnyMapForPluginOAuth(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func pluginOAuthCallbackSessionData(code, provider string) map[string]any {
	data := map[string]any{
		"callback_received_at": time.Now().UTC().Format(time.RFC3339),
		"callback_code":        strings.TrimSpace(code),
	}
	if provider = strings.TrimSpace(provider); provider != "" {
		data["callback_provider"] = provider
	}
	return data
}
