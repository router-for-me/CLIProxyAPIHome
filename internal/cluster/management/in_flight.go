package management

import (
	"encoding/json"
	"errors"
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
	inFlightSnapshotCursorTTL   = 2 * time.Minute
)

type inFlightDetailsQuery struct {
	CredentialID   string
	Model          string
	Limit          int
	Offset         int
	StableSnapshot bool
	SnapshotCursor string
}

type inFlightDetailsCursorPayload struct {
	CredentialID string                               `json:"credential_id"`
	Model        string                               `json:"model"`
	Observation  cluster.InFlightObservationReadModel `json:"observation"`
	States       []cluster.CredentialConcurrencyState `json:"states"`
}

type inFlightDetailsCursorPage struct {
	Cursor    string
	ExpiresAt *time.Time
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
	read, states, cursorPage, ok := h.readInFlightDetailsSnapshot(c, query)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, inFlightDetailsPageResponse(read, query, states, cursorPage))
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
	query.SnapshotCursor = strings.TrimSpace(c.Query("snapshot_cursor"))
	stableSnapshot := strings.TrimSpace(c.Query("stable_snapshot"))
	if stableSnapshot != "" {
		parsed, errParse := strconv.ParseBool(stableSnapshot)
		if errParse != nil {
			return inFlightDetailsQuery{}, fmt.Errorf("stable_snapshot must be a boolean")
		}
		query.StableSnapshot = parsed
	}
	if query.SnapshotCursor != "" {
		query.StableSnapshot = true
	}

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

func (h *Handler) readInFlightDetailsSnapshot(c *gin.Context, query inFlightDetailsQuery) (cluster.InFlightObservationReadModel, []cluster.CredentialConcurrencyState, inFlightDetailsCursorPage, bool) {
	if h == nil || h.repo == nil {
		respondError(c, http.StatusInternalServerError, "in_flight_observation_load_failed", fmt.Errorf("in-flight observation repository is unavailable"))
		return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
	}
	if query.SnapshotCursor != "" {
		return h.readInFlightDetailsCursor(c, query)
	}
	read, ok := h.readInFlightObservation(c)
	if !ok {
		return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	states, _ := h.repo.ReadConcurrencyState(ctx)
	read = filterInFlightDetailsSnapshot(read, query)
	states = filterInFlightDetailsStates(states, read)
	cursorPage := inFlightDetailsCursorPage{}
	_, nextOffset := inFlightDetailsPageBounds(len(read.Details), query.Offset, query.Limit)
	if query.StableSnapshot && nextOffset != nil {
		payload, errMarshal := json.Marshal(inFlightDetailsCursorPayload{
			CredentialID: query.CredentialID,
			Model:        query.Model,
			Observation:  read,
			States:       states,
		})
		if errMarshal != nil {
			respondError(c, http.StatusInternalServerError, "in_flight_snapshot_cursor_store_failed", errMarshal)
			return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
		}
		cursor, errCursor := h.repo.CreateInFlightSnapshotCursor(ctx, payload, inFlightSnapshotCursorTTL)
		if errCursor != nil {
			respondError(c, http.StatusInternalServerError, "in_flight_snapshot_cursor_store_failed", errCursor)
			return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
		}
		expiresAt := cursor.ExpiresAt
		cursorPage = inFlightDetailsCursorPage{Cursor: cursor.Cursor, ExpiresAt: &expiresAt}
	}
	return read, states, cursorPage, true
}

func (h *Handler) readInFlightDetailsCursor(c *gin.Context, query inFlightDetailsQuery) (cluster.InFlightObservationReadModel, []cluster.CredentialConcurrencyState, inFlightDetailsCursorPage, bool) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	cursor, errCursor := h.repo.ReadInFlightSnapshotCursor(ctx, query.SnapshotCursor)
	if errors.Is(errCursor, cluster.ErrInFlightSnapshotCursorExpired) {
		respondError(c, http.StatusConflict, "in_flight_snapshot_cursor_expired", errCursor)
		return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
	}
	if errCursor != nil {
		respondError(c, http.StatusInternalServerError, "in_flight_snapshot_cursor_load_failed", errCursor)
		return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
	}
	payload := inFlightDetailsCursorPayload{}
	if errUnmarshal := json.Unmarshal(cursor.Payload, &payload); errUnmarshal != nil {
		respondError(c, http.StatusInternalServerError, "in_flight_snapshot_cursor_load_failed", errUnmarshal)
		return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
	}
	if payload.CredentialID != query.CredentialID || payload.Model != query.Model {
		respondError(c, http.StatusConflict, "in_flight_snapshot_cursor_expired", cluster.ErrInFlightSnapshotCursorExpired)
		return cluster.InFlightObservationReadModel{}, nil, inFlightDetailsCursorPage{}, false
	}
	read := payload.Observation
	if !read.Stale && (read.FreshUntil == nil || cursor.ReadAt.After(*read.FreshUntil)) {
		read.Stale = true
		read.CoverageComplete = false
	}
	expiresAt := cursor.ExpiresAt
	return read, payload.States, inFlightDetailsCursorPage{Cursor: cursor.Cursor, ExpiresAt: &expiresAt}, true
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
	read = filterInFlightDetailsSnapshot(read, query)
	states = filterInFlightDetailsStates(states, read)
	return inFlightDetailsPageResponse(read, query, states, inFlightDetailsCursorPage{})
}

func inFlightDetailsPageResponse(read cluster.InFlightObservationReadModel, query inFlightDetailsQuery, states []cluster.CredentialConcurrencyState, cursorPage inFlightDetailsCursorPage) gin.H {
	statesByCredential := make(map[string]cluster.CredentialConcurrencyState, len(states))
	for index := range states {
		state := states[index]
		statesByCredential[state.CredentialID] = state
	}
	observed := observedCredentialsByID(read)
	start, nextOffset := inFlightDetailsPageBounds(len(read.Details), query.Offset, query.Limit)
	end := len(read.Details)
	if nextOffset != nil {
		end = *nextOffset
	}
	items := make([]gin.H, 0, minInFlightDetailsCapacity(end-start, query.Limit))
	for index := start; index < end; index++ {
		detail := read.Details[index]
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
	response := inFlightObservationResponse(read, items)
	response["total"] = len(read.Details)
	response["next_offset"] = nextOffset
	response["snapshot_cursor"] = nil
	response["snapshot_expires_at"] = nil
	if cursorPage.Cursor != "" && cursorPage.ExpiresAt != nil {
		response["snapshot_cursor"] = cursorPage.Cursor
		response["snapshot_expires_at"] = cursorPage.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return response
}

func inFlightDetailsPageBounds(total int, offset int, limit int) (int, *int) {
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end < start || end >= total {
		return start, nil
	}
	next := end
	return start, &next
}

func filterInFlightDetailsSnapshot(read cluster.InFlightObservationReadModel, query inFlightDetailsQuery) cluster.InFlightObservationReadModel {
	filtered := read
	filtered.Details = make([]cluster.InFlightRequestDetail, 0, len(read.Details))
	credentialIDs := make(map[string]struct{})
	for index := range read.Details {
		detail := read.Details[index]
		if query.CredentialID != "" && detail.CredentialID != query.CredentialID {
			continue
		}
		if query.Model != "" && detail.Model != query.Model {
			continue
		}
		filtered.Details = append(filtered.Details, detail)
		credentialIDs[detail.CredentialID] = struct{}{}
	}
	filtered.Credentials = make([]cluster.InFlightObservedCredentialItem, 0, len(credentialIDs))
	for index := range read.Credentials {
		item := read.Credentials[index]
		if _, exists := credentialIDs[item.CredentialID]; exists {
			filtered.Credentials = append(filtered.Credentials, item)
		}
	}
	return filtered
}

func filterInFlightDetailsStates(states []cluster.CredentialConcurrencyState, read cluster.InFlightObservationReadModel) []cluster.CredentialConcurrencyState {
	credentialIDs := make(map[string]struct{}, len(read.Details))
	for index := range read.Details {
		credentialIDs[read.Details[index].CredentialID] = struct{}{}
	}
	filtered := make([]cluster.CredentialConcurrencyState, 0, len(credentialIDs))
	for index := range states {
		if _, exists := credentialIDs[states[index].CredentialID]; exists {
			filtered = append(filtered, states[index])
		}
	}
	return filtered
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
