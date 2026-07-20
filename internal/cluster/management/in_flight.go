package management

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gorm.io/gorm"
)

func (h *Handler) GetCredentialInFlightSummary(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	summaries, observedAt, errList := h.repo.ListInFlightCredentialSummaries(ctx)
	if errList != nil {
		respondError(c, http.StatusInternalServerError, "in_flight_load_failed", errList)
		return
	}
	items := make([]gin.H, 0, len(summaries))
	for i := range summaries {
		items = append(items, inFlightSummaryResponse(summaries[i], true))
	}
	c.JSON(http.StatusOK, gin.H{
		"observed_at": observedAt.UTC(),
		"items":       items,
	})
}

func (h *Handler) ListCredentialInFlight(c *gin.Context) {
	cursor, limit, ok := parseInFlightPagination(c)
	if !ok {
		return
	}
	credentialID := strings.TrimSpace(c.Query("credential_id"))

	ctx, cancel := h.requestContext(c)
	defer cancel()
	page, errList := h.repo.ListInFlightLeases(ctx, credentialID, cursor, limit)
	if errList != nil {
		respondError(c, http.StatusInternalServerError, "in_flight_load_failed", errList)
		return
	}
	items := make([]gin.H, 0, len(page.Requests))
	for i := range page.Requests {
		items = append(items, inFlightLeaseResponse(page.Requests[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"observed_at": page.ObservedAt.UTC(),
		"items":       items,
		"next_cursor": emptyStringAsNil(page.NextCursor),
	})
}

func (h *Handler) GetCredentialInFlight(c *gin.Context) {
	credentialID := strings.TrimSpace(c.Param("credential_id"))
	if credentialID == "" {
		respondError(c, http.StatusBadRequest, "missing_credential_id", nil)
		return
	}
	cursor, limit, ok := parseInFlightPagination(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	detail, errDetail := h.repo.GetInFlightCredentialDetail(ctx, credentialID, cursor, limit)
	if errDetail != nil {
		if errors.Is(errDetail, gorm.ErrRecordNotFound) {
			respondError(c, http.StatusNotFound, "not_found", nil)
			return
		}
		respondError(c, http.StatusInternalServerError, "in_flight_load_failed", errDetail)
		return
	}
	requests := make([]gin.H, 0, len(detail.Requests))
	for i := range detail.Requests {
		requests = append(requests, inFlightLeaseResponse(detail.Requests[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"observed_at": detail.ObservedAt.UTC(),
		"credential":  inFlightSummaryResponse(detail.Summary, true),
		"requests": gin.H{
			"items":       requests,
			"next_cursor": emptyStringAsNil(detail.NextCursor),
		},
	})
}

func parseInFlightPagination(c *gin.Context) (uint, int, bool) {
	limit := 50
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		parsed, errParse := strconv.Atoi(rawLimit)
		if errParse != nil || parsed <= 0 || parsed > 200 {
			respondError(c, http.StatusBadRequest, "invalid_limit", nil)
			return 0, 0, false
		}
		limit = parsed
	}
	var cursor uint
	if rawCursor := strings.TrimSpace(c.Query("cursor")); rawCursor != "" {
		parsed, errParse := strconv.ParseUint(rawCursor, 10, 64)
		if errParse != nil {
			respondError(c, http.StatusBadRequest, "invalid_cursor", nil)
			return 0, 0, false
		}
		cursor = uint(parsed)
	}
	return cursor, limit, true
}

func inFlightSummaryResponse(summary home.InFlightCredentialSummary, includeModels bool) gin.H {
	response := gin.H{
		"credential_id":         summary.CredentialID,
		"in_flight":             summary.InFlight,
		"max_in_flight":         summary.MaxInFlight,
		"remaining":             summary.Remaining,
		"total_saturated":       summary.TotalSaturated,
		"saturated_model_count": summary.SaturatedModelCount,
	}
	if includeModels {
		models := make([]gin.H, 0, len(summary.Models))
		for _, model := range summary.Models {
			models = append(models, gin.H{
				"model":         model.Model,
				"in_flight":     model.InFlight,
				"max_in_flight": model.MaxInFlight,
				"remaining":     model.Remaining,
				"saturated":     model.Saturated,
			})
		}
		response["models"] = models
	}
	return response
}

func inFlightLeaseResponse(lease home.InFlightLease) gin.H {
	return gin.H{
		"lease_id":        lease.LeaseID,
		"request_id":      lease.RequestID,
		"credential_id":   lease.CredentialID,
		"provider":        lease.Provider,
		"requested_model": lease.RequestedModel,
		"model":           lease.Model,
		"cpa_node_id":     emptyStringAsNil(lease.CPANodeID),
		"cpa_ip":          emptyStringAsNil(lease.CPAIP),
		"cpa_label":       emptyStringAsNil(lease.CPALabel),
		"started_at":      lease.StartedAt.UTC(),
		"last_renewed_at": lease.LastRenewedAt.UTC(),
		"expires_at":      lease.ExpiresAt.UTC(),
	}
}
