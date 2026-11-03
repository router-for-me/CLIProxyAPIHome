package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type heavyStatisticsMeasurement struct {
	mu         sync.Mutex
	heartbeats []time.Time
	admissions []time.Duration
	queueTimes []time.Duration
	writes     int
	running    atomic.Int64
	peak       atomic.Int64
}

// TestHeavyStatisticsPostgresMeasure is an opt-in, disposable-database comparison of B alone.
// A stays enabled in both variants. The fixed queries use the same gate and Repository calls
// as the HTTP handlers, isolating queue time from HTTP parsing and serialization overhead.
func TestHeavyStatisticsPostgresMeasure(t *testing.T) {
	if os.Getenv("HOME_STATISTICS_MEASURE") != "1" {
		t.Skip("set HOME_STATISTICS_MEASURE=1 with a disposable local PostgreSQL database")
	}
	gin.SetMode(gin.TestMode)
	dsn, errParse := url.Parse(os.Getenv("HOME_TEST_POSTGRES_DSN"))
	if errParse != nil || dsn == nil || dsn.Hostname() != "127.0.0.1" || dsn.Path != "/home_statistics_test" {
		t.Fatal("measurement requires a loopback PostgreSQL URL for disposable home_statistics_test; never use a production tunnel")
	}
	admin, errOpen := gorm.Open(postgres.Open(dsn.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	adminSQL, errAdminDB := admin.DB()
	if errAdminDB != nil {
		t.Fatal(errAdminDB)
	}
	t.Cleanup(func() {
		if errClose := adminSQL.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	schema := "statistics_measure_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := admin.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	t.Cleanup(func() {
		if errDrop := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Error(errDrop)
		}
	})
	params := dsn.Query()
	params.Set("search_path", schema)
	dsn.RawQuery = params.Encode()
	db, errSchemaOpen := gorm.Open(postgres.Open(dsn.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if errSchemaOpen != nil {
		t.Fatal(errSchemaOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "statistics-home", 8327)
	coordinator := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "statistics-home", Port: 8327}, cluster.CoordinatorOptions{
		HeartbeatInterval: 50 * time.Millisecond,
		HeartbeatTimeout:  20 * time.Second,
	})
	if errInitialize := coordinator.Initialize(t.Context()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	home, _ := coordinator.HomeIncarnation()
	member := cluster.CPANodeMembershipRecord{
		CertificateFingerprint: "statistics-measure", NodeID: "statistics-measure", HomeIP: home.IP, HomePort: home.Port,
		HomeStartedAt: home.StartedAt, ProtocolVersion: 1, State: cluster.MembershipStateActive,
		ConnectedAt: home.StartedAt, LastSeenAt: home.StartedAt, CPAHeartbeatTimeout: time.Hour,
	}
	limit := int64(10000)
	for _, record := range []any{
		&member,
		&cluster.AuthRecord{UUID: "statistics-credential", ID: "statistics-credential", Index: "statistics-credential", AuthJSON: cluster.JSONB(`{}`), Version: 1},
		&cluster.CredentialConcurrencyPolicyRecord{CredentialID: "statistics-credential", MaxInFlight: &limit, Version: 1, EffectiveAt: home.StartedAt},
	} {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	registry := node.GlobalRegistry()
	registry.AddWithNodeID("192.0.2.230", member.NodeID, home.StartedAt)
	t.Cleanup(func() { registry.RemoveWithNodeID("192.0.2.230", member.NodeID) })
	from := time.Date(2026, 6, 10, 1, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Minute)
	if errSeed := db.Exec(`INSERT INTO "usage" ("timestamp", provider, model, auth_index, input_tokens, output_tokens, total_tokens, latency_ms, payload, created_at)
		SELECT ?::timestamptz + (n % 600) * interval '1 second', 'openai', 'model-' || (n % 10), 'statistics-credential', 100, 50, 150, n % 1000, '{}'::jsonb, ?::timestamptz
		FROM generate_series(1, 20000) AS series(n)`, from, from).Error; errSeed != nil {
		t.Fatal(errSeed)
	}
	if errSeed := db.Exec(`INSERT INTO billing_charge (id, usage_id, payload_hash, provider, model, input_tokens, output_tokens, amount, price_snapshot, created_at)
		SELECT 'charge-' || id, id, 'hash-' || id, provider, model, input_tokens, output_tokens, 0.001, '{}'::jsonb, "timestamp" FROM "usage"`).Error; errSeed != nil {
		t.Fatal(errSeed)
	}
	if errAnalyze := db.Exec("ANALYZE").Error; errAnalyze != nil {
		t.Fatal(errAnalyze)
	}
	var current atomic.Pointer[heavyStatisticsMeasurement]
	if errRegister := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:measure-heartbeat", func(tx *gorm.DB) {
		measure := current.Load()
		if measure == nil || tx.Error != nil || tx.Statement.Table != (cluster.HomeProcessIncarnationRecord{}).TableName() {
			return
		}
		values, ok := tx.Statement.Dest.(map[string]any)
		if !ok || values["last_seen_at"] == nil {
			return
		}
		measure.mu.Lock()
		measure.heartbeats = append(measure.heartbeats, time.Now())
		measure.mu.Unlock()
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := db.Callback().Delete().After("gorm:delete").Register("test:measure-snapshots", func(tx *gorm.DB) {
		if measure := current.Load(); measure != nil && tx.Error == nil && tx.Statement.Table == (cluster.CPANodeRecord{}).TableName() {
			measure.mu.Lock()
			measure.writes++
			measure.mu.Unlock()
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	lifetime := cluster.ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Controlled: true, Home: home}
	var releaseSequence int64
	for _, workers := range []int{0, 8} {
		for round := 1; round <= 3; round++ {
			// Alternate order to reduce systematic warm-cache bias.
			variants := []bool{false, true}
			if round%2 == 0 {
				slices.Reverse(variants)
			}
			for _, limited := range variants {
				t.Run(fmt.Sprintf("workers=%d/round=%d/limited=%t", workers, round, limited), func(t *testing.T) {
					measure := &heavyStatisticsMeasurement{}
					current.Store(measure)
					defer current.Store(nil)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					heartbeatDone := make(chan error, 1)
					go func() { heartbeatDone <- coordinator.Start(ctx) }()
					stop := make(chan struct{})
					var done sync.WaitGroup
					errors := make(chan error, workers+1)
					done.Add(workers + 1)
					go func() {
						defer done.Done()
						ticker := time.NewTicker(10 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-stop:
								return
							case <-ticker.C:
							}
							startedAt := time.Now()
							result, errAdmit := repo.AdmitCredentialConcurrency(ctx, cluster.ConcurrencyAdmissionRequest{CredentialID: "statistics-credential", Model: "model-0", Lifetime: lifetime, ProtocolVersion: 1})
							elapsed := time.Since(startedAt)
							if errAdmit != nil || !result.Accounted {
								errors <- fmt.Errorf("admission accounted=%t: %w", result.Accounted, errAdmit)
								return
							}
							measure.mu.Lock()
							measure.admissions = append(measure.admissions, elapsed)
							measure.mu.Unlock()
							releaseSequence++
							if errRelease := repo.ApplyConcurrencyRelease(ctx, cluster.ConcurrencyReleaseRequest{CredentialID: "statistics-credential", Model: "model-0", ReleaseSeq: releaseSequence, Lifetime: lifetime}); errRelease != nil {
								errors <- errRelease
								return
							}
						}
					}()
					for worker := range workers {
						go func() {
							defer done.Done()
							c, _ := gin.CreateTestContext(httptest.NewRecorder())
							c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/v8/management"+heavyStatisticsRoutes[worker%3].path, nil)
							for {
								select {
								case <-stop:
									return
								default:
								}
								queryCtx, cancelQuery := handler.requestContext(c)
								startedAt := time.Now()
								release := func() {}
								if limited {
									var errAcquire error
									release, errAcquire = handler.acquireHeavyStatisticsSlot(queryCtx, c)
									if errAcquire != nil {
										cancelQuery()
										errors <- errAcquire
										return
									}
								}
								queueTime := time.Since(startedAt)
								active := measure.running.Add(1)
								for peak := measure.peak.Load(); active > peak && !measure.peak.CompareAndSwap(peak, active); peak = measure.peak.Load() {
								}
								var errQuery error
								switch worker % 3 {
								case 0:
									_, errQuery = repo.UsageObservabilityOverview(queryCtx, cluster.UsageObservabilityOverviewQuery{From: &from, To: &to, Timezone: "UTC", Interval: "minute"})
								case 1:
									_, errQuery = repo.ListUsageObservabilityAggregates(queryCtx, cluster.UsageObservabilityAggregateQuery{From: &from, To: &to, GroupBy: "model", Metric: "request_count", Direction: "desc"})
								case 2:
									_, errQuery = repo.BillingOverview(queryCtx, cluster.BillingOverviewQuery{From: &from, ToExclusive: &to, Timezone: "UTC"})
								}
								measure.running.Add(-1)
								release()
								cancelQuery()
								if errQuery != nil {
									errors <- errQuery
									return
								}
								measure.mu.Lock()
								measure.queueTimes = append(measure.queueTimes, queueTime)
								measure.mu.Unlock()
							}
						}()
					}
					<-time.After(3 * time.Second)
					// Count heartbeat/snapshot activity only in the common load window, not drain time.
					current.Store(nil)
					close(stop)
					done.Wait()
					cancel()
					if errStart := <-heartbeatDone; errStart != nil {
						t.Fatal(errStart)
					}
					close(errors)
					for errWorker := range errors {
						t.Error(errWorker)
					}
					measure.mu.Lock()
					defer measure.mu.Unlock()
					intervals := make([]time.Duration, 0, len(measure.heartbeats))
					for index := 1; index < len(measure.heartbeats); index++ {
						intervals = append(intervals, measure.heartbeats[index].Sub(measure.heartbeats[index-1]))
					}
					if len(intervals) == 0 || len(measure.admissions) == 0 || (limited && measure.peak.Load() > 2) {
						t.Fatalf("invalid measurement: heartbeats=%d admissions=%d peak=%d", len(intervals), len(measure.admissions), measure.peak.Load())
					}
					var counter cluster.CredentialConcurrencyCounterRecord
					if errFind := db.First(&counter, "credential_id = ?", "statistics-credential").Error; errFind != nil {
						t.Fatal(errFind)
					}
					if counter.ActiveCount != 0 || counter.LastReleaseSeq != releaseSequence {
						t.Fatalf("counter regressed: %#v", counter)
					}
					t.Logf("MEASURE workers=%d round=%d limited=%t admission_n=%d admission_p95_ms=%.3f admission_p99_ms=%.3f heartbeat_p95_ms=%.3f heartbeat_max_ms=%.3f snapshot_writes=%d statistics_n=%d statistics_peak=%d queue_p95_ms=%.3f queue_p99_ms=%.3f",
						workers, round, limited, len(measure.admissions), statisticsMeasurePercentile(measure.admissions, 95), statisticsMeasurePercentile(measure.admissions, 99), statisticsMeasurePercentile(intervals, 95), statisticsMeasurePercentile(intervals, 100), measure.writes, len(measure.queueTimes), measure.peak.Load(), statisticsMeasurePercentile(measure.queueTimes, 95), statisticsMeasurePercentile(measure.queueTimes, 99))
				})
			}
		}
	}
}

func statisticsMeasurePercentile(values []time.Duration, percentile int) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return float64(sorted[(len(sorted)*percentile+99)/100-1]) / float64(time.Millisecond)
}
