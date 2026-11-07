package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"gorm.io/gorm"
)

func newUsageFeedbackTestRuntime(t *testing.T) (*gorm.DB, *cluster.Repository, *home.Runtime) {
	t.Helper()
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
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
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.10"))
	t.Cleanup(rt.Stop)

	const authID = "usage-feedback-auth"
	auth := &coreauth.Auth{ID: authID, Index: authID, Provider: "codex", Status: coreauth.StatusActive}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if _, errRegister := rt.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	return db, repo, rt
}

func TestReviewStoredFailureReplayPreservesNewerSuccess(t *testing.T) {
	for _, withExecutionID := range []bool{false, true} {
		t.Run(fmt.Sprintf("execution_id_%t", withExecutionID), func(t *testing.T) {
			db, repo, rt := newUsageFeedbackTestRuntime(t)
			ctx := context.Background()
			env := dispatch.Env{Runtime: rt}
			executionField := ""
			if withExecutionID {
				executionField = `,"execution_id":"stored-failure"`
			}
			failure := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}` + executionField + `}`
			success := `{"timestamp":"2026-11-06T10:27:16Z","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5"}`
			for _, payload := range []string{failure, success} {
				if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
					t.Fatalf("initial delivery reply = %+v", reply)
				}
			}
			before, beforeRecord, errBefore := repo.GetAuth(ctx, "usage-feedback-auth")
			if errBefore != nil {
				t.Fatal(errBefore)
			}
			if before.ModelStates["gpt-5"].Unavailable {
				t.Fatal("newer success did not clear cooldown")
			}
			localBefore, _ := rt.CoreManager().GetByID("usage-feedback-auth")
			failLookup := true
			injected := errors.New("usage deduplication lookup unavailable")
			if errCallback := db.Callback().Query().Before("gorm:query").Register("test:usage-lookup-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "usage" && failLookup {
					tx.AddError(injected)
				}
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(ctx, failure, time.Now()); !errors.Is(errPersist, injected) || errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
				t.Fatalf("lookup failure classification = %v", errPersist)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", failure}); reply.Kind != dispatch.ReplyKindRedisError {
					t.Fatalf("failed deduplication reply = %+v", reply)
				}
				after, afterRecord, errAfter := repo.GetAuth(ctx, "usage-feedback-auth")
				if errAfter != nil {
					t.Fatal(errAfter)
				}
				localAfter, _ := rt.CoreManager().GetByID("usage-feedback-auth")
				if afterRecord.Version != beforeRecord.Version || !reflect.DeepEqual(before.ModelStates, after.ModelStates) || !reflect.DeepEqual(localBefore.ModelStates, localAfter.ModelStates) {
					t.Fatal("stored failure replay restored cooldown after a newer success")
				}
				if localAfter.Success != localBefore.Success || localAfter.Failed != localBefore.Failed {
					t.Fatal("failed deduplication changed request counters")
				}
			}
			failLookup = false
			if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", failure}); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("recovered duplicate reply = %+v", reply)
			}
		})
	}
}

func TestReviewUncommittedFeedbackReplay(t *testing.T) {
	for _, oldStatus := range []int{http.StatusOK, http.StatusTooManyRequests} {
		for _, otherHome := range []bool{false, true} {
			t.Run(fmt.Sprintf("old_%d/other_home_%t", oldStatus, otherHome), func(t *testing.T) {
				db, repo, rt := newUsageFeedbackTestRuntime(t)
				ctx := context.Background()
				newStatus := http.StatusOK
				if oldStatus == http.StatusOK {
					newStatus = http.StatusTooManyRequests
				}
				payload := func(second, status int) string {
					return fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:%02dZ","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":%t,"fail":{"status_code":%d}}`, second, status != http.StatusOK, status)
				}
				oldPayload := payload(15, oldStatus)
				failWrites := true
				if errCallback := db.Callback().Create().Before("gorm:create").Register("test:uncommitted-feedback", func(tx *gorm.DB) {
					if tx.Statement.Table == "usage" && failWrites {
						tx.AddError(errors.New("usage write unavailable"))
					}
				}); errCallback != nil {
					t.Fatal(errCallback)
				}
				if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", oldPayload}); reply.Kind != dispatch.ReplyKindRedisError {
					t.Fatalf("old failed write reply = %+v", reply)
				}
				first, _, errFirst := repo.GetAuth(ctx, "usage-feedback-auth")
				if errFirst != nil || first.ModelStates["gpt-5"] == nil || first.ModelStates["gpt-5"].Unavailable != (oldStatus != http.StatusOK) {
					t.Fatalf("old feedback did not apply: %v", errFirst)
				}
				failWrites = false
				if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload(16, newStatus)}); reply.Kind != dispatch.ReplyKindInteger {
					t.Fatalf("newer event reply = %+v", reply)
				}
				before, beforeRecord, errBefore := repo.GetAuth(ctx, "usage-feedback-auth")
				if errBefore != nil || before.ModelStates["gpt-5"].Unavailable != (newStatus != http.StatusOK) {
					t.Fatalf("newer feedback did not apply: %v", errBefore)
				}
				retryRuntime := rt
				if otherHome {
					var errRuntime error
					retryRuntime, errRuntime = home.NewRuntime(&config.Config{})
					if errRuntime != nil {
						t.Fatal(errRuntime)
					}
					t.Cleanup(retryRuntime.Stop)
					retryRuntime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.11"))
					if _, errRegister := retryRuntime.CoreManager().Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{ID: "usage-feedback-auth", Index: "usage-feedback-auth", Provider: "codex"}); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				localBefore, _ := retryRuntime.CoreManager().GetByID("usage-feedback-auth")
				failWrites = true
				for attempt := 0; attempt < 2; attempt++ {
					if reply := handleUsage(ctx, dispatch.Env{Runtime: retryRuntime}, []string{"LPUSH", "usage", oldPayload}); reply.Kind != dispatch.ReplyKindRedisError {
						t.Fatalf("replayed failed write reply = %+v", reply)
					}
					after, afterRecord, errAfter := repo.GetAuth(ctx, "usage-feedback-auth")
					if errAfter != nil || afterRecord.Version != beforeRecord.Version || !reflect.DeepEqual(before.ModelStates, after.ModelStates) {
						t.Fatalf("uncommitted feedback replay replaced newer state: %v", errAfter)
					}
					localAfter, _ := retryRuntime.CoreManager().GetByID("usage-feedback-auth")
					if localAfter.Success != localBefore.Success || localAfter.Failed != localBefore.Failed {
						t.Fatal("failed usage replay changed request counters")
					}
				}
				// A recovered first insert counts the request, not its old transition.
				failWrites = false
				if reply := handleUsage(ctx, dispatch.Env{Runtime: retryRuntime}, []string{"LPUSH", "usage", oldPayload}); reply.Kind != dispatch.ReplyKindInteger {
					t.Fatalf("recovered write reply = %+v", reply)
				}
				committed, _ := retryRuntime.CoreManager().GetByID("usage-feedback-auth")
				if committed.Success+committed.Failed != localBefore.Success+localBefore.Failed+1 || committed.ModelStates["gpt-5"].Unavailable != (newStatus != http.StatusOK) {
					t.Fatal("recovered write lost its count or replayed old feedback")
				}
			})
		}
	}
}

func TestReviewCommittedFeedbackRetry(t *testing.T) {
	for _, retryBeforeOlder := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry_before_older_%t", retryBeforeOlder), func(t *testing.T) {
			db, repo, rt := newUsageFeedbackTestRuntime(t)
			ctx := context.Background()
			older := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"older-quota","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","tokens":{"input_tokens":1,"total_tokens":1},"failed":true,"fail":{"status_code":429}}`
			newer := strings.ReplaceAll(strings.ReplaceAll(older, "older-quota", "newer-quota"), "10:27:15", "10:27:16")
			olderCtx := coreauth.WithUsageFeedback(ctx)
			if _, inserted, errPersist := rt.PersistClusterUsagePayloadOutcome(olderCtx, older, time.Now()); errPersist != nil || !inserted {
				t.Fatalf("persist older: %t, %v", inserted, errPersist)
			}
			failAuth := true
			injected := errors.New("auth unavailable")
			if errCallback := db.Callback().Update().Before("gorm:update").Register("test:committed-feedback-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "auth" && failAuth {
					tx.AddError(injected)
				}
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", newer}); reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("failed feedback acknowledged: %+v", reply)
			}
			failAuth = false
			retryRuntime, errRuntime := home.NewRuntime(&config.Config{})
			if errRuntime != nil {
				t.Fatal(errRuntime)
			}
			retryRuntime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.11"))
			t.Cleanup(retryRuntime.Stop)
			minimal := &coreauth.Auth{ID: "usage-feedback-auth", Index: "usage-feedback-auth", Provider: "codex", Status: coreauth.StatusActive}
			if _, errRegister := retryRuntime.CoreManager().Register(coreauth.WithSkipPersist(ctx), minimal); errRegister != nil {
				t.Fatal(errRegister)
			}
			if !retryBeforeOlder {
				if errRecord := rt.RecordUsagePayload(olderCtx, older); errRecord != nil {
					t.Fatal(errRecord)
				}
			}
			for i := 0; i < 2; i++ {
				if reply := handleUsage(ctx, dispatch.Env{Runtime: retryRuntime}, []string{"LPUSH", "usage", newer}); reply.Kind != dispatch.ReplyKindInteger {
					t.Fatalf("feedback retry: %+v", reply)
				}
			}
			if retryBeforeOlder {
				if errRecord := rt.RecordUsagePayload(olderCtx, older); errRecord != nil {
					t.Fatal(errRecord)
				}
			}
			retried, _ := retryRuntime.CoreManager().GetByID("usage-feedback-auth")
			if retried.Failed != 0 {
				t.Fatalf("other Home counted retry: %d", retried.Failed)
			}
			auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
			if errGet != nil {
				t.Fatal(errGet)
			}
			if state := auth.ModelStates["gpt-5"]; state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() {
				t.Fatalf("acknowledged retry lost pending feedback: %#v", state)
			}
			local, _ := rt.CoreManager().GetByID("usage-feedback-auth")
			if local.Failed != 2 {
				t.Fatalf("request failures = %d, want 2", local.Failed)
			}
			for _, table := range []string{"usage", "billing_charge"} {
				var count int64
				if errCount := db.Table(table).Count(&count).Error; errCount != nil || count != 2 {
					t.Fatalf("%s rows = %d, %v", table, count, errCount)
				}
			}
		})
	}
}

func TestReviewDuplicateMissingRoutingFields(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		for _, missing := range []struct{ field, value string }{{"auth_index", "usage-feedback-auth"}, {"model", "gpt-5"}} {
			t.Run(fmt.Sprintf("status_%d/missing_%s", status, missing.field), func(t *testing.T) {
				db, repo, rt := newUsageFeedbackTestRuntime(t)
				ctx := context.Background()
				payload := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"missing-routing","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","tokens":{"input_tokens":1,"total_tokens":1},"failed":true,"fail":{"status_code":%d}}`, status)
				pendingCtx := coreauth.WithUsageFeedback(ctx)
				if persisted, inserted, errPersist := rt.PersistClusterUsagePayloadOutcome(pendingCtx, payload, time.Now()); errPersist != nil || !persisted || !inserted {
					t.Fatalf("persist pending feedback: persisted=%t inserted=%t error=%v", persisted, inserted, errPersist)
				}
				feedback := coreauth.UsageFeedbackFromContext(pendingCtx)
				var pending int64
				if errCount := db.Model(&cluster.KVRecord{}).Where("key = ?", feedback.ScopeKey).Count(&pending).Error; errCount != nil || pending != 1 {
					t.Fatalf("pending feedback rows = %d, error=%v", pending, errCount)
				}

				retry := strings.Replace(payload, fmt.Sprintf(`,"%s":%q`, missing.field, missing.value), "", 1)
				if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", retry}); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
					t.Fatalf("retry reply = %+v", reply)
				}
				auth, record, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
				if errGet != nil {
					t.Fatal(errGet)
				}
				state := auth.ModelStates["gpt-5"]
				if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() || state.LastError == nil || state.LastError.HTTPStatus != status {
					t.Fatalf("acknowledged retry left feedback unapplied: %#v", state)
				}
				if errCount := db.Model(&cluster.KVRecord{}).Where("key = ?", feedback.ScopeKey).Count(&pending).Error; errCount != nil || pending != 0 {
					t.Fatalf("acknowledged retry retained pending feedback: %d, error=%v", pending, errCount)
				}

				// Repeating the sparse delivery must not reapply feedback or count it.
				if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", retry}); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
					t.Fatalf("completed duplicate reply = %+v", reply)
				}
				after, afterRecord, errAfter := repo.GetAuth(ctx, "usage-feedback-auth")
				if errAfter != nil {
					t.Fatal(errAfter)
				}
				if record.Version != afterRecord.Version || !reflect.DeepEqual(auth.ModelStates, after.ModelStates) {
					t.Fatal("completed duplicate changed credential state")
				}
				local, _ := rt.CoreManager().GetByID("usage-feedback-auth")
				if local.Success != 0 || local.Failed != 0 {
					t.Fatalf("feedback retry counted requests: %d/%d", local.Success, local.Failed)
				}
				for _, table := range []string{"usage", "billing_charge"} {
					var rows int64
					if errCount := db.Table(table).Count(&rows).Error; errCount != nil || rows != 1 {
						t.Fatalf("%s rows = %d, error=%v", table, rows, errCount)
					}
				}
			})
		}
	}
}

func TestReviewSupersededSuccessClearsExistingQuota(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse_%t", reverse), func(t *testing.T) {
			_, repo, rt := newUsageFeedbackTestRuntime(t)
			ctx := context.Background()
			initial := `{"timestamp":"2026-11-06T10:27:14Z","execution_id":"existing-quota","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`
			if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", initial}); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("initial quota: %+v", reply)
			}
			before, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
			if errGet != nil || before.ModelStates["gpt-5"].NextRetryAfter.Before(time.Now().Add(59*time.Minute)) {
				t.Fatalf("initial quota missing: %v", errGet)
			}
			payloads := []string{
				`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"quota-success","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5"}`,
				`{"timestamp":"2026-11-06T10:27:16Z","execution_id":"fresh-quota","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`,
			}
			contexts := make([]context.Context, len(payloads))
			for i, payload := range payloads {
				contexts[i] = coreauth.WithUsageFeedback(ctx)
				if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(contexts[i], payload, time.Now()); errPersist != nil {
					t.Fatal(errPersist)
				}
			}
			order := []int{0, 1}
			if reverse {
				order = []int{1, 0}
			}
			for _, i := range order {
				if errRecord := rt.RecordUsagePayload(contexts[i], payloads[i]); errRecord != nil {
					t.Fatal(errRecord)
				}
			}
			after, _, errAfter := repo.GetAuth(ctx, "usage-feedback-auth")
			if errAfter != nil {
				t.Fatal(errAfter)
			}
			state := after.ModelStates["gpt-5"]
			if state == nil || !state.Unavailable || state.NextRetryAfter.After(time.Now().Add(time.Minute)) {
				t.Fatalf("success reset was lost: %#v", state)
			}
		})
	}
}

func TestReviewUnappliedQuotaResetHint(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("tracked_%t/reverse_%t", tracked, reverse), func(t *testing.T) {
				_, repo, rt := newUsageFeedbackTestRuntime(t)
				start := time.Now()
				contexts := []context.Context{context.Background(), context.Background()}
				if tracked {
					for i := range contexts {
						contexts[i] = coreauth.WithUsageFeedback(contexts[i])
					}
				}
				body := `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`
				payloads := []string{
					fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"quota-hint","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":%q}}`, body),
					`{"timestamp":"2026-11-06T10:27:16Z","execution_id":"quota-no-hint","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`,
				}
				for i, payload := range payloads {
					if _, inserted, errPersist := rt.PersistClusterUsagePayloadOutcome(contexts[i], payload, start); errPersist != nil || !inserted {
						t.Fatalf("persist %d: inserted=%t error=%v", i, inserted, errPersist)
					}
				}
				order := []int{0, 1}
				if reverse {
					order = []int{1, 0}
				}
				for _, i := range order {
					if errRecord := rt.RecordUsagePayload(contexts[i], payloads[i]); errRecord != nil {
						t.Fatal(errRecord)
					}
				}
				auth, _, errGet := repo.GetAuth(context.Background(), "usage-feedback-auth")
				if errGet != nil {
					t.Fatal(errGet)
				}
				state := auth.ModelStates["gpt-5"]
				if state == nil || !state.Unavailable || state.NextRetryAfter.Before(start.Add(59*time.Minute)) || state.NextRetryAfter.After(time.Now().Add(61*time.Minute)) {
					t.Fatalf("unapplied quota reset hint lost: %#v", state)
				}
			})
		}
	}
}

func TestUsagePushPendingQuotaHintRecoveryBoundary(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	start := time.Now()
	body := `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`
	payloads := []string{
		fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"before-success","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":%q}}`, body),
		`{"timestamp":"2026-11-06T10:27:16Z","execution_id":"success-boundary","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5"}`,
		`{"timestamp":"2026-11-06T10:27:17Z","execution_id":"after-success","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`,
	}
	contexts := make([]context.Context, len(payloads))
	for i, payload := range payloads {
		contexts[i] = coreauth.WithUsageFeedback(context.Background())
		if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(contexts[i], payload, start); errPersist != nil {
			t.Fatal(errPersist)
		}
	}
	for i, payload := range payloads {
		if errRecord := rt.RecordUsagePayload(contexts[i], payload); errRecord != nil {
			t.Fatal(errRecord)
		}
	}
	auth, record, errGet := repo.GetAuth(context.Background(), "usage-feedback-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	state := auth.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.NextRetryAfter.After(time.Now().Add(time.Minute)) {
		t.Fatalf("success did not discard earlier pending hint: %#v", state)
	}
	if errFeedback := rt.ApplyUsagePayloadFeedback(contexts[2], payloads[2]); errFeedback != nil {
		t.Fatal(errFeedback)
	}
	after, afterRecord, errAfter := repo.GetAuth(context.Background(), "usage-feedback-auth")
	if errAfter != nil || record.Version != afterRecord.Version || !reflect.DeepEqual(auth.ModelStates, after.ModelStates) {
		t.Fatalf("feedback replay changed cooldown: %v", errAfter)
	}
	// Corrupted internal JSON must fail, never fall back to an empty pending state.
	feedback := coreauth.UsageFeedbackFromContext(contexts[2])
	if errUpdate := db.Create(&cluster.KVRecord{Key: feedback.ScopeKey, Value: []byte("{"), Version: 1}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	var syntaxError *json.SyntaxError
	if errFeedback := rt.ApplyUsagePayloadFeedback(contexts[2], payloads[2]); !errors.As(errFeedback, &syntaxError) {
		t.Fatalf("corrupt scope error = %v, want JSON syntax error", errFeedback)
	}
}

func TestReviewLaterCompletingRequestAppliesFeedback(t *testing.T) {
	_, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := context.Background()
	for _, payload := range []string{
		`{"timestamp":"2026-11-06T10:27:16Z","latency_ms":100,"execution_id":"short","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`,
		`{"timestamp":"2026-11-06T10:27:15Z","latency_ms":2000,"execution_id":"long","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":401}}`,
	} {
		if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
			t.Fatalf("delivery reply = %+v", reply)
		}
	}
	auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	state := auth.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.LastError == nil || state.LastError.HTTPStatus != 401 {
		t.Fatalf("later-completing 401 was discarded: %#v", state)
	}
}

func TestReviewCleanSuccessRecoversPendingFailure(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := context.Background()
	failUsage, failAuth := true, true
	if errCallback := db.Callback().Create().Before("gorm:create").Register("test:old-usage", func(tx *gorm.DB) {
		if tx.Statement.Table == "usage" && failUsage {
			tx.AddError(errors.New("usage unavailable"))
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	if errCallback := db.Callback().Update().Before("gorm:update").Register("test:old-auth", func(tx *gorm.DB) {
		if tx.Statement.Table == "auth" && failAuth {
			tx.AddError(errors.New("auth unavailable"))
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	old := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","execution_id":"old-failure","failed":true,"fail":{"status_code":429}}`
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", old}); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("old failed delivery reply = %+v", reply)
	}
	before, beforeRecord, errBefore := repo.GetAuth(ctx, "usage-feedback-auth")
	if errBefore != nil || len(before.ModelStates) != 0 {
		t.Fatalf("failed auth write changed state: %v", errBefore)
	}
	failUsage, failAuth = false, false
	// A fast clock is unrelated to Home's result observation order.
	success := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","execution_id":"new-success","access_token_sha256":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339Nano), strings.Repeat("a", 64))
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", success}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("clean success reply = %+v", reply)
	}
	before, beforeRecord, errBefore = repo.GetAuth(ctx, "usage-feedback-auth")
	if errBefore != nil || before.ModelStates["gpt-5"].Unavailable || !before.ModelStates["gpt-5"].NextRetryAfter.IsZero() {
		t.Fatalf("success did not recover pending failure: %v", errBefore)
	}
	failUsage = true
	for attempt := 0; attempt < 2; attempt++ {
		if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", old}); reply.Kind != dispatch.ReplyKindRedisError {
			t.Fatalf("old retry reply = %+v", reply)
		}
		after, afterRecord, errAfter := repo.GetAuth(ctx, "usage-feedback-auth")
		if errAfter != nil || afterRecord.Version != beforeRecord.Version || !reflect.DeepEqual(before.ModelStates, after.ModelStates) {
			t.Fatalf("old failure replaced clean success: %v", errAfter)
		}
	}
}

func TestUsagePushConcurrentFailedFeedback(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	failWrites := true
	if errCallback := db.Callback().Create().Before("gorm:create").Register("test:concurrent-usage", func(tx *gorm.DB) {
		if tx.Statement.Table == "usage" && failWrites {
			tx.AddError(errors.New("usage unavailable"))
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	_, before, errBefore := repo.GetAuth(context.Background(), "usage-feedback-auth")
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	type observation struct {
		ctx     context.Context
		payload string
		err     error
	}
	observed := make(chan observation, 8)
	start := make(chan struct{})
	for i := 0; i < cap(observed); i++ {
		go func() {
			<-start
			ctx := coreauth.WithUsageFeedback(context.Background())
			body := ""
			if i == 0 {
				body = `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`
			}
			payload := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"concurrent-%d","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":%q}}`, i, body)
			_, _, errPersist := rt.PersistClusterUsagePayloadOutcome(ctx, payload, time.Now())
			observed <- observation{ctx, payload, errPersist}
		}()
	}
	close(start)
	items := make([]observation, 0, cap(observed))
	for i := 0; i < cap(observed); i++ {
		item := <-observed
		if !errors.Is(item.err, cluster.ErrUsageWriteAborted) {
			t.Fatalf("observe failed usage: %v", item.err)
		}
		items = append(items, item)
	}
	applied := make(chan error, len(items))
	for _, item := range items {
		go func() { applied <- rt.ApplyUsagePayloadFeedback(item.ctx, item.payload) }()
	}
	for range items {
		if errApply := <-applied; errApply != nil {
			t.Fatal(errApply)
		}
	}
	auth, after, errAfter := repo.GetAuth(context.Background(), "usage-feedback-auth")
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	state := auth.ModelStates["gpt-5"]
	if after.Version != before.Version+1 || state == nil || state.LastError == nil || state.LastError.HTTPStatus != 429 || state.NextRetryAfter.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("concurrent results lost a quota constraint: %#v", state)
	}
	failWrites = false
	if reply := handleUsage(context.Background(), dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", items[0].payload}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("recovered usage reply = %+v", reply)
	}
	local, _ := rt.CoreManager().GetByID("usage-feedback-auth")
	if local.Success != 0 || local.Failed != 1 {
		t.Fatalf("recovered request counted incorrectly: %d/%d", local.Success, local.Failed)
	}
}

func TestUsagePushFeedbackPreservesResultGuards(t *testing.T) {
	for _, test := range []struct {
		name               string
		requestScoped      bool
		status, wantStatus int
	}{
		{"old_token_401", false, 401, 401},
		{"request_scoped", true, 401, 401},
		{"old_token_quota_still_applies", false, 429, 429},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, repo, rt := newUsageFeedbackTestRuntime(t)
			ctx := context.Background()
			auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
			if errGet != nil {
				t.Fatal(errGet)
			}
			auth.Metadata = map[string]any{"access_token": "current-token"}
			if _, errUpdate := repo.UpsertAuth(ctx, auth, "test"); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			currentHash := coreauth.AccessTokenSHA256(auth)
			failWrites := true
			if errCallback := db.Callback().Create().Before("gorm:create").Register("test:pending-usage", func(tx *gorm.DB) {
				if tx.Statement.Table == "usage" && failWrites {
					tx.AddError(errors.New("usage unavailable"))
				}
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			pending := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"pending","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","access_token_sha256":%q,"failed":true,"fail":{"status_code":401}}`, currentHash)
			pendingCtx := coreauth.WithUsageFeedback(ctx)
			if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(pendingCtx, pending, time.Now()); !errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
				t.Fatalf("pending write = %v", errPersist)
			}
			code := ""
			observedHash := coreauth.AccessTokenSHA256(&coreauth.Auth{Metadata: map[string]any{"access_token": "old-token"}})
			if test.requestScoped {
				code, observedHash = coreauth.ErrorCodeRequestScoped, currentHash
			}
			ignored := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:16Z","execution_id":"ignored","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","access_token_sha256":%q,"failed":true,"fail":{"status_code":%d,"code":%q}}`, observedHash, test.status, code)
			failWrites = false
			if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", ignored}); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("ignored result reply = %+v", reply)
			}
			if errFeedback := rt.ApplyUsagePayloadFeedback(pendingCtx, pending); errFeedback != nil {
				t.Fatal(errFeedback)
			}
			after, _, errAfter := repo.GetAuth(ctx, "usage-feedback-auth")
			if errAfter != nil {
				t.Fatal(errAfter)
			}
			state := after.ModelStates["gpt-5"]
			if state == nil || !state.Unavailable || state.LastError == nil || state.LastError.HTTPStatus != test.wantStatus {
				t.Fatalf("result guard changed feedback ordering: %#v", state)
			}
		})
	}
}

func TestUsagePushObservationFailureIsFailClosed(t *testing.T) {
	for _, failure := range []string{"pending", "receipt"} {
		t.Run(failure, func(t *testing.T) {
			db, repo, rt := newUsageFeedbackTestRuntime(t)
			injected := errors.New("observation unavailable")
			failObservation := true
			if errCallback := db.Callback().Create().Before("gorm:create").Register("test:observation-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "usage" {
					tx.AddError(errors.New("usage unavailable"))
				}
				if record, ok := tx.Statement.Dest.(*cluster.KVRecord); ok {
					if failObservation && ((failure == "pending" && strings.Contains(record.Key, ":scope:")) || (failure == "receipt" && strings.Contains(record.Key, ":event:"))) {
						tx.AddError(injected)
					}
				}
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":401}}`
			ctx := coreauth.WithUsageFeedback(context.Background())
			_, _, errPersist := rt.PersistClusterUsagePayloadOutcome(ctx, payload, time.Now())
			if failure == "pending" {
				if !errors.Is(errPersist, injected) || errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
					t.Fatalf("observation failure classification = %v", errPersist)
				}
			} else {
				if !errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
					t.Fatalf("usage failure classification = %v", errPersist)
				}
				if errFeedback := rt.ApplyUsagePayloadFeedback(ctx, payload); !errors.Is(errFeedback, injected) {
					t.Fatalf("receipt failure = %v", errFeedback)
				}
			}
			if reply := handleUsage(context.Background(), dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("observation failure acknowledged: %+v", reply)
			}
			auth, _, errAuth := repo.GetAuth(context.Background(), "usage-feedback-auth")
			if errAuth != nil || len(auth.ModelStates) != 0 {
				t.Fatalf("unrecorded observation changed availability: %v", errAuth)
			}
			var rows int64
			if errCount := db.Model(&cluster.UsageRecord{}).Count(&rows).Error; errCount != nil || rows != 0 {
				t.Fatalf("failed observation left usage rows: %d, %v", rows, errCount)
			}
			wantPending := int64(0)
			if failure == "receipt" {
				wantPending = 1
			}
			if errCount := db.Model(&cluster.KVRecord{}).Where("key LIKE ?", "internal:usage-feedback:%").Count(&rows).Error; errCount != nil || rows != wantPending {
				t.Fatalf("failed transaction lost pending state: %d, %v", rows, errCount)
			}
			if failure == "receipt" {
				// Completion failure must roll back the auth write and preserve a
				// retryable pending event, not acknowledge it or erase its receipt.
				failObservation = false
				if errFeedback := rt.ApplyUsagePayloadFeedback(ctx, payload); errFeedback != nil {
					t.Fatal(errFeedback)
				}
				recovered, _, errGet := repo.GetAuth(context.Background(), "usage-feedback-auth")
				if errGet != nil || !recovered.ModelStates["gpt-5"].Unavailable {
					t.Fatalf("receipt retry lost cooldown: %v", errGet)
				}
			}
		})
	}
}

func TestUsagePushPendingCompletionRollback(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := context.Background()
	payload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"completion-rollback","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`
	failCompletion := true
	if errCallback := db.Callback().Delete().Before("gorm:delete").Register("test:feedback-completion", func(tx *gorm.DB) {
		if tx.Statement.Table == (cluster.KVRecord{}).TableName() && failCompletion {
			tx.AddError(errors.New("completion unavailable"))
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("completion failure acknowledged: %+v", reply)
	}
	auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil || len(auth.ModelStates) != 0 {
		t.Fatalf("completion failure committed auth: %v", errGet)
	}
	local, _ := rt.CoreManager().GetByID("usage-feedback-auth")
	if len(local.ModelStates) != 0 {
		t.Fatal("completion failure changed local state")
	}
	failCompletion = false
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("completion retry: %+v", reply)
	}
	auth, _, errGet = repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil || !auth.ModelStates["gpt-5"].Unavailable {
		t.Fatalf("retry lost cooldown: %v", errGet)
	}
	local, _ = rt.CoreManager().GetByID("usage-feedback-auth")
	if local.Failed != 1 {
		t.Fatalf("completion retry counted twice: %d", local.Failed)
	}
}

func TestUsagePushFeedbackBackpressure(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := context.Background()
	var overflow string
	admitted := int64(0)
	for i := 0; i < 256; i++ {
		payload := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"backlog-%d","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`, i)
		_, inserted, errPersist := rt.PersistClusterUsagePayloadOutcome(coreauth.WithUsageFeedback(ctx), payload, time.Now())
		if errors.Is(errPersist, cluster.ErrUsageFeedbackBacklog) {
			overflow = payload
			break
		}
		if errPersist != nil || !inserted {
			t.Fatalf("stage backlog: inserted=%t, error=%v", inserted, errPersist)
		}
		admitted++
	}
	if overflow == "" {
		t.Fatal("pending feedback was not bounded")
	}
	// The new event is not admitted, but its call repairs the existing batch.
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", overflow}); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("unadmitted event acknowledged: %+v", reply)
	}
	var rows int64
	if errCount := db.Model(&cluster.UsageRecord{}).Count(&rows).Error; errCount != nil || rows != admitted {
		t.Fatalf("backpressure stored new usage: %d, %v", rows, errCount)
	}
	auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil || !auth.ModelStates["gpt-5"].Unavailable {
		t.Fatalf("backpressure did not drain pending feedback: %v", errGet)
	}
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", overflow}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("backlog did not recover: %+v", reply)
	}
	local, _ := rt.CoreManager().GetByID("usage-feedback-auth")
	if local.Failed != 1 {
		t.Fatalf("draining feedback counted unhandled requests: %d", local.Failed)
	}
}

func TestUsagePushUnknownCredentialDoesNotAccumulateFeedback(t *testing.T) {
	db, _, rt := newUsageFeedbackTestRuntime(t)
	payload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"unknown-auth","provider":"codex","auth_index":"deleted-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`
	for i := 0; i < 2; i++ {
		if reply := handleUsage(context.Background(), dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
			t.Fatalf("historical usage rejected: %+v", reply)
		}
	}
	var rows int64
	if errCount := db.Model(&cluster.KVRecord{}).Where("key LIKE ?", "internal:usage-feedback:%").Count(&rows).Error; errCount != nil || rows != 0 {
		t.Fatalf("unknown credential left feedback: %d, %v", rows, errCount)
	}
}

func TestUsagePushPendingHintDoesNotRestartOnRetry(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	payload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"fixed-reset","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`
	failWrites := true
	if errCallback := db.Callback().Create().Before("gorm:create").Register("test:freeze-reset", func(tx *gorm.DB) {
		if tx.Statement.Table == "usage" && failWrites {
			tx.AddError(errors.New("usage unavailable"))
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	firstSeen := time.Now().Add(-30 * time.Minute)
	if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(coreauth.WithUsageFeedback(context.Background()), payload, firstSeen); !errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
		t.Fatalf("failed first delivery: %v", errPersist)
	}
	failWrites = false
	if reply := handleUsage(context.Background(), dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("recovered delivery: %+v", reply)
	}
	auth, _, errGet := repo.GetAuth(context.Background(), "usage-feedback-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	if got := auth.ModelStates["gpt-5"].NextRetryAfter; !got.Equal(firstSeen.Add(time.Hour)) {
		t.Fatalf("retry restarted reset period: got %s want %s", got, firstSeen.Add(time.Hour))
	}
}

func TestUsagePushPreservesFeedbackOnDatabaseFailure(t *testing.T) {
	for _, test := range []struct {
		statusCode int
		writeTable string
	}{
		{http.StatusUnauthorized, "usage"},
		{http.StatusTooManyRequests, "usage"},
		{http.StatusUnauthorized, "billing_charge"},
		{http.StatusTooManyRequests, "billing_charge"},
	} {
		t.Run(fmt.Sprintf("%s/%s", http.StatusText(test.statusCode), test.writeTable), func(t *testing.T) {
			statusCode := test.statusCode
			ctx := context.Background()
			db, repo, rt := newUsageFeedbackTestRuntime(t)
			const authID = "usage-feedback-auth"
			const model = "gpt-5"

			// Fail usage or billing writes while leaving shared auth-state mutation
			// available. Rolling back usage must not suppress its feedback.
			failUsage := true
			writeFailure := errors.New("usage write unavailable")
			if errCallback := db.Callback().Create().Before("gorm:create").Register("test:usage-write-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == test.writeTable && failUsage {
					tx.AddError(writeFailure)
				}
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			payload := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","auth_index":%q,"model":%q,"execution_id":"usage-feedback-exec","request_id":"usage-feedback-request","tokens":{"input_tokens":1,"total_tokens":1},"failed":true,"fail":{"status_code":%d,"body":"upstream failure"}}`, authID, model, statusCode)
			args := []string{"LPUSH", "usage", payload}
			env := dispatch.Env{Runtime: rt}
			// Invalid events must not become scheduler feedback when storage is down.
			invalid := fmt.Sprintf(`{"provider":"codex","auth_index":%q,"model":%q,"failed":true,"fail":{"status_code":%d}}`, authID, model, statusCode)
			if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", invalid}); reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("invalid payload reply = %+v", reply)
			}
			unchanged, _ := rt.CoreManager().GetByID(authID)
			if len(unchanged.ModelStates) != 0 || unchanged.Failed != 0 {
				t.Fatal("invalid event changed scheduling state or counters")
			}
			for attempt := 0; attempt < 2; attempt++ {
				if reply := handleUsage(ctx, env, args); reply.Kind != dispatch.ReplyKindRedisError {
					t.Fatalf("failed write reply = %+v, want Redis error", reply)
				}
				local, ok := rt.CoreManager().GetByID(authID)
				if !ok || local == nil {
					t.Fatal("missing local auth")
				}
				state := local.ModelStates[model]
				if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() {
					t.Fatalf("failed usage write suppressed cooldown: %#v", state)
				}
				if local.Success != 0 || local.Failed != 0 {
					t.Fatalf("uncommitted delivery changed counters: success=%d failed=%d", local.Success, local.Failed)
				}
				for _, bucket := range local.RecentRequestsSnapshot(time.Now()) {
					if bucket.Success != 0 || bucket.Failed != 0 {
						t.Fatalf("uncommitted delivery changed recent requests: %+v", bucket)
					}
				}
				persisted, _, errGet := repo.GetAuth(ctx, authID)
				if errGet != nil {
					t.Fatal(errGet)
				}
				if persisted == nil || persisted.ModelStates[model] == nil || !persisted.ModelStates[model].Unavailable {
					t.Fatal("failed usage write suppressed shared cooldown")
				}
			}
			var usageCount int64
			if errCount := db.Model(&cluster.UsageRecord{}).Count(&usageCount).Error; errCount != nil || usageCount != 0 {
				t.Fatalf("failed usage writes left %d rows: %v", usageCount, errCount)
			}

			if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(ctx, payload, time.Now()); !errors.Is(errPersist, writeFailure) || !errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
				t.Fatalf("write failure classification = %v", errPersist)
			}
			failUsage = false
			if reply := handleUsage(ctx, env, args); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
				t.Fatalf("committed write reply = %+v, want integer 1", reply)
			}
			committed, ok := rt.CoreManager().GetByID(authID)
			if !ok || committed == nil || committed.Success != 0 || committed.Failed != 1 {
				t.Fatalf("first committed delivery must count once: %#v", committed)
			}
			var recentFailures int64
			for _, bucket := range committed.RecentRequestsSnapshot(time.Now()) {
				recentFailures += bucket.Failed
			}
			if recentFailures != 1 {
				t.Fatalf("recent failures = %d, want 1", recentFailures)
			}

			// A newer success clears the failure. Replaying the stored failure must
			// neither restore its cooldown nor increment request counters.
			success := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:16Z","provider":"codex","auth_index":%q,"model":%q}`, authID, model)
			if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", success}); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("newer success reply = %+v", reply)
			}
			beforeReplay, _ := rt.CoreManager().GetByID(authID)
			if beforeReplay.ModelStates[model].Unavailable {
				t.Fatal("newer success did not clear cooldown")
			}
			if reply := handleUsage(ctx, env, args); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
				t.Fatalf("duplicate reply = %+v, want integer 1", reply)
			}
			afterReplay, _ := rt.CoreManager().GetByID(authID)
			if !reflect.DeepEqual(beforeReplay.ModelStates, afterReplay.ModelStates) || beforeReplay.Success != afterReplay.Success || beforeReplay.Failed != afterReplay.Failed {
				t.Fatalf("duplicate changed state or counters: before=%#v after=%#v", beforeReplay, afterReplay)
			}
			if errCount := db.Model(&cluster.UsageRecord{}).Count(&usageCount).Error; errCount != nil || usageCount != 2 {
				t.Fatalf("retry usage count = %d, error=%v, want 2", usageCount, errCount)
			}

			var receipts int64
			if errCount := db.Model(&cluster.KVRecord{}).Where("key LIKE ?", "internal:usage-feedback:event:%").Count(&receipts).Error; errCount != nil || receipts != 0 {
				t.Fatalf("committed usage retained pending receipts: %d, %v", receipts, errCount)
			}

			// Preserve the storage error identity instead of swallowing feedback failures.
			injected := errors.New("auth write unavailable")
			failAuth := true
			if errCallback := db.Callback().Update().Before("gorm:update").Register("test:auth-write-failure", func(tx *gorm.DB) {
				if tx.Statement.Table == "auth" && failAuth {
					tx.AddError(injected)
				}
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			newPayload := strings.Replace(payload, "usage-feedback-exec", "usage-feedback-write-failure", 1)
			newPayload = strings.Replace(newPayload, "10:27:15Z", "10:27:17Z", 1)
			failUsage = true
			feedbackCtx := coreauth.WithUsageFeedback(ctx)
			if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(feedbackCtx, newPayload, time.Now()); !errors.Is(errPersist, cluster.ErrUsageWriteAborted) {
				t.Fatalf("expected an observed write failure: %v", errPersist)
			}
			if errFeedback := rt.ApplyUsagePayloadFeedback(feedbackCtx, newPayload); !errors.Is(errFeedback, injected) {
				t.Fatalf("feedback error = %v, want wrapped auth failure", errFeedback)
			}
			failUsage = true
			if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", newPayload}); reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("feedback failure was acknowledged: %+v", reply)
			}
			// A failed auth mutation must not complete feedback or change availability.
			failed, _ := rt.CoreManager().GetByID(authID)
			if !reflect.DeepEqual(beforeReplay.ModelStates, failed.ModelStates) {
				t.Fatal("failed auth mutation advanced local feedback state")
			}
			failAuth = false
			if errFeedback := rt.ApplyUsagePayloadFeedback(feedbackCtx, newPayload); errFeedback != nil {
				t.Fatal(errFeedback)
			}
			recovered, _, errRecovered := repo.GetAuth(ctx, authID)
			if errRecovered != nil || !recovered.ModelStates[model].Unavailable {
				t.Fatalf("feedback recovery lost the transition or its fence: %v", errRecovered)
			}
		})
	}
}
