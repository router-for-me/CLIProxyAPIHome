package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestReviewPendingFeedbackCrossesRestoredIdentity(t *testing.T) {
	for _, usageStored := range []bool{false, true} {
		for _, originalContext := range []bool{false, true} {
			t.Run(fmt.Sprintf("stored_%t/original_context_%t", usageStored, originalContext), func(t *testing.T) {
				db, repo, rt := newUsageFeedbackTestRuntime(t)
				ctx := context.Background()
				auth, beforeRecord, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
				if errGet != nil {
					t.Fatal(errGet)
				}
				failUsage := !usageStored
				if errCallback := db.Callback().Create().Before("gorm:create").Register("test:identity-usage-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "usage" && failUsage {
						tx.AddError(errors.New("usage unavailable"))
					}
				}); errCallback != nil {
					t.Fatal(errCallback)
				}
				payload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"previous-identity","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","tokens":{"input_tokens":1,"total_tokens":1},"failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`
				pendingCtx := coreauth.WithUsageFeedback(ctx)
				_, inserted, errPersist := rt.PersistClusterUsagePayloadOutcome(pendingCtx, payload, time.Now())
				if usageStored {
					if errPersist != nil || !inserted {
						t.Fatalf("persist old feedback: inserted=%t, error=%v", inserted, errPersist)
					}
				} else if !errors.Is(errPersist, cluster.ErrUsageWriteAborted) || inserted {
					t.Fatalf("aborted usage: inserted=%t, error=%v", inserted, errPersist)
				}

				if errDelete := repo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
					t.Fatal(errDelete)
				}
				auth.StateVersion = 0
				if _, errRestore := repo.UpsertAuth(ctx, auth, "restore"); errRestore != nil {
					t.Fatal(errRestore)
				}
				restored, restoredRecord, errRestored := repo.GetAuth(ctx, auth.ID)
				if errRestored != nil {
					t.Fatal(errRestored)
				}
				if restoredRecord.QuotaIdentityVersion <= beforeRecord.QuotaIdentityVersion {
					t.Fatal("restore did not advance identity generation")
				}
				if _, errUpdate := rt.CoreManager().Update(coreauth.WithSkipPersist(ctx), restored); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				if originalContext {
					// An in-flight caller must recheck the live identity, not trust
					// the context it obtained before deletion/restoration.
					if errFeedback := rt.ApplyUsagePayloadFeedback(pendingCtx, payload); errFeedback != nil {
						t.Fatal(errFeedback)
					}
				}
				deliveryRuntime := rt
				if !originalContext {
					otherHome, errRuntime := home.NewRuntime(&config.Config{})
					if errRuntime != nil {
						t.Fatal(errRuntime)
					}
					otherHome.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.11"))
					t.Cleanup(otherHome.Stop)
					if _, errRegister := otherHome.CoreManager().Register(coreauth.WithSkipPersist(ctx), restored); errRegister != nil {
						t.Fatal(errRegister)
					}
					deliveryRuntime = otherHome
				}
				failUsage = false
				for attempt := 0; attempt < 2; attempt++ {
					if reply := handleUsage(ctx, dispatch.Env{Runtime: deliveryRuntime}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
						t.Fatalf("old identity retry = %+v", reply)
					}
					after, afterRecord, errAfter := repo.GetAuth(ctx, auth.ID)
					if errAfter != nil {
						t.Fatal(errAfter)
					}
					if afterRecord.Version != restoredRecord.Version || !reflect.DeepEqual(after.ModelStates, restored.ModelStates) {
						t.Fatalf("old feedback changed restored identity: %#v", after.ModelStates["gpt-5"])
					}
				}
				for _, table := range []string{"usage", "billing_charge"} {
					var rows int64
					if errCount := db.Table(table).Count(&rows).Error; errCount != nil || rows != 1 {
						t.Fatalf("%s rows = %d, error=%v", table, rows, errCount)
					}
				}
				local, _ := deliveryRuntime.CoreManager().GetByID(auth.ID)
				wantCount := int64(0)
				if !usageStored {
					wantCount = 1
				}
				if local.Success != 0 || local.Failed != wantCount || !reflect.DeepEqual(local.ModelStates, restored.ModelStates) {
					t.Fatalf("stale feedback changed local state or request count: %d/%d, %#v", local.Success, local.Failed, local.ModelStates)
				}
				var pending int64
				if errCount := db.Model(&cluster.KVRecord{}).Where("key LIKE ?", "internal:usage-feedback:%").Count(&pending).Error; errCount != nil || pending != 0 {
					t.Fatalf("obsolete feedback was not completed: %d, %v", pending, errCount)
				}
			})
		}
	}
}

func TestUsagePushMixedIdentityFeedbackRollback(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := context.Background()
	auth, beforeRecord, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	oldPayload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"old-batch","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`
	oldCtx := coreauth.WithUsageFeedback(ctx)
	if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(oldCtx, oldPayload, time.Now()); errPersist != nil {
		t.Fatal(errPersist)
	}
	if errDelete := repo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	auth.StateVersion = 0
	if _, errRestore := repo.UpsertAuth(ctx, auth, "restore"); errRestore != nil {
		t.Fatal(errRestore)
	}
	restored, restoredRecord, errRestored := repo.GetAuth(ctx, auth.ID)
	if errRestored != nil {
		t.Fatal(errRestored)
	}
	if restoredRecord.QuotaIdentityVersion <= beforeRecord.QuotaIdentityVersion {
		t.Fatal("identity did not change")
	}
	if _, errUpdate := rt.CoreManager().Update(coreauth.WithSkipPersist(ctx), restored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	newPayload := `{"timestamp":"2026-11-06T10:27:16Z","execution_id":"new-batch","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`
	if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(coreauth.WithUsageFeedback(ctx), newPayload, time.Now()); errPersist != nil {
		t.Fatal(errPersist)
	}

	failCompletion := true
	injected := errors.New("completion unavailable")
	if errCallback := db.Callback().Delete().Before("gorm:delete").Register("test:mixed-identity-completion", func(tx *gorm.DB) {
		if tx.Statement.Table == (cluster.KVRecord{}).TableName() && failCompletion {
			tx.AddError(injected)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	if errFeedback := rt.ApplyUsagePayloadFeedback(oldCtx, oldPayload); !errors.Is(errFeedback, injected) {
		t.Fatalf("completion failure = %v", errFeedback)
	}
	unchanged, unchangedRecord, errUnchanged := repo.GetAuth(ctx, auth.ID)
	if errUnchanged != nil {
		t.Fatal(errUnchanged)
	}
	if unchangedRecord.Version != restoredRecord.Version || !reflect.DeepEqual(unchanged.ModelStates, restored.ModelStates) {
		t.Fatal("completion failure committed partial availability")
	}
	var scope cluster.KVRecord
	if errScope := db.Where("key = ?", coreauth.UsageFeedbackFromContext(oldCtx).ScopeKey).First(&scope).Error; errScope != nil {
		t.Fatal(errScope)
	}
	var batch struct {
		Pending []json.RawMessage `json:"pending"`
	}
	if errDecode := json.Unmarshal(scope.Value, &batch); errDecode != nil || len(batch.Pending) != 2 {
		t.Fatalf("rollback lost pending events: %d, %v", len(batch.Pending), errDecode)
	}

	failCompletion = false
	// The old caller can drain the batch, but only the current identity's event
	// may affect availability. The previous identity's hour-long hint is obsolete.
	if errFeedback := rt.ApplyUsagePayloadFeedback(oldCtx, oldPayload); errFeedback != nil {
		t.Fatal(errFeedback)
	}
	after, _, errAfter := repo.GetAuth(ctx, auth.ID)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	state := after.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(time.Now().Add(time.Minute)) {
		t.Fatalf("mixed generations corrupted current cooldown: %#v", state)
	}
	var pending int64
	if errCount := db.Model(&cluster.KVRecord{}).Where("key = ?", scope.Key).Count(&pending).Error; errCount != nil || pending != 0 {
		t.Fatalf("batch not completed: %d, %v", pending, errCount)
	}
}

func TestUsagePushOldIdentitySuccessDoesNotClearCurrentQuota(t *testing.T) {
	_, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := context.Background()
	auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	oldSuccess := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"old-success","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5"}`
	if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(coreauth.WithUsageFeedback(ctx), oldSuccess, time.Now()); errPersist != nil {
		t.Fatal(errPersist)
	}
	if errDelete := repo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	auth.StateVersion = 0
	if _, errRestore := repo.UpsertAuth(ctx, auth, "restore"); errRestore != nil {
		t.Fatal(errRestore)
	}
	restored, _, errRestored := repo.GetAuth(ctx, auth.ID)
	if errRestored != nil {
		t.Fatal(errRestored)
	}
	if _, errUpdate := rt.CoreManager().Update(coreauth.WithSkipPersist(ctx), restored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	// Establish availability for the new identity independently of pending usage.
	currentQuota := `{"provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`
	if errRecord := rt.RecordUsagePayload(ctx, currentQuota); errRecord != nil {
		t.Fatal(errRecord)
	}
	before, beforeRecord, errBefore := repo.GetAuth(ctx, auth.ID)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	if state := before.ModelStates["gpt-5"]; state == nil || !state.Unavailable {
		t.Fatal("current quota was not established")
	}
	if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", oldSuccess}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("old success retry = %+v", reply)
	}
	after, afterRecord, errAfter := repo.GetAuth(ctx, auth.ID)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if afterRecord.Version != beforeRecord.Version || !reflect.DeepEqual(before.ModelStates, after.ModelStates) {
		t.Fatal("old identity success cleared current quota")
	}
}

func TestUsagePushFeedbackMissingIdentityFailsClosed(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	ctx := coreauth.WithUsageFeedback(context.Background())
	payload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"missing-identity","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429}}`
	if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(ctx, payload, time.Now()); errPersist != nil {
		t.Fatal(errPersist)
	}
	var scope cluster.KVRecord
	if errScope := db.Where("key = ?", coreauth.UsageFeedbackFromContext(ctx).ScopeKey).First(&scope).Error; errScope != nil {
		t.Fatal(errScope)
	}
	var batch struct {
		Pending []map[string]json.RawMessage `json:"pending"`
	}
	if errDecode := json.Unmarshal(scope.Value, &batch); errDecode != nil || len(batch.Pending) != 1 {
		t.Fatalf("pending data: %v", errDecode)
	}
	delete(batch.Pending[0], "quota_identity_version")
	invalid, errEncode := json.Marshal(batch)
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	if errUpdate := db.Model(&cluster.KVRecord{}).Where("key = ?", scope.Key).Update("value", invalid).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if reply := handleUsage(context.Background(), dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("unknown identity acknowledged: %+v", reply)
	}
	auth, _, errGet := repo.GetAuth(context.Background(), "usage-feedback-auth")
	if errGet != nil || len(auth.ModelStates) != 0 {
		t.Fatalf("unknown identity changed availability: %v", errGet)
	}
	// Repair the original metadata, then verify the error did not consume it.
	if errUpdate := db.Model(&cluster.KVRecord{}).Where("key = ?", scope.Key).Update("value", scope.Value).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if reply := handleUsage(context.Background(), dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("repaired metadata retry = %+v", reply)
	}
}

func TestUsagePushFeedbackIdentityCheckedAfterAuthLock(t *testing.T) {
	db, repo, rt := newUsageFeedbackTestRuntime(t)
	if db.Dialector.Name() != "postgres" {
		t.Skip("requires PostgreSQL row locks and multiple connections")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	auth, beforeRecord, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	payload := `{"timestamp":"2026-11-06T10:27:15Z","execution_id":"concurrent-restore","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`
	pendingCtx := coreauth.WithUsageFeedback(ctx)
	if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(pendingCtx, payload, time.Now()); errPersist != nil {
		t.Fatal(errPersist)
	}
	auth.StateVersion = 0
	restoredReady, releaseRestore, restoredDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var errRestore error
	go func() {
		defer close(restoredDone)
		errRestore = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			txRepo := cluster.NewRepository(tx)
			if errDelete := txRepo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
				return errDelete
			}
			if _, errUpsert := txRepo.UpsertAuth(ctx, auth, "restore"); errUpsert != nil {
				return errUpsert
			}
			close(restoredReady)
			select {
			case <-releaseRestore:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-restoredDone })
	select {
	case <-restoredReady:
	case <-restoredDone:
		t.Fatalf("prepare concurrent restore: %v", errRestore)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	readingAuth := make(chan struct{})
	var signalRead sync.Once
	if errCallback := db.Callback().Query().Before("gorm:query").Register("test:identity-auth-lock", func(tx *gorm.DB) {
		if tx.Statement.Table == "auth" {
			if locking, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking); ok && locking.Strength == "UPDATE" {
				signalRead.Do(func() { close(readingAuth) })
			}
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	applied := make(chan error, 1)
	appliedDone := make(chan struct{})
	go func() { defer close(appliedDone); applied <- rt.ApplyUsagePayloadFeedback(pendingCtx, payload) }()
	t.Cleanup(func() { cancel(); <-appliedDone })
	select {
	case <-readingAuth:
	case errApply := <-applied:
		t.Fatalf("feedback finished before acquiring auth lock: %v", errApply)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The restore is still uncommitted when feedback reaches SELECT FOR UPDATE.
	// A generation check performed earlier would observe the previous identity.
	close(releaseRestore)
	<-restoredDone
	if errRestore != nil {
		t.Fatal(errRestore)
	}
	if errApply := <-applied; errApply != nil {
		t.Fatal(errApply)
	}
	after, afterRecord, errAfter := repo.GetAuth(ctx, auth.ID)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if afterRecord.QuotaIdentityVersion <= beforeRecord.QuotaIdentityVersion || len(after.ModelStates) != 0 {
		t.Fatalf("concurrent restore inherited stale feedback: %#v", after.ModelStates)
	}
}

func TestUsagePushPendingFeedbackPreservesSameIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		refresh   bool
		wantQuota bool
	}{
		{"label_update", http.StatusTooManyRequests, false, true},
		{"token_refresh_quota", http.StatusTooManyRequests, true, true},
		{"token_refresh_unauthorized", http.StatusUnauthorized, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, repo, rt := newUsageFeedbackTestRuntime(t)
			ctx := context.Background()
			auth, _, errGet := repo.GetAuth(ctx, "usage-feedback-auth")
			if errGet != nil {
				t.Fatal(errGet)
			}
			auth.Metadata = map[string]any{"type": "codex", "access_token": "old-token"}
			before, errUpsert := repo.UpsertAuth(ctx, auth, "test")
			if errUpsert != nil {
				t.Fatal(errUpsert)
			}
			body := `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`
			payload := fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","execution_id":"same-identity","provider":"codex","auth_index":"usage-feedback-auth","model":"gpt-5","access_token_sha256":%q,"failed":true,"fail":{"status_code":%d,"body":%q}}`, coreauth.AccessTokenSHA256(auth), test.status, body)
			if _, _, errPersist := rt.PersistClusterUsagePayloadOutcome(coreauth.WithUsageFeedback(ctx), payload, time.Now()); errPersist != nil {
				t.Fatal(errPersist)
			}
			auth, _, errGet = repo.GetAuth(ctx, auth.ID)
			if errGet != nil {
				t.Fatal(errGet)
			}
			if test.refresh {
				auth.Metadata["access_token"] = "new-token"
			} else {
				auth.Label = "updated label"
			}
			if _, errUpdate := repo.UpsertAuth(ctx, auth, "test"); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			current, currentRecord, errCurrent := repo.GetAuth(ctx, auth.ID)
			if errCurrent != nil {
				t.Fatal(errCurrent)
			}
			if currentRecord.Version <= before.Version || currentRecord.QuotaIdentityVersion != before.QuotaIdentityVersion {
				t.Fatal("ordinary update should change revision without changing identity generation")
			}
			if _, errUpdate := rt.CoreManager().Update(coreauth.WithSkipPersist(ctx), current); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("same identity retry = %+v", reply)
			}
			after, _, errAfter := repo.GetAuth(ctx, auth.ID)
			if errAfter != nil {
				t.Fatal(errAfter)
			}
			state := after.ModelStates["gpt-5"]
			if test.wantQuota {
				if state == nil || !state.Unavailable || state.NextRetryAfter.Before(time.Now().Add(59*time.Minute)) {
					t.Fatalf("same-generation quota feedback was discarded: %#v", state)
				}
			} else if !reflect.DeepEqual(after.ModelStates, current.ModelStates) {
				t.Fatalf("old-token 401 affected refreshed credential: %#v", state)
			}
		})
	}
}
