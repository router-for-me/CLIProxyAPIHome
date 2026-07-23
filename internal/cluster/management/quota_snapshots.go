package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

const (
	quotaListDefaultLimit = 50
	quotaListMaxLimit     = 200
)

type quotaHTTPError struct {
	Code    string
	Message string
}

type quotaResetCreditDTO struct {
	Status    string     `json:"status"`
	GrantedAt time.Time  `json:"granted_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type quotaResetCreditsDTO struct {
	AvailableCount *int                  `json:"available_count"`
	ObservedAt     time.Time             `json:"observed_at"`
	Credits        []quotaResetCreditDTO `json:"credits"`
}

func quotaResetCreditsDTOFrom(value *cluster.QuotaResetCredits) *quotaResetCreditsDTO {
	if value == nil {
		return nil
	}
	credits := make([]quotaResetCreditDTO, 0, len(value.Credits))
	for _, credit := range value.Credits {
		var expiresAt *time.Time
		if credit.ExpiresAt != nil {
			expiresAtUTC := credit.ExpiresAt.UTC()
			expiresAt = &expiresAtUTC
		}
		credits = append(credits, quotaResetCreditDTO{
			Status: credit.Status, GrantedAt: credit.GrantedAt.UTC(), ExpiresAt: expiresAt,
		})
	}
	return &quotaResetCreditsDTO{
		AvailableCount: value.AvailableCount,
		ObservedAt:     value.ObservedAt.UTC(),
		Credits:        credits,
	}
}

func (h *Handler) ListQuotaCredentials(c *gin.Context) {
	query, errParse := parseQuotaListQuery(c)
	if errParse != nil {
		respondQuotaHTTPError(c, http.StatusBadRequest, errParse.Code, errParse.Message, false)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	result, errList := h.repo.ListQuotaCredentials(ctx, query)
	if errList != nil {
		status := quotaReadErrorStatus(errList)
		respondQuotaHTTPError(c, status, "QUOTA_SNAPSHOTS_LOAD_FAILED", "failed to load quota snapshots", true)
		return
	}
	now := time.Now().UTC()
	c.JSON(http.StatusOK, gin.H{
		"items": result.Items, "total": result.Total, "limit": query.Limit, "offset": query.Offset,
		"sort": query.Sort, "generated_at": now, "summary": result.Summary,
		"global_summary": result.GlobalSummary, "facets": result.Facets,
	})
}

func (h *Handler) GetQuotaCredential(c *gin.Context) {
	credentialID := strings.TrimSpace(c.Param("credential_id"))
	if credentialID == "" {
		respondQuotaHTTPError(c, http.StatusNotFound, "QUOTA_CREDENTIAL_NOT_FOUND", "quota credential not found", false)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	item, errGet := h.repo.GetQuotaCredential(ctx, credentialID, time.Now().UTC())
	if errGet != nil {
		if errors.Is(errGet, gorm.ErrRecordNotFound) {
			respondQuotaHTTPError(c, http.StatusNotFound, "QUOTA_CREDENTIAL_NOT_FOUND", "quota credential not found", false)
			return
		}
		status := quotaReadErrorStatus(errGet)
		respondQuotaHTTPError(c, status, "QUOTA_SNAPSHOT_LOAD_FAILED", "failed to load quota credential", true)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"credential":    item,
		"windows":       item.Windows,
		"reset_credits": quotaResetCreditsDTOFrom(item.ResetCredits),
		"collection": gin.H{
			"source": item.Source, "freshness": item.Freshness, "status": item.CollectionStatus,
			"observed_at": item.ObservedAt, "expires_at": item.ExpiresAt, "last_attempt_at": item.LastAttemptAt,
			"last_success_at": item.LastSuccessAt, "next_probe_at": item.NextProbeAt,
			"consecutive_failures": item.ConsecutiveFailure, "error": item.Error,
		},
		"generated_at": time.Now().UTC(),
	})
}

func parseQuotaListQuery(c *gin.Context) (cluster.QuotaListQuery, *quotaHTTPError) {
	query := cluster.QuotaListQuery{Limit: quotaListDefaultLimit, Sort: "risk_desc", Now: time.Now().UTC()}
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		limit, errLimit := strconv.Atoi(rawLimit)
		if errLimit != nil || limit <= 0 || limit > quotaListMaxLimit {
			return query, quotaQueryError("INVALID_PAGINATION", "limit must be between 1 and %d", quotaListMaxLimit)
		}
		query.Limit = limit
	}
	if rawOffset := strings.TrimSpace(c.Query("offset")); rawOffset != "" {
		offset, errOffset := strconv.Atoi(rawOffset)
		if errOffset != nil || offset < 0 {
			return query, quotaQueryError("INVALID_PAGINATION", "offset must be a non-negative integer")
		}
		query.Offset = offset
	}
	query.Search = strings.TrimSpace(c.Query("search"))
	query.IDs = quotaExactCSVSet(c.Query("ids"))
	query.Providers = quotaCSVSet(c.Query("provider"))
	var errFilter *quotaHTTPError
	if query.QuotaStatuses, errFilter = quotaEnumCSVSet(c.Query("quota_status"), "quota_status", "healthy", "low", "exhausted", "unknown", "error", "unsupported"); errFilter != nil {
		return query, errFilter
	}
	if query.Freshness, errFilter = quotaEnumCSVSet(c.Query("freshness"), "freshness", "fresh", "stale", "never"); errFilter != nil {
		return query, errFilter
	}
	if query.Sources, errFilter = quotaEnumCSVSet(c.Query("source"), "source", "response_header", "active_probe", "mixed", "none"); errFilter != nil {
		return query, errFilter
	}
	if query.CredentialStatuses, errFilter = quotaEnumCSVSet(c.Query("credential_status"), "credential_status", "enabled", "disabled", "unavailable", "cooldown", "unknown"); errFilter != nil {
		return query, errFilter
	}
	if query.CollectionStatuses, errFilter = quotaEnumCSVSet(c.Query("collection_status"), "collection_status", "idle", "collecting", "success", "partial", "failed", "unsupported"); errFilter != nil {
		return query, errFilter
	}
	if rawSort := strings.TrimSpace(c.Query("sort")); rawSort != "" {
		if !quotaManagementAllowed(rawSort, "risk_desc", "observed_at_desc", "observed_at_asc", "reset_at_asc", "provider_asc", "label_asc") {
			return query, quotaQueryError("INVALID_SORT", "sort contains an unsupported value")
		}
		query.Sort = rawSort
	}
	return query, nil
}

func quotaExactCSVSet(raw string) map[string]struct{} {
	values := make(map[string]struct{})
	for _, candidate := range strings.Split(raw, ",") {
		value := strings.TrimSpace(candidate)
		if value != "" {
			values[value] = struct{}{}
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func quotaCSVSet(raw string) map[string]struct{} {
	values := make(map[string]struct{})
	for _, candidate := range strings.Split(raw, ",") {
		value := strings.ToLower(strings.TrimSpace(candidate))
		if value != "" {
			values[value] = struct{}{}
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func quotaEnumCSVSet(raw string, field string, allowed ...string) (map[string]struct{}, *quotaHTTPError) {
	values := quotaCSVSet(raw)
	for value := range values {
		if !quotaManagementAllowed(value, allowed...) {
			return nil, quotaQueryError("INVALID_FILTER", "%s contains an unsupported value", field)
		}
	}
	return values, nil
}

func quotaManagementAllowed(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func quotaQueryError(code string, format string, args ...any) *quotaHTTPError {
	return &quotaHTTPError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func respondQuotaHTTPError(c *gin.Context, status int, code string, message string, retryable bool) {
	requestID := ""
	if c != nil && c.Request != nil {
		requestID = strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if len(requestID) > 128 {
			requestID = ""
		}
	}
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message, "request_id": requestID, "retryable": retryable}})
}

func quotaReadErrorStatus(errRead error) int {
	if errRead == nil {
		return http.StatusOK
	}
	if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
		return http.StatusServiceUnavailable
	}
	message := strings.ToLower(errRead.Error())
	for _, marker := range []string{
		"database is nil", "database is closed", "database is locked", "driver: bad connection",
		"connection refused", "connection reset", "connection unavailable", "server closed the connection",
		"too many connections", "i/o timeout", "sqlstate 57p",
	} {
		if strings.Contains(message, marker) {
			return http.StatusServiceUnavailable
		}
	}
	return http.StatusInternalServerError
}
