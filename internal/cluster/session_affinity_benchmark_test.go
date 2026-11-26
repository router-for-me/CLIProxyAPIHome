package cluster

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These benchmarks exercise only the affinity store, not upstream execution or
// the rest of dispatch admission. Each sub-benchmark uses an isolated schema.
func newSessionAffinityBenchmarkRepository(b *testing.B) *Repository {
	b.Helper()
	dsn := strings.TrimSpace(os.Getenv("HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	}
	if dsn == "" {
		b.Skip("HOME_TEST_POSTGRES_DSN is not configured")
	}
	admin, errOpen := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if errOpen != nil {
		b.Fatal(errOpen)
	}
	adminSQL, errDB := admin.DB()
	if errDB != nil {
		b.Fatal(errDB)
	}
	b.Cleanup(func() {
		if errClose := adminSQL.Close(); errClose != nil {
			b.Error(errClose)
		}
	})
	schema := "session_affinity_benchmark_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if errCreate := admin.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		b.Fatal(errCreate)
	}
	b.Cleanup(func() {
		if errDrop := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			b.Error(errDrop)
		}
	})
	db, errSchema := gorm.Open(postgres.Open(postgresDSNWithSearchPath(dsn, schema)), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errSchema != nil {
		b.Fatal(errSchema)
	}
	db = db.Session(&gorm.Session{Logger: logger.Discard})
	sqlDB, errSQL := db.DB()
	if errSQL != nil {
		b.Fatal(errSQL)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	b.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			b.Error(errClose)
		}
	})
	if errMigrate := db.AutoMigrate(&SessionAffinityRecord{}); errMigrate != nil {
		b.Fatal(errMigrate)
	}
	return NewRepository(db)
}

func BenchmarkPostgresSessionAffinityDispatch(b *testing.B) {
	previousLevel := log.GetLevel()
	log.SetLevel(log.ErrorLevel)
	b.Cleanup(func() { log.SetLevel(previousLevel) })
	for _, enabled := range []bool{true, false} {
		name := "history_on"
		if !enabled {
			name = "history_off"
		}
		for _, mode := range []string{"serial", "parallel_same_session", "parallel_sessions"} {
			b.Run(name+"/"+mode, func(b *testing.B) {
				repo := newSessionAffinityBenchmarkRepository(b)
				selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
					Store: repo, Fallback: &coreauth.FillFirstSelector{}, SubagentAffinity: &enabled,
				})
				auths := []*coreauth.Auth{{ID: "benchmark-auth", Provider: "codex", Status: coreauth.StatusActive}}
				opts := coreauth.Options{Headers: http.Header{"Session-Id": {"benchmark-session"}}}
				if _, errPick := selector.Pick(b.Context(), "codex", "model", opts, auths); errPick != nil {
					b.Fatal(errPick)
				}
				b.ReportAllocs()
				b.ResetTimer()
				if mode == "serial" {
					for b.Loop() {
						if _, errPick := selector.Pick(b.Context(), "codex", "model", opts, auths); errPick != nil {
							b.Fatal(errPick)
						}
					}
				} else {
					var workerID atomic.Int64
					b.RunParallel(func(pb *testing.PB) {
						workerOpts := opts
						if mode == "parallel_sessions" {
							workerOpts.Headers = http.Header{"Session-Id": {"worker-" + strconv.FormatInt(workerID.Add(1), 10)}}
						}
						for pb.Next() {
							if _, errPick := selector.Pick(b.Context(), "codex", "model", workerOpts, auths); errPick != nil {
								b.Error(errPick)
								return
							}
						}
					})
				}
				b.StopTimer()
			})
		}
	}
}

func BenchmarkPostgresSessionAffinityParentLookup(b *testing.B) {
	repo := newSessionAffinityBenchmarkRepository(b)
	sessionKey := strings.Repeat("a", 64)
	commit := coreauth.SessionAffinityCommit{Key: strings.Repeat("b", 64), SessionKey: sessionKey, Provider: "codex", AuthID: "parent", TTL: time.Hour}
	if swapped, errSwap := repo.CompareAndSwapSessionBinding(b.Context(), commit); errSwap != nil || !swapped {
		b.Fatalf("seed=%t err=%v", swapped, errSwap)
	}
	providers := []string{"codex"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if binding, errLoad := repo.LoadLatestSessionBinding(b.Context(), sessionKey, providers); errLoad != nil || binding.AuthID != "parent" {
			b.Fatalf("parent=%+v err=%v", binding, errLoad)
		}
	}
	b.StopTimer()
}
