package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type concurrencyPolicyPatchRequest struct {
	Version            *int64                        `json:"version"`
	MaxInFlight        cluster.OptionalLimit         `json:"max_in_flight"`
	MaxInFlightByModel cluster.OptionalModelLimitMap `json:"max_in_flight_by_model"`
}

type credentialModelLimiter struct {
	Model            string `json:"model"`
	MaxInFlight      int64  `json:"max_in_flight"`
	AdmittedInFlight int64  `json:"admitted_in_flight"`
	Remaining        int64  `json:"remaining"`
	Saturated        bool   `json:"saturated"`
}

type credentialConcurrencyResponse struct {
	CredentialID       string                   `json:"credential_id"`
	MaxInFlight        *int64                   `json:"max_in_flight"`
	AdmittedInFlight   int64                    `json:"admitted_in_flight"`
	Remaining          *int64                   `json:"remaining"`
	TotalSaturated     bool                     `json:"total_saturated"`
	Models             []credentialModelLimiter `json:"models"`
	PolicyVersion      int64                    `json:"policy_version"`
	EffectiveAt        time.Time                `json:"effective_at"`
	ObservationBarrier int64                    `json:"observation_barrier_revision"`
	FullyEnforced      string                   `json:"fully_enforced"`
	Observed           any                      `json:"observed"`
}

func (h *Handler) ListCredentialConcurrencyPolicies(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	policies, errPolicies := h.repo.ListCredentialConcurrencyPolicies(ctx)
	if errPolicies != nil {
		respondError(c, http.StatusInternalServerError, "concurrency_policy_load_failed", errPolicies)
		return
	}
	items := make([]gin.H, 0, len(policies))
	for index := range policies {
		items = append(items, concurrencyPolicyResponse(policies[index]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) GetCredentialConcurrencyPolicy(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	policy, errPolicy := h.repo.GetCredentialConcurrencyPolicy(ctx, c.Param("credential_id"))
	if errPolicy != nil {
		respondConcurrencyPolicyError(c, errPolicy)
		return
	}
	c.JSON(http.StatusOK, concurrencyPolicyResponse(policy))
}

func (h *Handler) PatchCredentialConcurrencyPolicy(c *gin.Context) {
	var request concurrencyPolicyPatchRequest
	if errBind := c.ShouldBindJSON(&request); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	if !request.MaxInFlight.Set && !request.MaxInFlightByModel.Set {
		respondError(c, http.StatusBadRequest, "no fields to update", nil)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	credentialID := strings.TrimSpace(c.Param("credential_id"))
	errPatch := h.repo.PatchAuthAndConcurrency(ctx, cluster.AuthConcurrencyPatchRequest{
		CredentialID:          credentialID,
		PolicyPatch:           cluster.ConcurrencyPolicyPatch{MaxInFlight: request.MaxInFlight, MaxInFlightByModel: request.MaxInFlightByModel},
		ExpectedPolicyVersion: request.Version,
	})
	if errPatch != nil {
		respondConcurrencyPolicyError(c, errPatch)
		return
	}
	policy, errPolicy := h.repo.GetCredentialConcurrencyPolicy(ctx, credentialID)
	if errPolicy != nil {
		respondError(c, http.StatusInternalServerError, "concurrency_policy_load_failed", errPolicy)
		return
	}
	c.JSON(http.StatusOK, concurrencyPolicyResponse(policy))
}

func (h *Handler) GetCredentialConcurrency(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	states, errState := h.repo.ReadConcurrencyState(ctx)
	if errState != nil {
		respondError(c, http.StatusInternalServerError, "concurrency_state_load_failed", errState)
		return
	}
	items := make([]credentialConcurrencyResponse, 0, len(states))
	for index := range states {
		items = append(items, credentialConcurrencyStateResponse(states[index], nil, cluster.InFlightObservedCredentialItem{}, false))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func concurrencyPolicyResponse(policy cluster.CredentialConcurrencyPolicy) gin.H {
	return gin.H{
		"credential_id":                policy.CredentialID,
		"max_in_flight":                policy.MaxInFlight,
		"max_in_flight_by_model":       policy.MaxInFlightByModel,
		"version":                      policy.Version,
		"effective_at":                 policy.EffectiveAt,
		"observation_barrier_revision": policy.ObservationBarrierRevision,
	}
}

func credentialConcurrencyStateResponse(state cluster.CredentialConcurrencyState, observation *cluster.InFlightObservationReadModel, observed cluster.InFlightObservedCredentialItem, observedOK bool) credentialConcurrencyResponse {
	models := make([]credentialModelLimiter, 0, len(state.Models))
	for index := range state.Models {
		model := state.Models[index]
		models = append(models, credentialModelLimiter{
			Model:            model.Model,
			MaxInFlight:      model.MaxInFlight,
			AdmittedInFlight: model.AdmittedInFlight,
			Remaining:        remainingConcurrencyLimit(model.MaxInFlight, model.AdmittedInFlight),
			Saturated:        model.AdmittedInFlight >= model.MaxInFlight,
		})
	}
	var remaining *int64
	if state.MaxInFlight != nil {
		value := remainingConcurrencyLimit(*state.MaxInFlight, state.AdmittedInFlight)
		remaining = &value
	}
	response := credentialConcurrencyResponse{
		CredentialID:       state.CredentialID,
		MaxInFlight:        state.MaxInFlight,
		AdmittedInFlight:   state.AdmittedInFlight,
		Remaining:          remaining,
		TotalSaturated:     state.MaxInFlight != nil && state.AdmittedInFlight >= *state.MaxInFlight,
		Models:             models,
		PolicyVersion:      state.PolicyVersion,
		EffectiveAt:        state.EffectiveAt,
		ObservationBarrier: state.ObservationBarrier,
		FullyEnforced:      fullyEnforcedConcurrency(state, observation, observed, observedOK),
	}
	if observation != nil && observedOK {
		response.Observed = inFlightObservedResponse(*observation, observed)
	}
	return response
}

func remainingConcurrencyLimit(limit int64, admitted int64) int64 {
	remaining := limit - admitted
	if remaining < 0 {
		return 0
	}
	return remaining
}

func fullyEnforcedConcurrency(state cluster.CredentialConcurrencyState, observation *cluster.InFlightObservationReadModel, observed cluster.InFlightObservedCredentialItem, observedOK bool) string {
	if !observedOK || observation == nil || observation.ObservedAt == nil || observation.Stale || !observation.CoverageComplete || !observation.AggregatesComplete || !observation.ProtocolCoverageComplete || observation.MinimumProcessedBarrierRevision == nil || *observation.MinimumProcessedBarrierRevision < state.ObservationBarrier {
		return "unknown"
	}
	if observed.ObservedUnaccounted > 0 {
		return "false"
	}
	return "true"
}

func observedCredentialsByID(observation cluster.InFlightObservationReadModel) map[string]cluster.InFlightObservedCredentialItem {
	result := make(map[string]cluster.InFlightObservedCredentialItem, len(observation.Credentials))
	for index := range observation.Credentials {
		item := observation.Credentials[index]
		result[item.CredentialID] = item
	}
	return result
}

func applyConcurrencyStateToAuthFile(entry gin.H, state cluster.CredentialConcurrencyState, observation *cluster.InFlightObservationReadModel, observed cluster.InFlightObservedCredentialItem, observedOK bool) {
	if entry == nil || state.CredentialID == "" {
		return
	}
	response := credentialConcurrencyStateResponse(state, observation, observed, observedOK)
	entry["max_in_flight"] = response.MaxInFlight
	entry["max_in_flight_by_model"] = concurrencyModelLimits(state)
	entry["admitted_in_flight"] = response.AdmittedInFlight
	entry["remaining"] = response.Remaining
	entry["total_saturated"] = response.TotalSaturated
	saturatedModels := 0
	for index := range response.Models {
		if response.Models[index].Saturated {
			saturatedModels++
		}
	}
	entry["saturated_model_count"] = saturatedModels
	entry["limiter"] = response
}

func concurrencyModelLimits(state cluster.CredentialConcurrencyState) map[string]int64 {
	limits := make(map[string]int64, len(state.Models))
	for index := range state.Models {
		limits[state.Models[index].Model] = state.Models[index].MaxInFlight
	}
	return limits
}

func respondConcurrencyPolicyError(c *gin.Context, errValue error) {
	switch {
	case errors.Is(errValue, cluster.ErrConcurrencyCredentialNotFound):
		respondError(c, http.StatusNotFound, "not_found", errValue)
	case errors.Is(errValue, cluster.ErrConcurrencyPolicyVersionConflict):
		respondError(c, http.StatusConflict, "concurrency_policy_version_conflict", errValue)
	case errors.Is(errValue, cluster.ErrConcurrencyInvalidLimit), errors.Is(errValue, cluster.ErrConcurrencyInvalidModel), errors.Is(errValue, cluster.ErrConcurrencyDuplicateModel):
		respondError(c, http.StatusBadRequest, "invalid_concurrency_policy", errValue)
	default:
		respondError(c, http.StatusInternalServerError, "concurrency_policy_write_failed", errValue)
	}
}

func decodeConcurrencyPolicyPatchRequest(body map[string]json.RawMessage) (concurrencyPolicyPatchRequest, error) {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return concurrencyPolicyPatchRequest{}, errMarshal
	}
	var request concurrencyPolicyPatchRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return concurrencyPolicyPatchRequest{}, errUnmarshal
	}
	return request, nil
}
