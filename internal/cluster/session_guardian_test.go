package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gorm.io/gorm"
)

// guardianLegacyBindingRecord freezes the schema before Guardian selection metadata.
type guardianLegacyBindingRecord struct {
	Key          string    `gorm:"column:key;type:varchar(64);primaryKey"`
	CredentialID string    `gorm:"column:credential_id;not null"`
	Revision     string    `gorm:"column:revision;type:varchar(36);not null"`
	ExpiresAt    time.Time `gorm:"column:expires_at;not null"`
}

func (guardianLegacyBindingRecord) TableName() string { return "session_affinity" }

func TestGuardianSessionSchemaUpgrade(t *testing.T) {
	testGuardianSessionSchemaUpgrade(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresGuardianSessionSchemaUpgrade(t *testing.T) {
	testGuardianSessionSchemaUpgrade(t, newPostgresQuiescenceRepository(t))
}

func testGuardianSessionSchemaUpgrade(t *testing.T, repo *Repository) {
	t.Helper()
	if errDrop := repo.db.Migrator().DropTable(&SessionAffinityRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if errCreate := repo.db.AutoMigrate(&guardianLegacyBindingRecord{}); errCreate != nil {
		t.Fatal(errCreate)
	}
	legacy := guardianLegacyBindingRecord{Key: strings.Repeat("a", 64), CredentialID: "parent", Revision: "legacy-revision", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if errCreate := repo.db.Create(&legacy).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errVersion := repo.db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 18).Error; errVersion != nil {
		t.Fatal(errVersion)
	}
	if errMigrate := AutoMigrate(repo.db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if repo.db.Dialector.Name() == "postgres" {
		assertPostgresSchemaMigrationVersion(t, repo.db, currentDatabaseVersion)
	}
	if repo.db.Migrator().HasTable("session_affinity_sequence") {
		t.Fatal("migration created an independent sequence table")
	}
	for _, index := range []string{"idx_session_affinity_counter_expires", "idx_session_affinity_binding_expires"} {
		if !repo.db.Migrator().HasIndex(&SessionAffinityRecord{}, index) {
			t.Fatalf("migration did not create cleanup index %s", index)
		}
	}
	binding, errLoad := repo.LoadSessionBinding(t.Context(), legacy.Key)
	if errLoad != nil || binding.AuthID != legacy.CredentialID || binding.Revision != legacy.Revision {
		t.Fatalf("migration changed binding: %+v err=%v", binding, errLoad)
	}
	// Opaque legacy keys cannot be backfilled to sessions. Their next successful
	// dispatch adds metadata without replacing the live binding or its revision.
	sessionKey := strings.Repeat("f", 64)
	commit := coreauth.SessionAffinityCommit{Key: legacy.Key, ExpectedRevision: legacy.Revision, SessionKey: sessionKey, Provider: "codex", AuthID: legacy.CredentialID, TTL: time.Hour}
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("enrich legacy binding=%t err=%v", swapped, errSwap)
	}
	latest, errLatest := repo.LoadLatestSessionBinding(t.Context(), sessionKey, []string{"codex"})
	if errLatest != nil || latest.AuthID != legacy.CredentialID || latest.Revision != legacy.Revision {
		t.Fatalf("legacy binding not discoverable: %+v err=%v", latest, errLatest)
	}
	for _, version := range []int{18, 19} {
		if models, ok := databaseSnapshotModels(version); !ok || len(models) != len(homeDatabaseModels) {
			t.Fatalf("v%d snapshot compatibility was lost", version)
		}
	}
}

func TestGuardianSessionDatabaseSelectionOrder(t *testing.T) {
	testGuardianSessionDatabaseSelectionOrder(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresGuardianSessionDatabaseSelectionOrder(t *testing.T) {
	testGuardianSessionDatabaseSelectionOrder(t, newPostgresQuiescenceRepository(t))
}

func testGuardianSessionDatabaseSelectionOrder(t *testing.T, repo *Repository) {
	t.Helper()
	other := NewRepository(repo.db)
	sessionKey := strings.Repeat("f", 64)
	commit := func(key, provider, auth string) coreauth.SessionAffinityCommit {
		return coreauth.SessionAffinityCommit{Key: strings.Repeat(key, 64), SessionKey: sessionKey, Provider: provider, AuthID: auth, TTL: time.Hour}
	}
	old := commit("a", "codex", "old-review")
	main := commit("b", "codex", "main")
	foreign := commit("c", "openai", "other-provider")
	for _, selection := range []coreauth.SessionAffinityCommit{old, main, foreign} {
		if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), selection); errSwap != nil || !swapped {
			t.Fatalf("commit=%t err=%v", swapped, errSwap)
		}
	}
	assertLatest := func(providers []string, want string) {
		t.Helper()
		latest, errLatest := other.LoadLatestSessionBinding(t.Context(), sessionKey, providers)
		if errLatest != nil || latest.AuthID != want {
			t.Fatalf("providers=%v latest=%+v err=%v, want %s", providers, latest, errLatest, want)
		}
	}
	assertLatest([]string{"codex"}, "main")
	assertLatest([]string{"codex", "openai"}, "other-provider")
	assertLatest([]string{"claude"}, "")
	// Merely reading an older model does not reorder successful selections.
	if _, errLoad := other.LoadSessionBinding(t.Context(), old.Key); errLoad != nil {
		t.Fatal(errLoad)
	}
	assertLatest([]string{"codex"}, "main")
	// Stale CAS attempts and rolled-back transactions must not advance the order.
	if swapped, errSwap := other.CompareAndSwapSessionBinding(t.Context(), old); errSwap != nil || swapped {
		t.Fatalf("conflicting selection=%t err=%v", swapped, errSwap)
	}
	before, errBefore := repo.LoadSessionBinding(t.Context(), old.Key)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	old.ExpectedRevision = before.Revision
	failure := errors.New("admission rejected")
	errTransaction := withConcurrencyTransaction(t.Context(), repo.db, func(tx *gorm.DB) error {
		if swapped, errSwap := compareAndSwapSessionBindingTx(tx, old); errSwap != nil || !swapped {
			return errors.New("selection could not be committed inside test transaction")
		}
		return failure
	})
	if !errors.Is(errTransaction, failure) {
		t.Fatalf("rollback error=%v", errTransaction)
	}
	assertLatest([]string{"codex"}, "main")
	// Reusing a live binding is a real selection and makes that model latest.
	if swapped, errSwap := other.CompareAndSwapSessionBinding(t.Context(), old); errSwap != nil || !swapped {
		t.Fatalf("renewed selection=%t err=%v", swapped, errSwap)
	}
	assertLatest([]string{"codex"}, "old-review")
	if errExpire := repo.db.Model(&SessionAffinityRecord{}).Where("key = ?", old.Key).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	assertLatest([]string{"codex"}, "main")
	// A scoped lookup never extends parent TTL.
	after, errAfter := repo.LoadSessionBinding(t.Context(), main.Key)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	assertLatest([]string{"codex"}, "main")
	final, errFinal := repo.LoadSessionBinding(t.Context(), main.Key)
	if errFinal != nil || !after.ExpiresAt.Equal(final.ExpiresAt) || after.Revision != final.Revision {
		t.Fatalf("parent lookup renewed binding: before=%+v after=%+v err=%v", after, final, errFinal)
	}
	// Sequence cleanup is bounded and must retain live sessions.
	if _, errPurge := repo.PurgeExpiredSessionBindings(t.Context(), time.Now(), 100); errPurge != nil {
		t.Fatal(errPurge)
	}
	var sequences int64
	if errCount := repo.db.Model(&SessionAffinityRecord{}).Where("key = ?", sessionAffinitySequenceKey(sessionKey)).Count(&sequences).Error; errCount != nil || sequences != 1 {
		t.Fatalf("live sequence count=%d err=%v", sequences, errCount)
	}
	if _, errPurge := repo.PurgeExpiredSessionBindings(t.Context(), time.Now().Add(2*time.Hour), 100); errPurge != nil {
		t.Fatal(errPurge)
	}
	if errCount := repo.db.Model(&SessionAffinityRecord{}).Where("key = ?", sessionAffinitySequenceKey(sessionKey)).Count(&sequences).Error; errCount != nil || sequences != 0 {
		t.Fatalf("expired sequence count=%d err=%v", sequences, errCount)
	}
}

func TestGuardianSessionAdmissionAtomicOrder(t *testing.T) {
	testGuardianSessionAdmissionAtomicOrder(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresGuardianSessionAdmissionAtomicOrder(t *testing.T) {
	testGuardianSessionAdmissionAtomicOrder(t, newPostgresQuiescenceRepository(t))
}

func testGuardianSessionAdmissionAtomicOrder(t *testing.T, repo *Repository) {
	t.Helper()
	seedConcurrencyAdmissionLifetime(t, repo, "guardian-fp", "guardian-home")
	seedConcurrencyAdmissionAuth(t, repo, "cred-a")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-a", int64Pointer(1), nil)
	key := strings.Repeat("a", 64)
	sessionKey := strings.Repeat("d", 64)
	request := ConcurrencyAdmissionRequest{CredentialID: "cred-a", Model: "main", Lifetime: concurrencyAdmissionLifetime(t, repo, "guardian-fp", "guardian-home"), ProtocolVersion: 1,
		SessionAffinity: &coreauth.SessionAffinityCommit{Key: key, SessionKey: sessionKey, Provider: "codex", AuthID: "cred-a", TTL: time.Hour}}
	if result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); errAdmit != nil || !result.Accounted {
		t.Fatalf("admission=%+v err=%v", result, errAdmit)
	}
	var before SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", key).Take(&before).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if before.SelectionSequence != 1 {
		t.Fatalf("first sequence=%d", before.SelectionSequence)
	}
	request.SessionAffinity.ExpectedRevision = before.Revision
	if _, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); !IsConcurrencySaturated(errAdmit) {
		t.Fatalf("saturated admission=%v", errAdmit)
	}
	var after SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", key).Take(&after).Error; errFind != nil || before.SelectionSequence != after.SelectionSequence {
		t.Fatalf("rejected admission reordered binding: before=%+v after=%+v err=%v", before, after, errFind)
	}
	request.SessionAffinity.Key = strings.Repeat("b", 64)
	request.SessionAffinity.ExpectedRevision = ""
	if _, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); !IsConcurrencySaturated(errAdmit) {
		t.Fatalf("saturated new model=%v", errAdmit)
	}
	latest, errLatest := repo.LoadLatestSessionBinding(t.Context(), sessionKey, []string{"codex"})
	if errLatest != nil || latest.AuthID != "cred-a" || latest.Revision != before.Revision {
		t.Fatalf("rejected model published: %+v err=%v", latest, errLatest)
	}
	var counter SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", sessionAffinitySequenceKey(sessionKey)).Take(&counter).Error; errFind != nil || counter.SelectionSequence != 1 {
		t.Fatalf("rejected admission consumed order: %+v err=%v", counter, errFind)
	}
}

func TestGuardianSessionRuntimeAcrossHomesAndReload(t *testing.T) {
	testGuardianSessionRuntimeAcrossHomesAndReload(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresGuardianSessionRuntimeAcrossHomesAndReload(t *testing.T) {
	testGuardianSessionRuntimeAcrossHomesAndReload(t, newPostgresQuiescenceRepository(t))
}

func testGuardianSessionRuntimeAcrossHomesAndReload(t *testing.T, repo *Repository) {
	t.Helper()
	ids := []string{"33333333-3333-4333-8333-000000000001", "33333333-3333-4333-8333-000000000002"}
	for _, id := range ids {
		auth := &coreauth.Auth{ID: id, Index: id, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "fixture-key"}, Metadata: map[string]any{"home_config_models": []map[string]any{
			{"id": "guardian-main", "name": "guardian-main", "user_defined": true}, {"id": "guardian-review", "name": "guardian-review", "user_defined": true},
		}}}
		if _, errUpsert := repo.UpsertAuth(t.Context(), auth, "upsert"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	nodeA := newSessionAffinityRuntime(t, repo, "guardian-a")
	nodeB := newSessionAffinityRuntime(t, NewRepository(repo.db), "guardian-b")
	dispatch := func(node *home.Runtime, session, parent, model, pinned string) string {
		t.Helper()
		headers := http.Header{"Session_id": {session}, "X-Codex-Parent-Thread-Id": {parent}, "X-Openai-Subagent": {"guardian"}}
		result, errDispatch := node.DispatchForAPIKeyWithConcurrency(t.Context(), model, headers, "", "", nil, pinned, home.DispatchConcurrencyContext{})
		if errDispatch != nil || result == nil {
			t.Fatalf("session=%s model=%s dispatch=%v err=%v", session, model, result, errDispatch)
		}
		return result.AuthID
	}
	// Establish an older same-model binding first, then a newer parent-model selection.
	dispatch(nodeB, "parent", "", "guardian-review", ids[0])
	dispatch(nodeA, "parent", "", "guardian-main", ids[1])
	if got := dispatch(nodeB, "child", "parent", "guardian-review", ""); got != ids[1] {
		t.Fatalf("cross-Home inheritance=%s, want %s", got, ids[1])
	}
	// A fresh runtime has no node-local history but must still inherit the parent.
	restarted := newSessionAffinityRuntime(t, repo, "guardian-restarted")
	if got := dispatch(restarted, "another-child", "parent", "guardian-review", ""); got != ids[1] {
		t.Fatalf("restart inheritance=%s", got)
	}
	cfg := *nodeB.Config()
	disabled := false
	cfg.Routing.Strategy = "fill-first"
	cfg.Routing.SessionAffinitySubagents = &disabled
	if errApply := nodeB.ApplyConfigFromCluster(t.Context(), &cfg); errApply != nil {
		t.Fatal(errApply)
	}
	if got := dispatch(nodeB, "disabled-child", "parent", "guardian-review", ""); got != ids[0] {
		t.Fatalf("disabled guardian inherited=%s", got)
	}
	if got := dispatch(nodeB, "child", "parent", "guardian-review", ""); got != ids[1] {
		t.Fatalf("config reload discarded child binding=%s", got)
	}
}

func TestPostgresGuardianSessionConcurrentSelectionOrder(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const count = 16
	var wait sync.WaitGroup
	start := make(chan struct{})
	errorsCh := make(chan error, count)
	for index := range count {
		wait.Go(func() {
			<-start
			commit := coreauth.SessionAffinityCommit{Key: fmt.Sprintf("%064x", index), SessionKey: strings.Repeat("f", 64), Provider: "codex", AuthID: fmt.Sprintf("cred-%d", index), TTL: time.Hour}
			if swapped, errSwap := NewRepository(repo.db).CompareAndSwapSessionBinding(ctx, commit); errSwap != nil {
				errorsCh <- errSwap
			} else if !swapped {
				errorsCh <- errors.New("concurrent selection conflicted on distinct models")
			}
		})
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for errSelection := range errorsCh {
		t.Error(errSelection)
	}
	var records []SessionAffinityRecord
	if errFind := repo.db.Where("session_key = ?", strings.Repeat("f", 64)).Order("selection_sequence").Find(&records).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(records) != count {
		t.Fatalf("successful selections=%d", len(records))
	}
	for index, record := range records {
		if record.SelectionSequence != int64(index+1) {
			t.Fatalf("selection order not unique: %+v", records)
		}
	}
	latest, errLatest := repo.LoadLatestSessionBinding(ctx, strings.Repeat("f", 64), []string{"codex"})
	if errLatest != nil || latest.AuthID != records[count-1].CredentialID {
		t.Fatalf("latest concurrent binding=%+v err=%v", latest, errLatest)
	}
}
