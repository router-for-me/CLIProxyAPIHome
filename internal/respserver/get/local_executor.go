package get

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func handleLocalExecutorDispatch(ctx context.Context, env dispatch.Env, policy *coreauth.ModelAccess, caller *access.Result, model string) dispatch.Reply {
	model = coreauth.CanonicalModelID(model)
	if !strings.HasPrefix(model, home.LocalExecutorPrefix) {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.TypeModelNotFound + ": model not found")))
	}
	routes, credentialIDs, errRoutes := env.Runtime.LocalExecutorRoutes(ctx, model)
	if errRoutes != nil {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageError)))
	}
	if credentialIDs != nil {
		allowed := slices.ContainsFunc(credentialIDs, func(id string) bool { return policy.Allows(id, model) })
		if len(credentialIDs) == 0 {
			allowed = policy.Allows("", model)
		}
		if !allowed {
			return dispatch.BulkString([]byte(buildErrorJSON("model_forbidden: model is outside the API key's scope")))
		}
		return dispatch.BulkString([]byte(`{"kind":"credential"}`))
	}
	route, found := routes[model]
	if !found {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.TypeModelNotFound + ": model not found")))
	}
	if !policy.Allows("", model) {
		return dispatch.BulkString([]byte(buildErrorJSON("model_forbidden: model is outside the API key's scope")))
	}
	if env.NodeID == "" || env.ClientCertificateFingerprint == "" {
		return dispatch.BulkString([]byte(buildErrorJSON("local_executor_unavailable: CPA identity is unavailable")))
	}
	active, errActive := env.Runtime.PluginSyncNodeActive(ctx, env.NodeID, env.ClientCertificateFingerprint)
	if errActive != nil {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageError)))
	}
	if !active {
		return dispatch.BulkString([]byte(buildErrorJSON("local_executor_unavailable: CPA identity is unavailable")))
	}
	allowed, errAdmit := env.Runtime.AdmitLocalExecutor(ctx, caller.Principal, model)
	if errAdmit != nil {
		errorType, _ := homeerrors.SplitRedisErrorMessage(errAdmit.Error())
		if errorType == homeerrors.TypeUserCreditsInsufficient || errorType == homeerrors.TypeUserPeriodLimitExceeded {
			return dispatch.BulkString([]byte(buildErrorJSON(errAdmit.Error())))
		}
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageError)))
	}
	if !allowed {
		return dispatch.BulkString([]byte(buildErrorJSON("model_forbidden: model is outside the API key's scope")))
	}
	raw, errMarshal := json.Marshal(struct {
		Kind         string `json:"kind"`
		Plugin       string `json:"plugin"`
		Version      string `json:"version"`
		ConfigDigest string `json:"config_digest"`
		Model        string `json:"model"`
		Principal    string `json:"principal"`
		Access       string `json:"access_provider"`
	}{"local-plugin", route.Plugin, route.Version, route.ConfigDigest, route.Model.SourceModelID, caller.Principal, caller.Provider})
	if errMarshal != nil {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageError)))
	}
	return dispatch.SensitiveBulkString(raw)
}
