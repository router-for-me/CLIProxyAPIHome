package config

type RequestScopedErrorRule struct {
	// Status matches the HTTP status code of the upstream response (e.g. 400).
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// Match matches substrings in the upstream error body.
	Match []string `yaml:"match,omitempty" json:"match,omitempty"`
	// MatchRegexr matches regular expressions in the upstream error body.
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	// Action specifies the handling behavior: "stop", "stop-and-cooldown", "continue", "continue-and-cooldown".
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}

type ClaudeConfig struct {
	// ModelLevelCooling scopes Claude quota cooldowns to the requested model
	// rather than cooling down the entire credential across all sibling models.
	ModelLevelCooling bool `yaml:"model-level-cooling" json:"model-level-cooling"`
	// ToolSearch is a CPA option (upstream.claude.tool-search) that enables Codex
	// client tool_search on Claude Responses routes. Home forwards it to CPA.
	ToolSearch bool `yaml:"tool-search" json:"tool-search"`
}

type XAIConfig struct {
	// InjectXSearch injects xAI's native x_search tool when the request does not declare it.
	InjectXSearch bool `yaml:"inject-x-search" json:"inject-x-search"`
}

type DevinConfig struct {
	// SensitiveWords is a list of words to obfuscate with zero-width characters in system prompts and messages.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// SensitiveWordsReplace maps target substrings to literal replacement text in system prompts.
	SensitiveWordsReplace map[string]string `yaml:"sensitive-words-replace,omitempty" json:"sensitive-words-replace,omitempty"`
}

type AntigravityConnectionPoolConfig struct {
	// Enabled controls whether upstream connection pooling is enabled.
	// Defaults to false (short-lived connection mode). Set to true to enable connection pooling.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// IdleConnTimeout specifies how long an idle connection stays in the pool before expiring.
	// Defaults to "30s". Capped at 210s to prevent exceeding Google Frontend (GFE) 240s cutoff.
	IdleConnTimeout string `yaml:"idle-conn-timeout,omitempty" json:"idle-conn-timeout,omitempty"`

	// MaxIdleConnsPerHost specifies the maximum number of idle connections to retain per host per credential.
	// Defaults to 2.
	MaxIdleConnsPerHost *int `yaml:"max-idle-conns-per-host,omitempty" json:"max-idle-conns-per-host,omitempty"`
}

type CodexConfig struct {
	// DisableCodexCloaking disables forcing the official Codex identity headers on HTTP/SSE and WebSocket requests.
	DisableCodexCloaking bool `yaml:"disable-codex-cloaking" json:"disable-codex-cloaking"`
	// StreamBootstrapBuffering holds back the frames that arrive before generation starts, none of
	// which the client has seen anything from - the handshake (response.created, response.in_progress,
	// the websocket metadata frames), keepalive heartbeats, and the *.added announcements of an item
	// or part that is still empty - until the first generated event arrives. The upstream delivers
	// server_is_overloaded rejections inside an HTTP 200 stream right after those frames instead of
	// returning 503 on the wire, so buffering them keeps the downstream response headers uncommitted
	// long enough to retry on another credential. Trade-off: the response headers are delayed until
	// the upstream starts generating, which on a slow reasoning turn now means several heartbeat
	// intervals rather than one, and can trip client or reverse-proxy read timeouts. The hold is
	// bounded by a frame and a byte budget, not by wall-clock time, and neither budget is advanced
	// by a websocket peer that sends only control frames or by an upstream that never terminates an
	// SSE line. Only overload and rate-limit rejections fail over deliberately. A stream that ends
	// while the bootstrap is still holding ends the attempt rather than reaching the client, and
	// what follows is pre-existing but now far more likely, since the hold can span the whole
	// reasoning phase instead of ending at the first keepalive: a clean end with no terminal event
	// is request-scoped on SSE and stops there, while a websocket close or a transport error on
	// either transport is not, so the request may be retried on another credential.
	// Default is false.
	StreamBootstrapBuffering bool `yaml:"stream-bootstrap-buffering" json:"stream-bootstrap-buffering"`
	// StreamBootstrapTimeout specifies an optional maximum duration to hold back uncommitted response
	// headers during bootstrap buffering before releasing the stream to the client.
	// Defaults to "0" (unlimited time, relying purely on the 48-frame and 1MB byte bounds).
	// When set (e.g. "20s"), the stream is released once the time ceiling is reached, avoiding
	// reverse-proxy timeouts (e.g. Nginx 60s proxy_read_timeout).
	StreamBootstrapTimeout string `yaml:"stream-bootstrap-timeout,omitempty" json:"stream-bootstrap-timeout,omitempty"`
	// OptimizeMultiAgentV2 optimizes official Codex multi-agent requests.
	OptimizeMultiAgentV2 bool `yaml:"optimize-multi-agent-v2" json:"optimize-multi-agent-v2"`
	// OrphanDelegationCompatibility enables opt-in compatibility for orphan Codex delegation outputs.
	OrphanDelegationCompatibility bool `yaml:"orphan-delegation-compatibility" json:"orphan-delegation-compatibility"`
	// ModelLevelCooling scopes Codex usage_limit_reached quota cooldowns to the requested model
	// rather than cooling down the entire credential across all sibling models.
	ModelLevelCooling bool `yaml:"model-level-cooling" json:"model-level-cooling"`
	// LiveMediaRelay terminates and relays Codex Live WebRTC media in this process.
	LiveMediaRelay CodexLiveMediaRelayConfig `yaml:"live-media-relay" json:"live-media-relay"`
	// ResponseSteering enables full-duplex Codex WebSockets, bound to one
	// upstream model/account/socket for their entire lifetime. Default is false.
	ResponseSteering bool `yaml:"response-steering" json:"response-steering"`
}

type CodexLiveMediaRelayConfig struct {
	Enabled                 bool                 `yaml:"enabled" json:"enabled"`
	MaxSessions             int                  `yaml:"max-sessions" json:"max-sessions"`
	DisablePrivateRemoteIPs bool                 `yaml:"disable-private-remote-ips" json:"disable-private-remote-ips"`
	PublicIP                string               `yaml:"public-ip" json:"public-ip"`
	UDPPortMin              uint16               `yaml:"udp-port-min" json:"udp-port-min"`
	UDPPortMax              uint16               `yaml:"udp-port-max" json:"udp-port-max"`
	ICEServers              []CodexLiveICEServer `yaml:"ice-servers" json:"ice-servers"`
}

type CodexLiveICEServer struct {
	URLs       []string `yaml:"urls" json:"urls"`
	Username   string   `yaml:"username" json:"-"`
	Credential string   `yaml:"credential" json:"-"`
}

type DiscoveryInterfacesConfig struct {
	Include []string `yaml:"include" json:"include"`
	Exclude []string `yaml:"exclude" json:"exclude"`
}

type DiscoveryConfig struct {
	// Enabled toggles mDNS service advertising on the local network (default: false).
	Enabled bool `yaml:"enabled" json:"enabled"`

	// ServiceName is the optional custom instance name. When empty, defaults to CPA-<ShortID>.
	ServiceName string `yaml:"service-name" json:"service-name"`

	// ServiceType is the DNS-SD service type (default: _ai-gateway._tcp).
	ServiceType string `yaml:"service-type" json:"service-type"`

	// Subtypes specifies DNS-SD API protocol subtypes to advertise (e.g. _responses, _messages, _generate-content).
	Subtypes []string `yaml:"subtypes" json:"subtypes"`

	// Interfaces specifies network interface filtering rules.
	Interfaces DiscoveryInterfacesConfig `yaml:"interfaces" json:"interfaces"`

	// AuthRequired indicates whether authentication is required for client calls (default: true).
	AuthRequired *bool `yaml:"auth-required" json:"auth-required"`

	// AdvertiseManagement explicitly controls whether management endpoints are exposed (default: false).
	AdvertiseManagement bool `yaml:"advertise-management" json:"advertise-management"`
}
