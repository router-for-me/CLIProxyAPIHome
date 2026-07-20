package usermail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	mailWorkerPollInterval = time.Second
	mailJobLease           = 45 * time.Second
	mailJobMaxAttempts     = 3
	mailSecurityRetention  = 7 * 24 * time.Hour
)

type Service struct {
	repo           *cluster.Repository
	configProvider func() *appconfig.Config
	workerID       string
	senderFactory  func(ResolvedConfig) Sender
	configMu       sync.Mutex
	configState    string
}

func NewService(repo *cluster.Repository, configProvider func() *appconfig.Config) *Service {
	return &Service{
		repo:           repo,
		configProvider: configProvider,
		workerID:       newWorkerID(),
		senderFactory: func(cfg ResolvedConfig) Sender {
			return NewSMTPSender(cfg)
		},
	}
}

func (s *Service) Start(ctx context.Context) {
	if s == nil || s.repo == nil || s.configProvider == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go s.run(ctx)
}

func (s *Service) run(ctx context.Context) {
	pollTicker := time.NewTicker(mailWorkerPollInterval)
	cleanupTicker := time.NewTicker(time.Hour)
	defer pollTicker.Stop()
	defer cleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			s.processAvailable(ctx)
		case now := <-cleanupTicker.C:
			if errPurge := s.repo.PurgeExpiredUserSecurity(ctx, now.UTC(), mailSecurityRetention); errPurge != nil {
				log.WithError(errPurge).Warn("user mail security cleanup failed")
			}
		}
	}
}

func (s *Service) processAvailable(ctx context.Context) {
	for processed := 0; processed < 4; processed++ {
		worked, errProcess := s.ProcessOne(ctx)
		if errProcess != nil {
			log.WithError(errProcess).Warn("user mail job processing failed")
			return
		}
		if !worked {
			return
		}
	}
}

// ProcessOne processes at most one queued message and is exported for focused tests.
func (s *Service) ProcessOne(ctx context.Context) (bool, error) {
	if s == nil || s.repo == nil || s.configProvider == nil {
		return false, fmt.Errorf("user mail service is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resolved, configured := s.resolveRuntimeConfig()
	if !configured {
		return false, nil
	}
	now := time.Now().UTC()
	job, errClaim := s.repo.ClaimUserMailJob(ctx, s.workerID, now, mailJobLease)
	if errClaim != nil || job == nil {
		return false, errClaim
	}
	user, errUser := s.repo.GetUser(ctx, job.UserID)
	if errors.Is(errUser, gorm.ErrRecordNotFound) {
		return true, s.supersedeJob(ctx, job.ID, now)
	}
	if errUser != nil {
		return true, s.failJob(ctx, job, "user_load_failed", now)
	}
	if user.Email == nil || strings.TrimSpace(*user.Email) == "" || user.EmailVersion != job.EmailVersion {
		return true, s.supersedeJob(ctx, job.ID, now)
	}
	if job.Purpose == cluster.UserSecurityTokenPurposePasswordReset && user.EmailVerifiedAt == nil {
		return true, s.supersedeJob(ctx, job.ID, now)
	}
	if job.Purpose == cluster.UserSecurityTokenPurposePasswordReset && user.SessionVersion != job.SessionVersion {
		return true, s.supersedeJob(ctx, job.ID, now)
	}
	if job.Purpose == cluster.UserSecurityTokenPurposeEmailVerification && user.EmailVerifiedAt != nil {
		return true, s.supersedeJob(ctx, job.ID, now)
	}

	ttl, route, message := messageForPurpose(job.Purpose, resolved, user.Username)
	if ttl <= 0 || route == "" {
		return true, s.supersedeJob(ctx, job.ID, now)
	}
	if !job.CreatedAt.IsZero() && !job.CreatedAt.Add(ttl).After(now) {
		return true, s.supersedeJob(ctx, job.ID, now)
	}
	token, errToken := randomMailToken()
	if errToken != nil {
		return true, s.failJob(ctx, job, "token_generate_failed", now)
	}
	tokenHash := cluster.HashUserSecurityValue(token)
	message.To = *user.Email
	if errStore := s.repo.ReplaceUserSecurityTokenForMailJob(ctx, job.ID, s.workerID, cluster.UserSecurityTokenRecord{
		UserID:       user.ID,
		Purpose:      job.Purpose,
		TokenHash:    tokenHash,
		EmailVersion: user.EmailVersion,
		ExpiresAt:    now.Add(ttl),
		CreatedAt:    now,
	}); errStore != nil {
		if errors.Is(errStore, cluster.ErrUserMailJobClaimLost) {
			return true, nil
		}
		return true, s.failJob(ctx, job, "token_store_failed", now)
	}
	latestUser, errLatestUser := s.repo.GetUser(ctx, job.UserID)
	if errors.Is(errLatestUser, gorm.ErrRecordNotFound) {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.supersedeJob(ctx, job.ID, time.Now().UTC())
	}
	if errLatestUser != nil {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.failJob(ctx, job, "user_reload_failed", time.Now().UTC())
	}
	if latestUser == nil || latestUser.Email == nil || strings.TrimSpace(*latestUser.Email) != message.To || latestUser.EmailVersion != job.EmailVersion {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.supersedeJob(ctx, job.ID, time.Now().UTC())
	}
	if job.Purpose == cluster.UserSecurityTokenPurposePasswordReset && latestUser.SessionVersion != job.SessionVersion {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.supersedeJob(ctx, job.ID, time.Now().UTC())
	}
	if job.Purpose == cluster.UserSecurityTokenPurposeEmailVerification && latestUser.EmailVerifiedAt != nil {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.supersedeJob(ctx, job.ID, time.Now().UTC())
	}
	if job.Purpose == cluster.UserSecurityTokenPurposeEmailVerification {
		owner, errOwner := s.repo.GetUserByEmail(ctx, message.To)
		if errOwner == nil && owner != nil && owner.ID != job.UserID {
			_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
			return true, s.supersedeJob(ctx, job.ID, time.Now().UTC())
		}
		if errOwner != nil && !errors.Is(errOwner, gorm.ErrRecordNotFound) {
			_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
			return true, s.failJob(ctx, job, "email_owner_check_failed", time.Now().UTC())
		}
	}
	deliveryLease := resolved.SMTP.Timeout + 10*time.Second
	if deliveryLease < mailJobLease {
		deliveryLease = mailJobLease
	}
	if errRenew := s.repo.RenewUserMailJobClaim(ctx, job.ID, s.workerID, time.Now().UTC(), deliveryLease); errRenew != nil {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		if errors.Is(errRenew, cluster.ErrUserMailJobClaimLost) {
			return true, nil
		}
		return true, s.failJob(ctx, job, "job_claim_renew_failed", time.Now().UTC())
	}
	message.Text = strings.ReplaceAll(message.Text, "{{LINK}}", buildUserLink(resolved.PublicUserURL, route, token))
	if s.senderFactory == nil {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.failJobWithRetry(ctx, job, "sender_unavailable", time.Now().UTC(), false)
	}
	sender := s.senderFactory(resolved)
	if sender == nil {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		return true, s.failJobWithRetry(ctx, job, "sender_unavailable", time.Now().UTC(), false)
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, resolved.SMTP.Timeout)
	errSend := sender.Send(deliveryCtx, message)
	cancel()
	if errSend != nil {
		_ = s.repo.InvalidateUserSecurityTokenHash(ctx, tokenHash, time.Now().UTC())
		failure := classifyDeliveryFailure(errSend)
		fields := log.Fields{
			"job_id":       job.ID,
			"user_id":      job.UserID,
			"purpose":      job.Purpose,
			"error_code":   failure.code,
			"retryable":    failure.retryable,
			"smtp_host":    resolved.SMTP.Host,
			"smtp_port":    resolved.SMTP.Port,
			"attempt":      job.Attempts,
			"max_attempts": mailJobMaxAttempts,
		}
		if failure.smtpStatus > 0 {
			fields["smtp_status"] = failure.smtpStatus
		}
		log.WithFields(fields).Warn("user mail delivery failed")
		return true, s.failJobWithRetry(ctx, job, failure.code, time.Now().UTC(), failure.retryable)
	}
	if errComplete := s.repo.CompleteUserMailJob(ctx, job.ID, s.workerID, time.Now().UTC()); errors.Is(errComplete, cluster.ErrUserMailJobClaimLost) {
		log.WithFields(log.Fields{"job_id": job.ID, "user_id": job.UserID, "purpose": job.Purpose}).Warn("user mail delivered after worker claim was lost")
		return true, nil
	} else if errComplete != nil {
		return true, errComplete
	}
	log.WithFields(log.Fields{"job_id": job.ID, "user_id": job.UserID, "purpose": job.Purpose}).Info("user mail job delivered")
	return true, nil
}

func (s *Service) resolveRuntimeConfig() (ResolvedConfig, bool) {
	current := s.configProvider()
	if current == nil || !current.UserEmail.Enabled {
		s.reportConfigState("disabled", nil)
		return ResolvedConfig{}, false
	}
	resolved, errResolve := ResolveConfig(current)
	if errResolve != nil {
		s.reportConfigState("invalid:"+errResolve.Error(), errResolve)
		return ResolvedConfig{}, false
	}
	s.reportConfigState("ready", nil)
	return resolved, true
}

func (s *Service) reportConfigState(state string, err error) {
	s.configMu.Lock()
	previous := s.configState
	if previous == state {
		s.configMu.Unlock()
		return
	}
	s.configState = state
	s.configMu.Unlock()

	switch {
	case strings.HasPrefix(state, "invalid:"):
		log.WithError(err).Warn("user email capability disabled because configuration is invalid")
	case state == "ready" && previous != "" && previous != "ready":
		log.Info("user email capability enabled with valid configuration")
	case state == "disabled" && previous != "" && previous != "disabled":
		log.Info("user email capability disabled")
	}
}

func (s *Service) failJob(ctx context.Context, job *cluster.UserMailJobRecord, code string, now time.Time) error {
	return s.failJobWithRetry(ctx, job, code, now, true)
}

func (s *Service) failJobWithRetry(ctx context.Context, job *cluster.UserMailJobRecord, code string, now time.Time, retryable bool) error {
	retryAfter := time.Minute << max(job.Attempts-1, 0)
	if retryAfter > 15*time.Minute {
		retryAfter = 15 * time.Minute
	}
	maxAttempts := mailJobMaxAttempts
	if !retryable {
		maxAttempts = job.Attempts
	}
	errFail := s.repo.FailUserMailJob(ctx, job, s.workerID, code, now, retryAfter, maxAttempts)
	if errors.Is(errFail, cluster.ErrUserMailJobClaimLost) {
		return nil
	}
	return errFail
}

func (s *Service) supersedeJob(ctx context.Context, jobID uint, now time.Time) error {
	errSupersede := s.repo.SupersedeUserMailJob(ctx, jobID, s.workerID, now)
	if errors.Is(errSupersede, cluster.ErrUserMailJobClaimLost) {
		return nil
	}
	return errSupersede
}

type deliveryFailure struct {
	code       string
	retryable  bool
	smtpStatus int
}

func classifyDeliveryFailure(err error) deliveryFailure {
	if errors.Is(err, context.DeadlineExceeded) {
		return deliveryFailure{code: "smtp_timeout", retryable: true}
	}
	if errors.Is(err, context.Canceled) {
		return deliveryFailure{code: "smtp_canceled", retryable: true}
	}
	if errors.Is(err, errSMTPStartTLSUnsupported) {
		return deliveryFailure{code: "smtp_starttls_unsupported", retryable: false}
	}
	if errors.Is(err, errSMTPAuthUnsupported) {
		return deliveryFailure{code: "smtp_auth_unsupported", retryable: false}
	}
	var protocolError *textproto.Error
	if errors.As(err, &protocolError) {
		failure := deliveryFailure{code: "smtp_rejected", retryable: protocolError.Code < 500, smtpStatus: protocolError.Code}
		if protocolError.Code >= 400 && protocolError.Code < 500 {
			failure.code = "smtp_transient_rejection"
		}
		if protocolError.Code >= 500 {
			failure.code = "smtp_permanent_rejection"
		}
		return failure
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return deliveryFailure{code: "smtp_timeout", retryable: true}
	}
	return deliveryFailure{code: "smtp_delivery_failed", retryable: true}
}

func messageForPurpose(purpose string, cfg ResolvedConfig, username string) (time.Duration, string, Message) {
	username = mailGreetingName(username)
	switch purpose {
	case cluster.UserSecurityTokenPurposeEmailVerification:
		return cfg.VerificationTTL, "/user/verify-email", Message{
			Subject: "Verify your CLIProxyAPIHome email",
			Text:    "Hello " + username + ",\n\nConfirm this email address for your CLIProxyAPIHome account:\n\n{{LINK}}\n\nIf you did not request this, you can ignore this message.",
		}
	case cluster.UserSecurityTokenPurposePasswordReset:
		return cfg.ResetTTL, "/user/reset-password", Message{
			Subject: "Reset your CLIProxyAPIHome password",
			Text:    "Hello " + username + ",\n\nUse this link to set a new password for your CLIProxyAPIHome account:\n\n{{LINK}}\n\nThis link is single-use and expires soon. TOTP and Passkey protections remain enabled.",
		}
	default:
		return 0, "", Message{}
	}
}

func mailGreetingName(username string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, username)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return "there"
	}
	runes := []rune(cleaned)
	if len(runes) > 80 {
		cleaned = strings.TrimSpace(string(runes[:80]))
	}
	return cleaned
}

func buildUserLink(base *url.URL, route string, token string) string {
	if base == nil {
		return ""
	}
	next := *base
	next.RawQuery = ""
	next.Fragment = route + "?token=" + url.QueryEscape(token)
	return next.String()
}

func randomMailToken() (string, error) {
	buf := make([]byte, 32)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func newWorkerID() string {
	host, _ := os.Hostname()
	suffix, errToken := randomMailToken()
	if errToken != nil || len(suffix) < 8 {
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	} else {
		suffix = suffix[:8]
	}
	return fmt.Sprintf("%s-%d-%s", strings.TrimSpace(host), os.Getpid(), suffix)
}
