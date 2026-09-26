package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPIHome/internal/logging"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
type RoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]int
	maxKeys int
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model           string
	resetIn         time.Duration
	provider        string
	requestRetry    int
	hasRequestRetry bool
}

// ErrorCode identifies the structured dispatch error exposed to CPA.
func (e *modelCooldownError) ErrorCode() string {
	return "model_cooldown"
}

// ErrorMessage returns the stable human-readable dispatch error message.
func (e *modelCooldownError) ErrorMessage() string {
	if e == nil {
		return "all credentials are cooling down"
	}
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	return message
}

// RetryAfter returns the earliest credential recovery delay.
func (e *modelCooldownError) RetryAfter() *time.Duration {
	if e == nil || e.resetIn <= 0 {
		return nil
	}
	value := e.resetIn
	return &value
}

// RequestRetryLimit returns the effective additional-round limit for the
// credentials represented by this cooldown error.
func (e *modelCooldownError) RequestRetryLimit() (int, bool) {
	if e == nil || !e.hasRequestRetry {
		return 0, false
	}
	return e.requestRetry, true
}

// newModelCooldownError creates a model cooldown error.
func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

// Error returns the error message.
func (e *modelCooldownError) Error() string {
	// Keep validation before state changes so failures leave existing data intact.
	message := e.ErrorMessage()
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

// StatusCode returns the HTTP status code.
func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

// Headers returns response headers associated with the error.
func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

// authPriority handles an auth priority.
func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

// CanonicalModelID removes a request-option suffix and surrounding whitespace from a model ID.
func CanonicalModelID(model string) string {
	return canonicalModelKey(model)
}

// canonicalModelKey handles a canonical model key.
func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := parseModelSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

// authWebsocketsEnabled handles an auth websockets enabled.
func authWebsocketsEnabled(auth *Auth) bool {
	// Normalize auth state before updating runtime indexes.
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

// authRateLimitWarned reports whether a candidate currently carries an
// active rate-limit warning for any window. It is read-only: it never
// mutates auth.RateLimitWarnings (that pruning happens only on the
// result/mutation path, where the manager lock or StateMutator is held;
// selection can run without holding it).
//
// Iterates the fixed claudeRateLimitWarningWindows array (result.go) and
// does a keyed lookup per window rather than ranging auth.RateLimitWarnings
// directly: selection runs unlocked while MarkResult mutates that same map
// under a lock elsewhere, and ranging a map that is concurrently written is
// the shape that triggers Go's fatal "concurrent map iteration and map
// write" error, which would take the whole router down. A keyed lookup is
// the same shape as the pre-existing unsynchronized keyed read of
// auth.ModelStates in isAuthBlockedForModel below -- it does not introduce
// a new crash mode, it just stops amplifying the existing one. This does
// NOT add locking. The underlying lack of synchronization is
// pre-existing in this package and is deliberately left out of scope
// for this change rather than fixed here.
func authRateLimitWarned(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.RateLimitWarnings) == 0 {
		return false
	}
	for _, window := range claudeRateLimitWarningWindows {
		if warning, ok := auth.RateLimitWarnings[window]; ok && rateLimitWarningActive(warning, now) {
			return true
		}
	}
	return false
}

// collectAvailableByPriority handles a collect available by priority. It
// returns two priority-bucketed maps: available (never warned) and warned
// (carrying an active rate-limit warning but otherwise fully serviceable).
// Callers must prefer available and fall back to warned only when available
// is empty, so a warned-but-usable credential is never treated as absent.
func collectAvailableByPriority(auths []*Auth, model string, now time.Time) (available map[int][]*Auth, warned map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	warned = make(map[int][]*Auth)
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			priority := authPriority(candidate)
			if authRateLimitWarned(candidate, now) {
				warned[priority] = append(warned[priority], candidate)
			} else {
				available[priority] = append(available[priority], candidate)
			}
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	return available, warned, cooldownCount, earliest
}

// bestPriorityBucket returns the highest-priority non-empty bucket from a
// priority-keyed map, sorted by ID within the bucket, or nil if the map is
// empty.
func bestPriorityBucket(byPriority map[int][]*Auth) []*Auth {
	bestPriority := 0
	found := false
	for priority := range byPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}
	if !found {
		return nil
	}
	best := byPriority[bestPriority]
	if len(best) > 1 {
		sort.Slice(best, func(i, j int) bool { return best[i].ID < best[j].ID })
	}
	return best
}

// getAvailableAuths returns an available auths. A candidate warned by a
// provider early-warning header is de-preferred (only used when no
// never-warned candidate exists at any priority) but is NEVER treated as
// unavailable: never-starve is structural here, not a runtime guard — the
// warned bucket is consulted before any error is built.
func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	// Build the candidate view before applying availability rules.
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, warnedByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now)
	if len(availableByPriority) == 0 && len(warnedByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	if best := bestPriorityBucket(availableByPriority); len(best) > 0 {
		return best, nil
	}
	return bestPriorityBucket(warnedByPriority), nil
}

func filterConcurrencyExcludedAuths(auths []*Auth, model string, opts Options) []*Auth {
	if len(excludedConcurrencyCandidatesFromOptions(opts)) == 0 {
		return auths
	}
	filtered := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || concurrencyCandidateExcluded(opts, auth.ID, model) {
			continue
		}
		filtered = append(filtered, auth)
	}
	return filtered
}

// Pick selects the next available auth for the provider in a round-robin manner.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(filterConcurrencyExcludedAuths(auths, model, opts), provider, model, now)
	if err != nil {
		return nil, err
	}
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	s.ensureCursorKey(key, limit)
	index := s.cursors[key]
	if index >= 2_147_483_640 {
		index = 0
	}
	s.cursors[key] = index + 1
	s.mu.Unlock()
	return available[index%len(available)], nil
}

// ensureCursorKey ensures the cursor map has capacity for the given key.
// Must be called with s.mu held.
func (s *RoundRobinSelector) ensureCursorKey(key string, limit int) {
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]int)
	}
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(filterConcurrencyExcludedAuths(auths, model, opts), provider, model, now)
	if err != nil {
		return nil, err
	}
	return available[0], nil
}

// isAuthBlockedForModel reports whether auth blocked for model.
func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	// Normalize source data before building the derived payload.
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if RefreshBlocksDispatch(auth) {
		return true, blockReasonOther, auth.NextRetryAfter
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			if (!ok || state == nil) && model != "" {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		return false, blockReasonNone, time.Time{}
	}
	return false, blockReasonNone, time.Time{}
}

// sessionPattern matches Claude Code user_id format:
// user_{hash}_account__session_{uuid}
var sessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// SessionAffinitySelector wraps another selector with session-sticky behavior.
// It extracts session ID from multiple sources and maintains session-to-auth
// mappings with automatic failover when the bound auth becomes unavailable.
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
}

// SessionAffinityConfig configures the session affinity selector.
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// NewSessionAffinitySelectorWithConfig creates a selector with custom configuration.
func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	return &SessionAffinitySelector{
		fallback: cfg.Fallback,
		cache:    NewSessionCache(cfg.TTL),
	}
}

// Pick selects an auth with session affinity when possible.
// Priority for session ID extraction:
//  1. Anthropic / Claude Code headers (X-Claude-Code-Session-Id, X-Claude-Code-Agent-Id)
//  2. Claude Code metadata.user_id in payload (user_xxx_account__session_xxx)
//  3. OpenAI / Codex CLI headers (Session-Id, Session_id, X-Codex-Parent-Thread-Id)
//  4. Antigravity CLI headers (X-Http-Session-Id)
//  5. Explicit session headers (X-Session-ID, X-Session-Affinity, X-Slot-Session-Id)
//  6. Conversation / Client headers (X-Conversation-Id, X-Thread-Id, X-Client-Request-Id)
//  7. Body session_id / sessionId (root or nested request)
//  8. Body metadata.user_id (non-Claude Code format)
//  9. Body conversation_id
//  10. Hash-based fallback from message content
//
// Note: The cache key includes provider, session ID, and model to handle cases where
// a session uses multiple models (e.g., gemini-2.5-pro and gemini-3-flash-preview)
// that may be supported by different auth credentials, and to avoid cross-provider conflicts.
func (s *SessionAffinitySelector) Pick(ctx context.Context, provider, model string, opts Options, auths []*Auth) (*Auth, error) {
	entry := selectorLogEntry(ctx)
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		entry.Debugf("session-affinity: no session ID extracted, falling back to default selector | provider=%s model=%s", provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}

	now := time.Now()
	auths = filterConcurrencyExcludedAuths(auths, model, opts)
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	cacheKey := provider + "::" + primaryID + "::" + model

	if cachedAuthID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		for _, auth := range available {
			if auth.ID == cachedAuthID {
				entry.Infof("session-affinity: cache hit | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
				return auth, nil
			}
		}
		// Cached auth not available, reselect via fallback selector for even distribution
		auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
		if err != nil {
			return nil, err
		}
		s.cache.Set(cacheKey, auth.ID)
		entry.Infof("session-affinity: cache hit but auth unavailable, reselected | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
		return auth, nil
	}

	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := provider + "::" + fallbackID + "::" + model
		if cachedAuthID, ok := s.cache.Get(fallbackKey); ok {
			for _, auth := range available {
				if auth.ID == cachedAuthID {
					s.cache.Set(cacheKey, auth.ID)
					entry.Infof("session-affinity: fallback cache hit | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
					return auth, nil
				}
			}
		}
	}

	auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
	if err != nil {
		return nil, err
	}
	s.cache.Set(cacheKey, auth.ID)
	entry.Infof("session-affinity: cache miss, new binding | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
	return auth, nil
}

// selectorLogEntry handles a selector log entry.
func selectorLogEntry(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

// truncateSessionID shortens session ID for logging (first 8 chars + "...")
func truncateSessionID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:8] + "..."
}

// Stop releases resources held by the selector.
func (s *SessionAffinitySelector) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
}

// InvalidateAuth removes all session bindings for a specific auth.
// Called when an auth becomes rate-limited or unavailable.
func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
}

// NormalizeExplicitID validates and cleans an explicit session identifier.
// It trims whitespace, rejects control characters, and rejects IDs exceeding 256 bytes.
func NormalizeExplicitID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return raw
}

var knownSessionPrefixes = []string{
	"claude:",
	"codex:",
	"header:",
	"slot:",
	"affinity:",
	"agy:",
	"thread:",
	"conv:",
	"user:",
	"clientreq:",
	"ctx:",
	"geminicache:",
	"session:",
	"lcp:",
}

func hasKnownSessionPrefix(id string) bool {
	for _, prefix := range knownSessionPrefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func formatSessionID(raw, defaultPrefix string) string {
	raw = NormalizeExplicitID(raw)
	if raw == "" {
		return ""
	}
	if hasKnownSessionPrefix(raw) {
		return raw
	}
	return defaultPrefix + raw
}

func sessionHeaderValue(headers http.Header, names ...string) string {
	if headers == nil {
		return ""
	}
	for _, name := range names {
		if val := NormalizeExplicitID(headers.Get(name)); val != "" {
			return val
		}
		for key, values := range headers {
			if strings.EqualFold(key, name) {
				for _, v := range values {
					if trimmed := NormalizeExplicitID(v); trimmed != "" {
						return trimmed
					}
				}
			}
		}
	}
	return ""
}

// extractSessionIDs returns (primaryID, fallbackID) for session affinity.
// primaryID: session identity (or full message hash)
// fallbackID: parent session identity (or short message hash without assistant)
func extractSessionIDs(headers http.Header, payload []byte, _ map[string]any) (string, string) {
	// Extract parent candidate from payload once if payload is non-empty
	var parentCandidate string
	var parsedRoot gjson.Result
	var reqRoot gjson.Result
	hasNestedReq := false
	if len(payload) > 0 {
		parsedRoot = gjson.ParseBytes(payload)
		req := parsedRoot.Get("request")
		hasNestedReq = req.Exists() && !parsedRoot.Get("contents").Exists()
		if hasNestedReq {
			reqRoot = req
		}
		for _, parentPath := range []string{
			"parent_session_id", "parentSessionId",
			"parent_thread_id", "parentThreadId",
			"forked_from_thread_id", "forked_from_id",
			"parent_conversation_id", "parentConversationId",
			"metadata.parent_session_id", "metadata.parent_thread_id",
			"extra_body.parent_session_id", "extra_body.parent_thread_id",
		} {
			if psid := NormalizeExplicitID(parsedRoot.Get(parentPath).String()); psid != "" {
				parentCandidate = psid
				break
			}
			if hasNestedReq {
				if psid := NormalizeExplicitID(reqRoot.Get(parentPath).String()); psid != "" {
					parentCandidate = psid
					break
				}
			}
		}
	}

	// 1. Anthropic / Claude Code headers
	if sid := sessionHeaderValue(headers, "X-Claude-Code-Session-Id"); sid != "" {
		agentID := sessionHeaderValue(headers, "X-Claude-Code-Agent-Id")
		parentAgentID := sessionHeaderValue(headers, "X-Claude-Code-Parent-Agent-Id")
		if agentID != "" && agentID != "main" {
			primary := formatSessionID(sid+":agent:"+agentID, "claude:")
			fallback := formatSessionID(sid, "claude:")
			if parentAgentID != "" && parentAgentID != "main" && parentAgentID != agentID {
				fallback = formatSessionID(sid+":agent:"+parentAgentID, "claude:")
			} else if parentCandidate != "" && parentCandidate != sid {
				fallback = formatSessionID(parentCandidate, "claude:")
			}
			return primary, fallback
		}
		primary := formatSessionID(sid, "claude:")
		var fallback string
		if parentCandidate != "" && parentCandidate != sid {
			fallback = formatSessionID(parentCandidate, "claude:")
		}
		return primary, fallback
	}

	// 2. metadata.user_id with Claude Code session format
	if len(payload) > 0 {
		userID := strings.TrimSpace(parsedRoot.Get("metadata.user_id").String())
		if userID == "" && hasNestedReq {
			userID = strings.TrimSpace(reqRoot.Get("metadata.user_id").String())
		}
		if userID != "" {
			if len(userID) > 0 && userID[0] == '{' {
				parsed := gjson.Parse(userID)
				if sid := NormalizeExplicitID(parsed.Get("session_id").String()); sid != "" {
					agentID := NormalizeExplicitID(parsed.Get("agent_id").String())
					parentSID := NormalizeExplicitID(parsed.Get("parent_session_id").String())
					if parentSID == "" {
						parentSID = parentCandidate
					}
					if agentID != "" && agentID != "main" {
						primary := formatSessionID(sid+":agent:"+agentID, "claude:")
						fallback := formatSessionID(sid, "claude:")
						if parentSID != "" && parentSID != sid {
							fallback = formatSessionID(parentSID, "claude:")
						}
						return primary, fallback
					}
					primary := formatSessionID(sid, "claude:")
					var fallback string
					if parentSID != "" && parentSID != sid {
						fallback = formatSessionID(parentSID, "claude:")
					}
					return primary, fallback
				}
			}
			if matches := sessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
				if sid := NormalizeExplicitID(matches[1]); sid != "" {
					primary := formatSessionID(sid, "claude:")
					var fallback string
					if parentCandidate != "" && parentCandidate != sid {
						fallback = formatSessionID(parentCandidate, "claude:")
					}
					return primary, fallback
				}
			}
		}
	}

	// 3. Codex Session-Id / Session_id
	if sid := sessionHeaderValue(headers, "Session-Id", "Session_id"); sid != "" {
		parentThreadID := sessionHeaderValue(headers, "x-codex-parent-thread-id", "X-Codex-Parent-Thread-Id", "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentThreadID == "" {
			parentThreadID = parentCandidate
		}
		primary := formatSessionID(sid, "codex:")
		var fallback string
		if parentThreadID != "" && parentThreadID != sid {
			fallback = formatSessionID(parentThreadID, "codex:")
		}
		return primary, fallback
	}

	// 4. Antigravity CLI / Google Cloud Code
	if sid := sessionHeaderValue(headers, "X-Http-Session-Id"); sid != "" {
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentSID == "" {
			parentSID = parentCandidate
		}
		primary := formatSessionID(sid, "agy:")
		var fallback string
		if parentSID != "" && parentSID != sid {
			fallback = formatSessionID(parentSID, "agy:")
		}
		return primary, fallback
	}

	// 5. X-Session-ID (universal header used by CPA Home dispatch and generic clients)
	if sid := sessionHeaderValue(headers, "X-Session-ID"); sid != "" {
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentSID == "" {
			parentSID = parentCandidate
		}
		primary := formatSessionID(sid, "header:")
		var fallback string
		if parentSID != "" && parentSID != sid {
			fallback = formatSessionID(parentSID, "header:")
		}
		return primary, fallback
	}

	// 6. X-Session-Affinity (OpenCode)
	if sid := sessionHeaderValue(headers, "X-Session-Affinity"); sid != "" {
		parentAffinity := sessionHeaderValue(headers, "X-Parent-Session-Affinity", "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentAffinity == "" {
			parentAffinity = parentCandidate
		}
		primary := formatSessionID(sid, "affinity:")
		var fallback string
		if parentAffinity != "" && parentAffinity != sid {
			fallback = formatSessionID(parentAffinity, "affinity:")
		}
		return primary, fallback
	}

	// 7. X-Slot-Session-Id (Pi Slot)
	if sid := sessionHeaderValue(headers, "X-Slot-Session-Id"); sid != "" {
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentSID == "" {
			parentSID = parentCandidate
		}
		primary := formatSessionID(sid, "slot:")
		var fallback string
		if parentSID != "" && parentSID != sid {
			fallback = formatSessionID(parentSID, "slot:")
		}
		return primary, fallback
	}

	// 8. X-Conversation-Id / X-Thread-Id
	if sid := sessionHeaderValue(headers, "X-Conversation-Id", "X-Conversation-ID"); sid != "" {
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentSID == "" {
			parentSID = parentCandidate
		}
		primary := formatSessionID(sid, "conv:")
		var fallback string
		if parentSID != "" && parentSID != sid {
			fallback = formatSessionID(parentSID, "conv:")
		}
		return primary, fallback
	}
	if sid := sessionHeaderValue(headers, "X-Thread-Id", "X-Thread-ID", "Thread-Id"); sid != "" {
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID", "X-Parent-Session-Id")
		if parentSID == "" {
			parentSID = parentCandidate
		}
		primary := formatSessionID(sid, "thread:")
		var fallback string
		if parentSID != "" && parentSID != sid {
			fallback = formatSessionID(parentSID, "thread:")
		}
		return primary, fallback
	}

	// 9. X-Client-Request-Id header (PI)
	if rid := sessionHeaderValue(headers, "X-Client-Request-Id"); rid != "" {
		return formatSessionID(rid, "clientreq:"), ""
	}

	if len(payload) == 0 {
		return "", ""
	}

	// 10. explicit session_id / sessionId in payload root
	for _, path := range []string{"session_id", "sessionId"} {
		if sid := NormalizeExplicitID(parsedRoot.Get(path).String()); sid != "" {
			primary := formatSessionID(sid, "header:")
			var fallback string
			if parentCandidate != "" && parentCandidate != sid {
				fallback = formatSessionID(parentCandidate, "header:")
			}
			return primary, fallback
		}
		if hasNestedReq {
			if sid := NormalizeExplicitID(reqRoot.Get(path).String()); sid != "" {
				primary := formatSessionID(sid, "header:")
				var fallback string
				if parentCandidate != "" && parentCandidate != sid {
					fallback = formatSessionID(parentCandidate, "header:")
				}
				return primary, fallback
			}
		}
	}

	// 11. metadata.user_id (non-Claude Code format)
	rawUserID := NormalizeExplicitID(parsedRoot.Get("metadata.user_id").String())
	if rawUserID == "" && hasNestedReq {
		rawUserID = NormalizeExplicitID(reqRoot.Get("metadata.user_id").String())
	}
	if rawUserID != "" {
		return formatSessionID(rawUserID, "user:"), ""
	}

	// 12. conversation_id field in payload
	convID := NormalizeExplicitID(parsedRoot.Get("conversation_id").String())
	if convID == "" && hasNestedReq {
		convID = NormalizeExplicitID(reqRoot.Get("conversation_id").String())
	}
	if convID != "" {
		primary := formatSessionID(convID, "conv:")
		var fallback string
		if parentCandidate != "" && parentCandidate != convID {
			fallback = formatSessionID(parentCandidate, "conv:")
		}
		return primary, fallback
	}

	// 13. Hash-based fallback from message content
	return extractMessageHashIDs(payload)
}

// extractMessageHashIDs extracts a message hash i ds.
func extractMessageHashIDs(payload []byte) (primaryID, fallbackID string) {
	var systemPrompt, firstUserMsg, firstAssistantMsg string

	// OpenAI/Claude messages format
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := extractMessageContent(msg.Get("content"))
			if content == "" {
				return true
			}

			switch role {
			case "system":
				if systemPrompt == "" {
					systemPrompt = truncateString(content, 100)
				}
			case "user":
				if firstUserMsg == "" {
					firstUserMsg = truncateString(content, 100)
				}
			case "assistant":
				if firstAssistantMsg == "" {
					firstAssistantMsg = truncateString(content, 100)
				}
			}

			if systemPrompt != "" && firstUserMsg != "" && firstAssistantMsg != "" {
				return false
			}
			return true
		})
	}

	// Claude API: top-level "system" field (array or string)
	if systemPrompt == "" {
		topSystem := gjson.GetBytes(payload, "system")
		if topSystem.Exists() {
			if topSystem.IsArray() {
				topSystem.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text").String(); text != "" && systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
						return false
					}
					return true
				})
			} else if topSystem.Type == gjson.String {
				systemPrompt = truncateString(topSystem.String(), 100)
			}
		}
	}

	// Gemini format
	if systemPrompt == "" && firstUserMsg == "" {
		sysInstr := gjson.GetBytes(payload, "systemInstruction.parts")
		if sysInstr.Exists() && sysInstr.IsArray() {
			sysInstr.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" && systemPrompt == "" {
					systemPrompt = truncateString(text, 100)
					return false
				}
				return true
			})
		}

		contents := gjson.GetBytes(payload, "contents")
		if contents.Exists() && contents.IsArray() {
			contents.ForEach(func(_, msg gjson.Result) bool {
				role := msg.Get("role").String()
				msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
					text := part.Get("text").String()
					if text == "" {
						return true
					}
					switch role {
					case "user":
						if firstUserMsg == "" {
							firstUserMsg = truncateString(text, 100)
						}
					case "model":
						if firstAssistantMsg == "" {
							firstAssistantMsg = truncateString(text, 100)
						}
					}
					return false
				})
				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	// OpenAI Responses API format (v1/responses)
	if systemPrompt == "" && firstUserMsg == "" {
		if instr := gjson.GetBytes(payload, "instructions").String(); instr != "" {
			systemPrompt = truncateString(instr, 100)
		}

		input := gjson.GetBytes(payload, "input")
		if input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				itemType := item.Get("type").String()
				if itemType == "reasoning" {
					return true
				}
				// Skip non-message typed items (function_call, function_call_output, etc.)
				// but allow items with no type that have a role (inline message format).
				if itemType != "" && itemType != "message" {
					return true
				}

				role := item.Get("role").String()
				if itemType == "" && role == "" {
					return true
				}

				// Handle both string content and array content (multimodal).
				content := item.Get("content")
				var text string
				if content.Type == gjson.String {
					text = content.String()
				} else {
					text = extractResponsesAPIContent(content)
				}
				if text == "" {
					return true
				}

				switch role {
				case "developer", "system":
					if systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
					}
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateString(text, 100)
					}
				case "assistant":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateString(text, 100)
					}
				}

				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	if systemPrompt == "" && firstUserMsg == "" {
		return "", ""
	}

	shortHash := computeSessionHash(systemPrompt, firstUserMsg, "")
	if firstAssistantMsg == "" {
		return shortHash, ""
	}

	fullHash := computeSessionHash(systemPrompt, firstUserMsg, firstAssistantMsg)
	return fullHash, shortHash
}

// computeSessionHash computes a session hash.
func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	h := fnv.New64a()
	if systemPrompt != "" {
		h.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		h.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		h.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

// truncateString truncates a string.
func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// extractMessageContent extracts text content from a message content field.
// Handles both string content and array content (multimodal messages).
// For array content, extracts text from all text-type elements.
func extractMessageContent(content gjson.Result) string {
	// String content: "Hello world"
	if content.Type == gjson.String {
		return content.String()
	}

	// Array content: [{"type":"text","text":"Hello"},{"type":"image",...}]
	if content.IsArray() {
		var texts []string
		content.ForEach(func(_, part gjson.Result) bool {
			// Handle Claude format: {"type":"text","text":"content"}
			if part.Get("type").String() == "text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			// Handle OpenAI format: {"type":"text","text":"content"}
			// Same structure as Claude, already handled above
			return true
		})
		if len(texts) > 0 {
			return strings.Join(texts, " ")
		}
	}

	return ""
}

// extractResponsesAPIContent extracts a responses api content.
func extractResponsesAPIContent(content gjson.Result) string {
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "input_text" || partType == "output_text" || partType == "text" {
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	if len(texts) > 0 {
		return strings.Join(texts, " ")
	}
	return ""
}
