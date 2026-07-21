package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"golang.org/x/crypto/bcrypt"
)

func TestUserUpdateFromRequestHashesManagementPassword(t *testing.T) {
	t.Parallel()

	password := "plain-management-password"
	update, ok := userUpdateFromRequest(nil, userWriteRequest{Password: &password}, false)
	if !ok {
		t.Fatalf("userUpdateFromRequest() ok = false, want true")
	}
	if update.Password == nil {
		t.Fatalf("UserUpdate.Password = nil, want hash")
	}
	if *update.Password == password {
		t.Fatalf("UserUpdate.Password stored plaintext")
	}
	if errCompare := bcrypt.CompareHashAndPassword([]byte(*update.Password), []byte(password)); errCompare != nil {
		t.Fatalf("stored password is not a valid bcrypt hash: %v", errCompare)
	}
}

func TestUserUpdateFromRequestKeepsExistingBcryptPassword(t *testing.T) {
	t.Parallel()

	rawHash, errHash := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if errHash != nil {
		t.Fatalf("GenerateFromPassword() error = %v", errHash)
	}
	password := string(rawHash)
	update, ok := userUpdateFromRequest(nil, userWriteRequest{Password: &password}, false)
	if !ok {
		t.Fatalf("userUpdateFromRequest() ok = false, want true")
	}
	if update.Password == nil || *update.Password != password {
		t.Fatalf("UserUpdate.Password = %q, want existing hash", stringValue(update.Password))
	}
}

func TestUserRecordToMapDoesNotExposePassword(t *testing.T) {
	t.Parallel()

	item := userRecordToMap(&cluster.UserRecord{Password: "stored-secret"})
	if _, ok := item["password"]; ok {
		t.Fatalf("userRecordToMap exposed password")
	}
	if got := item["password_set"]; got != true {
		t.Fatalf("password_set = %v, want true", got)
	}

	empty := userRecordToMap(&cluster.UserRecord{})
	if got := empty["password_set"]; got != false {
		t.Fatalf("empty password_set = %v, want false", got)
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestOptionalJSONFloatNullAndValue(t *testing.T) {
	t.Parallel()

	var body userWriteRequest
	if err := json.Unmarshal([]byte(`{"limit_1d_credits":null,"limit_5h_credits":12.5}`), &body); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !body.Limit1d.set || !body.Limit1d.clear {
		t.Fatalf("limit_1d = %+v, want set+clear", body.Limit1d)
	}
	if !body.Limit5h.set || body.Limit5h.clear || body.Limit5h.value != 12.5 {
		t.Fatalf("limit_5h = %+v, want value 12.5", body.Limit5h)
	}
	update, ok := userUpdateFromRequest(nil, body, false)
	if !ok {
		t.Fatal("userUpdateFromRequest failed")
	}
	if !update.Limit1dCredits.Set || !update.Limit1dCredits.Clear {
		t.Fatalf("update.Limit1dCredits = %+v", update.Limit1dCredits)
	}
	if !update.Limit5hCredits.Set || update.Limit5hCredits.Clear || update.Limit5hCredits.Value != 12.5 {
		t.Fatalf("update.Limit5hCredits = %+v", update.Limit5hCredits)
	}
}

func TestUserRecordToMapIncludesPeriodLimitFields(t *testing.T) {
	t.Parallel()

	limit := 9.0
	item := userRecordToMap(&cluster.UserRecord{
		Username:         "alice",
		CreditsUnlimited: true,
		Limit5hCredits:   &limit,
		// Legacy alias "rolling" must surface as canonical "sliding".
		WindowMode1d: cluster.PeriodWindowModeRolling,
	})
	if item["limit_5h_credits"] == nil {
		t.Fatalf("limit_5h_credits missing: %#v", item)
	}
	if item["window_mode_1d"] != cluster.PeriodWindowModeSliding {
		t.Fatalf("window_mode_1d = %v, want sliding", item["window_mode_1d"])
	}
	if item["timezone"] != cluster.DefaultUserTimezone {
		t.Fatalf("timezone = %v", item["timezone"])
	}
	if item["credits_unlimited"] != true {
		t.Fatalf("credits_unlimited = %v, want true", item["credits_unlimited"])
	}
}

func TestUserRecordToMapIncludesPeriodLimitsSummary(t *testing.T) {
	t.Parallel()

	limit5h := 0.0
	limit1d := 25.5
	item := userRecordToMap(&cluster.UserRecord{
		Username:       "alice",
		Limit5hCredits: &limit5h,
		Limit1dCredits: &limit1d,
	})
	summary, ok := item["period_limits_summary"].(gin.H)
	if !ok {
		t.Fatalf("period_limits_summary missing or wrong type: %#v", item["period_limits_summary"])
	}
	enabled, ok := summary["enabled_windows"].([]string)
	if !ok || len(enabled) != 2 || enabled[0] != "5h" || enabled[1] != "1d" {
		t.Fatalf("enabled_windows = %#v, want [5h 1d]", summary["enabled_windows"])
	}
	zero, ok := summary["zero_limit_windows"].([]string)
	if !ok || len(zero) != 1 || zero[0] != "5h" {
		t.Fatalf("zero_limit_windows = %#v, want [5h]", summary["zero_limit_windows"])
	}

	empty := userRecordToMap(&cluster.UserRecord{Username: "bob"})["period_limits_summary"].(gin.H)
	if got := len(empty["enabled_windows"].([]string)); got != 0 {
		t.Fatalf("enabled_windows = %d entries, want 0", got)
	}
	if got := len(empty["zero_limit_windows"].([]string)); got != 0 {
		t.Fatalf("zero_limit_windows = %d entries, want 0", got)
	}
}

func TestRespondUserPeriodLimitValidationErrorWritesFieldErrors(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/users", nil)

	validationErr := cluster.PeriodLimitConfigError{
		Message: "timezone: invalid timezone \"Mars/Olympus\"",
		Field:   "timezone",
		Code:    cluster.PeriodLimitErrInvalidTimezone,
	}
	if !respondUserPeriodLimitValidationError(ginCtx, validationErr) {
		t.Fatal("respondUserPeriodLimitValidationError() = false, want true")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if payload["error"] != "invalid body" {
		t.Fatalf("error = %v, want invalid body", payload["error"])
	}
	fieldErrors, ok := payload["field_errors"].([]any)
	if !ok || len(fieldErrors) != 1 {
		t.Fatalf("field_errors = %#v, want one entry", payload["field_errors"])
	}
	entry, ok := fieldErrors[0].(map[string]any)
	if !ok {
		t.Fatalf("field_errors[0] = %#v, want object", fieldErrors[0])
	}
	if entry["field"] != "timezone" || entry["code"] != cluster.PeriodLimitErrInvalidTimezone {
		t.Fatalf("field_errors[0] = %#v", entry)
	}

	if respondUserPeriodLimitValidationError(ginCtx, errors.New("boom")) {
		t.Fatal("respondUserPeriodLimitValidationError() = true for non-validation error")
	}
}

func TestGetCapabilitiesDeclaresUserDomain(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/capabilities", nil)

	handler := &Handler{}
	handler.GetCapabilities(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	for _, key := range []string{"users", "access_groups", "user_period_limits"} {
		if !payload.Capabilities[key] {
			t.Fatalf("capabilities[%q] missing or false", key)
		}
	}
}
