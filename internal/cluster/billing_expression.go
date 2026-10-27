package cluster

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/billingexpr"
)

// billingExpressionTokens maps usage to expression token variables. Complete v2
// accounting carves detail buckets out of their parents; legacy accounting only
// exposes input, cache, and output buckets.
func billingExpressionTokens(usage *UsageRecord) map[string]float64 {
	unitDetails := billingTokenDetailsForUsage(usage)
	numbers := map[string]float64{
		billingexpr.VarRequest:        1,
		billingexpr.VarContextLength:  float64(billingContextInputTokens(usage)),
		billingexpr.VarWebSearchCalls: float64(unitDetails.WebSearchCalls),
		billingexpr.VarImages:         float64(unitDetails.Images),
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
		numbers[billingexpr.VarInput] = float64(inputTokens)
		numbers[billingexpr.VarCacheRead] = float64(cacheReadTokens)
		numbers[billingexpr.VarCacheWrite] = float64(usage.CacheCreationTokens)
		numbers[billingexpr.VarOutput] = float64(usage.OutputTokens)
		return numbers
	}
	details := billingTokenDetailsForUsage(usage)
	uncached := breakdown.Input.UncachedTokens
	cacheWrite := breakdown.Input.CacheWriteTokens
	cacheWrite1h := clampBillingTokens(details.CacheWrite1h, cacheWrite)
	cacheWrite12h := clampBillingTokens(details.CacheWrite12h, cacheWrite-cacheWrite1h)
	imageInput := clampBillingTokens(details.ImageInput, uncached)
	audioInput := clampBillingTokens(details.AudioInput, uncached-imageInput)
	nonReasoning := breakdown.Output.NonReasoningTokens
	imageOutput := clampBillingTokens(details.ImageOutput, nonReasoning)
	audioOutput := clampBillingTokens(details.AudioOutput, nonReasoning-imageOutput)
	numbers[billingexpr.VarInput] = float64(uncached - imageInput - audioInput)
	numbers[billingexpr.VarImageInput] = float64(imageInput)
	numbers[billingexpr.VarAudioInput] = float64(audioInput)
	numbers[billingexpr.VarCacheRead] = float64(breakdown.Input.CacheReadTokens)
	numbers[billingexpr.VarCacheWrite] = float64(cacheWrite - cacheWrite1h - cacheWrite12h)
	numbers[billingexpr.VarCacheWrite1h] = float64(cacheWrite1h)
	numbers[billingexpr.VarCacheWrite12h] = float64(cacheWrite12h)
	numbers[billingexpr.VarOutput] = float64(nonReasoning - imageOutput - audioOutput)
	numbers[billingexpr.VarReasoning] = float64(breakdown.Output.ReasoningTokens)
	numbers[billingexpr.VarImageOutput] = float64(imageOutput)
	numbers[billingexpr.VarAudioOutput] = float64(audioOutput)
	return numbers
}

// billingExpressionChargeCost prices usage with a rule's pricing expression.
func billingExpressionChargeCost(cost *BillingCostBreakdown, usage *UsageRecord, snapshot BillingPriceSnapshot, expression string) error {
	program, errCompile := billingexpr.Compile(expression)
	if errCompile != nil {
		return fmt.Errorf("compile pricing_expr: %w", errCompile)
	}
	numbers := billingExpressionTokens(usage)
	program.FoldTokens(numbers)
	env := billingexpr.Env{
		Numbers: numbers,
		Strings: map[string]string{
			billingexpr.VarTier:     snapshot.EffectiveServiceTier,
			billingexpr.VarEffort:   strings.ToLower(strings.TrimSpace(usage.Effort)),
			billingexpr.VarModel:    strings.TrimSpace(usage.Model),
			billingexpr.VarProvider: strings.ToLower(strings.TrimSpace(usage.Provider)),
			billingexpr.VarEndpoint: strings.TrimSpace(usage.Endpoint),
		},
		Now: usage.Timestamp,
	}
	amount, errEval := program.EvalUSD(env)
	if errEval != nil {
		return fmt.Errorf("evaluate pricing_expr: %w", errEval)
	}
	// Record the folded buckets for audit; their cost is carried by the expression item.
	for name, tokens := range numbers {
		if name == billingexpr.VarRequest || name == billingexpr.VarContextLength || name == billingexpr.VarWebSearchCalls || name == billingexpr.VarImages || tokens <= 0 {
			continue
		}
		if cost.Items == nil {
			cost.Items = make(map[string]BillingCostItem)
		}
		cost.Items["expr:"+name] = BillingCostItem{Tokens: int64(tokens)}
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
	if program.Uses(billingexpr.VarImages) {
		details.Images = 0
	}
	cost.addSurcharges(details, snapshot)
	cost.Total = amount
	cost.PricingExpr = program.Source()
	return nil
}
