package subscribe

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestConfigSubscribeUsesProtocolOneRevisionAndReturnsLifetime(t *testing.T) {
	lifetime := cluster.ConnectionLifetime{
		Fingerprint:  "fp-a",
		ConnectedAt:  time.Unix(1, 0).UTC(),
		Home:         cluster.HomeIncarnationID{IP: "127.0.0.1", Port: 8317, StartedAt: time.Unix(2, 0).UTC()},
		Subscription: true,
	}
	var gotProtocol int
	var gotRevision int64
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(_ context.Context, protocol int, revision int64) (cluster.ConnectionLifetime, error) {
			gotProtocol = protocol
			gotRevision = revision
			return lifetime, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config", "17"})

	if gotProtocol != 1 || gotRevision != 17 {
		t.Fatalf("membership request = protocol %d revision %d", gotProtocol, gotRevision)
	}
	if reply.SubscriptionLifetime == nil || *reply.SubscriptionLifetime != lifetime {
		t.Fatalf("reply subscription lifetime = %#v", reply.SubscriptionLifetime)
	}
}

func TestConfigSubscribeUsesProtocolZeroWithoutRevision(t *testing.T) {
	var gotProtocol int
	called := false
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(_ context.Context, protocol int, _ int64) (cluster.ConnectionLifetime, error) {
			called = true
			gotProtocol = protocol
			return cluster.ConnectionLifetime{}, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config"})

	if !called || gotProtocol != 0 {
		t.Fatalf("membership request = called %t protocol %d", called, gotProtocol)
	}
	if reply.Kind != dispatch.ReplyKindArray {
		t.Fatalf("reply = %#v", reply)
	}
}
