package cluster

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
)

// billingModelWildcard is the trailing wildcard accepted in price rule models,
// and the provider value that matches every provider.
const billingModelWildcard = "*"

// Price match levels recorded in price snapshots, from most to least specific.
const (
	BillingPriceMatchExact                   = "exact"
	BillingPriceMatchModelPattern            = "model_pattern"
	BillingPriceMatchAnyProvider             = "any_provider"
	BillingPriceMatchAnyProviderModelPattern = "any_provider_model_pattern"
)

type billingPriceMatchLevel struct {
	anyProvider bool
	pattern     bool
	name        string
}

var billingPriceMatchLevels = []billingPriceMatchLevel{
	{anyProvider: false, pattern: false, name: BillingPriceMatchExact},
	{anyProvider: false, pattern: true, name: BillingPriceMatchModelPattern},
	{anyProvider: true, pattern: false, name: BillingPriceMatchAnyProvider},
	{anyProvider: true, pattern: true, name: BillingPriceMatchAnyProviderModelPattern},
}

// validateBillingPricePattern accepts provider "*" and a single trailing "*" in model.
func validateBillingPricePattern(provider string, model string) error {
	provider = strings.TrimSpace(provider)
	if provider != billingModelWildcard && strings.Contains(provider, billingModelWildcard) {
		return fmt.Errorf("provider wildcard must be exactly %q", billingModelWildcard)
	}
	model = strings.TrimSpace(model)
	if strings.Contains(strings.TrimSuffix(model, billingModelWildcard), billingModelWildcard) {
		return fmt.Errorf("model wildcard %q is only allowed as a trailing suffix", billingModelWildcard)
	}
	return nil
}

// BillingModelMatches compares a rule model with a usage model case-insensitively.
// Pattern mode only accepts rules ending in "*" and matches by prefix.
func BillingModelMatches(ruleModel string, model string, pattern bool) bool {
	return billingModelMatches(ruleModel, model, pattern)
}

// BillingStripChannelPrefix returns the bare model name after a leading "prefix/"
// segment when present, or empty when the model has no prefix.
func BillingStripChannelPrefix(model string) string {
	return billingStripChannelPrefix(model)
}

func billingModelMatches(ruleModel string, model string, pattern bool) bool {
	ruleModel = strings.ToLower(strings.TrimSpace(ruleModel))
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	isPattern := strings.HasSuffix(ruleModel, billingModelWildcard)
	if !pattern {
		return !isPattern && ruleModel == model
	}
	return isPattern && strings.HasPrefix(model, strings.TrimSuffix(ruleModel, billingModelWildcard))
}

// billingFindPrice selects the most specific enabled rule. Specificity wins over
// candidate order: an exact model or alias beats any pattern, and a provider rule
// beats a "*" provider rule. Within one rule key an exact service tier beats the
// "*" tier, and the greatest context band not above contextInputTokens is used.
func billingFindPrice(ctx context.Context, tx *gorm.DB, provider string, candidates []billingPriceModelCandidate, tier string, contextInputTokens int64) (*BillingModelPriceRecord, string, string, error) {
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if name := strings.ToLower(strings.TrimSpace(candidate.model)); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, "", "", nil
	}
	var rules []BillingModelPriceRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).
		Where("enabled = ? AND provider IN ? AND (LOWER(model) IN ? OR model LIKE ?)", true, []string{provider, billingModelWildcard}, names, "%"+billingModelWildcard).
		Order("min_input_tokens DESC, id ASC").
		Find(&rules).Error; errFind != nil {
		return nil, "", "", errFind
	}
	price, source, level := billingMatchPrice(rules, provider, candidates, tier, contextInputTokens)
	return price, source, level, nil
}

// billingMatchPrice serves both the charge and each breakpoint of the published price ladder.
// Rules are ordered by min_input_tokens DESC, id ASC.
func billingMatchPrice(rules []BillingModelPriceRecord, provider string, candidates []billingPriceModelCandidate, tier string, contextInputTokens int64) (*BillingModelPriceRecord, string, string) {
	for _, level := range billingPriceMatchLevels {
		levelProvider := provider
		if level.anyProvider {
			levelProvider = billingModelWildcard
		}
		for _, candidate := range candidates {
			if price := billingSelectPriceRule(rules, levelProvider, candidate.model, level.pattern, tier, contextInputTokens); price != nil {
				return price, candidate.source, level.name
			}
		}
	}
	return nil, "", ""
}

// billingSelectPriceRule picks one rule for a provider and model at one match level.
// rules must be ordered by min_input_tokens DESC, id ASC.
func billingSelectPriceRule(rules []BillingModelPriceRecord, provider string, model string, pattern bool, tier string, contextInputTokens int64) *BillingModelPriceRecord {
	var best *BillingModelPriceRecord
	bestLength := -1
	for i := range rules {
		rule := &rules[i]
		if rule.Provider != provider || rule.MinInputTokens > contextInputTokens ||
			(rule.ServiceTier != tier && rule.ServiceTier != BillingServiceTierWildcard) || !billingModelMatches(rule.Model, model, pattern) {
			continue
		}
		length := len(strings.ToLower(strings.TrimSpace(rule.Model)))
		if length > bestLength || (length == bestLength && rule.ServiceTier == tier && best.ServiceTier != tier) {
			best, bestLength = rule, length
		}
	}
	return best
}

// BillingCatalogPriceTiers returns the effective rungs for each published service tier.
func BillingCatalogPriceTiers(records []BillingModelPriceRecord, provider string, names []string) map[string][]BillingModelPriceRecord {
	candidates := make([]billingPriceModelCandidate, 0, len(names))
	for _, name := range names {
		candidates = append(candidates, billingPriceModelCandidate{model: name})
	}
	var rules []BillingModelPriceRecord
	var thresholds []int64
	tiers := make(map[string]struct{})
	for i := range records {
		rule := &records[i]
		if rule.Provider != provider && rule.Provider != billingModelWildcard {
			continue
		}
		pattern := strings.HasSuffix(strings.TrimSpace(rule.Model), billingModelWildcard)
		if slices.ContainsFunc(names, func(name string) bool { return billingModelMatches(rule.Model, name, pattern) }) {
			rules = append(rules, *rule)
			tiers[rule.ServiceTier] = struct{}{}
			thresholds = append(thresholds, rule.MinInputTokens)
		}
	}
	slices.SortStableFunc(rules, func(a, b BillingModelPriceRecord) int {
		return cmp.Or(cmp.Compare(b.MinInputTokens, a.MinInputTokens), strings.Compare(a.ID, b.ID))
	})
	slices.Sort(thresholds)
	thresholds = slices.Compact(thresholds)
	out := make(map[string][]BillingModelPriceRecord, len(tiers))
	for tier := range tiers {
		var rungs []BillingModelPriceRecord
		var previous *BillingModelPriceRecord
		hasOwnRule := false
		for _, threshold := range thresholds {
			price, _, _ := billingMatchPrice(rules, provider, candidates, tier, threshold)
			if price == nil || price == previous {
				continue
			}
			rungs = append(rungs, *price)
			hasOwnRule = hasOwnRule || price.ServiceTier == tier
			previous = price
		}
		if hasOwnRule {
			out[tier] = rungs
		}
	}
	return out
}
