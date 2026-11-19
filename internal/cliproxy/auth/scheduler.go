package auth

import (
	"context"
	"hash/maphash"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// schedulerStrategy identifies which built-in routing semantics the scheduler should apply.
type schedulerStrategy int

type schedulerModelResolver func(auth *Auth, routeModel string) string
type schedulerAvailabilityResolver func(auth *Auth, now time.Time) *Auth

const (
	schedulerStrategyCustom schedulerStrategy = iota
	schedulerStrategyRoundRobin
	schedulerStrategyFillFirst
	schedulerStrategyCurrent
)

// scheduledState describes how an auth currently participates in a model shard.
type scheduledState int

const (
	scheduledStateReady scheduledState = iota
	scheduledStateCooldown
	scheduledStateBlocked
	scheduledStateDisabled
)

// authScheduler keeps the incremental provider/model scheduling state used by Manager.
type authScheduler struct {
	mu              sync.Mutex
	strategy        schedulerStrategy
	modelResolver   schedulerModelResolver
	availability    schedulerAvailabilityResolver
	providers       map[string]*providerScheduler
	authProviders   map[string]string
	mixedCursors    map[mixedCursorKey]int
	mixedPoolStarts uint64
}

const maxMixedCursors = 4096

var mixedPoolSeed = maphash.MakeSeed()

type mixedIDSetKey struct {
	sum, xor uint64
	count    int
}

// mixedIDSetFingerprint is order-independent. It affects rotation only, never eligibility.
func mixedIDSetFingerprint(ids map[string]struct{}) mixedIDSetKey {
	key := mixedIDSetKey{count: len(ids)}
	for id := range ids {
		hash := maphash.String(mixedPoolSeed, id)
		key.sum += hash
		key.xor ^= hash
	}
	return key
}

type mixedCursorKey struct {
	route, weights, policy string
	allowed, tried         mixedIDSetKey
	round, defaultRetry    int
	hasAllowlist           bool
}

// providerScheduler stores auth metadata and model shards for a single provider.
type providerScheduler struct {
	providerKey   string
	modelResolver schedulerModelResolver
	availability  schedulerAvailabilityResolver
	auths         map[string]*scheduledAuthMeta
	modelShards   map[string]*modelScheduler
}

// scheduledAuthMeta stores the immutable scheduling fields derived from an auth snapshot.
type scheduledAuthMeta struct {
	auth               *Auth
	providerKey        string
	priority           int
	websocketEnabled   bool
	supportedModelSet  map[string]struct{}
	registeredModelSet map[string]struct{}
}

// modelScheduler tracks ready and blocked auths for one provider/model combination.
type modelScheduler struct {
	modelKey              string
	modelResolver         schedulerModelResolver
	availability          schedulerAvailabilityResolver
	entries               map[string]*scheduledAuth
	priorityOrder         []int
	readyByPriority       map[int]*readyBucket
	readyVersion          uint64
	blocked               cooldownQueue
	retryRoundAll         dispatchAvailabilityAggregate
	retryRoundAlphaSearch dispatchAvailabilityAggregate
}

// scheduledAuth stores the runtime scheduling state for a single auth inside a model shard.
type scheduledAuth struct {
	id                      string // Immutable; safe to read from a published ready view.
	meta                    *scheduledAuthMeta
	auth                    *Auth
	schedulerEligible       bool
	state                   scheduledState
	nextRetryAt             time.Time
	retryRoundEligible      bool
	retryRoundCooldown      bool
	retryRoundNext          time.Time
	requestRetryOverride    int
	hasRequestRetryOverride bool
	alphaSearchAllowed      bool
}

// dispatchAvailabilityAggregate stores request-independent retry-round state
// for a provider/model shard.
type dispatchAvailabilityAggregate struct {
	total                      int
	cooldownCount              int
	earliest                   time.Time
	inheritedRequestRetryCount int
	maxRequestRetryOverride    int
	hasRequestRetryOverride    bool
}

func (a *dispatchAvailabilityAggregate) add(entry *scheduledAuth) {
	if a == nil || entry == nil || !entry.retryRoundEligible {
		return
	}
	a.total++
	if entry.hasRequestRetryOverride {
		if !a.hasRequestRetryOverride || entry.requestRetryOverride > a.maxRequestRetryOverride {
			a.maxRequestRetryOverride = entry.requestRetryOverride
		}
		a.hasRequestRetryOverride = true
	} else {
		a.inheritedRequestRetryCount++
	}
	if !entry.retryRoundCooldown {
		return
	}
	a.cooldownCount++
	if !entry.retryRoundNext.IsZero() && (a.earliest.IsZero() || entry.retryRoundNext.Before(a.earliest)) {
		a.earliest = entry.retryRoundNext
	}
}

func (a *dispatchAvailabilityAggregate) merge(other dispatchAvailabilityAggregate) {
	if a == nil || other.total == 0 {
		return
	}
	a.total += other.total
	a.cooldownCount += other.cooldownCount
	a.inheritedRequestRetryCount += other.inheritedRequestRetryCount
	if !other.earliest.IsZero() && (a.earliest.IsZero() || other.earliest.Before(a.earliest)) {
		a.earliest = other.earliest
	}
	if other.hasRequestRetryOverride && (!a.hasRequestRetryOverride || other.maxRequestRetryOverride > a.maxRequestRetryOverride) {
		a.maxRequestRetryOverride = other.maxRequestRetryOverride
	}
	a.hasRequestRetryOverride = a.hasRequestRetryOverride || other.hasRequestRetryOverride
}

func (a dispatchAvailabilityAggregate) summary(now time.Time, defaultRequestRetry int) dispatchAvailabilitySummary {
	summary := dispatchAvailabilitySummary{
		now:           now,
		total:         a.total,
		cooldownCount: a.cooldownCount,
		earliest:      a.earliest,
	}
	if a.total == 0 {
		return summary
	}
	if defaultRequestRetry < 0 {
		defaultRequestRetry = 0
	}
	if a.inheritedRequestRetryCount > 0 {
		summary.requestRetry = defaultRequestRetry
	}
	if a.hasRequestRetryOverride && a.maxRequestRetryOverride > summary.requestRetry {
		summary.requestRetry = a.maxRequestRetryOverride
	}
	summary.hasRequestRetry = true
	return summary
}

// readyBucket keeps the ready views for one priority level.
type readyBucket struct {
	all, ws     readyView
	retry       readyRetryCounts
	alphaSearch readyRetryCounts
	pickFacts   []readyPickFacts // Immutable; indexed by the position in all.flat.
}

type readyPickFacts struct {
	retry       int // Negative means the credential inherits the request default.
	alphaSearch bool
}

// readyRetryCounts answers retry-round counts without walking the ready view.
type readyRetryCounts struct {
	total     int
	overrides []int // Sorted; credentials without an override use the request default.
}

func (c *readyRetryCounts) add(entry *scheduledAuth) {
	c.total++
	if entry.hasRequestRetryOverride {
		c.overrides = append(c.overrides, entry.requestRetryOverride)
	}
}

func (c readyRetryCounts) count(round, defaultRetry int) int {
	if round == 0 {
		return c.total
	}
	count := len(c.overrides) - sort.SearchInts(c.overrides, round)
	if defaultRetry >= round {
		count += c.total - len(c.overrides)
	}
	return count
}

// readyView holds the selection order for round-robin traversal.
type readyView struct {
	flat   []*scheduledAuth
	cursor int
}

// cooldownQueue is the blocked auth collection ordered by next retry time during rebuilds.
type cooldownQueue []*scheduledAuth

type readyViewCursorState struct {
	cursor int
}

type readyBucketCursorState struct {
	all readyViewCursorState
	ws  readyViewCursorState
}

// snapshotReadyViewCursors snapshots a ready view cursors.
func snapshotReadyViewCursors(view readyView) readyViewCursorState {
	return readyViewCursorState{cursor: view.cursor}
}

// restoreReadyViewCursors restores a ready view cursors.
func restoreReadyViewCursors(view *readyView, state readyViewCursorState) {
	// Validate input data before converting it into runtime state.
	if view == nil {
		return
	}
	if len(view.flat) > 0 {
		view.cursor = normalizeCursor(state.cursor, len(view.flat))
	}
}

// normalizeCursor normalizes a cursor.
func normalizeCursor(cursor, size int) int {
	if size <= 0 || cursor <= 0 {
		return 0
	}
	cursor = cursor % size
	if cursor < 0 {
		cursor += size
	}
	return cursor
}

// newAuthScheduler constructs an empty scheduler configured for the supplied selector strategy.
func newAuthScheduler(selector Selector, modelResolver schedulerModelResolver, availabilityResolver schedulerAvailabilityResolver) *authScheduler {
	return &authScheduler{
		strategy:      selectorStrategy(selector),
		modelResolver: modelResolver,
		availability:  availabilityResolver,
		providers:     make(map[string]*providerScheduler),
		authProviders: make(map[string]string),
		mixedCursors:  make(map[mixedCursorKey]int),
	}
}

// selectorStrategy maps a selector implementation to the scheduler semantics it should emulate.
func selectorStrategy(selector Selector) schedulerStrategy {
	switch selector.(type) {
	case *FillFirstSelector:
		return schedulerStrategyFillFirst
	case nil, *RoundRobinSelector:
		return schedulerStrategyRoundRobin
	default:
		return schedulerStrategyCustom
	}
}

// setSelector updates the active built-in strategy and resets mixed-provider cursors.
func (s *authScheduler) setSelector(selector Selector) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strategy = selectorStrategy(selector)
	clear(s.mixedCursors)
}

// resetModelShards clears derived model shards after model resolver inputs change.
func (s *authScheduler) resetModelShards() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, providerState := range s.providers {
		if providerState == nil {
			continue
		}
		providerState.modelResolver = s.modelResolver
		providerState.availability = s.availability
		providerState.modelShards = make(map[string]*modelScheduler)
	}
	clear(s.mixedCursors)
}

// upsertAuth incrementally synchronizes one auth into the scheduler.
func (s *authScheduler) upsertAuth(auth *Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertAuthLocked(auth, time.Now())
}

// removeAuth deletes one auth from every scheduler shard that references it.
func (s *authScheduler) removeAuth(authID string) {
	if s == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeAuthLocked(authID)
}

// pickSingle returns the next auth for a single provider/model request using scheduler state.
func (s *authScheduler) pickSingle(ctx context.Context, provider, model string, opts Options, tried map[string]struct{}) (*Auth, error) {
	return s.pickSingleWithStrategy(provider, model, opts, tried, schedulerStrategyCurrent, nil)
}

func (s *authScheduler) pickSingleWithStrategy(provider, model string, opts Options, tried map[string]struct{}, strategy schedulerStrategy, candidateIDs map[string]struct{}) (*Auth, error) {
	// Build the candidate view before applying availability rules.
	if s == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	modelKey := canonicalModelKey(model)
	preferWebsocket := false
	predicate := newSchedulerPickFilter(opts, tried, candidateIDs).predicate()

	s.mu.Lock()
	defer s.mu.Unlock()
	if strategy == schedulerStrategyCurrent {
		strategy = s.strategy
	}
	providerState := s.providers[providerKey]
	if providerState == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	shard := providerState.ensureModelLocked(modelKey, time.Now())
	if shard == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if picked := shard.pickReadyLocked(preferWebsocket, strategy, predicate); picked != nil {
		return picked, nil
	}
	return nil, shard.unavailableErrorLocked(provider, model, predicate)
}

// pickMixed returns the next auth and provider for a mixed-provider request.
func (s *authScheduler) pickMixed(ctx context.Context, providers []string, model string, opts Options, tried map[string]struct{}) (*Auth, string, error) {
	return s.pickMixedWithStrategy(ctx, providers, model, opts, tried, schedulerStrategyCurrent, nil)
}

func (s *authScheduler) pickMixedWithStrategy(ctx context.Context, providers []string, model string, opts Options, tried map[string]struct{}, strategy schedulerStrategy, candidateIDs map[string]struct{}) (*Auth, string, error) {
	if s == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	normalized := normalizeProviderKeys(providers)
	if len(normalized) == 0 {
		return nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if len(normalized) == 1 {
		// When a single provider is eligible, reuse pickSingle so provider-specific preferences
		// (for example Codex websocket transport) are applied consistently.
		providerKey := normalized[0]
		picked, errPick := s.pickSingleWithStrategy(providerKey, model, opts, tried, strategy, candidateIDs)
		if errPick != nil {
			return nil, "", errPick
		}
		if picked == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		return picked, providerKey, nil
	}
	modelKey := canonicalModelKey(model)
	filter := newSchedulerPickFilter(opts, tried, candidateIDs)
	cursorKey := mixedCursorKey{
		route: strings.Join(normalized, ",") + ":" + modelKey, policy: filter.policy,
		allowed: mixedIDSetFingerprint(filter.allowed), tried: mixedIDSetFingerprint(filter.tried),
		round: filter.round, defaultRetry: filter.defaultRetry, hasAllowlist: filter.allowed != nil,
	}
	const narrowAllowlistLimit = 128
	broad := len(filter.allowed) > narrowAllowlistLimit
	shards := make([]*modelScheduler, len(normalized))
	pools := make([]mixedReadyPool, len(normalized))
	for {
		var snapshots []mixedReadySnapshot
		if broad {
			if ctx != nil && ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			snapshots = s.mixedReadySnapshots(normalized, modelKey)
			for i, snapshot := range snapshots {
				pools[i] = snapshot.filteredPool(filter)
			}
		}
		s.mu.Lock()
		stable := true
		now := time.Now()
		for i, provider := range normalized {
			shards[i] = s.providers[provider].ensureModelLocked(modelKey, now)
			if broad && !snapshots[i].matches(shards[i]) {
				stable = false
			}
		}
		if stable {
			break
		}
		// The scanned ready view changed. Recompute outside the lock rather
		// than widening eligibility or moving the broad scan into this section.
		s.mu.Unlock()
	}
	defer s.mu.Unlock()
	if strategy == schedulerStrategyCurrent {
		strategy = s.strategy
	}
	predicate := filter.predicate()
	var allowedByProvider map[string][]string
	if !broad && filter.allowed != nil {
		allowedByProvider = make(map[string][]string, len(normalized))
		for id := range filter.allowed {
			if _, tried := filter.tried[id]; !tried {
				provider := s.authProviders[id]
				allowedByProvider[provider] = append(allowedByProvider[provider], id)
			}
		}
	}
	bestPriority := 0
	hasCandidate := false
	for providerIndex, shard := range shards {
		if shard == nil {
			continue
		}
		if !broad {
			pools[providerIndex] = shard.mixedReadyPool(filter, predicate, allowedByProvider[normalized[providerIndex]])
		}
		pool := pools[providerIndex]
		if pool.count == 0 {
			continue
		}
		if !hasCandidate || pool.priority > bestPriority {
			bestPriority = pool.priority
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	if strategy == schedulerStrategyFillFirst {
		for providerIndex, providerKey := range normalized {
			pool := &pools[providerIndex]
			if pool.count == 0 || pool.priority != bestPriority {
				continue
			}
			picked := pool.pick(strategy, predicate)
			if picked != nil {
				return picked, providerKey, nil
			}
		}
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	var weightKey [32]byte
	weightBytes := weightKey[:0]
	weights := make([]int, len(normalized))
	segmentStarts := make([]int, len(normalized))
	segmentEnds := make([]int, len(normalized))
	totalWeight := 0
	for providerIndex, pool := range pools {
		segmentStarts[providerIndex] = totalWeight
		if pool.priority == bestPriority {
			weights[providerIndex] = pool.count
		}
		totalWeight += weights[providerIndex]
		segmentEnds[providerIndex] = totalWeight
		weightBytes = strconv.AppendInt(weightBytes, int64(weights[providerIndex]), 10)
		weightBytes = append(weightBytes, ',')
	}
	cursorKey.weights = string(weightBytes)
	if totalWeight == 0 {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	cursor, knownPool := s.mixedCursors[cursorKey]
	if !knownPool {
		// Stagger first picks across new and evicted pools without sharing their rotation.
		cursor = int(s.mixedPoolStarts % uint64(totalWeight))
	}
	startSlot := cursor % totalWeight
	startProviderIndex := -1
	for providerIndex := range normalized {
		if weights[providerIndex] == 0 {
			continue
		}
		if startSlot < segmentEnds[providerIndex] {
			startProviderIndex = providerIndex
			break
		}
	}
	if startProviderIndex < 0 {
		return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
	}

	slot := startSlot
	for offset := 0; offset < len(normalized); offset++ {
		providerIndex := (startProviderIndex + offset) % len(normalized)
		if weights[providerIndex] == 0 {
			continue
		}
		if providerIndex != startProviderIndex {
			slot = segmentStarts[providerIndex]
		}
		providerKey := normalized[providerIndex]
		picked := pools[providerIndex].pick(schedulerStrategyRoundRobin, predicate)
		if picked == nil {
			continue
		}
		if !knownPool {
			s.mixedPoolStarts++
			if len(s.mixedCursors) >= maxMixedCursors {
				for key := range s.mixedCursors {
					delete(s.mixedCursors, key)
					break
				}
			}
		}
		s.mixedCursors[cursorKey] = slot + 1
		return picked, providerKey, nil
	}
	return nil, "", s.mixedUnavailableErrorLocked(normalized, model, predicate)
}

type mixedReadySnapshot struct {
	shard   *modelScheduler
	version uint64
	buckets map[int]*readyBucket
}

func (v mixedReadySnapshot) matches(shard *modelScheduler) bool {
	return v.shard == shard && (shard == nil || v.version == shard.readyVersion)
}

// mixedReadySnapshots captures immutable ready membership and filtering facts.
// The caller verifies the shard version before consuming the result.
func (s *authScheduler) mixedReadySnapshots(providers []string, model string) []mixedReadySnapshot {
	snapshots := make([]mixedReadySnapshot, len(providers))
	s.mu.Lock()
	now := time.Now()
	for i, provider := range providers {
		shard := s.providers[provider].ensureModelLocked(model, now)
		if shard != nil {
			snapshots[i] = mixedReadySnapshot{shard, shard.readyVersion, shard.readyByPriority}
		}
	}
	s.mu.Unlock()
	return snapshots
}

func (v mixedReadySnapshot) filteredPool(filter schedulerPickFilter) mixedReadyPool {
	var pool mixedReadyPool
	for priority, bucket := range v.buckets {
		if pool.count > 0 && priority <= pool.priority {
			continue
		}
		var positions []int
		count := 0
		for i, entry := range bucket.all.flat {
			_, allowed := filter.allowed[entry.id]
			_, tried := filter.tried[entry.id]
			facts := bucket.pickFacts[i]
			limit := facts.retry
			if limit < 0 {
				limit = filter.defaultRetry
			}
			if !allowed || tried || limit < filter.round ||
				(filter.policy != "" && (filter.policy != CredentialPolicyCodexAlphaSearchV1 || !facts.alphaSearch)) {
				if positions == nil {
					positions = make([]int, count, len(bucket.all.flat)-1)
					for j := range positions {
						positions[j] = j
					}
				}
				continue
			}
			count++
			if positions != nil {
				positions = append(positions, i)
			}
		}
		if count > 0 {
			pool = mixedReadyPool{view: &bucket.all, priority: priority, count: count, indexed: true, positions: positions}
		}
	}
	return pool
}

// summarizeDispatchAvailability reads retry-round state from the same
// provider/model shards used by built-in credential selection.
func (s *authScheduler) summarizeDispatchAvailability(providers []string, model string, opts Options, excludedAuthIDs map[string]struct{}, defaultRequestRetry int) dispatchAvailabilitySummary {
	now := time.Now()
	if s == nil {
		return dispatchAvailabilitySummary{now: now}
	}
	normalized := normalizeProviderKeys(providers)
	if len(normalized) == 0 {
		return dispatchAvailabilitySummary{now: now}
	}
	modelKey := canonicalModelKey(model)
	if !modelAllowedByID(modelKey, allowedModelIDsFromOptions(opts)) {
		return dispatchAvailabilitySummary{now: now}
	}
	allowedAuthIDs := allowedAuthIDsFromOptions(opts)
	credentialPolicy := credentialPolicyFromOptions(opts)
	retryRound := requestRetryRoundFromOptions(opts)

	s.mu.Lock()
	defer s.mu.Unlock()

	providerSet := make(map[string]struct{}, len(normalized))
	shards := make(map[string]*modelScheduler, len(normalized))
	for _, providerKey := range normalized {
		providerSet[providerKey] = struct{}{}
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		if shard := providerState.ensureModelLocked(modelKey, now); shard != nil {
			shards[providerKey] = shard
		}
	}

	aggregate := dispatchAvailabilityAggregate{}
	if allowedAuthIDs != nil {
		for authID := range allowedAuthIDs {
			if _, excluded := excludedAuthIDs[authID]; excluded {
				continue
			}
			providerKey := s.authProviders[authID]
			if _, requested := providerSet[providerKey]; !requested {
				continue
			}
			shard := shards[providerKey]
			if shard == nil {
				continue
			}
			entry := shard.entries[authID]
			if entry == nil || !credentialPolicyAllows(credentialPolicy, entry.auth) {
				continue
			}
			if !requestRetryRoundAllowed(entry.auth, retryRound, defaultRequestRetry) {
				continue
			}
			aggregate.add(entry)
		}
		return aggregate.summary(now, defaultRequestRetry)
	}

	if len(excludedAuthIDs) == 0 && retryRound == 0 {
		for _, shard := range shards {
			switch credentialPolicy {
			case "":
				aggregate.merge(shard.retryRoundAll)
			case CredentialPolicyCodexAlphaSearchV1:
				aggregate.merge(shard.retryRoundAlphaSearch)
			default:
				for _, entry := range shard.entries {
					if credentialPolicyAllows(credentialPolicy, entry.auth) {
						aggregate.add(entry)
					}
				}
			}
		}
		return aggregate.summary(now, defaultRequestRetry)
	}

	// Exclusions created while resolving stale full credentials only occur on
	// the exhausted slow path. Scan the already-resolved shard entries there
	// instead of returning to the full manager credential set.
	for _, shard := range shards {
		for authID, entry := range shard.entries {
			if _, excluded := excludedAuthIDs[authID]; excluded {
				continue
			}
			if !credentialPolicyAllows(credentialPolicy, entry.auth) {
				continue
			}
			if !requestRetryRoundAllowed(entry.auth, retryRound, defaultRequestRetry) {
				continue
			}
			aggregate.add(entry)
		}
	}
	return aggregate.summary(now, defaultRequestRetry)
}

// mixedUnavailableErrorLocked synthesizes the mixed-provider cooldown or unavailable error.
func (s *authScheduler) mixedUnavailableErrorLocked(providers []string, model string, predicate func(*scheduledAuth) bool) error {
	// Build the candidate view before applying availability rules.
	now := time.Now()
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, providerKey := range providers {
		providerState := s.providers[providerKey]
		if providerState == nil {
			continue
		}
		shard := providerState.ensureModelLocked(canonicalModelKey(model), now)
		if shard == nil {
			continue
		}
		localTotal, localCooldownCount, localEarliest := shard.availabilitySummaryLocked(predicate)
		total += localTotal
		cooldownCount += localCooldownCount
		if !localEarliest.IsZero() && (earliest.IsZero() || localEarliest.Before(earliest)) {
			earliest = localEarliest
		}
	}
	if total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if cooldownCount == total && !earliest.IsZero() {
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, "", resetIn)
	}
	return &Error{Code: "auth_unavailable", Message: "no auth available"}
}

// normalizeProviderKeys lowercases, trims, and de-duplicates provider keys while preserving order.
func normalizeProviderKeys(providers []string) []string {
	seen := make(map[string]struct{}, len(providers))
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" {
			continue
		}
		if _, ok := seen[providerKey]; ok {
			continue
		}
		seen[providerKey] = struct{}{}
		out = append(out, providerKey)
	}
	return out
}

// containsProvider reports whether provider is present in the normalized provider list.
func containsProvider(providers []string, provider string) bool {
	for _, candidate := range providers {
		if candidate == provider {
			return true
		}
	}
	return false
}

// upsertAuthLocked updates one auth in-place while the scheduler mutex is held.
func (s *authScheduler) upsertAuthLocked(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if authID == "" || providerKey == "" || auth.Disabled {
		s.removeAuthLocked(authID)
		return
	}
	if previousProvider := s.authProviders[authID]; previousProvider != "" && previousProvider != providerKey {
		if previousState := s.providers[previousProvider]; previousState != nil {
			previousState.removeAuthLocked(authID)
		}
	}
	meta := buildScheduledAuthMeta(auth)
	s.authProviders[authID] = providerKey
	s.ensureProviderLocked(providerKey).upsertAuthLocked(meta, now)
}

// removeAuthLocked removes one auth from the scheduler while the scheduler mutex is held.
func (s *authScheduler) removeAuthLocked(authID string) {
	if authID == "" {
		return
	}
	if providerKey := s.authProviders[authID]; providerKey != "" {
		if providerState := s.providers[providerKey]; providerState != nil {
			providerState.removeAuthLocked(authID)
		}
		delete(s.authProviders, authID)
	}
}

// ensureProviderLocked returns the provider scheduler for providerKey, creating it when needed.
func (s *authScheduler) ensureProviderLocked(providerKey string) *providerScheduler {
	if s.providers == nil {
		s.providers = make(map[string]*providerScheduler)
	}
	providerState := s.providers[providerKey]
	if providerState == nil {
		providerState = &providerScheduler{
			providerKey:   providerKey,
			modelResolver: s.modelResolver,
			availability:  s.availability,
			auths:         make(map[string]*scheduledAuthMeta),
			modelShards:   make(map[string]*modelScheduler),
		}
		s.providers[providerKey] = providerState
	}
	return providerState
}

// buildScheduledAuthMeta extracts the scheduling metadata needed for shard bookkeeping.
func buildScheduledAuthMeta(auth *Auth) *scheduledAuthMeta {
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	supportedModelSet, registeredModelSet := supportedModelSetsForAuth(auth.ID)
	return &scheduledAuthMeta{
		auth:               auth,
		providerKey:        providerKey,
		priority:           authPriority(auth),
		websocketEnabled:   authWebsocketsEnabled(auth),
		supportedModelSet:  supportedModelSet,
		registeredModelSet: registeredModelSet,
	}
}

// supportedModelSetsForAuth snapshots both canonical scheduler models and the
// exact registry IDs used by dispatch availability filtering.
func supportedModelSetsForAuth(authID string) (map[string]struct{}, map[string]struct{}) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(models) == 0 {
		return nil, nil
	}
	canonicalSet := make(map[string]struct{}, len(models))
	registeredSet := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		registeredModelKey := strings.ToLower(strings.TrimSpace(model.ID))
		if registeredModelKey != "" {
			registeredSet[registeredModelKey] = struct{}{}
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		canonicalSet[modelKey] = struct{}{}
	}
	return canonicalSet, registeredSet
}

// upsertAuthLocked updates every existing model shard that can reference the auth metadata.
func (p *providerScheduler) upsertAuthLocked(meta *scheduledAuthMeta, now time.Time) {
	if p == nil || meta == nil || meta.auth == nil {
		return
	}
	p.auths[meta.auth.ID] = meta
	for modelKey, shard := range p.modelShards {
		if shard == nil {
			continue
		}
		if !meta.supportsModel(modelKey) && !meta.hasRegisteredModel(modelKey) {
			shard.removeEntryLocked(meta.auth.ID)
			continue
		}
		shard.upsertEntryLocked(meta, now)
	}
}

// removeAuthLocked removes an auth from all model shards owned by the provider scheduler.
func (p *providerScheduler) removeAuthLocked(authID string) {
	if p == nil || authID == "" {
		return
	}
	delete(p.auths, authID)
	for _, shard := range p.modelShards {
		if shard != nil {
			shard.removeEntryLocked(authID)
		}
	}
}

// ensureModelLocked returns the shard for modelKey, building it lazily from provider auths.
func (p *providerScheduler) ensureModelLocked(modelKey string, now time.Time) *modelScheduler {
	// Normalize source data before building the derived payload.
	if p == nil {
		return nil
	}
	modelKey = canonicalModelKey(modelKey)
	if shard, ok := p.modelShards[modelKey]; ok && shard != nil {
		shard.promoteExpiredLocked(now)
		return shard
	}
	shard := &modelScheduler{
		modelKey:        modelKey,
		modelResolver:   p.modelResolver,
		availability:    p.availability,
		entries:         make(map[string]*scheduledAuth),
		readyByPriority: make(map[int]*readyBucket),
	}
	for _, meta := range p.auths {
		if meta == nil || meta.auth == nil || (!meta.supportsModel(modelKey) && !meta.hasRegisteredModel(modelKey)) {
			continue
		}
		entry := &scheduledAuth{id: meta.auth.ID, meta: meta, auth: meta.auth}
		if shard.availability != nil {
			entry.auth = shard.availability(meta.auth, now)
		}
		shard.refreshEntryStateLocked(entry, now)
		shard.entries[meta.auth.ID] = entry
	}
	shard.rebuildIndexesLocked()
	p.modelShards[modelKey] = shard
	return shard
}

// runtimeModelKeyForAuth resolves the auth-specific model key used by runtime state.
func (m *modelScheduler) runtimeModelKeyForAuth(auth *Auth) string {
	if m == nil {
		return ""
	}
	if m.modelResolver != nil {
		resolved := strings.TrimSpace(m.modelResolver(auth, m.modelKey))
		if key := canonicalModelKey(resolved); key != "" {
			return key
		}
	}
	return canonicalModelKey(m.modelKey)
}

// supportsModel reports whether the auth metadata currently supports modelKey.
func (m *scheduledAuthMeta) supportsModel(modelKey string) bool {
	modelKey = canonicalModelKey(modelKey)
	if modelKey == "" {
		return true
	}
	if len(m.supportedModelSet) == 0 {
		return false
	}
	_, ok := m.supportedModelSet[modelKey]
	return ok
}

func (m *scheduledAuthMeta) hasRegisteredModel(modelKey string) bool {
	modelKey = strings.ToLower(strings.TrimSpace(modelKey))
	if modelKey == "" {
		return true
	}
	if m == nil || len(m.registeredModelSet) == 0 {
		return false
	}
	_, ok := m.registeredModelSet[modelKey]
	return ok
}

func (m *modelScheduler) refreshEntryStateLocked(entry *scheduledAuth, now time.Time) {
	if entry == nil {
		return
	}
	entry.state = scheduledStateBlocked
	entry.nextRetryAt = time.Time{}
	entry.schedulerEligible = false
	entry.retryRoundEligible = false
	entry.retryRoundCooldown = false
	entry.retryRoundNext = time.Time{}
	entry.requestRetryOverride = 0
	entry.hasRequestRetryOverride = false
	entry.alphaSearchAllowed = false
	if m == nil || entry.auth == nil {
		return
	}

	entry.requestRetryOverride, entry.hasRequestRetryOverride = entry.auth.RequestRetryOverride()
	entry.alphaSearchAllowed = credentialPolicyAllows(CredentialPolicyCodexAlphaSearchV1, entry.auth)
	entry.schedulerEligible = entry.meta != nil && entry.meta.supportsModel(m.modelKey)
	runtimeModelKey := m.runtimeModelKeyForAuth(entry.auth)
	blockedModel, blocked, reason, next := dispatchModelBlockStatus(entry.auth, runtimeModelKey, m.modelKey, now)
	switch {
	case !blocked:
		entry.state = scheduledStateReady
	case reason == blockReasonCooldown:
		entry.state = scheduledStateCooldown
		entry.nextRetryAt = next
	case reason == blockReasonDisabled:
		entry.state = scheduledStateDisabled
	default:
		entry.state = scheduledStateBlocked
		entry.nextRetryAt = next
	}
	if entry.meta == nil || !entry.meta.hasRegisteredModel(canonicalModelKey(m.modelKey)) {
		return
	}

	available, retryReason, retryNext := classifyDispatchAvailability(entry.auth, blockedModel, blocked, reason, next, now)
	if !available && retryReason != blockReasonCooldown {
		return
	}
	entry.retryRoundEligible = true
	if !available {
		entry.retryRoundCooldown = true
		entry.retryRoundNext = retryNext
	}
}

// upsertEntryLocked updates or inserts one auth entry and rebuilds indexes when ordering changes.
func (m *modelScheduler) upsertEntryLocked(meta *scheduledAuthMeta, now time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	if m == nil || meta == nil || meta.auth == nil {
		return
	}
	entry, ok := m.entries[meta.auth.ID]
	if !ok || entry == nil {
		entry = &scheduledAuth{id: meta.auth.ID}
		m.entries[meta.auth.ID] = entry
	}
	previousState := entry.state
	previousNextRetryAt := entry.nextRetryAt
	previousSchedulerEligible := entry.schedulerEligible
	previousRetryRoundEligible := entry.retryRoundEligible
	previousRetryRoundCooldown := entry.retryRoundCooldown
	previousRetryRoundNext := entry.retryRoundNext
	previousRequestRetryOverride := entry.requestRetryOverride
	previousHasRequestRetryOverride := entry.hasRequestRetryOverride
	previousAlphaSearchAllowed := entry.alphaSearchAllowed
	previousPriority := 0
	previousWebsocketEnabled := false
	if entry.meta != nil {
		previousPriority = entry.meta.priority
		previousWebsocketEnabled = entry.meta.websocketEnabled
	}

	entry.meta = meta
	entry.auth = meta.auth
	if m.availability != nil {
		entry.auth = m.availability(meta.auth, now)
	}
	m.refreshEntryStateLocked(entry, now)

	if ok &&
		previousState == entry.state &&
		previousNextRetryAt.Equal(entry.nextRetryAt) &&
		previousSchedulerEligible == entry.schedulerEligible &&
		previousRetryRoundEligible == entry.retryRoundEligible &&
		previousRetryRoundCooldown == entry.retryRoundCooldown &&
		previousRetryRoundNext.Equal(entry.retryRoundNext) &&
		previousRequestRetryOverride == entry.requestRetryOverride &&
		previousHasRequestRetryOverride == entry.hasRequestRetryOverride &&
		previousAlphaSearchAllowed == entry.alphaSearchAllowed &&
		previousPriority == meta.priority &&
		previousWebsocketEnabled == meta.websocketEnabled {
		return
	}
	m.rebuildIndexesLocked()
}

// removeEntryLocked deletes one auth entry and rebuilds the shard indexes if needed.
func (m *modelScheduler) removeEntryLocked(authID string) {
	if m == nil || authID == "" {
		return
	}
	if _, ok := m.entries[authID]; !ok {
		return
	}
	delete(m.entries, authID)
	m.rebuildIndexesLocked()
}

// promoteExpiredLocked reevaluates blocked auths whose retry time has elapsed.
func (m *modelScheduler) promoteExpiredLocked(now time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	if m == nil || len(m.blocked) == 0 {
		return
	}
	changed := false
	for _, entry := range m.blocked {
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.nextRetryAt.IsZero() || entry.nextRetryAt.After(now) {
			// blocked is ordered by retry deadline with zero deadlines last, so
			// no later entry can be promoted during this pass.
			break
		}
		previous := *entry
		m.refreshEntryStateLocked(entry, now)
		// An expired retry deadline may still be fenced by refresh acquisition.
		// Only changed scheduling facts invalidate published ready snapshots.
		if previous != *entry {
			changed = true
		}
	}
	if changed {
		m.rebuildIndexesLocked()
	}
}

// pickReadyLocked selects the next ready auth from the highest available priority bucket.
func (m *modelScheduler) pickReadyLocked(preferWebsocket bool, strategy schedulerStrategy, predicate func(*scheduledAuth) bool) *Auth {
	if m == nil {
		return nil
	}
	m.promoteExpiredLocked(time.Now())
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, predicate)
	if !okPriority {
		return nil
	}
	return m.pickReadyAtPriorityLocked(preferWebsocket, priorityReady, strategy, predicate)
}

// highestReadyPriorityLocked returns the highest priority bucket that still has a matching ready auth.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) highestReadyPriorityLocked(preferWebsocket bool, predicate func(*scheduledAuth) bool) (int, bool) {
	if m == nil {
		return 0, false
	}
	if preferWebsocket {
		// When downstream is websocket and Codex supports websocket transport, prefer websocket-enabled
		// credentials even if they are in a lower priority tier than HTTP-only credentials.
		for _, priority := range m.priorityOrder {
			bucket := m.readyByPriority[priority]
			if bucket == nil {
				continue
			}
			if bucket.ws.pickFirst(predicate) != nil {
				return priority, true
			}
		}
	}
	for _, priority := range m.priorityOrder {
		bucket := m.readyByPriority[priority]
		if bucket == nil {
			continue
		}
		if bucket.all.pickFirst(predicate) != nil {
			return priority, true
		}
	}
	return 0, false
}

// pickReadyAtPriorityLocked selects the next ready auth from a specific priority bucket.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) pickReadyAtPriorityLocked(preferWebsocket bool, priority int, strategy schedulerStrategy, predicate func(*scheduledAuth) bool) *Auth {
	// Build the candidate view before applying availability rules.
	if m == nil {
		return nil
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	if preferWebsocket && bucket.ws.pickFirst(predicate) != nil {
		view = &bucket.ws
	}
	var picked *scheduledAuth
	if strategy == schedulerStrategyFillFirst {
		picked = view.pickFirst(predicate)
	} else {
		picked = view.pickRoundRobin(predicate)
	}
	if picked == nil || picked.auth == nil {
		return nil
	}
	return picked.auth
}

// mixedReadyPool holds only the highest eligible priority of one provider.
type mixedReadyPool struct {
	view         *readyView
	priority     int
	count        int
	allowedReady []*scheduledAuth // Populated for narrow allowlists.
	indexed      bool
	positions    []int // Sorted; nil means the whole view is eligible.
}

func (m *modelScheduler) mixedReadyPool(filter schedulerPickFilter, predicate func(*scheduledAuth) bool, allowedIDs []string) mixedReadyPool {
	var pool mixedReadyPool
	if filter.allowed != nil {
		for _, id := range allowedIDs {
			entry := m.entries[id]
			if entry == nil || !entry.schedulerEligible || entry.state != scheduledStateReady || !predicate(entry) {
				continue
			}
			priority := entry.meta.priority
			if pool.count == 0 || priority > pool.priority {
				pool.priority = priority
				pool.allowedReady = pool.allowedReady[:0]
			} else if priority < pool.priority {
				continue
			}
			pool.allowedReady = append(pool.allowedReady, entry)
			pool.count = len(pool.allowedReady)
		}
		if pool.count > 0 {
			pool.view = &m.readyByPriority[pool.priority].all
		}
		return pool
	}

	var excludedByPriority map[int]int
	if len(filter.tried) > 0 {
		// Count the exclusions rather than scanning the ready bucket. Use the
		// same policy and retry filter, ignoring only the tried-ID check.
		matchesPolicy := schedulerPredicate(nil, nil, filter.policy, filter.round, filter.defaultRetry)
		excludedByPriority = make(map[int]int)
		for id := range filter.tried {
			entry := m.entries[id]
			if entry != nil && entry.schedulerEligible && entry.state == scheduledStateReady && matchesPolicy(entry) {
				excludedByPriority[entry.meta.priority]++
			}
		}
	}
	for _, priority := range m.priorityOrder {
		bucket := m.readyByPriority[priority]
		var count int
		switch filter.policy {
		case "":
			count = bucket.retry.count(filter.round, filter.defaultRetry)
		case CredentialPolicyCodexAlphaSearchV1:
			count = bucket.alphaSearch.count(filter.round, filter.defaultRetry)
		}
		count -= excludedByPriority[priority]
		if count > 0 {
			return mixedReadyPool{view: &bucket.all, priority: priority, count: count}
		}
	}
	return pool
}

func (p *mixedReadyPool) pick(strategy schedulerStrategy, predicate func(*scheduledAuth) bool) *Auth {
	var picked *scheduledAuth
	switch {
	case p.indexed:
		index := 0
		if strategy != schedulerStrategyFillFirst {
			index = p.view.cursor % len(p.view.flat)
		}
		if p.positions != nil {
			next := sort.SearchInts(p.positions, index)
			index = p.positions[next%len(p.positions)]
		}
		picked = p.view.flat[index]
		if strategy != schedulerStrategyFillFirst {
			p.view.cursor = index + 1
		}
	case p.allowedReady != nil:
		// Preserve the shared bucket cursor without walking credentials outside
		// the allowlist. The full ready view is sorted by auth ID.
		startID := ""
		if strategy != schedulerStrategyFillFirst {
			startID = p.view.flat[p.view.cursor%len(p.view.flat)].auth.ID
		}
		var first *scheduledAuth
		for _, entry := range p.allowedReady {
			id := entry.auth.ID
			if first == nil || id < first.auth.ID {
				first = entry
			}
			if id >= startID && (picked == nil || id < picked.auth.ID) {
				picked = entry
			}
		}
		if picked == nil {
			picked = first
		}
		if strategy != schedulerStrategyFillFirst {
			index := sort.Search(len(p.view.flat), func(i int) bool { return p.view.flat[i].auth.ID >= picked.auth.ID })
			p.view.cursor = index + 1
		}
	case strategy == schedulerStrategyFillFirst:
		picked = p.view.pickFirst(predicate)
	default:
		picked = p.view.pickRoundRobin(predicate)
	}
	if picked == nil {
		return nil
	}
	return picked.auth
}

// unavailableErrorLocked returns the correct unavailable or cooldown error for the shard.
func (m *modelScheduler) unavailableErrorLocked(provider, model string, predicate func(*scheduledAuth) bool) error {
	now := time.Now()
	total, cooldownCount, earliest := m.availabilitySummaryLocked(predicate)
	if total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if cooldownCount == total && !earliest.IsZero() {
		providerForError := provider
		if providerForError == "mixed" {
			providerForError = ""
		}
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, providerForError, resetIn)
	}
	return &Error{Code: "auth_unavailable", Message: "no auth available"}
}

// availabilitySummaryLocked summarizes total candidates, cooldown count, and earliest retry time.
func (m *modelScheduler) availabilitySummaryLocked(predicate func(*scheduledAuth) bool) (int, int, time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	if m == nil {
		return 0, 0, time.Time{}
	}
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, entry := range m.entries {
		if predicate != nil && !predicate(entry) {
			continue
		}
		if entry == nil || !entry.schedulerEligible {
			continue
		}
		total++
		if entry.auth == nil {
			continue
		}
		if entry.state != scheduledStateCooldown {
			continue
		}
		cooldownCount++
		if !entry.nextRetryAt.IsZero() && (earliest.IsZero() || entry.nextRetryAt.Before(earliest)) {
			earliest = entry.nextRetryAt
		}
	}
	return total, cooldownCount, earliest
}

// rebuildIndexesLocked reconstructs ready and blocked views from the current entry map.
func (m *modelScheduler) rebuildIndexesLocked() {
	// Keep validation before state changes so failures leave existing data intact.
	cursorStates := make(map[int]readyBucketCursorState, len(m.readyByPriority))
	for priority, bucket := range m.readyByPriority {
		if bucket == nil {
			continue
		}
		cursorStates[priority] = readyBucketCursorState{
			all: snapshotReadyViewCursors(bucket.all),
			ws:  snapshotReadyViewCursors(bucket.ws),
		}
	}

	m.readyVersion++
	m.readyByPriority = make(map[int]*readyBucket)
	m.priorityOrder = m.priorityOrder[:0]
	m.blocked = m.blocked[:0]
	m.retryRoundAll = dispatchAvailabilityAggregate{}
	m.retryRoundAlphaSearch = dispatchAvailabilityAggregate{}
	priorityBuckets := make(map[int][]*scheduledAuth)
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		m.retryRoundAll.add(entry)
		if entry.alphaSearchAllowed {
			m.retryRoundAlphaSearch.add(entry)
		}
		if !entry.schedulerEligible {
			switch entry.state {
			case scheduledStateCooldown, scheduledStateBlocked:
				m.blocked = append(m.blocked, entry)
			}
			continue
		}
		switch entry.state {
		case scheduledStateReady:
			priority := entry.meta.priority
			priorityBuckets[priority] = append(priorityBuckets[priority], entry)
		case scheduledStateCooldown, scheduledStateBlocked:
			m.blocked = append(m.blocked, entry)
		}
	}
	for priority, entries := range priorityBuckets {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].auth.ID < entries[j].auth.ID
		})
		bucket := buildReadyBucket(entries)
		if cursorState, ok := cursorStates[priority]; ok && bucket != nil {
			restoreReadyViewCursors(&bucket.all, cursorState.all)
			restoreReadyViewCursors(&bucket.ws, cursorState.ws)
		}
		m.readyByPriority[priority] = bucket
		m.priorityOrder = append(m.priorityOrder, priority)
	}
	sort.Slice(m.priorityOrder, func(i, j int) bool {
		return m.priorityOrder[i] > m.priorityOrder[j]
	})
	sort.Slice(m.blocked, func(i, j int) bool {
		left := m.blocked[i]
		right := m.blocked[j]
		if left == nil || right == nil {
			return left != nil
		}
		if left.nextRetryAt.Equal(right.nextRetryAt) {
			return left.auth.ID < right.auth.ID
		}
		if left.nextRetryAt.IsZero() {
			return false
		}
		if right.nextRetryAt.IsZero() {
			return true
		}
		return left.nextRetryAt.Before(right.nextRetryAt)
	})
}

// buildReadyBucket prepares the general and websocket-only ready views for one priority bucket.
func buildReadyBucket(entries []*scheduledAuth) *readyBucket {
	bucket := &readyBucket{}
	bucket.all = buildReadyView(entries)
	bucket.pickFacts = make([]readyPickFacts, len(entries))
	wsEntries := make([]*scheduledAuth, 0, len(entries))
	for i, entry := range entries {
		retry := -1
		if entry.hasRequestRetryOverride {
			retry = entry.requestRetryOverride
		}
		bucket.pickFacts[i] = readyPickFacts{retry: retry, alphaSearch: entry.alphaSearchAllowed}
		bucket.retry.add(entry)
		if entry.alphaSearchAllowed {
			bucket.alphaSearch.add(entry)
		}
		if entry.meta.websocketEnabled {
			wsEntries = append(wsEntries, entry)
		}
	}
	sort.Ints(bucket.retry.overrides)
	sort.Ints(bucket.alphaSearch.overrides)
	bucket.ws = buildReadyView(wsEntries)
	return bucket
}

// buildReadyView creates a flat view for rotation.
func buildReadyView(entries []*scheduledAuth) readyView {
	// Validate input data before converting it into runtime state.
	return readyView{flat: append([]*scheduledAuth(nil), entries...)}
}

// pickFirst returns the first ready entry that satisfies predicate without advancing cursors.
func (v *readyView) pickFirst(predicate func(*scheduledAuth) bool) *scheduledAuth {
	for _, entry := range v.flat {
		if predicate == nil || predicate(entry) {
			return entry
		}
	}
	return nil
}

// pickRoundRobin returns the next ready entry using round-robin traversal.
func (v *readyView) pickRoundRobin(predicate func(*scheduledAuth) bool) *scheduledAuth {
	if len(v.flat) == 0 {
		return nil
	}
	start := 0
	if len(v.flat) > 0 {
		start = v.cursor % len(v.flat)
	}
	for offset := 0; offset < len(v.flat); offset++ {
		index := (start + offset) % len(v.flat)
		entry := v.flat[index]
		if predicate != nil && !predicate(entry) {
			continue
		}
		v.cursor = index + 1
		return entry
	}
	return nil
}
