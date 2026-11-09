package auth

import (
	"strings"

	"github.com/tidwall/gjson"
)

// sessionSelectionProvider identifies the real credential provider, never the
// mixed scheduling pool. Guardian inheritance must not cross provider boundaries.
func sessionSelectionProvider(auth *Auth, fallback string) string {
	provider := fallback
	if auth != nil && strings.TrimSpace(auth.Provider) != "" {
		provider = auth.Provider
	}
	switch normalized := strings.ToLower(strings.TrimSpace(provider)); normalized {
	case "kimi.com":
		return "kimi"
	case "kimi.ai":
		return "kimi-ai"
	default:
		return normalized
	}
}

// sessionFallbackIsSubagent distinguishes child-session inheritance from content
// continuity. Disabling subagent affinity must not disable forks or hash aliases.
func sessionFallbackIsSubagent(primaryID, fallbackID string, opts Options) bool {
	if primaryID == "" || fallbackID == "" || primaryID == fallbackID {
		return false
	}

	// Older CPA dispatches can wrap derived IDs in the generic header namespace.
	primary := strings.TrimPrefix(primaryID, "header:")
	fallback := strings.TrimPrefix(fallbackID, "header:")
	for _, prefix := range []string{"msg:", "lcp:"} {
		if strings.HasPrefix(primary, prefix) && strings.HasPrefix(fallback, prefix) {
			return false
		}
	}

	nodeKind := sessionHeaderValue(opts.Headers, "X-Node-Kind")
	if nodeKind == "" {
		nodeKind, _ = opts.Metadata["node_kind"].(string)
	}
	switch strings.ToLower(strings.TrimSpace(nodeKind)) {
	case "fork", "compaction":
		return false
	}
	for _, key := range []string{"is_fork", "is_compaction"} {
		if enabled, _ := opts.Metadata[key].(bool); enabled {
			return false
		}
	}

	// Direct selector callers still carry request bodies; older CPA nodes may
	// carry fork metadata only in the Codex turn header instead of node_kind.
	root := gjson.ParseBytes(opts.OriginalRequest)
	sources := []gjson.Result{root}
	if request := root.Get("request"); request.Exists() && !root.Get("contents").Exists() {
		sources = append(sources, request)
	}
	for key, values := range opts.Headers {
		if strings.EqualFold(key, "X-Codex-Turn-Metadata") {
			for _, value := range values {
				sources = append(sources, gjson.Parse(value))
			}
		}
	}
	for _, source := range sources {
		for _, prefix := range []string{"", "metadata.", "extra_body."} {
			switch strings.ToLower(strings.TrimSpace(source.Get(prefix + "node_kind").String())) {
			case "fork", "compaction":
				return false
			}
			if source.Get(prefix+"is_fork").Type == gjson.True || source.Get(prefix+"is_compaction").Type == gjson.True {
				return false
			}
			for _, field := range []string{"forked_from_thread_id", "forked_from_id"} {
				if NormalizeExplicitID(source.Get(prefix+field).String()) != "" {
					return false
				}
			}
		}
	}
	// The extractor's remaining fallbacks are explicit parent references,
	// including Claude agent IDs. They are not alternative names for a session.
	return true
}
