package userapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gorm.io/gorm"
)

func TestNormalizeUserEmailAndMask(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "trim and lowercase", raw: "  Alice@EXAMPLE.com  ", want: "alice@example.com"},
		{name: "idna domain", raw: "User@例子.测试", want: "user@xn--fsqu00a.xn--0zwm56d"},
		{name: "display name", raw: "Alice <alice@example.com>", wantErr: true},
		{name: "multiple", raw: "alice@example.com,bob@example.com", wantErr: true},
		{name: "header break", raw: "alice@example.com\r\nBcc: bob@example.com", wantErr: true},
		{name: "empty", raw: " ", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, errNormalize := normalizeUserEmail(test.raw)
			if test.wantErr {
				if errNormalize == nil {
					t.Fatalf("normalizeUserEmail(%q) = %q, want error", test.raw, got)
				}
				return
			}
			if errNormalize != nil || got != test.want {
				t.Fatalf("normalizeUserEmail(%q) = %q, %v; want %q", test.raw, got, errNormalize, test.want)
			}
		})
	}
	if got := maskUserEmail("alice@example.com"); got != "a***@example.com" {
		t.Fatalf("maskUserEmail() = %q", got)
	}
}

func TestRespondErrorDoesNotExposeInternalFailureDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	respondError(ginContext, http.StatusInternalServerError, "user_load_failed", errors.New("database password=secret failed"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), "password=secret") || !strings.Contains(recorder.Body.String(), "internal server error") {
		t.Fatalf("internal error response = %s", recorder.Body.String())
	}
}

func TestForgotPasswordResponsesDoNotEnumerateAccounts(t *testing.T) {
	handler, router, db := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	unverifiedEmail := "unverified@example.com"
	unverifiedName := "unverified"
	if _, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &unverifiedName, Email: &unverifiedEmail}); errCreate != nil {
		t.Fatalf("CreateUser(unverified) error = %v", errCreate)
	}
	verifiedEmail := "verified@example.com"
	verifiedName := "verified"
	verified, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &verifiedName, Email: &verifiedEmail})
	if errCreate != nil {
		t.Fatalf("CreateUser(verified) error = %v", errCreate)
	}
	verifiedAt := time.Now().UTC()
	if errVerify := db.Model(&cluster.UserRecord{}).Where("id = ?", verified.ID).Update("email_verified_at", verifiedAt).Error; errVerify != nil {
		t.Fatalf("mark email verified: %v", errVerify)
	}

	requests := []map[string]any{
		{"email": "not-an-email"},
		{"email": "missing@example.com"},
		{"email": unverifiedEmail},
		{"email": verifiedEmail},
	}
	var wantBody string
	for _, request := range requests {
		response := performUserJSONRequest(t, router, http.MethodPost, "/user/password/forgot", request, "")
		if response.Code != http.StatusAccepted {
			t.Fatalf("forgot status = %d body=%s", response.Code, response.Body.String())
		}
		if wantBody == "" {
			wantBody = response.Body.String()
		} else if response.Body.String() != wantBody {
			t.Fatalf("forgot response differs: got %q want %q", response.Body.String(), wantBody)
		}
		if strings.Contains(response.Body.String(), verifiedEmail) || strings.Contains(response.Body.String(), unverifiedEmail) {
			t.Fatalf("forgot response leaked email: %s", response.Body.String())
		}
	}
	for attempt := 0; attempt < 4; attempt++ {
		response := performUserJSONRequest(t, router, http.MethodPost, "/user/password/forgot", map[string]any{"email": verifiedEmail}, "")
		if response.Code != http.StatusAccepted || response.Body.String() != wantBody {
			t.Fatalf("rate-limited forgot response = %d %q, want generic accepted", response.Code, response.Body.String())
		}
	}
	var jobs int64
	if errCount := db.Model(&cluster.UserMailJobRecord{}).Where("user_id = ? AND purpose = ?", verified.ID, cluster.UserSecurityTokenPurposePasswordReset).Count(&jobs).Error; errCount != nil {
		t.Fatalf("count recovery jobs: %v", errCount)
	}
	if jobs == 0 {
		t.Fatal("eligible recovery request did not enqueue a mail job")
	}
}

func TestEmailVerificationResendCooldown(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	bearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	first := performUserJSONRequest(t, router, http.MethodPost, "/user/email/verification", map[string]any{}, bearer)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first verification request = %d %s", first.Code, first.Body.String())
	}
	second := performUserJSONRequest(t, router, http.MethodPost, "/user/email/verification", map[string]any{}, bearer)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second verification request = %d %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
}

func TestRegistrationVerificationUsesSharedRateLimits(t *testing.T) {
	_, router, db := newUserEmailTestHandler(t, nil)
	register := performUserJSONRequest(t, router, http.MethodPost, "/user/register", map[string]any{
		"username": "alice",
		"password": "password",
		"email":    "alice@example.com",
	}, "")
	if register.Code != http.StatusOK {
		t.Fatalf("register status = %d body=%s", register.Code, register.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	if errDecode := json.Unmarshal(register.Body.Bytes(), &login); errDecode != nil || login.Token == "" {
		t.Fatalf("decode registration token: token=%q error=%v", login.Token, errDecode)
	}
	resend := performUserJSONRequest(t, router, http.MethodPost, "/user/email/verification", map[string]any{}, login.Token)
	if resend.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate resend status = %d body=%s", resend.Code, resend.Body.String())
	}
	if got := resend.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	var jobs int64
	if errCount := db.Model(&cluster.UserMailJobRecord{}).Where("purpose = ?", cluster.UserSecurityTokenPurposeEmailVerification).Count(&jobs).Error; errCount != nil {
		t.Fatalf("count verification jobs: %v", errCount)
	}
	if jobs != 1 {
		t.Fatalf("verification jobs = %d, want 1", jobs)
	}
}

func TestRegistrationWithoutEmailIsRateLimited(t *testing.T) {
	_, router, _ := newUserEmailTestHandler(t, nil)
	for attempt := 0; attempt < registrationIPLimit; attempt++ {
		response := performUserJSONRequest(t, router, http.MethodPost, "/user/register", map[string]any{
			"username": "alice",
			"password": "password",
		}, "")
		if attempt == 0 && response.Code != http.StatusOK {
			t.Fatalf("initial registration status = %d body=%s", response.Code, response.Body.String())
		}
		if attempt > 0 && response.Code != http.StatusConflict {
			t.Fatalf("duplicate registration %d status = %d body=%s", attempt, response.Code, response.Body.String())
		}
	}
	limited := performUserJSONRequest(t, router, http.MethodPost, "/user/register", map[string]any{
		"username": "bob",
		"password": "password",
	}, "")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("limited registration status = %d body=%s", limited.Code, limited.Body.String())
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("limited registration omitted Retry-After")
	}
}

func TestRegistrationDoesNotRevealOrMailVerifiedEmailOwner(t *testing.T) {
	handler, router, db := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	email := "shared@example.com"
	ownerName := "owner"
	owner, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &ownerName, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser(owner) error = %v", errCreate)
	}
	verifiedAt := time.Now().UTC()
	if errVerify := db.Model(&cluster.UserRecord{}).Where("id = ?", owner.ID).Update("email_verified_at", verifiedAt).Error; errVerify != nil {
		t.Fatalf("mark owner email verified: %v", errVerify)
	}
	register := performUserJSONRequest(t, router, http.MethodPost, "/user/register", map[string]any{
		"username": "claimant",
		"password": "password",
		"email":    email,
	}, "")
	if register.Code != http.StatusOK {
		t.Fatalf("claimant registration status = %d body=%s", register.Code, register.Body.String())
	}
	var verificationJobs int64
	if errCount := db.Model(&cluster.UserMailJobRecord{}).Where("purpose = ?", cluster.UserSecurityTokenPurposeEmailVerification).Count(&verificationJobs).Error; errCount != nil {
		t.Fatalf("count verification jobs: %v", errCount)
	}
	if verificationJobs != 0 {
		t.Fatalf("verification jobs for owned address = %d, want 0", verificationJobs)
	}
	forgot := performUserJSONRequest(t, router, http.MethodPost, "/user/password/forgot", map[string]any{"email": email}, "")
	if forgot.Code != http.StatusAccepted {
		t.Fatalf("forgot-password status = %d body=%s", forgot.Code, forgot.Body.String())
	}
	var resetJobs int64
	if errCount := db.Model(&cluster.UserMailJobRecord{}).Where("user_id = ? AND purpose = ?", owner.ID, cluster.UserSecurityTokenPurposePasswordReset).Count(&resetJobs).Error; errCount != nil {
		t.Fatalf("count owner reset jobs: %v", errCount)
	}
	if resetJobs != 1 {
		t.Fatalf("verified owner reset jobs = %d, want 1", resetJobs)
	}
}

func TestClearEmailWorksWhenMailCapabilityIsDisabled(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, func(cfg *appconfig.Config) {
		cfg.UserEmail.Enabled = false
	})
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	bearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	response := performUserJSONRequest(t, router, http.MethodDelete, "/user/email", nil, bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("clear email status = %d body=%s", response.Code, response.Body.String())
	}
	updated, errLoad := handler.repo.GetUser(ctx, user.ID)
	if errLoad != nil {
		t.Fatalf("GetUser() error = %v", errLoad)
	}
	if updated.Email != nil || updated.EmailVerifiedAt != nil || updated.EmailVersion != user.EmailVersion+1 {
		t.Fatalf("cleared email state = %#v", updated)
	}
}

func TestClearEmailWithoutConfiguredAddressIsIdempotent(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	username := "alice"
	user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	bearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	for attempt := 0; attempt <= emailMutationUserLimit; attempt++ {
		response := performUserJSONRequest(t, router, http.MethodDelete, "/user/email", nil, bearer)
		if response.Code != http.StatusOK {
			t.Fatalf("idempotent clear %d status = %d body=%s", attempt, response.Code, response.Body.String())
		}
	}
}

func TestUpdateEmailWithSameAddressIsIdempotent(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	bearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	for attempt := 0; attempt <= emailMutationUserLimit; attempt++ {
		response := performUserJSONRequest(t, router, http.MethodPut, "/user/email", map[string]any{"email": " Alice@EXAMPLE.com "}, bearer)
		if response.Code != http.StatusOK {
			t.Fatalf("idempotent update %d status = %d body=%s", attempt, response.Code, response.Body.String())
		}
	}
	stored, errLoad := handler.repo.GetUser(ctx, user.ID)
	if errLoad != nil {
		t.Fatalf("GetUser() error = %v", errLoad)
	}
	if stored.EmailVersion != user.EmailVersion {
		t.Fatalf("email version = %d, want %d", stored.EmailVersion, user.EmailVersion)
	}
}

func TestVerifyEmailHidesVerifiedOwnershipConflict(t *testing.T) {
	handler, router, db := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	email := "shared@example.com"
	ownerName := "owner"
	owner, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &ownerName, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser(owner) error = %v", errCreate)
	}
	claimantName := "claimant"
	claimant, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &claimantName, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser(claimant) error = %v", errCreate)
	}
	verifiedAt := time.Now().UTC()
	if errVerify := db.Model(&cluster.UserRecord{}).Where("id = ?", owner.ID).Update("email_verified_at", verifiedAt).Error; errVerify != nil {
		t.Fatalf("mark owner email verified: %v", errVerify)
	}
	rawToken := "claimant-verification-token"
	if errStore := handler.repo.ReplaceUserSecurityToken(ctx, cluster.UserSecurityTokenRecord{
		UserID:       claimant.ID,
		Purpose:      cluster.UserSecurityTokenPurposeEmailVerification,
		TokenHash:    cluster.HashUserSecurityValue(rawToken),
		EmailVersion: claimant.EmailVersion,
		ExpiresAt:    verifiedAt.Add(time.Hour),
		CreatedAt:    verifiedAt,
	}); errStore != nil {
		t.Fatalf("store claimant verification token: %v", errStore)
	}
	response := performUserJSONRequest(t, router, http.MethodPost, "/user/email/verify", map[string]any{"token": rawToken}, "")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_or_expired_token") {
		t.Fatalf("ownership-conflict response = %d %s", response.Code, response.Body.String())
	}
	stored, errLoad := handler.repo.GetUser(ctx, claimant.ID)
	if errLoad != nil {
		t.Fatalf("GetUser(claimant) error = %v", errLoad)
	}
	if stored.EmailVerifiedAt != nil {
		t.Fatal("ownership-conflict verification unexpectedly succeeded")
	}
}

func TestUserAPIRejectsOversizedJSONBody(t *testing.T) {
	_, router, _ := newUserEmailTestHandler(t, nil)
	payload := []byte(`{"email":"` + strings.Repeat("a", int(maxUserJSONBodyBytes)) + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/user/password/forgot", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "request_too_large") {
		t.Fatalf("oversized request body = %s", response.Body.String())
	}
}

func TestRegistrationRejectsPasswordBeyondBcryptLimit(t *testing.T) {
	_, router, _ := newUserEmailTestHandler(t, nil)
	response := performUserJSONRequest(t, router, http.MethodPost, "/user/register", map[string]any{
		"username": "alice",
		"password": strings.Repeat("p", maxPasswordBytes+1),
	}, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("long password status = %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "invalid_password") {
		t.Fatalf("long password body = %s", response.Body.String())
	}
}

func TestChangePasswordRotatesSessionAndReturnsReplacement(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()
	username := "alice"
	oldPassword := "old-password"
	hashed, errHash := hashPassword(oldPassword)
	if errHash != nil {
		t.Fatalf("hashPassword() error = %v", errHash)
	}
	user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Password: &hashed})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	oldBearer := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	change := performUserJSONRequest(t, router, http.MethodPost, "/user/password", map[string]any{"new_password": "new-password"}, oldBearer)
	if change.Code != http.StatusOK {
		t.Fatalf("change password = %d %s", change.Code, change.Body.String())
	}
	var replacement struct {
		Token string `json:"token"`
	}
	if errDecode := json.Unmarshal(change.Body.Bytes(), &replacement); errDecode != nil || replacement.Token == "" {
		t.Fatalf("decode replacement session: token=%q error=%v", replacement.Token, errDecode)
	}
	oldSession := performUserJSONRequest(t, router, http.MethodGet, "/user/me", nil, oldBearer)
	if oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("old session status = %d body=%s", oldSession.Code, oldSession.Body.String())
	}
	newSession := performUserJSONRequest(t, router, http.MethodGet, "/user/me", nil, replacement.Token)
	if newSession.Code != http.StatusOK {
		t.Fatalf("replacement session status = %d body=%s", newSession.Code, newSession.Body.String())
	}
	updated, errLoad := handler.repo.GetUser(ctx, user.ID)
	if errLoad != nil {
		t.Fatalf("GetUser() error = %v", errLoad)
	}
	if updated.SessionVersion != user.SessionVersion+1 || !passwordMatches(updated.Password, "new-password") {
		t.Fatalf("updated session/password state = version %d", updated.SessionVersion)
	}
}

func newUserEmailTestHandler(t *testing.T, mutate func(*appconfig.Config)) (*Handler, *gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &appconfig.Config{
		AuthDir: t.TempDir(),
		UserEmail: appconfig.UserEmailConfig{
			Enabled:              true,
			PublicUserURL:        "http://127.0.0.1/user.html",
			FromAddress:          "no-reply@example.com",
			FromName:             "CLIProxyAPIHome",
			VerificationTokenTTL: "24h",
			ResetTokenTTL:        "30m",
			Sender: appconfig.UserEmailSenderConfig{
				Type: "smtp",
				SMTP: appconfig.UserEmailSMTPConfig{Host: "127.0.0.1", Port: 2525},
			},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	cfg.NormalizeUserEmailConfig()
	runtime, errRuntime := home.NewRuntime(cfg)
	if errRuntime != nil {
		t.Fatalf("home.NewRuntime() error = %v", errRuntime)
	}
	t.Cleanup(runtime.Stop)
	db, errOpen := cluster.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	handler := NewHandler(cluster.NewRepository(db), runtime)
	handler.forgotPasswordResponseFloor = 0
	router := gin.New()
	Register(router.Group("/user"), handler)
	return handler, router, db
}

func createUserTestBearerToken(t *testing.T, handler *Handler, userID uint, sessionVersion uint64) string {
	t.Helper()
	ctx := context.Background()
	if _, _, errKey := handler.repo.ClusterCAKeyPair(ctx); errKey != nil {
		t.Fatalf("ClusterCAKeyPair() error = %v", errKey)
	}
	token, _, errToken := handler.createBearerToken(ctx, userID, sessionVersion, time.Hour)
	if errToken != nil {
		t.Fatalf("createBearerToken() error = %v", errToken)
	}
	return token
}

func performUserJSONRequest(t *testing.T, router http.Handler, method string, path string, body any, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var errMarshal error
		payload, errMarshal = json.Marshal(body)
		if errMarshal != nil {
			t.Fatalf("marshal request body: %v", errMarshal)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
