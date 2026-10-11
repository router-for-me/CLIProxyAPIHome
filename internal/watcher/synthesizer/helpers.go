package synthesizer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/diff"
)

// StableIDGenerator generates stable, deterministic IDs for auth entries.
// It uses SHA256 hashing with collision handling via counters.
// It is not safe for concurrent use.
type StableIDGenerator struct {
	counters map[string]int
}

// NewStableIDGenerator creates a new StableIDGenerator instance.
func NewStableIDGenerator() *StableIDGenerator {
	return &StableIDGenerator{counters: make(map[string]int)}
}

// Next generates a stable ID based on the kind and parts.
// Returns the full ID (kind:hash) and the short hash portion.
func (g *StableIDGenerator) Next(kind string, parts ...string) (string, string) {
	// Keep validation before state changes so failures leave existing data intact.
	if g == nil {
		return kind + ":000000000000", "000000000000"
	}
	hasher := sha256.New()
	hasher.Write([]byte(kind))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		hasher.Write([]byte{0})
		hasher.Write([]byte(trimmed))
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if len(digest) < 12 {
		digest = fmt.Sprintf("%012s", digest)
	}
	short := digest[:12]
	key := kind + ":" + short
	index := g.counters[key]
	g.counters[key] = index + 1
	if index > 0 {
		short = fmt.Sprintf("%s-%d", short, index)
	}
	return fmt.Sprintf("%s:%s", kind, short), short
}

// ApplyAuthExcludedModelsMeta stores the credential's own excluded models on an auth entry.
// It computes a hash of excluded models and sets the auth_kind attribute.
// Global oauth-excluded-models are intentionally not merged here: they are resolved at
// model registration time, and a non-empty credential list overrides them.
func ApplyAuthExcludedModelsMeta(auth *coreauth.Auth, perKey []string, authKind string) {
	if auth == nil {
		return
	}
	seen := make(map[string]struct{})
	for _, entry := range perKey {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			seen[strings.ToLower(trimmed)] = struct{}{}
		}
	}
	combined := make([]string, 0, len(seen))
	for k := range seen {
		combined = append(combined, k)
	}
	sort.Strings(combined)
	hash := diff.ComputeExcludedModelsHash(combined)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if hash != "" {
		auth.Attributes["excluded_models_hash"] = hash
	}
	// Store the combined excluded models list so that routing can read it at runtime
	if len(combined) > 0 {
		auth.Attributes["excluded_models"] = strings.Join(combined, ",")
	}
	if authKind != "" {
		auth.Attributes["auth_kind"] = authKind
	}
}

// SyncOAuthExcludedModelsAttributes rebuilds the excluded_models attributes of an OAuth auth
// from its metadata, dropping stale values first because ApplyAuthExcludedModelsMeta never clears them.
// Invalid metadata returns an error and leaves the attributes untouched.
func SyncOAuthExcludedModelsAttributes(auth *coreauth.Auth) error {
	if auth == nil {
		return nil
	}
	excluded, errExcluded := ExtractExcludedModelsFromMetadata(auth.Metadata)
	if errExcluded != nil {
		return errExcluded
	}
	if auth.Attributes != nil {
		delete(auth.Attributes, "excluded_models")
		delete(auth.Attributes, "excluded_models_hash")
	}
	ApplyAuthExcludedModelsMeta(auth, excluded, "oauth")
	return nil
}

// addConfigHeadersToAttrs adds header configuration to auth attributes.
// Headers are prefixed with "header:" in the attributes map.
func addConfigHeadersToAttrs(headers map[string]string, attrs map[string]string) {
	if len(headers) == 0 || attrs == nil {
		return
	}
	for hk, hv := range headers {
		key := strings.TrimSpace(hk)
		val := strings.TrimSpace(hv)
		if key == "" || val == "" {
			continue
		}
		attrs["header:"+key] = val
	}
}
