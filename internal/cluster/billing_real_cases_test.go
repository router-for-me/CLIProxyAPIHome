package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// billingRealCase mirrors the fields of a sanitized production usage payload
// (one per distinct provider/executor/endpoint/failure/cache/reasoning/tier shape).
type billingRealCase struct {
	Provider             string `json:"provider"`
	Endpoint             string `json:"endpoint"`
	Failed               bool   `json:"failed"`
	ServiceTier          string `json:"service_tier"`
	EffectiveServiceTier string `json:"effective_service_tier"`
	ReasoningEffort      string `json:"reasoning_effort"`
	Tokens               struct {
		InputTokens     int64 `json:"input_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
		CacheReadTokens int64 `json:"cache_read_tokens"`
	} `json:"tokens"`
	TokenBreakdown struct {
		Quality     string `json:"quality"`
		TotalTokens int64  `json:"total_tokens"`
		Input       struct {
			UncachedTokens   int64 `json:"uncached_tokens"`
			CacheReadTokens  int64 `json:"cache_read_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"input"`
		Output struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"output"`
	} `json:"token_breakdown"`
}

type billingRealPrices struct {
	input, output, cacheRead, cacheWrite, request float64
}

// TestBillingReplaysRealUsageCases prices every production usage shape and
// checks the charge against an independent calculation.
func TestBillingReplaysRealUsageCases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "real-cases-key")
	standard := billingRealPrices{input: 1.25, output: 10, cacheRead: 0.125, cacheWrite: 1.5625, request: 0.002}
	priority := billingRealPrices{input: 2.5, output: 20, cacheRead: 0.25, cacheWrite: 3.125, request: 0.004}
	multipliers := map[string]float64{"xhigh": 1.5, "max": 2, "low": 0.8}
	for tier, prices := range map[string]billingRealPrices{BillingServiceTierWildcard: standard, "priority": priority} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
			Provider: "*", Model: "*", ServiceTier: tier,
			InputPricePerMillion: prices.input, OutputPricePerMillion: prices.output,
			CacheReadPricePerMillion: prices.cacheRead, CacheWritePricePerMillion: prices.cacheWrite,
			RequestPrice: prices.request, ReasoningEffortMultipliers: multipliers, Enabled: true,
		}); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%s) error = %v", tier, errCreate)
		}
	}

	file, errOpen := os.Open("testdata/billing_real_usage_cases.jsonl")
	if errOpen != nil {
		t.Fatalf("open cases: %v", errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			t.Errorf("close cases: %v", errClose)
		}
	}()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	expected := make(map[string]float64)
	base := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	index := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		index++
		var raw map[string]any
		var item billingRealCase
		if errRaw := json.Unmarshal([]byte(line), &raw); errRaw != nil {
			t.Fatalf("case %d: %v", index, errRaw)
		}
		if errCase := json.Unmarshal([]byte(line), &item); errCase != nil {
			t.Fatalf("case %d: %v", index, errCase)
		}
		requestID := fmt.Sprintf("real-%02d", index)
		raw["api_key"] = "real-cases-key"
		raw["request_id"] = requestID
		raw["timestamp"] = base.Add(time.Duration(index) * time.Second).Format(time.RFC3339)
		payload, errMarshal := json.Marshal(raw)
		if errMarshal != nil {
			t.Fatalf("case %d: %v", index, errMarshal)
		}
		if _, errUsage := repo.AppendUsage(ctx, string(payload), "192.0.2.10"); errUsage != nil {
			t.Fatalf("case %d AppendUsage() error = %v", index, errUsage)
		}
		if want, billed := billingRealCaseExpectedAmount(item, standard, priority, multipliers); billed {
			expected[requestID] = want
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		t.Fatalf("scan cases: %v", errScan)
	}
	if index < 50 {
		t.Fatalf("loaded %d cases, want the full production matrix", index)
	}

	charges := billingCaseCharges(t, repo, user.ID)
	if len(charges) != len(expected) {
		t.Fatalf("charges = %d, want %d", len(charges), len(expected))
	}
	spent := 0.0
	for requestID, want := range expected {
		charge, ok := charges[requestID]
		if !ok {
			t.Fatalf("%s: missing charge", requestID)
		}
		// Amounts are quantized to 8 decimals in two steps (base, then multiplied),
		// so allow one quantum against the single-rounding reference.
		if math.Abs(charge.Amount-want) > 1.0000001e-8 {
			t.Fatalf("%s: amount = %.10f, want %.10f (snapshot %s, cost %s)", requestID, charge.Amount, want, charge.PriceSnapshot, charge.CostBreakdown)
		}
		spent += charge.Amount
	}
	reloaded, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if math.Abs(reloaded.Credits-(100-spent)) > 1e-6 {
		t.Fatalf("credits = %.10f, want %.10f", reloaded.Credits, 100-spent)
	}
}

// billingRealCaseExpectedAmount is an independent reference implementation of
// the pricing rules for the production shapes.
func billingRealCaseExpectedAmount(item billingRealCase, standard, priority billingRealPrices, multipliers map[string]float64) (float64, bool) {
	breakdown := item.TokenBreakdown
	if item.Failed && breakdown.TotalTokens == 0 {
		return 0, false
	}
	prices := standard
	tier := item.EffectiveServiceTier
	if tier == "" {
		tier = item.ServiceTier
	}
	if strings.EqualFold(tier, "priority") {
		prices = priority
	}
	var tokenCost float64
	if breakdown.Quality == "complete" {
		tokenCost = float64(breakdown.Input.UncachedTokens)*prices.input +
			float64(breakdown.Input.CacheReadTokens)*prices.cacheRead +
			float64(breakdown.Input.CacheWriteTokens)*prices.cacheWrite +
			float64(breakdown.Output.TotalTokens)*prices.output
	} else {
		// Inconsistent v2 accounting falls back to the legacy counters, where
		// OpenAI-style input already contains the cached tokens.
		uncached := max(item.Tokens.InputTokens-item.Tokens.CacheReadTokens, 0)
		tokenCost = float64(uncached)*prices.input +
			float64(item.Tokens.CacheReadTokens)*prices.cacheRead +
			float64(item.Tokens.OutputTokens)*prices.output
	}
	tokenCost /= 1e6
	if multiplier, ok := multipliers[strings.ToLower(item.ReasoningEffort)]; ok {
		tokenCost *= multiplier
	}
	amount := tokenCost
	if !item.Failed {
		amount += prices.request
	}
	return billingRoundAmount(amount), true
}
