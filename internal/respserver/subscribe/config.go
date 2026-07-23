package subscribe

import (
	"context"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

// handleConfig handles a config.
func handleConfig(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if len(args) != 2 && len(args) != 3 {
		return dispatch.Err("wrong number of arguments for 'subscribe' command")
	}

	if env.Conn == nil || env.Conn.SubscribeConfigYAML == nil {
		return dispatch.Err("subscribe not supported")
	}

	protocolVersion := 0
	lifecycleConfigRevision := int64(0)
	if len(args) == 3 {
		parsedRevision, errParse := strconv.ParseInt(strings.TrimSpace(args[2]), 10, 64)
		if errParse != nil || parsedRevision < 1 {
			return dispatch.Err("invalid lifecycle configuration revision")
		}
		protocolVersion = 1
		lifecycleConfigRevision = parsedRevision
	}

	var lifetimeMetadata *cluster.ConnectionLifetime
	if env.Conn.IsSubscribed != nil && env.Conn.IsSubscribed() {
		if lifetime, attached := env.Conn.SubscriptionLifetime(); attached {
			lifetimeMetadata = &lifetime
		}
	} else if env.Conn.SubscribeMembership != nil {
		lifetime, errMembership := env.Conn.SubscribeMembership(ctx, protocolVersion, lifecycleConfigRevision)
		if errMembership != nil {
			return dispatch.Err(errMembership.Error())
		}
		lifetimeMetadata = &lifetime
	}

	count, errSub := env.Conn.SubscribeConfigYAML()
	if errSub != nil {
		return dispatch.Err(errSub.Error())
	}

	return dispatch.Reply{
		Kind:                 dispatch.ReplyKindArray,
		SubscriptionLifetime: lifetimeMetadata,
		Array: []dispatch.Reply{
			dispatch.BulkString([]byte("subscribe")),
			dispatch.BulkString([]byte("config")),
			dispatch.Integer(count),
		},
	}
}
