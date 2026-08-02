package userapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/usermail"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/idna"
	"gorm.io/gorm"
)

const forgotPasswordAcceptedMessage = "If an eligible account matches, password reset instructions will be sent."

const maxUserSecurityTokenBytes = 512

const (
	verificationResendCooldown   = time.Minute
	verificationUserHourlyLimit  = 5
	verificationEmailHourlyLimit = 5
	verificationIPLimit          = 20
	verificationIPWindow         = 15 * time.Minute
	verificationGlobalLimit      = 100
	verificationGlobalWindow     = time.Minute
	emailMutationIPLimit         = 20
	emailMutationIPWindow        = 15 * time.Minute
	emailMutationUserLimit       = 10
	emailMutationUserWindow      = time.Hour
	emailMutationGlobalLimit     = 100
	emailMutationGlobalWindow    = time.Minute
	registrationIPLimit          = 10
	registrationIPWindow         = 15 * time.Minute
	registrationGlobalLimit      = 100
	registrationGlobalWindow     = time.Minute

	forgotPasswordIPLimit       = 5
	forgotPasswordIPWindow      = 15 * time.Minute
	forgotPasswordEmailCooldown = 5 * time.Minute
	forgotPasswordEmailLimit    = 3
	forgotPasswordEmailWindow   = time.Hour
	forgotPasswordGlobalLimit   = 100
	forgotPasswordGlobalWindow  = time.Minute

	publicTokenIPLimit      = 30
	publicTokenIPWindow     = 15 * time.Minute
	publicTokenGlobalLimit  = 300
	publicTokenGlobalWindow = time.Minute
)

type emailRequest struct {
	Email string `json:"email"`
}

type securityTokenRequest struct {
	Token string `json:"token"`
}

type passwordResetRequest struct {
	Token           string `json:"token"`
	NewPassword     string `json:"new_password"`
	NewPasswordDash string `json:"new-password"`
}

// UpdateEmail adds or replaces the authenticated user's optional email.
func (h *Handler) UpdateEmail(c *gin.Context) {
	if !h.requireUserEmailEnabled(c) {
		return
	}
	var body emailRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	email, errEmail := normalizeUserEmail(body.Email)
	if errEmail != nil {
		respondError(c, http.StatusBadRequest, "invalid_email", fmt.Errorf("email is invalid"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	if record.Email != nil && strings.TrimSpace(*record.Email) == email {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "user": h.userResponse(record)})
		return
	}
	allowed, retryAfter, errAllowed := h.allowUserSecurityActions(ctx, time.Now().UTC(), []userSecurityActionLimit{
		{action: "email_update_user", scope: "user:" + strconv.FormatUint(uint64(record.ID), 10), limit: emailMutationUserLimit, window: emailMutationUserWindow},
		{action: "email_update_ip", scope: "ip:" + c.ClientIP(), limit: emailMutationIPLimit, window: emailMutationIPWindow},
		{action: "email_update_global", scope: "global", limit: emailMutationGlobalLimit, window: emailMutationGlobalWindow},
	})
	if errAllowed != nil {
		respondError(c, http.StatusInternalServerError, "email_update_failed", errAllowed)
		return
	}
	if !allowed {
		c.Header("Retry-After", retryAfterHeaderValue(retryAfter))
		respondError(c, http.StatusTooManyRequests, "email_update_rate_limited", fmt.Errorf("email update rate limit exceeded"))
		return
	}
	updated, errUpdate := h.repo.UpdateUser(ctx, record.ID, cluster.UserUpdate{Email: &email})
	if errUpdate != nil {
		respondError(c, http.StatusInternalServerError, "email_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "user": h.userResponse(updated)})
}

// ClearEmail removes the authenticated user's email and invalidates email-bound security state.
func (h *Handler) ClearEmail(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	if record.Email == nil || strings.TrimSpace(*record.Email) == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "user": h.userResponse(record)})
		return
	}
	allowed, retryAfter, errAllowed := h.allowUserSecurityActions(ctx, time.Now().UTC(), []userSecurityActionLimit{
		{action: "email_update_user", scope: "user:" + strconv.FormatUint(uint64(record.ID), 10), limit: emailMutationUserLimit, window: emailMutationUserWindow},
		{action: "email_update_ip", scope: "ip:" + c.ClientIP(), limit: emailMutationIPLimit, window: emailMutationIPWindow},
		{action: "email_update_global", scope: "global", limit: emailMutationGlobalLimit, window: emailMutationGlobalWindow},
	})
	if errAllowed != nil {
		respondError(c, http.StatusInternalServerError, "email_update_failed", errAllowed)
		return
	}
	if !allowed {
		c.Header("Retry-After", retryAfterHeaderValue(retryAfter))
		respondError(c, http.StatusTooManyRequests, "email_update_rate_limited", fmt.Errorf("email update rate limit exceeded"))
		return
	}
	emptyEmail := ""
	updated, errUpdate := h.repo.UpdateUser(ctx, record.ID, cluster.UserUpdate{Email: &emptyEmail})
	if errUpdate != nil {
		respondError(c, http.StatusInternalServerError, "email_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "user": h.userResponse(updated)})
}

// RequestEmailVerification queues a verification message for the current unverified email.
func (h *Handler) RequestEmailVerification(c *gin.Context) {
	if !h.requireUserEmailEnabled(c) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	if record.Email == nil || strings.TrimSpace(*record.Email) == "" {
		respondError(c, http.StatusConflict, "email_not_configured", fmt.Errorf("email is not configured"))
		return
	}
	if record.EmailVerifiedAt != nil {
		respondOK(c)
		return
	}
	queued, retryAfter, errQueue := h.enqueueEmailVerification(ctx, record, c.ClientIP(), time.Now().UTC())
	if errQueue != nil {
		respondError(c, http.StatusInternalServerError, "verification_request_failed", errQueue)
		return
	}
	if !queued {
		c.Header("Retry-After", retryAfterHeaderValue(retryAfter))
		respondError(c, http.StatusTooManyRequests, "verification_rate_limited", fmt.Errorf("verification request rate limit exceeded"))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}

// VerifyEmail consumes a single-use email-verification token.
func (h *Handler) VerifyEmail(c *gin.Context) {
	var body securityTokenRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	allowed, _, errAllowed := h.allowUserSecurityActions(ctx, time.Now().UTC(), []userSecurityActionLimit{
		{action: "verify_email_token_ip", scope: "ip:" + c.ClientIP(), limit: publicTokenIPLimit, window: publicTokenIPWindow},
		{action: "verify_email_token_global", scope: "global", limit: publicTokenGlobalLimit, window: publicTokenGlobalWindow},
	})
	if errAllowed != nil || !allowed {
		respondInvalidOrExpiredToken(c, "verification link is invalid or expired")
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" || len(token) > maxUserSecurityTokenBytes {
		respondInvalidOrExpiredToken(c, "verification link is invalid or expired")
		return
	}
	tokenHash := cluster.HashUserSecurityValue(token)
	if _, errVerify := h.repo.ConsumeUserEmailVerificationToken(ctx, tokenHash, time.Now().UTC()); errVerify != nil {
		if errors.Is(errVerify, cluster.ErrUserSecurityTokenInvalid) || errors.Is(errVerify, gorm.ErrRecordNotFound) || cluster.IsUserEmailConflictError(errVerify) {
			respondInvalidOrExpiredToken(c, "verification link is invalid or expired")
			return
		}
		respondError(c, http.StatusInternalServerError, "email_verification_failed", errVerify)
		return
	}
	respondOK(c)
}

// ForgotPassword accepts recovery requests without revealing account state.
func (h *Handler) ForgotPassword(c *gin.Context) {
	if !h.requireUserEmailEnabled(c) {
		return
	}
	startedAt := time.Now()
	var body emailRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	now := time.Now().UTC()
	allowed, _, errAllowed := h.allowUserSecurityActions(ctx, now, []userSecurityActionLimit{
		{action: "forgot_password_ip", scope: "ip:" + c.ClientIP(), limit: forgotPasswordIPLimit, window: forgotPasswordIPWindow},
		{action: "forgot_password_global", scope: "global", limit: forgotPasswordGlobalLimit, window: forgotPasswordGlobalWindow},
	})
	if errAllowed != nil || !allowed {
		if errAllowed != nil {
			log.WithField("error_type", fmt.Sprintf("%T", errAllowed)).Warn("user recovery throttle failed")
		}
		h.respondForgotPasswordAccepted(c, startedAt)
		return
	}
	email, errEmail := normalizeUserEmail(body.Email)
	if errEmail != nil {
		h.respondForgotPasswordAccepted(c, startedAt)
		return
	}
	allowed, _, errAllowed = h.allowUserSecurityActions(ctx, now, []userSecurityActionLimit{
		{action: "forgot_password_email_cooldown", scope: "email:" + email, limit: 1, window: forgotPasswordEmailCooldown},
		{action: "forgot_password_email_hourly", scope: "email:" + email, limit: forgotPasswordEmailLimit, window: forgotPasswordEmailWindow},
	})
	if errAllowed != nil || !allowed {
		if errAllowed != nil {
			log.WithField("error_type", fmt.Sprintf("%T", errAllowed)).Warn("user recovery email throttle failed")
		}
		h.respondForgotPasswordAccepted(c, startedAt)
		return
	}
	record, errUser := h.repo.GetUserByEmail(ctx, email)
	if errUser != nil {
		if !errors.Is(errUser, gorm.ErrRecordNotFound) {
			log.WithField("error_type", fmt.Sprintf("%T", errUser)).Warn("user recovery account lookup failed")
		}
		h.respondForgotPasswordAccepted(c, startedAt)
		return
	}
	if record == nil || record.EmailVerifiedAt == nil {
		h.respondForgotPasswordAccepted(c, startedAt)
		return
	}
	if _, errEnqueue := h.repo.EnqueueUserMailJob(ctx, record.ID, cluster.UserSecurityTokenPurposePasswordReset, record.EmailVersion, now); errEnqueue != nil {
		log.WithFields(log.Fields{
			"user_id":    record.ID,
			"purpose":    cluster.UserSecurityTokenPurposePasswordReset,
			"error_type": fmt.Sprintf("%T", errEnqueue),
		}).Warn("user recovery mail enqueue failed")
	}
	h.respondForgotPasswordAccepted(c, startedAt)
}

// ResetPassword consumes a reset token and sets a new password without bypassing MFA or Passkeys.
func (h *Handler) ResetPassword(c *gin.Context) {
	var body passwordResetRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	allowed, _, errAllowed := h.allowUserSecurityActions(ctx, time.Now().UTC(), []userSecurityActionLimit{
		{action: "reset_password_ip", scope: "ip:" + c.ClientIP(), limit: 10, window: 15 * time.Minute},
		{action: "reset_password_global", scope: "global", limit: publicTokenGlobalLimit, window: publicTokenGlobalWindow},
	})
	if errAllowed != nil || !allowed {
		respondInvalidOrExpiredToken(c, "password reset link is invalid or expired")
		return
	}
	newPassword := firstRawNonEmpty(body.NewPassword, body.NewPasswordDash)
	token := strings.TrimSpace(body.Token)
	if token == "" || len(token) > maxUserSecurityTokenBytes || newPassword == "" {
		respondInvalidOrExpiredToken(c, "password reset link is invalid or expired")
		return
	}
	hashed, errHash := hashPassword(newPassword)
	if errHash != nil {
		respondPasswordHashError(c, errHash)
		return
	}
	if _, errReset := h.repo.ConsumeUserPasswordResetToken(ctx, cluster.HashUserSecurityValue(token), hashed, time.Now().UTC()); errReset != nil {
		if errors.Is(errReset, cluster.ErrUserSecurityTokenInvalid) || errors.Is(errReset, gorm.ErrRecordNotFound) {
			respondInvalidOrExpiredToken(c, "password reset link is invalid or expired")
			return
		}
		respondError(c, http.StatusInternalServerError, "password_reset_failed", errReset)
		return
	}
	respondOK(c)
}

func (h *Handler) userEmailEnabled() bool {
	return h != nil && h.runtime != nil && usermail.Enabled(h.runtime.Config())
}

func (h *Handler) requireUserEmailEnabled(c *gin.Context) bool {
	if h.userEmailEnabled() {
		return true
	}
	respondError(c, http.StatusNotFound, "email_feature_unavailable", fmt.Errorf("email feature is unavailable"))
	return false
}

func (h *Handler) enqueueEmailVerification(ctx context.Context, record *cluster.UserRecord, clientIP string, now time.Time) (bool, time.Duration, error) {
	if h == nil || h.repo == nil || record == nil || record.Email == nil || strings.TrimSpace(*record.Email) == "" {
		return false, 0, fmt.Errorf("user email is required")
	}
	userScope := "user:" + strconv.FormatUint(uint64(record.ID), 10)
	emailScope := "email:" + strings.TrimSpace(*record.Email)
	allowed, retryAfter, errAllowed := h.allowUserSecurityActions(ctx, now, []userSecurityActionLimit{
		{action: "verify_email_user_cooldown", scope: userScope, limit: 1, window: verificationResendCooldown},
		{action: "verify_email_email_cooldown", scope: emailScope, limit: 1, window: verificationResendCooldown},
		{action: "verify_email_user_hourly", scope: userScope, limit: verificationUserHourlyLimit, window: time.Hour},
		{action: "verify_email_email_hourly", scope: emailScope, limit: verificationEmailHourlyLimit, window: time.Hour},
		{action: "verify_email_ip", scope: "ip:" + strings.TrimSpace(clientIP), limit: verificationIPLimit, window: verificationIPWindow},
		{action: "verify_email_global", scope: "global", limit: verificationGlobalLimit, window: verificationGlobalWindow},
	})
	if errAllowed != nil || !allowed {
		return false, retryAfter, errAllowed
	}
	owner, errOwner := h.repo.GetUserByEmail(ctx, strings.TrimSpace(*record.Email))
	if errOwner == nil && owner != nil && owner.ID != record.ID {
		// Preserve a non-enumerating accepted response without sending mail to
		// an address that is already verified by another active account.
		return true, 0, nil
	}
	if errOwner != nil && !errors.Is(errOwner, gorm.ErrRecordNotFound) {
		return false, 0, errOwner
	}
	if _, errEnqueue := h.repo.EnqueueUserMailJob(ctx, record.ID, cluster.UserSecurityTokenPurposeEmailVerification, record.EmailVersion, now); errEnqueue != nil {
		return false, 0, errEnqueue
	}
	return true, 0, nil
}

type userSecurityActionLimit struct {
	action string
	scope  string
	limit  int
	window time.Duration
}

func (h *Handler) allowUserSecurityActions(ctx context.Context, now time.Time, limits []userSecurityActionLimit) (bool, time.Duration, error) {
	if h == nil || h.repo == nil {
		return false, 0, fmt.Errorf("user security repository is unavailable")
	}
	for _, current := range limits {
		allowed, retryAfter, errAllow := h.repo.AllowUserSecurityActionWithRetry(
			ctx,
			current.action,
			cluster.HashUserSecurityValue(current.scope),
			current.limit,
			current.window,
			now,
		)
		if errAllow != nil || !allowed {
			return false, retryAfter, errAllow
		}
	}
	return true, 0, nil
}

func retryAfterHeaderValue(retryAfter time.Duration) string {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

func userEmailStatusResponse(record *cluster.UserRecord, recoveryEnabled bool) gin.H {
	configured := record != nil && record.Email != nil && strings.TrimSpace(*record.Email) != ""
	verified := configured && record.EmailVerifiedAt != nil
	masked := ""
	if configured {
		masked = maskUserEmail(*record.Email)
	}
	return gin.H{
		"configured":     configured,
		"verified":       verified,
		"masked":         masked,
		"recovery_ready": verified && recoveryEnabled,
	}
}

func normalizeUserEmail(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 320 {
		return "", fmt.Errorf("email is invalid")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("email is invalid")
		}
	}
	parsed, errParse := mail.ParseAddress(value)
	if errParse != nil || parsed == nil || parsed.Name != "" {
		return "", fmt.Errorf("email is invalid")
	}
	address := strings.TrimSpace(parsed.Address)
	at := strings.LastIndex(address, "@")
	if at <= 0 || at == len(address)-1 {
		return "", fmt.Errorf("email is invalid")
	}
	local := strings.ToLower(address[:at])
	domain, errDomain := idna.Lookup.ToASCII(strings.ToLower(address[at+1:]))
	if errDomain != nil || strings.TrimSpace(domain) == "" {
		return "", fmt.Errorf("email is invalid")
	}
	normalized := local + "@" + strings.ToLower(domain)
	if len(normalized) > 320 {
		return "", fmt.Errorf("email is invalid")
	}
	return normalized, nil
}

func maskUserEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "***"
	}
	localRunes := []rune(email[:at])
	maskedLocal := "*"
	if len(localRunes) > 0 {
		maskedLocal = string(localRunes[0]) + "***"
	}
	return maskedLocal + email[at:]
}

func (h *Handler) respondForgotPasswordAccepted(c *gin.Context, startedAt time.Time) {
	if h != nil && h.forgotPasswordResponseFloor > 0 {
		remaining := h.forgotPasswordResponseFloor - time.Since(startedAt)
		if remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			var requestDone <-chan struct{}
			if c != nil && c.Request != nil {
				requestDone = c.Request.Context().Done()
			}
			select {
			case <-requestDone:
			case <-timer.C:
			}
		}
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted", "message": forgotPasswordAcceptedMessage})
}

func respondInvalidOrExpiredToken(c *gin.Context, message string) {
	respondError(c, http.StatusBadRequest, "invalid_or_expired_token", fmt.Errorf("%s", message))
}
