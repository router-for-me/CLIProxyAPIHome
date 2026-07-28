package kv

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func newKVHandlerTestRuntime(t *testing.T) *home.Runtime {
	t.Helper()

	ctx := context.Background()
	db, errOpenSQLite := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(cluster.NewRepository(db), ""))
	return rt
}

func executeKVCommand(t *testing.T, rt *home.Runtime, args ...string) dispatch.Reply {
	t.Helper()

	reg := dispatch.NewRegistry()
	Register(reg)
	return reg.Execute(context.Background(), dispatch.Env{
		Runtime:            rt,
		ConnectionLifetime: cluster.ConnectionLifetime{Controlled: true},
	}, args)
}

func requireSimpleOK(t *testing.T, reply dispatch.Reply) {
	t.Helper()
	if reply.Kind != dispatch.ReplyKindSimpleString || reply.SimpleString != "OK" {
		t.Fatalf("reply = %#v, want simple OK", reply)
	}
}

func requireInteger(t *testing.T, reply dispatch.Reply, want int64) {
	t.Helper()
	if reply.Kind != dispatch.ReplyKindInteger || reply.Integer != want {
		t.Fatalf("reply = %#v, want integer %d", reply, want)
	}
}

func requireNullBulk(t *testing.T, reply dispatch.Reply) {
	t.Helper()
	if reply.Kind != dispatch.ReplyKindBulkString || reply.BulkString != nil {
		t.Fatalf("reply = %#v, want null bulk", reply)
	}
}

func requireRedisError(t *testing.T, reply dispatch.Reply) {
	t.Helper()
	if reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("reply = %#v, want redis error", reply)
	}
}

func TestKVCommands(t *testing.T) {
	rt := newKVHandlerTestRuntime(t)

	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "key", "value"))
	value, found, errGet := rt.KVGet(context.Background(), "key")
	if errGet != nil {
		t.Fatalf("KVGet(key) error = %v", errGet)
	}
	if !found || string(value) != "value" {
		t.Fatalf("KVGet(key) = %q, %v, want value, true", value, found)
	}

	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "ex-key", "value", "EX", "10"))
	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "px-key", "value", "PX", "1500"))
	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "nx-key", "first", "NX", "EX", "10"))
	requireNullBulk(t, executeKVCommand(t, rt, "SET", "nx-key", "second", "EX", "10", "NX"))
	requireNullBulk(t, executeKVCommand(t, rt, "SET", "xx-key", "value", "XX"))
	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "xx-key", "value"))
	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "xx-key", "next", "XX"))

	requireInteger(t, executeKVCommand(t, rt, "SETNX", "setnx-key", "value"), 1)
	requireInteger(t, executeKVCommand(t, rt, "SETNX", "setnx-key", "other"), 0)

	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "del-1", "value"))
	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "del-2", "value"))
	requireInteger(t, executeKVCommand(t, rt, "DEL", "del-1", "missing", "del-2"), 2)

	requireSimpleOK(t, executeKVCommand(t, rt, "SET", "ttl-key", "value"))
	requireInteger(t, executeKVCommand(t, rt, "TTL", "ttl-key"), -1)
	requireInteger(t, executeKVCommand(t, rt, "EXPIRE", "ttl-key", "10"), 1)
	requireInteger(t, executeKVCommand(t, rt, "EXPIRE", "missing", "10"), 0)
	ttlReply := executeKVCommand(t, rt, "TTL", "ttl-key")
	if ttlReply.Kind != dispatch.ReplyKindInteger || ttlReply.Integer < 0 {
		t.Fatalf("TTL(ttl-key) = %#v, want non-negative integer", ttlReply)
	}

	requireInteger(t, executeKVCommand(t, rt, "INCRBY", "counter", "3"), 3)
	requireSimpleOK(t, executeKVCommand(t, rt, "MSET", "m1", "one", "m2", "two"))
	mgetReply := executeKVCommand(t, rt, "MGET", "m1", "missing", "m2")
	if mgetReply.Kind != dispatch.ReplyKindArray || len(mgetReply.Array) != 3 {
		t.Fatalf("MGET reply = %#v, want array with 3 entries", mgetReply)
	}
	if string(mgetReply.Array[0].BulkString) != "one" || mgetReply.Array[1].BulkString != nil || string(mgetReply.Array[2].BulkString) != "two" {
		t.Fatalf("MGET entries = %#v, want one, nil, two", mgetReply.Array)
	}
}

func TestKVCommandErrors(t *testing.T) {
	rt := newKVHandlerTestRuntime(t)

	for _, args := range [][]string{
		{"SET", "key", "value", "EX", "10", "PX", "10"},
		{"SET", "key", "value", "EX", "10", "EX", "20"},
		{"SET", "key", "value", "NX", "XX"},
		{"SET", "key", "value", "EX", "bad"},
		{"MSET", "key", "value", "dangling"},
		{"INCRBY", "key", "bad"},
		{"CAS", "key", "0", "", "value", "PX"},
		{"CAS", "key", "0", ""},
		{"CAS", "key", "2", "", "value"},
		{"CAS", "key", "0", "", "value", "EX", "10"},
		{"CAS", "key", "0", "", "value", "PX", "0"},
		{"CAS", "key", "0", "", "value", "PX", "bad"},
	} {
		requireRedisError(t, executeKVCommand(t, rt, args...))
	}
}

func TestKVCompareAndSwapCommand(t *testing.T) {
	rt := newKVHandlerTestRuntime(t)

	// Reserving an absent key succeeds once, mirroring the CPA replay-cache fence.
	requireInteger(t, executeKVCommand(t, rt, "CAS", "replay", "0", "", "reservation", "PX", "1500"), 1)
	requireInteger(t, executeKVCommand(t, rt, "CAS", "replay", "0", "", "other", "PX", "1500"), 0)

	// A matching expected value swaps; a stale one is a no-op.
	requireInteger(t, executeKVCommand(t, rt, "CAS", "replay", "1", "stale", "chain", "PX", "1500"), 0)
	requireInteger(t, executeKVCommand(t, rt, "CAS", "replay", "1", "reservation", "chain", "PX", "1500"), 1)
	value, found, errGet := rt.KVGet(context.Background(), "replay")
	if errGet != nil || !found || string(value) != "chain" {
		t.Fatalf("KVGet(replay) = %q, %v, %v, want chain, true, nil", value, found, errGet)
	}
	ttlReply := executeKVCommand(t, rt, "TTL", "replay")
	if ttlReply.Kind != dispatch.ReplyKindInteger || ttlReply.Integer < 0 {
		t.Fatalf("TTL(replay) = %#v, want non-negative integer", ttlReply)
	}

	// Omitting PX stores the value without a TTL.
	requireInteger(t, executeKVCommand(t, rt, "CAS", "replay", "1", "chain", "tombstone"), 1)
	requireInteger(t, executeKVCommand(t, rt, "TTL", "replay"), -1)

	// Binary values round-trip through the comparison.
	binary := string([]byte{0x00, 0xff, 0x10})
	requireInteger(t, executeKVCommand(t, rt, "CAS", "binary", "0", "", binary), 1)
	requireInteger(t, executeKVCommand(t, rt, "CAS", "binary", "1", binary, "next"), 1)
}

func TestKVCompareAndSwapRequiresControlledLifetime(t *testing.T) {
	rt := newKVHandlerTestRuntime(t)

	reg := dispatch.NewRegistry()
	Register(reg)
	reply := reg.Execute(context.Background(), dispatch.Env{
		Runtime:            rt,
		ConnectionLifetime: cluster.ConnectionLifetime{Controlled: false},
	}, []string{"CAS", "replay", "0", "", "reservation"})
	requireRedisError(t, reply)
	if _, found, errGet := rt.KVGet(context.Background(), "replay"); errGet != nil || found {
		t.Fatalf("KVGet(replay) = %v, %v, want not found, nil", found, errGet)
	}
}
