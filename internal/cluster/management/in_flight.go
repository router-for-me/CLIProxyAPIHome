package management

import (
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

type inFlightDetailsCursorPage struct {
	Cursor    string
	ReadAt    *time.Time
	ExpiresAt *time.Time
}

type inFlightDetailsSnapshot struct {
	Read       cluster.InFlightObservationReadModel
	States     []cluster.CredentialConcurrencyState
	Total      int
	NextOffset *int
	CursorPage inFlightDetailsCursorPage
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
	snapshot, ok := h.readInFlightDetailsSnapshot(c, query)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, inFlightDetailsPageResponse(snapshot))
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

func (h *Handler) readInFlightDetailsSnapshot(c *gin.Context, query inFlightDetailsQuery) (inFlightDetailsSnapshot, bool) {
	if h == nil || h.repo == nil {
		respondError(c, http.StatusInternalServerError, "in_flight_observation_load_failed", fmt.Errorf("in-flight observation repository is unavailable"))
		return inFlightDetailsSnapshot{}, false
	}
	if query.SnapshotCursor != "" {
		return h.readInFlightDetailsCursor(c, query)
	}
	read, ok := h.readInFlightObservation(c)
	if !ok {
		return inFlightDetailsSnapshot{}, false
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	states, _ := h.repo.ReadConcurrencyState(ctx)
	read = filterInFlightDetailsSnapshot(read, query)
	states = filterInFlightDetailsStates(states, read)
	total := len(read.Details)
	start, nextOffset := inFlightDetailsPageBounds(total, query.Offset, query.Limit)
	cursorPage := inFlightDetailsCursorPage{}
	if query.StableSnapshot && nextOffset != nil {
		cursor, errCursor := h.repo.CreateInFlightSnapshotCursor(ctx, cluster.InFlightSnapshotCursorInput{
			CredentialID: query.CredentialID,
			Model:        query.Model,
			Observation:  read,
			States:       states,
		}, inFlightSnapshotCursorTTL)
		if errCursor != nil {
			respondError(c, http.StatusInternalServerError, "in_flight_snapshot_cursor_store_failed", errCursor)
			return inFlightDetailsSnapshot{}, false
		}
		expiresAt := cursor.ExpiresAt
		readAt := cursor.ReadAt
		cursorPage = inFlightDetailsCursorPage{Cursor: cursor.Cursor, ReadAt: &readAt, ExpiresAt: &expiresAt}
		read = updateInFlightDetailsFreshness(read, cursor.ReadAt)
	}
	read = sliceInFlightDetailsSnapshot(read, start, nextOffset)
	states = filterInFlightDetailsStates(states, read)
	return inFlightDetailsSnapshot{
		Read:       read,
		States:     states,
		Total:      total,
		NextOffset: nextOffset,
		CursorPage: cursorPage,
	}, true
}

func (h *Handler) readInFlightDetailsCursor(c *gin.Context, query inFlightDetailsQuery) (inFlightDetailsSnapshot, bool) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	cursor, errCursor := h.repo.ReadInFlightSnapshotCursorPage(ctx, query.SnapshotCursor, query.CredentialID, query.Model, query.Offset, query.Limit)
	if errors.Is(errCursor, cluster.ErrInFlightSnapshotCursorExpired) {
		respondError(c, http.StatusConflict, "in_flight_snapshot_cursor_expired", errCursor)
		return inFlightDetailsSnapshot{}, false
	}
	if errCursor != nil {
		respondError(c, http.StatusInternalServerError, "in_flight_snapshot_cursor_load_failed", errCursor)
		return inFlightDetailsSnapshot{}, false
	}
	read := updateInFlightDetailsFreshness(cursor.Observation, cursor.ReadAt)
	_, nextOffset := inFlightDetailsPageBounds(cursor.Total, query.Offset, query.Limit)
	expiresAt := cursor.ExpiresAt
	readAt := cursor.ReadAt
	return inFlightDetailsSnapshot{
		Read:       read,
		States:     cursor.States,
		Total:      cursor.Total,
		NextOffset: nextOffset,
		CursorPage: inFlightDetailsCursorPage{Cursor: cursor.Cursor, ReadAt: &readAt, ExpiresAt: &expiresAt},
	}, true
}

func updateInFlightDetailsFreshness(read cluster.InFlightObservationReadModel, readAt time.Time) cluster.InFlightObservationReadModel {
	if !read.Stale && (read.FreshUntil == nil || readAt.After(*read.FreshUntil)) {
		read.Stale = true
		read.CoverageComplete = false
	}
	return read
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
	total := len(read.Details)
	start, nextOffset := inFlightDetailsPageBounds(total, query.Offset, query.Limit)
	read = sliceInFlightDetailsSnapshot(read, start, nextOffset)
	states = filterInFlightDetailsStates(states, read)
	return inFlightDetailsPageResponse(inFlightDetailsSnapshot{
		Read:       read,
		States:     states,
		Total:      total,
		NextOffset: nextOffset,
	})
}

func inFlightDetailsPageResponse(snapshot inFlightDetailsSnapshot) gin.H {
	statesByCredential := make(map[string]cluster.CredentialConcurrencyState, len(snapshot.States))
	for index := range snapshot.States {
		state := snapshot.States[index]
		statesByCredential[state.CredentialID] = state
	}
	observed := observedCredentialsByID(snapshot.Read)
	items := make([]gin.H, 0, len(snapshot.Read.Details))
	for index := range snapshot.Read.Details {
		detail := snapshot.Read.Details[index]
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
			entry["limiter"] = credentialConcurrencyStateResponse(state, &snapshot.Read, item, ok)
		}
		items = append(items, entry)
	}
	response := inFlightObservationResponse(snapshot.Read, items)
	response["total"] = snapshot.Total
	response["next_offset"] = snapshot.NextOffset
	response["snapshot_cursor"] = nil
	response["snapshot_cursor_read_at"] = nil
	response["snapshot_expires_at"] = nil
	if snapshot.CursorPage.Cursor != "" && snapshot.CursorPage.ReadAt != nil && snapshot.CursorPage.ExpiresAt != nil {
		response["snapshot_cursor"] = snapshot.CursorPage.Cursor
		response["snapshot_cursor_read_at"] = snapshot.CursorPage.ReadAt.UTC().Format(time.RFC3339Nano)
		response["snapshot_expires_at"] = snapshot.CursorPage.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return response
}

func sliceInFlightDetailsSnapshot(read cluster.InFlightObservationReadModel, start int, nextOffset *int) cluster.InFlightObservationReadModel {
	end := len(read.Details)
	if nextOffset != nil {
		end = *nextOffset
	}
	if start > end {
		start = end
	}
	page := read
	page.Details = append([]cluster.InFlightRequestDetail(nil), read.Details[start:end]...)
	credentialIDs := make(map[string]struct{}, len(page.Details))
	for index := range page.Details {
		credentialIDs[page.Details[index].CredentialID] = struct{}{}
	}
	page.Credentials = make([]cluster.InFlightObservedCredentialItem, 0, len(credentialIDs))
	for index := range read.Credentials {
		item := read.Credentials[index]
		if _, exists := credentialIDs[item.CredentialID]; exists {
			page.Credentials = append(page.Credentials, item)
		}
	}
	return page
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
