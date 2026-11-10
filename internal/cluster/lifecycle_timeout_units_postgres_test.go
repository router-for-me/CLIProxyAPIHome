package cluster

import (
	"context"
	"testing"

	"gorm.io/gorm"
)

func TestPostgresLifecycleTimeoutUnitsAndIndependentLimits(t *testing.T) {
	db := newLifecyclePostgresDB(t)
	for _, batched := range []bool{false, true} {
		mode := "separate"
		if batched {
			mode = "batched"
		}
		for _, testCase := range []struct {
			name, lock, statement   string
			wantLock, wantStatement int64
		}{
			{"disabled", "0", "0", 1000, 5000},
			{"short lock", "100ms", "0", 100, 5000},
			{"short statement", "0", "100ms", 1000, 100},
			{"milliseconds", "250ms", "1500ms", 250, 1500},
			{"seconds", "1s", "2s", 1000, 2000},
			{"minutes", "2min", "1min", 1000, 5000},
			{"hours", "1h", "2h", 1000, 5000},
			{"days", "1d", "2d", 1000, 5000},
			{"maximum", "2147483647ms", "2147483647ms", 1000, 5000},
		} {
			t.Run(mode+"/"+testCase.name, func(t *testing.T) {
				if errConnection := db.Connection(func(pinned *gorm.DB) error {
					if errSet := pinned.Exec("SELECT set_config('lock_timeout', ?, false), set_config('statement_timeout', ?, false)", testCase.lock, testCase.statement).Error; errSet != nil {
						return errSet
					}
					type settings struct{ Lock, Statement int64 }
					// Use the catalog only as an independent test oracle for milliseconds.
					readSQL := `SELECT (SELECT setting::bigint FROM pg_settings WHERE name = 'lock_timeout') AS lock,
						(SELECT setting::bigint FROM pg_settings WHERE name = 'statement_timeout') AS statement`
					var before settings
					if errRead := pinned.Raw(readSQL).Scan(&before).Error; errRead != nil {
						return errRead
					}
					ctx := t.Context()
					if batched {
						var cancel context.CancelFunc
						ctx, cancel = lifecycleTransactionContext(ctx)
						defer cancel()
					}
					if errTransaction := pinned.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
						if errConfigure := configureLifecycleTransaction(tx); errConfigure != nil {
							return errConfigure
						}
						var actual settings
						if errRead := tx.Raw(readSQL).Scan(&actual).Error; errRead != nil {
							return errRead
						}
						if want := (settings{testCase.wantLock, testCase.wantStatement}); actual != want {
							t.Errorf("timeouts = %+v, want %+v", actual, want)
						}
						return nil
					}); errTransaction != nil {
						return errTransaction
					}
					var after settings
					if errRead := pinned.Raw(readSQL).Scan(&after).Error; errRead != nil {
						return errRead
					}
					if after != before {
						t.Errorf("limits leaked into session: before=%+v after=%+v", before, after)
					}
					return nil
				}); errConnection != nil {
					t.Fatal(errConnection)
				}
			})
		}
	}
}
