package auth

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/credentialweight"
)

// WeightedRoundRobinSelector provides smooth weighted round-robin selection.
type WeightedRoundRobinSelector struct {
	mu      sync.Mutex
	states  map[string]*smoothWeightedState
	maxKeys int
}

type smoothWeightedState struct {
	current    map[string]int64
	weights    map[string]int64
	pending    map[string]int64
	generation uint64
}

// weightedReservation keeps speculative credit separate from committed, clamped
// credit, so rolling back a request never undoes another request's accounting.
type weightedReservation struct {
	selector   *WeightedRoundRobinSelector
	state      *smoothWeightedState
	generation uint64
	weights    map[string]int64
	authID     string
	total      int64
	finished   bool
}

func authWeight(auth *Auth) int64 {
	if auth == nil {
		return credentialweight.Default
	}
	if rawWeight, ok := auth.Attributes["weight"]; ok && strings.TrimSpace(rawWeight) != "" {
		weight, errParse := credentialweight.ParseString(rawWeight)
		if errParse != nil {
			return 0
		}
		return weight
	}
	if rawWeight, ok := auth.Metadata["weight"]; ok && rawWeight != nil {
		weight, errParse := credentialweight.ParseValue(rawWeight)
		if errParse != nil {
			return 0
		}
		return weight
	}
	return credentialweight.Default
}

func positiveWeightAuths(auths []*Auth) []*Auth {
	weightedCandidates := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if authWeight(auth) > 0 {
			weightedCandidates = append(weightedCandidates, auth)
		}
	}
	return weightedCandidates
}

// Pick selects the next available auth using smooth weighted round-robin.
func (s *WeightedRoundRobinSelector) Pick(ctx context.Context, provider, model string, opts Options, auths []*Auth) (*Auth, error) {
	available, errAvailable := getAvailableAuths(positiveWeightAuths(filterConcurrencyExcludedAuths(auths, model, opts)), provider, model, time.Now())
	if errAvailable != nil {
		return nil, errAvailable
	}
	picked, reservation := s.reserve(weightedStateKey(provider, model, opts), available, "")
	if picked == nil {
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available with positive weight"}
	}
	reservation.finish(true)
	return picked, nil
}

func weightedStateKey(provider, model string, opts Options) string {
	if requested, ok := opts.Metadata[RequestedModelMetadataKey].(string); ok && strings.TrimSpace(requested) != "" {
		model = requested
	}
	return provider + ":" + canonicalModelKey(model)
}

// reserve selects and reserves credit atomically. A non-empty authID preserves a
// sticky binding; otherwise committed and pending credits both guide selection.
func (s *WeightedRoundRobinSelector) reserve(key string, available []*Auth, authID string) (*Auth, *weightedReservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.stateFor(key)
	weights := authWeightVector(available)
	state.prepare(weights)
	var picked *Auth
	var pickedCurrent, total int64
	for _, auth := range available {
		if auth == nil || weights[auth.ID] <= 0 {
			continue
		}
		total = saturatingAddInt64(total, weights[auth.ID])
		if authID != "" && auth.ID != authID {
			continue
		}
		current := saturatingAddInt64(state.current[auth.ID], state.pending[auth.ID])
		current = saturatingAddInt64(current, weights[auth.ID])
		if picked == nil || current > pickedCurrent {
			picked, pickedCurrent = auth, current
		}
	}
	if picked == nil {
		return nil, nil
	}
	state.adjustPending(weights, picked.ID, total, false)
	return picked, &weightedReservation{
		selector: s, state: state, generation: state.generation,
		weights: weights, authID: picked.ID, total: total,
	}
}

// finish confirms or releases a reservation exactly once. Pending deltas are not
// clamped: applying and then reversing a clamped delta would lose prior credit.
func (r *weightedReservation) finish(committed bool) {
	if r == nil {
		return
	}
	r.selector.mu.Lock()
	defer r.selector.mu.Unlock()
	if r.finished {
		return
	}
	r.finished = true
	// A configuration reset already discarded this generation's pending credit.
	if r.state.generation != r.generation {
		return
	}
	r.state.adjustPending(r.weights, r.authID, r.total, true)
	if committed {
		r.state.charge(r.weights, r.authID)
	}
}

// adjustPending updates only this reservation's delta, including on rollback.
// Must be called with the selector mutex held.
func (s *smoothWeightedState) adjustPending(weights map[string]int64, authID string, total int64, release bool) {
	for id, weight := range weights {
		delta := weight
		if id == authID {
			delta -= total
		}
		if release {
			delta = -delta
		}
		s.pending[id] = saturatingAddInt64(s.pending[id], delta)
		if s.pending[id] == 0 {
			delete(s.pending, id)
		}
	}
}

// stateFor returns a model's accumulator with s.mu held.
func (s *WeightedRoundRobinSelector) stateFor(key string) *smoothWeightedState {
	if s.states == nil {
		s.states = make(map[string]*smoothWeightedState)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.states[key]; !ok && len(s.states) >= limit {
		s.states = make(map[string]*smoothWeightedState)
	}
	state := s.states[key]
	if state == nil {
		state = &smoothWeightedState{}
		s.states[key] = state
	}
	return state
}

// maxSmoothWeightedStateEntries bounds a single accumulator map so credentials that are
// removed permanently cannot leak entries. Real pools stay far below this bound, so the
// transient subsets produced by retry exclusions and cooldowns are never pruned.
const maxSmoothWeightedStateEntries = 1024

// prepare syncs the configured weights into the state without discarding accumulated
// credits. Credits are reset only when a credential's configured weight actually changes,
// never when the candidate set shrinks temporarily (retry exclusions, cooldowns, session
// affinity), because discarding credits there would collapse selection onto the first
// candidate in slice order.
func (s *smoothWeightedState) prepare(weights map[string]int64) {
	if s.current == nil || weightsConfigChanged(s.weights, weights) {
		s.current = make(map[string]int64, len(weights))
		s.pending = make(map[string]int64, len(weights))
		s.generation++
	}
	if s.weights == nil {
		s.weights = make(map[string]int64, len(weights))
	}
	for authID, weight := range weights {
		s.weights[authID] = weight
	}
	s.pruneStale(weights)
}

// pruneStale drops entries for credentials outside the current candidate set, but only
// once a map exceeds the safety bound, so ordinary transient exclusions keep their credits.
func (s *smoothWeightedState) pruneStale(weights map[string]int64) {
	if len(s.current) <= maxSmoothWeightedStateEntries && len(s.weights) <= maxSmoothWeightedStateEntries {
		return
	}
	for authID := range s.current {
		if _, ok := weights[authID]; !ok {
			delete(s.current, authID)
		}
	}
	for authID := range s.weights {
		if _, ok := weights[authID]; !ok {
			delete(s.weights, authID)
		}
	}
}

// weightsConfigChanged reports whether any credential present in both vectors has a
// different configured weight. Credentials that are merely missing from one side are
// ignored, since a candidate subset is not a configuration change.
func weightsConfigChanged(left, right map[string]int64) bool {
	if len(left) == 0 {
		return false
	}
	for authID, weight := range right {
		if previous, ok := left[authID]; ok && previous != weight {
			return true
		}
	}
	return false
}

func authWeightVector(auths []*Auth) map[string]int64 {
	weights := make(map[string]int64, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if weight := authWeight(auth); weight > 0 {
			weights[auth.ID] = weight
		}
	}
	return weights
}

// maxSmoothWeightedCreditRounds bounds compensation after a long sticky session.
const maxSmoothWeightedCreditRounds int64 = 32

func (s *smoothWeightedState) charge(weights map[string]int64, authID string) {
	var totalWeight int64
	for id, weight := range weights {
		s.current[id] = saturatingAddInt64(s.current[id], weight)
		totalWeight = saturatingAddInt64(totalWeight, weight)
	}
	s.current[authID] = saturatingAddInt64(s.current[authID], -totalWeight)
	limit := int64(math.MaxInt64)
	if totalWeight <= math.MaxInt64/maxSmoothWeightedCreditRounds {
		limit = totalWeight * maxSmoothWeightedCreditRounds
	}
	// Preserve temporarily excluded credentials' credits. Clamping deliberately
	// trades exact historical compensation for a bounded correction window.
	for id := range weights {
		if s.current[id] > limit {
			s.current[id] = limit
		} else if s.current[id] < -limit {
			s.current[id] = -limit
		}
	}
}

func saturatingAddInt64(value, delta int64) int64 {
	if delta > 0 && value > math.MaxInt64-delta {
		return math.MaxInt64
	}
	if delta < 0 && value < math.MinInt64-delta {
		return math.MinInt64
	}
	return value + delta
}

// isWeightedSelector also covers session affinity wrapping weighted fallback.
func isWeightedSelector(selector Selector) bool {
	switch current := selector.(type) {
	case *WeightedRoundRobinSelector:
		return true
	case *SessionAffinitySelector:
		return isWeightedSelector(current.fallback)
	default:
		return false
	}
}
