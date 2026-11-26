package cluster

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// sessionAffinitySQLTrace counts executed data statements, excluding BEGIN/COMMIT.
// Each test uses it on a private GORM session without concurrent operations.
type sessionAffinitySQLTrace struct {
	logger.Interface
	statements []string
}

func (trace *sessionAffinitySQLTrace) Trace(_ context.Context, _ time.Time, sql func() (string, int64), _ error) {
	statement, _ := sql()
	trace.statements = append(trace.statements, statement)
}

func TestSessionAffinitySQLBudget(t *testing.T) {
	testSessionAffinitySQLBudget(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresSessionAffinitySQLBudget(t *testing.T) {
	testSessionAffinitySQLBudget(t, newPostgresQuiescenceRepository(t))
}

func testSessionAffinitySQLBudget(t *testing.T, repo *Repository) {
	t.Helper()
	trace := &sessionAffinitySQLTrace{Interface: logger.Discard}
	repo = NewRepository(repo.db.Session(&gorm.Session{Logger: trace}))
	postgres := repo.db.Dialector.Name() == "postgres"
	trackedColdSQL, trackedWarmSQL, ordinarySQL := 4, 4, 2
	if postgres {
		trackedColdSQL, trackedWarmSQL, ordinarySQL = 2, 1, 1
	}
	assertBudget := func(operation string, want int) {
		t.Helper()
		if got := len(trace.statements); got != want {
			t.Fatalf("%s used %d statements, want %d: %v", operation, got, want, trace.statements)
		}
		t.Logf("%s: %d SQL statements", operation, want)
	}
	commit := coreauth.SessionAffinityCommit{Key: strings.Repeat("a", 64), AuthID: "parent", TTL: time.Hour,
		SessionKey: strings.Repeat("f", 64), Provider: "codex"}
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("cold commit=%t err=%v", swapped, errSwap)
	}
	assertBudget("tracked cold commit", trackedColdSQL)
	if postgres && !strings.HasPrefix(trace.statements[0], "INSERT INTO session_affinity ") {
		t.Fatalf("cold commit did not claim binding before sequence: %v", trace.statements)
	}
	binding, errLoad := repo.LoadSessionBinding(t.Context(), commit.Key)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	commit.ExpectedRevision = binding.Revision
	trace.statements = nil
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("renew=%t err=%v", swapped, errSwap)
	}
	assertBudget("tracked warm commit", trackedWarmSQL)
	if postgres && !strings.Contains(trace.statements[0], "FOR UPDATE") {
		t.Fatalf("renewal did not lock binding before sequence: %v", trace.statements)
	}
	trace.statements = nil
	latest, errLatest := repo.LoadLatestSessionBinding(t.Context(), commit.SessionKey, []string{"codex"})
	if errLatest != nil || latest.AuthID != "parent" || latest.Revision != binding.Revision {
		t.Fatalf("latest=%+v err=%v", latest, errLatest)
	}
	assertBudget("Guardian parent lookup", 1)

	conflict := commit
	conflict.ExpectedRevision = "stale-revision"
	trace.statements = nil
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), conflict); errSwap != nil || swapped {
		t.Fatalf("conflict=%t err=%v", swapped, errSwap)
	}
	assertBudget("stale tracked commit", ordinarySQL)

	// Turning history off removes the touched binding from the parent index,
	// but neither discards the sticky binding nor updates the shared sequence.
	ordinary := commit
	ordinary.SessionKey, ordinary.Provider = "", ""
	trace.statements = nil
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), ordinary); errSwap != nil || !swapped {
		t.Fatalf("ordinary renewal=%t err=%v", swapped, errSwap)
	}
	assertBudget("ordinary warm commit", ordinarySQL)
	var record SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", commit.Key).Take(&record).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if record.SessionKey != "" || record.Provider != "" || record.SelectionSequence != 0 || record.CredentialID != "parent" || record.Revision != binding.Revision {
		t.Fatalf("ordinary renewal did not preserve binding and clear history: %+v", record)
	}
	var sequence SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", sessionAffinitySequenceKey(commit.SessionKey)).Take(&sequence).Error; errFind != nil || sequence.SelectionSequence != 2 {
		t.Fatalf("ordinary renewal advanced history: %+v err=%v", sequence, errFind)
	}
	latest, errLatest = repo.LoadLatestSessionBinding(t.Context(), commit.SessionKey, []string{"codex"})
	if errLatest != nil || latest.AuthID != "" {
		t.Fatalf("disabled history still discoverable: %+v err=%v", latest, errLatest)
	}
	trace.statements = nil
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("re-enabled history=%t err=%v", swapped, errSwap)
	}
	assertBudget("re-enabled history commit", trackedWarmSQL)
	if errFind := repo.db.Where("key = ?", sessionAffinitySequenceKey(commit.SessionKey)).Take(&sequence).Error; errFind != nil || sequence.SelectionSequence != 3 {
		t.Fatalf("re-enabled selection order=%+v err=%v", sequence, errFind)
	}
	ordinary.Key, ordinary.ExpectedRevision = strings.Repeat("b", 64), ""
	trace.statements = nil
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), ordinary); errSwap != nil || !swapped {
		t.Fatalf("ordinary cold commit=%t err=%v", swapped, errSwap)
	}
	assertBudget("ordinary cold commit", ordinarySQL)
}

func TestPostgresSessionAffinityStatementRollback(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	commit := coreauth.SessionAffinityCommit{Key: strings.Repeat("a", 64), AuthID: "original", TTL: time.Hour,
		SessionKey: strings.Repeat("f", 64), Provider: "codex"}
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
		t.Fatalf("seed=%t err=%v", swapped, errSwap)
	}
	original, errLoad := repo.LoadSessionBinding(t.Context(), commit.Key)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if errFunction := repo.db.Exec(`CREATE FUNCTION reject_session_binding_update() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'test binding update rejection'; END;
		$$ LANGUAGE plpgsql`).Error; errFunction != nil {
		t.Fatal(errFunction)
	}
	if errTrigger := repo.db.Exec(`CREATE TRIGGER reject_session_binding_update BEFORE UPDATE ON session_affinity
		FOR EACH ROW WHEN (NEW.credential_id <> '') EXECUTE FUNCTION reject_session_binding_update()`).Error; errTrigger != nil {
		t.Fatal(errTrigger)
	}
	commit.ExpectedRevision, commit.AuthID = original.Revision, "rejected"
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap == nil || swapped {
		t.Fatalf("failing statement=%t err=%v", swapped, errSwap)
	}
	binding, errBinding := repo.LoadSessionBinding(t.Context(), commit.Key)
	if errBinding != nil || binding != original {
		t.Fatalf("failed statement changed binding: %+v err=%v", binding, errBinding)
	}
	var sequence SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", sessionAffinitySequenceKey(commit.SessionKey)).Take(&sequence).Error; errFind != nil || sequence.SelectionSequence != 1 {
		t.Fatalf("failed statement changed sequence: %+v err=%v", sequence, errFind)
	}
}

func TestPostgresSessionAffinityConcurrentWarmOrder(t *testing.T) {
	for _, sameBinding := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_binding=%t", sameBinding), func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			const count = 16
			commits := make([]coreauth.SessionAffinityCommit, count)
			for index := range count {
				key := strings.Repeat("a", 64)
				if !sameBinding {
					key = fmt.Sprintf("%064x", index)
				}
				commit := coreauth.SessionAffinityCommit{Key: key, AuthID: "parent", TTL: time.Hour,
					SessionKey: strings.Repeat("f", 64), Provider: "codex"}
				if index == 0 || !sameBinding {
					if swapped, errSwap := repo.CompareAndSwapSessionBinding(ctx, commit); errSwap != nil || !swapped {
						t.Fatalf("seed=%t err=%v", swapped, errSwap)
					}
				}
				binding, errLoad := repo.LoadSessionBinding(ctx, key)
				if errLoad != nil {
					t.Fatal(errLoad)
				}
				commit.ExpectedRevision = binding.Revision
				commits[index] = commit
			}
			start := make(chan struct{})
			errorsCh := make(chan error, count)
			var wait sync.WaitGroup
			for _, commit := range commits {
				wait.Go(func() {
					<-start
					swapped, errSwap := NewRepository(repo.db).CompareAndSwapSessionBinding(ctx, commit)
					if errSwap != nil || !swapped {
						errorsCh <- fmt.Errorf("concurrent renewal=%t err=%v", swapped, errSwap)
					}
				})
			}
			close(start)
			wait.Wait()
			close(errorsCh)
			for errRenew := range errorsCh {
				t.Error(errRenew)
			}
			var records []SessionAffinityRecord
			if errFind := repo.db.Where("credential_id <> ''").Order("selection_sequence").Find(&records).Error; errFind != nil {
				t.Fatal(errFind)
			}
			wantRecords := count
			if sameBinding {
				wantRecords = 1
			}
			if len(records) != wantRecords {
				t.Fatalf("bindings=%d want=%d", len(records), wantRecords)
			}
			for index, record := range records {
				if record.SelectionSequence != int64(count+index+1) {
					t.Fatalf("concurrent renewal order: %+v", records)
				}
			}
			var sequence SessionAffinityRecord
			if errFind := repo.db.Where("key = ?", sessionAffinitySequenceKey(commits[0].SessionKey)).Take(&sequence).Error; errFind != nil || sequence.SelectionSequence != int64(count+wantRecords) {
				t.Fatalf("sequence=%+v err=%v", sequence, errFind)
			}
			for _, record := range records {
				if sequence.ExpiresAt.Before(record.ExpiresAt) {
					t.Fatalf("sequence expires before live binding: sequence=%+v binding=%+v", sequence, record)
				}
			}
		})
	}
}

func TestSessionAffinityInternalCounter(t *testing.T) {
	testSessionAffinityInternalCounter(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresSessionAffinityInternalCounter(t *testing.T) {
	testSessionAffinityInternalCounter(t, newPostgresQuiescenceRepository(t))
}

func testSessionAffinityInternalCounter(t *testing.T, repo *Repository) {
	t.Helper()
	sessionKey := strings.Repeat("f", 64)
	counterKey := sessionAffinitySequenceKey(sessionKey)
	if !strings.HasPrefix(counterKey, "seq:") || len(counterKey) > 64 || len(counterKey) == 64 || counterKey == sessionAffinitySequenceKey(strings.Repeat("e", 64)) {
		t.Fatalf("counter namespace is not separate from binding keys: %q", counterKey)
	}
	long := coreauth.SessionAffinityCommit{Key: strings.Repeat("a", 64), SessionKey: sessionKey, Provider: "codex", AuthID: "long", TTL: 3 * time.Hour}
	short := long
	short.Key, short.AuthID, short.TTL = strings.Repeat("b", 64), "short", time.Hour
	for _, commit := range []coreauth.SessionAffinityCommit{long, short} {
		if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), commit); errSwap != nil || !swapped {
			t.Fatalf("seed=%t err=%v", swapped, errSwap)
		}
	}
	var counter, longBinding, shortBinding SessionAffinityRecord
	for key, target := range map[string]*SessionAffinityRecord{counterKey: &counter, long.Key: &longBinding, short.Key: &shortBinding} {
		if errFind := repo.db.Where("key = ?", key).Take(target).Error; errFind != nil {
			t.Fatal(errFind)
		}
	}
	if counter.CredentialID != "" || counter.Revision != "" || counter.SessionKey != "" || counter.Provider != "" || counter.SelectionSequence != 2 || counter.ExpiresAt.Before(longBinding.ExpiresAt) {
		t.Fatalf("invalid internal counter or shortened lease: %+v", counter)
	}
	if binding, errLoad := repo.LoadSessionBinding(t.Context(), counterKey); errLoad != nil || binding != (coreauth.SessionBinding{}) {
		t.Fatalf("internal counter exposed as binding: %+v err=%v", binding, errLoad)
	}
	if latest, errLatest := repo.LoadLatestSessionBinding(t.Context(), "", []string{""}); errLatest != nil || latest.AuthID != "" {
		t.Fatalf("internal counter exposed as parent: %+v err=%v", latest, errLatest)
	}
	// Simulate time advancing past the shorter binding, but not the longer one.
	cutoff := shortBinding.ExpiresAt.Add(time.Hour)
	if purged, errPurge := repo.PurgeExpiredSessionBindings(t.Context(), cutoff, 1); errPurge != nil || purged != 1 {
		t.Fatalf("short binding cleanup=%d err=%v", purged, errPurge)
	}
	if errFind := repo.db.Where("key = ?", counterKey).Take(&counter).Error; errFind != nil || counter.SelectionSequence != 2 {
		t.Fatalf("live counter removed: %+v err=%v", counter, errFind)
	}
	short.Key = strings.Repeat("c", 64)
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(t.Context(), short); errSwap != nil || !swapped {
		t.Fatalf("next model=%t err=%v", swapped, errSwap)
	}
	var next SessionAffinityRecord
	if errFind := repo.db.Where("key = ?", short.Key).Take(&next).Error; errFind != nil || next.SelectionSequence != 3 {
		t.Fatalf("cleanup reset live selection order: %+v err=%v", next, errFind)
	}
	// Each call deletes at most one binding and one counter; counters are not
	// included in the returned binding count and may expire with the last binding.
	cutoff = longBinding.ExpiresAt.Add(time.Hour)
	for _, want := range []int64{1, 1, 0} {
		if purged, errPurge := repo.PurgeExpiredSessionBindings(t.Context(), cutoff, 1); errPurge != nil || purged != want {
			t.Fatalf("final cleanup=%d want=%d err=%v", purged, want, errPurge)
		}
	}
	var remaining int64
	if errCount := repo.db.Model(&SessionAffinityRecord{}).Count(&remaining).Error; errCount != nil || remaining != 0 {
		t.Fatalf("internal state leaked: rows=%d err=%v", remaining, errCount)
	}
}

func TestSessionAffinityParentLookupTimezone(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	sessionKey := strings.Repeat("f", 64)
	now := time.Now().UTC()
	// SQLite stores text timestamps. A future local wall-clock representation
	// must not make an already-expired instant eligible for Guardian inheritance.
	records := []SessionAffinityRecord{
		{Key: strings.Repeat("a", 64), CredentialID: "live", Revision: "a", SessionKey: sessionKey, Provider: "codex", SelectionSequence: 1, ExpiresAt: now.Add(time.Hour)},
		{Key: strings.Repeat("b", 64), CredentialID: "expired", Revision: "b", SessionKey: sessionKey, Provider: "codex", SelectionSequence: 2, ExpiresAt: now.Add(-time.Hour).In(time.FixedZone("offset", 8*60*60))},
	}
	if errCreate := repo.db.Create(&records).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	latest, errLatest := repo.LoadLatestSessionBinding(t.Context(), sessionKey, []string{"codex"})
	if errLatest != nil || latest.AuthID != "live" {
		t.Fatalf("timezone-dependent parent lookup=%+v err=%v", latest, errLatest)
	}
}
