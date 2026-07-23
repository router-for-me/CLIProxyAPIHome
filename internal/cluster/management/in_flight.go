package management

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const (
	defaultInFlightDetailsLimit = 100
	maxInFlightDetailsLimit     = 1000
)

type inFlightDetailsQuery struct {
	CredentialID string
	Model        string
	Limit        int
	Offset       int
}

func (h *Handler) inFlightStaleAfter() time.Duration {
	if h == nil || h.runtime == nil {
		return 10 * time.Second
	}
	cfg := h.runtime.Config()
	if cfg == nil {
		return 10 * time.Second
	}
	_, staleAfter, _, errDurations := cfg.CredentialInFlight.Durations()
	if errDurations != nil {
		return 10 * time.Second
	}
	return staleAfter
}

func (h *Handler) GetInFlightDetails(c *gin.Context) {
	query, errQuery := parseInFlightDetailsQuery(c)
	if errQuery != nil {
		respondError(c, http.StatusBadRequest, "invalid_in_flight_pagination", errQuery)
		return
	}
	read, ok := h.readInFlightObservation(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	states, _ := h.repo.ReadConcurrencyState(ctx)
	c.JSON(http.StatusOK, inFlightDetailsResponseWithConcurrency(read, query, states))
}

func (h *Handler) GetInFlightSummary(c *gin.Context) {
	read, ok := h.readInFlightObservation(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	states, _ := h.repo.ReadConcurrencyState(ctx)
	c.JSON(http.StatusOK, inFlightSummaryResponseWithConcurrency(read, states))
}

func (h *Handler) readInFlightObservation(c *gin.Context) (cluster.InFlightObservationReadModel, bool) {
	if h == nil || h.repo == nil {
		respondError(c, http.StatusInternalServerError, "in_flight_observation_load_failed", fmt.Errorf("in-flight observation repository is unavailable"))
		return cluster.InFlightObservationReadModel{}, false
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	read, errRead := h.repo.ReadInFlightObservation(ctx, h.inFlightStaleAfter())
	if errRead != nil {
		respondError(c, http.StatusInternalServerError, "in_flight_observation_load_failed", errRead)
		return cluster.InFlightObservationReadModel{}, false
	}
	return read, true
}

func parseInFlightDetailsQuery(c *gin.Context) (inFlightDetailsQuery, error) {
	query := inFlightDetailsQuery{Limit: defaultInFlightDetailsLimit}
	if c == nil {
		return query, nil
	}
	query.CredentialID = strings.TrimSpace(c.Query("credential_id"))
	query.Model = strings.TrimSpace(c.Query("model"))

	limit, errLimit := parseInFlightPaginationValue(c.Query("limit"), defaultInFlightDetailsLimit, 1, maxInFlightDetailsLimit, "limit")
	if errLimit != nil {
		return inFlightDetailsQuery{}, errLimit
	}
	offset, errOffset := parseInFlightPaginationValue(c.Query("offset"), 0, 0, 0, "offset")
	if errOffset != nil {
		return inFlightDetailsQuery{}, errOffset
	}
	query.Limit = limit
	query.Offset = offset
	return query, nil
}

func parseInFlightPaginationValue(raw string, defaultValue int, minValue int, maxValue int, field string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return defaultValue, nil
	}
	parsed, errParse := strconv.Atoi(value)
	if errParse != nil || parsed < minValue || (maxValue > 0 && parsed > maxValue) {
		if maxValue > 0 {
			return 0, fmt.Errorf("%s must be between %d and %d", field, minValue, maxValue)
		}
		return 0, fmt.Errorf("%s must be at least %d", field, minValue)
	}
	return parsed, nil
}

func inFlightDetailsResponse(read cluster.InFlightObservationReadModel, query inFlightDetailsQuery) gin.H {
	return inFlightDetailsResponseWithConcurrency(read, query, nil)
}

func inFlightDetailsResponseWithConcurrency(read cluster.InFlightObservationReadModel, query inFlightDetailsQuery, states []cluster.CredentialConcurrencyState) gin.H {
	statesByCredential := make(map[string]cluster.CredentialConcurrencyState, len(states))
	for index := range states {
		state := states[index]
		statesByCredential[state.CredentialID] = state
	}
	observed := observedCredentialsByID(read)
	items := make([]gin.H, 0, minInFlightDetailsCapacity(len(read.Details), query.Limit))
	skipped := 0
	for index := range read.Details {
		detail := read.Details[index]
		if query.CredentialID != "" && detail.CredentialID != query.CredentialID {
			continue
		}
		if query.Model != "" && detail.Model != query.Model {
			continue
		}
		if skipped < query.Offset {
			skipped++
			continue
		}
		if len(items) >= query.Limit {
			break
		}
		entry := gin.H{
			"request_id":    detail.RequestID,
			"credential_id": detail.CredentialID,
			"model":         detail.Model,
			"request_kind":  detail.RequestKind,
			"started_at":    detail.StartedAt.UTC().Format(time.RFC3339Nano),
			"limiter":       nil,
		}
		if state := statesByCredential[detail.CredentialID]; state.CredentialID != "" {
			item, ok := observed[detail.CredentialID]
			entry["limiter"] = credentialConcurrencyStateResponse(state, &read, item, ok)
		}
		items = append(items, entry)
	}
	return inFlightObservationResponse(read, items)
}

func minInFlightDetailsCapacity(detailsCount int, limit int) int {
	if detailsCount < limit {
		return detailsCount
	}
	return limit
}

func inFlightSummaryResponse(read cluster.InFlightObservationReadModel) gin.H {
	return inFlightSummaryResponseWithConcurrency(read, nil)
}

func inFlightSummaryResponseWithConcurrency(read cluster.InFlightObservationReadModel, states []cluster.CredentialConcurrencyState) gin.H {
	statesByCredential := make(map[string]cluster.CredentialConcurrencyState, len(states))
	for index := range states {
		state := states[index]
		statesByCredential[state.CredentialID] = state
	}
	items := make([]gin.H, 0, len(read.Credentials))
	for index := range read.Credentials {
		item := read.Credentials[index]
		models := inFlightSummaryModels(item, statesByCredential[item.CredentialID])
		entry := gin.H{
			"credential_id":         item.CredentialID,
			"in_flight":             item.ObservedInFlight,
			"max_in_flight":         nil,
			"remaining":             nil,
			"total_saturated":       false,
			"saturated_model_count": 0,
			"models":                models,
			"observed":              inFlightObservedResponse(read, item),
			"limiter":               nil,
		}
		applyConcurrencyStateToSummary(entry, statesByCredential[item.CredentialID], read, item)
		items = append(items, entry)
	}
	return inFlightObservationResponse(read, items)
}

func inFlightSummaryModels(item cluster.InFlightObservedCredentialItem, state cluster.CredentialConcurrencyState) []gin.H {
	stateModels := make(map[string]cluster.CredentialConcurrencyModelState, len(state.Models))
	for index := range state.Models {
		model := state.Models[index]
		stateModels[model.Model] = model
	}
	models := make([]gin.H, 0, len(item.Models))
	for modelIndex := range item.Models {
		model := item.Models[modelIndex]
		entry := gin.H{
			"model":         model.Model,
			"in_flight":     model.ObservedInFlight,
			"max_in_flight": nil,
			"remaining":     nil,
			"saturated":     false,
		}
		if limiter, ok := stateModels[model.Model]; ok {
			entry["max_in_flight"] = limiter.MaxInFlight
			entry["remaining"] = remainingConcurrencyLimit(limiter.MaxInFlight, limiter.AdmittedInFlight)
			entry["saturated"] = limiter.AdmittedInFlight >= limiter.MaxInFlight
		}
		models = append(models, entry)
	}
	return models
}

func applyConcurrencyStateToSummary(entry gin.H, state cluster.CredentialConcurrencyState, observation cluster.InFlightObservationReadModel, observed cluster.InFlightObservedCredentialItem) {
	if entry == nil || state.CredentialID == "" {
		return
	}
	response := credentialConcurrencyStateResponse(state, &observation, observed, true)
	entry["max_in_flight"] = response.MaxInFlight
	entry["admitted_in_flight"] = response.AdmittedInFlight
	entry["remaining"] = response.Remaining
	entry["total_saturated"] = response.TotalSaturated
	saturatedModels := 0
	for index := range response.Models {
		if response.Models[index].Saturated {
			saturatedModels++
		}
	}
	entry["saturated_model_count"] = saturatedModels
	entry["limiter"] = response
}

func inFlightObservationResponse(read cluster.InFlightObservationReadModel, items []gin.H) gin.H {
	return gin.H{
		"observed_at":                        inFlightObservedAt(read),
		"stale":                              read.Stale,
		"coverage_complete":                  read.CoverageComplete,
		"aggregates_complete":                read.AggregatesComplete,
		"protocol_coverage_complete":         read.ProtocolCoverageComplete,
		"minimum_processed_barrier_revision": inFlightMinimumProcessedBarrierRevision(read),
		"details_truncated":                  read.DetailsTruncated,
		"items":                              items,
	}
}

func inFlightMinimumProcessedBarrierRevision(read cluster.InFlightObservationReadModel) any {
	if read.MinimumProcessedBarrierRevision == nil {
		return nil
	}
	return *read.MinimumProcessedBarrierRevision
}

func inFlightObservedResponse(read cluster.InFlightObservationReadModel, item cluster.InFlightObservedCredentialItem) gin.H {
	return gin.H{
		"in_flight":           item.ObservedInFlight,
		"accounted":           item.ObservedAccounted,
		"unaccounted":         item.ObservedUnaccounted,
		"stale":               read.Stale,
		"coverage_complete":   read.CoverageComplete,
		"aggregates_complete": read.AggregatesComplete,
		"details_truncated":   read.DetailsTruncated,
	}
}

func applyInFlightObservationToAuthFile(entry gin.H, read cluster.InFlightObservationReadModel, item cluster.InFlightObservedCredentialItem) {
	if entry == nil {
		return
	}
	entry["in_flight"] = item.ObservedInFlight
	entry["max_in_flight"] = nil
	entry["max_in_flight_by_model"] = nil
	entry["remaining"] = nil
	entry["total_saturated"] = false
	entry["saturated_model_count"] = 0
	entry["admitted_in_flight"] = nil
	entry["observed"] = inFlightObservedResponse(read, item)
	entry["limiter"] = nil
}

func applyNoInFlightObservationToAuthFile(entry gin.H) {
	if entry == nil {
		return
	}
	entry["in_flight"] = nil
	entry["max_in_flight"] = nil
	entry["max_in_flight_by_model"] = nil
	entry["remaining"] = nil
	entry["total_saturated"] = false
	entry["saturated_model_count"] = 0
	entry["admitted_in_flight"] = nil
	entry["observed"] = nil
	entry["limiter"] = nil
}

func inFlightObservedAt(read cluster.InFlightObservationReadModel) any {
	if read.ObservedAt == nil {
		return nil
	}
	return read.ObservedAt.UTC().Format(time.RFC3339Nano)
}
