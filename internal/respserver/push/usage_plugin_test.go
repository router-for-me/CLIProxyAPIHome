package push

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type capturingUsageSink struct {
	records []pluginapi.UsageRecord
	onUsage func()
	err     error
}

func (s *capturingUsageSink) HandleUsage(_ context.Context, record pluginapi.UsageRecord) error {
	if s.err != nil {
		return s.err
	}
	s.records = append(s.records, record)
	if s.onUsage != nil {
		s.onUsage()
	}
	return nil
}

// Inject delivery and storage outcomes to test retries against the real runtime.
type pluginUsageIngestionRuntime struct {
	*home.Runtime
	sink         home.PluginUsageSink
	feedbackErr  error
	persistErr   error
	commitErr    error
	persistCalls atomic.Int32
}

func (r *pluginUsageIngestionRuntime) UsagePluginSink() home.PluginUsageSink {
	return r.sink
}

func (r *pluginUsageIngestionRuntime) PersistClusterUsagePayload(ctx context.Context, payload string, receivedAt time.Time) (bool, bool, error) {
	r.persistCalls.Add(1)
	if r.persistErr != nil {
		return false, false, r.persistErr
	}
	persisted, inserted, errPersist := r.Runtime.PersistClusterUsagePayload(ctx, payload, receivedAt)
	if errPersist == nil && r.commitErr != nil {
		return false, false, r.commitErr
	}
	return persisted, inserted, errPersist
}

func (r *pluginUsageIngestionRuntime) RecordUsagePayload(ctx context.Context, payload string) error {
	if r.feedbackErr != nil {
		return r.feedbackErr
	}
	return r.Runtime.RecordUsagePayload(ctx, payload)
}

func TestUsageIngestionPreservesPluginHeadersWithoutPersistingSecrets(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	sink := &capturingUsageSink{}
	runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink, feedbackErr: errors.New("scheduler feedback unavailable")}
	env := dispatch.Env{Runtime: rt, NodeID: "plugin-cpa", ClientIP: "192.0.2.15"}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","model":"gpt-5","auth_type":"api_key","auth_index":"usage-ingestion-auth","source":"provider-source-secret","execution_id":"header-execution","selection_id":"header-selection","response_headers":{"X-Codex-Active-Limit":["premium"],"X-Codex-Primary-Used-Percent":["82"],"X-Request-Id":["upstream-request"],"Set-Cookie":["provider-cookie-secret"],"Authorization":["provider-header-secret"]}}`
	if reply := ingestUsagePayload(t.Context(), env, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("feedback failure reply = %+v", reply)
	}
	if len(sink.records) != 0 {
		t.Fatalf("delivered plugin feedback before scheduler feedback succeeded: %d", len(sink.records))
	}
	var pending cluster.UsageRecord
	if errFirst := db.First(&pending).Error; errFirst != nil || pending.PluginUsageDelivered {
		t.Fatalf("feedback failure completed plugin delivery: record=%+v error=%v", pending, errFirst)
	}
	// Retry the committed row after scheduler feedback recovers. The plugin
	// must still receive raw response headers without persisting those secrets.
	runtime.feedbackErr = nil
	if reply := ingestUsagePayload(t.Context(), env, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("feedback retry reply = %+v", reply)
	}
	if len(sink.records) != 1 {
		t.Fatalf("deliveries = %d, want one committed usage event", len(sink.records))
	}
	record := sink.records[0]
	if record.RequestID != "header-execution" || record.SelectionID != "header-selection" || record.Source != "usage-ingestion-auth" {
		t.Fatalf("unexpected plugin identity/source: %+v", record)
	}
	for header, want := range map[string]string{
		"X-Codex-Primary-Used-Percent": "82",
		"X-Request-Id":                 "upstream-request",
		"Set-Cookie":                   "provider-cookie-secret",
		"Authorization":                "provider-header-secret",
	} {
		if got := record.ResponseHeaders.Get(header); got != want {
			t.Fatalf("plugin header %s = %q, want %q", header, got, want)
		}
	}

	// A fully processed event must not deliver again on redelivery.
	if reply := ingestUsagePayload(t.Context(), env, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("redelivery reply = %+v", reply)
	}
	if len(sink.records) != 1 {
		t.Fatalf("redelivery duplicated plugin feedback: %d", len(sink.records))
	}
	var records []cluster.UsageRecord
	if errFind := db.Find(&records).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(records) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(records))
	}
	stored := string(records[0].PayloadJSON)
	for _, secret := range []string{"provider-source-secret", "provider-cookie-secret", "provider-header-secret"} {
		if strings.Contains(stored, secret) {
			t.Fatalf("private response metadata persisted: %s", secret)
		}
	}
	if gjson.Get(stored, "response_headers").Exists() || gjson.Get(stored, "quota_headers.Authorization").Exists() {
		t.Fatal("raw response headers were persisted")
	}
	if got := gjson.Get(stored, "quota_headers.X-Codex-Primary-Used-Percent").String(); got != "82" {
		t.Fatalf("sanitized quota header = %q, want 82", got)
	}
	if got := gjson.Get(stored, "upstream_request_id").String(); got != "upstream-request" {
		t.Fatalf("safe upstream request identity = %q", got)
	}
	if got := gjson.Get(stored, "cpa_node_id").String(); got != "plugin-cpa" {
		t.Fatalf("persisted CPA identity = %q", got)
	}
}

func TestUsageIngestionPluginFailureStillAppliesFeedback(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "pending", err: fmt.Errorf("consumer replacing: %w", pluginhost.ErrUsageDeliveryPending)},
		{name: "unavailable", err: pluginhost.ErrUsageDeliveryUnavailable},
		{name: "callback_failure", err: errors.New("callback failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, rt := newUsageIngestionTestRuntime(t, true)
			sink := &capturingUsageSink{err: test.err}
			runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink}
			payload := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","model":"gpt-5","auth_index":"usage-ingestion-auth","execution_id":"plugin-failure-429","failed":true,"fail":{"status_code":429}}`, time.Now().UTC().Format(time.RFC3339Nano))
			var deadline time.Time
			for attempt := range 4 {
				if attempt == 2 {
					// Successful delivery completes the existing row; another replay
					// must not redeliver or count the 429 a second time.
					sink.err = nil
				}
				reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC())
				if attempt < 2 {
					if reply.Kind != dispatch.ReplyKindRedisError || len(sink.records) != 0 {
						t.Fatalf("attempt %d acknowledged failed delivery: reply=%+v records=%+v", attempt, reply, sink.records)
					}
				} else if reply.Kind != dispatch.ReplyKindInteger || len(sink.records) != 1 {
					t.Fatalf("attempt %d retry reply=%+v records=%+v", attempt, reply, sink.records)
				}
				auth, ok := rt.CoreManager().GetByID("usage-ingestion-auth")
				if !ok {
					t.Fatal("credential missing from manager")
				}
				state := auth.ModelStates["gpt-5"]
				if state == nil || !state.Quota.Exceeded || state.Quota.BackoffLevel != 1 || auth.Failed != 1 {
					t.Fatalf("attempt %d missing or duplicated cooldown feedback: failed=%d state=%+v", attempt, auth.Failed, state)
				}
				if attempt == 0 {
					deadline = state.Quota.NextRecoverAt
				} else if !state.Quota.NextRecoverAt.Equal(deadline) {
					t.Fatalf("attempt %d changed cooldown deadline: got %v want %v", attempt, state.Quota.NextRecoverAt, deadline)
				}
				var rows []cluster.UsageRecord
				if errFind := db.Find(&rows).Error; errFind != nil || len(rows) != 1 || rows[0].PluginUsageDelivered != (attempt >= 2) {
					t.Fatalf("attempt %d unexpected persisted delivery state: rows=%+v error=%v", attempt, rows, errFind)
				}
			}
		})
	}
}

func TestUsageIngestionWithoutUsageSinkStillPersists(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	runtime := &pluginUsageIngestionRuntime{Runtime: rt}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","model":"gpt-5","auth_index":"usage-ingestion-auth","execution_id":"non-usage-plugin","response_headers":{"Authorization":["must-not-persist"]}}`
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{Runtime: rt}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("reply without usage consumer = %+v", reply)
	}
	var record cluster.UsageRecord
	if errFirst := db.First(&record).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if runtime.persistCalls.Load() != 1 || !record.PluginUsageDelivered || gjson.Get(string(record.PayloadJSON), "response_headers").Exists() || strings.Contains(string(record.PayloadJSON), "must-not-persist") {
		t.Fatalf("unexpected persisted payload or completion: %+v", record)
	}
	// Loading a consumer later must not replay an already acknowledged event.
	sink := &capturingUsageSink{}
	runtime.sink = sink
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{Runtime: rt}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger || len(sink.records) != 0 {
		t.Fatalf("late consumer reply=%+v deliveries=%d", reply, len(sink.records))
	}
}

func TestUsageIngestionDoesNotDeliverPluginFeedbackOnDatabaseFailure(t *testing.T) {
	_, rt := newUsageIngestionTestRuntime(t, true)
	sink := &capturingUsageSink{}
	runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink, persistErr: errors.New("database unavailable")}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"failed-insert","response_headers":{"X-Request-Id":["retry-header"]}}`
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindRedisError || len(sink.records) != 0 {
		t.Fatalf("failed insert reply=%+v deliveries=%d", reply, len(sink.records))
	}
	runtime.persistErr = nil
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger || len(sink.records) != 1 {
		t.Fatalf("retry reply=%+v deliveries=%d", reply, len(sink.records))
	}
	if got := sink.records[0].ResponseHeaders.Get("X-Request-Id"); got != "retry-header" {
		t.Fatalf("retried plugin header=%q", got)
	}
}

func TestUsageIngestionPreservesPluginBackpressureAfterCommit(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	defer func() {
		close(release)
		<-done
	}()
	completed := make(chan dispatch.Reply, 1)
	sink := &capturingUsageSink{onUsage: func() {
		close(entered)
		<-release
	}}
	runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","model":"gpt-5","auth_index":"usage-ingestion-auth","execution_id":"backpressure","failed":true,"fail":{"status_code":429},"response_headers":{"X-Request-Id":["blocked-header"]}}`
	go func() {
		defer close(done)
		completed <- ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC())
	}()
	select {
	case <-entered:
	case reply := <-completed:
		t.Fatalf("ingestion returned before plugin delivery: %+v", reply)
	case <-t.Context().Done():
		t.Fatal("ingestion canceled before plugin delivery")
	}
	var count int64
	if errCount := db.Model(&cluster.UsageRecord{}).Count(&count).Error; errCount != nil || count != 1 {
		t.Fatalf("plugin delivery preceded commit: count=%d error=%v", count, errCount)
	}
	auth, ok := rt.CoreManager().GetByID("usage-ingestion-auth")
	if !ok || auth.ModelStates["gpt-5"] == nil || !auth.ModelStates["gpt-5"].Quota.Exceeded || auth.Failed != 1 {
		t.Fatalf("plugin backpressure blocked cooldown feedback: ok=%v auth=%+v", ok, auth)
	}
	select {
	case reply := <-completed:
		t.Fatalf("ingestion acknowledged during plugin backpressure: %+v", reply)
	default:
	}
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("concurrent retry acknowledged a pending callback: %+v", reply)
	}
	var pending cluster.UsageRecord
	if errFirst := db.First(&pending).Error; errFirst != nil || pending.PluginUsageDelivered {
		t.Fatalf("delivery marked complete during backpressure: record=%+v error=%v", pending, errFirst)
	}
	// A send releases this callback without closing the channel before deferred cleanup.
	release <- struct{}{}
	select {
	case reply := <-completed:
		if reply.Kind != dispatch.ReplyKindInteger {
			t.Fatalf("completed ingestion reply=%+v", reply)
		}
	case <-t.Context().Done():
		t.Fatal("ingestion canceled after plugin delivery")
	}
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger || len(sink.records) != 1 {
		t.Fatalf("completed retry reply=%+v deliveries=%d", reply, len(sink.records))
	}
}

func TestUsageIngestionRecoversPluginDeliveryAfterAmbiguousCommit(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	sink := &capturingUsageSink{}
	runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink, commitErr: errors.New("commit result lost")}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","model":"gpt-5","auth_index":"usage-ingestion-auth","execution_id":"ambiguous-commit","selection_id":"retained-selection","response_headers":{"X-Request-Id":["retry-header"]}}`
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindRedisError || len(sink.records) != 0 {
		t.Fatalf("uncertain commit reply=%+v deliveries=%d", reply, len(sink.records))
	}
	var record cluster.UsageRecord
	if errFirst := db.First(&record).Error; errFirst != nil || record.PluginUsageDelivered {
		t.Fatalf("committed but undelivered event=%+v error=%v", record, errFirst)
	}
	runtime.commitErr = nil
	for attempt := 0; attempt < 2; attempt++ {
		if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger {
			t.Fatalf("retry %d reply=%+v", attempt, reply)
		}
	}
	if len(sink.records) != 1 || sink.records[0].RequestID != "ambiguous-commit" || sink.records[0].ResponseHeaders.Get("X-Request-Id") != "retry-header" {
		t.Fatalf("recovered plugin records=%+v", sink.records)
	}
	var records []cluster.UsageRecord
	if errFind := db.Find(&records).Error; errFind != nil || len(records) != 1 || !records[0].PluginUsageDelivered {
		t.Fatalf("recovered usage records=%+v error=%v", records, errFind)
	}
}

func TestUsageIngestionDoesNotAcknowledgeFailedCompletionMarker(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	sink := &capturingUsageSink{}
	runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink}
	failMarker := true
	markerErr := errors.New("completion write unavailable")
	if errRegister := db.Callback().Update().Before("gorm:update").Register("test:usage_delivery_marker", func(tx *gorm.DB) {
		if failMarker && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "usage" {
			tx.AddError(markerErr)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"failed-marker"}`
	if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindRedisError || len(sink.records) != 1 {
		t.Fatalf("failed completion reply=%+v deliveries=%d", reply, len(sink.records))
	}
	failMarker = false
	// If completion cannot be recorded, replay is at-least-once rather than
	// silently acknowledging a state that cannot be recovered after restart.
	for attempt := 0; attempt < 2; attempt++ {
		if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger {
			t.Fatalf("completion retry %d reply=%+v", attempt, reply)
		}
	}
	if len(sink.records) != 2 || sink.records[0].RequestID != sink.records[1].RequestID {
		t.Fatalf("completion retry records=%+v", sink.records)
	}
}

func TestUsageIngestionRejectsMalformedPluginFeedbackBeforePersistence(t *testing.T) {
	for _, fields := range []string{
		`"response_headers":"invalid"`,
		`"response_headers":{"X-Test":123}`,
		`"response_headers":{"X-Test":["valid",123]}`,
		`"response_headers":{"X-Test":["valid"]},"latency_ms":"invalid"`,
	} {
		t.Run(fields, func(t *testing.T) {
			sink := &capturingUsageSink{}
			runtime := &pluginUsageIngestionRuntime{sink: sink}
			payload := fmt.Sprintf(`{"provider":"codex","execution_id":"invalid",%s}`, fields)
			if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("invalid feedback reply = %+v", reply)
			}
			if runtime.persistCalls.Load() != 0 || len(sink.records) != 0 {
				t.Fatalf("invalid feedback persisted or delivered: writes=%d deliveries=%d", runtime.persistCalls.Load(), len(sink.records))
			}
		})
	}
}
