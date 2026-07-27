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
	var gotTakeover bool
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(_ context.Context, protocol int, revision int64, takeover bool) (cluster.ConnectionLifetime, error) {
			gotProtocol = protocol
			gotRevision = revision
			gotTakeover = takeover
			return lifetime, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config", "17"})

	if gotProtocol != 1 || gotRevision != 17 || gotTakeover {
		t.Fatalf("membership request = protocol %d revision %d takeover %t", gotProtocol, gotRevision, gotTakeover)
	}
	if reply.SubscriptionLifetime == nil || *reply.SubscriptionLifetime != lifetime {
		t.Fatalf("reply subscription lifetime = %#v", reply.SubscriptionLifetime)
	}
}

func TestConfigSubscribeUsesProtocolZeroWithoutRevision(t *testing.T) {
	var gotProtocol int
	called := false
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(_ context.Context, protocol int, _ int64, takeover bool) (cluster.ConnectionLifetime, error) {
			called = true
			gotProtocol = protocol
			if takeover {
				t.Fatal("protocol zero subscription requested takeover")
			}
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

func TestConfigSubscribePassesTakeover(t *testing.T) {
	var gotTakeover bool
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(_ context.Context, _ int, _ int64, takeover bool) (cluster.ConnectionLifetime, error) {
			gotTakeover = takeover
			return cluster.ConnectionLifetime{}, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config", "17", "takeover"})

	if !gotTakeover || reply.Kind != dispatch.ReplyKindArray {
		t.Fatalf("takeover = %t reply = %#v", gotTakeover, reply)
	}
}

func TestConfigSubscribeRejectsInvalidFourthArgument(t *testing.T) {
	called := false
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(context.Context, int, int64, bool) (cluster.ConnectionLifetime, error) {
			called = true
			return cluster.ConnectionLifetime{}, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config", "17", "invalid"})

	if called || reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("called = %t reply = %#v", called, reply)
	}
}
