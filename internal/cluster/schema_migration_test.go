package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type fakeSchemaMigrationStore struct {
	lock              chan struct{}
	mu                sync.Mutex
	state             schemaMigrationState
	tryLockCalls      int
	migrationCalls    int
	failuresRemaining int
	migrationStarted  chan int
	lockMissed        chan struct{}
	releaseMigration  <-chan struct{}
}

func newFakeSchemaMigrationStore(state schemaMigrationState) *fakeSchemaMigrationStore {
	return &fakeSchemaMigrationStore{
		lock:  make(chan struct{}, 1),
		state: state,
	}
}

func (f *fakeSchemaMigrationStore) operations() schemaMigrationOperations {
	return schemaMigrationOperations{
		currentVersion: func(context.Context) (schemaMigrationState, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.state, nil
		},
		tryLock: func(ctx context.Context, migrate func(lockedSchemaMigrationOperations) error) (bool, error) {
			f.mu.Lock()
			f.tryLockCalls++
			f.mu.Unlock()
			select {
			case f.lock <- struct{}{}:
				defer func() { <-f.lock }()
				return true, migrate(lockedSchemaMigrationOperations{
					currentVersion: func(context.Context) (schemaMigrationState, error) {
						f.mu.Lock()
						defer f.mu.Unlock()
						return f.state, nil
					},
					migrateToVersion: func(ctx context.Context, _ int64, requiredVersion int64) error {
						f.mu.Lock()
						f.migrationCalls++
						call := f.migrationCalls
						shouldFail := f.failuresRemaining > 0
						if shouldFail {
							f.failuresRemaining--
						}
						started := f.migrationStarted
						release := f.releaseMigration
						f.mu.Unlock()

						if started != nil {
							select {
							case started <- call:
							default:
							}
						}
						if release != nil {
							select {
							case <-ctx.Done():
								return ctx.Err()
							case <-release:
							}
						}
						if shouldFail {
							return errors.New("forced migration failure")
						}
						f.mu.Lock()
						f.state = schemaMigrationState{Version: requiredVersion, Initialized: true}
						f.mu.Unlock()
						return nil
					},
				})
			default:
				if f.lockMissed != nil {
					select {
					case f.lockMissed <- struct{}{}:
					default:
					}
				}
				return false, nil
			}
		},
	}
}

func (f *fakeSchemaMigrationStore) snapshot() (schemaMigrationState, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.tryLockCalls, f.migrationCalls
}

func TestCoordinateSchemaMigrationSkipsSatisfiedVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		currentVersion int64
	}{
		{name: "current version", currentVersion: currentDatabaseVersion},
		{name: "higher version", currentVersion: currentDatabaseVersion + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeSchemaMigrationStore(schemaMigrationState{Version: test.currentVersion, Initialized: true})
			result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
			if errCoordinate != nil {
				t.Fatalf("coordinateSchemaMigration() error = %v", errCoordinate)
			}
			state, lockCalls, migrationCalls := store.snapshot()
			if lockCalls != 0 || migrationCalls != 0 || result.LockAcquired || result.MigrationExecutor || result.MigrationCommitted {
				t.Fatalf("satisfied migration result = %+v lock_calls=%d migration_calls=%d", result, lockCalls, migrationCalls)
			}
			if state.Version != test.currentVersion || result.CurrentState.Version != test.currentVersion {
				t.Fatalf("schema version changed: state=%d result=%d want=%d", state.Version, result.CurrentState.Version, test.currentVersion)
			}
		})
	}
}

func TestCoordinateSchemaMigrationBootstrapsUnversionedDatabase(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"fresh database", "legacy database without version record"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newFakeSchemaMigrationStore(schemaMigrationState{})
			result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
			if errCoordinate != nil {
				t.Fatalf("coordinateSchemaMigration() error = %v", errCoordinate)
			}
			state, lockCalls, migrationCalls := store.snapshot()
			if state.Version != currentDatabaseVersion || !state.Initialized {
				t.Fatalf("schema state = %+v, want initialized version %d", state, currentDatabaseVersion)
			}
			if lockCalls != 1 || migrationCalls != 1 || !result.LockAcquired || !result.MigrationExecutor || !result.MigrationCommitted {
				t.Fatalf("bootstrap migration result = %+v lock_calls=%d migration_calls=%d", result, lockCalls, migrationCalls)
			}
		})
	}
}

func TestCoordinateSchemaMigrationUpgradesLowerRecordedVersion(t *testing.T) {
	t.Parallel()

	const previousVersion int64 = 1
	store := newFakeSchemaMigrationStore(schemaMigrationState{Version: previousVersion, Initialized: true})
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
	if errCoordinate != nil {
		t.Fatalf("coordinateSchemaMigration() error = %v", errCoordinate)
	}
	state, lockCalls, migrationCalls := store.snapshot()
	if state.Version != currentDatabaseVersion || !state.Initialized {
		t.Fatalf("schema state = %+v, want initialized version %d", state, currentDatabaseVersion)
	}
	if result.InitialState.Version != previousVersion || lockCalls != 1 || migrationCalls != 1 || !result.LockAcquired || !result.MigrationExecutor || !result.MigrationCommitted {
		t.Fatalf("upgrade migration result = %+v lock_calls=%d migration_calls=%d", result, lockCalls, migrationCalls)
	}
}

func TestCoordinateSchemaMigrationConcurrentStartersExecuteOnce(t *testing.T) {
	t.Parallel()

	const starters = 8
	started := make(chan int, 1)
	missed := make(chan struct{}, starters)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseMigration := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseMigration()
	store := newFakeSchemaMigrationStore(schemaMigrationState{})
	store.migrationStarted = started
	store.lockMissed = missed
	store.releaseMigration = release

	start := make(chan struct{})
	results := make([]schemaMigrationResult, starters)
	errorsByStarter := make([]error, starters)
	var waitGroup sync.WaitGroup
	for index := 0; index < starters; index++ {
		waitGroup.Add(1)
		go func(starter int) {
			defer waitGroup.Done()
			<-start
			results[starter], errorsByStarter[starter] = coordinateSchemaMigration(context.Background(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
		}(index)
	}
	close(start)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("migration executor did not start")
	}
	for index := 0; index < starters-1; index++ {
		select {
		case <-missed:
		case <-time.After(time.Second):
			t.Fatalf("only %d waiting starters observed the held migration lock", index)
		}
	}
	releaseMigration()
	waitGroup.Wait()

	executors := 0
	for index, errCoordinate := range errorsByStarter {
		if errCoordinate != nil {
			t.Fatalf("starter %d error = %v", index, errCoordinate)
		}
		if results[index].MigrationExecutor {
			executors++
		}
	}
	state, _, migrationCalls := store.snapshot()
	if executors != 1 || migrationCalls != 1 {
		t.Fatalf("executors=%d migration_calls=%d results=%+v", executors, migrationCalls, results)
	}
	if state.Version != currentDatabaseVersion || !state.Initialized {
		t.Fatalf("schema state = %+v, want version %d", state, currentDatabaseVersion)
	}
}

func TestCoordinateSchemaMigrationRechecksVersionAfterLock(t *testing.T) {
	t.Parallel()

	initialReads := 0
	migrationCalls := 0
	operations := schemaMigrationOperations{
		currentVersion: func(context.Context) (schemaMigrationState, error) {
			initialReads++
			return schemaMigrationState{}, nil
		},
		tryLock: func(_ context.Context, migrate func(lockedSchemaMigrationOperations) error) (bool, error) {
			return true, migrate(lockedSchemaMigrationOperations{
				currentVersion: func(context.Context) (schemaMigrationState, error) {
					return schemaMigrationState{Version: currentDatabaseVersion, Initialized: true}, nil
				},
				migrateToVersion: func(context.Context, int64, int64) error {
					migrationCalls++
					return nil
				},
			})
		},
	}
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, false, operations)
	if errCoordinate != nil {
		t.Fatalf("coordinateSchemaMigration() error = %v", errCoordinate)
	}
	if initialReads != 1 || migrationCalls != 0 {
		t.Fatalf("initial_reads=%d migration_calls=%d", initialReads, migrationCalls)
	}
	if !result.LockAcquired || result.MigrationExecutor || result.MigrationCommitted || result.CurrentState.Version != currentDatabaseVersion {
		t.Fatalf("double-check result = %+v", result)
	}
}

func TestCoordinateSchemaMigrationRunsCurrentVersionWhenRequested(t *testing.T) {
	t.Parallel()

	store := newFakeSchemaMigrationStore(schemaMigrationState{Version: currentDatabaseVersion, Initialized: true})
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, true, store.operations())
	if errCoordinate != nil {
		t.Fatalf("coordinateSchemaMigration() error = %v", errCoordinate)
	}
	state, lockCalls, migrationCalls := store.snapshot()
	if lockCalls != 1 || migrationCalls != 1 || !result.LockAcquired || !result.MigrationExecutor || !result.MigrationCommitted {
		t.Fatalf("forced migration result = %+v lock_calls=%d migration_calls=%d", result, lockCalls, migrationCalls)
	}
	if state.Version != currentDatabaseVersion || result.CurrentState.Version != currentDatabaseVersion {
		t.Fatalf("forced migration version changed: state=%d result=%d", state.Version, result.CurrentState.Version)
	}
}

func TestCoordinateSchemaMigrationRejectsForcedHigherVersion(t *testing.T) {
	t.Parallel()

	higherVersion := int64(currentDatabaseVersion + 1)
	store := newFakeSchemaMigrationStore(schemaMigrationState{Version: higherVersion, Initialized: true})
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, true, store.operations())
	if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), "newer than required version") {
		t.Fatalf("coordinateSchemaMigration() error = %v, want newer-version rejection", errCoordinate)
	}
	state, lockCalls, migrationCalls := store.snapshot()
	if state.Version != higherVersion || lockCalls != 0 || migrationCalls != 0 {
		t.Fatalf("higher-version state=%+v lock_calls=%d migration_calls=%d", state, lockCalls, migrationCalls)
	}
	if result.LockAcquired || result.MigrationExecutor || result.MigrationCommitted || result.CurrentState.Version != higherVersion {
		t.Fatalf("higher-version migration result = %+v", result)
	}
}

func TestCoordinateSchemaMigrationRejectsHigherVersionAfterLock(t *testing.T) {
	t.Parallel()

	higherVersion := int64(currentDatabaseVersion + 1)
	lockCalls := 0
	migrationCalls := 0
	operations := schemaMigrationOperations{
		currentVersion: func(context.Context) (schemaMigrationState, error) {
			return schemaMigrationState{}, nil
		},
		tryLock: func(_ context.Context, migrate func(lockedSchemaMigrationOperations) error) (bool, error) {
			lockCalls++
			return true, migrate(lockedSchemaMigrationOperations{
				currentVersion: func(context.Context) (schemaMigrationState, error) {
					return schemaMigrationState{Version: higherVersion, Initialized: true}, nil
				},
				migrateToVersion: func(context.Context, int64, int64) error {
					migrationCalls++
					return nil
				},
			})
		},
	}
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, true, operations)
	if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), "newer than required version") {
		t.Fatalf("coordinateSchemaMigration() error = %v, want locked newer-version rejection", errCoordinate)
	}
	if lockCalls != 1 || migrationCalls != 0 {
		t.Fatalf("lock_calls=%d migration_calls=%d", lockCalls, migrationCalls)
	}
	if !result.LockAcquired || result.MigrationExecutor || result.MigrationCommitted || result.CurrentState.Version != higherVersion {
		t.Fatalf("locked higher-version migration result = %+v", result)
	}
}

func TestCoordinateSchemaMigrationExecutorIsNotLimitedByWaitTimeout(t *testing.T) {
	t.Parallel()

	const migrationDuration = 75 * time.Millisecond
	operations := schemaMigrationOperations{
		currentVersion: func(context.Context) (schemaMigrationState, error) {
			return schemaMigrationState{}, nil
		},
		tryLock: func(_ context.Context, migrate func(lockedSchemaMigrationOperations) error) (bool, error) {
			return true, migrate(lockedSchemaMigrationOperations{
				currentVersion: func(context.Context) (schemaMigrationState, error) {
					return schemaMigrationState{}, nil
				},
				migrateToVersion: func(migrationCtx context.Context, _ int64, _ int64) error {
					time.Sleep(migrationDuration)
					return migrationCtx.Err()
				},
			})
		},
	}
	startedAt := time.Now()
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, 5*time.Millisecond, false, operations)
	elapsed := time.Since(startedAt)
	if errCoordinate != nil {
		t.Fatalf("coordinateSchemaMigration() error = %v", errCoordinate)
	}
	if !result.MigrationExecutor || !result.MigrationCommitted {
		t.Fatalf("migration result = %+v", result)
	}
	if elapsed < migrationDuration {
		t.Fatalf("migration elapsed = %s, want at least %s", elapsed, migrationDuration)
	}
	if result.WaitDuration >= migrationDuration/2 {
		t.Fatalf("coordination wait = %s, includes migration duration %s", result.WaitDuration, migrationDuration)
	}
}

func TestCoordinateSchemaMigrationFailureDoesNotAdvanceVersion(t *testing.T) {
	t.Parallel()

	store := newFakeSchemaMigrationStore(schemaMigrationState{})
	store.failuresRemaining = 1
	result, errCoordinate := coordinateSchemaMigration(t.Context(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
	if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), "forced migration failure") {
		t.Fatalf("coordinateSchemaMigration() error = %v, want forced failure", errCoordinate)
	}
	state, _, migrationCalls := store.snapshot()
	if state.Version != 0 || state.Initialized || migrationCalls != 1 {
		t.Fatalf("failed migration state = %+v migration_calls=%d", state, migrationCalls)
	}
	if !result.LockAcquired || !result.MigrationExecutor || result.MigrationCommitted {
		t.Fatalf("failed migration result = %+v", result)
	}
}

func TestCoordinateSchemaMigrationWaiterTakesOverAfterFailure(t *testing.T) {
	t.Parallel()

	started := make(chan int, 2)
	missed := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseMigration := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseMigration()
	store := newFakeSchemaMigrationStore(schemaMigrationState{})
	store.failuresRemaining = 1
	store.migrationStarted = started
	store.lockMissed = missed
	store.releaseMigration = release

	firstResult := make(chan schemaMigrationResult, 1)
	firstError := make(chan error, 1)
	go func() {
		result, errCoordinate := coordinateSchemaMigration(context.Background(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
		firstResult <- result
		firstError <- errCoordinate
	}()
	select {
	case call := <-started:
		if call != 1 {
			t.Fatalf("first migration call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first migration executor did not start")
	}

	secondResult := make(chan schemaMigrationResult, 1)
	secondError := make(chan error, 1)
	go func() {
		result, errCoordinate := coordinateSchemaMigration(context.Background(), currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
		secondResult <- result
		secondError <- errCoordinate
	}()
	select {
	case <-missed:
	case <-time.After(time.Second):
		t.Fatal("waiting starter did not observe the held migration lock")
	}
	releaseMigration()

	resultFirst := <-firstResult
	errFirst := <-firstError
	resultSecond := <-secondResult
	errSecond := <-secondError
	if errFirst == nil || !strings.Contains(errFirst.Error(), "forced migration failure") {
		t.Fatalf("first migration error = %v", errFirst)
	}
	if errSecond != nil {
		t.Fatalf("takeover migration error = %v", errSecond)
	}
	if !resultFirst.MigrationExecutor || resultFirst.MigrationCommitted {
		t.Fatalf("first migration result = %+v", resultFirst)
	}
	if !resultSecond.MigrationExecutor || !resultSecond.MigrationCommitted {
		t.Fatalf("takeover migration result = %+v", resultSecond)
	}
	state, _, migrationCalls := store.snapshot()
	if state.Version != currentDatabaseVersion || migrationCalls != 2 {
		t.Fatalf("takeover state = %+v migration_calls=%d", state, migrationCalls)
	}
}

func TestCoordinateSchemaMigrationWaitTimeout(t *testing.T) {
	t.Parallel()

	store := newFakeSchemaMigrationStore(schemaMigrationState{})
	store.lock <- struct{}{}
	defer func() { <-store.lock }()
	result, errCoordinate := coordinateSchemaMigration(context.Background(), currentDatabaseVersion, 2*time.Millisecond, 25*time.Millisecond, false, store.operations())
	if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), "wait for database schema migration coordination") || !errors.Is(errCoordinate, context.DeadlineExceeded) {
		t.Fatalf("coordinateSchemaMigration() error = %v, want diagnostic deadline", errCoordinate)
	}
	if result.MigrationExecutor || result.MigrationCommitted || result.CurrentState.Version != 0 {
		t.Fatalf("timeout migration result = %+v", result)
	}
}

func TestCoordinateSchemaMigrationInitialReadHonorsWaitTimeout(t *testing.T) {
	t.Parallel()

	operations := schemaMigrationOperations{
		currentVersion: func(ctx context.Context) (schemaMigrationState, error) {
			<-ctx.Done()
			return schemaMigrationState{}, ctx.Err()
		},
		tryLock: func(context.Context, func(lockedSchemaMigrationOperations) error) (bool, error) {
			t.Fatal("tryLock() called after the initial version-read timeout")
			return false, nil
		},
	}
	result, errCoordinate := coordinateSchemaMigration(context.Background(), currentDatabaseVersion, time.Millisecond, 25*time.Millisecond, false, operations)
	if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), "while reading schema version") || !errors.Is(errCoordinate, context.DeadlineExceeded) {
		t.Fatalf("coordinateSchemaMigration() error = %v, want initial-read deadline", errCoordinate)
	}
	if result.LockAttempts != 0 || result.MigrationExecutor || result.MigrationCommitted {
		t.Fatalf("initial-read timeout result = %+v", result)
	}
}

func TestCoordinateSchemaMigrationWaitHonorsContext(t *testing.T) {
	t.Parallel()

	store := newFakeSchemaMigrationStore(schemaMigrationState{})
	store.lock <- struct{}{}
	defer func() { <-store.lock }()
	ctx, cancelCtx := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelCtx()
	result, errCoordinate := coordinateSchemaMigration(ctx, currentDatabaseVersion, 2*time.Millisecond, time.Second, false, store.operations())
	if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), "wait for database schema migration coordination") || !errors.Is(errCoordinate, context.DeadlineExceeded) {
		t.Fatalf("coordinateSchemaMigration() error = %v, want context deadline", errCoordinate)
	}
	if result.MigrationExecutor || result.MigrationCommitted || result.CurrentState.Version != 0 {
		t.Fatalf("context timeout migration result = %+v", result)
	}
}

func TestPostgresSchemaMigrationConnectionAcquisitionHonorsWaitTimeout(t *testing.T) {
	db, errOpen := OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("DB() error = %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close SQLite database: %v", errClose)
		}
	}()
	heldConnection, errConnection := sqlDB.Conn(t.Context())
	if errConnection != nil {
		t.Fatalf("reserve SQLite connection: %v", errConnection)
	}
	heldConnectionClosed := false
	closeHeldConnection := func() error {
		if heldConnectionClosed {
			return nil
		}
		heldConnectionClosed = true
		return heldConnection.Close()
	}
	defer func() {
		if errClose := closeHeldConnection(); errClose != nil {
			t.Errorf("release reserved SQLite connection: %v", errClose)
		}
	}()

	operations := postgresSchemaMigrationOperations(db, time.Now(), context.Background(), nil)
	operations.currentVersion = func(context.Context) (schemaMigrationState, error) {
		return schemaMigrationState{}, nil
	}
	type coordinateResult struct {
		result schemaMigrationResult
		err    error
	}
	resultCh := make(chan coordinateResult, 1)
	go func() {
		result, errCoordinate := coordinateSchemaMigration(context.Background(), currentDatabaseVersion, time.Millisecond, 25*time.Millisecond, false, operations)
		resultCh <- coordinateResult{result: result, err: errCoordinate}
	}()

	select {
	case coordinated := <-resultCh:
		if coordinated.err == nil || !strings.Contains(coordinated.err.Error(), "wait for database schema migration coordination") || !errors.Is(coordinated.err, context.DeadlineExceeded) {
			t.Fatalf("coordinateSchemaMigration() error = %v, want connection-acquisition deadline", coordinated.err)
		}
		if coordinated.result.LockAcquired || coordinated.result.MigrationExecutor || coordinated.result.MigrationCommitted {
			t.Fatalf("connection-acquisition timeout result = %+v", coordinated.result)
		}
	case <-time.After(time.Second):
		if errClose := closeHeldConnection(); errClose != nil {
			t.Errorf("release reserved SQLite connection after timeout: %v", errClose)
		}
		select {
		case <-resultCh:
		case <-time.After(time.Second):
		}
		t.Fatal("schema migration connection acquisition exceeded its wait timeout")
	}
}

func TestAutoMigrateSQLiteKeepsSingleNodeBehavior(t *testing.T) {
	t.Parallel()

	db, errOpen := OpenSQLite(t.Context(), t.TempDir()+"/home.db")
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("DB() error = %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite database: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrateContext(t.Context(), db); errMigrate != nil {
		t.Fatalf("AutoMigrateContext() error = %v", errMigrate)
	}
	if !db.Migrator().HasTable(&schemaMigrationRecord{}) || !db.Migrator().HasTable(&UsageRecord{}) {
		t.Fatal("SQLite migration did not create required tables")
	}
	record := ConfigRecord{Key: "schema-migration-sqlite-test", Value: JSONB(`{"value":true}`), Version: 1}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("create SQLite marker: %v", errCreate)
	}
	if errMigrate := AutoMigrateContext(t.Context(), db); errMigrate != nil {
		t.Fatalf("second AutoMigrateContext() error = %v", errMigrate)
	}
	var stored ConfigRecord
	if errFind := db.First(&stored, "key = ?", record.Key).Error; errFind != nil {
		t.Fatalf("find SQLite marker after repeat migration: %v", errFind)
	}
}

func TestAutoMigratePostgresSchemaVersionGate(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not configured")
	}

	adminDB, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errOpen != nil {
		t.Fatalf("open postgres admin database: %v", errOpen)
	}
	adminSQLDB, errAdminDB := adminDB.DB()
	if errAdminDB != nil {
		t.Fatalf("get postgres admin database: %v", errAdminDB)
	}
	schemaName := "schema_migration_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := adminDB.Exec("CREATE SCHEMA " + schemaName).Error; errCreate != nil {
		_ = adminSQLDB.Close()
		t.Fatalf("create postgres test schema: %v", errCreate)
	}
	db, errSchemaOpen := gorm.Open(postgres.Open(postgresDSNWithSearchPath(dsn, schemaName)), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errSchemaOpen != nil {
		_ = adminDB.Exec("DROP SCHEMA " + schemaName + " CASCADE").Error
		_ = adminSQLDB.Close()
		t.Fatalf("open postgres test schema: %v", errSchemaOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get postgres schema database: %v", errDB)
	}
	sqlDB.SetMaxOpenConns(12)
	sqlDB.SetMaxIdleConns(12)
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres schema database: %v", errClose)
		}
		if errDrop := adminDB.Exec("DROP SCHEMA " + schemaName + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres test schema: %v", errDrop)
		}
		if errClose := adminSQLDB.Close(); errClose != nil {
			t.Errorf("close postgres admin database: %v", errClose)
		}
	})

	const starters = 4
	ctx, cancelCtx := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCtx()
	start := make(chan struct{})
	errorsByStarter := make([]error, starters)
	var waitGroup sync.WaitGroup
	for index := 0; index < starters; index++ {
		waitGroup.Add(1)
		go func(starter int) {
			defer waitGroup.Done()
			<-start
			errorsByStarter[starter] = AutoMigrateContext(ctx, db)
		}(index)
	}
	close(start)
	waitGroup.Wait()
	for index, errMigrate := range errorsByStarter {
		if errMigrate != nil {
			t.Fatalf("fresh postgres starter %d error = %v", index, errMigrate)
		}
	}
	assertPostgresSchemaMigrationVersion(t, db, currentDatabaseVersion)

	releaseLock, lockDone := holdPostgresSchemaMigrationLock(t, db)
	fastCtx, cancelFast := context.WithTimeout(context.Background(), time.Second)
	errFast := AutoMigrateContext(fastCtx, db)
	cancelFast()
	releaseLock()
	if errLock := <-lockDone; errLock != nil {
		t.Fatalf("release postgres migration lock: %v", errLock)
	}
	if errFast != nil {
		t.Fatalf("current schema waited on advisory lock: %v", errFast)
	}

	if errDrop := db.Migrator().DropTable(&schemaMigrationRecord{}); errDrop != nil {
		t.Fatalf("drop version table for legacy upgrade: %v", errDrop)
	}
	if errMigrate := AutoMigrateContext(ctx, db); errMigrate != nil {
		t.Fatalf("migrate legacy postgres database without version record: %v", errMigrate)
	}
	assertPostgresSchemaMigrationVersion(t, db, currentDatabaseVersion)

	higherVersion := int64(currentDatabaseVersion + 1)
	if errUpdate := db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", higherVersion).Error; errUpdate != nil {
		t.Fatalf("set higher postgres schema version: %v", errUpdate)
	}
	if errMigrate := AutoMigrateContext(ctx, db); errMigrate != nil {
		t.Fatalf("check higher postgres schema version: %v", errMigrate)
	}
	assertPostgresSchemaMigrationVersion(t, db, higherVersion)

	if errUpdate := db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 0).Error; errUpdate != nil {
		t.Fatalf("reset postgres schema version for takeover: %v", errUpdate)
	}
	if errSequence := db.Exec(`CREATE SEQUENCE schema_migration_fail_once_seq START 1`).Error; errSequence != nil {
		t.Fatalf("create migration failure sequence: %v", errSequence)
	}
	if errFunction := db.Exec(`CREATE FUNCTION schema_migration_fail_once() RETURNS trigger AS $$
	DECLARE
		attempt BIGINT;
	BEGIN
		attempt := nextval('schema_migration_fail_once_seq');
		IF attempt = 1 THEN
			PERFORM pg_sleep(0.2);
			RAISE EXCEPTION 'forced schema migration failure';
		END IF;
		RETURN NEW;
	END;
	$$ LANGUAGE plpgsql`).Error; errFunction != nil {
		t.Fatalf("create migration failure function: %v", errFunction)
	}
	if errTrigger := db.Exec(`CREATE TRIGGER schema_migration_fail_once_trigger
		BEFORE INSERT OR UPDATE ON home_schema_migration
		FOR EACH ROW EXECUTE FUNCTION schema_migration_fail_once()`).Error; errTrigger != nil {
		t.Fatalf("create migration failure trigger: %v", errTrigger)
	}
	takeoverStart := make(chan struct{})
	takeoverErrors := make([]error, 2)
	for index := range takeoverErrors {
		waitGroup.Add(1)
		go func(starter int) {
			defer waitGroup.Done()
			<-takeoverStart
			takeoverErrors[starter] = AutoMigrateContext(ctx, db)
		}(index)
	}
	close(takeoverStart)
	waitGroup.Wait()
	failures := 0
	successes := 0
	for _, errMigrate := range takeoverErrors {
		switch {
		case errMigrate == nil:
			successes++
		case strings.Contains(errMigrate.Error(), "forced schema migration failure"):
			failures++
		default:
			t.Fatalf("unexpected takeover migration error: %v", errMigrate)
		}
	}
	if failures != 1 || successes != 1 {
		t.Fatalf("takeover results failures=%d successes=%d errors=%v", failures, successes, takeoverErrors)
	}
	assertPostgresSchemaMigrationVersion(t, db, currentDatabaseVersion)

	if errUpdate := db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 0).Error; errUpdate != nil {
		t.Fatalf("reset postgres schema version for timeout: %v", errUpdate)
	}
	releaseLock, lockDone = holdPostgresSchemaMigrationLock(t, db)
	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), 50*time.Millisecond)
	errTimeout := AutoMigrateContext(timeoutCtx, db)
	cancelTimeout()
	releaseLock()
	if errLock := <-lockDone; errLock != nil {
		t.Fatalf("release postgres migration lock after timeout: %v", errLock)
	}
	if errTimeout == nil || !strings.Contains(errTimeout.Error(), "wait for database schema migration coordination") || !errors.Is(errTimeout, context.DeadlineExceeded) {
		t.Fatalf("postgres migration timeout error = %v", errTimeout)
	}
}

func assertPostgresSchemaMigrationVersion(t *testing.T, db *gorm.DB, want int64) {
	t.Helper()
	state, errState := readPostgresSchemaMigrationState(t.Context(), db)
	if errState != nil {
		t.Fatalf("read postgres schema migration state: %v", errState)
	}
	if !state.Initialized || state.Version != want {
		t.Fatalf("postgres schema migration state = %+v, want initialized version %d", state, want)
	}
}

func holdPostgresSchemaMigrationLock(t *testing.T, db *gorm.DB) (func(), <-chan error) {
	t.Helper()
	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Transaction(func(tx *gorm.DB) error {
			var acquired bool
			if errLock := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", migrationAdvisoryLockKey).Scan(&acquired).Error; errLock != nil {
				return errLock
			}
			if !acquired {
				return fmt.Errorf("postgres migration advisory lock is already held")
			}
			close(ready)
			<-release
			return nil
		})
	}()
	select {
	case <-ready:
	case errLock := <-done:
		t.Fatalf("acquire postgres migration lock: %v", errLock)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out acquiring postgres migration lock")
	}
	var once sync.Once
	return func() { once.Do(func() { close(release) }) }, done
}

func TestSchemaMigrationErrorsAreDiagnostic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		required   int64
		poll       time.Duration
		wait       time.Duration
		operations schemaMigrationOperations
		want       string
	}{
		{name: "invalid required version", poll: time.Millisecond, operations: schemaMigrationOperations{}, want: "required schema migration version must be positive"},
		{name: "invalid poll interval", required: 1, operations: schemaMigrationOperations{}, want: "schema migration poll interval must be positive"},
		{name: "invalid wait timeout", required: 1, poll: time.Millisecond, operations: schemaMigrationOperations{}, want: "schema migration wait timeout must be positive"},
		{name: "incomplete operations", required: 1, poll: time.Millisecond, wait: time.Second, operations: schemaMigrationOperations{}, want: "schema migration operations are incomplete"},
		{name: "version read failure", required: 1, poll: time.Millisecond, wait: time.Second, operations: schemaMigrationOperations{
			currentVersion: func(context.Context) (schemaMigrationState, error) {
				return schemaMigrationState{}, fmt.Errorf("read failed")
			},
			tryLock: func(context.Context, func(lockedSchemaMigrationOperations) error) (bool, error) { return false, nil },
		}, want: "read database schema version: read failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, errCoordinate := coordinateSchemaMigration(t.Context(), test.required, test.poll, test.wait, false, test.operations)
			if errCoordinate == nil || !strings.Contains(errCoordinate.Error(), test.want) {
				t.Fatalf("coordinateSchemaMigration() error = %v, want %q", errCoordinate, test.want)
			}
		})
	}
}
