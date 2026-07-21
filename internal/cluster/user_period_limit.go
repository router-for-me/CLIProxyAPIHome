package cluster

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	PeriodWindow5h  = "5h"
	PeriodWindow1d  = "1d"
	PeriodWindow7d  = "7d"
	PeriodWindow30d = "30d"

	// Period window modes (canonical values stored and returned by APIs).
	// first_use: first billable charge opens a wall-clock duration window.
	//   Aligns with Claude Code / Codex "5h session" + optional weekly layers:
	//   quota is a spend budget inside the window, not wall-clock working time.
	// sliding: rolling last-N duration (API also accepts "rolling").
	// calendar: natural day / week / month (1d/7d/30d only).
	// "fixed" is a deprecated alias of first_use (name was ambiguous).
	PeriodWindowModeFirstUse = "first_use"
	PeriodWindowModeFixed    = "fixed" // deprecated alias of first_use
	PeriodWindowModeSliding  = "sliding"
	PeriodWindowModeRolling  = "rolling" // alias of sliding
	PeriodWindowModeCalendar = "calendar"

	PeriodResetModeCounter    = "counter"
	PeriodResetModeWindowOnly = "window_only"

	DefaultUserTimezone     = "Asia/Shanghai"
	DefaultPeriodWindowMode = PeriodWindowModeFirstUse
	DefaultWeekResetDay     = 1
	DefaultWeekResetHour    = 0

	periodLimit5hDuration  = 5 * time.Hour
	periodLimit1dDuration  = 24 * time.Hour
	periodLimit7dDuration  = 7 * 24 * time.Hour
	periodLimit30dDuration = 30 * 24 * time.Hour
)

// OptionalFloatUpdate captures a three-state float patch: absent / null / value.

// Stable machine-readable codes for period-limit validation failures. The
// Management API surfaces them in field_errors so clients can localize and
// anchor errors without parsing message strings.
const (
	PeriodLimitErrInvalidLimit         = "invalid_limit"
	PeriodLimitErrInvalidWindowMode    = "invalid_window_mode"
	PeriodLimitErrInvalidTimezone      = "invalid_timezone"
	PeriodLimitErrInvalidWeekResetDay  = "invalid_week_reset_day"
	PeriodLimitErrInvalidWeekResetHour = "invalid_week_reset_hour"
	PeriodLimitErrInvalidResetMode     = "invalid_reset_mode"
	PeriodLimitErrInvalidResetWindows  = "invalid_reset_windows"
)

// PeriodLimitConfigError is returned for invalid period-limit configuration inputs.
type PeriodLimitConfigError struct {
	Message string
	// Field is the Management API field that failed validation (for example
	// "limit_5h_credits" or "timezone"). Empty when not field-specific.
	Field string
	// Code is one of the PeriodLimitErr* constants. Empty for legacy errors.
	Code string
}

func (e PeriodLimitConfigError) Error() string {
	if e.Message == "" {
		return "invalid period limit configuration"
	}
	return e.Message
}

func periodLimitConfigErrorf(format string, args ...any) error {
	return PeriodLimitConfigError{Message: fmt.Sprintf(format, args...)}
}

// periodLimitFieldError keeps the historic message text while attaching the
// stable field/code pair exposed through the Management API.
func periodLimitFieldError(field, code, message string) error {
	return PeriodLimitConfigError{Message: message, Field: field, Code: code}
}

type OptionalFloatUpdate struct {
	Set   bool
	Clear bool
	Value float64
}

// UserPeriodWindowStatus is the management-facing status for one window.
type UserPeriodWindowStatus struct {
	ID            string     `json:"id"`
	Enabled       bool       `json:"enabled"`
	Limit         *float64   `json:"limit"`
	Used          float64    `json:"used"`
	Remaining     *float64   `json:"remaining"`
	Mode          string     `json:"mode"`
	Active        bool       `json:"active"`
	WindowStart   *time.Time `json:"window_start"`
	WindowEnd     *time.Time `json:"window_end"`
	ResetAt       *time.Time `json:"reset_at"`
	UsageEpoch    *time.Time `json:"usage_epoch"`
	WeekResetDay  *int       `json:"week_reset_day,omitempty"`
	WeekResetHour *int       `json:"week_reset_hour,omitempty"`
}

// UserPeriodLimitsStatus is the management-facing aggregate status.
type UserPeriodLimitsStatus struct {
	UserID           uint                     `json:"user_id"`
	Timezone         string                   `json:"timezone"`
	Credits          float64                  `json:"credits"`
	CreditsUnlimited bool                     `json:"credits_unlimited"`
	Windows          []UserPeriodWindowStatus `json:"windows"`
}

// UserPeriodLimitResetResult is returned by ResetUserPeriodLimits.
type UserPeriodLimitResetResult struct {
	UserID  uint                   `json:"user_id"`
	Mode    string                 `json:"mode"`
	Windows []string               `json:"windows"`
	At      time.Time              `json:"at"`
	Limits  UserPeriodLimitsStatus `json:"limits"`
}

func defaultUserPeriodLimitFields(record *UserRecord) {
	if record == nil {
		return
	}
	if strings.TrimSpace(record.Timezone) == "" {
		record.Timezone = DefaultUserTimezone
	}
	if strings.TrimSpace(record.WindowMode5h) == "" {
		record.WindowMode5h = DefaultPeriodWindowMode
	} else if mode, errMode := normalizePeriodWindowMode(record.WindowMode5h, false); errMode == nil {
		record.WindowMode5h = mode
	}
	if strings.TrimSpace(record.WindowMode1d) == "" {
		record.WindowMode1d = DefaultPeriodWindowMode
	} else if mode, errMode := normalizePeriodWindowMode(record.WindowMode1d, true); errMode == nil {
		record.WindowMode1d = mode
	}
	if strings.TrimSpace(record.WindowMode7d) == "" {
		record.WindowMode7d = DefaultPeriodWindowMode
	} else if mode, errMode := normalizePeriodWindowMode(record.WindowMode7d, true); errMode == nil {
		record.WindowMode7d = mode
	}
	if strings.TrimSpace(record.WindowMode30d) == "" {
		record.WindowMode30d = DefaultPeriodWindowMode
	} else if mode, errMode := normalizePeriodWindowMode(record.WindowMode30d, true); errMode == nil {
		record.WindowMode30d = mode
	}
	if record.WeekResetDay == 0 {
		record.WeekResetDay = DefaultWeekResetDay
	}
	if record.WeekResetHour < 0 || record.WeekResetHour > 23 {
		record.WeekResetHour = DefaultWeekResetHour
	}
}

func applyOptionalFloat(update OptionalFloatUpdate, dest **float64) error {
	if !update.Set {
		return nil
	}
	if update.Clear {
		*dest = nil
		return nil
	}
	if update.Value < 0 || math.IsNaN(update.Value) || math.IsInf(update.Value, 0) {
		return periodLimitConfigErrorf("limit must be a non-negative number")
	}
	value := update.Value
	*dest = &value
	return nil
}

// CanonicalPeriodWindowMode returns the stored/API mode for display.
// Unknown values fall back to the default first_use mode.
func CanonicalPeriodWindowMode(mode string, allowCalendar bool) string {
	normalized, errMode := normalizePeriodWindowMode(mode, allowCalendar)
	if errMode != nil {
		return DefaultPeriodWindowMode
	}
	return normalized
}

// normalizePeriodWindowMode normalizes API/storage mode values.
// allowCalendar enables calendar mode (1d/7d/30d). 5h must pass false.
func normalizePeriodWindowMode(mode string, allowCalendar bool) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return DefaultPeriodWindowMode, nil
	}
	switch mode {
	case PeriodWindowModeFirstUse, PeriodWindowModeFixed:
		// "fixed" kept as alias; canonical storage is first_use.
		return PeriodWindowModeFirstUse, nil
	case PeriodWindowModeSliding, PeriodWindowModeRolling:
		return PeriodWindowModeSliding, nil
	case PeriodWindowModeCalendar:
		if !allowCalendar {
			return "", periodLimitConfigErrorf("window mode %q is not supported for this window", mode)
		}
		return PeriodWindowModeCalendar, nil
	default:
		if allowCalendar {
			return "", periodLimitConfigErrorf("window mode must be %q, %q, or %q", PeriodWindowModeFirstUse, PeriodWindowModeSliding, PeriodWindowModeCalendar)
		}
		return "", periodLimitConfigErrorf("window mode must be %q or %q", PeriodWindowModeFirstUse, PeriodWindowModeSliding)
	}
}

func normalizeUserTimezone(timezone string) (string, error) {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return DefaultUserTimezone, nil
	}
	if _, errLoad := time.LoadLocation(timezone); errLoad != nil {
		return "", periodLimitConfigErrorf("invalid timezone %q", timezone)
	}
	return timezone, nil
}

func normalizeWeekResetDay(day int) (int, error) {
	if day < 1 || day > 7 {
		return 0, periodLimitConfigErrorf("week_reset_day must be between 1 and 7")
	}
	return day, nil
}

func normalizeWeekResetHour(hour int) (int, error) {
	if hour < 0 || hour > 23 {
		return 0, periodLimitConfigErrorf("week_reset_hour must be between 0 and 23")
	}
	return hour, nil
}

func loadUserLocation(timezone string) *time.Location {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		timezone = DefaultUserTimezone
	}
	loc, errLoad := time.LoadLocation(timezone)
	if errLoad != nil {
		loc, _ = time.LoadLocation(DefaultUserTimezone)
	}
	if loc == nil {
		return time.UTC
	}
	return loc
}

func startOfDayInLocation(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

func startOfMonthInLocation(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
}

// startOfCalendarWeek returns the start of the current billing week in loc.
// resetDay uses 1=Monday ... 7=Sunday; resetHour is 0-23 in loc.
func startOfCalendarWeek(now time.Time, loc *time.Location, resetDay, resetHour int) time.Time {
	if resetDay < 1 || resetDay > 7 {
		resetDay = DefaultWeekResetDay
	}
	if resetHour < 0 || resetHour > 23 {
		resetHour = DefaultWeekResetHour
	}
	local := now.In(loc)
	ourWeekday := int(local.Weekday())
	if ourWeekday == 0 {
		ourWeekday = 7
	}
	daysSince := (ourWeekday - resetDay + 7) % 7
	candidate := time.Date(local.Year(), local.Month(), local.Day(), resetHour, 0, 0, 0, loc).AddDate(0, 0, -daysSince)
	if local.Before(candidate) {
		candidate = candidate.AddDate(0, 0, -7)
	}
	return candidate
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}

func timePtr(value time.Time) *time.Time {
	copied := value.UTC()
	return &copied
}

func remainingCredits(limit *float64, used float64) *float64 {
	if limit == nil {
		return nil
	}
	remaining := *limit - used
	if remaining < 0 {
		remaining = 0
	}
	return &remaining
}

func periodDuration(id string) time.Duration {
	switch id {
	case PeriodWindow5h:
		return periodLimit5hDuration
	case PeriodWindow1d:
		return periodLimit1dDuration
	case PeriodWindow7d:
		return periodLimit7dDuration
	case PeriodWindow30d:
		return periodLimit30dDuration
	default:
		return 0
	}
}

func userWindowMode(user *UserRecord, id string) string {
	if user == nil {
		return DefaultPeriodWindowMode
	}
	switch id {
	case PeriodWindow5h:
		return user.WindowMode5h
	case PeriodWindow1d:
		return user.WindowMode1d
	case PeriodWindow7d:
		return user.WindowMode7d
	case PeriodWindow30d:
		return user.WindowMode30d
	default:
		return DefaultPeriodWindowMode
	}
}

func userWindowStart(user *UserRecord, id string) *time.Time {
	if user == nil {
		return nil
	}
	switch id {
	case PeriodWindow5h:
		return user.PeriodWindowStart5h
	case PeriodWindow1d:
		return user.PeriodWindowStart1d
	case PeriodWindow7d:
		return user.PeriodWindowStart7d
	case PeriodWindow30d:
		return user.PeriodWindowStart30d
	default:
		return nil
	}
}

func userUsageEpoch(user *UserRecord, id string) *time.Time {
	if user == nil {
		return nil
	}
	switch id {
	case PeriodWindow5h:
		return user.UsageEpoch5h
	case PeriodWindow1d:
		return user.UsageEpoch1d
	case PeriodWindow7d:
		return user.UsageEpoch7d
	case PeriodWindow30d:
		return user.UsageEpoch30d
	default:
		return nil
	}
}

func windowStartColumn(id string) string {
	switch id {
	case PeriodWindow5h:
		return "period_window_start_5h"
	case PeriodWindow1d:
		return "period_window_start_1d"
	case PeriodWindow7d:
		return "period_window_start_7d"
	case PeriodWindow30d:
		return "period_window_start_30d"
	default:
		return ""
	}
}

func usageEpochColumn(id string) string {
	switch id {
	case PeriodWindow5h:
		return "usage_epoch_5h"
	case PeriodWindow1d:
		return "usage_epoch_1d"
	case PeriodWindow7d:
		return "usage_epoch_7d"
	case PeriodWindow30d:
		return "usage_epoch_30d"
	default:
		return ""
	}
}

// SumUserChargeAmountSince sums billing_charge.amount for a user since the given timestamp.
func (r *Repository) SumUserChargeAmountSince(ctx context.Context, userID uint, since time.Time) (float64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	return sumUserChargeAmountSinceDB(ctx, db, userID, since)
}

func sumUserChargeAmountSinceDB(ctx context.Context, db *gorm.DB, userID uint, since time.Time) (float64, error) {
	totals, errSum := sumUserPeriodWindowChargesDB(ctx, db, userID, map[string]*time.Time{
		PeriodWindow5h: timePtr(since.UTC()),
	})
	if errSum != nil {
		return 0, errSum
	}
	return totals[PeriodWindow5h], nil
}

// sumUserPeriodWindowChargesDB aggregates enabled windows in a single SQL scan.
// A nil since for a window contributes 0 without scanning extra rows for that bucket.
func sumUserPeriodWindowChargesDB(ctx context.Context, db *gorm.DB, userID uint, sinceByWindow map[string]*time.Time) (map[string]float64, error) {
	out := map[string]float64{
		PeriodWindow5h:  0,
		PeriodWindow1d:  0,
		PeriodWindow7d:  0,
		PeriodWindow30d: 0,
	}
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if userID == 0 {
		return nil, fmt.Errorf("user id is required")
	}

	farFuture := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	sinceOrFar := func(id string) time.Time {
		if sinceByWindow == nil || sinceByWindow[id] == nil {
			return farFuture
		}
		return sinceByWindow[id].UTC()
	}
	s5h := sinceOrFar(PeriodWindow5h)
	s1d := sinceOrFar(PeriodWindow1d)
	s7d := sinceOrFar(PeriodWindow7d)
	s30d := sinceOrFar(PeriodWindow30d)
	minSince := s5h
	for _, candidate := range []time.Time{s1d, s7d, s30d} {
		if candidate.Before(minSince) {
			minSince = candidate
		}
	}
	if !minSince.Before(farFuture) {
		return out, nil
	}

	var row struct {
		Sum5h  float64 `gorm:"column:sum_5h"`
		Sum1d  float64 `gorm:"column:sum_1d"`
		Sum7d  float64 `gorm:"column:sum_7d"`
		Sum30d float64 `gorm:"column:sum_30d"`
	}
	errScan := db.WithContext(contextOrBackground(ctx)).Raw(`
SELECT
	COALESCE(SUM(CASE WHEN "billing_charge"."created_at" >= ? THEN "billing_charge"."amount" ELSE 0 END), 0) AS sum_5h,
	COALESCE(SUM(CASE WHEN "billing_charge"."created_at" >= ? THEN "billing_charge"."amount" ELSE 0 END), 0) AS sum_1d,
	COALESCE(SUM(CASE WHEN "billing_charge"."created_at" >= ? THEN "billing_charge"."amount" ELSE 0 END), 0) AS sum_7d,
	COALESCE(SUM(CASE WHEN "billing_charge"."created_at" >= ? THEN "billing_charge"."amount" ELSE 0 END), 0) AS sum_30d
FROM "billing_charge"
WHERE "billing_charge"."user_id" = ?
	AND "billing_charge"."created_at" >= ?
`, s5h, s1d, s7d, s30d, userID, minSince).Scan(&row).Error
	if errScan != nil {
		return nil, errScan
	}
	out[PeriodWindow5h] = row.Sum5h
	out[PeriodWindow1d] = row.Sum1d
	out[PeriodWindow7d] = row.Sum7d
	out[PeriodWindow30d] = row.Sum30d
	return out, nil
}

type periodWindowEval struct {
	ID         string
	Enabled    bool
	Limit      *float64
	Mode       string
	Used       float64
	Active     bool
	Lower      *time.Time
	Upper      *time.Time
	ResetAt    *time.Time
	UsageEpoch *time.Time
	// NeedOpen is true when first_use should open on the next billable charge.
	NeedOpen  bool
	OpenStart time.Time
	Exceeded  bool
}

func evaluateUserPeriodWindows(ctx context.Context, db *gorm.DB, user *UserRecord, now time.Time) ([]periodWindowEval, error) {
	if user == nil {
		return nil, fmt.Errorf("user is nil")
	}
	defaultUserPeriodLimitFields(user)
	now = now.UTC()
	loc := loadUserLocation(user.Timezone)

	type windowDef struct {
		id           string
		limit        *float64
		allowCal     bool
		configured   string
		start        *time.Time
		epoch        *time.Time
		duration     time.Duration
		calendarSpan func() (lower, upper time.Time)
	}

	defs := []windowDef{
		{
			id: PeriodWindow5h, limit: user.Limit5hCredits, allowCal: false,
			configured: user.WindowMode5h, start: user.PeriodWindowStart5h, epoch: user.UsageEpoch5h,
			duration: periodLimit5hDuration,
		},
		{
			id: PeriodWindow1d, limit: user.Limit1dCredits, allowCal: true,
			configured: user.WindowMode1d, start: user.PeriodWindowStart1d, epoch: user.UsageEpoch1d,
			duration: periodLimit1dDuration,
			calendarSpan: func() (time.Time, time.Time) {
				lower := startOfDayInLocation(now, loc)
				return lower, lower.AddDate(0, 0, 1)
			},
		},
		{
			id: PeriodWindow7d, limit: user.Limit7dCredits, allowCal: true,
			configured: user.WindowMode7d, start: user.PeriodWindowStart7d, epoch: user.UsageEpoch7d,
			duration: periodLimit7dDuration,
			calendarSpan: func() (time.Time, time.Time) {
				lower := startOfCalendarWeek(now, loc, user.WeekResetDay, user.WeekResetHour)
				return lower, lower.AddDate(0, 0, 7)
			},
		},
		{
			id: PeriodWindow30d, limit: user.Limit30dCredits, allowCal: true,
			configured: user.WindowMode30d, start: user.PeriodWindowStart30d, epoch: user.UsageEpoch30d,
			duration: periodLimit30dDuration,
			calendarSpan: func() (time.Time, time.Time) {
				lower := startOfMonthInLocation(now, loc)
				return lower, lower.AddDate(0, 1, 0)
			},
		},
	}

	out := make([]periodWindowEval, 0, len(defs))
	sinceByWindow := make(map[string]*time.Time, len(defs))

	for _, def := range defs {
		mode, errMode := normalizePeriodWindowMode(def.configured, def.allowCal)
		if errMode != nil {
			// Invalid stored mode fails closed to first_use rather than skipping enforcement.
			mode = DefaultPeriodWindowMode
		}
		eval := periodWindowEval{
			ID:         def.id,
			Enabled:    def.limit != nil,
			Limit:      def.limit,
			Mode:       mode,
			UsageEpoch: cloneTimePtr(def.epoch),
		}

		switch mode {
		case PeriodWindowModeSliding:
			lower := now.Add(-def.duration)
			eval.Active = true
			eval.Lower = timePtr(lower)
			eval.Upper = timePtr(now)
			// Sliding has no discrete reset instant; capacity recovers continuously.
		case PeriodWindowModeCalendar:
			if def.calendarSpan == nil {
				return nil, fmt.Errorf("calendar mode is not supported for window %s", def.id)
			}
			lower, upper := def.calendarSpan()
			eval.Active = true
			eval.Lower = timePtr(lower)
			eval.Upper = timePtr(upper)
			eval.ResetAt = timePtr(upper)
		default: // first_use
			start := def.start
			if start == nil || !now.Before(start.UTC().Add(def.duration)) {
				// No active first_use window. used=0 until a billable charge opens one.
				eval.Active = false
				eval.NeedOpen = def.limit != nil
				eval.OpenStart = now
			} else {
				lower := start.UTC()
				upper := lower.Add(def.duration)
				eval.Active = true
				eval.Lower = &lower
				eval.Upper = &upper
				eval.ResetAt = &upper
			}
		}

		if eval.Lower != nil {
			since := eval.Lower.UTC()
			if def.epoch != nil && def.epoch.UTC().After(since) {
				since = def.epoch.UTC()
			}
			sinceCopy := since
			sinceByWindow[def.id] = &sinceCopy
		}

		out = append(out, eval)
	}

	totals, errSum := sumUserPeriodWindowChargesDB(ctx, db, user.ID, sinceByWindow)
	if errSum != nil {
		return nil, errSum
	}

	for i := range out {
		eval := &out[i]
		if sinceByWindow[eval.ID] != nil {
			eval.Used = totals[eval.ID]
		}
		if eval.Enabled && eval.Limit != nil {
			if eval.Used >= *eval.Limit {
				eval.Exceeded = true
			}
			// Explicit zero limit blocks even with no usage/window.
			if *eval.Limit == 0 {
				eval.Exceeded = true
			}
		}
	}
	return out, nil
}

func periodLimitExceededError(eval periodWindowEval) error {
	parts := []string{
		fmt.Sprintf("window=%s", eval.ID),
		fmt.Sprintf("mode=%s", eval.Mode),
		fmt.Sprintf("used=%g", eval.Used),
	}
	if eval.Limit != nil {
		parts = append(parts, fmt.Sprintf("limit=%g", *eval.Limit))
	}
	if eval.ResetAt != nil {
		parts = append(parts, fmt.Sprintf("reset_at=%s", eval.ResetAt.UTC().Format(time.RFC3339)))
	}
	return fmt.Errorf("%s: %s (%s)", homeerrors.TypeUserPeriodLimitExceeded, homeerrors.MessageUserPeriodLimitExceeded, strings.Join(parts, " "))
}

// BuildUserPeriodLimitsStatus computes used/remaining for management APIs.
func (r *Repository) BuildUserPeriodLimitsStatus(ctx context.Context, userID uint, now time.Time) (UserPeriodLimitsStatus, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UserPeriodLimitsStatus{}, errDB
	}
	user, errUser := r.GetUser(ctx, userID)
	if errUser != nil {
		return UserPeriodLimitsStatus{}, errUser
	}
	return buildUserPeriodLimitsStatusDB(ctx, db, user, now)
}

func buildUserPeriodLimitsStatusDB(ctx context.Context, db *gorm.DB, user *UserRecord, now time.Time) (UserPeriodLimitsStatus, error) {
	if user == nil {
		return UserPeriodLimitsStatus{}, fmt.Errorf("user is nil")
	}
	defaultUserPeriodLimitFields(user)
	evals, errEval := evaluateUserPeriodWindows(ctx, db, user, now)
	if errEval != nil {
		return UserPeriodLimitsStatus{}, errEval
	}
	status := UserPeriodLimitsStatus{
		UserID:           user.ID,
		Timezone:         user.Timezone,
		Credits:          user.Credits,
		CreditsUnlimited: user.CreditsUnlimited,
		Windows:          make([]UserPeriodWindowStatus, 0, len(evals)),
	}
	for _, eval := range evals {
		item := UserPeriodWindowStatus{
			ID:          eval.ID,
			Enabled:     eval.Enabled,
			Limit:       eval.Limit,
			Used:        eval.Used,
			Remaining:   remainingCredits(eval.Limit, eval.Used),
			Mode:        eval.Mode,
			Active:      eval.Active,
			WindowStart: cloneTimePtr(eval.Lower),
			WindowEnd:   cloneTimePtr(eval.Upper),
			ResetAt:     cloneTimePtr(eval.ResetAt),
			UsageEpoch:  cloneTimePtr(eval.UsageEpoch),
		}
		if eval.ID == PeriodWindow7d {
			day := user.WeekResetDay
			hour := user.WeekResetHour
			item.WeekResetDay = &day
			item.WeekResetHour = &hour
		}
		status.Windows = append(status.Windows, item)
	}
	return status, nil
}

// ResetUserPeriodLimits soft-resets period counters for the selected windows.
func (r *Repository) ResetUserPeriodLimits(ctx context.Context, userID uint, windows []string, mode string, now time.Time) (UserPeriodLimitResetResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UserPeriodLimitResetResult{}, errDB
	}
	if userID == 0 {
		return UserPeriodLimitResetResult{}, fmt.Errorf("user id is required")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = PeriodResetModeCounter
	}
	if mode != PeriodResetModeCounter && mode != PeriodResetModeWindowOnly {
		return UserPeriodLimitResetResult{}, periodLimitFieldError("mode", PeriodLimitErrInvalidResetMode, fmt.Sprintf("reset mode must be %q or %q", PeriodResetModeCounter, PeriodResetModeWindowOnly))
	}
	now = now.UTC()

	normalized, errWindows := normalizeResetWindows(windows)
	if errWindows != nil {
		return UserPeriodLimitResetResult{}, errWindows
	}

	var result UserPeriodLimitResetResult
	errTx := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		user := &UserRecord{}
		if errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", userID).First(user).Error; errFirst != nil {
			return errFirst
		}
		defaultUserPeriodLimitFields(user)

		target := normalized
		if len(target) == 0 {
			target = []string{PeriodWindow5h, PeriodWindow1d, PeriodWindow7d, PeriodWindow30d}
		}

		updates := map[string]any{}
		for _, id := range target {
			if col := windowStartColumn(id); col != "" {
				updates[col] = nil
			}
			windowMode, errMode := normalizePeriodWindowMode(userWindowMode(user, id), id != PeriodWindow5h)
			if errMode != nil {
				windowMode = DefaultPeriodWindowMode
			}
			// counter always moves usage_epoch.
			// window_only clears first_use starts; for sliding/calendar it also moves
			// usage_epoch so the reset actually zeroes used (starts alone are no-ops).
			needEpoch := mode == PeriodResetModeCounter ||
				windowMode == PeriodWindowModeSliding ||
				windowMode == PeriodWindowModeCalendar
			if needEpoch {
				if col := usageEpochColumn(id); col != "" {
					updates[col] = now
				}
			}
		}
		if len(updates) > 0 {
			if errUpdate := tx.Model(&UserRecord{}).Where("id = ?", userID).Updates(updates).Error; errUpdate != nil {
				return errUpdate
			}
		}
		// Reload into a zero-value record so database NULLs clear pointer fields
		// retained in the locked pre-update snapshot.
		user = &UserRecord{}
		if errReload := tx.Where("id = ?", userID).First(user).Error; errReload != nil {
			return errReload
		}
		status, errStatus := buildUserPeriodLimitsStatusDB(ctx, tx, user, now)
		if errStatus != nil {
			return errStatus
		}
		result = UserPeriodLimitResetResult{
			UserID:  userID,
			Mode:    mode,
			Windows: target,
			At:      now,
			Limits:  status,
		}
		return nil
	})
	if errTx != nil {
		return UserPeriodLimitResetResult{}, errTx
	}
	return result, nil
}

func normalizeResetWindows(windows []string) ([]string, error) {
	if len(windows) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(windows))
	out := make([]string, 0, len(windows))
	for _, raw := range windows {
		id := strings.ToLower(strings.TrimSpace(raw))
		switch id {
		case PeriodWindow5h, PeriodWindow1d, PeriodWindow7d, PeriodWindow30d:
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		case "":
			continue
		default:
			return nil, periodLimitFieldError("windows", PeriodLimitErrInvalidResetWindows, fmt.Sprintf("unknown period window %q", raw))
		}
	}
	return out, nil
}

// ensureAPIKeyUserBillingAllowed enforces credits and period limits for a user-owned API key.
// first_use windows open on the first billable charge, not on dispatch probes.
func ensureAPIKeyUserBillingAllowed(ctx context.Context, db *gorm.DB, record *APIKeyRecord) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if record == nil {
		return fmt.Errorf("api key record is nil")
	}
	userID := normalizeOptionalUserID(record.UserID)
	if userID == nil {
		return nil
	}

	user := UserRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).
		Where("id = ?", *userID).
		First(&user).Error
	if errFirst != nil {
		return errFirst
	}
	defaultUserPeriodLimitFields(&user)
	if !user.CreditsUnlimited && user.Credits <= 0 {
		return fmt.Errorf("%s: %s", homeerrors.TypeUserCreditsInsufficient, homeerrors.MessageUserCreditsInsufficient)
	}
	if !userPeriodLimitsEnabled(&user) {
		return nil
	}

	now := time.Now().UTC()
	evals, errEval := evaluateUserPeriodWindows(ctx, db, &user, now)
	if errEval != nil {
		return errEval
	}
	for _, eval := range evals {
		if eval.Exceeded {
			return periodLimitExceededError(eval)
		}
	}
	return nil
}

func userPeriodLimitsEnabled(user *UserRecord) bool {
	if user == nil {
		return false
	}
	return user.Limit5hCredits != nil ||
		user.Limit1dCredits != nil ||
		user.Limit7dCredits != nil ||
		user.Limit30dCredits != nil
}

// openUserFirstUseWindowsOnChargeTx opens inactive first_use windows when a billable charge lands.
// chargeTime becomes the window start so the charge itself is included in used.
func openUserFirstUseWindowsOnChargeTx(ctx context.Context, tx *gorm.DB, userID uint, chargeTime time.Time) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if userID == 0 {
		return fmt.Errorf("user id is required")
	}
	chargeTime = chargeTime.UTC()

	user := UserRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).
		First(&user).Error
	if errFirst != nil {
		return errFirst
	}
	defaultUserPeriodLimitFields(&user)

	evals, errEval := evaluateUserPeriodWindows(ctx, tx, &user, chargeTime)
	if errEval != nil {
		return errEval
	}

	updates := map[string]any{}
	for _, eval := range evals {
		if eval.Mode != PeriodWindowModeFirstUse || !eval.NeedOpen {
			continue
		}
		if col := windowStartColumn(eval.ID); col != "" {
			// Bound the session by the charge timestamp so this charge counts inside the window.
			updates[col] = chargeTime
		}
	}
	if len(updates) == 0 {
		return nil
	}
	return tx.WithContext(contextOrBackground(ctx)).
		Model(&UserRecord{}).
		Where("id = ?", userID).
		Updates(updates).Error
}
