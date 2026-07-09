package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/gorm"
)

const homeConfigModelsMetadataKey = "home_config_models"

type RuntimeAdapter struct {
	repo      *Repository
	index     map[string]AuthIndex
	fullCache map[string]*coreauth.Auth
	homeIP    string
	homePort  int
	mu        sync.RWMutex
}

// NewRuntimeAdapter creates a new runtime adapter.
func NewRuntimeAdapter(repo *Repository, homeIP string, homePort ...int) *RuntimeAdapter {
	port := 0
	if len(homePort) > 0 && homePort[0] > 0 {
		port = homePort[0]
	}
	return &RuntimeAdapter{
		repo:      repo,
		index:     make(map[string]AuthIndex),
		fullCache: make(map[string]*coreauth.Auth),
		homeIP:    strings.TrimSpace(homeIP),
		homePort:  port,
	}
}

// SetHomePort updates the advertised Home port used for future usage records.
func (a *RuntimeAdapter) SetHomePort(port int) {
	if a == nil || port <= 0 {
		return
	}
	a.mu.Lock()
	a.homePort = port
	a.mu.Unlock()
}

// Enabled handles an enabled.
func (a *RuntimeAdapter) Enabled() bool {
	return a != nil && a.repo != nil
}

// LoadIndex loads an index.
func (a *RuntimeAdapter) LoadIndex(ctx context.Context) error {
	// Validate input data before converting it into runtime state.
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	indexes, errIndexes := a.repo.ListAuthIndex(ctx)
	if errIndexes != nil {
		return errIndexes
	}

	next := make(map[string]AuthIndex, len(indexes))
	for _, item := range indexes {
		uuid := strings.TrimSpace(item.UUID)
		if uuid == "" {
			continue
		}
		item.UUID = uuid
		item.ID = uuid
		item.Index = uuid
		item.Attributes = cloneStringMap(item.Attributes)
		item.ModelMetadata = cloneModelMetadata(item.ModelMetadata)
		item.ModelStates = cloneModelStateMap(item.ModelStates)
		next[uuid] = item
	}

	a.mu.Lock()
	a.index = next
	a.fullCache = make(map[string]*coreauth.Auth)
	a.mu.Unlock()
	return nil
}

// LoadAuthIndex loads an auth index.
func (a *RuntimeAdapter) LoadAuthIndex(ctx context.Context) error {
	return a.LoadIndex(ctx)
}

// LoadConfigYAML loads a config yaml.
func (a *RuntimeAdapter) LoadConfigYAML(ctx context.Context) ([]byte, error) {
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	_, payload, errConfig := a.repo.LoadConfigAsRuntimeConfig(ctx)
	if errConfig != nil {
		return nil, errConfig
	}
	return payload, nil
}

// StoreUsagePayload stores an usage payload.
func (a *RuntimeAdapter) StoreUsagePayload(ctx context.Context, payload string) error {
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	a.mu.RLock()
	metadata := UsageRuntimeMetadata{
		HomeIP:   a.homeIP,
		HomePort: a.homePort,
	}
	a.mu.RUnlock()
	_, errAppend := a.repo.AppendUsageWithRuntime(ctx, payload, metadata)
	return errAppend
}

// StoreAppLogPayload stores a CPA app log payload.
func (a *RuntimeAdapter) StoreAppLogPayload(ctx context.Context, clientIP string, payload string) error {
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	_, errAppend := a.repo.AppendAppLog(ctx, clientIP, a.homeIP, payload)
	return errAppend
}

// KVGet returns an active KV value.
func (a *RuntimeAdapter) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	if !a.Enabled() {
		return nil, false, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVGet(ctx, key)
}

// KVSet writes a KV value.
func (a *RuntimeAdapter) KVSet(ctx context.Context, key string, value []byte, ttl time.Duration, mode string) (bool, error) {
	if !a.Enabled() {
		return false, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVSet(ctx, key, value, ttl, KVSetMode(strings.ToLower(strings.TrimSpace(mode))))
}

// KVDel deletes active KV values.
func (a *RuntimeAdapter) KVDel(ctx context.Context, keys []string) (int64, error) {
	if !a.Enabled() {
		return 0, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVDel(ctx, keys)
}

// KVExpire updates a KV TTL.
func (a *RuntimeAdapter) KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if !a.Enabled() {
		return false, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVExpire(ctx, key, ttl)
}

// KVTTL returns a Redis-compatible KV TTL.
func (a *RuntimeAdapter) KVTTL(ctx context.Context, key string) (int64, error) {
	if !a.Enabled() {
		return 0, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVTTL(ctx, key)
}

// KVIncrBy increments a KV integer.
func (a *RuntimeAdapter) KVIncrBy(ctx context.Context, key string, delta int64) (int64, error) {
	if !a.Enabled() {
		return 0, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVIncrBy(ctx, key, delta)
}

// KVMGet returns KV values in request order.
func (a *RuntimeAdapter) KVMGet(ctx context.Context, keys []string) ([]home.KVGetResult, error) {
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	results, errMGet := a.repo.KVMGet(ctx, keys)
	if errMGet != nil {
		return nil, errMGet
	}
	out := make([]home.KVGetResult, 0, len(results))
	for _, result := range results {
		out = append(out, home.KVGetResult{Value: result.Value, Found: result.Found})
	}
	return out, nil
}

// KVMSet atomically writes KV values.
func (a *RuntimeAdapter) KVMSet(ctx context.Context, pairs map[string][]byte) error {
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVMSet(ctx, pairs)
}

// KVPurgeExpired deletes expired KV rows.
func (a *RuntimeAdapter) KVPurgeExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	if !a.Enabled() {
		return 0, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.KVPurgeExpired(ctx, now, limit)
}

// ReplacePluginStatus stores the latest plugin status report for a node.
func (a *RuntimeAdapter) ReplacePluginStatus(ctx context.Context, nodeType string, status node.PluginTaskStatus) error {
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.ReplacePluginStatus(ctx, nodeType, status)
}

// ListPendingPluginTasks returns plugin tasks that the node has not acked yet.
func (a *RuntimeAdapter) ListPendingPluginTasks(ctx context.Context, nodeType string, nodeID string) ([]node.PluginTask, error) {
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.ListPendingPluginTasks(ctx, nodeType, nodeID)
}

// AllowedAuthIDsForAPIKey returns auth IDs allowed by API-key channel bindings.
func (a *RuntimeAdapter) AllowedAuthIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error) {
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.AllowedAuthIDsForAPIKey(ctx, apiKey)
}

// AllowedDispatchIDsForAPIKey returns auth and model IDs allowed by API-key bindings.
func (a *RuntimeAdapter) AllowedDispatchIDsForAPIKey(ctx context.Context, apiKey string) ([]string, []string, error) {
	if !a.Enabled() {
		return nil, nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.AllowedDispatchIDsForAPIKey(ctx, apiKey)
}

// AllowedModelIDsForAPIKey returns model IDs allowed by API-key model group bindings.
func (a *RuntimeAdapter) AllowedModelIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error) {
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.AllowedModelIDsForAPIKey(ctx, apiKey)
}

// ValidateAPIKey reports whether an API key is active in the cluster database.
func (a *RuntimeAdapter) ValidateAPIKey(ctx context.Context, apiKey string) (bool, error) {
	if !a.Enabled() {
		return false, fmt.Errorf("cluster runtime adapter is disabled")
	}
	return a.repo.ValidateAPIKey(ctx, apiKey)
}

// List returns the available entries.
func (a *RuntimeAdapter) List(ctx context.Context) ([]*coreauth.Auth, error) {
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	auths, errAuths := a.repo.ListAuths(ctx)
	if errAuths != nil {
		return nil, errAuths
	}
	for _, auth := range auths {
		normalizeAuthUUID(auth)
	}
	return auths, nil
}

// Save stores save.
func (a *RuntimeAdapter) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	// Prepare filesystem state before committing the write.
	if !a.Enabled() {
		return "", fmt.Errorf("cluster runtime adapter is disabled")
	}
	auth = normalizeAuthUUID(auth)
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return "", fmt.Errorf("cluster auth uuid is required")
	}
	record, errRecord := a.repo.UpsertAuth(ctx, auth, "update")
	if errRecord != nil {
		return "", errRecord
	}

	item := authIndexFromRecord(record, auth)
	a.mu.Lock()
	if a.index == nil {
		a.index = make(map[string]AuthIndex)
	}
	item.Attributes = cloneStringMap(item.Attributes)
	item.ModelMetadata = cloneModelMetadata(item.ModelMetadata)
	item.ModelStates = cloneModelStateMap(item.ModelStates)
	a.index[auth.ID] = item
	if a.fullCache == nil {
		a.fullCache = make(map[string]*coreauth.Auth)
	}
	a.fullCache[auth.ID] = auth.Clone()
	a.mu.Unlock()
	return auth.ID, nil
}

// MutateAuthState implements coreauth.StateMutator. It applies mutate to the
// persisted auth row under the cluster write lock so availability transitions
// stay atomic across Home nodes, then refreshes the local index and cache.
func (a *RuntimeAdapter) MutateAuthState(ctx context.Context, id string, mutate func(auth *coreauth.Auth) bool) (*coreauth.Auth, error) {
	// Keep validation before state changes so failures leave existing data intact.
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	uuid := strings.TrimSpace(id)
	if uuid == "" {
		return nil, fmt.Errorf("cluster auth uuid is required")
	}
	auth, record, changed, errMutate := a.repo.MutateAuth(ctx, uuid, "update", mutate)
	if errMutate != nil {
		return nil, errMutate
	}
	if changed && auth != nil {
		item := authIndexFromRecord(record, auth)
		a.mu.Lock()
		if a.index == nil {
			a.index = make(map[string]AuthIndex)
		}
		item.Attributes = cloneStringMap(item.Attributes)
		item.ModelMetadata = cloneModelMetadata(item.ModelMetadata)
		item.ModelStates = cloneModelStateMap(item.ModelStates)
		a.index[uuid] = item
		if a.fullCache == nil {
			a.fullCache = make(map[string]*coreauth.Auth)
		}
		a.fullCache[uuid] = auth.Clone()
		a.mu.Unlock()
	}
	return auth, nil
}

// Delete handles delete.
func (a *RuntimeAdapter) Delete(ctx context.Context, id string) error {
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	uuid := strings.TrimSpace(id)
	if uuid == "" {
		return fmt.Errorf("cluster auth uuid is required")
	}
	if errDelete := a.repo.SoftDeleteAuth(ctx, uuid); errDelete != nil {
		return errDelete
	}
	a.RemoveAuthIndex(uuid)
	return nil
}

// RefreshAuthIndex refreshes refresh auth index.
func (a *RuntimeAdapter) RefreshAuthIndex(ctx context.Context, uuid string) error {
	// Resolve credential context before calling upstream OAuth services.
	if !a.Enabled() {
		return fmt.Errorf("cluster runtime adapter is disabled")
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return fmt.Errorf("cluster auth uuid is required")
	}

	auth, record, errAuth := a.repo.GetAuth(ctx, uuid)
	if errAuth != nil {
		if errors.Is(errAuth, gorm.ErrRecordNotFound) {
			a.RemoveAuthIndex(uuid)
			return nil
		}
		return errAuth
	}

	item := authIndexFromRecord(record, auth)
	a.mu.Lock()
	if a.index == nil {
		a.index = make(map[string]AuthIndex)
	}
	item.Attributes = cloneStringMap(item.Attributes)
	item.ModelMetadata = cloneModelMetadata(item.ModelMetadata)
	item.ModelStates = cloneModelStateMap(item.ModelStates)
	a.index[uuid] = item
	if a.fullCache != nil {
		delete(a.fullCache, uuid)
	}
	a.mu.Unlock()
	return nil
}

// normalizeAuthUUID normalizes an auth uuid.
func normalizeAuthUUID(auth *coreauth.Auth) *coreauth.Auth {
	if auth == nil {
		return nil
	}
	uuid := strings.TrimSpace(auth.ID)
	if uuid == "" {
		uuid = strings.TrimSpace(auth.Index)
	}
	auth.ID = uuid
	auth.Index = uuid
	return auth
}

// ApplyEvent updates apply event.
func (a *RuntimeAdapter) ApplyEvent(ctx context.Context, event ClusterEventRecord) error {
	if !strings.EqualFold(strings.TrimSpace(event.Scope), "auth") {
		return nil
	}
	uuid := strings.TrimSpace(event.EntityUUID)
	if uuid == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(event.Op), "delete") {
		a.RemoveAuthIndex(uuid)
		return nil
	}
	return a.RefreshAuthIndex(ctx, uuid)
}

// RemoveAuthIndex removes an auth index.
func (a *RuntimeAdapter) RemoveAuthIndex(uuid string) {
	if a == nil {
		return
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return
	}
	a.mu.Lock()
	if a.index != nil {
		delete(a.index, uuid)
	}
	if a.fullCache != nil {
		delete(a.fullCache, uuid)
	}
	a.mu.Unlock()
}

// GetFullAuth returns a full auth.
func (a *RuntimeAdapter) GetFullAuth(ctx context.Context, uuid string) (*coreauth.Auth, error) {
	// Normalize auth state before updating runtime indexes.
	if !a.Enabled() {
		return nil, fmt.Errorf("cluster runtime adapter is disabled")
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return nil, fmt.Errorf("cluster auth uuid is required")
	}

	a.mu.RLock()
	if cached := a.fullCache[uuid]; cached != nil {
		a.mu.RUnlock()
		return cached.Clone(), nil
	}
	a.mu.RUnlock()

	auth, _, errAuth := a.repo.GetAuth(ctx, uuid)
	if errAuth != nil {
		if errors.Is(errAuth, gorm.ErrRecordNotFound) {
			a.RemoveAuthIndex(uuid)
			return nil, coreauth.ErrFullAuthNotFound
		}
		return nil, errAuth
	}
	if auth == nil {
		return nil, nil
	}
	auth.ID = uuid
	auth.Index = uuid

	a.mu.Lock()
	if a.fullCache == nil {
		a.fullCache = make(map[string]*coreauth.Auth)
	}
	a.fullCache[uuid] = auth.Clone()
	a.mu.Unlock()
	return auth.Clone(), nil
}

// InvalidateFullAuth invalidates a full auth.
func (a *RuntimeAdapter) InvalidateFullAuth(uuid string) {
	if a == nil {
		return
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return
	}
	a.mu.Lock()
	if a.fullCache != nil {
		delete(a.fullCache, uuid)
	}
	a.mu.Unlock()
}

// ListMinimalAuths returns a minimal auths.
func (a *RuntimeAdapter) ListMinimalAuths() []*coreauth.Auth {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	keys := make([]string, 0, len(a.index))
	for uuid := range a.index {
		keys = append(keys, uuid)
	}
	sort.Strings(keys)
	out := make([]*coreauth.Auth, 0, len(keys))
	for _, uuid := range keys {
		item := a.index[uuid]
		out = append(out, authFromIndex(item))
	}
	a.mu.RUnlock()
	return out
}

// authIndexFromRecord derives auth index from record.
func authIndexFromRecord(record *AuthRecord, auth *coreauth.Auth) AuthIndex {
	// Normalize auth state before updating runtime indexes.
	item := AuthIndex{}
	if record != nil {
		item.UUID = strings.TrimSpace(record.UUID)
		item.ID = item.UUID
		item.Index = item.UUID
		item.Provider = record.Provider
		item.Label = record.Label
		item.Prefix = record.Prefix
		item.Status = record.Status
		item.Disabled = record.Disabled
		item.Unavailable = record.Unavailable
		item.BaseURL = record.BaseURL
		item.ModelsHash = record.ModelsHash
	}
	if auth != nil {
		if item.UUID == "" {
			item.UUID = strings.TrimSpace(auth.ID)
			item.ID = item.UUID
			item.Index = item.UUID
		}
		item.Status = auth.Status
		item.Disabled = auth.Disabled
		item.Unavailable = auth.Unavailable
		item.NextRetryAfter = auth.NextRetryAfter
		item.Quota = auth.Quota
		item.ModelStates = auth.ModelStates
		item.Attributes = cloneStringMap(auth.Attributes)
		item.ModelMetadata = modelMetadataFromAuth(auth)
	}
	return item
}

// authFromIndex derives auth from index.
func authFromIndex(item AuthIndex) *coreauth.Auth {
	// Normalize auth state before updating runtime indexes.
	uuid := strings.TrimSpace(item.UUID)
	if uuid == "" {
		uuid = strings.TrimSpace(item.ID)
	}
	attrs := cloneStringMap(item.Attributes)
	if attrs == nil {
		attrs = make(map[string]string)
	}
	if item.BaseURL != "" {
		if _, ok := attrs["base_url"]; !ok {
			attrs["base_url"] = item.BaseURL
		}
	}
	if item.ModelsHash != "" {
		if _, ok := attrs["models_hash"]; !ok {
			attrs["models_hash"] = item.ModelsHash
		}
	}
	metadata := cloneModelMetadata(item.ModelMetadata)
	return &coreauth.Auth{
		ID:             uuid,
		Index:          uuid,
		Provider:       item.Provider,
		Label:          item.Label,
		Prefix:         item.Prefix,
		Status:         item.Status,
		Disabled:       item.Disabled,
		Unavailable:    item.Unavailable,
		NextRetryAfter: item.NextRetryAfter,
		Quota:          item.Quota,
		ModelStates:    cloneModelStateMap(item.ModelStates),
		Attributes:     attrs,
		Metadata:       metadata,
	}
}

// modelMetadataFromAuth keeps only Home-owned metadata needed for model registration.
func modelMetadataFromAuth(auth *coreauth.Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	raw, ok := auth.Metadata[homeConfigModelsMetadataKey]
	if !ok || raw == nil {
		return nil
	}
	return map[string]any{homeConfigModelsMetadataKey: raw}
}

// cloneModelStateMap clones a per-model state map.
func cloneModelStateMap(in map[string]*coreauth.ModelState) map[string]*coreauth.ModelState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*coreauth.ModelState, len(in))
	for key, state := range in {
		out[key] = state.Clone()
	}
	return out
}

// cloneModelMetadata clones model metadata.
func cloneModelMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// cloneStringMap clones a string map.
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
