package cluster

import (
	"strings"

	"github.com/tidwall/gjson"
)

// billingOutboundServiceTier returns the effective_service_tier CPA reported for
// the final outbound request, or "" when the payload does not carry it.
func billingOutboundServiceTier(usage *UsageRecord) string {
	if usage == nil || len(usage.PayloadJSON) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(usage.PayloadJSON, "effective_service_tier").String())
}

// billingUsageConsumedTokens reports whether usage carries tokens the upstream
// processed, which a failed request still costs.
func billingUsageConsumedTokens(usage *UsageRecord) bool {
	if usage == nil {
		return false
	}
	if breakdown := usageTokenBreakdownForRecord(usage); breakdown.TotalTokens > 0 {
		return true
	}
	return usage.TotalTokens > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0 ||
		usage.CacheReadTokens > 0 || usage.CacheCreationTokens > 0 || usage.CachedTokens > 0
}

// billingFailedUsageSnapshot strips per-request and per-unit fees from a failed
// request: only the consumed tokens are billed.
func billingFailedUsageSnapshot(usage *UsageRecord, snapshot BillingPriceSnapshot) BillingPriceSnapshot {
	snapshot.RequestPrice = 0
	snapshot.WebSearchPricePerCall = nil
	snapshot.ImagePricePerImage = nil
	snapshot.FailedRequest = true
	snapshot.FailStatusCode = usage.FailStatusCode
	return snapshot
}

// billingImagesEndpoint reports whether usage came from an image generation or edit endpoint.
func billingImagesEndpoint(endpoint string) bool {
	endpoint = strings.ToLower(endpoint)
	return strings.Contains(endpoint, "/images/generations") || strings.Contains(endpoint, "/images/edits")
}
