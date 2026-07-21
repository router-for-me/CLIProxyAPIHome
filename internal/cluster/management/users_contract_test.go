package management

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type userManagementRecordResponse struct {
	ID                uint            `json:"id"`
	Username          string          `json:"username"`
	PasswordSet       bool            `json:"password_set"`
	Credits           float64         `json:"credits"`
	CreditsUnlimited  bool            `json:"credits_unlimited"`
	Timezone          string          `json:"timezone"`
	Limit5hCredits    *float64        `json:"limit_5h_credits"`
	WindowMode5h      string          `json:"window_mode_5h"`
	Limit1dCredits    *float64        `json:"limit_1d_credits"`
	WindowMode1d      string          `json:"window_mode_1d"`
	Limit7dCredits    *float64        `json:"limit_7d_credits"`
	WindowMode7d      string          `json:"window_mode_7d"`
	WeekResetDay      int             `json:"week_reset_day"`
	WeekResetHour     int             `json:"week_reset_hour"`
	Limit30dCredits   *float64        `json:"limit_30d_credits"`
	WindowMode30d     string          `json:"window_mode_30d"`
	MFA               json.RawMessage `json:"mfa"`
	Passkey           json.RawMessage `json:"passkey"`
	PeriodLimitsBrief struct {
		EnabledWindows   []string `json:"enabled_windows"`
		ZeroLimitWindows []string `json:"zero_limit_windows"`
	} `json:"period_limits_summary"`
}

type userManagementErrorResponse struct {
	Error       string           `json:"error"`
	Message     string           `json:"message"`
	FieldErrors []fieldErrorItem `json:"field_errors"`
}

type userPeriodLimitResetHTTPResponse struct {
	Status string `json:"status"`
	UserID uint   `json:"user_id"`
	Reset  struct {
		Mode    string    `json:"mode"`
		Windows []string  `json:"windows"`
		At      time.Time `json:"at"`
	} `json:"reset"`
	Limits cluster.UserPeriodLimitsStatus `json:"limits"`
}

func TestUserManagementHTTPCreateAndPatchPeriodLimits(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()

	createPayload := `{
		"username":"alice",
		"password":"secret-password",
		"credits":50,
		"credits_unlimited":true,
		"timezone":"America/New_York",
		"limit_5h_credits":0,
		"window_mode_5h":"sliding",
		"limit_1d_credits":12.5,
		"window_mode_1d":"calendar",
		"limit_7d_credits":25,
		"window_mode_7d":"calendar",
		"week_reset_day":7,
		"week_reset_hour":6,
		"limit_30d_credits":100,
		"window_mode_30d":"first_use",
		"mfa":{"enabled":true},
		"passkey":[{"id":"credential-1"}]
	}`
	createResp := performUserManagementRequest(t, engine, http.MethodPost, "/users", createPayload)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var createBody struct {
		User userManagementRecordResponse `json:"user"`
	}
	decodeUserManagementResponse(t, createResp, &createBody)
	assertUserManagementCreateRecord(t, createBody.User)

	patchPayload := `{"limit_5h_credits":null,"limit_1d_credits":0,"timezone":"Asia/Tokyo"}`
	patchResp := performUserManagementRequest(t, engine, http.MethodPatch, fmt.Sprintf("/users/%d", createBody.User.ID), patchPayload)
	if patchResp.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchResp.Code, patchResp.Body.String())
	}
	var patchBody struct {
		User userManagementRecordResponse `json:"user"`
	}
	decodeUserManagementResponse(t, patchResp, &patchBody)
	user := patchBody.User
	if user.Username != "alice" || !user.PasswordSet || user.Credits != 50 || !user.CreditsUnlimited {
		t.Fatalf("patch changed unrelated user fields: %+v", user)
	}
	if user.Timezone != "Asia/Tokyo" || user.Limit5hCredits != nil {
		t.Fatalf("patch timezone/5h = %q/%v", user.Timezone, user.Limit5hCredits)
	}
	if user.Limit1dCredits == nil || *user.Limit1dCredits != 0 {
		t.Fatalf("limit_1d_credits = %v, want enabled zero", user.Limit1dCredits)
	}
	if user.WindowMode5h != cluster.PeriodWindowModeSliding || user.WindowMode1d != cluster.PeriodWindowModeCalendar {
		t.Fatalf("patch changed window modes: 5h=%q 1d=%q", user.WindowMode5h, user.WindowMode1d)
	}
	assertUserManagementSecurityJSON(t, user)

	getResp := performUserManagementRequest(t, engine, http.MethodGet, fmt.Sprintf("/users/%d", user.ID), "")
	if getResp.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getResp.Code, getResp.Body.String())
	}
	var getBody struct {
		User userManagementRecordResponse `json:"user"`
	}
	decodeUserManagementResponse(t, getResp, &getBody)
	gotUser := getBody.User
	if gotUser.ID != user.ID || gotUser.Username != user.Username || gotUser.Timezone != user.Timezone || !gotUser.PasswordSet || !gotUser.CreditsUnlimited {
		t.Fatalf("get user = %+v, want patched user", gotUser)
	}
	if gotUser.Limit5hCredits != nil || gotUser.Limit1dCredits == nil || *gotUser.Limit1dCredits != 0 {
		t.Fatalf("get limits = 5h:%v 1d:%v", gotUser.Limit5hCredits, gotUser.Limit1dCredits)
	}
	assertStringSlice(t, "get enabled_windows", gotUser.PeriodLimitsBrief.EnabledWindows, []string{"1d", "7d", "30d"})
	assertStringSlice(t, "get zero_limit_windows", gotUser.PeriodLimitsBrief.ZeroLimitWindows, []string{"1d"})
	assertUserManagementSecurityJSON(t, gotUser)

	record, errGet := handler.repo.GetUser(context.Background(), user.ID)
	if errGet != nil {
		t.Fatalf("GetUser() error = %v", errGet)
	}
	if record.Password == "" || record.Credits != 50 || !record.CreditsUnlimited || len(record.MFA) == 0 || len(record.Passkey) == 0 {
		t.Fatalf("persisted unrelated fields were lost: %+v", record)
	}
	if record.Limit5hCredits != nil || record.Limit1dCredits == nil || *record.Limit1dCredits != 0 {
		t.Fatalf("persisted limits = 5h:%v 1d:%v", record.Limit5hCredits, record.Limit1dCredits)
	}

	listResp := performUserManagementRequest(t, engine, http.MethodGet, "/users", "")
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listResp.Code, listResp.Body.String())
	}
	var listBody struct {
		Users []userManagementRecordResponse `json:"users"`
	}
	decodeUserManagementResponse(t, listResp, &listBody)
	if len(listBody.Users) != 1 {
		t.Fatalf("users = %d, want 1", len(listBody.Users))
	}
	assertStringSlice(t, "enabled_windows", listBody.Users[0].PeriodLimitsBrief.EnabledWindows, []string{"1d", "7d", "30d"})
	assertStringSlice(t, "zero_limit_windows", listBody.Users[0].PeriodLimitsBrief.ZeroLimitWindows, []string{"1d"})
}

func TestUserManagementHTTPPeriodLimitStatusAndReset(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()

	createResp := performUserManagementRequest(t, engine, http.MethodPost, "/users", `{
		"username":"status-user",
		"credits":0,
		"credits_unlimited":true,
		"timezone":"Asia/Shanghai",
		"limit_5h_credits":10,
		"window_mode_5h":"first_use",
		"limit_1d_credits":12,
		"window_mode_1d":"sliding",
		"limit_7d_credits":20,
		"window_mode_7d":"calendar",
		"week_reset_day":1,
		"week_reset_hour":0,
		"limit_30d_credits":40,
		"window_mode_30d":"calendar"
	}`)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var createBody struct {
		User userManagementRecordResponse `json:"user"`
	}
	decodeUserManagementResponse(t, createResp, &createBody)
	userID := createBody.User.ID

	apiKey := "period-status-key"
	if _, errKey := handler.repo.CreateAPIKeyForUser(context.Background(), userID, cluster.APIKeyUserUpdate{APIKey: &apiKey}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, errPrice := handler.repo.CreateBillingModelPrice(context.Background(), cluster.BillingModelPriceUpdate{
		Provider: "openai", Model: "period-test-model", RequestPrice: 4, Enabled: true,
	}); errPrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errPrice)
	}
	appendUserManagementCharge(t, handler, apiKey, "period-request-1")

	statusResp := performUserManagementRequest(t, engine, http.MethodGet, fmt.Sprintf("/users/%d/period-limits", userID), "")
	if statusResp.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", statusResp.Code, statusResp.Body.String())
	}
	var status cluster.UserPeriodLimitsStatus
	decodeUserManagementResponse(t, statusResp, &status)
	if status.UserID != userID || !status.CreditsUnlimited || status.Timezone != "Asia/Shanghai" || len(status.Windows) != 4 {
		t.Fatalf("period status = %+v", status)
	}
	window5h := userManagementWindow(t, status, cluster.PeriodWindow5h)
	assertUserManagementWindowAmounts(t, window5h, 10, 4, 6)
	if !window5h.Active || window5h.WindowStart == nil || window5h.WindowEnd == nil || window5h.ResetAt == nil {
		t.Fatalf("5h window bounds = %+v", window5h)
	}
	window1d := userManagementWindow(t, status, cluster.PeriodWindow1d)
	assertUserManagementWindowAmounts(t, window1d, 12, 4, 8)
	if !window1d.Active || window1d.Mode != cluster.PeriodWindowModeSliding || window1d.WindowStart == nil || window1d.WindowEnd == nil || window1d.ResetAt != nil {
		t.Fatalf("1d sliding status = %+v", window1d)
	}
	window7d := userManagementWindow(t, status, cluster.PeriodWindow7d)
	assertUserManagementWindowAmounts(t, window7d, 20, 4, 16)
	if window7d.Mode != cluster.PeriodWindowModeCalendar || window7d.ResetAt == nil || window7d.WeekResetDay == nil || *window7d.WeekResetDay != 1 {
		t.Fatalf("7d calendar status = %+v", window7d)
	}
	window30d := userManagementWindow(t, status, cluster.PeriodWindow30d)
	assertUserManagementWindowAmounts(t, window30d, 40, 4, 36)
	if !window30d.Active || window30d.Mode != cluster.PeriodWindowModeCalendar || window30d.WindowStart == nil || window30d.WindowEnd == nil || window30d.ResetAt == nil {
		t.Fatalf("30d calendar status = %+v", window30d)
	}

	subsetResp := performUserManagementRequest(t, engine, http.MethodPost, fmt.Sprintf("/users/%d/period-limits/reset", userID), `{"windows":["5h"],"mode":"counter"}`)
	if subsetResp.Code != http.StatusOK {
		t.Fatalf("subset reset status = %d, body = %s", subsetResp.Code, subsetResp.Body.String())
	}
	var subset userPeriodLimitResetHTTPResponse
	decodeUserManagementResponse(t, subsetResp, &subset)
	assertStringSlice(t, "subset reset windows", subset.Reset.Windows, []string{"5h"})
	if subset.Status != "ok" || subset.Reset.Mode != cluster.PeriodResetModeCounter {
		t.Fatalf("subset reset response = %+v", subset)
	}
	if got := userManagementWindow(t, subset.Limits, cluster.PeriodWindow5h); got.Used != 0 || got.Active {
		t.Fatalf("5h after subset reset = %+v", got)
	}
	if got := userManagementWindow(t, subset.Limits, cluster.PeriodWindow1d); !floatEqual(got.Used, 4) {
		t.Fatalf("1d after subset reset = %+v", got)
	}

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{name: "empty body", payload: ""},
		{name: "omitted windows", payload: `{}`},
		{name: "empty windows", payload: `{"windows":[],"mode":"counter"}`},
		{name: "null windows", payload: `{"windows":null,"mode":"counter"}`},
	} {
		t.Run("reset all/"+tc.name, func(t *testing.T) {
			resetResp := performUserManagementRequest(t, engine, http.MethodPost, fmt.Sprintf("/users/%d/period-limits/reset", userID), tc.payload)
			if resetResp.Code != http.StatusOK {
				t.Fatalf("all reset status = %d, body = %s", resetResp.Code, resetResp.Body.String())
			}
			var reset userPeriodLimitResetHTTPResponse
			decodeUserManagementResponse(t, resetResp, &reset)
			assertStringSlice(t, "all reset windows", reset.Reset.Windows, []string{"5h", "1d", "7d", "30d"})
			if len(reset.Limits.Windows) != 4 {
				t.Fatalf("reset limits windows = %d, want 4", len(reset.Limits.Windows))
			}
			for _, window := range reset.Limits.Windows {
				if window.UsageEpoch == nil {
					t.Fatalf("window %s usage_epoch = nil after all-window counter reset", window.ID)
				}
			}
		})
	}

	appendUserManagementCharge(t, handler, apiKey, "period-request-2")
	windowOnlyResp := performUserManagementRequest(t, engine, http.MethodPost, fmt.Sprintf("/users/%d/period-limits/reset", userID), `{"windows":["5h"],"mode":"window_only"}`)
	if windowOnlyResp.Code != http.StatusOK {
		t.Fatalf("window_only status = %d, body = %s", windowOnlyResp.Code, windowOnlyResp.Body.String())
	}
	var windowOnly userPeriodLimitResetHTTPResponse
	decodeUserManagementResponse(t, windowOnlyResp, &windowOnly)
	if windowOnly.Reset.Mode != cluster.PeriodResetModeWindowOnly {
		t.Fatalf("window_only mode = %q", windowOnly.Reset.Mode)
	}
	if got := userManagementWindow(t, windowOnly.Limits, cluster.PeriodWindow5h); got.Active || got.Used != 0 {
		t.Fatalf("5h after window_only reset = %+v", got)
	}

	missingStatus := performUserManagementRequest(t, engine, http.MethodGet, "/users/999999/period-limits", "")
	if missingStatus.Code != http.StatusNotFound {
		t.Fatalf("missing status code = %d, body = %s", missingStatus.Code, missingStatus.Body.String())
	}
	missingReset := performUserManagementRequest(t, engine, http.MethodPost, "/users/999999/period-limits/reset", `{}`)
	if missingReset.Code != http.StatusNotFound {
		t.Fatalf("missing reset code = %d, body = %s", missingReset.Code, missingReset.Body.String())
	}
}

func TestUserManagementHTTPPeriodLimitFieldErrors(t *testing.T) {
	_, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()

	createResp := performUserManagementRequest(t, engine, http.MethodPost, "/users", `{"username":"validation-user"}`)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var createBody struct {
		User userManagementRecordResponse `json:"user"`
	}
	decodeUserManagementResponse(t, createResp, &createBody)
	userID := createBody.User.ID

	typeCases := []struct {
		name      string
		method    string
		path      string
		payload   string
		wantField string
		wantCode  string
	}{
		{name: "credits unlimited", method: http.MethodPost, path: "/users", payload: `{"username":"bad","credits_unlimited":"true"}`, wantField: "credits_unlimited", wantCode: userJSONFieldErrInvalidType},
		{name: "timezone", method: http.MethodPost, path: "/users", payload: `{"username":"bad","timezone":1}`, wantField: "timezone", wantCode: cluster.PeriodLimitErrInvalidTimezone},
		{name: "5h limit", method: http.MethodPatch, path: fmt.Sprintf("/users/%d", userID), payload: `{"limit_5h_credits":"5"}`, wantField: "limit_5h_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "5h mode", method: http.MethodPost, path: "/users", payload: `{"username":"bad","window_mode_5h":1}`, wantField: "window_mode_5h", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "1d limit", method: http.MethodPost, path: "/users", payload: `{"username":"bad","limit_1d_credits":"5"}`, wantField: "limit_1d_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "1d mode", method: http.MethodPost, path: "/users", payload: `{"username":"bad","window_mode_1d":1}`, wantField: "window_mode_1d", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "7d limit", method: http.MethodPost, path: "/users", payload: `{"username":"bad","limit_7d_credits":"5"}`, wantField: "limit_7d_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "7d mode", method: http.MethodPost, path: "/users", payload: `{"username":"bad","window_mode_7d":1}`, wantField: "window_mode_7d", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "week reset day", method: http.MethodPost, path: "/users", payload: `{"username":"bad","week_reset_day":"1"}`, wantField: "week_reset_day", wantCode: cluster.PeriodLimitErrInvalidWeekResetDay},
		{name: "week reset hour", method: http.MethodPost, path: "/users", payload: `{"username":"bad","week_reset_hour":"0"}`, wantField: "week_reset_hour", wantCode: cluster.PeriodLimitErrInvalidWeekResetHour},
		{name: "30d limit", method: http.MethodPost, path: "/users", payload: `{"username":"bad","limit_30d_credits":"5"}`, wantField: "limit_30d_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "30d mode", method: http.MethodPost, path: "/users", payload: `{"username":"bad","window_mode_30d":1}`, wantField: "window_mode_30d", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "reset windows", method: http.MethodPost, path: fmt.Sprintf("/users/%d/period-limits/reset", userID), payload: `{"windows":"5h"}`, wantField: "windows", wantCode: cluster.PeriodLimitErrInvalidResetWindows},
		{name: "reset windows element", method: http.MethodPost, path: fmt.Sprintf("/users/%d/period-limits/reset", userID), payload: `{"windows":["5h",1]}`, wantField: "windows", wantCode: cluster.PeriodLimitErrInvalidResetWindows},
		{name: "reset mode", method: http.MethodPost, path: fmt.Sprintf("/users/%d/period-limits/reset", userID), payload: `{"mode":1}`, wantField: "mode", wantCode: cluster.PeriodLimitErrInvalidResetMode},
	}
	for _, tc := range typeCases {
		t.Run("type/"+tc.name, func(t *testing.T) {
			assertUserManagementFieldError(t, engine, tc.method, tc.path, tc.payload, tc.wantField, tc.wantCode)
		})
	}

	semanticCases := []struct {
		name      string
		path      string
		payload   string
		wantField string
		wantCode  string
	}{
		{name: "timezone", path: "/users", payload: `{"username":"bad","timezone":"Mars/Olympus"}`, wantField: "timezone", wantCode: cluster.PeriodLimitErrInvalidTimezone},
		{name: "5h limit", path: "/users", payload: `{"username":"bad","limit_5h_credits":-1}`, wantField: "limit_5h_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "1d limit", path: "/users", payload: `{"username":"bad","limit_1d_credits":-1}`, wantField: "limit_1d_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "7d limit", path: "/users", payload: `{"username":"bad","limit_7d_credits":-1}`, wantField: "limit_7d_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "30d limit", path: "/users", payload: `{"username":"bad","limit_30d_credits":-1}`, wantField: "limit_30d_credits", wantCode: cluster.PeriodLimitErrInvalidLimit},
		{name: "5h mode", path: "/users", payload: `{"username":"bad","window_mode_5h":"calendar"}`, wantField: "window_mode_5h", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "1d mode", path: "/users", payload: `{"username":"bad","window_mode_1d":"bogus"}`, wantField: "window_mode_1d", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "7d mode", path: "/users", payload: `{"username":"bad","window_mode_7d":"bogus"}`, wantField: "window_mode_7d", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "30d mode", path: "/users", payload: `{"username":"bad","window_mode_30d":"bogus"}`, wantField: "window_mode_30d", wantCode: cluster.PeriodLimitErrInvalidWindowMode},
		{name: "week reset day", path: "/users", payload: `{"username":"bad","week_reset_day":0}`, wantField: "week_reset_day", wantCode: cluster.PeriodLimitErrInvalidWeekResetDay},
		{name: "week reset hour", path: "/users", payload: `{"username":"bad","week_reset_hour":24}`, wantField: "week_reset_hour", wantCode: cluster.PeriodLimitErrInvalidWeekResetHour},
		{name: "reset windows", path: fmt.Sprintf("/users/%d/period-limits/reset", userID), payload: `{"windows":["2h"],"mode":"counter"}`, wantField: "windows", wantCode: cluster.PeriodLimitErrInvalidResetWindows},
		{name: "reset mode", path: fmt.Sprintf("/users/%d/period-limits/reset", userID), payload: `{"windows":["5h"],"mode":"bogus"}`, wantField: "mode", wantCode: cluster.PeriodLimitErrInvalidResetMode},
	}
	for _, tc := range semanticCases {
		t.Run("semantic/"+tc.name, func(t *testing.T) {
			assertUserManagementFieldError(t, engine, http.MethodPost, tc.path, tc.payload, tc.wantField, tc.wantCode)
		})
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "create", path: "/users"},
		{name: "reset", path: fmt.Sprintf("/users/%d/period-limits/reset", userID)},
	} {
		t.Run("malformed/"+tc.name, func(t *testing.T) {
			malformedResp := performUserManagementRequest(t, engine, http.MethodPost, tc.path, `{"broken":`)
			if malformedResp.Code != http.StatusBadRequest {
				t.Fatalf("malformed status = %d, body = %s", malformedResp.Code, malformedResp.Body.String())
			}
			var malformed userManagementErrorResponse
			decodeUserManagementResponse(t, malformedResp, &malformed)
			if malformed.Error != "invalid body" || len(malformed.FieldErrors) != 0 {
				t.Fatalf("malformed response = %+v, want generic invalid body", malformed)
			}
		})
	}
}

func newUserManagementHTTPTestServer(t *testing.T) (*Handler, *gin.Engine, func()) {
	t.Helper()
	db, errOpen := cluster.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	handler := NewHandler(cluster.NewRepository(db), nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/users", handler.ListUsers)
	engine.POST("/users", handler.CreateUser)
	engine.GET("/users/:id", handler.GetUser)
	engine.PATCH("/users/:id", handler.UpdateUser)
	engine.GET("/users/:id/period-limits", handler.GetUserPeriodLimits)
	engine.POST("/users/:id/period-limits/reset", handler.ResetUserPeriodLimits)
	return handler, engine, closeRepo
}

func performUserManagementRequest(t *testing.T, engine *gin.Engine, method, path, payload string) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if payload == "" {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(payload)
	}
	req := httptest.NewRequest(method, path, body)
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func decodeUserManagementResponse(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if errDecode := json.Unmarshal(rec.Body.Bytes(), target); errDecode != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), errDecode)
	}
}

func assertUserManagementCreateRecord(t *testing.T, user userManagementRecordResponse) {
	t.Helper()
	if user.ID == 0 || user.Username != "alice" || !user.PasswordSet || user.Credits != 50 || !user.CreditsUnlimited {
		t.Fatalf("create user = %+v", user)
	}
	if user.Timezone != "America/New_York" || user.Limit5hCredits == nil || *user.Limit5hCredits != 0 {
		t.Fatalf("create timezone/5h = %q/%v", user.Timezone, user.Limit5hCredits)
	}
	if user.Limit1dCredits == nil || *user.Limit1dCredits != 12.5 || user.WindowMode1d != cluster.PeriodWindowModeCalendar {
		t.Fatalf("create 1d = limit:%v mode:%q", user.Limit1dCredits, user.WindowMode1d)
	}
	if user.Limit7dCredits == nil || *user.Limit7dCredits != 25 || user.WeekResetDay != 7 || user.WeekResetHour != 6 {
		t.Fatalf("create 7d = limit:%v day:%d hour:%d", user.Limit7dCredits, user.WeekResetDay, user.WeekResetHour)
	}
	if user.Limit30dCredits == nil || *user.Limit30dCredits != 100 || user.WindowMode30d != cluster.PeriodWindowModeFirstUse {
		t.Fatalf("create 30d = limit:%v mode:%q", user.Limit30dCredits, user.WindowMode30d)
	}
	assertStringSlice(t, "create enabled_windows", user.PeriodLimitsBrief.EnabledWindows, []string{"5h", "1d", "7d", "30d"})
	assertStringSlice(t, "create zero_limit_windows", user.PeriodLimitsBrief.ZeroLimitWindows, []string{"5h"})
	assertUserManagementSecurityJSON(t, user)
}

func assertUserManagementSecurityJSON(t *testing.T, user userManagementRecordResponse) {
	t.Helper()
	var mfa struct {
		Enabled bool `json:"enabled"`
	}
	if errMFA := json.Unmarshal(user.MFA, &mfa); errMFA != nil || !mfa.Enabled {
		t.Fatalf("mfa = %s, error = %v", string(user.MFA), errMFA)
	}
	var passkeys []struct {
		ID string `json:"id"`
	}
	if errPasskey := json.Unmarshal(user.Passkey, &passkeys); errPasskey != nil || len(passkeys) != 1 || passkeys[0].ID != "credential-1" {
		t.Fatalf("passkey = %s, error = %v", string(user.Passkey), errPasskey)
	}
}

func appendUserManagementCharge(t *testing.T, handler *Handler, apiKey, requestID string) {
	t.Helper()
	payload := fmt.Sprintf(`{"timestamp":%q,"provider":"openai","model":"period-test-model","api_key":%q,"request_id":%q,"tokens":{"input_tokens":1,"total_tokens":1}}`, time.Now().UTC().Format(time.RFC3339Nano), apiKey, requestID)
	if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
}

func userManagementWindow(t *testing.T, status cluster.UserPeriodLimitsStatus, id string) cluster.UserPeriodWindowStatus {
	t.Helper()
	for _, window := range status.Windows {
		if window.ID == id {
			return window
		}
	}
	t.Fatalf("window %q not found in %+v", id, status.Windows)
	return cluster.UserPeriodWindowStatus{}
}

func assertUserManagementWindowAmounts(t *testing.T, window cluster.UserPeriodWindowStatus, limit, used, remaining float64) {
	t.Helper()
	if !window.Enabled || window.Limit == nil || window.Remaining == nil || !floatEqual(*window.Limit, limit) || !floatEqual(window.Used, used) || !floatEqual(*window.Remaining, remaining) {
		t.Fatalf("window amounts = %+v, want limit=%g used=%g remaining=%g", window, limit, used, remaining)
	}
}

func floatEqual(left, right float64) bool {
	return math.Abs(left-right) < 1e-9
}

func assertUserManagementFieldError(t *testing.T, engine *gin.Engine, method, path, payload, wantField, wantCode string) {
	t.Helper()
	rec := performUserManagementRequest(t, engine, method, path, payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body userManagementErrorResponse
	decodeUserManagementResponse(t, rec, &body)
	if body.Error != "invalid body" || len(body.FieldErrors) != 1 {
		t.Fatalf("error response = %+v", body)
	}
	if body.FieldErrors[0].Field != wantField || body.FieldErrors[0].Code != wantCode {
		t.Fatalf("field error = %+v, want %s/%s", body.FieldErrors[0], wantField, wantCode)
	}
}

func assertStringSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}
