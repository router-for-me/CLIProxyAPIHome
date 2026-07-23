package push

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

type recordingInFlightStore struct {
	identity cluster.InFlightIngestIdentity
	raw      []byte
	limits   cluster.InFlightLimits
	ctx      context.Context
	err      error
}

func (s *recordingInFlightStore) IngestInFlightFrame(ctx context.Context, identity cluster.InFlightIngestIdentity, raw []byte, limits cluster.InFlightLimits) (cluster.InFlightIngestResult, error) {
	s.ctx = ctx
	s.identity = identity
	s.raw = append([]byte(nil), raw...)
	s.limits = limits
	if s.err != nil {
		return cluster.InFlightIngestResult{}, s.err
	}
	return cluster.InFlightIngestResult{Accepted: true, Revision: 1, State: "complete"}, nil
}

func TestHandleInFlightSnapshotUsesValidatedConnectionIdentity(t *testing.T) {
	store := &recordingInFlightStore{}
	connectedAt := time.Unix(100, 0).UTC()
	raw := mustInFlightSnapshotJSON(t, cluster.InFlightSnapshotFrame{
		Kind:            cluster.InFlightFramePart,
		Revision:        1,
		ObservedAt:      connectedAt,
		BarrierRevision: 1,
		PartIndex:       intPointer(0),
		PartCount:       intPointer(1),
		Aggregates:      []cluster.InFlightAggregate{},
		Details:         []cluster.InFlightRequestDetail{},
	})
	limits := cluster.DefaultInFlightLimits()
	reply := handleInFlightSnapshot(context.Background(), dispatch.Env{
		NodeID:                       "cpa-a",
		ClientCertificateFingerprint: "untrusted-payload-or-legacy-field",
		InFlightStore:                store,
		InFlightLimits:               limits,
		ConnectionLifetime: cluster.ConnectionLifetime{
			Fingerprint: "trusted-fingerprint",
			ConnectedAt: connectedAt,
			Controlled:  true,
		},
		Conn: &dispatch.ConnEnv{},
	}, []string{"LPUSH", "in-flight-snapshot", string(raw)})
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("reply = %#v", reply)
	}
	if store.identity.CertificateFingerprint != "trusted-fingerprint" || !store.identity.MembershipConnectedAt.Equal(connectedAt) {
		t.Fatalf("identity = %#v", store.identity)
	}
	if store.identity.NodeID != "cpa-a" {
		t.Fatalf("node ID = %q, want cpa-a", store.identity.NodeID)
	}
	if string(store.raw) != string(raw) || store.limits != limits {
		t.Fatalf("store input = raw %q limits %#v", store.raw, store.limits)
	}
}

func TestHandleInFlightSnapshotRejectsUncontrolledOrMissingLifetime(t *testing.T) {
	connectedAt := time.Unix(100, 0).UTC()
	tests := []struct {
		name string
		env  dispatch.Env
	}{
		{
			name: "bootstrap",
			env: dispatch.Env{InFlightStore: &recordingInFlightStore{}, ConnectionLifetime: cluster.ConnectionLifetime{
				Fingerprint: "trusted-fingerprint",
				ConnectedAt: connectedAt,
			}, Conn: &dispatch.ConnEnv{}},
		},
		{
			name: "missing fingerprint",
			env: dispatch.Env{InFlightStore: &recordingInFlightStore{}, ConnectionLifetime: cluster.ConnectionLifetime{
				ConnectedAt: connectedAt,
				Controlled:  true,
			}, Conn: &dispatch.ConnEnv{}},
		},
		{
			name: "missing connected at",
			env: dispatch.Env{InFlightStore: &recordingInFlightStore{}, ConnectionLifetime: cluster.ConnectionLifetime{
				Fingerprint: "trusted-fingerprint",
				Controlled:  true,
			}, Conn: &dispatch.ConnEnv{}},
		},
		{
			name: "missing connection environment",
			env: dispatch.Env{InFlightStore: &recordingInFlightStore{}, ConnectionLifetime: cluster.ConnectionLifetime{
				Fingerprint: "trusted-fingerprint",
				ConnectedAt: connectedAt,
				Controlled:  true,
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reply := handleInFlightSnapshot(context.Background(), test.env, []string{"LPUSH", "in-flight-snapshot", `{}`})
			if reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("reply = %#v, want redis error", reply)
			}
		})
	}
}

func TestHandleInFlightSnapshotPassesCanceledContextAndRejectsStaleLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &recordingInFlightStore{err: errors.Join(cluster.ErrInFlightLifetimeMismatch, errors.New("stale"))}
	reply := handleInFlightSnapshot(ctx, dispatch.Env{
		InFlightStore: store,
		ConnectionLifetime: cluster.ConnectionLifetime{
			Fingerprint: "trusted-fingerprint",
			ConnectedAt: time.Unix(100, 0).UTC(),
			Controlled:  true,
		},
		Conn: &dispatch.ConnEnv{},
	}, []string{"LPUSH", "in-flight-snapshot", `{}`})
	if reply.Kind != dispatch.ReplyKindRedisError || reply.RedisError != "ERR in-flight snapshot rejected" {
		t.Fatalf("reply = %#v", reply)
	}
	if store.ctx != ctx || !errors.Is(store.ctx.Err(), context.Canceled) {
		t.Fatalf("store context = %v, want original canceled context", store.ctx)
	}
}

func TestHandleInFlightSnapshotRejectsInvalidCommand(t *testing.T) {
	env := dispatch.Env{InFlightStore: &recordingInFlightStore{}, Conn: &dispatch.ConnEnv{}, ConnectionLifetime: cluster.ConnectionLifetime{
		Fingerprint: "trusted-fingerprint",
		ConnectedAt: time.Unix(100, 0).UTC(),
		Controlled:  true,
	}}
	for _, args := range [][]string{
		{"LPUSH", "in-flight-snapshot"},
		{"LPUSH", "other", `{}`},
		{"RPUSH", "in-flight-snapshot", `{}`},
	} {
		reply := handleInFlightSnapshot(context.Background(), env, args)
		if reply.Kind != dispatch.ReplyKindRedisError {
			t.Fatalf("args %q reply = %#v, want redis error", args, reply)
		}
	}
}

func TestRegisterRequiresControlledLifetimeBeforeInFlightHandler(t *testing.T) {
	store := &recordingInFlightStore{}
	registry := dispatch.NewRegistry()
	Register(registry)
	reply := registry.Execute(context.Background(), dispatch.Env{
		InFlightStore: store,
		Conn:          &dispatch.ConnEnv{},
		ConnectionLifetime: cluster.ConnectionLifetime{
			Fingerprint: "trusted-fingerprint",
			ConnectedAt: time.Unix(100, 0).UTC(),
		},
	}, []string{"LPUSH", "in-flight-snapshot", `{}`})
	if reply.Kind != dispatch.ReplyKindRedisError || reply.RedisError != "ERR controlled connection required" {
		t.Fatalf("reply = %#v", reply)
	}
	if store.ctx != nil {
		t.Fatal("in-flight store was called for bootstrap connection")
	}
}

func TestRegisterKeepsExistingLPushCompatibility(t *testing.T) {
	registry := dispatch.NewRegistry()
	Register(registry)
	controlled := dispatch.Env{ConnectionLifetime: cluster.ConnectionLifetime{Controlled: true}}
	reply := registry.Execute(context.Background(), controlled, []string{"LPUSH", "in-flight-snapshot", `{}`})
	if reply.Kind != dispatch.ReplyKindRedisError || reply.RedisError != "ERR in-flight snapshot connection is not active" {
		t.Fatalf("in-flight reply = %#v", reply)
	}
	reply = registry.Execute(context.Background(), controlled, []string{"LPUSH", "unknown", `{}`})
	if reply.Kind != dispatch.ReplyKindRedisError || reply.RedisError != "ERR unsupported key" {
		t.Fatalf("legacy LPUSH reply = %#v", reply)
	}
}

func intPointer(value int) *int {
	return &value
}

func mustInFlightSnapshotJSON(t *testing.T, frame cluster.InFlightSnapshotFrame) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(frame)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}
