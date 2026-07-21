package cluster

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUserEmailUniqueIndexSQLiteClaimsOnlyVerifiedAddresses(t *testing.T) {
	repo, db := newUserSecurityTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	email := "alice@example.com"
	firstName := "alice"
	first, errCreateFirst := repo.CreateUser(ctx, UserUpdate{Username: &firstName, Email: &email})
	if errCreateFirst != nil {
		t.Fatalf("CreateUser(first) error = %v", errCreateFirst)
	}

	secondName := "bob"
	second, errCreateSecond := repo.CreateUser(ctx, UserUpdate{Username: &secondName, Email: &email})
	if errCreateSecond != nil {
		t.Fatalf("CreateUser(unverified duplicate email) error = %v", errCreateSecond)
	}
	firstToken := HashUserSecurityValue("first-verification-token")
	if errStore := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       first.ID,
		Purpose:      UserSecurityTokenPurposeEmailVerification,
		TokenHash:    firstToken,
		EmailVersion: first.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errStore != nil {
		t.Fatalf("store first verification token: %v", errStore)
	}
	if _, errVerify := repo.ConsumeUserEmailVerificationToken(ctx, firstToken, now.Add(time.Second)); errVerify != nil {
		t.Fatalf("verify first email: %v", errVerify)
	}
	owner, errOwner := repo.GetUserByEmail(ctx, email)
	if errOwner != nil || owner.ID != first.ID {
		t.Fatalf("verified email owner = %#v, %v; want user %d", owner, errOwner, first.ID)
	}
	secondToken := HashUserSecurityValue("second-verification-token")
	if errStore := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       second.ID,
		Purpose:      UserSecurityTokenPurposeEmailVerification,
		TokenHash:    secondToken,
		EmailVersion: second.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errStore != nil {
		t.Fatalf("store second verification token: %v", errStore)
	}
	if _, errVerify := repo.ConsumeUserEmailVerificationToken(ctx, secondToken, now.Add(2*time.Second)); !IsUserEmailConflictError(errVerify) {
		t.Fatalf("verify duplicate email error = %v, want email conflict", errVerify)
	}
	if errDelete := repo.DeleteUser(ctx, first.ID); errDelete != nil {
		t.Fatalf("DeleteUser() error = %v", errDelete)
	}
	if _, errVerify := repo.ConsumeUserEmailVerificationToken(ctx, secondToken, now.Add(3*time.Second)); errVerify != nil {
		t.Fatalf("verify email after owner deletion: %v", errVerify)
	}
	owner, errOwner = repo.GetUserByEmail(ctx, email)
	if errOwner != nil || owner.ID != second.ID {
		t.Fatalf("reassigned verified email owner = %#v, %v; want user %d", owner, errOwner, second.ID)
	}

	var definition string
	if errIndex := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", "idx_user_email_verified_active_unique").Scan(&definition).Error; errIndex != nil {
		t.Fatalf("load unique email index: %v", errIndex)
	}
	lowerDefinition := strings.ToLower(definition)
	if !strings.Contains(lowerDefinition, "where") || !strings.Contains(lowerDefinition, "deleted_at") || !strings.Contains(lowerDefinition, "email_verified_at") {
		t.Fatalf("unique email index definition = %q, want partial verified active-user index", definition)
	}
}

func TestUserEmailUniqueIndexPostgresMigrationSQL(t *testing.T) {
	var logs bytes.Buffer
	db, errOpen := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 user=cliproxy dbname=cliproxy_home sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		DryRun:               true,
		Logger: logger.New(log.New(&logs, "", 0), logger.Config{
			LogLevel: logger.Info,
		}),
	})
	if errOpen != nil {
		t.Fatalf("open postgres dry-run db: %v", errOpen)
	}
	if errMigrate := migrateUserUniqueEmail(db); errMigrate != nil {
		t.Fatalf("migrateUserUniqueEmail() error = %v", errMigrate)
	}
	output := strings.ToLower(logs.String())
	for _, fragment := range []string{"drop index", "idx_user_email_active_unique", "create unique index", "idx_user_email_verified_active_unique", "deleted_at", "email_verified_at", "is not null"} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("postgres migration SQL = %q, missing %q", output, fragment)
		}
	}
}

func TestUserEmailIdempotentUpdatePreservesVerification(t *testing.T) {
	repo, _ := newUserSecurityTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	rawToken := "verification-token"
	if errToken := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      UserSecurityTokenPurposeEmailVerification,
		TokenHash:    HashUserSecurityValue(rawToken),
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errToken != nil {
		t.Fatalf("ReplaceUserSecurityToken() error = %v", errToken)
	}
	verified, errVerify := repo.ConsumeUserEmailVerificationToken(ctx, HashUserSecurityValue(rawToken), now.Add(time.Second))
	if errVerify != nil {
		t.Fatalf("ConsumeUserEmailVerificationToken() error = %v", errVerify)
	}
	if verified.EmailVerifiedAt == nil {
		t.Fatal("verified email timestamp is nil")
	}

	updated, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Email: &email})
	if errUpdate != nil {
		t.Fatalf("UpdateUser(same email) error = %v", errUpdate)
	}
	if updated.EmailVersion != verified.EmailVersion {
		t.Fatalf("email version = %d, want unchanged %d", updated.EmailVersion, verified.EmailVersion)
	}
	if updated.EmailVerifiedAt == nil {
		t.Fatal("same-address update cleared verification")
	}
}

func TestUserSecurityTokenSingleUseAndPasswordResetPreservesMFA(t *testing.T) {
	repo, db := newUserSecurityTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	username := "alice"
	email := "alice@example.com"
	oldPassword := "old-password-hash"
	mfa := JSONB(`{"totp":{"secret":"secret"}}`)
	passkey := JSONB(`[{"id":"credential"}]`)
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &username, Password: &oldPassword, Email: &email, MFA: &mfa, Passkey: &passkey})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}

	verifyRaw := "verify-raw-token"
	verifyHash := HashUserSecurityValue(verifyRaw)
	if errStore := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      UserSecurityTokenPurposeEmailVerification,
		TokenHash:    verifyHash,
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errStore != nil {
		t.Fatalf("store verification token: %v", errStore)
	}
	var storedVerify UserSecurityTokenRecord
	if errLoad := db.Where("token_hash = ?", verifyHash).First(&storedVerify).Error; errLoad != nil {
		t.Fatalf("load verification token: %v", errLoad)
	}
	if storedVerify.TokenHash == verifyRaw || strings.Contains(storedVerify.TokenHash, verifyRaw) {
		t.Fatal("database contains raw verification token")
	}
	if _, errVerify := repo.ConsumeUserEmailVerificationToken(ctx, verifyHash, now.Add(time.Second)); errVerify != nil {
		t.Fatalf("consume verification token: %v", errVerify)
	}
	if _, errReuse := repo.ConsumeUserEmailVerificationToken(ctx, verifyHash, now.Add(2*time.Second)); !errors.Is(errReuse, ErrUserSecurityTokenInvalid) {
		t.Fatalf("reuse verification token error = %v, want invalid token", errReuse)
	}

	resetRaw := "reset-raw-token"
	resetHash := HashUserSecurityValue(resetRaw)
	if errStore := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      UserSecurityTokenPurposePasswordReset,
		TokenHash:    resetHash,
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errStore != nil {
		t.Fatalf("store reset token: %v", errStore)
	}
	newPassword := "new-password-hash"
	resetUser, errReset := repo.ConsumeUserPasswordResetToken(ctx, resetHash, newPassword, now.Add(3*time.Second))
	if errReset != nil {
		t.Fatalf("ConsumeUserPasswordResetToken() error = %v", errReset)
	}
	if resetUser.Password != newPassword || resetUser.SessionVersion != 1 {
		t.Fatalf("reset user password/session = %q/%d", resetUser.Password, resetUser.SessionVersion)
	}
	if string(resetUser.MFA) != string(mfa) || string(resetUser.Passkey) != string(passkey) {
		t.Fatalf("password reset changed MFA/passkey: mfa=%s passkey=%s", resetUser.MFA, resetUser.Passkey)
	}
	if _, errReuse := repo.ConsumeUserPasswordResetToken(ctx, resetHash, "another", now.Add(4*time.Second)); !errors.Is(errReuse, ErrUserSecurityTokenInvalid) {
		t.Fatalf("reuse reset token error = %v, want invalid token", errReuse)
	}
}

func TestUserMailJobSupersedeInvalidatesTokenAndOlderClaim(t *testing.T) {
	repo, db := newUserSecurityTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	oldHash := HashUserSecurityValue("old-token")
	if errStore := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      UserSecurityTokenPurposeEmailVerification,
		TokenHash:    oldHash,
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errStore != nil {
		t.Fatalf("store old token: %v", errStore)
	}
	firstJob, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, UserSecurityTokenPurposeEmailVerification, user.EmailVersion, now.Add(time.Second))
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob(first) error = %v", errEnqueue)
	}
	var invalidated UserSecurityTokenRecord
	if errLoad := db.Where("token_hash = ?", oldHash).First(&invalidated).Error; errLoad != nil {
		t.Fatalf("load invalidated token: %v", errLoad)
	}
	if invalidated.UsedAt == nil {
		t.Fatal("enqueue did not invalidate outstanding token")
	}
	claimed, errClaim := repo.ClaimUserMailJob(ctx, "worker-old", now.Add(time.Second), time.Minute)
	if errClaim != nil || claimed == nil || claimed.ID != firstJob.ID {
		t.Fatalf("ClaimUserMailJob() = %#v, %v", claimed, errClaim)
	}
	if _, errEnqueue = repo.EnqueueUserMailJob(ctx, user.ID, UserSecurityTokenPurposeEmailVerification, user.EmailVersion, now.Add(2*time.Second)); errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob(second) error = %v", errEnqueue)
	}
	errReplace := repo.ReplaceUserSecurityTokenForMailJob(ctx, claimed.ID, "worker-old", UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      UserSecurityTokenPurposeEmailVerification,
		TokenHash:    HashUserSecurityValue("stale-worker-token"),
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now.Add(3 * time.Second),
	})
	if !errors.Is(errReplace, ErrUserMailJobClaimLost) {
		t.Fatalf("stale worker token replacement error = %v, want lost claim", errReplace)
	}
	if errComplete := repo.CompleteUserMailJob(ctx, claimed.ID, "worker-old", now.Add(3*time.Second)); !errors.Is(errComplete, ErrUserMailJobClaimLost) {
		t.Fatalf("stale worker completion error = %v, want lost claim", errComplete)
	}
}

func TestUserSecurityThrottleUsesAnchoredWindow(t *testing.T) {
	repo, _ := newUserSecurityTestRepository(t)
	ctx := context.Background()
	start := time.Date(2026, time.July, 21, 12, 0, 59, 0, time.UTC)
	scope := HashUserSecurityValue("user:1")

	allowed, retryAfter, errAllow := repo.AllowUserSecurityActionWithRetry(ctx, "verify_email_cooldown", scope, 1, time.Minute, start)
	if errAllow != nil || !allowed || retryAfter != 0 {
		t.Fatalf("first throttle decision = %v, %v, %v", allowed, retryAfter, errAllow)
	}
	allowed, retryAfter, errAllow = repo.AllowUserSecurityActionWithRetry(ctx, "verify_email_cooldown", scope, 1, time.Minute, start.Add(time.Second))
	if errAllow != nil || allowed || retryAfter != 59*time.Second {
		t.Fatalf("cross-minute throttle decision = %v, %v, %v", allowed, retryAfter, errAllow)
	}
	allowed, retryAfter, errAllow = repo.AllowUserSecurityActionWithRetry(ctx, "verify_email_cooldown", scope, 1, time.Minute, start.Add(time.Minute))
	if errAllow != nil || !allowed || retryAfter != 0 {
		t.Fatalf("expired throttle decision = %v, %v, %v", allowed, retryAfter, errAllow)
	}
}

func TestPasswordUpdateInvalidatesResetTokensAndQueuedMail(t *testing.T) {
	repo, db := newUserSecurityTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	username := "alice"
	email := "alice@example.com"
	oldPassword := "old-password-hash"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &username, Password: &oldPassword, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	verifiedAt := now
	if errVerify := db.Model(&UserRecord{}).Where("id = ?", user.ID).Update("email_verified_at", verifiedAt).Error; errVerify != nil {
		t.Fatalf("mark email verified: %v", errVerify)
	}
	job, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, UserSecurityTokenPurposePasswordReset, user.EmailVersion, now)
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	if job.SessionVersion != user.SessionVersion {
		t.Fatalf("mail job session version = %d, want %d", job.SessionVersion, user.SessionVersion)
	}
	tokenHash := HashUserSecurityValue("reset-before-password-change")
	if errStore := repo.ReplaceUserSecurityToken(ctx, UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      UserSecurityTokenPurposePasswordReset,
		TokenHash:    tokenHash,
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
	}); errStore != nil {
		t.Fatalf("ReplaceUserSecurityToken() error = %v", errStore)
	}

	newPassword := "new-password-hash"
	updated, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Password: &newPassword})
	if errUpdate != nil {
		t.Fatalf("UpdateUser(password) error = %v", errUpdate)
	}
	if updated.SessionVersion != user.SessionVersion+1 {
		t.Fatalf("session version = %d, want %d", updated.SessionVersion, user.SessionVersion+1)
	}
	var storedJob UserMailJobRecord
	if errLoad := db.Where("id = ?", job.ID).First(&storedJob).Error; errLoad != nil {
		t.Fatalf("load mail job: %v", errLoad)
	}
	if storedJob.Status != UserMailJobStatusSuperseded {
		t.Fatalf("mail job status = %q, want superseded", storedJob.Status)
	}
	var storedToken UserSecurityTokenRecord
	if errLoad := db.Where("token_hash = ?", tokenHash).First(&storedToken).Error; errLoad != nil {
		t.Fatalf("load reset token: %v", errLoad)
	}
	if storedToken.UsedAt == nil {
		t.Fatal("password update left reset token active")
	}
}

func TestPurgeExpiredUserSecurityRemovesAbandonedAndPreservesActiveMailJobs(t *testing.T) {
	repo, db := newUserSecurityTestRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	job, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, UserSecurityTokenPurposeEmailVerification, user.EmailVersion, now.Add(-8*24*time.Hour))
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	activeClaimExpiry := now.Add(time.Minute)
	activeJob := UserMailJobRecord{
		UserID:         user.ID,
		Purpose:        UserSecurityTokenPurposePasswordReset,
		EmailVersion:   user.EmailVersion,
		Status:         UserMailJobStatusSending,
		Attempts:       1,
		NextAttemptAt:  now.Add(-8 * 24 * time.Hour),
		ClaimedBy:      "active-worker",
		ClaimExpiresAt: &activeClaimExpiry,
		CreatedAt:      now.Add(-8 * 24 * time.Hour),
		UpdatedAt:      now,
	}
	if errCreate := db.Create(&activeJob).Error; errCreate != nil {
		t.Fatalf("create active old mail job: %v", errCreate)
	}
	expiredClaimExpiry := now.Add(-time.Minute)
	expiredJob := UserMailJobRecord{
		UserID:         user.ID,
		Purpose:        UserSecurityTokenPurposePasswordReset,
		EmailVersion:   user.EmailVersion,
		Status:         UserMailJobStatusSending,
		Attempts:       1,
		NextAttemptAt:  now.Add(-8 * 24 * time.Hour),
		ClaimedBy:      "expired-worker",
		ClaimExpiresAt: &expiredClaimExpiry,
		CreatedAt:      now.Add(-8 * 24 * time.Hour),
		UpdatedAt:      now.Add(-8 * 24 * time.Hour),
	}
	if errCreate := db.Create(&expiredJob).Error; errCreate != nil {
		t.Fatalf("create expired old mail job: %v", errCreate)
	}
	if errPurge := repo.PurgeExpiredUserSecurity(ctx, now, 7*24*time.Hour); errPurge != nil {
		t.Fatalf("PurgeExpiredUserSecurity() error = %v", errPurge)
	}
	var count int64
	if errCount := db.Model(&UserMailJobRecord{}).Where("id = ?", job.ID).Count(&count).Error; errCount != nil {
		t.Fatalf("count stale mail job: %v", errCount)
	}
	if count != 0 {
		t.Fatalf("stale active mail jobs = %d, want 0", count)
	}
	if errCount := db.Model(&UserMailJobRecord{}).Where("id = ?", activeJob.ID).Count(&count).Error; errCount != nil {
		t.Fatalf("count actively claimed old mail job: %v", errCount)
	}
	if count != 1 {
		t.Fatalf("actively claimed old mail jobs = %d, want 1", count)
	}
	if errCount := db.Model(&UserMailJobRecord{}).Where("id = ?", expiredJob.ID).Count(&count).Error; errCount != nil {
		t.Fatalf("count expired claimed old mail job: %v", errCount)
	}
	if count != 0 {
		t.Fatalf("expired claimed old mail jobs = %d, want 0", count)
	}
}

func newUserSecurityTestRepository(t *testing.T) (*Repository, *gorm.DB) {
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
	t.Cleanup(func() { _ = sqlDB.Close() })
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewRepository(db), db
}
