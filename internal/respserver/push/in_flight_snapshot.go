package push

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

// handleInFlightSnapshot ingests one bounded in-flight observation frame.
func handleInFlightSnapshot(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if len(args) != 3 || !strings.EqualFold(strings.TrimSpace(args[0]), "LPUSH") || !strings.EqualFold(strings.TrimSpace(args[1]), "in-flight-snapshot") {
		return dispatch.Err("wrong number of arguments for 'lpush' command")
	}
	if ctx == nil || env.InFlightStore == nil || env.Conn == nil || !env.ConnectionLifetime.Controlled {
		return dispatch.Err("in-flight snapshot connection is not active")
	}

	identity := cluster.InFlightIngestIdentity{
		CertificateFingerprint: strings.TrimSpace(env.ConnectionLifetime.Fingerprint),
		NodeID:                 strings.TrimSpace(env.NodeID),
		MembershipConnectedAt:  env.ConnectionLifetime.ConnectedAt,
	}
	if identity.CertificateFingerprint == "" || identity.MembershipConnectedAt.IsZero() {
		return dispatch.Err("in-flight snapshot identity is unavailable")
	}

	if _, errIngest := env.InFlightStore.IngestInFlightFrame(ctx, identity, []byte(args[2]), env.InFlightLimits); errIngest != nil {
		return dispatch.Err("in-flight snapshot rejected")
	}
	return dispatch.Integer(1)
}
