package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	schemaMigrationRecordID     int64 = 1
	schemaMigrationPollInterval       = 250 * time.Millisecond
	schemaMigrationWaitTimeout        = 10 * time.Minute
)

type schemaMigrationRecord struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement:false"`
	Version   int64     `gorm:"column:version;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (schemaMigrationRecord) TableName() string {
	return "home_schema_migration"
}

type schemaMigrationState struct {
	Version     int64
	Initialized bool
}

type schemaMigrationResult struct {
	InitialState       schemaMigrationState
	CurrentState       schemaMigrationState
	LockAcquired       bool
	MigrationExecutor  bool
	MigrationCommitted bool
	LockAttempts       int
	WaitDuration       time.Duration
}

type lockedSchemaMigrationOperations struct {
	currentVersion   func(context.Context) (schemaMigrationState, error)
	migrateToVersion func(context.Context, int64, int64) error
}

type schemaMigrationOperations struct {
	currentVersion func(context.Context) (schemaMigrationState, error)
	tryLock        func(context.Context, func(lockedSchemaMigrationOperations) error) (bool, error)
}

func coordinateSchemaMigration(ctx context.Context, requiredVersion int64, pollInterval time.Duration, waitTimeout time.Duration, runWhenCurrent bool, operations schemaMigrationOperations) (schemaMigrationResult, error) {
	result := schemaMigrationResult{}
	if requiredVersion <= 0 {
		return result, fmt.Errorf("required schema migration version must be positive")
	}
	if pollInterval <= 0 {
		return result, fmt.Errorf("schema migration poll interval must be positive")
	}
	if waitTimeout <= 0 {
		return result, fmt.Errorf("schema migration wait timeout must be positive")
	}
	if operations.currentVersion == nil || operations.tryLock == nil {
		return result, fmt.Errorf("schema migration operations are incomplete")
	}

	ctx = contextOrBackground(ctx)
	waitStartedAt := time.Now()
	waitDeadline := waitStartedAt.Add(waitTimeout)
	initialCtx, cancelInitial := context.WithDeadline(ctx, waitDeadline)
	initialState, errVersion := operations.currentVersion(initialCtx)
	initialErr := initialCtx.Err()
	cancelInitial()
	if errVersion != nil {
		waitErr := ctx.Err()
		if waitErr == nil {
			switch {
			case initialErr != nil:
				waitErr = initialErr
			case errors.Is(errVersion, context.DeadlineExceeded), errors.Is(errVersion, context.Canceled):
				waitErr = errVersion
			}
		}
		if waitErr != nil {
			result.WaitDuration = time.Since(waitStartedAt)
			return result, fmt.Errorf("wait for database schema migration coordination while reading schema version (required=%d waited=%s): %w", requiredVersion, result.WaitDuration.Round(time.Millisecond), waitErr)
		}
		return result, fmt.Errorf("read database schema version: %w", errVersion)
	}
	result.InitialState = initialState
	result.CurrentState = initialState
	if initialState.Version > requiredVersion && runWhenCurrent {
		return result, fmt.Errorf("database schema version %d is newer than required version %d; refusing to run a current-version migration", initialState.Version, requiredVersion)
	}
	if initialState.Version >= requiredVersion && !runWhenCurrent {
		return result, nil
	}

	for {
		migrationExecuted := false
		attemptCtx, cancelAttempt := context.WithDeadline(ctx, waitDeadline)
		acquired, errLock := operations.tryLock(attemptCtx, func(lockedOperations lockedSchemaMigrationOperations) error {
			result.WaitDuration = time.Since(waitStartedAt)
			if lockedOperations.currentVersion == nil || lockedOperations.migrateToVersion == nil {
				return fmt.Errorf("locked schema migration operations are incomplete")
			}
			currentState, errCurrent := lockedOperations.currentVersion(ctx)
			if errCurrent != nil {
				return fmt.Errorf("re-read database schema version after acquiring migration lock: %w", errCurrent)
			}
			result.CurrentState = currentState
			if currentState.Version > requiredVersion && runWhenCurrent {
				return fmt.Errorf("database schema version %d is newer than required version %d; refusing to run a current-version migration", currentState.Version, requiredVersion)
			}
			if currentState.Version >= requiredVersion && !runWhenCurrent {
				return nil
			}
			result.MigrationExecutor = true
			if errMigrate := lockedOperations.migrateToVersion(ctx, currentState.Version, requiredVersion); errMigrate != nil {
				return errMigrate
			}
			migrationExecuted = true
			return nil
		})
		attemptErr := attemptCtx.Err()
		cancelAttempt()
		result.LockAttempts++
		result.LockAcquired = acquired
		if !acquired {
			result.WaitDuration = time.Since(waitStartedAt)
		}
		if errLock != nil {
			if !acquired {
				waitErr := ctx.Err()
				if waitErr == nil && (attemptErr != nil || errors.Is(errLock, context.DeadlineExceeded)) {
					waitErr = context.DeadlineExceeded
				}
				if waitErr != nil {
					return result, fmt.Errorf("wait for database schema migration coordination (current=%d required=%d waited=%s): %w", result.CurrentState.Version, requiredVersion, result.WaitDuration.Round(time.Millisecond), waitErr)
				}
			}
			return result, fmt.Errorf("execute database schema migration from version %d to %d: %w", result.CurrentState.Version, requiredVersion, errLock)
		}
		if acquired {
			result.MigrationCommitted = migrationExecuted
			if migrationExecuted {
				committedVersion := requiredVersion
				if result.CurrentState.Version > committedVersion {
					committedVersion = result.CurrentState.Version
				}
				result.CurrentState = schemaMigrationState{Version: committedVersion, Initialized: true}
			}
			return result, nil
		}
		if result.WaitDuration >= waitTimeout {
			return result, fmt.Errorf("wait for database schema migration coordination (current=%d required=%d waited=%s): %w", result.CurrentState.Version, requiredVersion, result.WaitDuration.Round(time.Millisecond), context.DeadlineExceeded)
		}

		waitDuration := pollInterval
		if remaining := time.Until(waitDeadline); remaining < waitDuration {
			waitDuration = remaining
		}
		if waitDuration <= 0 {
			result.WaitDuration = time.Since(waitStartedAt)
			return result, fmt.Errorf("wait for database schema migration coordination (current=%d required=%d waited=%s): %w", result.CurrentState.Version, requiredVersion, result.WaitDuration.Round(time.Millisecond), context.DeadlineExceeded)
		}
		timer := time.NewTimer(waitDuration)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			result.WaitDuration = time.Since(waitStartedAt)
			return result, fmt.Errorf("wait for database schema migration coordination (current=%d required=%d waited=%s): %w", result.CurrentState.Version, requiredVersion, result.WaitDuration.Round(time.Millisecond), ctx.Err())
		case <-timer.C:
		}

		pollCtx, cancelPoll := context.WithDeadline(ctx, waitDeadline)
		currentState, errCurrent := operations.currentVersion(pollCtx)
		pollErr := pollCtx.Err()
		cancelPoll()
		if errCurrent != nil {
			waitErr := ctx.Err()
			if waitErr == nil && (pollErr != nil || errors.Is(errCurrent, context.DeadlineExceeded)) {
				waitErr = context.DeadlineExceeded
			}
			if waitErr != nil {
				result.WaitDuration = time.Since(waitStartedAt)
				return result, fmt.Errorf("wait for database schema migration coordination (current=%d required=%d waited=%s): %w", result.CurrentState.Version, requiredVersion, result.WaitDuration.Round(time.Millisecond), waitErr)
			}
			return result, fmt.Errorf("poll database schema version: %w", errCurrent)
		}
		result.CurrentState = currentState
		result.WaitDuration = time.Since(waitStartedAt)
		if currentState.Version > requiredVersion && runWhenCurrent {
			return result, fmt.Errorf("database schema version %d is newer than required version %d; refusing to run a current-version migration", currentState.Version, requiredVersion)
		}
		if currentState.Version >= requiredVersion && !runWhenCurrent {
			return result, nil
		}
		if result.WaitDuration >= waitTimeout {
			return result, fmt.Errorf("wait for database schema migration coordination (current=%d required=%d waited=%s): %w", result.CurrentState.Version, requiredVersion, result.WaitDuration.Round(time.Millisecond), context.DeadlineExceeded)
		}
	}
}

func postgresSchemaMigrationOperations(db *gorm.DB, startedAt time.Time, executionCtx context.Context, runMigration func(context.Context, *gorm.DB, int64, int64) error) schemaMigrationOperations {
	executionCtx = contextOrBackground(executionCtx)
	return schemaMigrationOperations{
		currentVersion: func(ctx context.Context) (schemaMigrationState, error) {
			return readPostgresSchemaMigrationState(ctx, db)
		},
		tryLock: func(waitCtx context.Context, migrate func(lockedSchemaMigrationOperations) error) (bool, error) {
			waitCtx = contextOrBackground(waitCtx)
			acquired := false
			acquisitionCtx, cancelAcquisition := context.WithCancel(executionCtx)
			waitCancellationDone := make(chan struct{})
			stopWaitCancellation := context.AfterFunc(waitCtx, func() {
				cancelAcquisition()
				close(waitCancellationDone)
			})
			defer func() {
				stopWaitCancellation()
				cancelAcquisition()
			}()

			errTransaction := db.WithContext(acquisitionCtx).Transaction(func(tx *gorm.DB) error {
				if errLock := tx.WithContext(acquisitionCtx).Raw("SELECT pg_try_advisory_xact_lock(?)", migrationAdvisoryLockKey).Scan(&acquired).Error; errLock != nil {
					return fmt.Errorf("try database migration advisory lock: %w", errLock)
				}
				if !acquired {
					return nil
				}
				// Stop only the coordination deadline after acquiring the lock. The
				// transaction remains governed by the startup execution context.
				if !stopWaitCancellation() {
					<-waitCancellationDone
				}
				if errAcquisition := acquisitionCtx.Err(); errAcquisition != nil {
					acquired = false
					return fmt.Errorf("complete database migration advisory lock acquisition: %w", errAcquisition)
				}
				return migrate(lockedSchemaMigrationOperations{
					currentVersion: func(lockedCtx context.Context) (schemaMigrationState, error) {
						return readPostgresSchemaMigrationState(lockedCtx, tx)
					},
					migrateToVersion: func(lockedCtx context.Context, currentVersion int64, requiredVersion int64) error {
						if runMigration == nil {
							return fmt.Errorf("postgres schema migration runner is nil")
						}
						log.WithFields(log.Fields{
							"database_backend":        "postgres",
							"schema_current_version":  currentVersion,
							"schema_required_version": requiredVersion,
							"migration_executor":      true,
							"migration_elapsed_ms":    time.Since(startedAt).Milliseconds(),
						}).Info("database schema migration execution started")
						return runMigration(lockedCtx, tx, currentVersion, requiredVersion)
					},
				})
			})
			return acquired, errTransaction
		},
	}
}

func readPostgresSchemaMigrationState(ctx context.Context, db *gorm.DB) (schemaMigrationState, error) {
	if db == nil {
		return schemaMigrationState{}, fmt.Errorf("database connection is nil")
	}
	ctx = contextOrBackground(ctx)
	tableName := (schemaMigrationRecord{}).TableName()
	var tableExists bool
	if errTable := db.WithContext(ctx).Raw(`SELECT EXISTS (
		SELECT 1
		FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?
	)`, tableName).Scan(&tableExists).Error; errTable != nil {
		return schemaMigrationState{}, errTable
	}
	if !tableExists {
		return schemaMigrationState{}, nil
	}

	var version int64
	result := db.WithContext(ctx).Raw(`SELECT "version" FROM "home_schema_migration" WHERE "id" = ? LIMIT 1`, schemaMigrationRecordID).Scan(&version)
	if result.Error != nil {
		return schemaMigrationState{}, result.Error
	}
	return schemaMigrationState{Version: version, Initialized: result.RowsAffected > 0}, nil
}

func writePostgresSchemaMigrationVersion(ctx context.Context, db *gorm.DB, version int64) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if version <= 0 {
		return fmt.Errorf("schema migration version must be positive")
	}
	return db.WithContext(contextOrBackground(ctx)).Exec(`INSERT INTO "home_schema_migration" ("id", "version", "updated_at")
		VALUES (?, ?, ?)
		ON CONFLICT ("id") DO UPDATE SET
			"version" = GREATEST("home_schema_migration"."version", EXCLUDED."version"),
			"updated_at" = EXCLUDED."updated_at"`, schemaMigrationRecordID, version, time.Now().UTC()).Error
}
