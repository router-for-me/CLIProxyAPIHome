package userapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// Modality reporting states. A buyer deciding whether a model can read an image
// must be able to tell "this model does not accept images" apart from "nobody
// told us what this model accepts"; collapsing the two into an empty list would
// quietly present the second as the first.
const (
	modalityStatusKnown   = "known"
	modalityStatusUnknown = "unknown"
)

// Capability reporting states, carrying the same three-way distinction as
// modalities: stated support, stated absence, and silence.
const (
	capabilityStatusSupported   = "supported"
	capabilityStatusUnsupported = "unsupported"
	capabilityStatusUnknown     = "unknown"
)

// toolCallingParameterNames are the parameter spellings that mean a model can be
// driven with tool definitions. Providers disagree on the name, so membership is
// checked against every spelling the catalog actually uses.
var toolCallingParameterNames = map[string]struct{}{
	"tools":         {},
	"tool_choice":   {},
	"functions":     {},
	"function_call": {},
}

// reasoningParameterNames are the parameter spellings that mean a model exposes
// internal reasoning. They are only consulted when the model carries no thinking
// budget definition, which is the stronger signal.
var reasoningParameterNames = map[string]struct{}{
	"reasoning":         {},
	"reasoning_effort":  {},
	"include_reasoning": {},
	"thinking":          {},
}

// structuredOutputParameterNames are the parameter spellings that mean a model
// can be constrained to a caller-supplied schema. Like tool calling, this is
// resolved server-side: a client that inferred it from the parameter list would
// be synthesising a capability claim the cluster never made.
var structuredOutputParameterNames = map[string]struct{}{
	"response_format":    {},
	"structured_outputs": {},
	"json_schema":        {},
	"json_mode":          {},
	"response_schema":    {},
}

// Reasons a buyer's accessible catalog looks the way it does. The list being
// empty is never self-explanatory, and a console that cannot say why would leave
// the buyer guessing whether the cluster is empty or their key is narrow.
const (
	modelAccessReasonUnrestricted = "unrestricted"
	modelAccessReasonModelGroups  = "model_groups"
	modelAccessReasonNoAPIKeys    = "no_api_keys"
)

// ListModels returns the public model catalog: every model the cluster can
// currently serve, described in buyer terms.
//
// The route is unauthenticated on purpose — a visitor has to see what is on
// offer before deciding to hold an account — and it is deliberately narrower
// than the signed-in view. Prices and observed performance are withheld here,
// not because they are secret in principle but because they are the operator's
// commercial terms with a specific buyer; the visitor sees capability, the
// account holder sees the offer. Nothing here exposes the registry's internal
// registration shape, so credentials, auth identifiers and per-client counts
// stay out of reach.
func (h *Handler) ListModels(c *gin.Context) {
	models := availableUserModels()

	items := make([]gin.H, 0, len(models))
	for _, model := range models {
		items = append(items, userModelResponse(model, nil))
	}

	c.JSON(http.StatusOK, gin.H{"models": items, "total": len(items)})
}

// ListAccessibleModels returns the subset of the catalog the caller's own API
// keys are allowed to call.
//
// It is deliberately separate from the public catalog rather than a query
// parameter on it: the two answer different questions ("what does this cluster
// sell" versus "what can I call today"), and only one of them may be answered
// without a token.
func (h *Handler) ListAccessibleModels(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()

	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}

	access, errAccess := h.repo.UserModelAccess(ctx, user.ID)
	if errAccess != nil {
		respondError(c, http.StatusInternalServerError, "model_access_load_failed", errAccess)
		return
	}

	models := filterModelsForAccess(availableUserModels(), access)
	modelIDs := make([]string, 0, len(models))
	for _, model := range models {
		modelIDs = append(modelIDs, model.ID)
	}

	prices, errPrices := h.modelPriceIndex(ctx, modelIDs)
	if errPrices != nil {
		respondError(c, http.StatusInternalServerError, "model_price_load_failed", errPrices)
		return
	}

	availability, bounds, errAvailability := h.modelAvailabilityIndex(ctx, modelIDs)
	if errAvailability != nil {
		respondError(c, http.StatusInternalServerError, "model_availability_load_failed", errAvailability)
		return
	}

	items := make([]gin.H, 0, len(models))
	for _, model := range models {
		item := userModelResponse(model, prices)
		item["availability"] = availabilityFor(availability, model.ID, bounds)
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{
		"models": items,
		"total":  len(items),
		"access": gin.H{
			"restricted":    access.Restricted,
			"api_key_count": access.APIKeyCount,
			"reason":        modelAccessReason(access),
		},
	})
}

// availableUserModels returns the registry's currently servable definitions with
// the unusable entries dropped, so every caller starts from the same list.
func availableUserModels() []*registry.ModelInfo {
	definitions := registry.GetGlobalRegistry().GetAvailableModelDefinitions()
	models := make([]*registry.ModelInfo, 0, len(definitions))
	for _, model := range definitions {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		models = append(models, model)
	}
	return models
}

// filterModelsForAccess narrows the catalog to what the user's keys allow.
// Holding no key allows nothing: an account without a credential cannot call
// the cluster, and listing the full catalog as "accessible" would say otherwise.
func filterModelsForAccess(models []*registry.ModelInfo, access cluster.UserModelAccess) []*registry.ModelInfo {
	if !access.Restricted {
		return models
	}
	if access.APIKeyCount == 0 || len(access.ModelIDs) == 0 {
		return nil
	}

	allowed := make(map[string]struct{}, len(access.ModelIDs))
	for _, modelID := range access.ModelIDs {
		key := strings.ToLower(strings.TrimSpace(coreauth.CanonicalModelID(modelID)))
		if key == "" {
			continue
		}
		allowed[key] = struct{}{}
	}

	filtered := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		// Model groups store canonical identifiers, so the registry identifier
		// is canonicalised too rather than compared raw; otherwise a suffixed
		// registration would never match the group that permits it.
		key := strings.ToLower(strings.TrimSpace(coreauth.CanonicalModelID(model.ID)))
		if _, ok := allowed[key]; !ok {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

// modelAccessReason names why the accessible catalog has the shape it has.
func modelAccessReason(access cluster.UserModelAccess) string {
	if !access.Restricted {
		return modelAccessReasonUnrestricted
	}
	if access.APIKeyCount == 0 {
		return modelAccessReasonNoAPIKeys
	}
	return modelAccessReasonModelGroups
}

// modelPriceIndex loads the enabled price rules for exactly the models being
// returned, in one query.
func (h *Handler) modelPriceIndex(ctx context.Context, modelIDs []string) (modelPriceIndex, error) {
	if len(modelIDs) == 0 {
		return modelPriceIndex{}, nil
	}
	records, errRecords := h.repo.ListEnabledBillingModelPricesForModels(ctx, modelIDs)
	if errRecords != nil {
		return nil, errRecords
	}
	return newModelPriceIndex(records), nil
}

// userModelResponse redacts a registry definition into the buyer-facing shape.
// Fields the registry never learned are omitted rather than zero-filled: a
// missing context window is unknown, not a context window of zero.
func userModelResponse(model *registry.ModelInfo, prices modelPriceIndex) gin.H {
	providers := copyNonEmptyStrings(model.Providers)
	entry := gin.H{
		"id":           strings.TrimSpace(model.ID),
		"modalities":   userModelModalities(model),
		"capabilities": userModelCapabilities(model),
	}
	// A nil index means the caller is not entitled to pricing at all, which is
	// not the same as a model nobody priced. The key is absent rather than
	// present-and-unpublished so the two never blur together.
	if prices != nil {
		entry["pricing"] = prices.pricingFor(model.ID, providers)
	}
	if displayName := strings.TrimSpace(model.DisplayName); displayName != "" {
		entry["display_name"] = displayName
	}
	if description := strings.TrimSpace(model.Description); description != "" {
		entry["description"] = description
	}
	if version := strings.TrimSpace(model.Version); version != "" {
		entry["version"] = version
	}
	if ownedBy := strings.TrimSpace(model.OwnedBy); ownedBy != "" {
		entry["owned_by"] = ownedBy
	}
	if modelType := strings.TrimSpace(model.Type); modelType != "" {
		entry["type"] = modelType
	}
	// Providers are the identifiers that appear on usage records and billing
	// rules, which makes them the only key a client can use to line a model up
	// with what it will be charged for.
	if len(providers) > 0 {
		entry["providers"] = providers
	}
	if contextLength := userModelContextLength(model); contextLength > 0 {
		entry["context_length"] = contextLength
	}
	if maxOutputTokens := userModelMaxOutputTokens(model); maxOutputTokens > 0 {
		entry["max_output_tokens"] = maxOutputTokens
	}
	return entry
}

// userModelContextLength collapses the two limit spellings the shared catalog
// uses. OpenAI-shaped entries carry context_length; Gemini-shaped entries carry
// inputTokenLimit. Clients should not have to know which family a model is from.
func userModelContextLength(model *registry.ModelInfo) int {
	if model.ContextLength > 0 {
		return model.ContextLength
	}
	return model.InputTokenLimit
}

// userModelMaxOutputTokens is the output-side counterpart to
// userModelContextLength: max_completion_tokens and outputTokenLimit name the
// same limit.
func userModelMaxOutputTokens(model *registry.ModelInfo) int {
	if model.MaxCompletionTokens > 0 {
		return model.MaxCompletionTokens
	}
	return model.OutputTokenLimit
}

// userModelModalities reports what the model accepts and produces. An empty pair
// is reported as explicitly unknown so a client can say "not published" instead
// of rendering a text-only model that may well accept images.
//
// The values originate in the hand-maintained model catalog, so they are
// normalized here rather than trusted verbatim: a stray "TEXT" or a duplicated
// entry is an editing slip in a JSON file, not a fact worth showing a buyer.
func userModelModalities(model *registry.ModelInfo) gin.H {
	input := normalizeModalities(model.SupportedInputModalities)
	output := normalizeModalities(model.SupportedOutputModalities)
	if len(input) == 0 && len(output) == 0 {
		return gin.H{"status": modalityStatusUnknown}
	}

	payload := gin.H{"status": modalityStatusKnown}
	if len(input) > 0 {
		payload["input"] = input
	}
	if len(output) > 0 {
		payload["output"] = output
	}
	return payload
}

// normalizeModalities lower-cases and de-duplicates catalog modality values
// while preserving the order the catalog listed them in, so the primary modality
// stays first. It never adds a modality the catalog did not state.
func normalizeModalities(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		modality := strings.ToLower(strings.TrimSpace(value))
		if modality == "" {
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		normalized = append(normalized, modality)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// userModelCapabilities reports the non-modality capabilities a buyer selects on.
func userModelCapabilities(model *registry.ModelInfo) gin.H {
	payload := gin.H{
		"reasoning":         userModelReasoning(model),
		"tool_calling":      gin.H{"status": parameterCapabilityStatus(model.SupportedParameters, toolCallingParameterNames)},
		"structured_output": gin.H{"status": parameterCapabilityStatus(model.SupportedParameters, structuredOutputParameterNames)},
	}
	if parameters := copyNonEmptyStrings(model.SupportedParameters); len(parameters) > 0 {
		payload["parameters"] = parameters
	}
	if methods := copyNonEmptyStrings(model.SupportedGenerationMethods); len(methods) > 0 {
		payload["generation_methods"] = methods
	}
	return payload
}

// userModelReasoning describes internal reasoning support. A declared thinking
// budget is proof of support and carries the range a caller may request; without
// one the parameter list is the only remaining evidence.
func userModelReasoning(model *registry.ModelInfo) gin.H {
	if model.Thinking == nil {
		return gin.H{"status": parameterCapabilityStatus(model.SupportedParameters, reasoningParameterNames)}
	}

	payload := gin.H{"status": capabilityStatusSupported}
	if levels := copyNonEmptyStrings(model.Thinking.Levels); len(levels) > 0 {
		payload["levels"] = levels
	}
	budget := gin.H{}
	if model.Thinking.Min > 0 {
		budget["min"] = model.Thinking.Min
	}
	if model.Thinking.Max > 0 {
		budget["max"] = model.Thinking.Max
	}
	if model.Thinking.ZeroAllowed {
		budget["can_disable"] = true
	}
	if model.Thinking.DynamicAllowed {
		budget["dynamic"] = true
	}
	if len(budget) > 0 {
		payload["budget"] = budget
	}
	return payload
}

// parameterCapabilityStatus reads a capability off the model's parameter list.
// A model that publishes parameters and omits the capability has said no; a
// model that publishes no parameters at all has said nothing, and the difference
// is preserved rather than resolved by guessing.
func parameterCapabilityStatus(parameters []string, names map[string]struct{}) string {
	stated := false
	for _, parameter := range parameters {
		normalized := strings.ToLower(strings.TrimSpace(parameter))
		if normalized == "" {
			continue
		}
		stated = true
		if _, ok := names[normalized]; ok {
			return capabilityStatusSupported
		}
	}
	if !stated {
		return capabilityStatusUnknown
	}
	return capabilityStatusUnsupported
}

// copyNonEmptyStrings returns a defensive copy with blanks dropped, so a
// response never shares backing storage with the live registry and never carries
// an empty string a client would have to filter out itself.
func copyNonEmptyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copied := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		copied = append(copied, trimmed)
	}
	if len(copied) == 0 {
		return nil
	}
	return copied
}
