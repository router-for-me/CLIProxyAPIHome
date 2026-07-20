package push

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func handleInFlight(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if len(args) != 3 {
		return dispatch.Err("wrong number of arguments for 'lpush' command")
	}
	if env.Runtime == nil {
		return dispatch.Err("runtime not ready")
	}
	payload := strings.TrimSpace(args[2])
	if payload == "" || !gjson.Valid(payload) {
		return dispatch.Err("invalid in-flight json")
	}
	leaseID := strings.TrimSpace(gjson.Get(payload, "lease_id").String())
	if leaseID == "" {
		return dispatch.Err("lease_id is required")
	}
	action := strings.ToLower(strings.TrimSpace(gjson.Get(payload, "action").String()))
	switch action {
	case "renew":
		renewed, errRenew := env.Runtime.RenewInFlightLease(ctx, leaseID, env.NodeID)
		if errRenew != nil {
			return dispatch.Err(errRenew.Error())
		}
		if !renewed {
			return dispatch.Integer(0)
		}
		return dispatch.Integer(1)
	case "release":
		reason := strings.TrimSpace(gjson.Get(payload, "reason").String())
		_, errRelease := env.Runtime.ReleaseInFlightLease(ctx, leaseID, env.NodeID, reason)
		if errRelease != nil {
			return dispatch.Err(errRelease.Error())
		}
		return dispatch.Integer(1)
	default:
		return dispatch.Err("unsupported in-flight action")
	}
}
