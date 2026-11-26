package home

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
)

type apiKeyModelScopedUsageStore interface {
	AllowedUsageIDsForAPIKeyModel(context.Context, string, string) ([]string, []string, error)
}

func (r *Runtime) accountReusedUsage(ctx context.Context, payload string) error {
	if !r.coreManager.UsesWeightedRouting() {
		return nil
	}
	selection := ojson.Get(payload, "home_selection")
	if !selection.IsObject() || selection.Get("reused").Type != ojson.True || selection.Get("additional").Type == ojson.True {
		return nil
	}
	model := strings.TrimSpace(selection.Get("model").String())
	executionID := strings.TrimSpace(ojson.Get(payload, "execution_id").String())
	authIndex := strings.TrimSpace(ojson.Get(payload, "auth_index").String())
	if model == "" || executionID == "" || authIndex == "" {
		return nil
	}
	policy, validPolicy := coreauth.NormalizeCredentialPolicy(selection.Get("credential_policy").String())
	if !validPolicy {
		return nil
	}
	// Only primary usage charges a reused execution. Additional-model billing
	// records have separate IDs but do not represent extra scheduled requests.
	// Deduplicate primary redelivery by execution ID, not the shared WS request ID.
	eventID := ojson.Get(payload, "cpa_node_id").String() + "\x00" + executionID
	if !r.coreManager.CanAccountReusedSelection(ctx, model, authIndex, policy, eventID) {
		return nil
	}
	metadata := map[string]any{coreauth.CredentialPolicyMetadataKey: policy}
	if apiKey := strings.TrimSpace(ojson.Get(payload, "api_key").String()); apiKey != "" && r.clusterAdapter != nil {
		store, ok := r.clusterAdapter.(apiKeyModelScopedUsageStore)
		if !ok {
			return fmt.Errorf("home runtime: usage access policy unavailable")
		}
		allowedAuthIDs, allowedModelIDs, errAllowed := store.AllowedUsageIDsForAPIKeyModel(ctx, apiKey, coreauth.CanonicalModelID(model))
		if errors.Is(errAllowed, homeerrors.ErrUsageAPIKeyNotFound) {
			// Completed usage cannot reconstruct the deleted key's permissions.
			// Skip only weighted compensation; the caller must still apply result
			// feedback and acknowledge the already-persisted execution.
			return nil
		}
		if errAllowed != nil {
			return errAllowed
		}
		if allowedAuthIDs != nil {
			metadata[coreauth.AllowedAuthIDsMetadataKey] = allowedAuthIDs
		}
		if allowedModelIDs != nil {
			metadata[coreauth.AllowedModelIDsMetadataKey] = allowedModelIDs
		}
	}
	r.coreManager.AccountReusedSelection(ctx, nil, model, authIndex, eventID, coreauth.Options{Metadata: metadata})
	return nil
}
