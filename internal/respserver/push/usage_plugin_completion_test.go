package push

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestUsageIngestionUnconfirmedCallbackSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.db")
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"restart-pending","response_headers":{"X-Request-Id":["restart-header"]}}`
	for generation := 0; generation < 2; generation++ {
		db, errOpen := cluster.OpenSQLite(t.Context(), path)
		if errOpen != nil {
			t.Fatal(errOpen)
		}
		sqlDB, errDB := db.DB()
		if errDB != nil {
			t.Fatal(errDB)
		}
		t.Cleanup(func() {
			if errClose := sqlDB.Close(); errClose != nil {
				t.Error(errClose)
			}
		})
		if generation == 0 {
			if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
				t.Fatal(errMigrate)
			}
		}
		rt, errRuntime := home.NewRuntime(&config.Config{})
		if errRuntime != nil {
			t.Fatal(errRuntime)
		}
		t.Cleanup(rt.Stop)
		rt.SetClusterAdapter(cluster.NewRuntimeAdapter(cluster.NewRepository(db), "192.0.2.10"))
		sink := &capturingUsageSink{}
		if generation == 0 {
			// The SDK external boundary must reject volatile handoff while its
			// consumer is replacing/replaying, without acknowledging ingestion.
			sink.err = pluginhost.ErrUsageDeliveryPending
		}
		runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink}
		reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC())
		if generation == 0 {
			if reply.Kind != dispatch.ReplyKindRedisError || reply.RedisError != dispatch.Err("usage plugin delivery pending").RedisError || len(sink.records) != 0 {
				t.Fatalf("unconfirmed callback reply=%+v records=%+v", reply, sink.records)
			}
		} else {
			if reply.Kind != dispatch.ReplyKindInteger || len(sink.records) != 1 || sink.records[0].RequestID != "restart-pending" || sink.records[0].ResponseHeaders.Get("X-Request-Id") != "restart-header" {
				t.Fatalf("retry after restart reply=%+v records=%+v", reply, sink.records)
			}
			if retry := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); retry.Kind != dispatch.ReplyKindInteger || len(sink.records) != 1 {
				t.Fatalf("completed event was replayed: reply=%+v records=%+v", retry, sink.records)
			}
		}
		var records []cluster.UsageRecord
		if errFind := db.Find(&records).Error; errFind != nil || len(records) != 1 || records[0].PluginUsageDelivered != (generation == 1) {
			t.Fatalf("generation %d completion state=%+v error=%v", generation, records, errFind)
		}
		// Reopen the database and construct a fresh runtime/consumer. No volatile
		// state from the first owner is available to the retry.
		rt.Stop()
		if errClose := sqlDB.Close(); errClose != nil {
			t.Fatal(errClose)
		}
	}
}
