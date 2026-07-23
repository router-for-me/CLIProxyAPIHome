package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// PluginAuthRefresher lets provider plugins refresh Home-owned auth records.
type PluginAuthRefresher interface {
	RefreshAuth(context.Context, *Auth) (*Auth, bool, error)
}

// PluginScheduler lets scheduler plugins select from Home-filtered auth candidates.
type PluginScheduler interface {
	PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

type pluginSchedulerState interface {
	HasScheduler() bool
}

// SetPluginAuthRefresher sets the optional plugin auth refresher.
func (m *Manager) SetPluginAuthRefresher(refresher PluginAuthRefresher) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pluginRefresher = refresher
	m.mu.Unlock()
}

// SetPluginScheduler sets the optional plugin scheduler.
func (m *Manager) SetPluginScheduler(scheduler PluginScheduler) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pluginScheduler = scheduler
	m.mu.Unlock()
}

func (m *Manager) refreshViaPlugin(ctx context.Context, target *Auth) (*Auth, bool, error) {
	if m == nil || target == nil {
		return nil, false, nil
	}
	m.mu.RLock()
	refresher := m.pluginRefresher
	m.mu.RUnlock()
	if refresher == nil {
		return nil, false, nil
	}
	return refresher.RefreshAuth(ctx, target.Clone())
}

func (m *Manager) hasPluginScheduler() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	scheduler := m.pluginScheduler
	m.mu.RUnlock()
	if scheduler == nil {
		return false
	}
	if state, ok := scheduler.(pluginSchedulerState); ok {
		return state.HasScheduler()
	}
	return true
}

func schedulerAuthCandidates(auths []*Auth) []pluginapi.SchedulerAuthCandidate {
	if len(auths) == 0 {
		return nil
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, pluginapi.SchedulerAuthCandidate{
			ID:         auth.ID,
			Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
			Priority:   authPriority(auth),
			Status:     string(auth.Status),
			Attributes: schedulerSafeAttributes(auth.Attributes),
		})
	}
	return out
}

func schedulerAttributeSensitive(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	normalized := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(key)
	compact := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(key)
	for _, fragment := range []string{
		"api_key",
		"apikey",
		"token",
		"secret",
		"cookie",
		"credential",
		"password",
		"storage",
		"authorization",
		"auth_header",
		"proxy_url",
	} {
		if strings.Contains(key, fragment) || strings.Contains(normalized, fragment) || strings.Contains(compact, fragment) {
			return true
		}
	}
	return false
}

func schedulerSafeAttributes(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		if schedulerAttributeSensitive(key) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func schedulerProviders(provider string, providers []string) []string {
	out := make([]string, 0, len(providers)+1)
	seen := make(map[string]struct{}, len(providers)+1)
	addProvider := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "mixed" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	addProvider(provider)
	for _, value := range providers {
		addProvider(value)
	}
	return out
}

func schedulerOptions(opts Options) pluginapi.SchedulerOptions {
	return pluginapi.SchedulerOptions{
		Headers:  cloneHTTPHeader(opts.Headers),
		Metadata: cloneAnyMap(opts.Metadata),
	}
}

func cloneHTTPHeader(in http.Header) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
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

func pickSchedulerAuthByID(candidates []*Auth, authID string) *Auth {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID == authID {
			return candidate
		}
	}
	return nil
}

func builtinSchedulerStrategy(delegate string) (schedulerStrategy, bool) {
	switch strings.TrimSpace(delegate) {
	case pluginapi.SchedulerBuiltinRoundRobin:
		return schedulerStrategyRoundRobin, true
	case pluginapi.SchedulerBuiltinFillFirst:
		return schedulerStrategyFillFirst, true
	default:
		return schedulerStrategyCustom, false
	}
}

func (m *Manager) pickViaBuiltinScheduler(ctx context.Context, strategy schedulerStrategy, provider string, providers []string, model string, opts Options, tried map[string]struct{}) (*Auth, bool, error) {
	if m == nil || m.scheduler == nil {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	var selected *Auth
	var errPick error
	if providerKey == "mixed" {
		selected, _, errPick = m.scheduler.pickMixedWithStrategy(ctx, providers, model, opts, tried, strategy)
	} else {
		selected, errPick = m.scheduler.pickSingleWithStrategy(ctx, providerKey, model, opts, tried, strategy)
	}
	if errPick != nil {
		return nil, true, errPick
	}
	if selected == nil {
		return nil, true, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	return selected, true, nil
}

func (m *Manager) pickViaPluginScheduler(ctx context.Context, scheduler PluginScheduler, provider string, providers []string, model string, opts Options, tried map[string]struct{}, candidates []*Auth) (*Auth, bool, error) {
	if scheduler == nil || len(candidates) == 0 {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	requestProvider := providerKey
	if providerKey == "mixed" {
		requestProvider = ""
	}
	req := pluginapi.SchedulerPickRequest{
		Provider:   requestProvider,
		Providers:  schedulerProviders(providerKey, providers),
		Model:      model,
		Options:    schedulerOptions(opts),
		Candidates: schedulerAuthCandidates(candidates),
	}
	resp, handled, errPick := scheduler.PickAuth(ctx, req)
	if errPick != nil {
		return nil, true, errPick
	}
	if !handled || !resp.Handled {
		return nil, false, nil
	}
	if selected := pickSchedulerAuthByID(candidates, resp.AuthID); selected != nil {
		return selected, true, nil
	}
	strategy, okStrategy := builtinSchedulerStrategy(resp.DelegateBuiltin)
	if !okStrategy {
		return nil, false, nil
	}
	if len(excludedConcurrencyCandidatesFromOptions(opts)) > 0 {
		return nil, false, nil
	}
	return m.pickViaBuiltinScheduler(ctx, strategy, providerKey, providers, model, opts, tried)
}
