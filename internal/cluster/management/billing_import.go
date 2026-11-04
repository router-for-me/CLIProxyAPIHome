package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const (
	modelsDevCatalogURL                  = "https://models.dev/api.json"
	openRouterCatalogURL                 = "https://openrouter.ai/api/v1/models"
	liteLLMCatalogURL                    = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	modelsDevDefaultCacheReadInputRatio  = 0.1
	modelsDevDefaultCacheWriteInputRatio = 1.25
	billingImportMaxRequestBodySize      = 16 << 20
)

func (h *Handler) PreviewBillingModelPriceImport(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, billingImportMaxRequestBodySize)
	var body cluster.BillingModelPriceImportPreviewInput
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	if errValidate := cluster.ValidateBillingModelPriceImportPreviewInput(body); errValidate != nil {
		respondError(c, http.StatusUnprocessableEntity, "invalid_import_preview", errValidate)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if body.AutoDiscover && len(body.Targets) == 0 {
		discovered, errDiscover := h.discoverBillingImportTargetsWithRegistry(ctx)
		if errDiscover != nil {
			respondError(c, http.StatusInternalServerError, "billing_import_preview_failed", errDiscover)
			return
		}
		body.Targets = discovered
	}
	catalog, errCatalog := h.resolveBillingModelPriceImportCatalog(ctx, body)
	if errCatalog != nil {
		respondError(c, http.StatusBadGateway, "models_dev_fetch_failed", errCatalog)
		return
	}
	preview, errPreview := h.repo.CreateBillingModelPriceImportPreview(ctx, body, catalog)
	if errPreview != nil {
		respondError(c, http.StatusInternalServerError, "billing_import_preview_failed", errPreview)
		return
	}
	c.JSON(http.StatusOK, preview)
}

func (h *Handler) ApplyBillingModelPriceImport(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, billingImportMaxRequestBodySize)
	var body cluster.BillingModelPriceImportApplyInput
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	headerKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = headerKey
	} else if headerKey != "" && body.IdempotencyKey != headerKey {
		respondError(c, http.StatusUnprocessableEntity, "idempotency_key_mismatch", fmt.Errorf("Idempotency-Key header does not match request body"))
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	operation, errApply := h.repo.ApplyBillingModelPriceImport(ctx, body)
	if errApply != nil {
		switch {
		case errors.Is(errApply, cluster.ErrBillingImportPreviewNotFound):
			respondError(c, http.StatusNotFound, "import_preview_not_found", errApply)
		case errors.Is(errApply, cluster.ErrBillingImportPreviewExpired):
			respondError(c, http.StatusGone, "import_preview_expired", errApply)
		case errors.Is(errApply, cluster.ErrBillingImportPreviewStale):
			respondError(c, http.StatusPreconditionFailed, "import_preview_stale", errApply)
		case errors.Is(errApply, cluster.ErrBillingImportRuleConflict):
			respondError(c, http.StatusConflict, "model_price_revision_conflict", errApply)
		case errors.Is(errApply, cluster.ErrBillingImportInvalidSelection), errors.Is(errApply, cluster.ErrBillingImportOverwriteRequired), errors.Is(errApply, cluster.ErrBillingImportIdempotencyConflict):
			respondError(c, http.StatusUnprocessableEntity, "invalid_import_apply", errApply)
		default:
			respondError(c, http.StatusInternalServerError, "billing_import_apply_failed", errApply)
		}
		return
	}
	c.JSON(http.StatusOK, operation)
}

func (h *Handler) GetBillingModelPriceImportOperation(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	operation, errOperation := h.repo.GetBillingModelPriceImportOperation(ctx, c.Param("id"))
	if errOperation != nil {
		if errors.Is(errOperation, cluster.ErrBillingImportOperationNotFound) {
			respondError(c, http.StatusNotFound, "import_operation_not_found", errOperation)
			return
		}
		respondError(c, http.StatusInternalServerError, "billing_import_operation_load_failed", errOperation)
		return
	}
	c.JSON(http.StatusOK, operation)
}

func (h *Handler) GetBillingTierDiagnostics(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	diagnostics, errDiagnostics := h.repo.GetBillingTierDiagnostics(ctx)
	if errDiagnostics != nil {
		respondError(c, http.StatusInternalServerError, "billing_tier_diagnostics_load_failed", errDiagnostics)
		return
	}
	c.JSON(http.StatusOK, diagnostics)
}

func (h *Handler) resolveBillingModelPriceImportCatalog(ctx context.Context, body cluster.BillingModelPriceImportPreviewInput) (cluster.BillingModelPriceImportCatalog, error) {
	source := strings.ToLower(strings.TrimSpace(body.Source))
	if source == "" {
		source = cluster.BillingModelPriceImportSourceModelsDev
	}
	now := time.Now().UTC()
	if len(bytesTrimSpace(body.CatalogJSON)) > 0 {
		return parseBillingModelPriceImportCatalogBySource(source, body.CatalogJSON, "inline://"+source, now)
	}
	switch source {
	case cluster.BillingModelPriceImportSourceOpenRouter:
		raw, errFetch := h.fetchCatalogBytes(ctx, openRouterCatalogURL)
		if errFetch != nil {
			return cluster.BillingModelPriceImportCatalog{}, errFetch
		}
		return parseOpenRouterBillingImportCatalog(raw, openRouterCatalogURL, now)
	case cluster.BillingModelPriceImportSourceLiteLLM:
		raw, errFetch := h.fetchCatalogBytes(ctx, liteLLMCatalogURL)
		if errFetch != nil {
			return cluster.BillingModelPriceImportCatalog{}, errFetch
		}
		return parseLiteLLMBillingImportCatalog(raw, liteLLMCatalogURL, now)
	case cluster.BillingModelPriceImportSourceAuto:
		return h.fetchAutoBillingModelPriceImportCatalog(ctx, now)
	default:
		return h.fetchBillingModelPriceImportCatalog(ctx)
	}
}

func (h *Handler) fetchBillingModelPriceImportCatalog(ctx context.Context) (cluster.BillingModelPriceImportCatalog, error) {
	raw, errFetch := h.fetchCatalogBytes(ctx, modelsDevCatalogURL)
	if errFetch != nil {
		return cluster.BillingModelPriceImportCatalog{}, errFetch
	}
	return parseBillingModelPriceImportCatalog(raw, modelsDevCatalogURL, time.Now().UTC())
}

func (h *Handler) fetchCatalogBytes(ctx context.Context, url string) ([]byte, error) {
	client := h.modelsDevHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func (h *Handler) fetchAutoBillingModelPriceImportCatalog(ctx context.Context, now time.Time) (cluster.BillingModelPriceImportCatalog, error) {
	var catalogs []cluster.BillingModelPriceImportCatalog
	for _, item := range []struct {
		source string
		url    string
	}{
		{source: cluster.BillingModelPriceImportSourceModelsDev, url: modelsDevCatalogURL},
		{source: cluster.BillingModelPriceImportSourceOpenRouter, url: openRouterCatalogURL},
		{source: cluster.BillingModelPriceImportSourceLiteLLM, url: liteLLMCatalogURL},
	} {
		raw, errFetch := h.fetchCatalogBytes(ctx, item.url)
		if errFetch != nil {
			continue
		}
		parsed, errParse := parseBillingModelPriceImportCatalogBySource(item.source, raw, item.url, now)
		if errParse == nil && len(parsed.Models) > 0 {
			catalogs = append(catalogs, parsed)
		}
	}
	if len(catalogs) == 0 {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("failed to fetch any pricing catalog")
	}
	return mergeBillingImportCatalogs(catalogs, now), nil
}

func parseBillingModelPriceImportCatalogBySource(source string, raw []byte, sourceURL string, fetchedAt time.Time) (cluster.BillingModelPriceImportCatalog, error) {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case cluster.BillingModelPriceImportSourceOpenRouter:
		return parseOpenRouterBillingImportCatalog(raw, sourceURL, fetchedAt)
	case cluster.BillingModelPriceImportSourceLiteLLM:
		return parseLiteLLMBillingImportCatalog(raw, sourceURL, fetchedAt)
	case cluster.BillingModelPriceImportSourceAuto:
		// Auto-detect JSON shape when inline catalog_json is used with source="auto".
		if cat, errOR := parseOpenRouterBillingImportCatalog(raw, sourceURL, fetchedAt); errOR == nil && len(cat.Models) > 0 {
			return cat, nil
		}
		if cat, errMD := parseBillingModelPriceImportCatalog(raw, sourceURL, fetchedAt); errMD == nil && len(cat.Models) > 0 {
			return cat, nil
		}
		return parseLiteLLMBillingImportCatalog(raw, sourceURL, fetchedAt)
	default:
		return parseBillingModelPriceImportCatalog(raw, sourceURL, fetchedAt)
	}
}

func parseBillingModelPriceImportCatalog(raw []byte, sourceURL string, fetchedAt time.Time) (cluster.BillingModelPriceImportCatalog, error) {
	var root map[string]any
	if errDecode := json.Unmarshal(raw, &root); errDecode != nil {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("decode models.dev catalog: %w", errDecode)
	}
	sum := sha256.Sum256(raw)
	catalog := cluster.BillingModelPriceImportCatalog{SourceURL: sourceURL, Version: hex.EncodeToString(sum[:]), FetchedAt: fetchedAt}
	providers := make([]string, 0, len(root))
	for provider := range root {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		providerValue := root[provider]
		providerRecord, ok := providerValue.(map[string]any)
		if !ok {
			continue
		}
		models, ok := providerRecord["models"].(map[string]any)
		if !ok {
			continue
		}
		modelKeys := make([]string, 0, len(models))
		for model := range models {
			modelKeys = append(modelKeys, model)
		}
		sort.Strings(modelKeys)
		for _, model := range modelKeys {
			modelValue := models[model]
			modelRecord, ok := modelValue.(map[string]any)
			if !ok {
				continue
			}
			costRaw := billingImportRecordValue(modelRecord, "cost", "pricing")
			modelID := billingImportStringValue(modelRecord, "id")
			if modelID == "" {
				modelID = model
			}
			defaultCacheWrite := !strings.EqualFold(provider, "google") && !strings.HasPrefix(strings.ToLower(modelID), "gemini")
			cost, basePresence, configured := parseBillingModelPriceImportCostWithOptions(costRaw, defaultCacheWrite)
			entry := cluster.BillingModelPriceImportCatalogModel{Provider: provider, Model: modelID, Name: billingImportStringValue(modelRecord, "name"), Cost: cost, InvalidPriceFields: billingImportInvalidPriceFields(costRaw), UnsupportedDimensions: billingImportUnsupportedDimensions(costRaw)}
			if entry.Name == "" {
				entry.Name = entry.Model
			}
			if !configured {
				entry.Cost = nil
			}
			entry.ContextBands, entry.ContextBandIssues = parseBillingModelPriceImportContextBands(costRaw, basePresence)
			catalog.Models = append(catalog.Models, entry)
		}
	}
	if len(catalog.Models) == 0 {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("models.dev catalog has no models")
	}
	return catalog, nil
}

type billingModelPriceImportCostPresence struct {
	Input      bool
	Output     bool
	CacheRead  bool
	CacheWrite bool
	Request    bool
}

func parseBillingModelPriceImportCost(value any) (*cluster.BillingModelPriceImportCost, billingModelPriceImportCostPresence, bool) {
	return parseBillingModelPriceImportCostWithOptions(value, true)
}

func parseBillingModelPriceImportCostWithOptions(value any, defaultCacheWrite bool) (*cluster.BillingModelPriceImportCost, billingModelPriceImportCostPresence, bool) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, billingModelPriceImportCostPresence{}, false
	}
	input, hasInput := billingImportNumberValue(record, "input", "input_price", "inputPrice")
	output, hasOutput := billingImportNumberValue(record, "output", "output_price", "outputPrice")
	cacheRead, hasCacheRead := billingImportNumberValue(record, "cache_read", "cacheRead", "cache_read_price")
	cacheWrite, hasCacheWrite := billingImportNumberValue(record, "cache_write", "cacheWrite", "cache_write_price")
	request, hasRequest := billingImportNumberValue(record, "request", "request_price", "requestPrice")
	cw30m := billingImportOptionalNumberPtr(record, "cache_write_30m", "cache_write_30m_price")
	cw1h := billingImportOptionalNumberPtr(record, "cache_write_1h", "cache_write_1h_price", "input_cache_write_1h")
	cw12h := billingImportOptionalNumberPtr(record, "cache_write_12h", "cache_write_12h_price")
	reasoning := billingImportOptionalNumberPtr(record, "reasoning", "reasoning_price", "internal_reasoning")
	imgIn := billingImportOptionalNumberPtr(record, "image_input", "input_image", "image_input_price")
	imgOut := billingImportOptionalNumberPtr(record, "image_output", "output_image", "image_output_price")
	audioIn := billingImportOptionalNumberPtr(record, "audio_input", "input_audio", "audio", "audio_input_price")
	audioOut := billingImportOptionalNumberPtr(record, "audio_output", "output_audio", "audio_output_price")
	webSearch := billingImportOptionalNumberPtr(record, "web_search", "web_search_price")
	imagePrice := billingImportOptionalNumberPtr(record, "image", "image_price", "image_price_per_image")
	hasExtra := cw30m != nil || cw1h != nil || cw12h != nil || reasoning != nil || imgIn != nil || imgOut != nil || audioIn != nil || audioOut != nil || webSearch != nil || imagePrice != nil
	if !hasInput && !hasOutput && !hasCacheRead && !hasCacheWrite && !hasRequest && !hasExtra {
		return nil, billingModelPriceImportCostPresence{}, false
	}
	if hasInput {
		if !hasCacheRead {
			cacheRead = input * modelsDevDefaultCacheReadInputRatio
			hasCacheRead = true
		}
		if !hasCacheWrite && defaultCacheWrite {
			cacheWrite = input * modelsDevDefaultCacheWriteInputRatio
			hasCacheWrite = true
		}
	}
	return &cluster.BillingModelPriceImportCost{
		Input:         input,
		Output:        output,
		CacheRead:     cacheRead,
		CacheWrite:    cacheWrite,
		Request:       request,
		CacheWrite30m: cw30m,
		CacheWrite1h:  cw1h,
		CacheWrite12h: cw12h,
		Reasoning:     reasoning,
		ImageInput:    imgIn,
		ImageOutput:   imgOut,
		AudioInput:    audioIn,
		AudioOutput:   audioOut,
		WebSearch:     webSearch,
		Image:         imagePrice,
	}, billingModelPriceImportCostPresence{Input: hasInput, Output: hasOutput, CacheRead: hasCacheRead, CacheWrite: hasCacheWrite, Request: hasRequest}, true
}

func parseBillingModelPriceImportContextBands(value any, base billingModelPriceImportCostPresence) ([]cluster.BillingModelPriceImportContextBand, []string) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	var bands []cluster.BillingModelPriceImportContextBand
	var issues []string
	if rawTiers, exists := record["tiers"]; exists {
		switch tiers := rawTiers.(type) {
		case []any:
			for _, item := range tiers {
				itemRecord, ok := item.(map[string]any)
				if !ok {
					issues = append(issues, "invalid_context_band")
					continue
				}
				tier, ok := itemRecord["tier"].(map[string]any)
				if !ok {
					issues = append(issues, "invalid_context_band_metadata")
					continue
				}
				size, validSize := billingImportNumberValue(tier, "size")
				if !validSize || size < 0 || math.Trunc(size) != size {
					issues = append(issues, "invalid_context_band_boundary")
					continue
				}
				cost, presence, configured := parseBillingModelPriceImportCost(itemRecord)
				invalidFields := billingImportInvalidPriceFields(itemRecord)
				if !configured {
					if len(invalidFields) > 0 {
						issues = append(issues, "invalid_context_band_prices")
					} else {
						issues = append(issues, "missing_context_band_prices")
					}
					continue
				}
				band := cluster.BillingModelPriceImportContextBand{MinInputTokens: int64(size) + 1, Cost: *cost, InvalidPriceFields: invalidFields, UnsupportedDimensions: billingImportUnsupportedDimensions(itemRecord), MissingPriceFields: billingImportMissingTierPriceFields(base, presence)}
				if tierType := strings.TrimSpace(billingImportStringValue(tier, "type")); tierType != "context" {
					band.UnsupportedDimensions = append(band.UnsupportedDimensions, "unsupported_tier_type")
				}
				bands = append(bands, band)
			}
		case map[string]any:
			for boundary, value := range tiers {
				upper, errBoundary := strconv.ParseInt(strings.TrimSpace(boundary), 10, 64)
				if errBoundary != nil || upper < 0 {
					issues = append(issues, "invalid_context_band_boundary")
					continue
				}
				if nested, ok := value.(map[string]any); ok {
					if wrapped := billingImportRecordValue(nested, "cost", "pricing"); wrapped != nil {
						value = wrapped
					}
				}
				cost, presence, configured := parseBillingModelPriceImportCost(value)
				bandRecord, _ := value.(map[string]any)
				invalidFields := billingImportInvalidPriceFields(bandRecord)
				if !configured {
					if len(invalidFields) > 0 {
						issues = append(issues, "invalid_context_band_prices")
					} else {
						issues = append(issues, "missing_context_band_prices")
					}
					continue
				}
				bands = append(bands, cluster.BillingModelPriceImportContextBand{MinInputTokens: upper + 1, Cost: *cost, InvalidPriceFields: invalidFields, UnsupportedDimensions: billingImportUnsupportedDimensions(bandRecord), MissingPriceFields: billingImportMissingTierPriceFields(base, presence)})
			}
		case nil:
		default:
			issues = append(issues, "invalid_context_bands")
		}
	}
	if len(bands) == 0 && len(issues) == 0 {
		if over, ok := record["context_over_200k"]; ok {
			cost, presence, configured := parseBillingModelPriceImportCost(over)
			overRecord, _ := over.(map[string]any)
			invalidFields := billingImportInvalidPriceFields(overRecord)
			if !configured {
				if len(invalidFields) > 0 {
					issues = append(issues, "invalid_context_band_prices")
				} else {
					issues = append(issues, "missing_context_band_prices")
				}
			} else {
				bands = append(bands, cluster.BillingModelPriceImportContextBand{MinInputTokens: 200001, Cost: *cost, InvalidPriceFields: invalidFields, UnsupportedDimensions: billingImportUnsupportedDimensions(overRecord), MissingPriceFields: billingImportMissingTierPriceFields(base, presence)})
			}
		}
	}
	return bands, billingImportUniqueStrings(issues)
}

func billingImportMissingTierPriceFields(base, tier billingModelPriceImportCostPresence) []string {
	missing := make([]string, 0, 5)
	for _, field := range []struct {
		name string
		base bool
		tier bool
	}{
		{name: "input", base: base.Input, tier: tier.Input},
		{name: "output", base: base.Output, tier: tier.Output},
		{name: "cache_read", base: base.CacheRead, tier: tier.CacheRead},
		{name: "cache_write", base: base.CacheWrite, tier: tier.CacheWrite},
		{name: "request", base: base.Request, tier: tier.Request},
	} {
		if field.base && !field.tier {
			missing = append(missing, field.name)
		}
	}
	return missing
}

func billingImportInvalidPriceFields(value any) []string {
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	fields := billingImportPriceFields()
	var invalid []string
	for _, field := range fields {
		name, aliases := field.name, field.aliases
		if billingImportHasMalformedNumber(record, aliases...) {
			invalid = append(invalid, name)
			continue
		}
		value, exists := billingImportNumberValue(record, aliases...)
		if exists && (!isFiniteBillingImportNumber(value) || value < 0) {
			invalid = append(invalid, name)
		}
	}
	return invalid
}

func billingImportUnsupportedDimensions(value any) []string {
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	var unsupported []string
	for _, field := range []string{"video"} {
		raw, exists := record[field]
		if !exists || raw == nil {
			continue
		}
		value, exists := billingImportNumberValue(record, field)
		if !exists || !isFiniteBillingImportNumber(value) || value != 0 {
			unsupported = append(unsupported, field)
		}
	}
	return unsupported
}

func billingImportOptionalNumberPtr(record map[string]any, keys ...string) *float64 {
	value, ok := billingImportNumberValue(record, keys...)
	if !ok || !isFiniteBillingImportNumber(value) || value < 0 {
		return nil
	}
	rounded := math.Round(value*1e8) / 1e8
	return &rounded
}

func billingImportPerTokenToPerMillionPtr(record map[string]any, keys ...string) *float64 {
	value, ok := billingImportNumberValue(record, keys...)
	if !ok || !isFiniteBillingImportNumber(value) || value < 0 {
		return nil
	}
	scaled := math.Round(value*1e6*1e8) / 1e8
	return &scaled
}

func parseOpenRouterBillingImportCatalog(raw []byte, sourceURL string, fetchedAt time.Time) (cluster.BillingModelPriceImportCatalog, error) {
	var root struct {
		Data []map[string]any `json:"data"`
	}
	if errDecode := json.Unmarshal(raw, &root); errDecode != nil {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("decode openrouter catalog: %w", errDecode)
	}
	if len(root.Data) == 0 {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("openrouter catalog has no models")
	}
	sum := sha256.Sum256(raw)
	catalog := cluster.BillingModelPriceImportCatalog{SourceURL: sourceURL, Version: hex.EncodeToString(sum[:]), FetchedAt: fetchedAt}
	for _, item := range root.Data {
		id := billingImportStringValue(item, "id")
		if id == "" || strings.HasSuffix(strings.ToLower(id), ":batch") {
			continue
		}
		provider := "openrouter"
		model := id
		if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
			provider = strings.ToLower(strings.TrimSpace(parts[0]))
			model = strings.TrimSpace(parts[1])
		}
		pricing, ok := item["pricing"].(map[string]any)
		if !ok {
			continue
		}
		cost, bands, okCost := parseOpenRouterModelPricing(provider, model, pricing)
		if !okCost {
			continue
		}
		name := billingImportStringValue(item, "name")
		if name == "" {
			name = id
		}
		catalog.Models = append(catalog.Models, cluster.BillingModelPriceImportCatalogModel{
			Provider:     provider,
			Model:        model,
			Name:         name,
			Cost:         cost,
			ContextBands: bands,
		})
	}
	if len(catalog.Models) == 0 {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("openrouter catalog has no valid models")
	}
	return catalog, nil
}

func parseOpenRouterModelPricing(provider, model string, pricing map[string]any) (*cluster.BillingModelPriceImportCost, []cluster.BillingModelPriceImportContextBand, bool) {
	promptPerM := billingImportPerTokenToPerMillionPtr(pricing, "prompt")
	compPerM := billingImportPerTokenToPerMillionPtr(pricing, "completion")
	if rawPrompt, ok := billingImportNumberValue(pricing, "prompt"); ok && rawPrompt < 0 {
		return nil, nil, false
	}
	if promptPerM == nil && compPerM == nil {
		return nil, nil, false
	}
	var input, output, cacheRead, cacheWrite, request float64
	if promptPerM != nil {
		input = *promptPerM
	}
	if compPerM != nil {
		output = *compPerM
	}
	if cr := billingImportPerTokenToPerMillionPtr(pricing, "input_cache_read"); cr != nil {
		cacheRead = *cr
	} else if promptPerM != nil {
		cacheRead = math.Round(input*modelsDevDefaultCacheReadInputRatio*1e8) / 1e8
	}
	isAnthropic := strings.EqualFold(provider, "anthropic") || strings.HasPrefix(strings.ToLower(model), "claude")
	if cw := billingImportPerTokenToPerMillionPtr(pricing, "input_cache_write"); cw != nil && isAnthropic {
		cacheWrite = *cw
	} else if promptPerM != nil && isAnthropic {
		cacheWrite = math.Round(input*modelsDevDefaultCacheWriteInputRatio*1e8) / 1e8
	}
	if req := billingImportOptionalNumberPtr(pricing, "request"); req != nil {
		request = *req
	}
	cost := &cluster.BillingModelPriceImportCost{
		Input:        input,
		Output:       output,
		CacheRead:    cacheRead,
		CacheWrite:   cacheWrite,
		Request:      request,
		CacheWrite1h: billingImportPerTokenToPerMillionPtr(pricing, "input_cache_write_1h"),
		Reasoning:    billingImportPerTokenToPerMillionPtr(pricing, "internal_reasoning"),
		ImageInput:   billingImportPerTokenToPerMillionPtr(pricing, "image"),
		ImageOutput:  billingImportPerTokenToPerMillionPtr(pricing, "image_output"),
		AudioInput:   billingImportPerTokenToPerMillionPtr(pricing, "audio"),
		AudioOutput:  billingImportPerTokenToPerMillionPtr(pricing, "audio_output"),
		WebSearch:    billingImportOptionalNumberPtr(pricing, "web_search"),
	}
	var bands []cluster.BillingModelPriceImportContextBand
	if rawOverrides, ok := pricing["overrides"].([]any); ok && len(rawOverrides) > 0 {
		if expr := buildOpenRouterTimeOverridesExpr(provider, model, cost, rawOverrides); expr != "" {
			cost.PricingExpr = expr
		} else {
			for _, rawOv := range rawOverrides {
				ov, ok := rawOv.(map[string]any)
				if !ok {
					continue
				}
				minPrompt, hasMin := billingImportNumberValue(ov, "min_prompt_tokens")
				if !hasMin || minPrompt <= 0 {
					continue
				}
				ovCost, _, okOv := parseOpenRouterModelPricing(provider, model, ov)
				if !okOv || ovCost == nil {
					continue
				}
				bands = append(bands, cluster.BillingModelPriceImportContextBand{
					MinInputTokens: int64(minPrompt) + 1,
					Cost:           *ovCost,
				})
			}
		}
	}
	return cost, bands, true
}

func buildOpenRouterTimeOverridesExpr(provider, model string, base *cluster.BillingModelPriceImportCost, rawOverrides []any) string {
	type branch struct {
		cond string
		expr string
	}
	var branches []branch
	for _, rawOv := range rawOverrides {
		ov, ok := rawOv.(map[string]any)
		if !ok {
			continue
		}
		_, hasStart := ov["utc_start"]
		_, hasEnd := ov["utc_end"]
		_, hasDays := ov["utc_days"]
		if !hasStart && !hasEnd && !hasDays {
			return ""
		}
		ovCost, _, okOv := parseOpenRouterModelPricing(provider, model, ov)
		if !okOv || ovCost == nil {
			continue
		}
		cond := openRouterOverrideTimeCondition(ov)
		if cond == "" {
			continue
		}
		branches = append(branches, branch{cond: cond, expr: openRouterLinearTokenExpr(ovCost)})
	}
	if len(branches) == 0 {
		return ""
	}
	result := fmt.Sprintf("tier('base', %s)", openRouterLinearTokenExpr(base))
	for i := len(branches) - 1; i >= 0; i-- {
		result = fmt.Sprintf("(%s ? tier('override_%d', %s) : %s)", branches[i].cond, i+1, branches[i].expr, result)
	}
	return result
}

func openRouterOverrideTimeCondition(ov map[string]any) string {
	var parts []string
	if rawDays, ok := ov["utc_days"].([]any); ok && len(rawDays) > 0 {
		dayMap := map[string]int{
			"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3,
			"thursday": 4, "friday": 5, "saturday": 6,
		}
		var nums []int
		for _, d := range rawDays {
			if s, ok := d.(string); ok {
				if n, exists := dayMap[strings.ToLower(strings.TrimSpace(s))]; exists {
					nums = append(nums, n)
				}
			}
		}
		if len(nums) == 5 && nums[0] == 1 && nums[4] == 5 {
			parts = append(parts, "(weekday('UTC') >= 1 && weekday('UTC') <= 5)")
		} else if len(nums) > 0 {
			var dConds []string
			for _, n := range nums {
				dConds = append(dConds, fmt.Sprintf("weekday('UTC') == %d", n))
			}
			parts = append(parts, "("+strings.Join(dConds, " || ")+")")
		}
	}
	start, hasStart := billingImportNumberValue(ov, "utc_start")
	end, hasEnd := billingImportNumberValue(ov, "utc_end")
	if hasStart && hasEnd {
		sH := int(start) / 100
		eH := int(end) / 100
		if eH == 0 {
			eH = 24
		}
		if sH < eH {
			parts = append(parts, fmt.Sprintf("(hour('UTC') >= %d && hour('UTC') < %d)", sH, eH))
		} else if sH > eH {
			parts = append(parts, fmt.Sprintf("(hour('UTC') >= %d || hour('UTC') < %d)", sH, eH))
		}
	}
	return strings.Join(parts, " && ")
}

func openRouterLinearTokenExpr(c *cluster.BillingModelPriceImportCost) string {
	priceOr := func(price *float64, fallback float64) float64 {
		if price != nil {
			return *price
		}
		return fallback
	}
	cacheWrite := c.CacheWrite
	if cacheWrite <= 0 {
		cacheWrite = c.Input
	}
	cacheWrite30m := priceOr(c.CacheWrite30m, cacheWrite)
	// Variable references are collected across all branches before folding.
	// Emit every token dimension in each branch, including explicit zero prices
	// and parent fallbacks, so a reference in another branch cannot drop tokens.
	var terms []string
	for _, bucket := range []struct {
		name  string
		price float64
	}{
		{"p", c.Input},
		{"c", c.Output},
		{"cr", c.CacheRead},
		{"cc", cacheWrite},
		{"cc30m", cacheWrite30m},
		{"cc1h", priceOr(c.CacheWrite1h, cacheWrite)},
		{"cc12h", priceOr(c.CacheWrite12h, priceOr(c.CacheWrite1h, cacheWrite30m))},
		{"r", priceOr(c.Reasoning, c.Output)},
		{"img", priceOr(c.ImageInput, c.Input)},
		{"imgo", priceOr(c.ImageOutput, c.Output)},
		{"ai", priceOr(c.AudioInput, c.Input)},
		{"ao", priceOr(c.AudioOutput, c.Output)},
	} {
		terms = append(terms, fmt.Sprintf("%s * %g", bucket.name, bucket.price))
	}
	if c.Request > 0 {
		terms = append(terms, fmt.Sprintf("req * usd(%g)", c.Request))
	}
	return strings.Join(terms, " + ")
}

func parseLiteLLMBillingImportCatalog(raw []byte, sourceURL string, fetchedAt time.Time) (cluster.BillingModelPriceImportCatalog, error) {
	var root map[string]any
	if errDecode := json.Unmarshal(raw, &root); errDecode != nil {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("decode litellm catalog: %w", errDecode)
	}
	sum := sha256.Sum256(raw)
	catalog := cluster.BillingModelPriceImportCatalog{SourceURL: sourceURL, Version: hex.EncodeToString(sum[:]), FetchedAt: fetchedAt}
	keys := make([]string, 0, len(root))
	for k := range root {
		if k == "sample_spec" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rec, ok := root[key].(map[string]any)
		if !ok {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(billingImportStringValue(rec, "litellm_provider")))
		model := strings.TrimSpace(key)
		if parts := strings.SplitN(key, "/", 2); len(parts) == 2 {
			if provider == "" {
				provider = strings.ToLower(strings.TrimSpace(parts[0]))
			}
			model = strings.TrimSpace(parts[1])
		}
		if provider == "" {
			provider = "openai"
		}
		inPtr := billingImportPerTokenToPerMillionPtr(rec, "input_cost_per_token")
		outPtr := billingImportPerTokenToPerMillionPtr(rec, "output_cost_per_token")
		imgPrice := billingImportOptionalNumberPtr(rec, "output_cost_per_image", "input_cost_per_image")
		wsPrice := billingImportOptionalNumberPtr(rec, "search_context_cost_per_query")
		if inPtr == nil && outPtr == nil && imgPrice == nil && wsPrice == nil {
			continue
		}
		var input, output, cacheRead, cacheWrite float64
		if inPtr != nil {
			input = *inPtr
		}
		if outPtr != nil {
			output = *outPtr
		}
		if cr := billingImportPerTokenToPerMillionPtr(rec, "cache_read_input_token_cost"); cr != nil {
			cacheRead = *cr
		} else if inPtr != nil {
			cacheRead = math.Round(input*modelsDevDefaultCacheReadInputRatio*1e8) / 1e8
		}
		isAnthropic := strings.Contains(strings.ToLower(provider), "anthropic") || strings.HasPrefix(strings.ToLower(model), "claude")
		if cw := billingImportPerTokenToPerMillionPtr(rec, "cache_creation_input_token_cost"); cw != nil && isAnthropic {
			cacheWrite = *cw
		} else if inPtr != nil && isAnthropic {
			cacheWrite = math.Round(input*modelsDevDefaultCacheWriteInputRatio*1e8) / 1e8
		}
		cost := &cluster.BillingModelPriceImportCost{
			Input:         input,
			Output:        output,
			CacheRead:     cacheRead,
			CacheWrite:    cacheWrite,
			CacheWrite1h:  billingImportPerTokenToPerMillionPtr(rec, "cache_creation_input_token_cost_above_1hr"),
			CacheWrite12h: billingImportPerTokenToPerMillionPtr(rec, "cache_creation_input_token_cost_above_12hr"),
			Reasoning:     billingImportPerTokenToPerMillionPtr(rec, "output_cost_per_reasoning_token"),
			ImageInput:    billingImportPerTokenToPerMillionPtr(rec, "input_cost_per_image_token"),
			ImageOutput:   billingImportPerTokenToPerMillionPtr(rec, "output_cost_per_image_token"),
			AudioInput:    billingImportPerTokenToPerMillionPtr(rec, "input_cost_per_audio_token"),
			AudioOutput:   billingImportPerTokenToPerMillionPtr(rec, "output_cost_per_audio_token"),
			WebSearch:     wsPrice,
			Image:         imgPrice,
		}
		var bands []cluster.BillingModelPriceImportContextBand
		for _, tierSpec := range []struct {
			suffix string
			minTok int64
		}{
			{suffix: "_above_128k_tokens", minTok: 128001},
			{suffix: "_above_200k_tokens", minTok: 200001},
		} {
			tIn := billingImportPerTokenToPerMillionPtr(rec, "input_cost_per_token"+tierSpec.suffix)
			tOut := billingImportPerTokenToPerMillionPtr(rec, "output_cost_per_token"+tierSpec.suffix)
			if tIn == nil && tOut == nil {
				continue
			}
			bandCost := *cost
			if tIn != nil {
				bandCost.Input = *tIn
				bandCost.CacheRead = math.Round(*tIn*modelsDevDefaultCacheReadInputRatio*1e8) / 1e8
				bandCost.CacheWrite = math.Round(*tIn*modelsDevDefaultCacheWriteInputRatio*1e8) / 1e8
			}
			if tOut != nil {
				bandCost.Output = *tOut
			}
			if tCR := billingImportPerTokenToPerMillionPtr(rec, "cache_read_input_token_cost"+tierSpec.suffix); tCR != nil {
				bandCost.CacheRead = *tCR
			}
			if tCW := billingImportPerTokenToPerMillionPtr(rec, "cache_creation_input_token_cost"+tierSpec.suffix); tCW != nil {
				bandCost.CacheWrite = *tCW
			}
			bands = append(bands, cluster.BillingModelPriceImportContextBand{
				MinInputTokens: tierSpec.minTok,
				Cost:           bandCost,
			})
		}
		catalog.Models = append(catalog.Models, cluster.BillingModelPriceImportCatalogModel{
			Provider:     provider,
			Model:        model,
			Name:         key,
			Cost:         cost,
			ContextBands: bands,
		})
	}
	if len(catalog.Models) == 0 {
		return cluster.BillingModelPriceImportCatalog{}, fmt.Errorf("litellm catalog has no valid models")
	}
	return catalog, nil
}

func (h *Handler) discoverBillingImportTargetsWithRegistry(ctx context.Context) ([]cluster.BillingModelPriceImportTarget, error) {
	targets, errTargets := h.repo.DiscoverBillingImportTargets(ctx)
	if errTargets != nil {
		return nil, errTargets
	}
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		seen[strings.ToLower(strings.TrimSpace(t.Provider))+"::"+strings.TrimSpace(t.Model)] = struct{}{}
	}
	for _, def := range registry.GetGlobalRegistry().GetAvailableModelDefinitions() {
		if def == nil {
			continue
		}
		modelID := strings.TrimSpace(def.ID)
		if modelID == "" || strings.Contains(modelID, "*") {
			continue
		}
		for _, rawProv := range def.Providers {
			prov := strings.ToLower(strings.TrimSpace(rawProv))
			if prov == "" || strings.Contains(prov, "*") {
				continue
			}
			key := prov + "::" + modelID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			targets = append(targets, cluster.BillingModelPriceImportTarget{
				Provider:       prov,
				Model:          modelID,
				ServiceTier:    cluster.BillingServiceTierWildcard,
				MinInputTokens: 0,
				Label:          modelID,
			})
		}
	}
	return targets, nil
}

func canonicalBillingImportProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "x-ai":
		return "xai"
	case "google-vertex", "vertex_ai":
		return "vertex"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func mergeBillingImportCatalogs(catalogs []cluster.BillingModelPriceImportCatalog, fetchedAt time.Time) cluster.BillingModelPriceImportCatalog {
	out := cluster.BillingModelPriceImportCatalog{
		SourceURL: catalogs[0].SourceURL,
		Version:   catalogs[0].Version,
		FetchedAt: fetchedAt,
	}
	indexByKey := make(map[string]int)
	for _, cat := range catalogs {
		for _, m := range cat.Models {
			key := canonicalBillingImportProvider(m.Provider) + "::" + strings.ToLower(strings.TrimSpace(m.Model))
			if idx, exists := indexByKey[key]; exists {
				mergeBillingImportCatalogModel(&out.Models[idx], m)
				continue
			}
			indexByKey[key] = len(out.Models)
			out.Models = append(out.Models, m)
		}
	}
	return out
}

func mergeBillingImportCatalogModel(dst *cluster.BillingModelPriceImportCatalogModel, src cluster.BillingModelPriceImportCatalogModel) {
	if dst == nil || src.Cost == nil {
		return
	}
	if dst.Cost == nil {
		dst.Cost = src.Cost
		dst.ContextBands = src.ContextBands
		return
	}
	fillPtr := func(target **float64, candidate *float64) {
		if *target == nil && candidate != nil {
			v := *candidate
			*target = &v
		}
	}
	fillPtr(&dst.Cost.CacheWrite30m, src.Cost.CacheWrite30m)
	fillPtr(&dst.Cost.CacheWrite1h, src.Cost.CacheWrite1h)
	fillPtr(&dst.Cost.CacheWrite12h, src.Cost.CacheWrite12h)
	fillPtr(&dst.Cost.Reasoning, src.Cost.Reasoning)
	fillPtr(&dst.Cost.ImageInput, src.Cost.ImageInput)
	fillPtr(&dst.Cost.ImageOutput, src.Cost.ImageOutput)
	fillPtr(&dst.Cost.AudioInput, src.Cost.AudioInput)
	fillPtr(&dst.Cost.AudioOutput, src.Cost.AudioOutput)
	fillPtr(&dst.Cost.WebSearch, src.Cost.WebSearch)
	fillPtr(&dst.Cost.Image, src.Cost.Image)
	if dst.Cost.PricingExpr == "" && src.Cost.PricingExpr != "" {
		dst.Cost.PricingExpr = src.Cost.PricingExpr
	}
	if len(dst.ContextBands) == 0 && len(src.ContextBands) > 0 {
		dst.ContextBands = src.ContextBands
	}
	for i := range dst.ContextBands {
		fillPtr(&dst.ContextBands[i].Cost.WebSearch, src.Cost.WebSearch)
		fillPtr(&dst.ContextBands[i].Cost.Image, src.Cost.Image)
	}
}

func billingImportRecordValue(record map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := record[key]; ok {
			return value
		}
	}
	return nil
}
func billingImportStringValue(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := record[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func billingImportNumberValue(record map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, exists := record[key]
		if !exists || value == nil {
			continue
		}
		switch value := value.(type) {
		case float64:
			return value, true
		case int:
			return float64(value), true
		case int64:
			return float64(value), true
		case json.Number:
			parsed, err := value.Float64()
			if err == nil {
				return parsed, true
			}
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

type billingImportPriceField struct {
	name    string
	aliases []string
}

func billingImportPriceFields() []billingImportPriceField {
	return []billingImportPriceField{
		{name: "input", aliases: []string{"input", "input_price", "inputPrice"}},
		{name: "output", aliases: []string{"output", "output_price", "outputPrice"}},
		{name: "cache_read", aliases: []string{"cache_read", "cacheRead", "cache_read_price"}},
		{name: "cache_write", aliases: []string{"cache_write", "cacheWrite", "cache_write_price"}},
		{name: "request", aliases: []string{"request", "request_price", "requestPrice"}},
	}
}

func billingImportHasMalformedNumber(record map[string]any, keys ...string) bool {
	for _, key := range keys {
		value, exists := record[key]
		if !exists || value == nil {
			continue
		}
		if _, valid := billingImportNumberValue(map[string]any{key: value}, key); !valid {
			return true
		}
	}
	return false
}

func billingImportUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func isFiniteBillingImportNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
