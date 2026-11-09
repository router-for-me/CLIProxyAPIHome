package auth

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/credentialweight"
)

// WeightedRoundRobinSelector provides smooth weighted round-robin selection.
type WeightedRoundRobinSelector struct {
	mu         sync.Mutex
	states     map[[32]byte]*weightedPoolState
	stateCount int
	maxKeys    int
}

// weightedReservation separates speculative credit from committed, clamped credit.
// Releasing one request must never undo accounting committed by another request.
type weightedReservation struct {
	selector   *WeightedRoundRobinSelector
	state      *weightedPoolState
	generation uint64
	member     int
	total      int64
	bound      bool
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

// weightedPoolMember stores each ID once, with parallel accounting values in a
// single allocation. Members are sorted by ID; reservations retain only an index.
type weightedPoolMember struct {
	id      string
	weight  int64
	current int64
	pending int64
}

// weightedPoolState has fixed membership and shares its weights across reservations.
type weightedPoolState struct {
	route      string
	members    []weightedPoolMember
	inFlight   uint64
	generation uint64
	next       *weightedPoolState // Exact-identity collision chain for the digest key.
}

func sortedWeightedIDs(weights map[string]int64) []string {
	ids := make([]string, 0, len(weights))
	for id := range weights {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// weightedPoolStateKey isolates competing sets without retaining concatenated IDs.
// Weights stay out of the key so configuration changes invalidate reservations.
func weightedPoolStateKey(route string, weights map[string]int64) [32]byte {
	return weightedPoolDigest(route, sortedWeightedIDs(weights))
}

func weightedPoolDigest(route string, ids []string) [32]byte {
	size := 8 + len(route)
	for _, id := range ids {
		size += 8 + len(id)
	}
	identity := make([]byte, 0, size)
	identity = binary.LittleEndian.AppendUint64(identity, uint64(len(route)))
	identity = append(identity, route...)
	for _, id := range ids {
		identity = binary.LittleEndian.AppendUint64(identity, uint64(len(id)))
		identity = append(identity, id...)
	}
	return sha256.Sum256(identity)
}

// matches verifies the full identity even on a digest hit. Hash collisions must
// never combine the quota or configured weights of unrelated candidate pools.
func (s *weightedPoolState) matches(route string, ids []string) bool {
	if s.route != route || len(s.members) != len(ids) {
		return false
	}
	for i, id := range ids {
		if s.members[i].id != id {
			return false
		}
	}
	return true
}

func (s *weightedPoolState) prepare(weights map[string]int64) {
	for _, member := range s.members {
		if member.weight != weights[member.id] {
			s.generation++
			s.inFlight = 0
			for i := range s.members {
				s.members[i].weight = weights[s.members[i].id]
				s.members[i].current = 0
				s.members[i].pending = 0
			}
			return
		}
	}
}

// reserve selects and reserves credit atomically. A non-empty authID preserves a
// sticky binding; otherwise committed and pending credits both guide selection.
func (s *WeightedRoundRobinSelector) reserve(route string, available []*Auth, authID string) (*Auth, *weightedReservation) {
	weights := authWeightVector(available)
	ids := sortedWeightedIDs(weights)
	key := weightedPoolDigest(route, ids)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[[32]byte]*weightedPoolState)
	}
	state := s.states[key]
	for state != nil && !state.matches(route, ids) {
		state = state.next
	}
	if state == nil {
		limit := s.maxKeys
		if limit <= 0 {
			limit = 4096
		}
		if s.stateCount >= limit {
			s.states = make(map[[32]byte]*weightedPoolState)
			s.stateCount = 0
		}
		state = &weightedPoolState{route: route, members: make([]weightedPoolMember, len(ids)), next: s.states[key]}
		for i, id := range ids {
			state.members[i] = weightedPoolMember{id: id, weight: weights[id]}
		}
		s.states[key] = state
		s.stateCount++
	} else {
		state.prepare(weights)
	}
	var picked *Auth
	var pickedCurrent, total int64
	pickedMember := -1
	for _, auth := range available {
		if auth == nil || weights[auth.ID] <= 0 {
			continue
		}
		total = saturatingAddInt64(total, weights[auth.ID])
		if authID != "" && auth.ID != authID {
			continue
		}
		index := sort.SearchStrings(ids, auth.ID)
		member := &state.members[index]
		current := saturatingAddInt64(member.current, member.pending)
		current = saturatingAddInt64(current, member.weight)
		if picked == nil || current > pickedCurrent {
			picked, pickedCurrent, pickedMember = auth, current, index
		}
	}
	if picked == nil {
		return nil, nil
	}
	// Bound traffic beyond the compensation window still keeps its credential,
	// but must not create speculative credit that can starve concurrent picks.
	if authID != "" && !state.boundChargeFits(pickedMember, total) {
		return picked, nil
	}
	state.adjustPending(pickedMember, total, false)
	state.inFlight++
	return picked, &weightedReservation{
		selector: s, state: state, generation: state.generation,
		member: pickedMember, total: total, bound: authID != "",
	}
}

// finish confirms or releases a reservation exactly once under the selector lock.
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
	if r.state.generation != r.generation {
		return
	}
	r.state.adjustPending(r.member, r.total, true)
	r.state.inFlight--
	if committed {
		r.state.charge(r.member, r.bound)
	} else if r.state.inFlight == 0 {
		// A final rollback can remove the offset to already committed credit.
		r.state.boundCredits(r.total)
	}
}

// adjustPending must be called with the selector mutex held. Pending deltas are
// not clamped, so releasing them cannot lose credit at the compensation bound.
func (s *weightedPoolState) adjustPending(selected int, total int64, release bool) {
	for i := range s.members {
		member := &s.members[i]
		delta := member.weight
		if i == selected {
			delta -= total
		}
		if release {
			delta = -delta
		}
		member.pending = saturatingAddInt64(member.pending, delta)
	}
}

// maxSmoothWeightedCreditRounds bounds compensation after a long sticky session.
const maxSmoothWeightedCreditRounds int64 = 32

// boundChargeFits admits the whole sticky delta or none. Both reservation and
// commit include outstanding deltas: committed credit alone can transiently
// exceed the cap while successful reservations settle in a different order.
func (s *weightedPoolState) boundChargeFits(selected int, totalWeight int64) bool {
	limit := int64(math.MaxInt64)
	if totalWeight <= math.MaxInt64/maxSmoothWeightedCreditRounds {
		limit = totalWeight * maxSmoothWeightedCreditRounds
	}
	for i, member := range s.members {
		delta := member.weight
		if i == selected {
			delta -= totalWeight
		}
		current := saturatingAddInt64(member.current, member.pending)
		next := saturatingAddInt64(current, delta)
		if next < -limit || next > limit {
			return false
		}
	}
	return true
}

func (s *weightedPoolState) charge(selected int, bound bool) {
	var totalWeight int64
	for _, member := range s.members {
		totalWeight = saturatingAddInt64(totalWeight, member.weight)
	}
	if !bound || s.boundChargeFits(selected, totalWeight) {
		// At the cap, forced traffic cannot erase a small peer's earnings.
		// Pending repayments still count until they actually roll back.
		for i := range s.members {
			member := &s.members[i]
			member.current = saturatingAddInt64(member.current, member.weight)
		}
		member := &s.members[selected]
		member.current = saturatingAddInt64(member.current, -totalWeight)
	}
	if s.inFlight == 0 {
		s.boundCredits(totalWeight)
	}
}

// boundCredits runs only once the pool settles. Net pending deltas can be zero
// even with live reservations, so the in-flight count, not len(pending), decides
// when clipping cannot irreversibly discard another reservation's offset.
func (s *weightedPoolState) boundCredits(totalWeight int64) {
	limit := int64(math.MaxInt64)
	if totalWeight <= math.MaxInt64/maxSmoothWeightedCreditRounds {
		limit = totalWeight * maxSmoothWeightedCreditRounds
	}
	var minCredit, maxCredit int64 = math.MaxInt64, math.MinInt64
	for _, member := range s.members {
		minCredit = min(minCredit, member.current)
		maxCredit = max(maxCredit, member.current)
	}
	// A common offset keeps leading credits distinct at the compensation bound.
	// Independent clamps can repeatedly tie the leaders and starve the later
	// candidate in deterministic selection order.
	minOffset := saturatingAddInt64(maxCredit, -limit)
	maxOffset := saturatingAddInt64(minCredit, limit)
	var offset int64
	if minOffset > 0 || minOffset > maxOffset {
		offset = minOffset
	} else if maxOffset < 0 {
		offset = maxOffset
	}
	// If the full spread cannot fit, cap only the trailing debt after rebasing
	// the leader to the upper bound. Do not flatten leading candidates' credits.
	for i := range s.members {
		member := &s.members[i]
		member.current = max(-limit, saturatingAddInt64(member.current, -offset))
	}
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
