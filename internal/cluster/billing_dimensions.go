package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// Optional price dimensions. A NULL price falls back to its parent price so a
// newly reported bucket is never billed below the ordinary rate; an explicit 0
// bills the bucket as free.
const (
	BillingPriceCacheWrite30m = "cache_write_30m_price_per_million"
	BillingPriceCacheWrite1h  = "cache_write_1h_price_per_million"
	BillingPriceCacheWrite12h = "cache_write_12h_price_per_million"
	BillingPriceReasoning     = "reasoning_price_per_million"
	BillingPriceImageInput    = "image_input_price_per_million"
	BillingPriceImageOutput   = "image_output_price_per_million"
	BillingPriceAudioInput    = "audio_input_price_per_million"
	BillingPriceAudioOutput   = "audio_output_price_per_million"
	// Per-unit surcharges in USD. They have no parent price, so NULL bills nothing.
	BillingPriceWebSearchCall = "web_search_price_per_call"
	BillingPriceImage         = "image_price_per_image"
)

// BillingExtraPrices holds optional per-dimension prices in USD per million tokens.
type BillingExtraPrices struct {
	CacheWrite30mPricePerMillion *float64 `gorm:"column:cache_write_30m_price_per_million" json:"cache_write_30m_price_per_million"`
	CacheWrite1hPricePerMillion  *float64 `gorm:"column:cache_write_1h_price_per_million" json:"cache_write_1h_price_per_million"`
	CacheWrite12hPricePerMillion *float64 `gorm:"column:cache_write_12h_price_per_million" json:"cache_write_12h_price_per_million"`
	ReasoningPricePerMillion     *float64 `gorm:"column:reasoning_price_per_million" json:"reasoning_price_per_million"`
	ImageInputPricePerMillion    *float64 `gorm:"column:image_input_price_per_million" json:"image_input_price_per_million"`
	ImageOutputPricePerMillion   *float64 `gorm:"column:image_output_price_per_million" json:"image_output_price_per_million"`
	AudioInputPricePerMillion    *float64 `gorm:"column:audio_input_price_per_million" json:"audio_input_price_per_million"`
	AudioOutputPricePerMillion   *float64 `gorm:"column:audio_output_price_per_million" json:"audio_output_price_per_million"`
	WebSearchPricePerCall        *float64 `gorm:"column:web_search_price_per_call" json:"web_search_price_per_call"`
	ImagePricePerImage           *float64 `gorm:"column:image_price_per_image" json:"image_price_per_image"`
}

// BillingExtraPriceNames returns the optional price field names in stable order.
func BillingExtraPriceNames() []string {
	return []string{
		BillingPriceCacheWrite30m,
		BillingPriceCacheWrite1h,
		BillingPriceCacheWrite12h,
		BillingPriceReasoning,
		BillingPriceImageInput,
		BillingPriceImageOutput,
		BillingPriceAudioInput,
		BillingPriceAudioOutput,
		BillingPriceWebSearchCall,
		BillingPriceImage,
	}
}

func (p *BillingExtraPrices) fieldPointers() map[string]**float64 {
	return map[string]**float64{
		BillingPriceCacheWrite30m: &p.CacheWrite30mPricePerMillion,
		BillingPriceCacheWrite1h:  &p.CacheWrite1hPricePerMillion,
		BillingPriceCacheWrite12h: &p.CacheWrite12hPricePerMillion,
		BillingPriceReasoning:     &p.ReasoningPricePerMillion,
		BillingPriceImageInput:    &p.ImageInputPricePerMillion,
		BillingPriceImageOutput:   &p.ImageOutputPricePerMillion,
		BillingPriceAudioInput:    &p.AudioInputPricePerMillion,
		BillingPriceAudioOutput:   &p.AudioOutputPricePerMillion,
		BillingPriceWebSearchCall: &p.WebSearchPricePerCall,
		BillingPriceImage:         &p.ImagePricePerImage,
	}
}

// Values returns the optional prices keyed by field name; unset prices are nil.
func (p BillingExtraPrices) Values() map[string]*float64 {
	out := make(map[string]*float64, 9)
	for name, field := range p.fieldPointers() {
		out[name] = *field
	}
	return out
}

// Set assigns one optional price by field name; nil clears it.
func (p *BillingExtraPrices) Set(name string, value *float64) error {
	field, ok := p.fieldPointers()[name]
	if !ok {
		return fmt.Errorf("unknown price field %q", name)
	}
	if value == nil {
		*field = nil
		return nil
	}
	copied := *value
	*field = &copied
	return nil
}

func validateBillingExtraPrices(values map[string]*float64) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	known := (&BillingExtraPrices{}).fieldPointers()
	for _, name := range names {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("unknown price field %q", name)
		}
		if value := values[name]; value != nil && *value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	return nil
}

func billingPriceOr(value *float64, fallback float64) float64 {
	if value != nil {
		return *value
	}
	return fallback
}

// billingTokenDetails are optional sub-buckets reported inside the usage tokens
// object or structured top-level usage fields. Each token bucket is a subset of
// a canonical v2 bucket and is clamped to it.
type billingTokenDetails struct {
	CacheWrite30m      int64 `json:"cache_write_30m_tokens,omitempty"`
	CacheWrite1h       int64 `json:"cache_write_1h_tokens,omitempty"`
	CacheWrite12h      int64 `json:"cache_write_12h_tokens,omitempty"`
	TextInput          int64 `json:"text_input_tokens,omitempty"`
	ImageInput         int64 `json:"image_input_tokens,omitempty"`
	ImageOutput        int64 `json:"image_output_tokens,omitempty"`
	AudioInput         int64 `json:"audio_input_tokens,omitempty"`
	AudioOutput        int64 `json:"audio_output_tokens,omitempty"`
	VideoInput         int64 `json:"video_input_tokens,omitempty"`
	VideoOutput        int64 `json:"video_output_tokens,omitempty"`
	FileInput          int64 `json:"file_input_tokens,omitempty"`
	ImageCacheRead     int64 `json:"image_cache_read_tokens,omitempty"`
	AudioCacheRead     int64 `json:"audio_cache_read_tokens,omitempty"`
	CachedText         int64 `json:"cached_text_tokens,omitempty"`
	AcceptedPrediction int64 `json:"accepted_prediction_tokens,omitempty"`
	RejectedPrediction int64 `json:"rejected_prediction_tokens,omitempty"`
	// Per-unit counters reported in the tokens object or at the payload top level.
	WebSearchCalls         int64   `json:"web_search_requests,omitempty"`
	WebSearchQueries       int64   `json:"web_search_queries,omitempty"`
	WebFetchCalls          int64   `json:"web_fetch_calls,omitempty"`
	XSearchCalls           int64   `json:"x_search_calls,omitempty"`
	CodeExecutionCalls     int64   `json:"code_execution_calls,omitempty"`
	FileSearchCalls        int64   `json:"file_search_calls,omitempty"`
	DocumentSearchCalls    int64   `json:"document_search_calls,omitempty"`
	MCPCalls               int64   `json:"mcp_calls,omitempty"`
	GoogleMapsChunks       int64   `json:"google_maps_chunks,omitempty"`
	NumSourcesUsed         int64   `json:"num_sources_used,omitempty"`
	NumServerSideToolsUsed int64   `json:"num_server_side_tools_used,omitempty"`
	PromptMedia            int64   `json:"prompt_media,omitempty"`
	CompletionMedia        int64   `json:"completion_media,omitempty"`
	PromptAudio            int64   `json:"prompt_audio,omitempty"`
	Images                 int64   `json:"image_count,omitempty"`
	AudioSeconds           float64 `json:"audio_seconds,omitempty"`
	UpstreamCost           float64 `json:"upstream_cost,omitempty"`
}

func (d billingTokenDetails) hasUnitActivity() bool {
	return d.WebSearchCalls > 0 || d.WebSearchQueries > 0 || d.WebFetchCalls > 0 ||
		d.XSearchCalls > 0 || d.CodeExecutionCalls > 0 || d.FileSearchCalls > 0 ||
		d.DocumentSearchCalls > 0 || d.MCPCalls > 0 || d.GoogleMapsChunks > 0 || d.NumSourcesUsed > 0 ||
		d.NumServerSideToolsUsed > 0 || d.PromptMedia > 0 || d.CompletionMedia > 0 ||
		d.PromptAudio > 0 || d.Images > 0 || d.AudioSeconds > 0 || d.UpstreamCost > 0
}

func billingTokenDetailsForUsage(usage *UsageRecord) billingTokenDetails {
	if usage == nil {
		return billingTokenDetails{}
	}
	raw := string(usage.TokensJSON)
	payload := string(usage.PayloadJSON)
	first := func(paths ...string) int64 {
		for _, path := range paths {
			if value := gjson.Get(raw, path); raw != "" && value.Exists() && value.Int() > 0 {
				return value.Int()
			}
			if value := gjson.Get(payload, path); payload != "" && value.Exists() && value.Int() > 0 {
				return value.Int()
			}
		}
		return 0
	}
	firstFloat := func(paths ...string) float64 {
		for _, path := range paths {
			if value := gjson.Get(raw, path); raw != "" && value.Exists() && value.Float() > 0 {
				return value.Float()
			}
			if value := gjson.Get(payload, path); payload != "" && value.Exists() && value.Float() > 0 {
				return value.Float()
			}
		}
		return 0
	}
	cw5m := first(
		"cache_creation_5m_tokens",
		"cache_write_5m_tokens",
		"cache_details.ephemeral_5m_input_tokens",
		"cache_details.by_ttl.5m",
		"cache_creation.ephemeral_5m_input_tokens",
	)
	cw30m := first(
		"cache_creation_30m_tokens",
		"cache_write_30m_tokens",
		"cache_details.ephemeral_30m_input_tokens",
		"cache_details.by_ttl.30m",
		"cache_creation.ephemeral_30m_input_tokens",
	)
	cw1h := first(
		"cache_creation_1h_tokens",
		"cache_write_1h_tokens",
		"cache_details.ephemeral_1h_input_tokens",
		"cache_details.by_ttl.1h",
		"cache_creation.ephemeral_1h_input_tokens",
	)
	cw12h := first(
		"cache_creation_12h_tokens",
		"cache_write_12h_tokens",
		"cache_details.ephemeral_12h_input_tokens",
		"cache_details.by_ttl.12h",
		"cache_creation.ephemeral_12h_input_tokens",
	)
	if cw5m == 0 && cw30m == 0 && cw1h == 0 && cw12h == 0 && payload != "" {
		totalWrite := usage.AccountingCacheWriteTokens
		if totalWrite <= 0 {
			totalWrite = usage.CacheCreationTokens
		}
		if totalWrite > 0 {
			switch strings.ToLower(strings.TrimSpace(usagePayloadString(payload, "cache_details.ttl", "cache_details.retention", "tokens.cache_creation.ttl", "cache_creation.ttl"))) {
			case "30m":
				cw30m = totalWrite
			case "1h":
				cw1h = totalWrite
			case "12h":
				cw12h = totalWrite
			}
		}
	}
	details := billingTokenDetails{
		CacheWrite30m:      cw30m,
		CacheWrite1h:       cw1h,
		CacheWrite12h:      cw12h,
		TextInput:          first("text_input_tokens", "input_text_tokens", "input_tokens_details.text_tokens", "prompt_tokens_details.text_tokens"),
		ImageInput:         first("image_input_tokens", "input_image_tokens", "input_tokens_details.image_tokens", "prompt_tokens_details.image_tokens"),
		ImageOutput:        first("image_output_tokens", "output_image_tokens", "output_tokens_details.image_tokens", "completion_tokens_details.image_tokens"),
		AudioInput:         first("audio_input_tokens", "input_audio_tokens", "input_tokens_details.audio_tokens", "prompt_tokens_details.audio_tokens"),
		AudioOutput:        first("audio_output_tokens", "output_audio_tokens", "output_tokens_details.audio_tokens", "completion_tokens_details.audio_tokens"),
		VideoInput:         first("video_input_tokens", "input_video_tokens", "input_tokens_details.video_tokens", "prompt_tokens_details.video_tokens"),
		VideoOutput:        first("video_output_tokens", "output_video_tokens", "output_tokens_details.video_tokens", "completion_tokens_details.video_tokens"),
		FileInput:          first("file_input_tokens", "input_file_tokens", "input_tokens_details.file_tokens", "prompt_tokens_details.file_tokens"),
		ImageCacheRead:     first("cached_image_tokens", "image_cache_read_tokens", "input_tokens_details.cached_tokens_details.image_tokens", "prompt_tokens_details.cached_tokens_details.image_tokens"),
		AudioCacheRead:     first("cached_audio_tokens", "audio_cache_read_tokens", "input_tokens_details.cached_tokens_details.audio_tokens", "prompt_tokens_details.cached_tokens_details.audio_tokens"),
		CachedText:         first("cached_text_tokens", "input_tokens_details.cached_tokens_details.text_tokens", "prompt_tokens_details.cached_tokens_details.text_tokens"),
		AcceptedPrediction: first("accepted_prediction_tokens", "output_tokens_details.accepted_prediction_tokens", "completion_tokens_details.accepted_prediction_tokens"),
		RejectedPrediction: first("rejected_prediction_tokens", "output_tokens_details.rejected_prediction_tokens", "completion_tokens_details.rejected_prediction_tokens"),
		UpstreamCost:       firstFloat("cost_details.cost", "cost_details.upstream_inference_cost", "cost"),
	}
	if details.UpstreamCost == 0 {
		if ticks := first("cost_details.cost_in_usd_ticks", "cost_in_usd_ticks"); ticks > 0 {
			details.UpstreamCost = float64(ticks) / 1e10
		} else {
			promptCost := firstFloat("cost_details.upstream_inference_prompt_cost")
			compCost := firstFloat("cost_details.upstream_inference_completions_cost")
			details.UpstreamCost = promptCost + compCost
		}
	}
	if usage.Failed {
		return details
	}
	details.WebSearchCalls = first(
		"web_search_requests",
		"tool_usage_details.web_search_calls",
		"server_tool_use.web_search_requests",
	)
	details.WebSearchQueries = first("tool_usage_details.web_search_queries")
	details.WebFetchCalls = first("tool_usage_details.web_fetch_calls", "server_tool_use.web_fetch_requests")
	details.XSearchCalls = first("tool_usage_details.x_search_calls")
	details.CodeExecutionCalls = first("tool_usage_details.code_execution_calls", "server_tool_use.code_execution_requests")
	details.FileSearchCalls = first("tool_usage_details.file_search_calls", "server_tool_use.file_search_requests")
	details.DocumentSearchCalls = first("tool_usage_details.document_search_calls")
	details.MCPCalls = first("tool_usage_details.mcp_calls", "server_tool_use.mcp_requests")
	details.GoogleMapsChunks = first("tool_usage_details.extra.google_maps_chunks", "google_maps_chunks")
	details.NumSourcesUsed = first("tool_usage_details.num_sources_used", "num_sources_used")
	details.NumServerSideToolsUsed = first("tool_usage_details.num_server_side_tools_used", "num_server_side_tools_used")
	details.PromptMedia = first("media_counts.prompt_media")
	details.CompletionMedia = first("media_counts.completion_media")
	details.PromptAudio = first("media_counts.prompt_audio")
	details.Images = first(
		"image_count",
		"images",
		"tool_usage_details.image_generation_calls",
	)
	details.AudioSeconds = firstFloat("media_counts.audio_seconds", "audio_seconds")
	// An images endpoint returns at least one image even when no count is known.
	if details.Images == 0 && billingImagesEndpoint(usage.Endpoint) {
		details.Images = 1
	}
	return details
}

// billingApplyAdvisorIterationsTx prices advisor_message entries in usage.iterations[]
// against their own model price rule and adds them to the parent charge breakdown.
// Standard "message" and "compaction" iterations are already summed in top-level usage.
func billingApplyAdvisorIterationsTx(ctx context.Context, tx *gorm.DB, usage *UsageRecord, cost *BillingCostBreakdown) error {
	if tx == nil || usage == nil || cost == nil {
		return nil
	}
	payload := strings.TrimSpace(string(usage.PayloadJSON))
	if payload == "" {
		return nil
	}
	iterations := gjson.Get(payload, "iterations")
	if !iterations.IsArray() {
		return nil
	}
	for _, item := range iterations.Array() {
		iterType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if iterType != "advisor_message" {
			continue
		}
		iterModel := strings.TrimSpace(item.Get("model").String())
		if iterModel == "" {
			iterModel = usage.Model
		}
		inputTokens := item.Get("input_tokens").Int()
		outputTokens := item.Get("output_tokens").Int()
		reasoningTokens := item.Get("reasoning_tokens").Int()
		if reasoningTokens == 0 {
			reasoningTokens = item.Get("thinking_tokens").Int()
		}
		cacheRead := item.Get("cache_read_input_tokens").Int()
		cacheWrite := item.Get("cache_creation_input_tokens").Int()
		cw5m := item.Get("cache_creation_5m_tokens").Int()
		if cw5m == 0 {
			cw5m = item.Get("cache_creation.ephemeral_5m_input_tokens").Int()
		}
		cw1h := item.Get("cache_creation_1h_tokens").Int()
		if cw1h == 0 {
			cw1h = item.Get("cache_creation.ephemeral_1h_input_tokens").Int()
		}
		nonReasoning := outputTokens
		if reasoningTokens > 0 && reasoningTokens <= outputTokens {
			nonReasoning = outputTokens - reasoningTokens
		}
		totalTokens := inputTokens + outputTokens + cacheRead + cacheWrite
		if totalTokens <= 0 {
			continue
		}
		iterPayload, _ := json.Marshal(map[string]any{
			"tokens": map[string]any{
				"cache_creation_5m_tokens": cw5m,
				"cache_creation_1h_tokens": cw1h,
			},
		})
		iterUsage := &UsageRecord{
			Timestamp:                  usage.Timestamp,
			Provider:                   usage.Provider,
			ExecutorType:               usage.ExecutorType,
			Model:                      iterModel,
			ServiceTier:                usage.ServiceTier,
			ResponseServiceTier:        usage.ResponseServiceTier,
			InputTokens:                inputTokens,
			OutputTokens:               outputTokens,
			ReasoningTokens:            reasoningTokens,
			CacheReadTokens:            cacheRead,
			CacheReadTokensPresent:     true,
			CacheCreationTokens:        cacheWrite,
			TotalTokens:                totalTokens,
			TokenAccountingVersion:     UsageTokenAccountingSchemaVersion,
			TokenAccountingQuality:     UsageTokenAccountingQualityComplete,
			AccountingTotalTokens:      totalTokens,
			AccountingInputTokens:      inputTokens + cacheRead + cacheWrite,
			UncachedInputTokens:        inputTokens,
			AccountingCacheReadTokens:  cacheRead,
			AccountingCacheWriteTokens: cacheWrite,
			AccountingOutputTokens:     outputTokens,
			NonReasoningOutputTokens:   nonReasoning,
			AccountingReasoningTokens:  reasoningTokens,
			TokensJSON:                 JSONB(gjson.GetBytes(iterPayload, "tokens").Raw),
			PayloadJSON:                JSONB(iterPayload),
		}
		_, iterSnapshot, errSnap := billingPriceSnapshotForUsage(ctx, tx, iterUsage)
		if errSnap != nil {
			return errSnap
		}
		iterSnapshot.RequestPrice = 0
		iterCost := billingChargeCost(iterUsage, iterSnapshot)
		if iterCost.BaseTotal <= 0 {
			continue
		}
		if cost.Items == nil {
			cost.Items = make(map[string]BillingCostItem)
		}
		key := "iteration:" + iterModel
		prev := cost.Items[key]
		cost.Items[key] = BillingCostItem{
			Tokens: prev.Tokens + totalTokens,
			Cost:   billingRoundAmount(prev.Cost + iterCost.BaseTotal),
		}
		cost.Surcharges = billingRoundAmount(cost.Surcharges + iterCost.BaseTotal)
		cost.BaseTotal = billingRoundAmount(cost.BaseTotal + iterCost.BaseTotal)
		cost.Total = cost.BaseTotal
	}
	return nil
}

// billingCarveInputModalities splits uncached and cache-read input tokens into
// disjoint modality buckets without double-counting cached media as uncached input.
func billingCarveInputModalities(uncached, cacheRead int64, details billingTokenDetails) (imageInput, audioInput, videoInput, fileInput, imageCacheRead, audioCacheRead, cachedText int64) {
	imageCacheRead = clampBillingTokens(details.ImageCacheRead, cacheRead)
	audioCacheRead = clampBillingTokens(details.AudioCacheRead, cacheRead-imageCacheRead)
	cachedText = clampBillingTokens(details.CachedText, cacheRead-imageCacheRead-audioCacheRead)

	rawImageInput := details.ImageInput
	if imageCacheRead > 0 {
		rawImageInput = max(rawImageInput-imageCacheRead, 0)
	} else if cacheRead > 0 && details.TextInput == 0 && rawImageInput > uncached && rawImageInput <= cacheRead-audioCacheRead-cachedText {
		// Upstreams such as OpenAI/DeepSeek Chat report total prompt image_tokens
		// alongside cached_tokens without cached_image_tokens. When the cached
		// prefix holds the image and exceeds uncached tokens, keep it in cache.
		imageCacheRead = rawImageInput
		rawImageInput = 0
	}
	rawAudioInput := details.AudioInput
	if audioCacheRead > 0 {
		rawAudioInput = max(rawAudioInput-audioCacheRead, 0)
	} else if cacheRead > 0 && details.TextInput == 0 && rawAudioInput > uncached && rawAudioInput <= cacheRead-imageCacheRead-cachedText {
		audioCacheRead = rawAudioInput
		rawAudioInput = 0
	}

	nonTextBudget := uncached
	if details.TextInput > 0 && details.CachedText > 0 && details.TextInput >= details.CachedText {
		uncachedText := clampBillingTokens(details.TextInput-details.CachedText, uncached)
		nonTextBudget = uncached - uncachedText
	}
	imageInput = clampBillingTokens(rawImageInput, nonTextBudget)
	audioInput = clampBillingTokens(rawAudioInput, nonTextBudget-imageInput)
	videoInput = clampBillingTokens(details.VideoInput, nonTextBudget-imageInput-audioInput)
	fileInput = clampBillingTokens(details.FileInput, nonTextBudget-imageInput-audioInput-videoInput)
	return
}

func clampBillingTokens(value int64, limit int64) int64 {
	if value < 0 || limit <= 0 {
		return 0
	}
	if value > limit {
		return limit
	}
	return value
}

// billingEffortMultiplier returns the normalized effort and its multiplier; a
// missing entry returns 0, which leaves the token cost unchanged.
func billingEffortMultiplier(effort string, multipliers map[string]float64) (string, float64) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" || len(multipliers) == 0 {
		return "", 0
	}
	multiplier, ok := multipliers[effort]
	if !ok {
		return "", 0
	}
	return effort, multiplier
}

func validateBillingEffortMultipliers(multipliers map[string]float64) error {
	for effort, multiplier := range multipliers {
		if strings.TrimSpace(effort) == "" {
			return fmt.Errorf("reasoning_effort_multipliers keys must be non-empty")
		}
		if multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
			return fmt.Errorf("reasoning_effort_multipliers[%s] must be a non-negative number", effort)
		}
	}
	return nil
}

// billingEffortMultipliersJSONB normalizes keys to lower case; an empty map stores NULL.
func billingEffortMultipliersJSONB(multipliers map[string]float64) (JSONB, error) {
	if errValidate := validateBillingEffortMultipliers(multipliers); errValidate != nil {
		return nil, errValidate
	}
	if len(multipliers) == 0 {
		return nil, nil
	}
	normalized := make(map[string]float64, len(multipliers))
	for effort, multiplier := range multipliers {
		normalized[strings.ToLower(strings.TrimSpace(effort))] = multiplier
	}
	return billingJSONB(normalized)
}

func billingEffortMultipliersFromJSONB(raw JSONB) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	multipliers := make(map[string]float64)
	if errUnmarshal := json.Unmarshal(raw, &multipliers); errUnmarshal != nil {
		return nil
	}
	return multipliers
}

// billingUserMultiplierTx returns the user's billing multiplier, defaulting to 1.
func billingUserMultiplierTx(ctx context.Context, tx *gorm.DB, userID uint) (float64, error) {
	user := &UserRecord{}
	result := tx.WithContext(contextOrBackground(ctx)).Select("id", "billing_multiplier").Where("id = ?", userID).Limit(1).Find(user)
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 0 || user.BillingMultiplier == nil {
		return 1, nil
	}
	return *user.BillingMultiplier, nil
}
