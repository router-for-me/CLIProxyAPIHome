package cluster

import (
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This opt-in measurement must use a disposable local PostgreSQL database.
func TestConcurrencyAdmissionMeasure(t *testing.T) {
	if os.Getenv("HOME_CONCURRENCY_MEASURE") != "1" {
		t.Skip("set HOME_CONCURRENCY_MEASURE=1 with an isolated local PostgreSQL database")
	}
	dsn := os.Getenv("HOME_TEST_POSTGRES_DSN")
	cfg, errConfig := pgx.ParseConfig(dsn)
	if errConfig != nil || cfg == nil {
		t.Fatalf("parse measurement DSN: %v", errConfig)
	}
	if (cfg.Host != "127.0.0.1" && cfg.Host != "::1") || cfg.Database != "home_concurrency_bench" {
		t.Fatal("measurement requires a loopback host and disposable database home_concurrency_bench; do not use a production tunnel")
	}
	const requests = 600
	const repeats = 5
	for _, scenario := range []struct {
		name      string
		workers   int
		different bool
	}{
		{"serial", 1, false},
		{"same_account", 4, false},
		{"different_accounts", 4, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			accounts := 1
			if scenario.different {
				accounts = scenario.workers
			}
			for account := range accounts {
				id := fmt.Sprintf("measure-%d", account)
				seedConcurrencyAdmissionAuth(t, repo, id)
				seedConcurrencyAdmissionPolicy(t, repo, id, int64Pointer(math.MaxInt64), map[string]int64{"gpt": math.MaxInt64})
			}
			counter := &lifecycleSQLCounter{Interface: logger.Default.LogMode(logger.Silent)}
			repo.db = repo.db.Session(&gorm.Session{Logger: counter})
			for round := -1; round < repeats; round++ {
				perWorker := requests / scenario.workers
				if round == -1 {
					perWorker = 20
				}
				latencies := make([][]time.Duration, scenario.workers)
				errors := make([]error, scenario.workers)
				ready := make(chan struct{})
				var waiting, done sync.WaitGroup
				waiting.Add(scenario.workers)
				done.Add(scenario.workers)
				for worker := range scenario.workers {
					go func() {
						defer done.Done()
						account := 0
						if scenario.different {
							account = worker
						}
						request := ConcurrencyAdmissionRequest{CredentialID: fmt.Sprintf("measure-%d", account), Model: "gpt", ProtocolVersion: 1, Lifetime: lifetime}
						latencies[worker] = make([]time.Duration, 0, perWorker)
						waiting.Done()
						<-ready
						for range perWorker {
							start := time.Now()
							result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request)
							latencies[worker] = append(latencies[worker], time.Since(start))
							if errAdmit != nil || !result.Accounted {
								errors[worker] = fmt.Errorf("admission accounted=%v: %w", result.Accounted, errAdmit)
								return
							}
						}
					}()
				}
				waiting.Wait()
				counter.statements.Store(0)
				start := time.Now()
				close(ready)
				done.Wait()
				elapsed := time.Since(start)
				for _, errWorker := range errors {
					if errWorker != nil {
						t.Fatal(errWorker)
					}
				}
				if round == -1 {
					continue
				}
				all := make([]time.Duration, 0, requests)
				for _, durations := range latencies {
					all = append(all, durations...)
				}
				slices.Sort(all)
				t.Logf("MEASURE scenario=%s round=%d requests=%d workers=%d sql=%d sql_per_op=%.1f elapsed_ms=%.3f ops_per_sec=%.2f p50_ms=%.3f p95_ms=%.3f p99_ms=%.3f",
					scenario.name, round+1, len(all), scenario.workers, counter.statements.Load(), float64(counter.statements.Load())/float64(len(all)),
					float64(elapsed)/float64(time.Millisecond), float64(len(all))/elapsed.Seconds(),
					float64(all[(len(all)*50+99)/100-1])/float64(time.Millisecond),
					float64(all[(len(all)*95+99)/100-1])/float64(time.Millisecond),
					float64(all[(len(all)*99+99)/100-1])/float64(time.Millisecond))
			}
			want := int64(repeats*requests + 20*scenario.workers)
			if got := countConcurrencyAdmissionCounters(t, repo); got != want {
				t.Fatalf("counter total=%d, want %d", got, want)
			}
		})
	}
}
