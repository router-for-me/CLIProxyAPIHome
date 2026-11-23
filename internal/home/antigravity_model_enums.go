package home

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityModelEnumScopeAttribute = "antigravity_model_enum_scope"
	antigravityModelEnumKeyPrefix      = "antigravity:model-enums:v1:"
	antigravityModelEnumSequenceKey    = "antigravity:model-enum-probe-sequence:v1"
	antigravityModelEnumTTL            = 7 * 24 * time.Hour
)

func antigravityModelEnumScope(auth *coreauth.Auth) string {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") || auth.ID == "" || auth.RuntimeProjection {
		return ""
	}
	grant, _ := auth.Metadata["refresh_token"].(string)
	if grant = strings.TrimSpace(grant); grant == "" {
		grant = extractAccessToken(auth)
	}
	if grant == "" {
		return ""
	}
	// The durable identity version changes on a same-ID restoration; a refresh
	// grant additionally isolates credential replacements without fencing out an
	// ordinary access-token rotation.
	identity := fmt.Sprintf("%q/%d/%q/%q/%q/%q", auth.ID, max(auth.CredentialIdentityVersion, 1), auth.CreatedAt.UTC().Format(time.RFC3339Nano), antigravityProjectID(auth), antigravityModelBaseURLs(auth)[0], grant)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

func antigravityModelProbeKey(cacheKey, enumScope, accessToken string) string {
	return cacheKey + fmt.Sprintf("/%s/%x", enumScope, sha256.Sum256([]byte(accessToken)))
}

type antigravityModelEnumPublication struct {
	// Zero means reservation failed and a new probe must reserve its own
	// version. Do not keep the unversioned catalog for publication.
	version           int64
	models            map[string]string
	thinkingGroups    map[string]map[string]int
	thinkingWebSearch map[string]bool
}

// Home nodes allocate a shared version before making the upstream request.
// A process-local revision or wall time cannot order overlapping probes.
func (r *Runtime) reserveAntigravityModelEnumVersion(ctx context.Context, scope string) (version int64, enabled bool) {
	if scope == "" || ctx.Err() != nil {
		return 0, false
	}
	store, errStore := r.kvStore()
	if errStore != nil {
		return 0, false
	}
	version, errIncr := store.KVIncrBy(ctx, antigravityModelEnumSequenceKey, 1)
	if errIncr != nil || version <= 0 {
		return 0, true
	}
	return version, true
}

func (r *Runtime) publishAntigravityModelEnums(ctx context.Context, authID, cacheKey string, entry antigravityCapabilityCacheEntry) {
	pending := entry.pendingEnum
	if pending == nil || pending.version == 0 ||
		!r.storeAntigravityModelEnums(ctx, authID, entry.hints.modelEnumScope, pending) {
		return
	}
	antigravityCapabilityMu.Lock()
	defer antigravityCapabilityMu.Unlock()
	current := antigravityCapabilityCache[cacheKey]
	if current.pendingEnum == pending {
		current.pendingEnum = nil
		antigravityCapabilityCache[cacheKey] = current
	}
}

// storeAntigravityModelEnums reports completion, including when a newer
// snapshot has already been published by another node.
func (r *Runtime) storeAntigravityModelEnums(ctx context.Context, authID, scope string, snapshot *antigravityModelEnumPublication) bool {
	if scope == "" || ctx.Err() != nil {
		return false
	}
	store, errStore := r.kvStore()
	if errStore != nil {
		return false
	}
	key := fmt.Sprintf("%s%x:%s", antigravityModelEnumKeyPrefix, sha256.Sum256([]byte(authID)), scope)
	if snapshot == nil {
		// Keep the last success for an actively probed credential, without
		// retaining abandoned scopes forever or rewriting the snapshot.
		kept, errExpire := store.KVExpire(ctx, key, antigravityModelEnumTTL)
		return kept && errExpire == nil
	}
	if snapshot.version <= 0 {
		return false
	}
	previous, found, errGet := store.KVGet(ctx, key)
	if errGet == nil {
		if found && ojson.GetBytes(previous, "version").Int() >= snapshot.version {
			return true
		}
		doc := ojson.NewDocString("{}")
		doc.Set("scope", scope)
		doc.Set("version", snapshot.version)
		doc.Set("models", snapshot.models)
		if len(snapshot.thinkingGroups) > 0 {
			doc.Set("thinkingGroups", snapshot.thinkingGroups)
		}
		if len(snapshot.thinkingWebSearch) > 0 {
			doc.Set("thinkingWebSearch", snapshot.thinkingWebSearch)
		}
		swapped, errSwap := store.KVCompareAndSwap(ctx, key, previous, found, doc.Bytes(), antigravityModelEnumTTL)
		if errSwap == nil {
			// On contention, let the next scan recheck the winning version.
			return swapped
		}
	}
	if ctx.Err() == nil {
		log.Warn("antigravity model enum snapshot write failed")
	}
	return false
}
