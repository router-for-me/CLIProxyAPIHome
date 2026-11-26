package dynamic

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func handleSchedulerContinuity(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if env.Runtime == nil {
		return dispatch.Err("runtime not ready")
	}
	raw, ok := dispatch.ExtractJSONArgument(args, 1)
	if !ok {
		return dispatch.Err("invalid scheduler continuity request")
	}
	var query home.SchedulerContinuityQuery
	if errUnmarshal := json.Unmarshal([]byte(raw), &query); errUnmarshal != nil {
		return dispatch.Err("invalid scheduler continuity json")
	}
	ctx = cluster.WithAPIKeyRequestCache(ctx)
	principal, authErr := env.Runtime.Authenticate(ctx, http.Header(query.Request.Options.Headers))
	if authErr != nil {
		return dispatch.BulkString([]byte(buildAuthErrorJSON(authErr)))
	}
	apiKey := ""
	if principal != nil {
		apiKey = principal.Principal
	}
	response, handled, errEvaluate := env.Runtime.ObserveSchedulerContinuity(ctx, query, apiKey)
	if errEvaluate != nil {
		return dispatch.BulkString([]byte(buildDispatchErrorJSON(env.Runtime, errEvaluate)))
	}
	payload, errMarshal := json.Marshal(struct {
		Response pluginapi.SchedulerContinuityResponse `json:"response"`
		Handled  bool                                  `json:"handled"`
	}{Response: response, Handled: handled})
	if errMarshal != nil {
		return dispatch.Err("encode scheduler continuity response failed")
	}
	return dispatch.BulkString(payload)
}
