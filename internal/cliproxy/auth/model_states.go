package auth

import (
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// ModelStateView pairs recorded execution state with effective scheduling availability.
// State is diagnostic history; Blocked and NextRetryAfter apply the current cooling policy.
type ModelStateView struct {
	Model          string
	UpstreamModel  string
	StateKey       string
	DisplayName    string
	Registered     bool
	State          *ModelState
	Blocked        bool
	BlockReason    string
	NextRetryAfter time.Time
}

// DescribeModelStates reads one auth snapshot without selecting credentials or changing state.
// Registered route models resolve through the same prefix and alias pipeline as Dispatch.
// Unregistered historical keys are opaque state identifiers and are not aliased again.
func (m *Manager) DescribeModelStates(auth *Auth, modelFilter string, now time.Time) []ModelStateView {
	views := make([]ModelStateView, 0)
	if auth == nil {
		return views
	}
	effective := m.effectiveAvailabilityAuth(auth, now)
	covered := make(map[string]bool)
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		route := strings.TrimSpace(model.ID)
		upstream := m.resolveDispatchModel(auth, route).Key
		view := describeModelState(auth, effective, route, upstream, now)
		view.Registered = true
		view.DisplayName = model.DisplayName
		views = append(views, view)
		covered[upstream] = true
		covered[canonicalModelKey(route)] = true
	}
	for key := range auth.ModelStates {
		if !covered[key] {
			views = append(views, describeModelState(auth, effective, key, key, now))
		}
	}

	modelFilter = strings.TrimSpace(modelFilter)
	if modelFilter != "" {
		filterModel := modelFilter
		prefix := strings.TrimSpace(auth.Prefix)
		// Normalize diagnostic prefix casing without changing Dispatch's matching rules.
		for i, ch := range modelFilter {
			if ch == '/' && strings.EqualFold(modelFilter[:i], prefix) {
				filterModel = prefix + modelFilter[i:]
				break
			}
		}
		filterUpstream := m.resolveDispatchModel(auth, filterModel).Key
		filtered := views[:0]
		for _, view := range views {
			if strings.EqualFold(view.Model, modelFilter) ||
				strings.EqualFold(view.StateKey, modelFilter) ||
				strings.EqualFold(view.UpstreamModel, filterUpstream) {
				filtered = append(filtered, view)
			}
		}
		views = filtered
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Model < views[j].Model })
	return views
}

func describeModelState(auth, effective *Auth, route, upstream string, now time.Time) ModelStateView {
	checkedModel, blocked, reason, next := dispatchModelBlockStatus(effective, upstream, route, now)
	stateKey := upstream
	if blocked {
		stateKey = checkedModel
	}
	// Credential-level gates must not hide legacy history when upstream history is absent.
	if auth.ModelStates[stateKey] == nil {
		stateKey = route
		if auth.ModelStates[stateKey] == nil {
			stateKey = canonicalModelKey(stateKey)
		}
	}
	view := ModelStateView{
		Model:         route,
		UpstreamModel: upstream,
		StateKey:      stateKey,
		State:         auth.ModelStates[stateKey].Clone(),
		Blocked:       blocked,
		BlockReason:   "none",
	}
	switch reason {
	case blockReasonCooldown:
		view.BlockReason = "cooldown"
	case blockReasonDisabled:
		view.BlockReason = "disabled"
	case blockReasonOther:
		view.BlockReason = "other"
	}
	if blocked && next.After(now) {
		view.NextRetryAfter = next
	}
	return view
}
