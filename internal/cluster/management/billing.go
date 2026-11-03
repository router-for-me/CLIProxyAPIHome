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
	"gorm.io/gorm"
)

type billingModelPriceRequest struct {
	Provider                  *string  `json:"provider"`
	Model                     *string  `json:"model"`
	ServiceTier               *string  `json:"service_tier"`
	MinInputTokens            *int64   `json:"min_input_tokens"`
	InputPricePerMillion      *float64 `json:"input_price_per_million"`
	OutputPricePerMillion     *float64 `json:"output_price_per_million"`
	CacheReadPricePerMillion  *float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion *float64 `json:"cache_write_price_per_million"`
	RequestPrice              *float64 `json:"request_price"`
	Source                    *string  `json:"source"`
	Enabled                   *bool    `json:"enabled"`
	Note                      *string  `json:"note"`
}

type billingSettingsRequest struct {
	ServiceTierSource *string `json:"service_tier_source"`
	ReportTimezone    *string `json:"report_timezone"`
}

type billingBalanceRequest struct {
	UserID uint     `json:"user_id"`
	Amount *float64 `json:"amount"`
	Note   string   `json:"note"`
}

func (h *Handler) CreateBillingModelPrice(c *gin.Context) {
	var body billingModelPriceRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	update, ok := billingModelPriceUpdateFromRequest(c, body)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errCreate := h.repo.CreateBillingModelPrice(ctx, update)
	if errCreate != nil {
		if errors.Is(errCreate, cluster.ErrBillingInvalidModelPrice) {
			respondError(c, http.StatusBadRequest, "invalid_body", errCreate)
			return
		}
		if errors.Is(errCreate, cluster.ErrBillingDuplicateModelPrice) {
			respondError(c, http.StatusConflict, "model_price_exists", errCreate)
			return
		}
		respondError(c, http.StatusInternalServerError, "model_price_create_failed", errCreate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "model_price": billingModelPriceResponse(record)})
}

func (h *Handler) UpdateBillingModelPrice(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		respondError(c, http.StatusNotFound, "model_price_not_found", gorm.ErrRecordNotFound)
		return
	}
	var body billingModelPriceRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	patch, ok := billingModelPricePatchFromRequest(c, body)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errUpdate := h.repo.UpdateBillingModelPrice(ctx, id, patch)
	if errUpdate != nil {
		if errors.Is(errUpdate, cluster.ErrBillingInvalidModelPrice) {
			respondError(c, http.StatusBadRequest, "invalid_body", errUpdate)
			return
		}
		if errors.Is(errUpdate, gorm.ErrRecordNotFound) {
			respondError(c, http.StatusNotFound, "model_price_not_found", errUpdate)
			return
		}
		if errors.Is(errUpdate, cluster.ErrBillingDuplicateModelPrice) {
			respondError(c, http.StatusConflict, "model_price_exists", errUpdate)
			return
		}
		respondError(c, http.StatusInternalServerError, "model_price_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "model_price": billingModelPriceResponse(record)})
}

func (h *Handler) DeleteBillingModelPrice(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		respondError(c, http.StatusNotFound, "model_price_not_found", gorm.ErrRecordNotFound)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	errDelete := h.repo.DeleteBillingModelPrice(ctx, id)
	if errDelete != nil {
		if errors.Is(errDelete, gorm.ErrRecordNotFound) {
			respondError(c, http.StatusNotFound, "model_price_not_found", errDelete)
			return
		}
		respondError(c, http.StatusInternalServerError, "model_price_delete_failed", errDelete)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) RechargeBillingBalance(c *gin.Context) {
	h.applyBillingBalance(c, cluster.BillingBalanceTypeRecharge)
}

func (h *Handler) DeductBillingBalance(c *gin.Context) {
	h.applyBillingBalance(c, cluster.BillingBalanceTypeDeduct)
}

func (h *Handler) GetBillingOverview(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	settings, errSettings := h.repo.GetBillingSettings(ctx)
	if errSettings != nil {
		respondError(c, http.StatusInternalServerError, "billing_settings_load_failed", errSettings)
		return
	}
	query, ok := billingOverviewQueryFromRequest(c, settings.ReportTimezone)
	if !ok {
		return
	}
	release, errAcquire := h.acquireHeavyStatisticsSlot(ctx, c)
	if errAcquire != nil {
		respondError(c, http.StatusInternalServerError, "billing_overview_load_failed", errAcquire)
		return
	}
	overview, errOverview := h.repo.BillingOverview(ctx, query)
	release()
	if errOverview != nil {
		respondError(c, http.StatusInternalServerError, "billing_overview_load_failed", errOverview)
		return
	}
	c.JSON(http.StatusOK, gin.H{"overview": billingOverviewResponse(overview)})
}

func (h *Handler) ListBillingCharges(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	settings, errSettings := h.repo.GetBillingSettings(ctx)
	if errSettings != nil {
		respondError(c, http.StatusInternalServerError, "billing_settings_load_failed", errSettings)
		return
	}
	query, ok := billingChargeQueryFromRequest(c, settings.ReportTimezone)
	if !ok {
		return
	}
	limit, offset := query.Limit, query.Offset
	result, errCharges := h.repo.ListBillingCharges(ctx, query)
	if errCharges != nil {
		respondError(c, http.StatusInternalServerError, "billing_charge_load_failed", errCharges)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		items = append(items, billingChargeResponse(&result.Records[index], true))
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  items,
		"total":  result.Total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *Handler) ListBillingBalanceRecords(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	settings, errSettings := h.repo.GetBillingSettings(ctx)
	if errSettings != nil {
		respondError(c, http.StatusInternalServerError, "billing_settings_load_failed", errSettings)
		return
	}
	query, ok := billingBalanceQueryFromRequest(c, settings.ReportTimezone)
	if !ok {
		return
	}
	limit, offset := query.Limit, query.Offset
	result, errRecords := h.repo.ListBillingBalanceRecords(ctx, query)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "billing_balance_record_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		items = append(items, billingBalanceRecordResponse(&result.Records[index]))
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  items,
		"total":  result.Total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *Handler) ListBillingModelPrices(c *gin.Context) {
	enabled, ok := billingEnabledQuery(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	records, errRecords := h.repo.ListBillingModelPrices(ctx, cluster.BillingModelPriceQuery{
		Provider: firstNonEmptyQuery(c, "provider"),
		Model:    firstNonEmptyQuery(c, "model"),
		Enabled:  enabled,
	})
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "billing_model_price_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for index := range records {
		items = append(items, billingModelPriceResponse(&records[index]))
	}
	c.JSON(http.StatusOK, gin.H{"price_rule_schema_version": 2, "items": items})
}

func (h *Handler) GetBillingSettings(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	settings, errSettings := h.repo.GetBillingSettings(ctx)
	if errSettings != nil {
		respondError(c, http.StatusInternalServerError, "billing_settings_load_failed", errSettings)
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) UpdateBillingSettings(c *gin.Context) {
	var body billingSettingsRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	settings, errSettings := h.repo.UpdateBillingSettings(ctx, cluster.BillingSettingsPatch{
		ServiceTierSource: body.ServiceTierSource,
		ReportTimezone:    body.ReportTimezone,
	})
	if errSettings != nil {
		if strings.Contains(errSettings.Error(), "service_tier_source") || strings.Contains(errSettings.Error(), "report_timezone") {
			respondError(c, http.StatusBadRequest, "invalid_body", errSettings)
			return
		}
		respondError(c, http.StatusInternalServerError, "billing_settings_update_failed", errSettings)
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) applyBillingBalance(c *gin.Context, recordType string) {
	var body billingBalanceRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	if body.UserID == 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("user_id is required"))
		return
	}
	if body.Amount == nil || *body.Amount <= 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("amount must be positive"))
		return
	}
	if recordType == cluster.BillingBalanceTypeDeduct && strings.TrimSpace(body.Note) == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("note is required for deduct balance record"))
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.ApplyBillingBalanceRecord(ctx, cluster.BillingBalanceUpdate{
		UserID:   body.UserID,
		Type:     recordType,
		Amount:   *body.Amount,
		Operator: "admin",
		Note:     strings.TrimSpace(body.Note),
	})
	if errRecord != nil {
		if errors.Is(errRecord, gorm.ErrRecordNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", errRecord)
			return
		}
		respondError(c, http.StatusInternalServerError, "balance_record_create_failed", errRecord)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "balance_record": billingBalanceRecordResponse(record)})
}

func billingModelPriceUpdateFromRequest(c *gin.Context, body billingModelPriceRequest) (cluster.BillingModelPriceUpdate, bool) {
	update := cluster.BillingModelPriceUpdate{
		Provider:                  strings.TrimSpace(billingStringValue(body.Provider)),
		Model:                     strings.TrimSpace(billingStringValue(body.Model)),
		ServiceTier:               strings.TrimSpace(billingStringValue(body.ServiceTier)),
		MinInputTokens:            int64Value(body.MinInputTokens),
		InputPricePerMillion:      floatValue(body.InputPricePerMillion),
		OutputPricePerMillion:     floatValue(body.OutputPricePerMillion),
		CacheReadPricePerMillion:  floatValue(body.CacheReadPricePerMillion),
		CacheWritePricePerMillion: floatValue(body.CacheWritePricePerMillion),
		RequestPrice:              floatValue(body.RequestPrice),
		Source:                    strings.TrimSpace(billingStringValue(body.Source)),
		Enabled:                   true,
		Note:                      strings.TrimSpace(billingStringValue(body.Note)),
	}
	if body.Enabled != nil {
		update.Enabled = *body.Enabled
	}
	if update.Provider == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("provider is required"))
		return update, false
	}
	if update.Model == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("model is required"))
		return update, false
	}
	if update.MinInputTokens < 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("min_input_tokens must be non-negative"))
		return update, false
	}
	for name, value := range map[string]float64{
		"input_price_per_million":       update.InputPricePerMillion,
		"output_price_per_million":      update.OutputPricePerMillion,
		"cache_read_price_per_million":  update.CacheReadPricePerMillion,
		"cache_write_price_per_million": update.CacheWritePricePerMillion,
		"request_price":                 update.RequestPrice,
	} {
		if value < 0 {
			respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("%s must be non-negative", name))
			return update, false
		}
	}
	return update, true
}

func billingModelPricePatchFromRequest(c *gin.Context, body billingModelPriceRequest) (cluster.BillingModelPricePatch, bool) {
	patch := cluster.BillingModelPricePatch{}
	if body.Provider != nil {
		provider := strings.TrimSpace(*body.Provider)
		if provider == "" {
			respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("provider is required"))
			return patch, false
		}
		patch.Provider = &provider
	}
	if body.Model != nil {
		model := strings.TrimSpace(*body.Model)
		if model == "" {
			respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("model is required"))
			return patch, false
		}
		patch.Model = &model
	}
	if body.ServiceTier != nil {
		serviceTier := strings.TrimSpace(*body.ServiceTier)
		patch.ServiceTier = &serviceTier
	}
	if body.MinInputTokens != nil && *body.MinInputTokens < 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("min_input_tokens must be non-negative"))
		return patch, false
	}
	patch.MinInputTokens = body.MinInputTokens
	for name, value := range map[string]*float64{
		"input_price_per_million":       body.InputPricePerMillion,
		"output_price_per_million":      body.OutputPricePerMillion,
		"cache_read_price_per_million":  body.CacheReadPricePerMillion,
		"cache_write_price_per_million": body.CacheWritePricePerMillion,
		"request_price":                 body.RequestPrice,
	} {
		if value != nil && *value < 0 {
			respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("%s must be non-negative", name))
			return patch, false
		}
	}
	patch.InputPricePerMillion = body.InputPricePerMillion
	patch.OutputPricePerMillion = body.OutputPricePerMillion
	patch.CacheReadPricePerMillion = body.CacheReadPricePerMillion
	patch.CacheWritePricePerMillion = body.CacheWritePricePerMillion
	patch.RequestPrice = body.RequestPrice
	if body.Source != nil {
		source := strings.TrimSpace(*body.Source)
		patch.Source = &source
	}
	patch.Enabled = body.Enabled
	if body.Note != nil {
		note := strings.TrimSpace(*body.Note)
		patch.Note = &note
	}
	return patch, true
}

func billingStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func floatValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func billingModelPriceResponse(record *cluster.BillingModelPriceRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":                            record.ID,
		"provider":                      record.Provider,
		"model":                         record.Model,
		"service_tier":                  record.ServiceTier,
		"min_input_tokens":              record.MinInputTokens,
		"input_price_per_million":       record.InputPricePerMillion,
		"output_price_per_million":      record.OutputPricePerMillion,
		"cache_read_price_per_million":  record.CacheReadPricePerMillion,
		"cache_write_price_per_million": record.CacheWritePricePerMillion,
		"request_price":                 record.RequestPrice,
		"source":                        record.Source,
		"enabled":                       record.Enabled,
		"note":                          record.Note,
		"created_at":                    record.CreatedAt,
		"updated_at":                    record.UpdatedAt,
		"revision":                      record.Revision,
	}
}

func billingChargeResponse(record *cluster.BillingChargeRecord, admin bool) gin.H {
	if record == nil {
		return gin.H{}
	}
	response := gin.H{
		"id":            record.ID,
		"created_at":    record.CreatedAt,
		"provider":      record.Provider,
		"model":         record.Model,
		"input_tokens":  record.InputTokens,
		"output_tokens": record.OutputTokens,
		"cache_tokens":  record.CacheTokens,
		"amount":        record.Amount,
		"balance_after": record.BalanceAfter,
	}
	if admin {
		response["user_id"] = record.UserID
		response["api_key_label"] = record.APIKeyLabel
		response["api_key_masked"] = record.APIKeyMasked
		response["original_model"] = record.OriginalModel
		response["actual_model"] = record.ActualModel
		response["request_id"] = record.RequestID
		response["endpoint"] = record.Endpoint
		response["balance_before"] = record.BalanceBefore
		response["matched_price_rule"] = record.MatchedPriceRule
		response["price_snapshot"] = record.PriceSnapshot
		response["cache_read_tokens"] = record.CacheReadTokens
		response["cache_write_tokens"] = record.CacheWriteTokens
	}
	return response
}

func billingOverviewResponse(overview cluster.BillingOverview) gin.H {
	dailyTrend := make([]gin.H, 0, len(overview.DailyTrend))
	for _, point := range overview.DailyTrend {
		dailyTrend = append(dailyTrend, gin.H{
			"date":          point.Date,
			"charge_amount": point.ChargeAmount,
			"request_count": point.RequestCount,
		})
	}
	return gin.H{
		"range": gin.H{
			"from":            overview.Range.From,
			"to":              overview.Range.To,
			"from_at":         overview.Range.FromAt,
			"to_at_exclusive": overview.Range.ToAtExclusive,
			"timezone":        overview.Range.Timezone,
		},
		"total_charge_amount":   overview.TotalChargeAmount,
		"total_recharge_amount": overview.TotalRechargeAmount,
		"total_deduct_amount":   overview.TotalDeductAmount,
		"total_balance":         overview.TotalBalance,
		"request_count":         overview.RequestCount,
		"input_tokens":          overview.InputTokens,
		"output_tokens":         overview.OutputTokens,
		"cache_tokens":          overview.CacheTokens,
		"active_user_count":     overview.ActiveUserCount,
		"daily_trend":           dailyTrend,
		"top_users":             billingTopItemsResponse(overview.TopUsers),
		"top_models":            billingTopItemsResponse(overview.TopModels),
		"top_providers":         billingTopItemsResponse(overview.TopProviders),
	}
}

func billingTopItemsResponse(items []cluster.BillingTopItem) []gin.H {
	response := make([]gin.H, 0, len(items))
	for _, item := range items {
		response = append(response, gin.H{
			"id":            item.ID,
			"label":         item.Label,
			"amount":        item.Amount,
			"request_count": item.RequestCount,
		})
	}
	return response
}

func billingBalanceRecordResponse(record *cluster.BillingBalanceRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":             record.ID,
		"user_id":        record.UserID,
		"type":           record.Type,
		"amount":         record.Amount,
		"balance_before": record.BalanceBefore,
		"balance_after":  record.BalanceAfter,
		"operator":       record.Operator,
		"note":           record.Note,
		"created_at":     record.CreatedAt,
	}
}

func billingOverviewQueryFromRequest(c *gin.Context, defaultTimezone string) (cluster.BillingOverviewQuery, bool) {
	from, to, timezone, ok := billingRangeQueryFromRequest(c, defaultTimezone)
	if !ok {
		return cluster.BillingOverviewQuery{}, false
	}
	userID, ok := billingUintQuery(c, "user_id", "uid")
	if !ok {
		return cluster.BillingOverviewQuery{}, false
	}
	return cluster.BillingOverviewQuery{
		From:        from,
		ToExclusive: to,
		Timezone:    timezone,
		UserText:    firstNonEmptyQuery(c, "user", "user_text", "username"),
		UserID:      userID,
		Provider:    firstNonEmptyQuery(c, "provider"),
		Model:       firstNonEmptyQuery(c, "model"),
	}, true
}

func billingChargeQueryFromRequest(c *gin.Context, defaultTimezone string) (cluster.BillingChargeQuery, bool) {
	from, to, _, ok := billingRangeQueryFromRequest(c, defaultTimezone)
	if !ok {
		return cluster.BillingChargeQuery{}, false
	}
	userID, ok := billingUintQuery(c, "user_id", "uid")
	if !ok {
		return cluster.BillingChargeQuery{}, false
	}
	limit, offset, ok := billingPaginationQuery(c)
	if !ok {
		return cluster.BillingChargeQuery{}, false
	}
	return cluster.BillingChargeQuery{
		From:        from,
		ToExclusive: to,
		UserText:    firstNonEmptyQuery(c, "user", "user_text", "username"),
		UserID:      userID,
		Provider:    firstNonEmptyQuery(c, "provider"),
		Model:       firstNonEmptyQuery(c, "model"),
		Limit:       limit,
		Offset:      offset,
	}, true
}

func billingBalanceQueryFromRequest(c *gin.Context, defaultTimezone string) (cluster.BillingBalanceQuery, bool) {
	from, to, _, ok := billingRangeQueryFromRequest(c, defaultTimezone)
	if !ok {
		return cluster.BillingBalanceQuery{}, false
	}
	userID, ok := billingUintQuery(c, "user_id", "uid")
	if !ok {
		return cluster.BillingBalanceQuery{}, false
	}
	limit, offset, ok := billingPaginationQuery(c)
	if !ok {
		return cluster.BillingBalanceQuery{}, false
	}
	return cluster.BillingBalanceQuery{
		From:        from,
		ToExclusive: to,
		UserText:    firstNonEmptyQuery(c, "user", "user_text", "username"),
		UserID:      userID,
		Limit:       limit,
		Offset:      offset,
	}, true
}

func clusterBillingPagination(limit int, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func billingRangeQueryFromRequest(c *gin.Context, defaultTimezone string) (*time.Time, *time.Time, string, bool) {
	timezoneInput := strings.TrimSpace(c.Query("timezone"))
	if timezoneInput == "" {
		timezoneInput = defaultTimezone
	}
	location, timezone, errTimezone := parseUsageTimezone(timezoneInput)
	if errTimezone != nil {
		respondUsageHTTPError(c, errTimezone)
		return nil, nil, "", false
	}
	from, _, ok := billingTimeQuery(c, "from", location)
	if !ok {
		return nil, nil, "", false
	}
	toExclusive, toDateOnly, ok := billingTimeQuery(c, "to", location)
	if !ok {
		return nil, nil, "", false
	}
	if toExclusive != nil && toDateOnly {
		normalized := nextBillingDateBoundary(*toExclusive, location)
		toExclusive = &normalized
	}
	if from != nil && toExclusive != nil && from.After(*toExclusive) {
		respondError(c, http.StatusBadRequest, "invalid_time_range", fmt.Errorf("from must not be after to"))
		return nil, nil, "", false
	}
	return from, toExclusive, timezone, true
}

func billingTimeQuery(c *gin.Context, key string, location *time.Location) (*time.Time, bool, bool) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return nil, false, true
	}
	parsed, dateOnly, errParse := parseBillingTime(raw, location)
	if errParse != nil {
		respondError(c, http.StatusBadRequest, "invalid_"+key, errParse)
		return nil, false, false
	}
	return &parsed, dateOnly, true
}

func parseBillingTime(raw string, location *time.Location) (time.Time, bool, error) {
	raw = strings.TrimSpace(raw)
	if location == nil {
		location = time.UTC
	}
	if parsed, errDate := time.ParseInLocation("2006-01-02", raw, location); errDate == nil {
		return parsed.UTC(), true, nil
	}
	if parsed, errRFC3339 := time.Parse(time.RFC3339Nano, raw); errRFC3339 == nil {
		return parsed.UTC(), false, nil
	}
	seconds, errSeconds := strconv.ParseInt(raw, 10, 64)
	if errSeconds == nil {
		return time.Unix(seconds, 0).UTC(), false, nil
	}
	return time.Time{}, false, fmt.Errorf("time must be YYYY-MM-DD, RFC3339, or unix seconds")
}

func nextBillingDateBoundary(value time.Time, location *time.Location) time.Time {
	if location == nil {
		location = time.UTC
	}
	local := value.In(location)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location).UTC()
}

func billingPaginationQuery(c *gin.Context) (int, int, bool) {
	limit, ok := billingIntQuery(c, "limit", "invalid_limit")
	if !ok {
		return 0, 0, false
	}
	offset, ok := billingIntQuery(c, "offset", "invalid_offset")
	if !ok {
		return 0, 0, false
	}
	limit, offset = clusterBillingPagination(limit, offset)
	return limit, offset, true
}

func billingIntQuery(c *gin.Context, key string, code string) (int, bool) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return 0, true
	}
	parsed, errParse := strconv.Atoi(raw)
	if errParse != nil {
		respondError(c, http.StatusBadRequest, code, fmt.Errorf("%s must be an integer", key))
		return 0, false
	}
	return parsed, true
}

func billingEnabledQuery(c *gin.Context) (*bool, bool) {
	raw := strings.TrimSpace(c.Query("enabled"))
	if raw == "" {
		return nil, true
	}
	parsed, errParse := strconv.ParseBool(raw)
	if errParse != nil {
		respondError(c, http.StatusBadRequest, "invalid_enabled", fmt.Errorf("enabled must be a boolean"))
		return nil, false
	}
	return &parsed, true
}

func billingUintQuery(c *gin.Context, keys ...string) (*uint, bool) {
	raw := firstNonEmptyQuery(c, keys...)
	if strings.TrimSpace(raw) == "" {
		return nil, true
	}
	parsed, errParse := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if errParse != nil || parsed == 0 {
		respondError(c, http.StatusBadRequest, "invalid_user_id", fmt.Errorf("user_id must be a positive integer"))
		return nil, false
	}
	value := uint(parsed)
	return &value, true
}
