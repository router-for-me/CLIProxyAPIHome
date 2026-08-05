// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Home-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
)

var ErrInvalidProviderCredentialID = errors.New("provider credential id must be a canonical UUID")

// Config represents the application's configuration, loaded from a YAML file.
type Config struct {
	SDKConfig `yaml:",inline"`
	// Host is the network host/interface on which the API server will bind.
	// Default is empty ("") to bind all interfaces (IPv4 + IPv6). Use "127.0.0.1" or "localhost" for local-only access.
	Host string `yaml:"host" json:"-"`
	// Port is the network port on which the API server will listen.
	Port int `yaml:"port" json:"-"`

	// AllowHost limits RESP client connections to the listed IP addresses.
	// Empty means all client hosts are allowed.
	AllowHost []string `yaml:"allow-host" json:"-"`

	// TLS config controls HTTPS server settings.
	TLS TLSConfig `yaml:"tls" json:"tls"`

	// TrustedProxies lists reverse-proxy IPs or CIDRs whose forwarded client
	// address headers the HTTP server may trust. Empty disables forwarded headers.
	TrustedProxies []string `yaml:"trusted-proxies" json:"-"`

	// RemoteManagement nests management-related options under 'remote-management'.
	RemoteManagement RemoteManagement `yaml:"remote-management" json:"-"`

	// Plugins configures dynamic plugin distribution for downstream CPA nodes.
	Plugins PluginsConfig `yaml:"plugins" json:"plugins"`

	// UserEmail configures verified user emails and password-recovery delivery.
	UserEmail UserEmailConfig `yaml:"user-email" json:"-"`

	// CredentialConcurrency controls Home credential concurrency lifecycle behavior.
	CredentialConcurrency CredentialConcurrencyConfig `yaml:"credential-concurrency" json:"credential-concurrency"`

	// CredentialInFlight configures credential observation snapshots.
	CredentialInFlight CredentialInFlightConfig `yaml:"credential-in-flight" json:"credential-in-flight"`

	// AuthDir is the directory where authentication token files are stored.
	AuthDir string `yaml:"auth-dir" json:"-"`

	// Debug enables or disables debug-level logging and other debug features.
	Debug bool `yaml:"debug" json:"debug"`

	// Pprof config controls the optional pprof HTTP debug server.
	Pprof PprofConfig `yaml:"pprof" json:"pprof"`

	// CommercialMode disables high-overhead HTTP middleware features to minimize per-request memory usage.
	CommercialMode bool `yaml:"commercial-mode" json:"commercial-mode"`

	// LoggingToFile controls whether application logs are written to rotating files or stdout.
	LoggingToFile bool `yaml:"logging-to-file" json:"logging-to-file"`

	// LogsMaxTotalSizeMB limits the total size (in MB) of log files under the logs directory.
	// When exceeded, the oldest log files are deleted until within the limit. Set to 0 to disable.
	LogsMaxTotalSizeMB int `yaml:"logs-max-total-size-mb" json:"logs-max-total-size-mb"`

	// ErrorLogsMaxFiles limits the number of error log files retained when request logging is disabled.
	// When exceeded, the oldest error log files are deleted. Default is 10. Set to 0 to disable cleanup.
	ErrorLogsMaxFiles int `yaml:"error-logs-max-files" json:"error-logs-max-files"`

	// UsageStatisticsEnabled toggles in-memory usage aggregation; when false, usage data is discarded.
	UsageStatisticsEnabled bool `yaml:"usage-statistics-enabled" json:"usage-statistics-enabled"`

	// RedisUsageQueueRetentionSeconds controls how long usage queue items are retained
	// in memory before database-backed persistence consumes them.
	// Default: 60. Max: 3600.
	RedisUsageQueueRetentionSeconds int `yaml:"redis-usage-queue-retention-seconds" json:"redis-usage-queue-retention-seconds"`

	// DisableCooling disables quota cooldown scheduling when true.
	DisableCooling bool `yaml:"disable-cooling" json:"disable-cooling"`

	// WebsocketAuth enables or disables authentication for the WebSocket API.
	WebsocketAuth bool `yaml:"ws-auth" json:"ws-auth"`

	// AuthAutoRefreshWorkers overrides the size of the core auth auto-refresh worker pool.
	// When <= 0, the default worker count is used.
	AuthAutoRefreshWorkers int `yaml:"auth-auto-refresh-workers" json:"auth-auto-refresh-workers"`

	// RequestRetry defines the retry times when the request failed.
	RequestRetry int `yaml:"request-retry" json:"request-retry"`
	// MaxRetryCredentials defines the maximum number of credentials to try for a failed request.
	// Set to 0 or a negative value to keep trying all available credentials (legacy behavior).
	MaxRetryCredentials int `yaml:"max-retry-credentials" json:"max-retry-credentials"`
	// MaxRetryInterval defines the maximum wait time in seconds before retrying a cooled-down credential.
	MaxRetryInterval int `yaml:"max-retry-interval" json:"max-retry-interval"`

	// QuotaExceeded defines the behavior when a quota is exceeded.
	QuotaExceeded QuotaExceeded `yaml:"quota-exceeded" json:"quota-exceeded"`

	// Routing controls credential selection behavior.
	Routing RoutingConfig `yaml:"routing" json:"routing"`

	// AntigravitySignatureCacheEnabled controls whether signature cache validation is enabled for thinking blocks.
	// When true (default), cached signatures are preferred and validated.
	// When false, client signatures are used directly after normalization (bypass mode).
	AntigravitySignatureCacheEnabled *bool `yaml:"antigravity-signature-cache-enabled,omitempty" json:"antigravity-signature-cache-enabled,omitempty"`

	AntigravitySignatureBypassStrict *bool `yaml:"antigravity-signature-bypass-strict,omitempty" json:"antigravity-signature-bypass-strict,omitempty"`

	// GeminiKey defines Gemini API key configurations with optional routing overrides.
	GeminiKey []GeminiKey `yaml:"gemini-api-key" json:"gemini-api-key"`

	// Codex defines a list of Codex API key configurations as specified in the YAML configuration file.
	CodexKey []CodexKey `yaml:"codex-api-key" json:"codex-api-key"`

	// XAIKey defines xAI API key configurations using the same structure as Codex API keys.
	XAIKey []XAIKey `yaml:"xai-api-key" json:"xai-api-key"`

	// CodexHeaderDefaults configures fallback headers for Codex OAuth model requests.
	// These are used only when the client does not send its own headers.
	CodexHeaderDefaults CodexHeaderDefaults `yaml:"codex-header-defaults" json:"codex-header-defaults"`

	// ClaudeKey defines a list of Claude API key configurations as specified in the YAML configuration file.
	ClaudeKey []ClaudeKey `yaml:"claude-api-key" json:"claude-api-key"`

	// ClaudeHeaderDefaults configures default header values for Claude API requests.
	// These are used as fallbacks when the client does not send its own headers.
	ClaudeHeaderDefaults ClaudeHeaderDefaults `yaml:"claude-header-defaults" json:"claude-header-defaults"`

	// OpenAICompatibility defines OpenAI API compatibility configurations for external providers.
	OpenAICompatibility []OpenAICompatibility `yaml:"openai-compatibility" json:"openai-compatibility"`

	// VertexCompatAPIKey defines Vertex AI-compatible API key configurations for third-party providers.
	// Used for services that use Vertex AI-style paths but with simple API key authentication.
	VertexCompatAPIKey []VertexCompatKey `yaml:"vertex-api-key" json:"vertex-api-key"`

	// OAuthExcludedModels defines per-provider global model exclusions applied to OAuth/file-backed auth entries.
	OAuthExcludedModels map[string][]string `yaml:"oauth-excluded-models,omitempty" json:"oauth-excluded-models,omitempty"`

	// OAuthModelAlias defines global model name aliases for OAuth/file-backed auth channels.
	// These aliases affect both model listing and model routing for supported channels:
	// vertex, antigravity, claude, codex, kimi, xai.
	//
	// NOTE: This does not apply to existing per-credential model alias features under:
	// gemini-api-key, codex-api-key, xai-api-key, claude-api-key, openai-compatibility, and vertex-api-key.
	OAuthModelAlias map[string][]OAuthModelAlias `yaml:"oauth-model-alias,omitempty" json:"oauth-model-alias,omitempty"`

	// Payload defines default and override rules for provider payload parameters.
	Payload PayloadConfig `yaml:"payload" json:"payload"`
}

// ClaudeHeaderDefaults configures default header values injected into Claude API requests.
// In legacy mode, UserAgent/PackageVersion/RuntimeVersion/Timeout act as fallbacks when
// the client omits them, while OS/Arch remain runtime-derived. When stabilized device
// profiles are enabled, OS/Arch become the pinned platform baseline, while
// UserAgent/PackageVersion/RuntimeVersion seed the upgradeable software fingerprint.
type ClaudeHeaderDefaults struct {
	UserAgent              string `yaml:"user-agent" json:"user-agent"`
	PackageVersion         string `yaml:"package-version" json:"package-version"`
	RuntimeVersion         string `yaml:"runtime-version" json:"runtime-version"`
	OS                     string `yaml:"os" json:"os"`
	Arch                   string `yaml:"arch" json:"arch"`
	Timeout                string `yaml:"timeout" json:"timeout"`
	StabilizeDeviceProfile *bool  `yaml:"stabilize-device-profile,omitempty" json:"stabilize-device-profile,omitempty"`
}

// CodexHeaderDefaults configures fallback header values injected into Codex
// model requests for OAuth/file-backed auth when the client omits them.
// UserAgent applies to HTTP and websocket requests; BetaFeatures only applies to websockets.
type CodexHeaderDefaults struct {
	UserAgent    string `yaml:"user-agent" json:"user-agent"`
	BetaFeatures string `yaml:"beta-features" json:"beta-features"`
}

// TLSConfig holds HTTPS server settings.
type TLSConfig struct {
	// Enable toggles HTTPS server mode.
	Enable bool `yaml:"enable" json:"enable"`
	// Cert is the path to the TLS certificate file.
	Cert string `yaml:"cert" json:"cert"`
	// Key is the path to the TLS private key file.
	Key string `yaml:"key" json:"key"`
}

// PprofConfig holds pprof HTTP server settings.
type PprofConfig struct {
	// Enable toggles the pprof HTTP debug server.
	Enable bool `yaml:"enable" json:"enable"`
	// Addr is the host:port address for the pprof HTTP server.
	Addr string `yaml:"addr" json:"addr"`
}

// UserEmailConfig controls optional verified-email and password-recovery behavior.
type UserEmailConfig struct {
	Enabled              bool                  `yaml:"enabled" json:"enabled"`
	PublicUserURL        string                `yaml:"public-user-url" json:"public-user-url"`
	FromAddress          string                `yaml:"from-address" json:"from-address"`
	FromName             string                `yaml:"from-name" json:"from-name"`
	Sender               UserEmailSenderConfig `yaml:"sender" json:"sender"`
	VerificationTokenTTL string                `yaml:"verification-token-ttl" json:"verification-token-ttl"`
	ResetTokenTTL        string                `yaml:"reset-token-ttl" json:"reset-token-ttl"`
}

// UserEmailSenderConfig selects the configured mail transport.
type UserEmailSenderConfig struct {
	Type string              `yaml:"type" json:"type"`
	SMTP UserEmailSMTPConfig `yaml:"smtp" json:"smtp"`
}

// UserEmailSMTPConfig configures SMTP delivery without persisting the password itself.
type UserEmailSMTPConfig struct {
	Host        string `yaml:"host" json:"host"`
	Port        int    `yaml:"port" json:"port"`
	Username    string `yaml:"username" json:"username"`
	PasswordEnv string `yaml:"password-env" json:"password-env"`
	StartTLS    bool   `yaml:"starttls" json:"starttls"`
}

// RemoteManagement holds management API configuration under 'remote-management'.
type RemoteManagement struct {
	// AllowRemote toggles remote (non-localhost) access to management API.
	AllowRemote bool `yaml:"allow-remote"`
	// SecretKey is the management key (plaintext or bcrypt hashed). YAML key intentionally 'secret-key'.
	SecretKey string `yaml:"secret-key"`
	// DisableControlPanel skips serving and syncing the bundled management UI when true.
	DisableControlPanel bool `yaml:"disable-control-panel"`
	// DisableAutoUpdatePanel is retained for configuration compatibility with older deployments.
	// Embedded bundle mode serves local assets at build time and does not perform runtime remote updates.
	DisableAutoUpdatePanel bool `yaml:"disable-auto-update-panel"`
	// PanelGitHubRepository is retained for legacy compatibility and version tracking.
	// Embedded bundle mode does not fetch control panel assets from GitHub at runtime.
	PanelGitHubRepository string `yaml:"panel-github-repository"`
}

// QuotaExceeded defines the behavior when API quota limits are exceeded.
// It provides configuration options for automatic failover mechanisms.
type QuotaExceeded struct {
	// SwitchProject indicates whether to automatically switch to another project when a quota is exceeded.
	SwitchProject bool `yaml:"switch-project" json:"switch-project"`

	// SwitchPreviewModel indicates whether to automatically switch to a preview model when a quota is exceeded.
	SwitchPreviewModel bool `yaml:"switch-preview-model" json:"switch-preview-model"`

	// AntigravityCredits enables credits-based last-resort fallback for Claude models.
	// When all free-tier auths are exhausted (429/503), the conductor retries with
	// an auth that has available Google One AI credits.
	AntigravityCredits bool `yaml:"antigravity-credits" json:"antigravity-credits"`
}

// RoutingConfig configures how credentials are selected for requests.
type RoutingConfig struct {
	// Strategy selects the credential selection strategy.
	// Supported values: "round-robin" (default), "fill-first".
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// ClaudeCodeSessionAffinity enables session-sticky routing for Claude Code clients.
	// When enabled, requests with the same session ID (extracted from metadata.user_id)
	// are routed to the same auth credential when available.
	// Deprecated: Use SessionAffinity instead for universal session support.
	ClaudeCodeSessionAffinity bool `yaml:"claude-code-session-affinity,omitempty" json:"claude-code-session-affinity,omitempty"`

	// SessionAffinity enables universal session-sticky routing for all clients.
	// Session IDs are extracted from multiple sources:
	// metadata.user_id (Claude Code session format), X-Session-ID, Session_id (Codex),
	// X-Client-Request-Id (PI), metadata.user_id, conversation_id, or message hash.
	// Automatic failover is always enabled when bound auth becomes unavailable.
	SessionAffinity bool `yaml:"session-affinity,omitempty" json:"session-affinity,omitempty"`

	// SessionAffinityTTL specifies how long session-to-auth bindings are retained.
	// Default: 1h. Accepts duration strings like "30m", "1h", "2h30m".
	SessionAffinityTTL string `yaml:"session-affinity-ttl,omitempty" json:"session-affinity-ttl,omitempty"`
}

// OAuthModelAlias defines a model ID alias for a specific channel.
// It maps the upstream model name (Name) to the client-visible alias (Alias).
// When Fork is true, the alias is added as an additional model in listings while
// keeping the original model ID available.
type OAuthModelAlias struct {
	Name         string `yaml:"name" json:"name"`
	Alias        string `yaml:"alias" json:"alias"`
	Fork         bool   `yaml:"fork,omitempty" json:"fork,omitempty"`
	ForceMapping bool   `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

// PayloadConfig defines default and override parameter rules applied to provider payloads.
type PayloadConfig struct {
	// Default defines rules that only set parameters when they are missing in the payload.
	Default []PayloadRule `yaml:"default" json:"default"`
	// DefaultRaw defines rules that set raw JSON values only when they are missing.
	DefaultRaw []PayloadRule `yaml:"default-raw" json:"default-raw"`
	// Override defines rules that always set parameters, overwriting any existing values.
	Override []PayloadRule `yaml:"override" json:"override"`
	// OverrideRaw defines rules that always set raw JSON values, overwriting any existing values.
	OverrideRaw []PayloadRule `yaml:"override-raw" json:"override-raw"`
	// Filter defines rules that remove parameters from the payload by JSON path.
	Filter []PayloadFilterRule `yaml:"filter" json:"filter"`
}

// PayloadFilterRule describes a rule to remove specific JSON paths from matching model payloads.
type PayloadFilterRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params lists JSON paths (gjson/sjson syntax) to remove from the payload.
	Params []string `yaml:"params" json:"params"`
}

// PayloadRule describes a single rule targeting a list of models with parameter updates.
type PayloadRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params maps JSON paths (gjson/sjson syntax) to values written into the payload.
	// For *-raw rules, values are treated as raw JSON fragments (strings are used as-is).
	Params map[string]any `yaml:"params" json:"params"`
}

// PayloadModelRule ties a model name pattern to a specific translator protocol.
type PayloadModelRule struct {
	// Name is the model name or wildcard pattern (e.g., "gpt-*", "*-5", "gemini-*-pro").
	Name string `yaml:"name" json:"name"`
	// Protocol restricts the rule to a specific translator format (e.g., "gemini", "responses").
	Protocol string `yaml:"protocol" json:"protocol"`
	// Headers restricts the rule to requests whose headers match all configured wildcard patterns.
	Headers map[string]string `yaml:"headers" json:"headers"`
	// FromProtocol restricts the rule to a specific source protocol (e.g., "gemini", "responses").
	FromProtocol string `yaml:"from-protocol" json:"from-protocol"`
	// Match requires payload JSON paths to equal the configured values.
	Match []map[string]any `yaml:"match" json:"match"`
	// NotMatch requires payload JSON paths to not equal the configured values.
	NotMatch []map[string]any `yaml:"not-match" json:"not-match"`
	// Exist requires payload JSON paths to exist and not be null.
	Exist []string `yaml:"exist" json:"exist"`
	// NotExist requires payload JSON paths to be missing or null.
	NotExist []string `yaml:"not-exist" json:"not-exist"`
}

// CloakConfig configures request cloaking for non-Claude-Code clients.
// Cloaking disguises API requests to appear as originating from the official Claude Code CLI.
type CloakConfig struct {
	// Mode controls cloaking behavior: "auto" (default), "always", or "never".
	// - "auto": cloak only when client is not Claude Code (based on User-Agent)
	// - "always": always apply cloaking regardless of client
	// - "never": never apply cloaking
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// StrictMode controls how system prompts are handled when cloaking.
	// - false (default): prepend Claude Code prompt to user system messages
	// - true: strip all user system messages, keep only Claude Code prompt
	StrictMode bool `yaml:"strict-mode,omitempty" json:"strict-mode,omitempty"`

	// SensitiveWords is a list of words to obfuscate with zero-width characters.
	// This can help bypass certain content filters.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// CacheUserID controls whether Claude user_id values are cached per API key.
	// When false, a fresh random user_id is generated for every request.
	CacheUserID *bool `yaml:"cache-user-id,omitempty" json:"cache-user-id,omitempty"`
}

// ClaudeKey represents the configuration for a Claude API key,
// including the API key itself and an optional base URL for the API endpoint.
type ClaudeKey struct {
	// ID is the immutable credential identity.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// UUID is accepted for backward-compatible input and normalized to ID.
	UUID string `yaml:"uuid,omitempty" json:"uuid,omitempty"`
	// APIKey is the authentication key for accessing Claude API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/claude-sonnet-4").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Claude API endpoint.
	// If empty, the default Claude API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []ClaudeModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this credential when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// Cloak configures request cloaking for non-Claude-Code clients.
	Cloak *CloakConfig `yaml:"cloak,omitempty" json:"cloak,omitempty"`

	// ExperimentalCCHSigning enables opt-in final-body cch signing for cloaked
	// Claude /v1/messages requests. It is disabled by default so upstream seed
	// changes do not alter the proxy's legacy behavior.
	ExperimentalCCHSigning bool `yaml:"experimental-cch-signing,omitempty" json:"experimental-cch-signing,omitempty"`
}

// GetAPIKey returns an api key.
func (k ClaudeKey) GetAPIKey() string { return k.APIKey }

// GetBaseURL returns a base url.
func (k ClaudeKey) GetBaseURL() string { return k.BaseURL }

// ClaudeModel describes a mapping between an alias and the actual upstream model name.
type ClaudeModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`
}

// GetName returns a name.
func (m ClaudeModel) GetName() string { return m.Name }

// GetAlias returns an alias.
func (m ClaudeModel) GetAlias() string { return m.Alias }

// CodexKey represents the configuration for a Codex API key,
// including the API key itself and an optional base URL for the API endpoint.
type CodexKey struct {
	// ID is the immutable credential identity.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// UUID is accepted for backward-compatible input and normalized to ID.
	UUID string `yaml:"uuid,omitempty" json:"uuid,omitempty"`
	// APIKey is the authentication key for accessing Codex API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gpt-5-codex").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Codex API endpoint.
	// If empty, the default Codex API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// Websockets enables the Responses API websocket transport for this credential.
	Websockets bool `yaml:"websockets,omitempty" json:"websockets,omitempty"`

	// AlphaSearch allows this Codex API key to serve the Alpha Search endpoint.
	AlphaSearch bool `yaml:"alpha-search,omitempty" json:"alpha-search,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []CodexModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this credential when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

// GetAPIKey returns an api key.
func (k CodexKey) GetAPIKey() string { return k.APIKey }

// GetBaseURL returns a base url.
func (k CodexKey) GetBaseURL() string { return k.BaseURL }

// CodexModel describes a mapping between an alias and the actual upstream model name.
type CodexModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

// GetName returns a name.
func (m CodexModel) GetName() string { return m.Name }

// GetAlias returns an alias.
func (m CodexModel) GetAlias() string { return m.Alias }

// GetDisplayName returns a display name.
func (m CodexModel) GetDisplayName() string { return m.DisplayName }

// GetForceMapping returns whether response model fields should be rewritten.
func (m CodexModel) GetForceMapping() bool { return m.ForceMapping }

// XAIKey uses the Codex API key structure for native xAI execution.
type XAIKey = CodexKey

// XAIModel uses the Codex model mapping structure for xAI models.
type XAIModel = CodexModel

// GeminiKey represents the configuration for a Gemini API key,
// including optional overrides for upstream base URL, proxy routing, and headers.
type GeminiKey struct {
	// ID is the immutable credential identity.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// UUID is accepted for backward-compatible input and normalized to ID.
	UUID string `yaml:"uuid,omitempty" json:"uuid,omitempty"`
	// APIKey is the authentication key for accessing Gemini API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gemini-3-pro-preview").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Gemini API endpoint.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []GeminiModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this credential when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

// GetAPIKey returns an api key.
func (k GeminiKey) GetAPIKey() string { return k.APIKey }

// GetBaseURL returns a base url.
func (k GeminiKey) GetBaseURL() string { return k.BaseURL }

// GeminiModel describes a mapping between an alias and the actual upstream model name.
type GeminiModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`
}

// GetName returns a name.
func (m GeminiModel) GetName() string { return m.Name }

// GetAlias returns an alias.
func (m GeminiModel) GetAlias() string { return m.Alias }

// OpenAICompatibility represents the configuration for OpenAI API compatibility
// with external providers, allowing model aliases to be routed through OpenAI API format.
type OpenAICompatibility struct {
	// ID is the immutable identity of the fallback credential when no API key entries exist.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// UUID is accepted for backward-compatible input and normalized to ID.
	UUID string `yaml:"uuid,omitempty" json:"uuid,omitempty"`
	// Name is the identifier for this OpenAI compatibility configuration.
	Name string `yaml:"name" json:"name"`

	// Priority controls selection preference when multiple providers or credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Disabled prevents this provider from being used for routing.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// Prefix optionally namespaces model aliases for this provider (e.g., "teamA/kimi-k2").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the external OpenAI-compatible API endpoint.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// APIKeyEntries defines API keys with optional per-key proxy configuration.
	APIKeyEntries []OpenAICompatibilityAPIKey `yaml:"api-key-entries,omitempty" json:"api-key-entries,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []OpenAICompatibilityModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent to this provider.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// DisableCooling disables auth/model cooldown scheduling for this provider when true.
	DisableCooling bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

// OpenAICompatibilityAPIKey represents an API key configuration with optional proxy setting.
type OpenAICompatibilityAPIKey struct {
	// ID is the immutable credential identity.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// UUID is accepted for backward-compatible input and normalized to ID.
	UUID string `yaml:"uuid,omitempty" json:"uuid,omitempty"`
	// APIKey is the authentication key for accessing the external API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
}

// OpenAICompatibilityModel represents a model configuration for OpenAI compatibility,
// including the actual model name and its alias for API routing.
type OpenAICompatibilityModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`

	// Thinking configures the thinking/reasoning capability for this model.
	// If nil, the model defaults to level-based reasoning with levels ["low", "medium", "high"].
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

// GetName returns a name.
func (m OpenAICompatibilityModel) GetName() string { return m.Name }

// GetAlias returns an alias.
func (m OpenAICompatibilityModel) GetAlias() string { return m.Alias }

// LoadConfigOptional reads YAML from configFile.
// If optional is true and the file is missing, it returns a Config with credential defaults.
// If optional is true and the file is empty or invalid, it returns a Config with credential defaults.
func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	// Read the entire configuration file into memory.
	data, err := os.ReadFile(configFile)
	if err != nil {
		if optional {
			if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
				// Missing and optional: return credential defaults (cloud deploy standby).
				return optionalConfigFallback(), nil
			}
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// In cloud deploy mode (optional=true), if file is empty or contains only whitespace, return credential defaults.
	if optional && len(bytes.TrimSpace(data)) == 0 {
		return optionalConfigFallback(), nil
	}

	// Unmarshal the YAML data into the Config struct.
	var cfg Config
	// Set defaults before unmarshal so that absent keys keep defaults.
	cfg.Host = "" // Default empty: binds to all interfaces (IPv4 + IPv6)
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.DisableCooling = false
	cfg.DisableImageGeneration = DisableImageGenerationOff
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.CredentialConcurrency = DefaultCredentialConcurrencyConfig()
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		if optional {
			// In cloud deploy mode, if YAML parsing fails, return credential defaults instead of error.
			return optionalConfigFallback(), nil
		}
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := ValidateCredentialConcurrencyConfig(cfg.CredentialConcurrency); errValidate != nil {
		return nil, errValidate
	}

	cfg.NormalizePluginsConfig()
	cfg.NormalizeUserEmailConfig()
	cfg.NormalizeTrustedProxies()
	if errTrustedProxies := ValidateTrustedProxies(cfg.TrustedProxies); errTrustedProxies != nil {
		return nil, errTrustedProxies
	}

	normalizedSecret, secretChanged, errNormalizeSecret := NormalizeRemoteManagementSecret(cfg.RemoteManagement.SecretKey)
	if errNormalizeSecret != nil {
		return nil, fmt.Errorf("failed to hash remote management key: %w", errNormalizeSecret)
	}
	if secretChanged {
		cfg.RemoteManagement.SecretKey = normalizedSecret
		// Persist the hashed value back to the config file to avoid re-hashing on next startup.
		// Preserve YAML comments and ordering; update only the nested key.
		_ = SaveConfigPreserveCommentsUpdateNestedScalar(configFile, []string{"remote-management", "secret-key"}, normalizedSecret)
	}

	cfg.RemoteManagement.PanelGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}

	cfg.Pprof.Addr = strings.TrimSpace(cfg.Pprof.Addr)
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}

	if cfg.LogsMaxTotalSizeMB < 0 {
		cfg.LogsMaxTotalSizeMB = 0
	}

	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 10
	}

	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
	} else if cfg.RedisUsageQueueRetentionSeconds > 3600 {
		log.WithField("value", cfg.RedisUsageQueueRetentionSeconds).Warn("redis-usage-queue-retention-seconds too large; clamping to 3600")
		cfg.RedisUsageQueueRetentionSeconds = 3600
	}

	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	if errNormalizeIDs := cfg.NormalizeProviderCredentialIDs(); errNormalizeIDs != nil {
		return nil, errNormalizeIDs
	}

	// Sanitize Gemini API key configuration and migrate legacy entries.
	cfg.SanitizeGeminiKeys()

	// Sanitize Vertex-compatible API keys.
	cfg.SanitizeVertexCompatKeys()

	// Sanitize Codex keys: drop entries without base-url
	cfg.SanitizeCodexKeys()

	// Sanitize xAI keys: drop entries without base-url
	cfg.SanitizeXAIKeys()

	// Sanitize Codex header defaults.
	cfg.SanitizeCodexHeaderDefaults()

	// Sanitize Claude header defaults.
	cfg.SanitizeClaudeHeaderDefaults()

	// Sanitize Claude key headers
	cfg.SanitizeClaudeKeys()

	// Sanitize OpenAI compatibility providers: drop entries without base-url
	cfg.SanitizeOpenAICompatibility()

	// Normalize OAuth provider model exclusion map.
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)

	// Normalize global OAuth model name aliases.
	cfg.SanitizeOAuthModelAlias()

	// Validate raw payload rules and drop invalid entries.
	cfg.SanitizePayloadRules()

	ForceHomeRuntimeConfig(&cfg)

	// Return the populated configuration struct.
	return &cfg, nil
}

func optionalConfigFallback() *Config {
	return &Config{
		CredentialConcurrency: DefaultCredentialConcurrencyConfig(),
		CredentialInFlight:    DefaultCredentialInFlightConfig(),
	}
}

// SanitizePayloadRules validates raw JSON payload rule params and drops invalid rules.
func (cfg *Config) SanitizePayloadRules() {
	if cfg == nil {
		return
	}
	cfg.Payload.DefaultRaw = sanitizePayloadRawRules(cfg.Payload.DefaultRaw, "default-raw")
	cfg.Payload.OverrideRaw = sanitizePayloadRawRules(cfg.Payload.OverrideRaw, "override-raw")
}

// sanitizePayloadRawRules sanitizes a payload raw rules.
func sanitizePayloadRawRules(rules []PayloadRule, section string) []PayloadRule {
	// Validate input data before converting it into runtime state.
	if len(rules) == 0 {
		return rules
	}
	out := make([]PayloadRule, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if len(rule.Params) == 0 {
			continue
		}
		invalid := false
		for path, value := range rule.Params {
			raw, ok := payloadRawString(value)
			if !ok {
				continue
			}
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || !json.Valid(trimmed) {
				log.WithFields(log.Fields{
					"section":    section,
					"rule_index": i + 1,
					"param":      path,
				}).Warn("payload rule dropped: invalid raw JSON")
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// payloadRawString handles a payload raw string.
func payloadRawString(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		return nil, false
	}
}

// SanitizeCodexHeaderDefaults trims surrounding whitespace from the
// configured Codex header fallback values.
func (cfg *Config) SanitizeCodexHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.CodexHeaderDefaults.UserAgent = strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent)
	cfg.CodexHeaderDefaults.BetaFeatures = strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

// SanitizeClaudeHeaderDefaults trims surrounding whitespace from the
// configured Claude fingerprint baseline values.
func (cfg *Config) SanitizeClaudeHeaderDefaults() {
	if cfg == nil {
		return
	}
	cfg.ClaudeHeaderDefaults.UserAgent = strings.TrimSpace(cfg.ClaudeHeaderDefaults.UserAgent)
	cfg.ClaudeHeaderDefaults.PackageVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.PackageVersion)
	cfg.ClaudeHeaderDefaults.RuntimeVersion = strings.TrimSpace(cfg.ClaudeHeaderDefaults.RuntimeVersion)
	cfg.ClaudeHeaderDefaults.OS = strings.TrimSpace(cfg.ClaudeHeaderDefaults.OS)
	cfg.ClaudeHeaderDefaults.Arch = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Arch)
	cfg.ClaudeHeaderDefaults.Timeout = strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timeout)
}

// SanitizeOAuthModelAlias normalizes and deduplicates global OAuth model name aliases.
// It trims whitespace, normalizes channel keys to lower-case, drops empty entries,
// allows multiple aliases per upstream name, and ensures aliases are unique within each channel.
func (cfg *Config) SanitizeOAuthModelAlias() {
	// Resolve credential context before calling upstream OAuth services.
	if cfg == nil || len(cfg.OAuthModelAlias) == 0 {
		return
	}
	out := make(map[string][]OAuthModelAlias, len(cfg.OAuthModelAlias))
	for rawChannel, aliases := range cfg.OAuthModelAlias {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(aliases) == 0 {
			continue
		}
		seenAlias := make(map[string]struct{}, len(aliases))
		clean := make([]OAuthModelAlias, 0, len(aliases))
		for _, entry := range aliases {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, ok := seenAlias[aliasKey]; ok {
				continue
			}
			seenAlias[aliasKey] = struct{}{}
			clean = append(clean, OAuthModelAlias{Name: name, Alias: alias, Fork: entry.Fork, ForceMapping: entry.ForceMapping})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	cfg.OAuthModelAlias = out
}

// NormalizeProviderCredentialIDs normalizes legacy UUID input to immutable IDs.
func (cfg *Config) NormalizeProviderCredentialIDs() error {
	if cfg == nil {
		return nil
	}
	for i := range cfg.GeminiKey {
		id, errNormalize := normalizeProviderCredentialID(cfg.GeminiKey[i].ID, cfg.GeminiKey[i].UUID)
		if errNormalize != nil {
			return errNormalize
		}
		cfg.GeminiKey[i].ID, cfg.GeminiKey[i].UUID = id, ""
	}
	for i := range cfg.VertexCompatAPIKey {
		id, errNormalize := normalizeProviderCredentialID(cfg.VertexCompatAPIKey[i].ID, cfg.VertexCompatAPIKey[i].UUID)
		if errNormalize != nil {
			return errNormalize
		}
		cfg.VertexCompatAPIKey[i].ID, cfg.VertexCompatAPIKey[i].UUID = id, ""
	}
	for i := range cfg.CodexKey {
		id, errNormalize := normalizeProviderCredentialID(cfg.CodexKey[i].ID, cfg.CodexKey[i].UUID)
		if errNormalize != nil {
			return errNormalize
		}
		cfg.CodexKey[i].ID, cfg.CodexKey[i].UUID = id, ""
	}
	for i := range cfg.XAIKey {
		id, errNormalize := normalizeProviderCredentialID(cfg.XAIKey[i].ID, cfg.XAIKey[i].UUID)
		if errNormalize != nil {
			return errNormalize
		}
		cfg.XAIKey[i].ID, cfg.XAIKey[i].UUID = id, ""
	}
	for i := range cfg.ClaudeKey {
		id, errNormalize := normalizeProviderCredentialID(cfg.ClaudeKey[i].ID, cfg.ClaudeKey[i].UUID)
		if errNormalize != nil {
			return errNormalize
		}
		cfg.ClaudeKey[i].ID, cfg.ClaudeKey[i].UUID = id, ""
	}
	for i := range cfg.OpenAICompatibility {
		id, errNormalize := normalizeProviderCredentialID(cfg.OpenAICompatibility[i].ID, cfg.OpenAICompatibility[i].UUID)
		if errNormalize != nil {
			return errNormalize
		}
		cfg.OpenAICompatibility[i].ID, cfg.OpenAICompatibility[i].UUID = id, ""
		for j := range cfg.OpenAICompatibility[i].APIKeyEntries {
			keyID, errNormalizeKey := normalizeProviderCredentialID(cfg.OpenAICompatibility[i].APIKeyEntries[j].ID, cfg.OpenAICompatibility[i].APIKeyEntries[j].UUID)
			if errNormalizeKey != nil {
				return errNormalizeKey
			}
			cfg.OpenAICompatibility[i].APIKeyEntries[j].ID, cfg.OpenAICompatibility[i].APIKeyEntries[j].UUID = keyID, ""
		}
	}
	return nil
}

func normalizeProviderCredentialID(id string, legacyUUID string) (string, error) {
	if errValidateID := validateProviderCredentialID(id); errValidateID != nil {
		return "", errValidateID
	}
	if errValidateUUID := validateProviderCredentialID(legacyUUID); errValidateUUID != nil {
		return "", errValidateUUID
	}
	if id != "" && legacyUUID != "" && id != legacyUUID {
		return "", fmt.Errorf("credential id and uuid must match")
	}
	if id == "" {
		id = legacyUUID
	}
	return id, nil
}

func validateProviderCredentialID(id string) error {
	if id == "" {
		return nil
	}
	parsed, errParse := uuid.Parse(id)
	if errParse != nil || parsed.String() != id {
		return fmt.Errorf("%w: %q", ErrInvalidProviderCredentialID, id)
	}
	return nil
}

// SanitizeOpenAICompatibility removes OpenAI-compatibility provider entries that are
// not actionable, specifically those missing a BaseURL. It trims whitespace before
// evaluation and preserves the relative order of remaining entries.
func (cfg *Config) SanitizeOpenAICompatibility() {
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return
	}
	out := make([]OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		e := cfg.OpenAICompatibility[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		if e.BaseURL == "" {
			// Skip providers with no base-url; treated as removed
			continue
		}
		out = append(out, e)
	}
	cfg.OpenAICompatibility = out
}

// SanitizeCodexKeys removes Codex API key entries missing a BaseURL.
// It trims whitespace and preserves order for remaining entries.
func (cfg *Config) SanitizeCodexKeys() {
	if cfg == nil {
		return
	}
	cfg.CodexKey = sanitizeCodexKeyEntries(cfg.CodexKey)
}

// SanitizeXAIKeys removes xAI API key entries missing a BaseURL.
// It applies the same normalization rules as codex-api-key.
func (cfg *Config) SanitizeXAIKeys() {
	if cfg == nil {
		return
	}
	cfg.XAIKey = sanitizeCodexKeyEntries(cfg.XAIKey)
	for i := range cfg.XAIKey {
		cfg.XAIKey[i].AlphaSearch = false
	}
}

func sanitizeCodexKeyEntries(entries []CodexKey) []CodexKey {
	if len(entries) == 0 {
		return entries
	}
	out := make([]CodexKey, 0, len(entries))
	for i := range entries {
		e := entries[i]
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		e.ExcludedModels = NormalizeExcludedModels(e.ExcludedModels)
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SanitizeClaudeKeys normalizes headers for Claude credentials.
func (cfg *Config) SanitizeClaudeKeys() {
	if cfg == nil || len(cfg.ClaudeKey) == 0 {
		return
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
	}
}

// SanitizeGeminiKeys deduplicates and normalizes Gemini credentials.
// It uses API key + base URL as the uniqueness key.
func (cfg *Config) SanitizeGeminiKeys() {
	// Keep validation before state changes so failures leave existing data intact.
	if cfg == nil {
		return
	}

	seen := make(map[string]struct{}, len(cfg.GeminiKey))
	out := cfg.GeminiKey[:0]
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		uniqueKey := entry.APIKey + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.GeminiKey = out
}

// normalizeModelPrefix normalizes a model prefix.
func normalizeModelPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// looksLikeBcrypt returns true if the provided string appears to be a bcrypt hash.
func looksLikeBcrypt(s string) bool {
	return len(s) > 4 && (s[:4] == "$2a$" || s[:4] == "$2b$" || s[:4] == "$2y$")
}

// NormalizeRemoteManagementSecret normalizes a remote management secret.
func NormalizeRemoteManagementSecret(secret string) (string, bool, error) {
	if secret == "" || looksLikeBcrypt(secret) {
		return secret, false, nil
	}
	hashed, errHash := hashSecret(secret)
	if errHash != nil {
		return "", false, errHash
	}
	return hashed, true, nil
}

// NormalizeHeaders trims header keys and values and removes empty pairs.
func NormalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// NormalizeExcludedModels trims, lowercases, and deduplicates model exclusion patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeOAuthExcludedModels cleans provider -> excluded models mappings by normalizing provider keys
// and applying model exclusion normalization to each entry.
func NormalizeOAuthExcludedModels(entries map[string][]string) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for provider, models := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		normalized := NormalizeExcludedModels(models)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hashSecret hashes the given secret using bcrypt.
func hashSecret(secret string) (string, error) {
	// Use default cost for simplicity.
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashedBytes), nil
}

// SaveConfigPreserveCommentsUpdateNestedScalar updates a nested scalar key path like ["a","b"]
// while preserving comments and positions.
func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err = yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid yaml document structure")
	}
	node := root.Content[0]
	// descend mapping nodes following path
	for i, key := range path {
		if i == len(path)-1 {
			// set final scalar
			v := getOrCreateMapValue(node, key)
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Value = value
		} else {
			next := getOrCreateMapValue(node, key)
			if next.Kind != yaml.MappingNode {
				next.Kind = yaml.MappingNode
				next.Tag = "!!map"
			}
			node = next
		}
	}
	f, err := os.Create(configFile)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("close config file: %v", errClose)
		}
	}()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(&root); err != nil {
		_ = enc.Close()
		return err
	}
	if err = enc.Close(); err != nil {
		return err
	}
	data = NormalizeCommentIndentation(buf.Bytes())
	_, err = f.Write(data)
	return err
}

// NormalizeCommentIndentation removes indentation from standalone YAML comment lines to keep them left aligned.
func NormalizeCommentIndentation(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) == 0 || trimmed[0] != '#' {
			continue
		}
		if len(trimmed) == len(line) {
			continue
		}
		lines[i] = append([]byte(nil), trimmed...)
		changed = true
	}
	if !changed {
		return data
	}
	return bytes.Join(lines, []byte("\n"))
}

// getOrCreateMapValue finds the value node for a given key in a mapping node.
// If not found, it appends a new key/value pair and returns the new value node.
func getOrCreateMapValue(mapNode *yaml.Node, key string) *yaml.Node {
	if mapNode.Kind != yaml.MappingNode {
		mapNode.Kind = yaml.MappingNode
		mapNode.Tag = "!!map"
		mapNode.Content = nil
	}
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		k := mapNode.Content[i]
		if k.Value == key {
			return mapNode.Content[i+1]
		}
	}
	// append new key/value
	mapNode.Content = append(mapNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key})
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
	mapNode.Content = append(mapNode.Content, val)
	return val
}
