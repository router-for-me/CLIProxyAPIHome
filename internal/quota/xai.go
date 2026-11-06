package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type xaiWeeklyEntitlement uint8

const (
	xaiEntitlementUnknown xaiWeeklyEntitlement = iota
	xaiEntitlementFree
	xaiEntitlementPaid
)

type xaiSubscription struct {
	entitlement xaiWeeklyEntitlement
	plan        *cluster.QuotaPlan
}

// xaiPercentage distinguishes an omitted percentage from null or malformed input.
type xaiPercentage struct {
	number  flexFloat
	present bool
}

func (p *xaiPercentage) UnmarshalJSON(data []byte) error {
	p.present = true
	return p.number.UnmarshalJSON(data)
}

func (p xaiPercentage) value() *float64 {
	value := flexFloatValue(p.number)
	if value == nil || *value < 0 || *value > 100 {
		return nil
	}
	return value
}

func xaiPercentageValue(primary, alternate xaiPercentage) *float64 {
	if primary.present && alternate.present {
		left, right := primary.value(), alternate.value()
		if left == nil || right == nil || *left != *right {
			return nil
		}
	}
	if primary.present {
		return primary.value()
	}
	return alternate.value()
}

func xaiSiblingEndpoint(billingURL, name string) string {
	target, errParse := url.Parse(billingURL)
	if errParse != nil || target.Scheme == "" || target.Host == "" {
		return ""
	}
	target.Path = path.Join(path.Dir(target.Path), name)
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	return target.String()
}

func xaiSubscriptionURL(profileURL string) string {
	target, errParse := url.Parse(profileURL)
	if errParse != nil {
		return ""
	}
	query := target.Query()
	query.Set("include", "subscription")
	target.RawQuery = query.Encode()
	return target.String()
}

func parseXAISubscription(body []byte) (xaiSubscription, error) {
	var payload map[string]json.RawMessage
	if errDecode := json.Unmarshal(body, &payload); errDecode != nil || payload == nil {
		return xaiSubscription{}, fmt.Errorf("xai profile response is not an object")
	}
	raw, exists := payload["subscriptionTier"]
	if _, alternateExists := payload["subscription_tier"]; exists && alternateExists {
		return xaiSubscription{}, fmt.Errorf("xai profile response has ambiguous subscription tier fields")
	}
	if !exists {
		raw, exists = payload["subscription_tier"]
	}
	if !exists {
		return xaiSubscription{}, fmt.Errorf("xai profile response is missing subscription tier")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return xaiSubscription{entitlement: xaiEntitlementFree, plan: &cluster.QuotaPlan{Name: "Free"}}, nil
	}
	var tier string
	if errDecode := json.Unmarshal(raw, &tier); errDecode != nil {
		return xaiSubscription{}, fmt.Errorf("xai subscription tier is not a string")
	}
	tier = strings.TrimSpace(tier)
	key := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(tier))
	key = strings.TrimPrefix(key, "subscriptiontier")
	var plan *cluster.QuotaPlan
	switch key {
	case "", "free", "none":
		return xaiSubscription{entitlement: xaiEntitlementFree, plan: &cluster.QuotaPlan{Name: "Free"}}, nil
	case "supergrok":
		plan = &cluster.QuotaPlan{Name: "SuperGrok"}
	case "supergrokplus":
		plan = &cluster.QuotaPlan{Name: "SuperGrok Plus", Premium: true}
	case "supergrokpro":
		plan = &cluster.QuotaPlan{Name: "SuperGrok Pro", Premium: true}
	case "supergrokheavy":
		plan = &cluster.QuotaPlan{Name: "SuperGrok Heavy", Premium: true}
	case "xpremiumplus":
		plan = &cluster.QuotaPlan{Name: "X Premium+", Premium: true}
	default:
		return xaiSubscription{plan: &cluster.QuotaPlan{Name: tier}}, fmt.Errorf("xai subscription tier is not recognized")
	}
	return xaiSubscription{entitlement: xaiEntitlementPaid, plan: plan}, nil
}

func xaiCurrentWeeklyPeriod(config *xaiBillingConfig) *xaiBillingPeriod {
	if config.CurrentPeriod != nil && config.CurrentPeriodSnake != nil && *config.CurrentPeriod != *config.CurrentPeriodSnake {
		return nil
	}
	period := config.CurrentPeriod
	if period == nil {
		period = config.CurrentPeriodSnake
	}
	return period
}

func xaiWeeklyPeriodIsCurrent(config *xaiBillingConfig, observedAt time.Time) bool {
	if config.CurrentPeriod != nil && config.CurrentPeriodSnake != nil && *config.CurrentPeriod != *config.CurrentPeriodSnake {
		return false
	}
	period := xaiCurrentWeeklyPeriod(config)
	endValue := firstNonEmptyString(config.BillingPeriodEnd, config.BillingEndSnake)
	if period != nil {
		switch strings.ToLower(strings.TrimSpace(period.Type)) {
		case "", "weekly", "usage_period_type_weekly":
		default:
			return false
		}
		if period.End != "" {
			endValue = period.End
		}
	}
	var end *time.Time
	if endValue != "" {
		end = parseProviderTime(endValue)
		if end == nil || !end.After(observedAt) {
			return false
		}
	}
	if period != nil && period.Start != "" {
		start := parseProviderTime(period.Start)
		if start == nil || start.After(observedAt) || end != nil && !start.Before(*end) {
			return false
		}
	}
	return true
}

func xaiCanInferZeroUsage(config *xaiBillingConfig, observedAt time.Time) bool {
	if config.CreditUsagePercent.present || config.CreditUsagePercentSnake.present {
		return false
	}
	period := xaiCurrentWeeklyPeriod(config)
	if period == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(period.Type)) {
	case "weekly", "usage_period_type_weekly":
	default:
		return false
	}
	end := parseProviderTime(period.End)
	if end == nil || !end.After(observedAt) {
		return false
	}
	if period.Start != "" {
		start := parseProviderTime(period.Start)
		if start == nil || start.After(observedAt) || !start.Before(*end) {
			return false
		}
	}
	for _, product := range append(append([]xaiBillingProductUsage(nil), config.ProductUsage...), config.ProductUsageSnake...) {
		for _, percent := range []xaiPercentage{product.UsagePercent, product.UsagePercentSnake} {
			if percent.present && (percent.value() == nil || *percent.value() != 0) {
				return false
			}
		}
	}
	return true
}

func xaiInvalidResponse(message string) *probeError {
	return &probeError{code: "UPSTREAM_RESPONSE_INVALID", message: message, retryable: true}
}

func xaiCollectionFailure(failures ...*probeError) *probeError {
	var selected *probeError
	for _, failure := range failures {
		if failure != nil && (selected == nil || xaiFailurePriority(failure.code) < xaiFailurePriority(selected.code)) {
			selected = failure
		}
	}
	if selected == nil {
		return nil
	}
	result := *selected
	for _, failure := range failures {
		if failure != nil && failure.retryAfter > result.retryAfter {
			result.retryAfter = failure.retryAfter
		}
	}
	return &result
}

func xaiFailurePriority(code string) int {
	switch code {
	case "UPSTREAM_CLIENT_OUTDATED":
		return 0
	case "UPSTREAM_AUTH_REJECTED":
		return 1
	case "UPSTREAM_RATE_LIMITED":
		return 2
	case "UPSTREAM_RESPONSE_INVALID":
		return 4
	default:
		return 3
	}
}

func xaiHasObservedAmount(windows []cluster.QuotaWindow) bool {
	for _, window := range windows {
		if window.Used != nil || window.Remaining != nil || window.UsedRatio != nil || window.RemainingRatio != nil {
			return true
		}
	}
	return false
}

func (c *Collector) probeXAI(ctx context.Context, auth *coreauth.Auth) (probeResult, *probeError) {
	// Keep all independent requests inside one probe budget and the credential lease.
	ctx, cancel := context.WithTimeout(ctx, c.options.ProbeTimeout)
	defer cancel()
	headers := http.Header{
		"Accept": []string{"*/*"}, "User-Agent": []string{xaiGrokUserAgent},
		xaiTokenAuthHeader: []string{xaiTokenAuthValue}, xaiClientVersionHeader: []string{xaiClientVersionValue},
	}
	if userID := quotaMetadataString(auth.Metadata, "sub", "subject", "user_id", "userId"); userID != "" {
		headers.Set("x-userid", userID)
	}
	targets := []string{c.options.XAIBillingURL, xaiWeeklyBillingURL(c.options.XAIBillingURL), xaiSubscriptionURL(c.options.XAIProfileURL)}
	responses := make([]struct {
		body []byte
		err  *probeError
	}, len(targets))
	var requests sync.WaitGroup
	for index, target := range targets {
		requests.Add(1)
		go func() {
			defer requests.Done()
			responses[index].body, _, responses[index].err = c.probeRequest(ctx, auth, http.MethodGet, target, nil, headers)
		}()
	}
	requests.Wait()
	now := c.options.Now().UTC()
	var retryAfter time.Duration
	for _, response := range responses {
		if response.err != nil && response.err.retryAfter > retryAfter {
			retryAfter = response.err.retryAfter
		}
	}
	errMonthly, errWeekly, errProfile := responses[0].err, responses[1].err, responses[2].err
	var windows []cluster.QuotaWindow
	var fallbackPlan *cluster.QuotaPlan
	if errMonthly == nil {
		var errParse error
		windows, fallbackPlan, errParse = parseXAIUsageWindows(responses[0].body, now)
		if errParse != nil {
			errMonthly = xaiInvalidResponse("xAI monthly billing response could not be parsed.")
		} else {
			for _, window := range windows {
				if window.Used == nil && window.Remaining == nil {
					errMonthly = xaiInvalidResponse("xAI monthly remaining quota is unavailable.")
					break
				}
			}
		}
	}
	var subscription xaiSubscription
	if errProfile == nil {
		var errParse error
		subscription, errParse = parseXAISubscription(responses[2].body)
		if errParse != nil {
			errProfile = xaiInvalidResponse("xAI subscription eligibility could not be confirmed.")
		}
	}
	if subscription.entitlement == xaiEntitlementFree {
		// Free profiles may return a credits period without entitlement to that allowance.
		errWeekly = nil
	} else if errWeekly == nil {
		var payload xaiBillingPayload
		if errDecode := json.Unmarshal(responses[1].body, &payload); errDecode != nil || payload.Config == nil {
			errWeekly = xaiInvalidResponse("xAI weekly quota response could not be parsed.")
		} else {
			weekly := xaiWeeklyWindows(payload.Config, now, subscription.entitlement)
			windows = append(weekly, windows...)
			complete := false
			productsComplete := true
			for _, window := range weekly {
				if window.ID == "xai-weekly-usage" && window.Used != nil {
					complete = true
				} else if window.ID != "xai-weekly-usage" && window.Used == nil {
					productsComplete = false
				}
			}
			if !complete || !productsComplete {
				errWeekly = xaiInvalidResponse("xAI weekly remaining quota is unavailable.")
			}
		}
	}
	failure := xaiCollectionFailure(errMonthly, errWeekly, errProfile)
	if failure != nil && retryAfter > failure.retryAfter {
		failure.retryAfter = retryAfter
	}
	failedWithoutAmounts := !xaiHasObservedAmount(windows) && failure != nil
	if failedWithoutAmounts || len(windows) == 0 && !(subscription.entitlement == xaiEntitlementFree && errMonthly == nil) {
		if failure != nil {
			return probeResult{}, failure
		}
		return probeResult{}, xaiInvalidResponse("xAI billing response did not contain usable quota.")
	}
	result := probeResult{windows: windows, plan: subscription.plan, fallbackPlan: fallbackPlan, replaceWindows: true, replacePlan: subscription.plan != nil, boundWindowLifetimes: true, retryAfter: retryAfter}
	if errMonthly != nil {
		result.preserveWindowPrefixes = append(result.preserveWindowPrefixes, "xai-monthly-", "xai-on-demand")
	}
	if errWeekly != nil {
		result.preserveWindowPrefixes = append(result.preserveWindowPrefixes, "xai-weekly-", "xai-product-")
	}
	if failure != nil {
		result.partial = true
		result.collectionError = probeCollectionError(failure, now)
	}
	return result, nil
}
