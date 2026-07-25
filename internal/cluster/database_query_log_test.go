package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDatabaseSQLIDIsStableAndStructural(t *testing.T) {
	firstSQL := "\r\n SELECT\t*  FROM auth WHERE uuid = ? \n"
	secondSQL := "SELECT * FROM auth WHERE uuid = ?"
	firstID := databaseSQLID(firstSQL)
	secondID := databaseSQLID(secondSQL)
	if firstID != secondID {
		t.Fatalf("whitespace-only SQL change produced IDs %q and %q", firstID, secondID)
	}
	wantHash := sha256.Sum256([]byte(databaseSQLIDNamespace + secondSQL))
	wantID := hex.EncodeToString(wantHash[:])[:16]
	if firstID != wantID {
		t.Fatalf("databaseSQLID() = %q, want %q", firstID, wantID)
	}
	if got := databaseSQLID("SELECT * FROM auth JOIN quota_snapshot ON quota_snapshot.credential_id = auth.uuid WHERE uuid = ?"); got == firstID {
		t.Fatal("adding a JOIN did not change the SQL ID")
	}
	if got := databaseSQLID("SELECT * FROM auth WHERE uuid = ? ORDER BY id DESC"); got == firstID {
		t.Fatal("changing ORDER BY did not change the SQL ID")
	}
	if len(firstID) != 16 {
		t.Fatalf("SQL ID length = %d, want 16", len(firstID))
	}
}

func TestCanonicalDatabaseSQLPreservesQuotedContent(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "quoted content",
			sql:  "\r\n SELECT\t'A  B', \"C  D\", `E  F`, [G  H], $tag$I  J\nK$tag$  FROM\titems ",
			want: "SELECT 'A  B', \"C  D\", `E  F`, [G  H], $tag$I  J\nK$tag$ FROM items",
		},
		{
			name: "escaped delimiters",
			sql:  "SELECT  'it''s  fine', \"A\"\"  B\", `A``  B`, [A]]  B]",
			want: "SELECT 'it''s  fine', \"A\"\"  B\", `A``  B`, [A]]  B]",
		},
		{
			name: "postgres parameters",
			sql:  "SELECT  $1  +  $2",
			want: "SELECT $1 + $2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canonicalDatabaseSQL(test.sql); got != test.want {
				t.Fatalf("canonicalDatabaseSQL() = %q, want %q", got, test.want)
			}
		})
	}

	if databaseSQLID("SELECT 'A  B'") == databaseSQLID("SELECT 'A B'") {
		t.Fatal("different string constants produced the same SQL ID")
	}
	if databaseSQLID(`SELECT "A  B"`) == databaseSQLID(`SELECT "A B"`) {
		t.Fatal("different quoted identifiers produced the same SQL ID")
	}
	if databaseSQLID("SELECT $tag$A  B$tag$") == databaseSQLID("SELECT $tag$A B$tag$") {
		t.Fatal("different dollar-quoted strings produced the same SQL ID")
	}
	if databaseSQLID("SELECT 'UPPER'") == databaseSQLID("select 'UPPER'") {
		t.Fatal("different SQL case produced the same SQL ID")
	}
}

func TestSingleLineDatabaseSQLPreservesQuotedWhitespace(t *testing.T) {
	sql := "SELECT 'line one\r\nline\t two'"
	if got, want := singleLineDatabaseSQL(sql), `SELECT 'line one\nline\t two'`; got != want {
		t.Fatalf("singleLineDatabaseSQL() = %q, want %q", got, want)
	}
}

func TestDatabaseSQLIDSurvivesCatalogRestart(t *testing.T) {
	sql := "SELECT * FROM auth WHERE uuid = ?"
	firstCatalog := newDatabaseQueryLog(8, time.Minute)
	secondCatalog := newDatabaseQueryLog(8, time.Minute)
	firstID := databaseSQLID(sql)
	secondID := databaseSQLID(sql)
	firstCatalog.register(firstID, "auth.credential.get", sql, nil)
	secondCatalog.register(secondID, "auth.credential.get", sql, nil)
	if firstID != secondID {
		t.Fatalf("SQL IDs across simulated restart = %q and %q", firstID, secondID)
	}
	if _, ok := firstCatalog.lookup(firstID); !ok {
		t.Fatal("first catalog did not register SQL")
	}
	if _, ok := secondCatalog.lookup(secondID); !ok {
		t.Fatal("second catalog did not register SQL")
	}
}

func TestDatabaseQueryNameContext(t *testing.T) {
	if got := databaseQueryName(context.Background()); got != databaseQueryUnclassified {
		t.Fatalf("unnamed query = %q, want %q", got, databaseQueryUnclassified)
	}
	ctx := WithDatabaseQueryName(context.Background(), " usage.aggregate.items ")
	if got := databaseQueryName(ctx); got != "usage.aggregate.items" {
		t.Fatalf("named query = %q, want usage.aggregate.items", got)
	}
}

func TestDatabaseQueryLogRegistersDefinitionOnceConcurrently(t *testing.T) {
	catalog := newDatabaseQueryLog(8, time.Minute)
	sql := "SELECT * FROM auth WHERE uuid = ?"
	sqlID := databaseSQLID(sql)
	var definitions atomic.Int64
	var waitGroup sync.WaitGroup
	for range 100 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			catalog.register(sqlID, "auth.credential.get", sql, func(databaseQueryDefinition) {
				definitions.Add(1)
			})
		}()
	}
	waitGroup.Wait()
	if got := definitions.Load(); got != 1 {
		t.Fatalf("SQL definitions = %d, want 1", got)
	}
	entry, ok := catalog.lookup(sqlID)
	if !ok {
		t.Fatal("concurrent registration lost catalog entry")
	}
	if entry.Query != "auth.credential.get" || entry.SQL != sql {
		t.Fatalf("catalog entry = %#v", entry.databaseQueryDefinition)
	}
}

func TestDatabaseQueryLogEvictsLeastRecentlyUsedDefinition(t *testing.T) {
	catalog := newDatabaseQueryLog(2, time.Minute)
	firstSQL := "SELECT 1"
	secondSQL := "SELECT 2"
	thirdSQL := "SELECT 3"
	firstID := databaseSQLID(firstSQL)
	secondID := databaseSQLID(secondSQL)
	thirdID := databaseSQLID(thirdSQL)
	catalog.register(firstID, "first", firstSQL, nil)
	catalog.register(secondID, "second", secondSQL, nil)
	catalog.register(firstID, "first", firstSQL, nil)
	catalog.register(thirdID, "third", thirdSQL, nil)
	if _, ok := catalog.lookup(firstID); !ok {
		t.Fatal("recently used definition was evicted")
	}
	if _, ok := catalog.lookup(secondID); ok {
		t.Fatal("least recently used definition was retained")
	}
	if _, ok := catalog.lookup(thirdID); !ok {
		t.Fatal("new definition was not registered")
	}
	var redefinitions int
	catalog.register(secondID, "second", secondSQL, func(databaseQueryDefinition) {
		redefinitions++
	})
	if redefinitions != 1 {
		t.Fatalf("evicted SQL redefinitions = %d, want 1", redefinitions)
	}
}

func TestDatabaseQueryLogAggregatesSuppressedSlowQueries(t *testing.T) {
	now := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	catalog := newDatabaseQueryLog(8, time.Minute)
	catalog.now = func() time.Time { return now }
	sql := "SELECT * FROM usage WHERE timestamp >= ?"
	sqlID := databaseSQLID(sql)
	catalog.register(sqlID, "usage.aggregate.items", sql, nil)
	if decision := catalog.observeSlow(sqlID, 250*time.Millisecond, 1); !decision.Log || decision.Suppressed != 0 {
		t.Fatalf("first slow decision = %#v", decision)
	}

	for index, elapsed := range []time.Duration{300 * time.Millisecond, 500 * time.Millisecond, 400 * time.Millisecond} {
		now = now.Add(10 * time.Second)
		if decision := catalog.observeSlow(sqlID, elapsed, int64(index+2)); decision.Log {
			t.Fatalf("suppressed slow decision %d = %#v", index, decision)
		}
	}
	entry, ok := catalog.lookup(sqlID)
	if !ok || entry.latestSuppressedRows != 4 {
		t.Fatalf("latest suppressed rows = %d, found = %t", entry.latestSuppressedRows, ok)
	}
	now = now.Add(31 * time.Second)
	decision := catalog.observeSlow(sqlID, 600*time.Millisecond, 5)
	if !decision.Log || decision.Suppressed != 3 || decision.Average != 400*time.Millisecond || decision.Maximum != 500*time.Millisecond {
		t.Fatalf("aggregated slow decision = %#v", decision)
	}
}
