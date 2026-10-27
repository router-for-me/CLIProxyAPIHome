package cluster

import (
	"fmt"
	"sort"

	"github.com/tidwall/gjson"
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
	}
}

// Values returns the optional prices keyed by field name; unset prices are nil.
func (p BillingExtraPrices) Values() map[string]*float64 {
	out := make(map[string]*float64, 7)
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
}

func billingTokenDetailsForUsage(usage *UsageRecord) billingTokenDetails {
	if usage == nil || len(usage.TokensJSON) == 0 {
		return billingTokenDetails{}
	}
	raw := string(usage.TokensJSON)
	first := func(paths ...string) int64 {
		for _, path := range paths {
			if value := gjson.Get(raw, path); value.Exists() {
				return value.Int()
			}
		}
		return 0
	}
	return billingTokenDetails{
		CacheWrite1h:  first("cache_creation_1h_tokens", "cache_write_1h_tokens"),
		CacheWrite12h: first("cache_creation_12h_tokens", "cache_write_12h_tokens"),
		ImageInput:    first("image_input_tokens"),
		ImageOutput:   first("image_output_tokens"),
		AudioInput:    first("audio_input_tokens"),
		AudioOutput:   first("audio_output_tokens"),
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
