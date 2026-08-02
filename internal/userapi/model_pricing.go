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

// modelPriceIndex groups enabled price rules by lower-cased model identifier so
// a catalog of any size costs one query rather than one query per model.
type modelPriceIndex map[string][]cluster.BillingModelPriceRecord

// newModelPriceIndex buckets price records by model.
func newModelPriceIndex(records []cluster.BillingModelPriceRecord) modelPriceIndex {
	if len(records) == 0 {
		return modelPriceIndex{}
	}
	index := make(modelPriceIndex, len(records))
	for i := range records {
		key := strings.ToLower(strings.TrimSpace(records[i].Model))
		if key == "" {
			continue
		}
		index[key] = append(index[key], records[i])
	}
	return index
}

// pricingFor builds the price ladder a buyer sees for one model.
//
// Only the providers that can actually serve the model are priced. A rule for
// some other provider may exist in the table — operators keep rules for channels
// they have since removed — and quoting it would promise a route the cluster
// cannot take. A model whose serving channels are unknown is therefore reported
// as unpublished, not as priced by everything.
func (index modelPriceIndex) pricingFor(modelID string, providers []string) gin.H {
	records := index[strings.ToLower(strings.TrimSpace(modelID))]
	if len(records) == 0 {
		return gin.H{"status": pricingStatusUnpublished}
	}

	servable := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		normalized := strings.ToLower(strings.TrimSpace(provider))
		if normalized == "" {
			continue
		}
		servable[normalized] = struct{}{}
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
	providerOrder := make([]string, 0, len(records))
	tiersByProvider := make(map[string][]*tierLadder, len(records))

	for i := range records {
		provider := strings.ToLower(strings.TrimSpace(records[i].Provider))
		if provider == "" {
			continue
		}
		if _, ok := servable[provider]; !ok {
			continue
		}
		tier := strings.TrimSpace(records[i].ServiceTier)
		if tier == "" {
			tier = cluster.BillingServiceTierWildcard
		}

		ladders, known := tiersByProvider[provider]
		if !known {
			providerOrder = append(providerOrder, provider)
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
			tiersByProvider[provider] = ladders
		}
		target.rungs = append(target.rungs, records[i])
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
	return gin.H{
		"min_input_tokens":              record.MinInputTokens,
		"input_price_per_million":       record.InputPricePerMillion,
		"output_price_per_million":      record.OutputPricePerMillion,
		"cache_read_price_per_million":  record.CacheReadPricePerMillion,
		"cache_write_price_per_million": record.CacheWritePricePerMillion,
		"request_price":                 record.RequestPrice,
	}
}
