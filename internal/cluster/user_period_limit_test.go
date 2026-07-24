package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"

	"gorm.io/gorm"
)

func newPeriodLimitTestRepo(t *testing.T) (*Repository, context.Context, func()) {
	t.Helper()
	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		_ = sqlDB.Close()
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewRepository(db), ctx, closeRepo
}

func createPeriodLimitUser(t *testing.T, repo *Repository, ctx context.Context, name string, credits float64) *UserRecord {
	t.Helper()
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &name, Credits: &credits})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	return user
}

func createCharge(t *testing.T, repo *Repository, ctx context.Context, userID uint, amount float64, at time.Time) {
	t.Helper()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	uid := userID
	record := &BillingChargeRecord{
		ID:            billingID("charge"),
		UsageID:       uint(at.UnixNano()%1_000_000_000) + uint(amount*1000) + userID,
		PayloadHash:   billingID("hash"),
		UserID:        &uid,
		Provider:      "test",
		Model:         "test-model",
		Amount:        amount,
		PriceSnapshot: JSONB(`{}`),
		CreatedAt:     at.UTC(),
	}
	errTx := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errCreate := tx.Create(record).Error; errCreate != nil {
			record.UsageID = uint(time.Now().UnixNano())
			record.ID = billingID("charge")
			record.PayloadHash = billingID("hash")
			if errRetry := tx.Create(record).Error; errRetry != nil {
				return errRetry
			}
		}
		// Mirror production: first_use windows open on billable charge.
		return openUserFirstUseWindowsOnChargeTx(ctx, tx, userID, at.UTC())
	})
	if errTx != nil {
		t.Fatalf("Create BillingChargeRecord error = %v", errTx)
	}
}

func TestUserPeriodLimit5hFirstUseWindow(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-5h", 100)
	limit := 10.0
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Limit5hCredits: OptionalFloatUpdate{Set: true, Value: limit}}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	key := "key-5h"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}

	// Dispatch probes must not open first_use windows.
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("first dispatch error = %v", errDispatch)
	}
	reloaded, errGet := repo.GetUser(ctx, user.ID)
	if errGet != nil {
		t.Fatalf("GetUser() error = %v", errGet)
	}
	if reloaded.PeriodWindowStart5h != nil {
		t.Fatal("dispatch must not set period_window_start_5h")
	}

	// First billable charge opens the window and counts toward the limit.
	chargeAt := time.Now().UTC()
	createCharge(t, repo, ctx, user.ID, 10, chargeAt)
	reloaded, errGet = repo.GetUser(ctx, user.ID)
	if errGet != nil {
		t.Fatalf("GetUser() after charge error = %v", errGet)
	}
	if reloaded.PeriodWindowStart5h == nil {
		t.Fatal("expected period_window_start_5h after billable charge")
	}

	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), homeerrors.TypeUserPeriodLimitExceeded) {
		t.Fatalf("expected period limit exceeded, got %v", errBlocked)
	}

	// Expire window and ensure a new session can start after the next charge.
	db, _ := repo.database()
	expired := time.Now().UTC().Add(-6 * time.Hour)
	if errUpdate := db.Model(&UserRecord{}).Where("id = ?", user.ID).Update("period_window_start_5h", expired).Error; errUpdate != nil {
		t.Fatalf("expire window error = %v", errUpdate)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch after expiry error = %v", errDispatch)
	}
}

func TestUserPeriodLimit1dCalendarAndRolling(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-1d", 100)
	limit := 5.0
	mode := PeriodWindowModeCalendar
	tz := "Asia/Shanghai"
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		Timezone:       &tz,
		Limit1dCredits: OptionalFloatUpdate{Set: true, Value: limit},
		WindowMode1d:   &mode,
	}); errUpdate != nil {
		t.Fatalf("UpdateUser(calendar) error = %v", errUpdate)
	}

	loc := loadUserLocation(tz)
	now := time.Now().In(loc)
	today := startOfDayInLocation(now, loc)
	createCharge(t, repo, ctx, user.ID, 5, today)

	key := "key-1d"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), "window=1d") {
		t.Fatalf("expected 1d calendar block, got %v", errBlocked)
	}

	// Switch to rolling; today's charge is still inside the 24-hour window.
	rolling := PeriodWindowModeRolling
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{WindowMode1d: &rolling}); errUpdate != nil {
		t.Fatalf("UpdateUser(rolling) error = %v", errUpdate)
	}
	// Existing charge is within 24h still, still blocked.
	_, _, errStill := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errStill == nil {
		t.Fatal("expected rolling 1d still blocked for recent charge")
	}

	// Reset counters and ensure pass.
	if _, errReset := repo.ResetUserPeriodLimits(ctx, user.ID, []string{PeriodWindow1d}, PeriodResetModeCounter, time.Now().UTC()); errReset != nil {
		t.Fatalf("ResetUserPeriodLimits() error = %v", errReset)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch after reset error = %v", errDispatch)
	}
}

func TestUserPeriodLimitResetSubsetAndMultiKey(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-multi", 50)
	limit5h := 1.0
	limit1d := 100.0
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		Limit5hCredits: OptionalFloatUpdate{Set: true, Value: limit5h},
		Limit1dCredits: OptionalFloatUpdate{Set: true, Value: limit1d},
	}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	key1 := "key-a"
	key2 := "key-b"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key1}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser(a) error = %v", errKey)
	}
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key2}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser(b) error = %v", errKey)
	}

	// Charge opens shared user 5h window; both keys inherit the limit.
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key1); errDispatch != nil {
		t.Fatalf("dispatch before charge error = %v", errDispatch)
	}
	createCharge(t, repo, ctx, user.ID, 1, time.Now().UTC())
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key2)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), "window=5h") {
		t.Fatalf("expected shared user 5h limit, got %v", errBlocked)
	}

	// Reset only 5h.
	result, errReset := repo.ResetUserPeriodLimits(ctx, user.ID, []string{PeriodWindow5h}, PeriodResetModeCounter, time.Now().UTC())
	if errReset != nil {
		t.Fatalf("reset error = %v", errReset)
	}
	if len(result.Windows) != 1 || result.Windows[0] != PeriodWindow5h {
		t.Fatalf("reset windows = %v, want [5h]", result.Windows)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key2); errDispatch != nil {
		t.Fatalf("dispatch after 5h reset error = %v", errDispatch)
	}
}

func TestUserPeriodLimitNullDisabledAndZeroBlocks(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-zero", 10)
	key := "key-zero"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	// No limits: pass.
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch without limits error = %v", errDispatch)
	}
	// limit=0 blocks.
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Limit1dCredits: OptionalFloatUpdate{Set: true, Value: 0}}); errUpdate != nil {
		t.Fatalf("UpdateUser(0) error = %v", errUpdate)
	}
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), homeerrors.TypeUserPeriodLimitExceeded) {
		t.Fatalf("expected zero limit block, got %v", errBlocked)
	}
	// clear limit.
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Limit1dCredits: OptionalFloatUpdate{Set: true, Clear: true}}); errUpdate != nil {
		t.Fatalf("UpdateUser(clear) error = %v", errUpdate)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch after clear error = %v", errDispatch)
	}
}

func TestUserPeriodLimitCreditsStillEnforced(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-credits", 0)
	key := "key-credits"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), homeerrors.TypeUserCreditsInsufficient) {
		t.Fatalf("expected credits insufficient, got %v", errBlocked)
	}
}

func TestUserUnlimitedCreditsStillEnforcesPeriodLimits(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-unlimited", 0)
	unlimited := true
	limit := 1.0
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		CreditsUnlimited: &unlimited,
		Limit5hCredits:   OptionalFloatUpdate{Set: true, Value: limit},
	}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	key := "key-unlimited"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch before period spend error = %v", errDispatch)
	}
	createCharge(t, repo, ctx, user.ID, 1, time.Now().UTC())
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), homeerrors.TypeUserPeriodLimitExceeded) {
		t.Fatalf("expected period limit exceeded, got %v", errBlocked)
	}
}

func TestUserPeriodLimitsEnabled(t *testing.T) {
	t.Parallel()

	if userPeriodLimitsEnabled(&UserRecord{}) {
		t.Fatal("empty user should not have period limits enabled")
	}
	limit := 1.0
	if !userPeriodLimitsEnabled(&UserRecord{Limit7dCredits: &limit}) {
		t.Fatal("configured limit should enable period limit evaluation")
	}
}

func TestStartOfCalendarWeek(t *testing.T) {
	t.Parallel()
	loc := loadUserLocation("Asia/Shanghai")
	// Wednesday 2026-07-08 15:00 +08
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, loc)
	start := startOfCalendarWeek(now, loc, 1, 0) // Monday 00:00
	want := time.Date(2026, 7, 6, 0, 0, 0, 0, loc)
	if !start.Equal(want) {
		t.Fatalf("start = %v, want %v", start, want)
	}
}

func TestUserPeriodLimit5hSliding(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-5h-slide", 100)
	limit := 10.0
	mode := PeriodWindowModeSliding
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		Limit5hCredits: OptionalFloatUpdate{Set: true, Value: limit},
		WindowMode5h:   &mode,
	}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	key := "key-5h-slide"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	// Sliding does not open first_use window_start.
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch error = %v", errDispatch)
	}
	reloaded, _ := repo.GetUser(ctx, user.ID)
	if reloaded.PeriodWindowStart5h != nil {
		t.Fatal("sliding mode should not set period_window_start_5h")
	}
	createCharge(t, repo, ctx, user.ID, 10, time.Now().UTC().Add(-time.Hour))
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), "window=5h") {
		t.Fatalf("expected sliding 5h block, got %v", errBlocked)
	}
	// Charge outside 5h should not block after reset... better: old charge beyond 5h
	// Move by inserting only old charge: first clear via epoch
	if _, errReset := repo.ResetUserPeriodLimits(ctx, user.ID, []string{PeriodWindow5h}, PeriodResetModeCounter, time.Now().UTC()); errReset != nil {
		t.Fatalf("reset error = %v", errReset)
	}
	createCharge(t, repo, ctx, user.ID, 10, time.Now().UTC().Add(-6*time.Hour))
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch with aged charge error = %v", errDispatch)
	}
}

func TestUserPeriodLimit1dFirstUseFirstCharge(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-1d-first-use", 100)
	limit := 3.0
	mode := PeriodWindowModeFirstUse
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		Limit1dCredits: OptionalFloatUpdate{Set: true, Value: limit},
		WindowMode1d:   &mode,
	}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	key := "key-1d-first-use"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("first dispatch error = %v", errDispatch)
	}
	reloaded, _ := repo.GetUser(ctx, user.ID)
	if reloaded.PeriodWindowStart1d != nil {
		t.Fatal("dispatch must not open period_window_start_1d")
	}
	chargeAt := time.Now().UTC()
	createCharge(t, repo, ctx, user.ID, 3, chargeAt)
	reloaded, _ = repo.GetUser(ctx, user.ID)
	if reloaded.PeriodWindowStart1d == nil {
		t.Fatal("expected period_window_start_1d after charge")
	}
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), "window=1d") {
		t.Fatalf("expected first_use 1d block, got %v", errBlocked)
	}
}

func TestNormalizePeriodWindowModeAliases(t *testing.T) {
	t.Parallel()
	mode, err := normalizePeriodWindowMode("rolling", false)
	if err != nil || mode != PeriodWindowModeSliding {
		t.Fatalf("rolling alias = %q %v", mode, err)
	}
	if _, err := normalizePeriodWindowMode("calendar", false); err == nil {
		t.Fatal("expected calendar rejected for 5h")
	}
	mode, err = normalizePeriodWindowMode("calendar", true)
	if err != nil || mode != PeriodWindowModeCalendar {
		t.Fatalf("calendar = %q %v", mode, err)
	}
}

func TestNormalizeFixedAliasToFirstUse(t *testing.T) {
	t.Parallel()
	mode, err := normalizePeriodWindowMode("fixed", true)
	if err != nil || mode != PeriodWindowModeFirstUse {
		t.Fatalf("fixed alias = %q %v, want first_use", mode, err)
	}
	mode, err = normalizePeriodWindowMode("first_use", false)
	if err != nil || mode != PeriodWindowModeFirstUse {
		t.Fatalf("first_use = %q %v", mode, err)
	}
}

func TestCalendarDayAndWeekUseAddDate(t *testing.T) {
	t.Parallel()
	// America/New_York observes DST; AddDate day boundaries stay on local midnights.
	loc := loadUserLocation("America/New_York")
	// 2026-03-08 is spring-forward day in US.
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	dayStart := startOfDayInLocation(now, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	if dayEnd.Location().String() != loc.String() {
		t.Fatalf("day end loc = %v", dayEnd.Location())
	}
	if dayEnd.Hour() != 0 || dayEnd.Day() != 9 {
		t.Fatalf("day end = %v, want next local midnight", dayEnd)
	}
	weekStart := startOfCalendarWeek(now, loc, 1, 0)
	weekEnd := weekStart.AddDate(0, 0, 7)
	if weekEnd.Weekday() != weekStart.Weekday() || weekEnd.Hour() != weekStart.Hour() {
		t.Fatalf("week end = %v start = %v", weekEnd, weekStart)
	}
}

func TestWindowOnlyResetClearsSlidingUsage(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-slide-reset", 100)
	limit := 5.0
	mode := PeriodWindowModeSliding
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		Limit1dCredits: OptionalFloatUpdate{Set: true, Value: limit},
		WindowMode1d:   &mode,
	}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}
	key := "key-slide-reset"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	createCharge(t, repo, ctx, user.ID, 5, time.Now().UTC().Add(-time.Hour))
	_, _, errBlocked := repo.AllowedDispatchIDsForAPIKey(ctx, key)
	if errBlocked == nil {
		t.Fatal("expected sliding block before reset")
	}
	if _, errReset := repo.ResetUserPeriodLimits(ctx, user.ID, []string{PeriodWindow1d}, PeriodResetModeWindowOnly, time.Now().UTC()); errReset != nil {
		t.Fatalf("window_only reset error = %v", errReset)
	}
	if _, _, errDispatch := repo.AllowedDispatchIDsForAPIKey(ctx, key); errDispatch != nil {
		t.Fatalf("dispatch after window_only sliding reset error = %v", errDispatch)
	}
}

func TestApplyUserPeriodLimitUpdateAttachesFieldAndCode(t *testing.T) {
	t.Parallel()

	badTimezone := "Mars/Olympus"
	badMode := "calendar"
	badHour := 24
	tests := []struct {
		name      string
		update    UserUpdate
		wantField string
		wantCode  string
	}{
		{
			name:      "timezone",
			update:    UserUpdate{Timezone: &badTimezone},
			wantField: "timezone",
			wantCode:  PeriodLimitErrInvalidTimezone,
		},
		{
			name:      "calendar rejected for 5h window",
			update:    UserUpdate{WindowMode5h: &badMode},
			wantField: "window_mode_5h",
			wantCode:  PeriodLimitErrInvalidWindowMode,
		},
		{
			name:      "negative limit",
			update:    UserUpdate{Limit1dCredits: OptionalFloatUpdate{Set: true, Value: -1}},
			wantField: "limit_1d_credits",
			wantCode:  PeriodLimitErrInvalidLimit,
		},
		{
			name:      "week reset hour out of range",
			update:    UserUpdate{WeekResetHour: &badHour},
			wantField: "week_reset_hour",
			wantCode:  PeriodLimitErrInvalidWeekResetHour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := &UserRecord{}
			err := applyUserPeriodLimitUpdate(record, tc.update)
			if err == nil {
				t.Fatal("applyUserPeriodLimitUpdate() error = nil, want validation error")
			}
			var configErr PeriodLimitConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("error type = %T, want PeriodLimitConfigError", err)
			}
			if configErr.Field != tc.wantField || configErr.Code != tc.wantCode {
				t.Fatalf("field/code = %q/%q, want %q/%q", configErr.Field, configErr.Code, tc.wantField, tc.wantCode)
			}
		})
	}
}

func TestResetUserPeriodLimitsAttachesFieldAndCode(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-reset-field-errors", 100)

	if _, err := repo.ResetUserPeriodLimits(ctx, user.ID, nil, "bogus", time.Now().UTC()); err != nil {
		var configErr PeriodLimitConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("reset mode error type = %T, want PeriodLimitConfigError", err)
		}
		if configErr.Field != "mode" || configErr.Code != PeriodLimitErrInvalidResetMode {
			t.Fatalf("field/code = %q/%q, want mode/%q", configErr.Field, configErr.Code, PeriodLimitErrInvalidResetMode)
		}
	} else {
		t.Fatal("ResetUserPeriodLimits() error = nil, want invalid mode error")
	}

	if _, err := repo.ResetUserPeriodLimits(ctx, user.ID, []string{"2h"}, PeriodResetModeCounter, time.Now().UTC()); err != nil {
		var configErr PeriodLimitConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("reset windows error type = %T, want PeriodLimitConfigError", err)
		}
		if configErr.Field != "windows" || configErr.Code != PeriodLimitErrInvalidResetWindows {
			t.Fatalf("field/code = %q/%q, want windows/%q", configErr.Field, configErr.Code, PeriodLimitErrInvalidResetWindows)
		}
	} else {
		t.Fatal("ResetUserPeriodLimits() error = nil, want unknown window error")
	}
}

func TestResetUserPeriodLimitsEmptyWindowsTargetsAllWindows(t *testing.T) {
	t.Parallel()
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	user := createPeriodLimitUser(t, repo, ctx, "u-reset-all-windows", 100)
	limit := 5.0
	if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{
		Limit5hCredits: OptionalFloatUpdate{Set: true, Value: limit},
	}); errUpdate != nil {
		t.Fatalf("UpdateUser() error = %v", errUpdate)
	}

	now := time.Now().UTC()
	result, errReset := repo.ResetUserPeriodLimits(ctx, user.ID, nil, PeriodResetModeCounter, now)
	if errReset != nil {
		t.Fatalf("ResetUserPeriodLimits() error = %v", errReset)
	}
	wantWindows := []string{PeriodWindow5h, PeriodWindow1d, PeriodWindow7d, PeriodWindow30d}
	if len(result.Windows) != len(wantWindows) {
		t.Fatalf("reset windows = %v, want %v", result.Windows, wantWindows)
	}
	for index, want := range wantWindows {
		if result.Windows[index] != want {
			t.Fatalf("reset windows = %v, want %v", result.Windows, wantWindows)
		}
	}

	reloaded, errGet := repo.GetUser(ctx, user.ID)
	if errGet != nil {
		t.Fatalf("GetUser() error = %v", errGet)
	}
	for field, epoch := range map[string]*time.Time{
		"usage_epoch_5h":  reloaded.UsageEpoch5h,
		"usage_epoch_1d":  reloaded.UsageEpoch1d,
		"usage_epoch_7d":  reloaded.UsageEpoch7d,
		"usage_epoch_30d": reloaded.UsageEpoch30d,
	} {
		if epoch == nil || !epoch.Equal(now) {
			t.Fatalf("%s = %v, want %v", field, epoch, now)
		}
	}
}
