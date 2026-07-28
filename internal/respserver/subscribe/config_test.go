package subscribe

import (
	"context"
	"strings"
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
		SubscribeMembership: func(_ context.Context, protocol int, revision int64, takeover bool, _ string) (cluster.ConnectionLifetime, error) {
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
		SubscribeMembership: func(_ context.Context, protocol int, _ int64, takeover bool, _ string) (cluster.ConnectionLifetime, error) {
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
	var gotInstanceID string
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(_ context.Context, _ int, _ int64, takeover bool, instanceID string) (cluster.ConnectionLifetime, error) {
			gotTakeover = takeover
			gotInstanceID = instanceID
			return cluster.ConnectionLifetime{}, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config", "17", "takeover", "550e8400-e29b-41d4-a716-446655440000"})

	if !gotTakeover || gotInstanceID != "550e8400-e29b-41d4-a716-446655440000" || reply.Kind != dispatch.ReplyKindArray {
		t.Fatalf("takeover = %t instance ID = %q reply = %#v", gotTakeover, gotInstanceID, reply)
	}
}

func TestConfigSubscribeRejectsInvalidFourthArgument(t *testing.T) {
	called := false
	reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
		SubscribeMembership: func(context.Context, int, int64, bool, string) (cluster.ConnectionLifetime, error) {
			called = true
			return cluster.ConnectionLifetime{}, nil
		},
		SubscribeConfigYAML: func() (int64, error) { return 1, nil },
	}}, []string{"SUBSCRIBE", "config", "17", "invalid"})

	if called || reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("called = %t reply = %#v", called, reply)
	}
}

func TestConfigSubscribeMembershipProtocolArguments(t *testing.T) {
	const instanceID = "550e8400-e29b-41d4-a716-446655440000"
	tests := []struct {
		name         string
		args         []string
		wantProtocol int
		wantRevision int64
		wantTakeover bool
		wantInstance string
		wantError    string
	}{
		{name: "protocol zero", args: []string{"SUBSCRIBE", "config"}},
		{name: "legacy normal", args: []string{"SUBSCRIBE", "config", "7"}, wantProtocol: 1, wantRevision: 7},
		{name: "secure normal", args: []string{"SUBSCRIBE", "config", "7", instanceID}, wantProtocol: 1, wantRevision: 7, wantInstance: instanceID},
		{name: "secure takeover", args: []string{"SUBSCRIBE", "config", "7", "takeover", instanceID}, wantProtocol: 1, wantRevision: 7, wantTakeover: true, wantInstance: instanceID},
		{name: "invalid revision", args: []string{"SUBSCRIBE", "config", "0"}, wantError: "ERR invalid lifecycle configuration revision"},
		{name: "empty instance", args: []string{"SUBSCRIBE", "config", "7", ""}, wantError: "ERR invalid membership instance ID"},
		{name: "invalid instance", args: []string{"SUBSCRIBE", "config", "7", "not-a-uuid"}, wantError: "ERR invalid membership instance ID"},
		{name: "long instance", args: []string{"SUBSCRIBE", "config", "7", strings.Repeat("a", 65)}, wantError: "ERR invalid membership instance ID"},
		{name: "bare takeover", args: []string{"SUBSCRIBE", "config", "7", "takeover"}, wantError: "ERR invalid membership instance ID"},
		{name: "invalid takeover mode", args: []string{"SUBSCRIBE", "config", "7", "replace", instanceID}, wantError: "ERR invalid membership takeover mode"},
		{name: "too few", args: []string{"SUBSCRIBE"}, wantError: "ERR wrong number of arguments for 'subscribe' command"},
		{name: "too many", args: []string{"SUBSCRIBE", "config", "7", "takeover", instanceID, "extra"}, wantError: "ERR wrong number of arguments for 'subscribe' command"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			var gotProtocol int
			var gotRevision int64
			var gotTakeover bool
			var gotInstance string
			reply := handleConfig(context.Background(), dispatch.Env{Conn: &dispatch.ConnEnv{
				SubscribeMembership: func(_ context.Context, protocol int, revision int64, takeover bool, instance string) (cluster.ConnectionLifetime, error) {
					called = true
					gotProtocol, gotRevision, gotTakeover, gotInstance = protocol, revision, takeover, instance
					return cluster.ConnectionLifetime{}, nil
				},
				SubscribeConfigYAML: func() (int64, error) { return 1, nil },
			}}, testCase.args)
			if testCase.wantError != "" {
				if called || reply.Kind != dispatch.ReplyKindRedisError || reply.RedisError != testCase.wantError {
					t.Fatalf("called = %t reply = %#v", called, reply)
				}
				return
			}
			if !called || reply.Kind != dispatch.ReplyKindArray || gotProtocol != testCase.wantProtocol || gotRevision != testCase.wantRevision || gotTakeover != testCase.wantTakeover || gotInstance != testCase.wantInstance {
				t.Fatalf("called = %t reply = %#v membership = (%d, %d, %t, %q)", called, reply, gotProtocol, gotRevision, gotTakeover, gotInstance)
			}
		})
	}
}
