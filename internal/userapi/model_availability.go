package userapi

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"golang.org/x/sync/singleflight"
)

const (
	// modelAvailabilityWindow is how far back an observation still counts. A
	// week is long enough for a small cluster to accumulate a readable sample
	// and short enough that a model which broke yesterday does not hide behind
	// last month's successes.
	modelAvailabilityWindow = 7 * 24 * time.Hour
	// modelAvailabilityMinSamples is the point below which a rate is noise
	// rather than a measurement. Two successful calls are not 100% uptime, and
	// publishing them as such would be the most misleading thing this endpoint
	// could do.
	modelAvailabilityMinSamples = 10
	// modelAvailabilityCacheTTL bounds how often the usage table is grouped by
	// model. The table has no model-only index, so this scan is the expensive
	// part of the response; the answer is cluster-wide rather than per-user, so
	// one computation serves everyone asking within the window.
	modelAvailabilityCacheTTL = 60 * time.Second
	// modelAvailabilityQueryTimeout bounds the shared scan. It is longer than
	// the per-request deadline on purpose: a waiter that gives up first still
	// leaves the scan running, so the answer it could not wait for is in the
	// cache by the time the next request asks.
	modelAvailabilityQueryTimeout = 30 * time.Second
)

// Availability reporting states.
const (
	availabilityStatusObserved         = "observed"
	availabilityStatusInsufficientData = "insufficient_data"
)

// modelAvailabilityCache memoizes the cluster-wide availability summary.
//
// The cached value is keyed only by the window because the underlying question
// — how has this model behaved lately — has the same answer for every caller.
// It holds no per-user data, so sharing it across sessions leaks nothing.
var modelAvailabilityCache = struct {
	mu        sync.Mutex
	summaries map[string]cluster.ModelAvailabilitySummary
	covered   map[string]struct{}
	from      time.Time
	to        time.Time
	expiresAt time.Time
}{}

// modelAvailabilityFlight collapses concurrent recomputations into one.
//
// The cache alone does not do this. It is read before the scan and written
// after it, so at the moment it expires every request in flight sees a miss and
// starts its own copy of the same week-long scan — the load spike lands exactly
// when the cache was supposed to be absorbing it.
var modelAvailabilityFlight singleflight.Group

// modelAvailabilityFlightKey is constant because every flight asks the same
// question. The scan is widened to the whole servable catalog precisely so that
// one answer serves every caller, which leaves nothing to key on.
const modelAvailabilityFlightKey = "catalog"

// modelAvailabilityWindowBounds is the summary a handler renders against.
type modelAvailabilityWindowBounds struct {
	From time.Time
	To   time.Time
}

// modelAvailabilityResult is one computed answer: the summaries, the models the
// query actually asked about, and the window it asked over. The three travel
// together because reading any one of them against another computation's
// results would misreport what was measured.
type modelAvailabilityResult struct {
	summaries map[string]cluster.ModelAvailabilitySummary
	covered   map[string]struct{}
	bounds    modelAvailabilityWindowBounds
}

// modelAvailabilityIndex returns the availability summary for the models being
// listed, recomputing at most once per cache window.
//
// Coverage is tracked alongside the summary rather than assumed. A model absent
// from the map is reported as having no observations, so answering for a model
// the cached query never asked about would invent a silence that was never
// measured; when that happens the summary is recomputed instead.
func (h *Handler) modelAvailabilityIndex(ctx context.Context, modelIDs []string) (map[string]cluster.ModelAvailabilitySummary, modelAvailabilityWindowBounds, error) {
	if cached, ok := cachedModelAvailability(time.Now().UTC(), modelIDs); ok {
		return cached.summaries, cached.bounds, nil
	}

	shared := modelAvailabilityFlight.DoChan(modelAvailabilityFlightKey, func() (any, error) {
		// Whoever queued behind the leader has already waited out a scan. Read
		// the cache once more before starting another one: the answer they were
		// waiting for may have been stored microseconds ago.
		if cached, ok := cachedModelAvailability(time.Now().UTC(), modelIDs); ok {
			return cached, nil
		}
		// The scan outlives the request that happened to start it. A buyer who
		// navigates away must not cancel a computation every other waiter is
		// blocked on, and the result is worth finishing even for an empty room
		// because the next request will read it from the cache.
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelAvailabilityQueryTimeout)
		defer cancel()
		result, errRefresh := h.refreshModelAvailability(queryCtx, modelIDs)
		if errRefresh != nil {
			return nil, errRefresh
		}
		return result, nil
	})

	var result modelAvailabilityResult
	select {
	case <-ctx.Done():
		// Waiting inside the flight is not covered by this caller's deadline,
		// so it is enforced here. The scan continues without them.
		return nil, modelAvailabilityWindowBounds{}, ctx.Err()
	case outcome := <-shared:
		if outcome.Err != nil {
			return nil, modelAvailabilityWindowBounds{}, outcome.Err
		}
		result, _ = outcome.Val.(modelAvailabilityResult)
	}

	if modelAvailabilityCacheCovers(result.covered, modelIDs) {
		return result.summaries, result.bounds, nil
	}

	// The shared answer was computed for a catalog registered a moment before
	// this caller read its own, and is missing a model this caller is about to
	// render. Borrowing it would report a model nobody asked about as one
	// nobody observed, so this caller pays for its own scan instead.
	own, errOwn := h.refreshModelAvailability(ctx, modelIDs)
	if errOwn != nil {
		return nil, modelAvailabilityWindowBounds{}, errOwn
	}
	return own.summaries, own.bounds, nil
}

// refreshModelAvailability recomputes the summary and memoizes it.
func (h *Handler) refreshModelAvailability(ctx context.Context, modelIDs []string) (modelAvailabilityResult, error) {
	now := time.Now().UTC()
	bounds := modelAvailabilityWindowBounds{From: now.Add(-modelAvailabilityWindow), To: now}

	// The query covers the whole servable catalog rather than this caller's
	// slice of it. The answer is the same for everyone, and widening it here is
	// what lets one computation serve every buyer within the cache window.
	wanted := modelAvailabilityCatalogIDs(modelIDs)
	summaries, errSummaries := h.repo.ListModelAvailability(ctx, cluster.ModelAvailabilityQuery{
		ModelIDs:    wanted,
		From:        bounds.From,
		ToExclusive: bounds.To,
	})
	if errSummaries != nil {
		return modelAvailabilityResult{}, errSummaries
	}

	covered := make(map[string]struct{}, len(wanted))
	for _, modelID := range wanted {
		covered[strings.ToLower(strings.TrimSpace(modelID))] = struct{}{}
	}

	modelAvailabilityCache.mu.Lock()
	modelAvailabilityCache.summaries = summaries
	modelAvailabilityCache.covered = covered
	modelAvailabilityCache.from = bounds.From
	modelAvailabilityCache.to = bounds.To
	modelAvailabilityCache.expiresAt = now.Add(modelAvailabilityCacheTTL)
	modelAvailabilityCache.mu.Unlock()

	return modelAvailabilityResult{summaries: summaries, covered: covered, bounds: bounds}, nil
}

// cachedModelAvailability returns the memoized summary when it is both fresh and
// known to have asked about every model the caller is going to render.
func cachedModelAvailability(now time.Time, modelIDs []string) (modelAvailabilityResult, bool) {
	modelAvailabilityCache.mu.Lock()
	defer modelAvailabilityCache.mu.Unlock()

	if modelAvailabilityCache.summaries == nil || !now.Before(modelAvailabilityCache.expiresAt) {
		return modelAvailabilityResult{}, false
	}
	if !modelAvailabilityCacheCovers(modelAvailabilityCache.covered, modelIDs) {
		return modelAvailabilityResult{}, false
	}
	return modelAvailabilityResult{
		summaries: modelAvailabilityCache.summaries,
		covered:   modelAvailabilityCache.covered,
		bounds:    modelAvailabilityWindowBounds{From: modelAvailabilityCache.from, To: modelAvailabilityCache.to},
	}, true
}

// modelAvailabilityCatalogIDs widens the requested models to the full servable
// catalog, so a cache filled by a narrowly-scoped buyer still answers for the
// next one.
func modelAvailabilityCatalogIDs(modelIDs []string) []string {
	seen := make(map[string]struct{}, len(modelIDs))
	wanted := make([]string, 0, len(modelIDs))
	appendID := func(modelID string) {
		trimmed := strings.TrimSpace(modelID)
		if trimmed == "" {
			return
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		wanted = append(wanted, trimmed)
	}
	for _, model := range availableUserModels() {
		appendID(model.ID)
	}
	for _, modelID := range modelIDs {
		appendID(modelID)
	}
	return wanted
}

// modelAvailabilityCacheCovers reports whether every requested model was part of
// the query that produced the cached summary.
func modelAvailabilityCacheCovers(covered map[string]struct{}, modelIDs []string) bool {
	for _, modelID := range modelIDs {
		key := strings.ToLower(strings.TrimSpace(modelID))
		if key == "" {
			continue
		}
		if _, ok := covered[key]; !ok {
			return false
		}
	}
	return true
}

// resetModelAvailabilityCache clears the memoized summary. Tests that seed usage
// records need each case to see its own data rather than the previous case's.
func resetModelAvailabilityCache() {
	modelAvailabilityCache.mu.Lock()
	modelAvailabilityCache.summaries = nil
	modelAvailabilityCache.covered = nil
	modelAvailabilityCache.expiresAt = time.Time{}
	modelAvailabilityCache.mu.Unlock()
}

// modelAvailabilityResponse renders one model's observed behaviour.
//
// The window is always reported, even when nothing was observed in it, because
// "no failures" and "no observations" look identical without it. A sample below
// the threshold reports its size and its window and nothing else: no rate, no
// latency, no throughput, because none of those numbers mean anything yet.
func modelAvailabilityResponse(summary cluster.ModelAvailabilitySummary, found bool, bounds modelAvailabilityWindowBounds) gin.H {
	window := gin.H{
		"from":        bounds.From.UTC().Format(time.RFC3339),
		"to":          bounds.To.UTC().Format(time.RFC3339),
		"hours":       int(modelAvailabilityWindow / time.Hour),
		"min_samples": modelAvailabilityMinSamples,
	}
	payload := gin.H{
		"window":       window,
		"sample_count": summary.SampleCount,
	}
	if !found || summary.SampleCount < modelAvailabilityMinSamples {
		payload["status"] = availabilityStatusInsufficientData
		if !found {
			payload["sample_count"] = int64(0)
			return payload
		}
		// Even an undersized sample has a most recent observation, and knowing
		// the model answered an hour ago is useful on its own.
		if summary.LastObservedAt != nil {
			payload["last_observed_at"] = summary.LastObservedAt.UTC().Format(time.RFC3339)
		}
		return payload
	}

	payload["status"] = availabilityStatusObserved
	payload["success_count"] = summary.SuccessCount
	payload["failed_count"] = summary.FailedCount
	payload["availability_rate"] = roundTo(float64(summary.SuccessCount)/float64(summary.SampleCount), 4)
	if summary.FirstObservedAt != nil {
		payload["first_observed_at"] = summary.FirstObservedAt.UTC().Format(time.RFC3339)
	}
	if summary.LastObservedAt != nil {
		payload["last_observed_at"] = summary.LastObservedAt.UTC().Format(time.RFC3339)
	}
	if summary.AvgLatencyMS != nil {
		payload["avg_latency_ms"] = roundTo(*summary.AvgLatencyMS, 1)
	}
	// Time to first token only exists for streamed responses. Its absence next
	// to a present latency is information, not an error.
	if summary.AvgTTFTMS != nil {
		payload["avg_ttft_ms"] = roundTo(*summary.AvgTTFTMS, 1)
	}
	// Throughput is total tokens produced over total time spent producing them,
	// which weights long generations more heavily than short ones — the same
	// way a buyer experiences the model.
	if summary.ThroughputLatencyMS > 0 && summary.ThroughputOutputTokens > 0 {
		payload["output_tokens_per_second"] = roundTo(float64(summary.ThroughputOutputTokens)*1000/float64(summary.ThroughputLatencyMS), 2)
	}
	return payload
}

// availabilityFor looks a model up in the summary index.
func availabilityFor(index map[string]cluster.ModelAvailabilitySummary, modelID string, bounds modelAvailabilityWindowBounds) gin.H {
	summary, found := index[strings.ToLower(strings.TrimSpace(modelID))]
	return modelAvailabilityResponse(summary, found, bounds)
}

// roundTo trims a computed float to the precision the number actually carries,
// so a rate does not arrive as 0.9333333333333333.
func roundTo(value float64, decimals int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	scale := math.Pow(10, float64(decimals))
	return math.Round(value*scale) / scale
}
