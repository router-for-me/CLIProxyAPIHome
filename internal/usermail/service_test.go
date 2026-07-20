package usermail

import (
	"context"
	"errors"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/gorm"
)

type recordingSender struct {
	messages []Message
	err      error
}

func (s *recordingSender) Send(_ context.Context, message Message) error {
	s.messages = append(s.messages, message)
	return s.err
}

func TestResolveConfigValidation(t *testing.T) {
	cfg := userMailTestConfig()
	resolved, errResolve := ResolveConfig(cfg)
	if errResolve != nil {
		t.Fatalf("ResolveConfig(valid) error = %v", errResolve)
	}
	if resolved.PublicUserURL.String() != "http://127.0.0.1/user.html" || resolved.VerificationTTL != 24*time.Hour || resolved.ResetTTL != 30*time.Minute {
		t.Fatalf("resolved config = %#v", resolved)
	}
	if !Enabled(cfg) {
		t.Fatal("Enabled(valid) = false")
	}

	invalid := userMailTestConfig()
	invalid.UserEmail.PublicUserURL = "http://example.com/user.html"
	if _, errInvalid := ResolveConfig(invalid); errInvalid == nil {
		t.Fatal("ResolveConfig(insecure remote URL) succeeded")
	}
	invalid = userMailTestConfig()
	invalid.UserEmail.Sender.SMTP.Host = "smtp.example.com"
	invalid.UserEmail.Sender.SMTP.StartTLS = false
	if _, errInvalid := ResolveConfig(invalid); errInvalid == nil {
		t.Fatal("ResolveConfig(insecure remote SMTP) succeeded")
	}
	invalid = userMailTestConfig()
	invalid.UserEmail.Sender.SMTP.Port = 465
	if _, errInvalid := ResolveConfig(invalid); errInvalid == nil {
		t.Fatal("ResolveConfig(implicit TLS port) succeeded")
	}
	invalid = userMailTestConfig()
	invalid.UserEmail.FromName = "CLIProxyAPIHome\r\nBcc: victim@example.com"
	if _, errInvalid := ResolveConfig(invalid); errInvalid == nil {
		t.Fatal("ResolveConfig(injected from name) succeeded")
	}
	invalid = userMailTestConfig()
	invalid.UserEmail.Sender.SMTP.Username = "smtp-user"
	invalid.UserEmail.Sender.SMTP.PasswordEnv = "MISSING_USER_EMAIL_SMTP_PASSWORD"
	if _, errInvalid := ResolveConfig(invalid); errInvalid == nil {
		t.Fatal("ResolveConfig(empty password environment) succeeded")
	}
}

func TestServiceProcessOneStoresOnlyTokenHashAndBuildsFragmentLink(t *testing.T) {
	repo, db := newUserMailTestRepository(t)
	cfg := userMailTestConfig()
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	if _, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, cluster.UserSecurityTokenPurposeEmailVerification, user.EmailVersion, time.Now().UTC()); errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	sender := &recordingSender{}
	service := NewService(repo, func() *appconfig.Config { return cfg })
	service.senderFactory = func(ResolvedConfig) Sender { return sender }
	worked, errProcess := service.ProcessOne(ctx)
	if errProcess != nil || !worked {
		t.Fatalf("ProcessOne() = %v, %v", worked, errProcess)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("sent messages = %d, want 1", len(sender.messages))
	}
	message := sender.messages[0]
	if message.To != email || strings.Contains(message.Subject, email) {
		t.Fatalf("message metadata = %#v", message)
	}
	link := firstHTTPLink(message.Text)
	parsed, errParse := url.Parse(link)
	if errParse != nil {
		t.Fatalf("parse mail link %q: %v", link, errParse)
	}
	fragment, errFragment := url.Parse(parsed.Fragment)
	if errFragment != nil {
		t.Fatalf("parse fragment %q: %v", parsed.Fragment, errFragment)
	}
	if fragment.Path != "/user/verify-email" {
		t.Fatalf("fragment path = %q", fragment.Path)
	}
	rawToken := fragment.Query().Get("token")
	if rawToken == "" {
		t.Fatal("verification link token is empty")
	}
	var stored cluster.UserSecurityTokenRecord
	if errLoad := db.Where("user_id = ? AND purpose = ?", user.ID, cluster.UserSecurityTokenPurposeEmailVerification).Order("id DESC").First(&stored).Error; errLoad != nil {
		t.Fatalf("load stored token: %v", errLoad)
	}
	if stored.TokenHash == rawToken || stored.TokenHash != cluster.HashUserSecurityValue(rawToken) {
		t.Fatalf("stored token hash = %q, raw token leaked or hash mismatch", stored.TokenHash)
	}
	var job cluster.UserMailJobRecord
	if errLoad := db.Where("user_id = ?", user.ID).First(&job).Error; errLoad != nil {
		t.Fatalf("load mail job: %v", errLoad)
	}
	if job.Status != cluster.UserMailJobStatusSent || job.ClaimedBy != "" {
		t.Fatalf("mail job = %#v", job)
	}
}

func TestServiceSupersedesExpiredQueuedRequest(t *testing.T) {
	repo, db := newUserMailTestRepository(t)
	cfg := userMailTestConfig()
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	verifiedAt := time.Now().UTC()
	if errUpdate := db.Model(&cluster.UserRecord{}).Where("id = ?", user.ID).Update("email_verified_at", verifiedAt).Error; errUpdate != nil {
		t.Fatalf("mark email verified: %v", errUpdate)
	}
	job, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, cluster.UserSecurityTokenPurposePasswordReset, user.EmailVersion, time.Now().UTC())
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	if errUpdate := db.Model(&cluster.UserMailJobRecord{}).Where("id = ?", job.ID).Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; errUpdate != nil {
		t.Fatalf("age mail job: %v", errUpdate)
	}
	sender := &recordingSender{}
	service := NewService(repo, func() *appconfig.Config { return cfg })
	service.senderFactory = func(ResolvedConfig) Sender { return sender }
	worked, errProcess := service.ProcessOne(ctx)
	if errProcess != nil || !worked {
		t.Fatalf("ProcessOne() = %v, %v", worked, errProcess)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent stale messages = %d", len(sender.messages))
	}
	if errLoad := db.Where("id = ?", job.ID).First(job).Error; errLoad != nil {
		t.Fatalf("reload mail job: %v", errLoad)
	}
	if job.Status != cluster.UserMailJobStatusSuperseded {
		t.Fatalf("stale mail job status = %q", job.Status)
	}
}

func TestServiceFailureInvalidatesTokenAndSchedulesBoundedRetry(t *testing.T) {
	repo, db := newUserMailTestRepository(t)
	cfg := userMailTestConfig()
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	job, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, cluster.UserSecurityTokenPurposeEmailVerification, user.EmailVersion, time.Now().UTC())
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	sender := &recordingSender{err: errors.New("recipient@example.com rejected")}
	service := NewService(repo, func() *appconfig.Config { return cfg })
	service.senderFactory = func(ResolvedConfig) Sender { return sender }
	for attempt := 1; attempt <= mailJobMaxAttempts; attempt++ {
		worked, errProcess := service.ProcessOne(ctx)
		if errProcess != nil || !worked {
			t.Fatalf("ProcessOne(attempt %d) = %v, %v", attempt, worked, errProcess)
		}
		if errLoad := db.Where("id = ?", job.ID).First(job).Error; errLoad != nil {
			t.Fatalf("reload mail job: %v", errLoad)
		}
		if attempt < mailJobMaxAttempts {
			if job.Status != cluster.UserMailJobStatusPending {
				t.Fatalf("attempt %d status = %q", attempt, job.Status)
			}
			if errReady := db.Model(&cluster.UserMailJobRecord{}).Where("id = ?", job.ID).Update("next_attempt_at", time.Now().UTC().Add(-time.Second)).Error; errReady != nil {
				t.Fatalf("make retry ready: %v", errReady)
			}
		}
	}
	if job.Status != cluster.UserMailJobStatusFailed || job.Attempts != mailJobMaxAttempts || job.LastErrorCode != "smtp_delivery_failed" {
		t.Fatalf("failed mail job = %#v", job)
	}
	var activeTokens int64
	if errCount := db.Model(&cluster.UserSecurityTokenRecord{}).Where("user_id = ? AND used_at IS NULL", user.ID).Count(&activeTokens).Error; errCount != nil {
		t.Fatalf("count active tokens: %v", errCount)
	}
	if activeTokens != 0 {
		t.Fatalf("active tokens after failed delivery = %d", activeTokens)
	}
}

func TestServiceDoesNotRetryPermanentSMTPRejection(t *testing.T) {
	repo, db := newUserMailTestRepository(t)
	cfg := userMailTestConfig()
	ctx := context.Background()
	username := "alice"
	email := "alice@example.com"
	user, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	job, errEnqueue := repo.EnqueueUserMailJob(ctx, user.ID, cluster.UserSecurityTokenPurposeEmailVerification, user.EmailVersion, time.Now().UTC())
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	sender := &recordingSender{err: &textproto.Error{Code: 550, Msg: "mailbox unavailable"}}
	service := NewService(repo, func() *appconfig.Config { return cfg })
	service.senderFactory = func(ResolvedConfig) Sender { return sender }
	worked, errProcess := service.ProcessOne(ctx)
	if errProcess != nil || !worked {
		t.Fatalf("ProcessOne() = %v, %v", worked, errProcess)
	}
	if errLoad := db.Where("id = ?", job.ID).First(job).Error; errLoad != nil {
		t.Fatalf("reload mail job: %v", errLoad)
	}
	if job.Status != cluster.UserMailJobStatusFailed || job.Attempts != 1 || job.LastErrorCode != "smtp_permanent_rejection" {
		t.Fatalf("permanently rejected mail job = %#v", job)
	}
}

func TestServiceSuppressesVerificationForAddressOwnedByAnotherUser(t *testing.T) {
	repo, db := newUserMailTestRepository(t)
	cfg := userMailTestConfig()
	ctx := context.Background()
	email := "shared@example.com"
	ownerName := "owner"
	owner, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &ownerName, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser(owner) error = %v", errCreate)
	}
	claimantName := "claimant"
	claimant, errCreate := repo.CreateUser(ctx, cluster.UserUpdate{Username: &claimantName, Email: &email})
	if errCreate != nil {
		t.Fatalf("CreateUser(claimant) error = %v", errCreate)
	}
	verifiedAt := time.Now().UTC()
	if errVerify := db.Model(&cluster.UserRecord{}).Where("id = ?", owner.ID).Update("email_verified_at", verifiedAt).Error; errVerify != nil {
		t.Fatalf("mark owner email verified: %v", errVerify)
	}
	job, errEnqueue := repo.EnqueueUserMailJob(ctx, claimant.ID, cluster.UserSecurityTokenPurposeEmailVerification, claimant.EmailVersion, verifiedAt)
	if errEnqueue != nil {
		t.Fatalf("EnqueueUserMailJob() error = %v", errEnqueue)
	}
	sender := &recordingSender{}
	service := NewService(repo, func() *appconfig.Config { return cfg })
	service.senderFactory = func(ResolvedConfig) Sender { return sender }
	worked, errProcess := service.ProcessOne(ctx)
	if errProcess != nil || !worked {
		t.Fatalf("ProcessOne() = %v, %v", worked, errProcess)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("messages sent to owned address = %d, want 0", len(sender.messages))
	}
	if errLoad := db.Where("id = ?", job.ID).First(job).Error; errLoad != nil {
		t.Fatalf("reload mail job: %v", errLoad)
	}
	if job.Status != cluster.UserMailJobStatusSuperseded {
		t.Fatalf("mail job status = %q, want superseded", job.Status)
	}
}

func TestClassifyDeliveryFailureTreatsUnsupportedSecurityAsPermanent(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "starttls", err: errSMTPStartTLSUnsupported, code: "smtp_starttls_unsupported"},
		{name: "auth", err: errSMTPAuthUnsupported, code: "smtp_auth_unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := classifyDeliveryFailure(test.err)
			if failure.retryable || failure.code != test.code {
				t.Fatalf("delivery failure = %#v, want permanent %q", failure, test.code)
			}
		})
	}
}

func TestMessageForPurposeSanitizesUsername(t *testing.T) {
	_, _, message := messageForPurpose(cluster.UserSecurityTokenPurposeEmailVerification, ResolvedConfig{VerificationTTL: time.Hour}, "Alice\r\nIgnore\u202ethe link above")
	if strings.Contains(message.Text, "Alice\r\n") {
		t.Fatalf("mail greeting contains injected line break: %q", message.Text)
	}
	if strings.ContainsRune(message.Text, '\u202e') {
		t.Fatalf("mail greeting contains bidi override: %q", message.Text)
	}
	if !strings.HasPrefix(message.Text, "Hello Alice Ignore the link above,") {
		t.Fatalf("sanitized mail greeting = %q", message.Text)
	}
}

func userMailTestConfig() *appconfig.Config {
	cfg := &appconfig.Config{
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
	cfg.NormalizeUserEmailConfig()
	return cfg
}

func newUserMailTestRepository(t *testing.T) (*cluster.Repository, *gorm.DB) {
	t.Helper()
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
	return cluster.NewRepository(db), db
}

func firstHTTPLink(text string) string {
	for _, field := range strings.Fields(text) {
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			return field
		}
	}
	return ""
}
