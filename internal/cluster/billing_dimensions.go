package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// Optional price dimensions. A NULL price falls back to its parent price so a
// newly reported bucket is never billed below the ordinary rate; an explicit 0
// bills the bucket as free.
const (
	BillingPriceCacheWrite1h  = "cache_write_1h_price_per_million"
	BillingPriceCacheWrite12h = "cache_write_12h_price_per_million"
	BillingPriceReasoning     = "reasoning_price_per_million"
	BillingPriceImageInput    = "image_input_price_per_million"
	BillingPriceImageOutput   = "image_output_price_per_million"
	BillingPriceAudioInput    = "audio_input_price_per_million"
	BillingPriceAudioOutput   = "audio_output_price_per_million"
	// Per-unit surcharges in USD. They have no parent price, so NULL bills nothing.
	BillingPriceWebSearchCall = "web_search_price_per_call"
	BillingPriceImage         = "image_price_per_image"
)

// BillingExtraPrices holds optional per-dimension prices in USD per million tokens.
type BillingExtraPrices struct {
	CacheWrite1hPricePerMillion  *float64 `gorm:"column:cache_write_1h_price_per_million" json:"cache_write_1h_price_per_million"`
	CacheWrite12hPricePerMillion *float64 `gorm:"column:cache_write_12h_price_per_million" json:"cache_write_12h_price_per_million"`
	ReasoningPricePerMillion     *float64 `gorm:"column:reasoning_price_per_million" json:"reasoning_price_per_million"`
	ImageInputPricePerMillion    *float64 `gorm:"column:image_input_price_per_million" json:"image_input_price_per_million"`
	ImageOutputPricePerMillion   *float64 `gorm:"column:image_output_price_per_million" json:"image_output_price_per_million"`
	AudioInputPricePerMillion    *float64 `gorm:"column:audio_input_price_per_million" json:"audio_input_price_per_million"`
	AudioOutputPricePerMillion   *float64 `gorm:"column:audio_output_price_per_million" json:"audio_output_price_per_million"`
	WebSearchPricePerCall        *float64 `gorm:"column:web_search_price_per_call" json:"web_search_price_per_call"`
	ImagePricePerImage           *float64 `gorm:"column:image_price_per_image" json:"image_price_per_image"`
}

// BillingExtraPriceNames returns the optional price field names in stable order.
func BillingExtraPriceNames() []string {
	return []string{
		BillingPriceCacheWrite1h,
		BillingPriceCacheWrite12h,
		BillingPriceReasoning,
		BillingPriceImageInput,
		BillingPriceImageOutput,
		BillingPriceAudioInput,
		BillingPriceAudioOutput,
		BillingPriceWebSearchCall,
		BillingPriceImage,
	}
}

func (p *BillingExtraPrices) fieldPointers() map[string]**float64 {
	return map[string]**float64{
		BillingPriceCacheWrite1h:  &p.CacheWrite1hPricePerMillion,
		BillingPriceCacheWrite12h: &p.CacheWrite12hPricePerMillion,
		BillingPriceReasoning:     &p.ReasoningPricePerMillion,
		BillingPriceImageInput:    &p.ImageInputPricePerMillion,
		BillingPriceImageOutput:   &p.ImageOutputPricePerMillion,
		BillingPriceAudioInput:    &p.AudioInputPricePerMillion,
		BillingPriceAudioOutput:   &p.AudioOutputPricePerMillion,
		BillingPriceWebSearchCall: &p.WebSearchPricePerCall,
		BillingPriceImage:         &p.ImagePricePerImage,
	}
}

// Values returns the optional prices keyed by field name; unset prices are nil.
func (p BillingExtraPrices) Values() map[string]*float64 {
	out := make(map[string]*float64, 9)
	for name, field := range p.fieldPointers() {
		out[name] = *field
	}
	return out
}

// Set assigns one optional price by field name; nil clears it.
func (p *BillingExtraPrices) Set(name string, value *float64) error {
	field, ok := p.fieldPointers()[name]
	if !ok {
		return fmt.Errorf("unknown price field %q", name)
	}
	if value == nil {
		*field = nil
		return nil
	}
	copied := *value
	*field = &copied
	return nil
}

func validateBillingExtraPrices(values map[string]*float64) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	known := (&BillingExtraPrices{}).fieldPointers()
	for _, name := range names {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("unknown price field %q", name)
		}
		if value := values[name]; value != nil && *value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	return nil
}

func billingPriceOr(value *float64, fallback float64) float64 {
	if value != nil {
		return *value
	}
	return fallback
}

// billingTokenDetails are optional sub-buckets reported inside the usage tokens
// object. Each is a subset of a canonical v2 bucket and is clamped to it.
type billingTokenDetails struct {
	CacheWrite1h  int64
	CacheWrite12h int64
	ImageInput    int64
	ImageOutput   int64
	AudioInput    int64
	AudioOutput   int64
	// Per-unit counters reported in the tokens object or at the payload top level.
	WebSearchCalls int64
	Images         int64
}

func billingTokenDetailsForUsage(usage *UsageRecord) billingTokenDetails {
	if usage == nil {
		return billingTokenDetails{}
	}
	raw := string(usage.TokensJSON)
	payload := string(usage.PayloadJSON)
	first := func(paths ...string) int64 {
		for _, path := range paths {
			if value := gjson.Get(raw, path); raw != "" && value.Exists() {
				return value.Int()
			}
		}
		return 0
	}
	units := func(path string) int64 {
		if value := first(path); value > 0 {
			return value
		}
		if payload == "" {
			return 0
		}
		if value := gjson.Get(payload, path).Int(); value > 0 {
			return value
		}
		return 0
	}
	images := units("image_count")
	// An images endpoint returns at least one image even when CPA omits the count.
	if images == 0 && billingImagesEndpoint(usage.Endpoint) {
		images = 1
	}
	return billingTokenDetails{
		WebSearchCalls: units("web_search_requests"),
		Images:         images,
		CacheWrite1h:   first("cache_creation_1h_tokens", "cache_write_1h_tokens"),
		CacheWrite12h:  first("cache_creation_12h_tokens", "cache_write_12h_tokens"),
		ImageInput:     first("image_input_tokens"),
		ImageOutput:    first("image_output_tokens"),
		AudioInput:     first("audio_input_tokens"),
		AudioOutput:    first("audio_output_tokens"),
	}
}

func clampBillingTokens(value int64, limit int64) int64 {
	if value < 0 || limit <= 0 {
		return 0
	}
	if value > limit {
		return limit
	}
	return value
}

// billingEffortMultiplier returns the normalized effort and its multiplier; a
// missing entry returns 0, which leaves the token cost unchanged.
func billingEffortMultiplier(effort string, multipliers map[string]float64) (string, float64) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" || len(multipliers) == 0 {
		return "", 0
	}
	multiplier, ok := multipliers[effort]
	if !ok {
		return "", 0
	}
	return effort, multiplier
}

func validateBillingEffortMultipliers(multipliers map[string]float64) error {
	for effort, multiplier := range multipliers {
		if strings.TrimSpace(effort) == "" {
			return fmt.Errorf("reasoning_effort_multipliers keys must be non-empty")
		}
		if multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
			return fmt.Errorf("reasoning_effort_multipliers[%s] must be a non-negative number", effort)
		}
	}
	return nil
}

// billingEffortMultipliersJSONB normalizes keys to lower case; an empty map stores NULL.
func billingEffortMultipliersJSONB(multipliers map[string]float64) (JSONB, error) {
	if errValidate := validateBillingEffortMultipliers(multipliers); errValidate != nil {
		return nil, errValidate
	}
	if len(multipliers) == 0 {
		return nil, nil
	}
	normalized := make(map[string]float64, len(multipliers))
	for effort, multiplier := range multipliers {
		normalized[strings.ToLower(strings.TrimSpace(effort))] = multiplier
	}
	return billingJSONB(normalized)
}

func billingEffortMultipliersFromJSONB(raw JSONB) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	multipliers := make(map[string]float64)
	if errUnmarshal := json.Unmarshal(raw, &multipliers); errUnmarshal != nil {
		return nil
	}
	return multipliers
}

// billingUserMultiplierTx returns the user's billing multiplier, defaulting to 1.
func billingUserMultiplierTx(ctx context.Context, tx *gorm.DB, userID uint) (float64, error) {
	user := &UserRecord{}
	result := tx.WithContext(contextOrBackground(ctx)).Select("id", "billing_multiplier").Where("id = ?", userID).Limit(1).Find(user)
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 0 || user.BillingMultiplier == nil {
		return 1, nil
	}
	return *user.BillingMultiplier, nil
}
