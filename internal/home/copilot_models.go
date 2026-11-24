package home

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	copilotauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth/copilot"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	copilotModelMaxAge          = 2 * time.Hour
	copilotModelRefreshInterval = 15 * time.Minute
	copilotModelRetryInterval   = time.Minute
	copilotDiscoveryWorkers     = 4
)

type copilotAccountIdentity struct {
	base      string
	accountID string
}

type copilotAccountModels struct {
	identity    copilotAccountIdentity
	ids         map[string]struct{}
	expiresAt   time.Time
	nextProbeAt time.Time
}

func (s copilotAccountModels) due(identity copilotAccountIdentity, now time.Time) bool {
	return s.identity != identity || !now.Before(s.nextProbeAt)
}

func activeCopilotAccount(auth *coreauth.Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), copilotauth.Provider) && !auth.Disabled && auth.Status != coreauth.StatusDisabled
}

func copilotTokenUsable(auth *coreauth.Auth, now time.Time) bool {
	if !activeCopilotAccount(auth) {
		return false
	}
	if !auth.RuntimeProjection && extractAccessToken(auth) == "" {
		return false
	}
	if expiry, hasExpiry := auth.ExpirationTime(); hasExpiry && !now.Before(expiry) {
		return false
	}
	return true
}

func copilotSnapshotUsable(auth *coreauth.Auth, snapshot copilotAccountModels, now time.Time) bool {
	return copilotTokenUsable(auth, now) &&
		snapshot.identity == copilotIdentity(auth) &&
		now.Before(snapshot.expiresAt) &&
		snapshot.ids != nil
}

func copilotIdentity(auth *coreauth.Auth) copilotAccountIdentity {
	base, _ := auth.Metadata["base_url"].(string)
	accountID, _ := auth.Metadata["account_id"].(string)
	return copilotAccountIdentity{base: strings.TrimRight(base, "/"), accountID: accountID}
}

func (r *Runtime) registerCopilotModelsForAuth(auth *coreauth.Auth) {
	r.copilotMu.Lock()
	defer r.copilotMu.Unlock()
	r.registerCopilotModelsLocked(auth)
}

// registerCopilotModelsLocked publishes a Copilot account's eligible catalog models under copilotMu.
func (r *Runtime) registerCopilotModelsLocked(auth *coreauth.Auth) {
	if r.coreManager != nil {
		auth, _ = r.coreManager.GetByID(auth.ID)
	}
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), copilotauth.Provider) {
		return
	}
	if _, _, compat := openAICompatInfoFromAuth(auth); compat {
		return
	}
	unregisterLegacyModelClient(auth)
	snapshot := r.copilotAccounts[auth.ID]
	identity := copilotIdentity(auth)
	active := activeCopilotAccount(auth)
	now := time.Now()
	if active && snapshot.due(identity, now) {
		r.wakeCopilotDiscovery()
	}
	if !active || snapshot.identity != identity {
		delete(r.copilotAccounts, auth.ID)
		r.registerResolvedModelsForAuth(auth, copilotauth.Provider, nil, nil)
		return
	}
	if !copilotSnapshotUsable(auth, snapshot, now) {
		r.registerResolvedModelsForAuth(auth, copilotauth.Provider, nil, nil)
		return
	}
	// Discovery grants access by ID. Model definitions come from the active Copilot catalog.
	models := make([]*ModelInfo, 0, len(snapshot.ids))
	for _, model := range registry.GetCopilotModels() {
		if model != nil {
			if _, allowed := snapshot.ids[model.ID]; allowed {
				setCopilotModelDisplayName(model, model.ID)
				applyModelNamespace(model, copilotauth.Provider, model.ID, model.ID)
				models = append(models, model)
			}
		}
	}
	cfg := r.Config()
	authKind, excluded := r.modelRegistrationAuthSettings(cfg, copilotauth.Provider, auth)
	models, candidates := applyOAuthModelConfig(cfg, copilotauth.Provider, authKind, models, excluded)
	r.registerResolvedModelsForAuth(auth, copilotauth.Provider, applyModelPrefixes(models, auth.Prefix, cfg != nil && cfg.ForceModelPrefix), candidates)
}

func (r *Runtime) copilotDispatchModelInfo(decision *coreauth.DispatchDecision, route string, now time.Time) *DispatchModelInfo {
	auth := decision.Auth
	if !copilotTokenUsable(auth, now) {
		return nil
	}
	upstream := coreauth.CanonicalModelID(decision.UpstreamModel)
	nativeID, qualified := strings.CutPrefix(upstream, copilotauth.Provider+"/")
	if !qualified || nativeID == "" {
		return nil
	}
	r.copilotMu.RLock()
	defer r.copilotMu.RUnlock()
	snapshot := r.copilotAccounts[auth.ID]
	if _, allowed := snapshot.ids[nativeID]; !allowed || !copilotSnapshotUsable(auth, snapshot, now) {
		return nil
	}
	if registry.LookupStaticModelInfoByChannel(nativeID, copilotauth.Provider) == nil {
		return nil
	}
	// Check the current publication under the same lock as the account permissions.
	published := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, coreauth.CanonicalModelID(route))
	if published == nil {
		published = registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, strings.TrimSpace(route))
	}
	if published == nil || published.SourceModelID != upstream || published.MetadataModelID != nativeID || published.ResponseModelAlias != decision.OriginalAlias {
		return nil
	}
	return newDispatchModelInfo(upstream, published)
}

// PruneCopilotModels removes expired or otherwise ineligible account models from the published registry.
func (r *Runtime) PruneCopilotModels() {
	if r == nil {
		return
	}
	now := time.Now()
	republish := make(map[string]struct{})
	r.copilotMu.Lock()
	for id, snapshot := range r.copilotAccounts {
		if !snapshot.expiresAt.IsZero() && !now.Before(snapshot.expiresAt) {
			snapshot.ids = nil
			snapshot.expiresAt = time.Time{}
			r.copilotAccounts[id] = snapshot
			republish[id] = struct{}{}
		}
	}
	r.copilotMu.Unlock()
	if r.coreManager != nil {
		reg := registry.GetGlobalRegistry()
		for _, auth := range r.coreManager.ListByProvider(copilotauth.Provider) {
			if auth == nil {
				continue
			}
			r.copilotMu.RLock()
			snapshot := r.copilotAccounts[auth.ID]
			r.copilotMu.RUnlock()
			if !copilotSnapshotUsable(auth, snapshot, now) && reg.GetModelCandidatesForClient(auth.ID) != nil {
				republish[auth.ID] = struct{}{}
			}
		}
	}
	for id := range republish {
		r.publishCopilotModels(id)
	}
}

func (r *Runtime) wakeCopilotDiscovery() {
	select {
	case r.copilotWake <- struct{}{}:
	default:
	}
}

func (r *Runtime) runCopilotDiscovery(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	scheduled := make(map[string]bool)
	finished := make(chan string, copilotDiscoveryWorkers)
	var pending []string
	enqueueDue := func() {
		r.PruneCopilotModels()
		auths := r.coreManager.ListByProvider(copilotauth.Provider)
		now := time.Now()
		r.copilotMu.RLock()
		defer r.copilotMu.RUnlock()
		for _, auth := range auths {
			if !activeCopilotAccount(auth) || scheduled[auth.ID] {
				continue
			}
			if r.copilotAccounts[auth.ID].due(copilotIdentity(auth), now) {
				pending = append(pending, auth.ID)
				scheduled[auth.ID] = true
			}
		}
	}
	enqueueDue()
	active := 0
	for ctx.Err() == nil {
		for len(pending) > 0 && active < copilotDiscoveryWorkers {
			id := pending[0]
			pending = pending[1:]
			active++
			workers.Go(func() {
				r.discoverCopilotModels(ctx, id)
				finished <- id
			})
		}
		select {
		case <-ctx.Done():
			return
		case id := <-finished:
			delete(scheduled, id)
			active--
		case <-r.copilotWake:
			enqueueDue()
		case <-ticker.C:
			enqueueDue()
		}
	}
}

func (r *Runtime) discoverCopilotModels(ctx context.Context, id string) {
	auth, _ := r.coreManager.GetByID(id)
	if !activeCopilotAccount(auth) {
		return
	}
	identity := copilotIdentity(auth)
	if !copilotauth.TrustedAPI(identity.base) {
		return
	}
	r.copilotMu.RLock()
	due := r.copilotAccounts[id].due(identity, time.Now())
	r.copilotMu.RUnlock()
	if !due {
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	full, errLookup := r.copilotDiscoveryAuth(probeCtx, auth)
	if errLookup != nil {
		r.recordCopilotLookupFailure(ctx, id, identity, "lookup_before_fetch", errLookup)
		return
	}
	if full == nil {
		return
	}
	if !copilotTokenUsable(full, time.Now()) {
		return
	}
	token := extractAccessToken(full)
	client := &http.Client{Transport: r.coreManager.AuthRoundTripper(full)}
	if client.Transport == nil {
		cfg := r.Config()
		if cfg != nil {
			sdkCfg := cfg.SDKConfig
			sdkCfg.ProxyURL = cfg.ProxyURL
			client = util.SetProxy(&sdkCfg, client)
		}
	}
	raw, errFetch := copilotauth.FetchModels(probeCtx, client, identity.base, token)
	var ids map[string]struct{}
	stage := "fetch"
	if errFetch == nil {
		stage = "decode"
		ids, errFetch = decodeCopilotModelIDs(raw)
	}
	if ctx.Err() != nil {
		return
	}
	var status copilotauth.HTTPError
	if errFetch != nil {
		entry := log.WithFields(log.Fields{"auth_id": id, "stage": stage, "reason": logging.SafeErrorDiagnostic(errFetch)})
		if errors.As(errFetch, &status) {
			entry = entry.WithField("status", status.Code)
		}
		entry.Warn("Copilot model discovery failed")
	}

	revoked := status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden
	if errFetch == nil || revoked {
		current, _ := r.coreManager.GetByID(id)
		current, errLookup = r.copilotDiscoveryAuth(probeCtx, current)
		if errLookup != nil {
			r.recordCopilotLookupFailure(ctx, id, identity, "lookup_after_fetch", errLookup)
			return
		}
		if current == nil || copilotIdentity(current) != identity || extractAccessToken(current) != token {
			return
		}
	}
	now := time.Now()
	r.copilotMu.Lock()
	latest, _ := r.coreManager.GetByID(id)
	if !activeCopilotAccount(latest) || copilotIdentity(latest) != identity ||
		(!latest.RuntimeProjection && extractAccessToken(latest) != token) {
		r.copilotMu.Unlock()
		return
	}
	next := r.copilotAccounts[id]
	if next.identity != identity {
		next = copilotAccountModels{identity: identity}
	}
	next.nextProbeAt = now.Add(copilotModelRetryInterval)
	switch {
	case errFetch == nil:
		next.ids = ids
		next.expiresAt = now.Add(copilotModelMaxAge)
		next.nextProbeAt = now.Add(copilotModelRefreshInterval)
	case revoked:
		next.ids = nil
		next.expiresAt = time.Time{}
	}
	r.copilotAccounts[id] = next
	// Publish the routes while their account permissions are still protected by the lock.
	r.registerCopilotModelsLocked(latest)
	r.copilotMu.Unlock()
	r.coreManager.ReconcileRegistryModelStates(ctx, id)
	r.coreManager.RefreshSchedulerEntry(id)
}

// decodeCopilotModelIDs reads account eligibility. Model limits and features live in the Home catalog.
func decodeCopilotModelIDs(raw []byte) (map[string]struct{}, error) {
	var response struct {
		Data []struct {
			ID     string `json:"id"`
			Picker *bool  `json:"model_picker_enabled"`
			Policy struct {
				State string `json:"state"`
			} `json:"policy"`
			Capabilities struct {
				Type string `json:"type"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	ids := make(map[string]struct{}, len(response.Data))
	for _, model := range response.Data {
		if model.ID == "" || model.ID == "auto" || (model.Picker != nil && !*model.Picker) {
			continue
		}
		if model.Policy.State != "" && model.Policy.State != "enabled" {
			continue
		}
		if model.Capabilities.Type != "" && model.Capabilities.Type != "chat" {
			continue
		}
		ids[model.ID] = struct{}{}
	}
	return ids, nil
}

func (r *Runtime) copilotDiscoveryAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if !activeCopilotAccount(auth) {
		return nil, nil
	}
	if auth.RuntimeProjection && r.clusterAdapter != nil && r.clusterAdapter.Enabled() {
		full, errFull := r.clusterAdapter.GetFullAuth(ctx, auth.ID)
		if errors.Is(errFull, coreauth.ErrFullAuthNotFound) {
			return nil, nil
		}
		if errFull != nil {
			return nil, errFull
		}
		if !activeCopilotAccount(full) || copilotIdentity(full) != copilotIdentity(auth) {
			return nil, nil
		}
		return full, nil
	}
	return auth, nil
}

// recordCopilotLookupFailure is the probe boundary for transient credential reads.
// Only retry scheduling changes; unverified responses cannot alter cached permissions.
func (r *Runtime) recordCopilotLookupFailure(ctx context.Context, id string, identity copilotAccountIdentity, stage string, err error) {
	if ctx.Err() != nil {
		return
	}
	entry := log.WithFields(log.Fields{"auth_id": id, "stage": stage, "reason": logging.SafeErrorDiagnostic(err)})
	entry.Warn("Copilot model discovery failed")

	r.copilotMu.Lock()
	defer r.copilotMu.Unlock()
	latest, _ := r.coreManager.GetByID(id)
	if !activeCopilotAccount(latest) || copilotIdentity(latest) != identity {
		return
	}
	next, exists := r.copilotAccounts[id]
	if exists && next.identity != identity {
		return
	}
	next.identity = identity
	next.nextProbeAt = time.Now().Add(copilotModelRetryInterval)
	r.copilotAccounts[id] = next
}

func (r *Runtime) publishCopilotModels(id string) {
	auth, _ := r.coreManager.GetByID(id)
	if auth == nil || auth.Provider != "copilot" {
		return
	}
	r.registerModelsForAuth(auth)
	r.coreManager.ReconcileRegistryModelStates(context.Background(), id)
	r.coreManager.RefreshSchedulerEntry(id)
}
