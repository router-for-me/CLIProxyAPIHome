package userapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// The model registry is process-wide, so these tests register into it and clean
// up after themselves rather than running in parallel. Sharing it across
// parallel tests would let one case observe another's catalog.

func TestPublicModelCatalogWithholdsPriceAndAvailability(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)
	seedUserModelPrices(t, handler)
	seedUserModelUsage(t, handler, "gpt-4.1-mini", "catalog-client-key", 12, 0)

	payload := getUserModels(t, handler.ListModels, "/models", "")

	if payload.Total != len(payload.Models) {
		t.Fatalf("total = %d, want %d", payload.Total, len(payload.Models))
	}
	if len(payload.Models) < 3 {
		t.Fatalf("models = %d, want the full registered catalog", len(payload.Models))
	}
	for _, model := range payload.Models {
		if model.Pricing != nil {
			t.Errorf("model %s exposes pricing to an anonymous visitor", model.ID)
		}
		if model.Availability != nil {
			t.Errorf("model %s exposes availability to an anonymous visitor", model.ID)
		}
	}
}

func TestPublicModelCatalogReportsUnknownModalitiesAndCapabilities(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)

	models := indexUserModelsByID(getUserModels(t, handler.ListModels, "/models", "").Models)

	described, ok := models["gpt-4.1-mini"]
	if !ok {
		t.Fatalf("gpt-4.1-mini missing from catalog")
	}
	if described.Modalities.Status != modalityStatusKnown {
		t.Errorf("described modality status = %q, want %q", described.Modalities.Status, modalityStatusKnown)
	}
	// Hand-maintained catalog values are normalized before a buyer sees them:
	// lower-cased, trimmed and de-duplicated, with catalog order preserved so the
	// primary modality stays first.
	if got := strings.Join(described.Modalities.Input, ","); got != "text,image" {
		t.Errorf("described input modalities = %q, want %q", got, "text,image")
	}
	if got := strings.Join(described.Modalities.Output, ","); got != "text" {
		t.Errorf("described output modalities = %q, want %q", got, "text")
	}
	if described.Capabilities.ToolCalling.Status != capabilityStatusSupported {
		t.Errorf("described tool calling = %q, want %q", described.Capabilities.ToolCalling.Status, capabilityStatusSupported)
	}
	if described.Capabilities.StructuredOutput.Status != capabilityStatusSupported {
		t.Errorf("described structured output = %q, want %q", described.Capabilities.StructuredOutput.Status, capabilityStatusSupported)
	}
	if described.ContextLength != 128000 {
		t.Errorf("described context length = %d, want 128000", described.ContextLength)
	}
	if described.MaxOutputTokens != 16384 {
		t.Errorf("described max output = %d, want 16384", described.MaxOutputTokens)
	}

	// The whole point of the tri-state: a model nobody described must not be
	// rendered as a text-only model with no tool support.
	silent, ok := models["mystery-model"]
	if !ok {
		t.Fatalf("mystery-model missing from catalog")
	}
	if silent.Modalities.Status != modalityStatusUnknown {
		t.Errorf("silent modality status = %q, want %q", silent.Modalities.Status, modalityStatusUnknown)
	}
	if len(silent.Modalities.Input) != 0 || len(silent.Modalities.Output) != 0 {
		t.Errorf("silent modalities = %+v, want empty", silent.Modalities)
	}
	if silent.Capabilities.ToolCalling.Status != capabilityStatusUnknown {
		t.Errorf("silent tool calling = %q, want %q", silent.Capabilities.ToolCalling.Status, capabilityStatusUnknown)
	}
	if silent.Capabilities.Reasoning.Status != capabilityStatusUnknown {
		t.Errorf("silent reasoning = %q, want %q", silent.Capabilities.Reasoning.Status, capabilityStatusUnknown)
	}
	if silent.Capabilities.StructuredOutput.Status != capabilityStatusUnknown {
		t.Errorf("silent structured output = %q, want %q", silent.Capabilities.StructuredOutput.Status, capabilityStatusUnknown)
	}
	if silent.ContextLength != 0 {
		t.Errorf("silent context length = %d, want the field omitted", silent.ContextLength)
	}

	// A model that publishes parameters and omits tools has said no.
	stated, ok := models["text-only-model"]
	if !ok {
		t.Fatalf("text-only-model missing from catalog")
	}
	if stated.Capabilities.ToolCalling.Status != capabilityStatusUnsupported {
		t.Errorf("stated tool calling = %q, want %q", stated.Capabilities.ToolCalling.Status, capabilityStatusUnsupported)
	}
	if stated.Capabilities.StructuredOutput.Status != capabilityStatusUnsupported {
		t.Errorf("stated structured output = %q, want %q", stated.Capabilities.StructuredOutput.Status, capabilityStatusUnsupported)
	}
	if stated.Modalities.Status != modalityStatusKnown {
		t.Errorf("stated modality status = %q, want %q", stated.Modalities.Status, modalityStatusKnown)
	}
}

func TestAccessibleModelsRequireAuthentication(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/models/accessible", nil)
	handler.ListAccessibleModels(ctx)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", resp.Code, resp.Body.String())
	}
}

func TestAccessibleModelsHonorModelGroupRestriction(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)

	restrictedUser := createUserModelTestUser(t, handler, "restricted-user")
	group := createUserModelGroup(t, handler, "cheap-tier", "gpt-4.1-mini")
	createUserModelAPIKey(t, handler, restrictedUser.ID, "restricted-client-key", []uint{group.ID})

	unrestrictedUser := createUserModelTestUser(t, handler, "unrestricted-user")
	createUserModelAPIKey(t, handler, unrestrictedUser.ID, "unrestricted-client-key", nil)

	keylessUser := createUserModelTestUser(t, handler, "keyless-user")

	restricted := getUserModels(t, handler.ListAccessibleModels, "/models/accessible", createUserBillingBearerToken(t, handler, restrictedUser.ID))
	if got := userModelIDs(restricted.Models); len(got) != 1 || got[0] != "gpt-4.1-mini" {
		t.Errorf("restricted models = %v, want [gpt-4.1-mini]", got)
	}
	if !restricted.Access.Restricted {
		t.Error("restricted access.restricted = false, want true")
	}
	if restricted.Access.Reason != modelAccessReasonModelGroups {
		t.Errorf("restricted reason = %q, want %q", restricted.Access.Reason, modelAccessReasonModelGroups)
	}

	unrestricted := getUserModels(t, handler.ListAccessibleModels, "/models/accessible", createUserBillingBearerToken(t, handler, unrestrictedUser.ID))
	if len(unrestricted.Models) < 3 {
		t.Errorf("unrestricted models = %d, want the full catalog", len(unrestricted.Models))
	}
	if unrestricted.Access.Restricted {
		t.Error("unrestricted access.restricted = true, want false")
	}
	if unrestricted.Access.Reason != modelAccessReasonUnrestricted {
		t.Errorf("unrestricted reason = %q, want %q", unrestricted.Access.Reason, modelAccessReasonUnrestricted)
	}

	// No key means no way to call the cluster, which is not the same as access
	// to everything.
	keyless := getUserModels(t, handler.ListAccessibleModels, "/models/accessible", createUserBillingBearerToken(t, handler, keylessUser.ID))
	if len(keyless.Models) != 0 {
		t.Errorf("keyless models = %d, want 0", len(keyless.Models))
	}
	if keyless.Access.Reason != modelAccessReasonNoAPIKeys {
		t.Errorf("keyless reason = %q, want %q", keyless.Access.Reason, modelAccessReasonNoAPIKeys)
	}
	if keyless.Access.APIKeyCount != 0 {
		t.Errorf("keyless api_key_count = %d, want 0", keyless.Access.APIKeyCount)
	}
}

func TestAccessibleModelsReturnPriceLadderWithoutOperatorBookkeeping(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)
	seedUserModelPrices(t, handler)

	user := createUserModelTestUser(t, handler, "ladder-user")
	createUserModelAPIKey(t, handler, user.ID, "ladder-client-key", nil)
	payload := getUserModels(t, handler.ListAccessibleModels, "/models/accessible", createUserBillingBearerToken(t, handler, user.ID))
	models := indexUserModelsByID(payload.Models)

	priced, ok := models["gpt-4.1-mini"]
	if !ok {
		t.Fatalf("gpt-4.1-mini missing from accessible catalog")
	}
	if priced.Pricing == nil || priced.Pricing.Status != pricingStatusPublished {
		t.Fatalf("priced pricing = %+v, want published", priced.Pricing)
	}
	if len(priced.Pricing.Providers) != 1 {
		t.Fatalf("priced providers = %d, want 1", len(priced.Pricing.Providers))
	}
	provider := priced.Pricing.Providers[0]
	if provider.Provider != "openai" {
		t.Errorf("provider = %q, want openai", provider.Provider)
	}
	if len(provider.Tiers) != 2 {
		t.Fatalf("tiers = %d, want 2", len(provider.Tiers))
	}

	tiers := make(map[string]userModelPriceTierPayload, len(provider.Tiers))
	for _, tier := range provider.Tiers {
		tiers[tier.ServiceTier] = tier
	}
	wildcard, ok := tiers["*"]
	if !ok {
		t.Fatalf("wildcard tier missing, got %v", provider.Tiers)
	}
	if !wildcard.IsDefault {
		t.Error("wildcard tier is_default = false, want true")
	}
	// The ladder must ascend, because the rung a request lands on is the last
	// threshold it clears.
	if len(wildcard.Rungs) != 2 {
		t.Fatalf("wildcard rungs = %d, want 2", len(wildcard.Rungs))
	}
	if wildcard.Rungs[0].MinInputTokens != 0 || wildcard.Rungs[1].MinInputTokens != 128000 {
		t.Errorf("wildcard rung thresholds = %d,%d, want 0,128000", wildcard.Rungs[0].MinInputTokens, wildcard.Rungs[1].MinInputTokens)
	}
	if wildcard.Rungs[1].InputPricePerMillion <= wildcard.Rungs[0].InputPricePerMillion {
		t.Errorf("long-context rung is not more expensive: %v", wildcard.Rungs)
	}
	if _, ok := tiers["flex"]; !ok {
		t.Errorf("flex tier missing, got %v", provider.Tiers)
	}

	// Operator bookkeeping must not ride along with the quote.
	assertNoUserModelPriceBookkeeping(t, payload.Raw)

	// A model with no enabled rule is unpriced, never free.
	unpriced, ok := models["mystery-model"]
	if !ok {
		t.Fatalf("mystery-model missing from accessible catalog")
	}
	if unpriced.Pricing == nil || unpriced.Pricing.Status != pricingStatusUnpublished {
		t.Fatalf("unpriced pricing = %+v, want unpublished", unpriced.Pricing)
	}
	if len(unpriced.Pricing.Providers) != 0 {
		t.Errorf("unpriced providers = %v, want none", unpriced.Pricing.Providers)
	}

	// A rule for a provider that cannot serve the model is not a quote.
	foreign, ok := models["text-only-model"]
	if !ok {
		t.Fatalf("text-only-model missing from accessible catalog")
	}
	if foreign.Pricing == nil || foreign.Pricing.Status != pricingStatusUnpublished {
		t.Errorf("foreign-provider pricing = %+v, want unpublished", foreign.Pricing)
	}
}

func TestAccessibleModelsReportInsufficientAvailabilityData(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)

	user := createUserModelTestUser(t, handler, "availability-user")
	createUserModelAPIKey(t, handler, user.ID, "availability-client-key", nil)
	// Twelve observations, two of them failures: enough to publish a rate.
	seedUserModelUsage(t, handler, "gpt-4.1-mini", "availability-client-key", 10, 2)
	// Three observations: a sample, but not a measurement.
	seedUserModelUsage(t, handler, "text-only-model", "availability-client-key", 3, 0)

	payload := getUserModels(t, handler.ListAccessibleModels, "/models/accessible", createUserBillingBearerToken(t, handler, user.ID))
	models := indexUserModelsByID(payload.Models)

	observed := models["gpt-4.1-mini"].Availability
	if observed == nil {
		t.Fatalf("observed availability missing")
	}
	if observed.Status != availabilityStatusObserved {
		t.Fatalf("observed status = %q body=%s, want %q", observed.Status, payload.Raw, availabilityStatusObserved)
	}
	if observed.SampleCount != 12 {
		t.Errorf("observed sample_count = %d, want 12", observed.SampleCount)
	}
	if observed.AvailabilityRate == nil || *observed.AvailabilityRate != 0.8333 {
		t.Errorf("observed availability_rate = %v, want 0.8333", observed.AvailabilityRate)
	}
	if observed.AvgLatencyMS == nil || *observed.AvgLatencyMS <= 0 {
		t.Errorf("observed avg_latency_ms = %v, want a positive mean", observed.AvgLatencyMS)
	}
	if observed.AvgTTFTMS == nil || *observed.AvgTTFTMS <= 0 {
		t.Errorf("observed avg_ttft_ms = %v, want a positive mean", observed.AvgTTFTMS)
	}
	if observed.OutputTokensPerSecond == nil || *observed.OutputTokensPerSecond <= 0 {
		t.Errorf("observed output_tokens_per_second = %v, want a positive rate", observed.OutputTokensPerSecond)
	}
	if observed.Window.Hours != int(modelAvailabilityWindow/time.Hour) {
		t.Errorf("observed window hours = %d, want %d", observed.Window.Hours, int(modelAvailabilityWindow/time.Hour))
	}
	if observed.Window.MinSamples != modelAvailabilityMinSamples {
		t.Errorf("observed window min_samples = %d, want %d", observed.Window.MinSamples, modelAvailabilityMinSamples)
	}

	// Three successes out of three is not 100% uptime.
	undersampled := models["text-only-model"].Availability
	if undersampled == nil {
		t.Fatalf("undersampled availability missing")
	}
	if undersampled.Status != availabilityStatusInsufficientData {
		t.Errorf("undersampled status = %q, want %q", undersampled.Status, availabilityStatusInsufficientData)
	}
	if undersampled.AvailabilityRate != nil {
		t.Errorf("undersampled availability_rate = %v, want omitted", *undersampled.AvailabilityRate)
	}
	if undersampled.SampleCount != 3 {
		t.Errorf("undersampled sample_count = %d, want 3", undersampled.SampleCount)
	}

	// A model nobody called is not a model that never failed.
	unobserved := models["mystery-model"].Availability
	if unobserved == nil {
		t.Fatalf("unobserved availability missing")
	}
	if unobserved.Status != availabilityStatusInsufficientData {
		t.Errorf("unobserved status = %q, want %q", unobserved.Status, availabilityStatusInsufficientData)
	}
	if unobserved.SampleCount != 0 {
		t.Errorf("unobserved sample_count = %d, want 0", unobserved.SampleCount)
	}
	if unobserved.AvailabilityRate != nil {
		t.Errorf("unobserved availability_rate = %v, want omitted", *unobserved.AvailabilityRate)
	}
}

// TestAccessibleModelsFoldUsageSpellingsIntoOneSummary pins that observations
// are counted whatever case the usage record spells the model in.
//
// A usage record carries the spelling the caller sent, not the spelling the
// catalog registers, so the two drift apart in normal operation. Matching them
// exactly drops the records that differ, which is indistinguishable on screen
// from a model nobody called.
func TestAccessibleModelsFoldUsageSpellingsIntoOneSummary(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)

	user := createUserModelTestUser(t, handler, "case-fold-user")
	createUserModelAPIKey(t, handler, user.ID, "case-fold-client-key", nil)
	// The same model, written three ways across fourteen observations. The
	// catalog registers only the lower-cased spelling.
	seedUserModelUsage(t, handler, "gpt-4.1-mini", "case-fold-client-key", 6, 1)
	seedUserModelUsage(t, handler, "GPT-4.1-Mini", "case-fold-client-key", 4, 1)
	seedUserModelUsage(t, handler, "GPT-4.1-MINI", "case-fold-client-key", 2, 0)

	payload := getUserModels(t, handler.ListAccessibleModels, "/models/accessible", createUserBillingBearerToken(t, handler, user.ID))
	availability := indexUserModelsByID(payload.Models)["gpt-4.1-mini"].Availability
	if availability == nil {
		t.Fatalf("availability missing body=%s", payload.Raw)
	}
	if availability.SampleCount != 14 {
		t.Fatalf("sample_count = %d, want 14 — every spelling counts once", availability.SampleCount)
	}
	if availability.Status != availabilityStatusObserved {
		t.Fatalf("status = %q, want %q", availability.Status, availabilityStatusObserved)
	}
	// Twelve of fourteen succeeded. Getting this from a single merged group is
	// the point: two half-sized groups would each round to a different rate and
	// only one of them would survive into the response.
	if availability.AvailabilityRate == nil || *availability.AvailabilityRate != 0.8571 {
		t.Errorf("availability_rate = %v, want 0.8571", availability.AvailabilityRate)
	}
}

// TestModelPricingWithholdsQuotesWhenNoProviderCanServe pins the fail-closed
// half of the servable-provider filter.
//
// The filter exists because the price table outlives the channels it prices. A
// model with no known channel is the case where nothing in the table can be
// confirmed to describe the route a request would take, so it is the last case
// that should quote all of it.
func TestModelPricingWithholdsQuotesWhenNoProviderCanServe(t *testing.T) {
	index := newModelPriceIndex([]cluster.BillingModelPriceRecord{
		{Provider: "anthropic", Model: "orphan-model", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 5, Enabled: true},
		{Provider: "openai", Model: "orphan-model", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 7, Enabled: true},
	})

	withoutProviders := index.pricingFor("orphan-model", nil)
	if status, _ := withoutProviders["status"].(string); status != pricingStatusUnpublished {
		t.Errorf("status without providers = %q, want %q", status, pricingStatusUnpublished)
	}
	if _, quoted := withoutProviders["providers"]; quoted {
		t.Errorf("providers quoted for a model no channel is known to serve: %#v", withoutProviders)
	}

	// Blank entries are not a serving channel either.
	if status, _ := index.pricingFor("orphan-model", []string{" ", ""})["status"].(string); status != pricingStatusUnpublished {
		t.Errorf("status with blank providers = %q, want %q", status, pricingStatusUnpublished)
	}

	// Naming a channel still prices it, and still prices only it.
	served := index.pricingFor("orphan-model", []string{"anthropic"})
	if status, _ := served["status"].(string); status != pricingStatusPublished {
		t.Fatalf("status with a serving provider = %q, want %q", status, pricingStatusPublished)
	}
	quoted, _ := served["providers"].([]gin.H)
	if len(quoted) != 1 {
		t.Fatalf("quoted providers = %d, want 1", len(quoted))
	}
	if name, _ := quoted[0]["provider"].(string); name != "anthropic" {
		t.Errorf("quoted provider = %q, want %q", name, "anthropic")
	}
}

// TestModelAvailabilityCollapsesConcurrentRecomputations pins that a cold cache
// costs one scan rather than one per in-flight request.
//
// Callers that share a computation are handed the same map. Callers that each
// ran their own would hold distinct maps built from distinct result sets, so
// identity is the observable difference between collapsing and stampeding.
func TestModelAvailabilityCollapsesConcurrentRecomputations(t *testing.T) {
	handler, closeRepo := newUserModelTestHandler(t)
	defer closeRepo()

	registerUserModelCatalog(t)
	seedUserModelUsage(t, handler, "gpt-4.1-mini", "stampede-client-key", 12, 0)

	const callers = 8
	modelIDs := []string{"gpt-4.1-mini", "text-only-model", "mystery-model"}
	summaries := make([]map[string]cluster.ModelAvailabilitySummary, callers)
	errs := make([]error, callers)

	var ready, done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func(slot int) {
			defer done.Done()
			ready.Done()
			<-start
			summaries[slot], _, errs[slot] = handler.modelAvailabilityIndex(context.Background(), modelIDs)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	for slot := range errs {
		if errs[slot] != nil {
			t.Fatalf("caller %d error = %v", slot, errs[slot])
		}
	}
	for slot := 1; slot < callers; slot++ {
		if reflect.ValueOf(summaries[slot]).Pointer() != reflect.ValueOf(summaries[0]).Pointer() {
			t.Fatalf("caller %d received its own summary, want one shared computation", slot)
		}
	}
	if got := summaries[0]["gpt-4.1-mini"].SampleCount; got != 12 {
		t.Errorf("shared sample_count = %d, want 12", got)
	}
}

// --- decoding helpers -------------------------------------------------------

type userModelModalitiesPayload struct {
	Status string   `json:"status"`
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type userModelCapabilityStatusPayload struct {
	Status string `json:"status"`
}

type userModelCapabilitiesPayload struct {
	Reasoning        userModelCapabilityStatusPayload `json:"reasoning"`
	ToolCalling      userModelCapabilityStatusPayload `json:"tool_calling"`
	StructuredOutput userModelCapabilityStatusPayload `json:"structured_output"`
	Parameters       []string                         `json:"parameters"`
}

type userModelPriceRungPayload struct {
	MinInputTokens            int64   `json:"min_input_tokens"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million"`
	RequestPrice              float64 `json:"request_price"`
}

type userModelPriceTierPayload struct {
	ServiceTier string                      `json:"service_tier"`
	IsDefault   bool                        `json:"is_default"`
	Rungs       []userModelPriceRungPayload `json:"rungs"`
}

type userModelPriceProviderPayload struct {
	Provider string                      `json:"provider"`
	Tiers    []userModelPriceTierPayload `json:"tiers"`
}

type userModelPricingPayload struct {
	Status    string                          `json:"status"`
	Providers []userModelPriceProviderPayload `json:"providers"`
}

type userModelAvailabilityWindowPayload struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Hours      int    `json:"hours"`
	MinSamples int    `json:"min_samples"`
}

type userModelAvailabilityPayload struct {
	Status                string                             `json:"status"`
	Window                userModelAvailabilityWindowPayload `json:"window"`
	SampleCount           int64                              `json:"sample_count"`
	SuccessCount          int64                              `json:"success_count"`
	FailedCount           int64                              `json:"failed_count"`
	AvailabilityRate      *float64                           `json:"availability_rate"`
	AvgLatencyMS          *float64                           `json:"avg_latency_ms"`
	AvgTTFTMS             *float64                           `json:"avg_ttft_ms"`
	OutputTokensPerSecond *float64                           `json:"output_tokens_per_second"`
	LastObservedAt        string                             `json:"last_observed_at"`
}

type userModelPayload struct {
	ID              string                        `json:"id"`
	DisplayName     string                        `json:"display_name"`
	Providers       []string                      `json:"providers"`
	ContextLength   int                           `json:"context_length"`
	MaxOutputTokens int                           `json:"max_output_tokens"`
	Modalities      userModelModalitiesPayload    `json:"modalities"`
	Capabilities    userModelCapabilitiesPayload  `json:"capabilities"`
	Pricing         *userModelPricingPayload      `json:"pricing"`
	Availability    *userModelAvailabilityPayload `json:"availability"`
}

type userModelAccessPayload struct {
	Restricted  bool   `json:"restricted"`
	APIKeyCount int    `json:"api_key_count"`
	Reason      string `json:"reason"`
}

type userModelListPayload struct {
	Models []userModelPayload     `json:"models"`
	Total  int                    `json:"total"`
	Access userModelAccessPayload `json:"access"`
	Raw    string                 `json:"-"`
}

func getUserModels(t *testing.T, handle gin.HandlerFunc, path, token string) userModelListPayload {
	t.Helper()

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		ctx.Request.Header.Set("Authorization", "Bearer "+token)
	}
	handle(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s, want 200", path, resp.Code, resp.Body.String())
	}
	var payload userModelListPayload
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode %s: %v body=%s", path, errDecode, resp.Body.String())
	}
	payload.Raw = resp.Body.String()
	return payload
}

func indexUserModelsByID(models []userModelPayload) map[string]userModelPayload {
	indexed := make(map[string]userModelPayload, len(models))
	for _, model := range models {
		indexed[model.ID] = model
	}
	return indexed
}

func userModelIDs(models []userModelPayload) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// assertNoUserModelPriceBookkeeping walks the raw JSON rather than a decoded
// struct, because a field that should not exist cannot be asserted absent by
// decoding into a type that does not declare it.
//
// The walk is scoped to the pricing subtree: "source" is forbidden on a price
// rung (it names the operator rule's origin) while being expected on modalities
// (it names who described the model), so a whole-body substring check would
// report the wrong thing.
func assertNoUserModelPriceBookkeeping(t *testing.T, raw string) {
	t.Helper()

	forbidden := map[string]struct{}{
		"id": {}, "note": {}, "source": {}, "enabled": {}, "revision": {},
		"created_at": {}, "updated_at": {}, "deleted_at": {},
	}

	var decoded struct {
		Models []struct {
			ID      string          `json:"id"`
			Pricing json.RawMessage `json:"pricing"`
		} `json:"models"`
	}
	if errDecode := json.Unmarshal([]byte(raw), &decoded); errDecode != nil {
		t.Fatalf("decode for redaction check: %v", errDecode)
	}
	for _, model := range decoded.Models {
		if len(model.Pricing) == 0 {
			continue
		}
		var pricing any
		if errDecode := json.Unmarshal(model.Pricing, &pricing); errDecode != nil {
			t.Fatalf("decode pricing for %s: %v", model.ID, errDecode)
		}
		walkJSONKeys(pricing, func(key string) {
			if _, banned := forbidden[key]; banned {
				t.Errorf("model %s pricing leaks operator bookkeeping key %q: %s", model.ID, key, model.Pricing)
			}
		})
	}
}

func walkJSONKeys(value any, visit func(key string)) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			visit(key)
			walkJSONKeys(nested, visit)
		}
	case []any:
		for _, nested := range typed {
			walkJSONKeys(nested, visit)
		}
	}
}

// --- seeding helpers --------------------------------------------------------

func newUserModelTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()

	resetModelAvailabilityCache()
	t.Cleanup(resetModelAvailabilityCache)

	ctx := context.Background()
	db, errOpenSQLite := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewHandler(cluster.NewRepository(db), nil), closeRepo
}

// registerUserModelCatalog installs a catalog covering the three descriptions a
// buyer-facing endpoint has to tell apart: fully described, partially described,
// and undescribed.
func registerUserModelCatalog(t *testing.T) {
	t.Helper()

	const clientID = "user-model-test-client"
	registry.GetGlobalRegistry().RegisterClient(clientID, "openai", []*registry.ModelInfo{
		{
			ID:                  "gpt-4.1-mini",
			DisplayName:         "GPT-4.1 mini",
			Description:         "Fast general purpose model.",
			Type:                "chat",
			ContextLength:       128000,
			MaxCompletionTokens: 16384,
			SupportedParameters: []string{"tools", "temperature", "reasoning_effort", "response_format"},
			// Deliberately messy: the catalog is hand-maintained JSON, so casing
			// slips, padding and duplicates are the realistic input, not the
			// exception. The endpoint must clean them rather than echo them.
			SupportedInputModalities: []string{"Text", " IMAGE ", "text", ""},
			SupportedOutputModalities: []string{
				"text",
			},
		},
		{
			ID:                        "text-only-model",
			DisplayName:               "Text Only",
			ContextLength:             32000,
			SupportedParameters:       []string{"temperature", "top_p"},
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		},
		{
			ID: "mystery-model",
		},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(clientID)
	})
}

func createUserModelTestUser(t *testing.T, handler *Handler, username string) *cluster.UserRecord {
	t.Helper()

	credits := 100.0
	user, errCreate := handler.repo.CreateUser(context.Background(), cluster.UserUpdate{Username: &username, Credits: &credits})
	if errCreate != nil {
		t.Fatalf("CreateUser(%s) error = %v", username, errCreate)
	}
	return user
}

func createUserModelAPIKey(t *testing.T, handler *Handler, userID uint, apiKey string, modelGroups []uint) {
	t.Helper()

	update := cluster.APIKeyUserUpdate{APIKey: &apiKey}
	if modelGroups != nil {
		update.ModelGroups = &modelGroups
	}
	if _, errCreate := handler.repo.CreateAPIKeyForUser(context.Background(), userID, update); errCreate != nil {
		t.Fatalf("CreateAPIKeyForUser(%s) error = %v", apiKey, errCreate)
	}
}

func createUserModelGroup(t *testing.T, handler *Handler, groupName string, modelIDs ...string) *cluster.ModelGroupRecord {
	t.Helper()

	ctx := context.Background()
	group, errGroup := handler.repo.CreateModelGroup(ctx, groupName, false)
	if errGroup != nil {
		t.Fatalf("CreateModelGroup(%s) error = %v", groupName, errGroup)
	}
	for _, modelID := range modelIDs {
		if _, errDetail := handler.repo.CreateModelGroupDetail(ctx, group.ID, modelID, nil); errDetail != nil {
			t.Fatalf("CreateModelGroupDetail(%s) error = %v", modelID, errDetail)
		}
	}
	return group
}

// seedUserModelPrices installs a two-rung ladder on the wildcard tier, a named
// tier, and a rule for a provider that cannot serve the model it names.
func seedUserModelPrices(t *testing.T, handler *Handler) {
	t.Helper()

	ctx := context.Background()
	prices := []cluster.BillingModelPriceUpdate{
		{Provider: "openai", Model: "gpt-4.1-mini", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 0.4, OutputPricePerMillion: 1.6, Enabled: true},
		{Provider: "openai", Model: "gpt-4.1-mini", ServiceTier: "*", MinInputTokens: 128000, InputPricePerMillion: 0.8, OutputPricePerMillion: 3.2, Enabled: true},
		{Provider: "openai", Model: "gpt-4.1-mini", ServiceTier: "flex", MinInputTokens: 0, InputPricePerMillion: 0.2, OutputPricePerMillion: 0.8, Enabled: true, Note: "internal rate card"},
		// Disabled rules are not offers.
		{Provider: "openai", Model: "mystery-model", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 9.9, Enabled: false},
		// A rule whose provider does not serve this model must not be quoted.
		{Provider: "anthropic", Model: "text-only-model", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 5, Enabled: true},
	}
	for i := range prices {
		if _, errCreate := handler.repo.CreateBillingModelPrice(ctx, prices[i]); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%d) error = %v", i, errCreate)
		}
	}
}

// seedUserModelUsage appends successful and failed observations for one model.
func seedUserModelUsage(t *testing.T, handler *Handler, modelID, apiKey string, successes, failures int) {
	t.Helper()

	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < successes; i++ {
		payload := fmt.Sprintf(
			`{"timestamp":%q,"provider":"openai","model":%q,"api_key":%q,"request_id":%q,"latency_ms":%d,"ttft_ms":%d,"failed":false,"tokens":{"input_tokens":1000,"output_tokens":500,"total_tokens":1500}}`,
			base.Add(time.Duration(i)*time.Second).Format(time.RFC3339), modelID, apiKey,
			fmt.Sprintf("req-%s-ok-%d", modelID, i), 2000+i*10, 300+i*5,
		)
		if _, errAppend := handler.repo.AppendUsage(ctx, payload, "192.0.2.10"); errAppend != nil {
			t.Fatalf("AppendUsage(success %d) error = %v", i, errAppend)
		}
	}
	for i := 0; i < failures; i++ {
		payload := fmt.Sprintf(
			`{"timestamp":%q,"provider":"openai","model":%q,"api_key":%q,"request_id":%q,"latency_ms":0,"failed":true,"tokens":{"input_tokens":1000,"output_tokens":0,"total_tokens":1000}}`,
			base.Add(time.Duration(successes+i)*time.Second).Format(time.RFC3339), modelID, apiKey,
			fmt.Sprintf("req-%s-fail-%d", modelID, i),
		)
		if _, errAppend := handler.repo.AppendUsage(ctx, payload, "192.0.2.10"); errAppend != nil {
			t.Fatalf("AppendUsage(failure %d) error = %v", i, errAppend)
		}
	}
}
