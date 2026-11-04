package userapi

import (
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// Pricing publication states. Price is not a scalar and its absence is not zero:
// a model with no enabled rule cannot be charged for, which means it cannot be
// sold, and reporting that as a price of 0 would advertise it as free.
const (
	pricingStatusPublished   = "published"
	pricingStatusUnpublished = "unpublished"
)

// modelPriceIndex holds enabled price rules so a catalog of any size costs one
// query rather than one query per model, while supporting provider="*" and
// trailing model wildcards with the same specificity order as billing.
type modelPriceIndex []cluster.BillingModelPriceRecord

// newModelPriceIndex stores enabled price records for catalog lookup.
func newModelPriceIndex(records []cluster.BillingModelPriceRecord) modelPriceIndex {
	if len(records) == 0 {
		return modelPriceIndex{}
	}
	out := make(modelPriceIndex, 0, len(records))
	for i := range records {
		if strings.TrimSpace(records[i].Model) == "" {
			continue
		}
		out = append(out, records[i])
	}
	return out
}

// selectProviderPriceRecords picks the most specific enabled rule group for one
// servable (provider, modelID) pair, matching billingFindPrice specificity:
// 1. exact provider + exact model
// 2. exact provider + longest model pattern
// 3. wildcard provider ("*") + exact model
// 4. wildcard provider ("*") + longest model pattern
func (index modelPriceIndex) selectProviderPriceRecords(provider, modelID string) []cluster.BillingModelPriceRecord {
	candidates := []string{modelID}
	if bare := cluster.BillingStripChannelPrefix(modelID); bare != "" && !strings.EqualFold(bare, modelID) {
		candidates = append(candidates, bare)
	}
	levels := []struct {
		ruleProvider string
		pattern      bool
	}{
		{ruleProvider: provider, pattern: false},
		{ruleProvider: provider, pattern: true},
		{ruleProvider: cluster.BillingServiceTierWildcard, pattern: false},
		{ruleProvider: cluster.BillingServiceTierWildcard, pattern: true},
	}
	for _, level := range levels {
		for _, candidate := range candidates {
			bestKey := ""
			for i := range index {
				rec := index[i]
				if !strings.EqualFold(strings.TrimSpace(rec.Provider), level.ruleProvider) {
					continue
				}
				if !cluster.BillingModelMatches(rec.Model, candidate, level.pattern) {
					continue
				}
				key := strings.ToLower(strings.TrimSpace(rec.Model))
				if bestKey == "" || len(key) > len(bestKey) {
					bestKey = key
				}
			}
			if bestKey == "" {
				continue
			}
			var matched []cluster.BillingModelPriceRecord
			for i := range index {
				rec := index[i]
				if strings.EqualFold(strings.TrimSpace(rec.Provider), level.ruleProvider) &&
					strings.EqualFold(strings.TrimSpace(rec.Model), bestKey) {
					matched = append(matched, rec)
				}
			}
			return matched
		}
	}
	return nil
}

// pricingFor builds the price ladder a buyer sees for one model.
//
// Only the providers that can actually serve the model are priced. A rule for
// some other provider may exist in the table — operators keep rules for channels
// they have since removed — and quoting it would promise a route the cluster
// cannot take. A model whose serving channels are unknown is therefore reported
// as unpublished, not as priced by everything.
func (index modelPriceIndex) pricingFor(modelID string, providers []string) gin.H {
	modelID = strings.TrimSpace(modelID)
	if len(index) == 0 || modelID == "" {
		return gin.H{"status": pricingStatusUnpublished}
	}

	servable := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		normalized := strings.ToLower(strings.TrimSpace(provider))
		if normalized == "" {
			continue
		}
		if _, ok := seenProviders[normalized]; ok {
			continue
		}
		seenProviders[normalized] = struct{}{}
		servable = append(servable, normalized)
	}
	if len(servable) == 0 {
		// Knowing of no channel that can serve the model is not the same as
		// being free to quote every channel in the table. It is the case where
		// the filter matters most: nothing here can be confirmed to name the
		// route a request would actually take, so nothing is quoted.
		return gin.H{"status": pricingStatusUnpublished}
	}

	// provider -> service tier -> rungs, preserving first-seen order so the
	// repository's ordering survives into the response.
	type tierLadder struct {
		tier  string
		rungs []cluster.BillingModelPriceRecord
	}
	providerOrder := make([]string, 0, len(servable))
	tiersByProvider := make(map[string][]*tierLadder, len(servable))

	for _, provider := range servable {
		records := index.selectProviderPriceRecords(provider, modelID)
		if len(records) == 0 {
			continue
		}
		providerOrder = append(providerOrder, provider)
		var ladders []*tierLadder
		for i := range records {
			tier := strings.TrimSpace(records[i].ServiceTier)
			if tier == "" {
				tier = cluster.BillingServiceTierWildcard
			}
			var target *tierLadder
			for _, ladder := range ladders {
				if ladder.tier == tier {
					target = ladder
					break
				}
			}
			if target == nil {
				target = &tierLadder{tier: tier}
				ladders = append(ladders, target)
			}
			target.rungs = append(target.rungs, records[i])
		}
		tiersByProvider[provider] = ladders
	}

	if len(providerOrder) == 0 {
		return gin.H{"status": pricingStatusUnpublished}
	}
	sort.Strings(providerOrder)

	providerPayloads := make([]gin.H, 0, len(providerOrder))
	for _, provider := range providerOrder {
		ladders := tiersByProvider[provider]
		tierPayloads := make([]gin.H, 0, len(ladders))
		for _, ladder := range ladders {
			// Rungs ascend by the input size that unlocks them, which is the
			// order a request climbs them: the last rung whose threshold the
			// request clears is the one that prices it.
			sort.SliceStable(ladder.rungs, func(a, b int) bool {
				return ladder.rungs[a].MinInputTokens < ladder.rungs[b].MinInputTokens
			})
			rungPayloads := make([]gin.H, 0, len(ladder.rungs))
			for i := range ladder.rungs {
				rungPayloads = append(rungPayloads, modelPriceRungResponse(ladder.rungs[i]))
			}
			tierPayload := gin.H{
				"service_tier": ladder.tier,
				"rungs":        rungPayloads,
			}
			// The wildcard tier is what a request gets when it names no tier or
			// names one nobody priced, so a client can label it as the default
			// instead of showing "*" to a buyer.
			if ladder.tier == cluster.BillingServiceTierWildcard {
				tierPayload["is_default"] = true
			}
			tierPayloads = append(tierPayloads, tierPayload)
		}
		providerPayloads = append(providerPayloads, gin.H{
			"provider": provider,
			"tiers":    tierPayloads,
		})
	}

	return gin.H{
		"status":    pricingStatusPublished,
		"providers": providerPayloads,
	}
}

// modelPriceRungResponse renders one rung of the ladder. Prices are per million
// tokens, matching how the rules are stored and charged; a zero component is
// emitted rather than omitted because zero is a real price here — a provider
// that does not bill for cache reads has stated that, and dropping the field
// would make it indistinguishable from a component nobody priced.
//
// The rule's own identifier, source and note stay behind: they are operator
// bookkeeping about how a price came to exist, not part of the quote, and the
// user contract puts them out of bounds.
func modelPriceRungResponse(record cluster.BillingModelPriceRecord) gin.H {
	rung := gin.H{
		"min_input_tokens":              record.MinInputTokens,
		"input_price_per_million":       record.InputPricePerMillion,
		"output_price_per_million":      record.OutputPricePerMillion,
		"cache_read_price_per_million":  record.CacheReadPricePerMillion,
		"cache_write_price_per_million": record.CacheWritePricePerMillion,
		"request_price":                 record.RequestPrice,
	}
	// Optional dimensions are quoted only when configured; an unset one bills at
	// its parent price, which the rung already shows.
	for name, value := range record.Values() {
		if value != nil {
			rung[name] = *value
		}
	}
	// A pricing expression replaces the column prices for this rung.
	if expression := strings.TrimSpace(record.PricingExpr); expression != "" {
		rung["pricing_expr"] = expression
	}
	return rung
}
