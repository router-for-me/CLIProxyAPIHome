package cluster

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/billingexpr"
	"github.com/tidwall/gjson"
)

// billingExpressionTokens maps usage to expression token variables. Complete v2
// accounting carves detail buckets out of their parents; legacy accounting only
// exposes input, cache, and output buckets.
func billingExpressionTokens(usage *UsageRecord) map[string]float64 {
	unitDetails := billingTokenDetailsForUsage(usage)
	numbers := map[string]float64{
		billingexpr.VarRequest:                1,
		billingexpr.VarContextLength:          float64(billingContextInputTokens(usage)),
		billingexpr.VarWebSearchCalls:         float64(unitDetails.WebSearchCalls),
		billingexpr.VarWebSearchQueries:       float64(unitDetails.WebSearchQueries),
		billingexpr.VarWebFetchCalls:          float64(unitDetails.WebFetchCalls),
		billingexpr.VarXSearchCalls:           float64(unitDetails.XSearchCalls),
		billingexpr.VarCodeExecutionCalls:     float64(unitDetails.CodeExecutionCalls),
		billingexpr.VarFileSearchCalls:        float64(unitDetails.FileSearchCalls),
		billingexpr.VarDocumentSearchCalls:    float64(unitDetails.DocumentSearchCalls),
		billingexpr.VarMCPCalls:               float64(unitDetails.MCPCalls),
		billingexpr.VarGoogleMapsChunks:       float64(unitDetails.GoogleMapsChunks),
		billingexpr.VarNumSourcesUsed:         float64(unitDetails.NumSourcesUsed),
		billingexpr.VarNumServerSideToolsUsed: float64(unitDetails.NumServerSideToolsUsed),
		billingexpr.VarPromptMedia:            float64(unitDetails.PromptMedia),
		billingexpr.VarCompletionMedia:        float64(unitDetails.CompletionMedia),
		billingexpr.VarPromptAudio:            float64(unitDetails.PromptAudio),
		billingexpr.VarImages:                 float64(unitDetails.Images),
		billingexpr.VarAudioSeconds:           unitDetails.AudioSeconds,
		billingexpr.VarUpstreamCost:           unitDetails.UpstreamCost,
		billingexpr.VarAcceptedPrediction:     float64(unitDetails.AcceptedPrediction),
		billingexpr.VarRejectedPrediction:     float64(unitDetails.RejectedPrediction),
	}
	if usage.Failed {
		numbers[billingexpr.VarRequest] = 0
	}
	breakdown := usageTokenBreakdownForRecord(usage)
	if breakdown.Quality != UsageTokenAccountingQualityComplete {
		inputTokens := usage.InputTokens
		cacheReadTokens := normalizedUsageCacheReadTokens(usage.Provider, usage.ExecutorType, usage.CachedTokens, usage.CacheReadTokens, usage.CacheReadTokensPresent)
		if billingCacheTokensIncludedInInput(usage) {
			inputTokens -= cacheReadTokens + usage.CacheCreationTokens
			if inputTokens < 0 {
				inputTokens = 0
			}
		}
		cachedText := clampBillingTokens(unitDetails.CachedText, cacheReadTokens)
		numbers[billingexpr.VarCachedText] = float64(cachedText)
		numbers[billingexpr.VarInput] = float64(inputTokens)
		numbers[billingexpr.VarCacheRead] = float64(cacheReadTokens - cachedText)
		numbers[billingexpr.VarCacheWrite] = float64(usage.CacheCreationTokens)
		numbers[billingexpr.VarOutput] = float64(usage.OutputTokens)
		numbers[billingexpr.VarTotalTokens] = float64(usage.TotalTokens)
		return numbers
	}
	details := unitDetails
	uncached := breakdown.Input.UncachedTokens
	cacheRead := breakdown.Input.CacheReadTokens
	cacheWrite := breakdown.Input.CacheWriteTokens
	cacheWrite30m := clampBillingTokens(details.CacheWrite30m, cacheWrite)
	cacheWrite1h := clampBillingTokens(details.CacheWrite1h, cacheWrite-cacheWrite30m)
	cacheWrite12h := clampBillingTokens(details.CacheWrite12h, cacheWrite-cacheWrite30m-cacheWrite1h)
	imageInput, audioInput, videoInput, fileInput, imageCacheRead, audioCacheRead, cachedText := billingCarveInputModalities(uncached, cacheRead, details)
	nonReasoning := breakdown.Output.NonReasoningTokens
	imageOutput := clampBillingTokens(details.ImageOutput, nonReasoning)
	audioOutput := clampBillingTokens(details.AudioOutput, nonReasoning-imageOutput)
	videoOutput := clampBillingTokens(details.VideoOutput, nonReasoning-imageOutput-audioOutput)
	numbers[billingexpr.VarInput] = float64(uncached - imageInput - audioInput - videoInput - fileInput)
	numbers[billingexpr.VarImageInput] = float64(imageInput)
	numbers[billingexpr.VarAudioInput] = float64(audioInput)
	numbers[billingexpr.VarVideoInput] = float64(videoInput)
	numbers[billingexpr.VarFileInput] = float64(fileInput)
	numbers[billingexpr.VarImageCacheRead] = float64(imageCacheRead)
	numbers[billingexpr.VarAudioCacheRead] = float64(audioCacheRead)
	numbers[billingexpr.VarCachedText] = float64(cachedText)
	numbers[billingexpr.VarCacheRead] = float64(cacheRead - imageCacheRead - audioCacheRead - cachedText)
	numbers[billingexpr.VarCacheWrite] = float64(cacheWrite - cacheWrite30m - cacheWrite1h - cacheWrite12h)
	numbers[billingexpr.VarCacheWrite30m] = float64(cacheWrite30m)
	numbers[billingexpr.VarCacheWrite1h] = float64(cacheWrite1h)
	numbers[billingexpr.VarCacheWrite12h] = float64(cacheWrite12h)
	numbers[billingexpr.VarOutput] = float64(nonReasoning - imageOutput - audioOutput - videoOutput)
	numbers[billingexpr.VarReasoning] = float64(breakdown.Output.ReasoningTokens)
	numbers[billingexpr.VarImageOutput] = float64(imageOutput)
	numbers[billingexpr.VarAudioOutput] = float64(audioOutput)
	numbers[billingexpr.VarVideoOutput] = float64(videoOutput)
	numbers[billingexpr.VarTotalTokens] = float64(breakdown.TotalTokens)
	return numbers
}

func billingExpressionStrings(usage *UsageRecord, snapshot BillingPriceSnapshot) map[string]string {
	payload := string(usage.PayloadJSON)
	pickLower := func(paths ...string) string {
		return strings.ToLower(strings.TrimSpace(usagePayloadString(payload, paths...)))
	}
	return map[string]string{
		billingexpr.VarTier:             snapshot.EffectiveServiceTier,
		billingexpr.VarUpstreamTier:     pickLower("upstream_service_tier", "response_service_tier"),
		billingexpr.VarSpeed:            pickLower("speed"),
		billingexpr.VarGeo:              pickLower("inference_geo"),
		billingexpr.VarCacheTTL:         pickLower("cache_details.ttl", "cache_details.retention", "tokens.cache_creation.ttl", "cache_creation.ttl"),
		billingexpr.VarImageSize:        pickLower("image_details.size", "image_details.image_size", "requested_image_details.size", "requested_image_details.image_size"),
		billingexpr.VarImageResolution:  pickLower("image_details.resolution", "image_details.image_size", "requested_image_details.resolution", "requested_image_details.image_size"),
		billingexpr.VarImageQuality:     pickLower("image_details.quality", "requested_image_details.quality"),
		billingexpr.VarImageAspectRatio: pickLower("image_details.aspect_ratio", "requested_image_details.aspect_ratio"),
		billingexpr.VarImageTier:        billingNormalizeImageSizeTier(pickLower("image_details.resolution", "image_details.image_size", "image_details.size", "requested_image_details.resolution", "requested_image_details.image_size", "requested_image_details.size")),
		billingexpr.VarBYOK:             pickLower("cost_details.is_byok", "is_byok"),
		billingexpr.VarResponseModel:    strings.TrimSpace(usagePayloadString(payload, "response_model")),
		billingexpr.VarEffort:           strings.ToLower(strings.TrimSpace(usage.Effort)),
		billingexpr.VarModel:            strings.TrimSpace(usage.Model),
		billingexpr.VarProvider:         strings.ToLower(strings.TrimSpace(usage.Provider)),
		billingexpr.VarEndpoint:         strings.TrimSpace(usage.Endpoint),
	}
}

// billingExpressionChargeCost prices usage with a rule's pricing expression.
func billingExpressionChargeCost(cost *BillingCostBreakdown, usage *UsageRecord, snapshot BillingPriceSnapshot, expression string) error {
	program, errCompile := billingexpr.Compile(expression)
	if errCompile != nil {
		return fmt.Errorf("compile pricing_expr: %w", errCompile)
	}
	numbers := billingExpressionTokens(usage)
	program.FoldTokens(numbers)
	payload := string(usage.PayloadJSON)
	rawTokens := string(usage.TokensJSON)
	env := billingexpr.Env{
		Numbers: numbers,
		Strings: billingExpressionStrings(usage, snapshot),
		LookupNumber: func(path string) float64 {
			path = strings.TrimSpace(path)
			if path == "" {
				return 0
			}
			if val := gjson.Get(payload, path); payload != "" && val.Exists() {
				return val.Float()
			}
			if val := gjson.Get(rawTokens, path); rawTokens != "" && val.Exists() {
				return val.Float()
			}
			return 0
		},
		LookupString: func(path string) string {
			path = strings.TrimSpace(path)
			if path == "" {
				return ""
			}
			if val := gjson.Get(payload, path); payload != "" && val.Exists() {
				return strings.TrimSpace(val.String())
			}
			if val := gjson.Get(rawTokens, path); rawTokens != "" && val.Exists() {
				return strings.TrimSpace(val.String())
			}
			return ""
		},
		Now: usage.Timestamp,
	}
	amount, matchedTier, errEval := program.EvalUSDWithTrace(env)
	if errEval != nil {
		return fmt.Errorf("evaluate pricing_expr: %w", errEval)
	}
	// Record the folded buckets and server-tool counters with their marginal USD contribution.
	for _, name := range []string{
		billingexpr.VarInput, billingexpr.VarImageInput, billingexpr.VarAudioInput,
		billingexpr.VarVideoInput, billingexpr.VarFileInput, billingexpr.VarImageCacheRead,
		billingexpr.VarAudioCacheRead, billingexpr.VarCachedText, billingexpr.VarCacheRead,
		billingexpr.VarCacheWrite, billingexpr.VarCacheWrite30m, billingexpr.VarCacheWrite1h,
		billingexpr.VarCacheWrite12h, billingexpr.VarOutput, billingexpr.VarReasoning,
		billingexpr.VarImageOutput, billingexpr.VarAudioOutput, billingexpr.VarVideoOutput,
		billingexpr.VarWebSearchCalls, billingexpr.VarWebSearchQueries, billingexpr.VarWebFetchCalls,
		billingexpr.VarXSearchCalls, billingexpr.VarCodeExecutionCalls, billingexpr.VarFileSearchCalls,
		billingexpr.VarDocumentSearchCalls, billingexpr.VarMCPCalls, billingexpr.VarGoogleMapsChunks,
		billingexpr.VarNumSourcesUsed, billingexpr.VarNumServerSideToolsUsed,
	} {
		tokens := numbers[name]
		if tokens <= 0 {
			continue
		}
		if cost.Items == nil {
			cost.Items = make(map[string]BillingCostItem)
		}
		item := BillingCostItem{Tokens: int64(tokens)}
		numbers[name] = 0
		withoutAmount, _, errWithout := program.EvalUSDWithTrace(env)
		numbers[name] = tokens
		if errWithout == nil && amount > withoutAmount {
			item.Cost = billingRoundAmount(amount - withoutAmount)
			if isBillingExpressionUnitVar(name) {
				item.PricePerMillion = billingRoundAmount(item.Cost / tokens)
			} else {
				item.PricePerMillion = billingRoundAmount(item.Cost * 1000000 / tokens)
			}
		}
		cost.Items["expr:"+name] = item
	}
	if cost.Items == nil {
		cost.Items = make(map[string]BillingCostItem)
	}
	cost.Items["expression"] = BillingCostItem{Cost: amount}
	// Per-unit counters the expression ignores still bill at the rule's unit prices.
	details := billingTokenDetailsForUsage(usage)
	if program.Uses(billingexpr.VarWebSearchCalls) {
		details.WebSearchCalls = 0
	}
	if program.Uses(billingexpr.VarXSearchCalls) {
		details.XSearchCalls = 0
	}
	if program.Uses(billingexpr.VarCodeExecutionCalls) {
		details.CodeExecutionCalls = 0
	}
	if program.Uses(billingexpr.VarImages) {
		details.Images = 0
	}
	cost.addSurcharges(details, snapshot)
	cost.Total = amount
	cost.PricingExpr = program.Source()
	cost.MatchedTier = matchedTier
	return nil
}

func isBillingExpressionUnitVar(name string) bool {
	switch name {
	case billingexpr.VarWebSearchCalls, billingexpr.VarWebSearchQueries, billingexpr.VarWebFetchCalls,
		billingexpr.VarXSearchCalls, billingexpr.VarCodeExecutionCalls, billingexpr.VarFileSearchCalls,
		billingexpr.VarDocumentSearchCalls, billingexpr.VarMCPCalls, billingexpr.VarGoogleMapsChunks,
		billingexpr.VarNumSourcesUsed, billingexpr.VarNumServerSideToolsUsed, billingexpr.VarImages:
		return true
	default:
		return false
	}
}

// billingNormalizeImageSizeTier maps raw image resolution or WxH dimensions to
// "1k" (max edge <= 1024), "2k" (<= 2048 or empty/"auto"), or "4k" (> 2048).
func billingNormalizeImageSizeTier(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "", "auto", "2k", "hd":
		return "2k"
	case "1k", "512", "512px", "standard", "sd":
		return "1k"
	case "4k", "ultra":
		return "4k"
	}
	if parts := strings.Split(raw, "x"); len(parts) == 2 {
		w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
		h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
		if errW == nil && errH == nil && w > 0 && h > 0 {
			maxEdge := w
			if h > maxEdge {
				maxEdge = h
			}
			if maxEdge <= 1024 {
				return "1k"
			}
			if maxEdge <= 2048 {
				return "2k"
			}
			return "4k"
		}
	}
	return raw
}
