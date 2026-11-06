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
	for _, code := range []string{"UPSTREAM_CLIENT_OUTDATED", "UPSTREAM_AUTH_REJECTED", "UPSTREAM_RATE_LIMITED"} {
		for _, failure := range failures {
			if failure != nil && failure.code == code {
				return failure
			}
		}
	}
	for _, failure := range failures {
		if failure != nil && failure.code != "UPSTREAM_RESPONSE_INVALID" {
			return failure
		}
	}
	for _, failure := range failures {
		if failure != nil {
			return failure
		}
	}
	return nil
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
	errMonthly, errWeekly, errProfile := responses[0].err, responses[1].err, responses[2].err
	var windows []cluster.QuotaWindow
	var plan *cluster.QuotaPlan
	if errMonthly == nil {
		var errParse error
		windows, plan, errParse = parseXAIUsageWindows(responses[0].body, now)
		if errParse != nil {
			errMonthly = xaiInvalidResponse("xAI monthly billing response could not be parsed.")
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
	if subscription.plan != nil {
		plan = subscription.plan
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
			for _, window := range weekly {
				if window.ID == "xai-weekly-usage" && window.Used != nil {
					complete = true
				}
			}
			if !complete {
				errWeekly = xaiInvalidResponse("xAI weekly remaining quota is unavailable.")
			}
		}
	}
	failedWithoutAmounts := !xaiHasObservedAmount(windows) && (errMonthly != nil || errWeekly != nil || errProfile != nil)
	if failedWithoutAmounts || len(windows) == 0 && !(subscription.entitlement == xaiEntitlementFree && errMonthly == nil) {
		if failure := xaiCollectionFailure(errMonthly, errWeekly, errProfile); failure != nil {
			return probeResult{}, failure
		}
		return probeResult{}, xaiInvalidResponse("xAI billing response did not contain usable quota.")
	}
	result := probeResult{windows: windows, plan: plan, replaceWindows: true, replacePlan: plan != nil}
	if failure := xaiCollectionFailure(errMonthly, errWeekly, errProfile); failure != nil {
		result.partial = true
		result.collectionError = probeCollectionError(failure, now)
	}
	return result, nil
}
