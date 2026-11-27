package userapi

import (
	"maps"
	"slices"
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

// pricingFor quotes only providers that can serve the model.
func (index modelPriceIndex) pricingFor(candidates, providers []string) gin.H {
	if len(index) == 0 || len(candidates) == 0 {
		return gin.H{"status": pricingStatusUnpublished}
	}
	servable := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if name := strings.ToLower(strings.TrimSpace(provider)); name != "" {
			servable[name] = struct{}{}
		}
	}
	var providerPayloads []gin.H
	for _, provider := range slices.Sorted(maps.Keys(servable)) {
		ladders := cluster.BillingCatalogPriceTiers(index, provider, candidates)
		if len(ladders) == 0 {
			continue
		}
		tierPayloads := make([]gin.H, 0, len(ladders))
		for _, tier := range slices.Sorted(maps.Keys(ladders)) {
			rungs := make([]gin.H, 0, len(ladders[tier]))
			for _, rule := range ladders[tier] {
				rungs = append(rungs, modelPriceRungResponse(rule))
			}
			entry := gin.H{"service_tier": tier, "rungs": rungs}
			if tier == cluster.BillingServiceTierWildcard {
				entry["is_default"] = true
			}
			tierPayloads = append(tierPayloads, entry)
		}
		providerPayloads = append(providerPayloads, gin.H{"provider": provider, "tiers": tierPayloads})
	}
	if len(providerPayloads) == 0 {
		return gin.H{"status": pricingStatusUnpublished}
	}
	return gin.H{"status": pricingStatusPublished, "providers": providerPayloads}
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
