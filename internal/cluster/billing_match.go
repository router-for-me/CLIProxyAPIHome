package cluster

import (
	"context"
	"fmt"
	"sort"
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
	for _, level := range billingPriceMatchLevels {
		levelProvider := provider
		if level.anyProvider {
			levelProvider = billingModelWildcard
		}
		for _, candidate := range candidates {
			if price := billingSelectPriceRule(rules, levelProvider, candidate.model, level.pattern, tier, contextInputTokens); price != nil {
				return price, candidate.source, level.name, nil
			}
		}
	}
	return nil, "", "", nil
}

// billingSelectPriceRule picks one rule for a provider and model at one match level.
// rules must be ordered by min_input_tokens DESC, id ASC.
func billingSelectPriceRule(rules []BillingModelPriceRecord, provider string, model string, pattern bool, tier string, contextInputTokens int64) *BillingModelPriceRecord {
	matching := make([]BillingModelPriceRecord, 0, len(rules))
	for _, rule := range rules {
		if rule.Provider == provider && rule.MinInputTokens <= contextInputTokens && billingModelMatches(rule.Model, model, pattern) {
			matching = append(matching, rule)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	// The longest pattern is the most specific; exact matches share one key.
	keys := make([]string, 0, len(matching))
	seen := make(map[string]struct{}, len(matching))
	for _, rule := range matching {
		key := strings.ToLower(strings.TrimSpace(rule.Model))
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		for _, candidateTier := range []string{tier, BillingServiceTierWildcard} {
			for index := range matching {
				rule := matching[index]
				if strings.ToLower(strings.TrimSpace(rule.Model)) == key && rule.ServiceTier == candidateTier {
					return &rule
				}
			}
		}
	}
	return nil
}
