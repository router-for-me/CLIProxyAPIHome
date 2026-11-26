package cluster

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newCoordinationPoolFixture opens a capped main pool and, when coordinationConns is positive, a
// separate coordination pool on one fresh schema.
func newCoordinationPoolFixture(t testing.TB, mainConns int, coordinationConns int) (*gorm.DB, *Coordinator) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	}
	if dsn == "" {
		t.Skip("HOME_TEST_POSTGRES_DSN is not configured")
	}
	adminDB, errAdmin := gorm.Open(postgres.Open(dsn), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errAdmin != nil {
		t.Fatalf("open postgres admin database: %v", errAdmin)
	}
	adminSQLDB, errAdminSQL := adminDB.DB()
	if errAdminSQL != nil {
		t.Fatal(errAdminSQL)
	}
	schema := "coordination_pool_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := adminDB.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		t.Fatalf("create postgres schema: %v", errCreate)
	}
	cfg := PGSQLConfig{
		PostgresURI:              postgresDSNWithSearchPath(dsn, schema),
		MaxOpenConns:             mainConns,
		CoordinationMaxOpenConns: coordinationConns,
	}
	mainDB, errOpen := Open(context.Background(), cfg)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	coordinationDB, errCoordination := OpenCoordination(context.Background(), cfg)
	if errCoordination != nil {
		t.Fatal(errCoordination)
	}
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{mainDB, coordinationDB} {
			if db == nil {
				continue
			}
			if sqlDB, errDB := db.DB(); errDB == nil {
				if errClose := sqlDB.Close(); errClose != nil {
					t.Errorf("close postgres database: %v", errClose)
				}
			}
		}
		if errDrop := adminDB.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres schema: %v", errDrop)
		}
		if errClose := adminSQLDB.Close(); errClose != nil {
			t.Errorf("close postgres admin database: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(mainDB); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	options := CoordinatorOptions{HeartbeatInterval: time.Second, HeartbeatTimeout: 20 * time.Second}
	if coordinationDB != nil {
		options.CoordinationRepository = NewRepository(coordinationDB)
	}
	coordinator := NewCoordinator(NewRepository(mainDB), NodeIdentity{IP: "127.0.0.1", Port: 18317, StartedAt: time.Now()}, options)
	if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	return mainDB, coordinator
}

// holdMainPool keeps every main-pool connection busy until the returned release is called.
func holdMainPool(t testing.TB, db *gorm.DB, conns int) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	started := make(chan struct{}, conns)
	for range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
				started <- struct{}{}
				<-ctx.Done()
				return nil
			})
		}()
	}
	for range conns {
		<-started
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

func heartbeatOnce(ctx context.Context, coordinator *Coordinator) error {
	home, errHome := coordinator.heartbeatHomeIncarnation(ctx)
	if errHome != nil {
		return errHome
	}
	return coordinator.heartbeatAndElect(ctx, home)
}

func TestCoordinationPoolKeepsHeartbeatsOffASaturatedMainPool(t *testing.T) {
	mainDB, coordinator := newCoordinationPoolFixture(t, 2, 1)
	release := holdMainPool(t, mainDB, 2)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if errHeartbeat := heartbeatOnce(ctx, coordinator); errHeartbeat != nil {
		t.Fatalf("heartbeat with a separate coordination pool: %v", errHeartbeat)
	}
	if !coordinator.IsMaster() {
		t.Fatal("the only live Home was not elected")
	}
	if _, errMaster := coordinator.CurrentMaster(ctx); errMaster != nil {
		t.Fatalf("master lookup with a separate coordination pool: %v", errMaster)
	}
}

func TestCoordinationPoolServesNodeLookupsOffASaturatedMainPool(t *testing.T) {
	for _, coordinationConns := range []int{1, 0} {
		mainDB, coordinator := newCoordinationPoolFixture(t, 2, coordinationConns)
		if errHeartbeat := heartbeatOnce(context.Background(), coordinator); errHeartbeat != nil {
			t.Fatal(errHeartbeat)
		}
		handler := NewRESPHandler(coordinator, nil, NewRepository(mainDB))
		release := holdMainPool(t, mainDB, 2)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, errNodes := handler.nodesPayload(ctx)
		cancel()
		release()
		if coordinationConns > 0 && errNodes != nil {
			t.Fatalf("node lookup with a coordination pool: %v", errNodes)
		}
		if coordinationConns == 0 && !errors.Is(errNodes, context.DeadlineExceeded) {
			t.Fatalf("node lookup on the shared saturated pool = %v, want the caller deadline", errNodes)
		}
	}
}

func TestSharedPoolHeartbeatWaitsBehindASaturatedMainPool(t *testing.T) {
	mainDB, coordinator := newCoordinationPoolFixture(t, 2, 0)
	release := holdMainPool(t, mainDB, 2)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if errHeartbeat := heartbeatOnce(ctx, coordinator); !errors.Is(errHeartbeat, context.DeadlineExceeded) {
		t.Fatalf("heartbeat on the shared saturated pool = %v, want the caller deadline", errHeartbeat)
	}
}

func TestOpenCoordinationIsOptionalAndSeparatelyCapped(t *testing.T) {
	if db, errOpen := OpenCoordination(context.Background(), PGSQLConfig{PostgresURI: "postgres://unused.invalid/db"}); db != nil || errOpen != nil {
		t.Fatalf("OpenCoordination without a limit = %v, %v; want no pool", db, errOpen)
	}
	if errValidate := (PGSQLConfig{PostgresURI: "postgres://unused.invalid/db", CoordinationMaxOpenConns: -1}).Validate(); errValidate == nil {
		t.Fatal("a negative coordination-max-open-conns was accepted")
	}
	mainDB, coordinator := newCoordinationPoolFixture(t, 2, 1)
	coordinationSQLDB, errSQL := coordinator.coordination.db.DB()
	if errSQL != nil {
		t.Fatal(errSQL)
	}
	mainSQLDB, errMainSQL := mainDB.DB()
	if errMainSQL != nil {
		t.Fatal(errMainSQL)
	}
	if coordinationSQLDB == mainSQLDB || coordinationSQLDB.Stats().MaxOpenConnections != 1 {
		t.Fatalf("coordination pool max open = %d, want a separate pool of 1", coordinationSQLDB.Stats().MaxOpenConnections)
	}
}

// This opt-in measurement must use a disposable local PostgreSQL database. Run it with network
// delay injected between Home and PostgreSQL to see the effect at real round-trip times.
func TestCoordinationPoolMeasure(t *testing.T) {
	if os.Getenv("HOME_COORDINATION_POOL_MEASURE") != "1" {
		t.Skip("set HOME_COORDINATION_POOL_MEASURE=1 with an isolated local PostgreSQL database")
	}
	const (
		mainConns  = 6
		statements = 40
		heartbeats = 30
	)
	for _, scenario := range []struct {
		name              string
		workers           int
		coordinationConns int
	}{
		{"idle, shared pool", 0, 0},
		{"18 busy requests, shared pool", 18, 0},
		{"18 busy requests, coordination pool of 2", 18, 2},
	} {
		mainDB, coordinator := newCoordinationPoolFixture(t, mainConns, scenario.coordinationConns)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for range scenario.workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					// One request-shaped burst: statements sent one at a time.
					for range statements {
						var now time.Time
						if errNow := mainDB.WithContext(ctx).Raw("SELECT clock_timestamp()").Scan(&now).Error; errNow != nil {
							return
						}
					}
				}
			}()
		}
		time.Sleep(time.Second)
		var latencies []time.Duration
		for range heartbeats {
			heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), 20*time.Second)
			startedAt := time.Now()
			if errHeartbeat := heartbeatOnce(heartbeatCtx, coordinator); errHeartbeat != nil {
				t.Fatalf("%s: heartbeat: %v", scenario.name, errHeartbeat)
			}
			latencies = append(latencies, time.Since(startedAt))
			heartbeatCancel()
		}
		cancel()
		wg.Wait()
		slices.Sort(latencies)
		t.Logf("%s: heartbeat+election p50=%v p95=%v max=%v",
			scenario.name, latencies[len(latencies)/2].Round(time.Millisecond), latencies[len(latencies)*95/100].Round(time.Millisecond), latencies[len(latencies)-1].Round(time.Millisecond))
	}
}
