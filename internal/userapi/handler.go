package userapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const maxUserJSONBodyBytes int64 = 1 << 20

type Handler struct {
	repo                        *cluster.Repository
	runtime                     *home.Runtime
	forgotPasswordResponseFloor time.Duration
}

type authFields struct {
	Username     string `json:"username"`
	UserName     string `json:"user_name"`
	UserNameDash string `json:"user-name"`
	Password     string `json:"password"`
	TOTPCode     string `json:"totp_code"`
	TOTPCodeDash string `json:"totp-code"`
	TOTP         string `json:"totp"`
	Code         string `json:"code"`
}

type loginRequest struct {
	authFields
	Email string `json:"email"`
}

type passwordRequest struct {
	authFields
	NewPassword     string `json:"new_password"`
	NewPasswordDash string `json:"new-password"`
}

type totpRequest struct {
	authFields
	Secret     string `json:"secret"`
	Issuer     string `json:"issuer"`
	Regenerate bool   `json:"regenerate"`
}

type apiKeyRequest struct {
	authFields
	ID              *uint   `json:"id"`
	APIKey          *string `json:"api_key"`
	APIKeyDash      *string `json:"api-key"`
	Key             *string `json:"key"`
	Value           *string `json:"value"`
	Old             *string `json:"old"`
	New             *string `json:"new"`
	NewAPIKey       *string `json:"new_api_key"`
	NewAPIKeyDash   *string `json:"new-api-key"`
	Channels        *[]uint `json:"channels"`
	ModelGroups     *[]uint `json:"model_groups"`
	ModelGroupsDash *[]uint `json:"model-groups"`
}

type passkeyRequest struct {
	authFields
	ID            string          `json:"id"`
	PasskeyID     string          `json:"passkey_id"`
	PasskeyIDDash string          `json:"passkey-id"`
	Name          string          `json:"name"`
	Credential    json.RawMessage `json:"credential"`
	ChallengeID   string          `json:"challenge_id"`
	ChallengeDash string          `json:"challenge-id"`
	SessionID     string          `json:"session_id"`
	SessionDash   string          `json:"session-id"`
	State         string          `json:"state"`
}

// NewHandler creates a user API handler.
func NewHandler(repo *cluster.Repository, runtime *home.Runtime) *Handler {
	return &Handler{
		repo:                        repo,
		runtime:                     runtime,
		forgotPasswordResponseFloor: 150 * time.Millisecond,
	}
}

// Register wires user API routes.
func Register(group *gin.RouterGroup, handler *Handler) {
	if group == nil || handler == nil {
		return
	}
	group.GET("/capabilities", handler.GetCapabilities)
	group.POST("/register", handler.RegisterUser)
	group.POST("/login", handler.Login)
	group.POST("/login/passkey/begin", handler.BeginPasskeyLogin)
	group.POST("/login/passkey/options", handler.BeginPasskeyLogin)
	group.POST("/login/totp", handler.LoginTOTP)
	group.POST("/login/passkey", handler.LoginPasskey)

	group.GET("/models", handler.ListModels)
	group.GET("/models/accessible", handler.ListAccessibleModels)

	group.GET("/me", handler.CurrentUser)
	group.GET("/billing/overview", handler.CurrentUserBillingOverview)
	group.GET("/billing/charges", handler.ListCurrentUserBillingCharges)

	group.POST("/password", handler.ChangePassword)
	group.PATCH("/password", handler.ChangePassword)
	group.POST("/password/forgot", handler.ForgotPassword)
	group.POST("/password/reset", handler.ResetPassword)

	group.PUT("/email", handler.UpdateEmail)
	group.DELETE("/email", handler.ClearEmail)
	group.POST("/email/verification", handler.RequestEmailVerification)
	group.POST("/email/verify", handler.VerifyEmail)

	group.GET("/totp", handler.ShowTOTP)
	group.POST("/totp/show", handler.ShowTOTP)
	group.POST("/totp", handler.BindTOTP)
	group.POST("/totp/bind", handler.BindTOTP)
	group.DELETE("/totp", handler.DeleteTOTP)

	group.GET("/api-keys", handler.ListAPIKeys)
	group.POST("/api-keys", handler.CreateAPIKey)
	group.POST("/api-key", handler.CreateAPIKey)
	group.PATCH("/api-keys", handler.UpdateAPIKey)
	group.PATCH("/api-key", handler.UpdateAPIKey)
	group.PATCH("/api-keys/:id", handler.UpdateAPIKey)
	group.PATCH("/api-key/:id", handler.UpdateAPIKey)
	group.DELETE("/api-keys", handler.DeleteAPIKey)
	group.DELETE("/api-key", handler.DeleteAPIKey)
	group.DELETE("/api-keys/:id", handler.DeleteAPIKey)
	group.DELETE("/api-key/:id", handler.DeleteAPIKey)

	group.POST("/passkeys/begin", handler.BeginPasskeyRegistration)
	group.POST("/passkey/begin", handler.BeginPasskeyRegistration)
	group.POST("/passkeys/options", handler.BeginPasskeyRegistration)
	group.POST("/passkey/options", handler.BeginPasskeyRegistration)
	group.POST("/passkeys", handler.CreatePasskey)
	group.POST("/passkey", handler.CreatePasskey)
	group.DELETE("/passkeys", handler.DeletePasskey)
	group.DELETE("/passkey", handler.DeletePasskey)
	group.DELETE("/passkeys/:id", handler.DeletePasskey)
	group.DELETE("/passkey/:id", handler.DeletePasskey)
}

// RegisterUser creates a user account.
func (h *Handler) RegisterUser(c *gin.Context) {
	var body loginRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	username := body.username()
	password := body.Password
	email := ""
	if username == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("username is required"))
		return
	}
	if password == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("password is required"))
		return
	}
	if strings.TrimSpace(body.Email) != "" {
		if !h.userEmailEnabled() {
			respondError(c, http.StatusConflict, "email_feature_unavailable", fmt.Errorf("email feature is unavailable"))
			return
		}
		var errEmail error
		email, errEmail = normalizeUserEmail(body.Email)
		if errEmail != nil {
			respondError(c, http.StatusBadRequest, "invalid_email", fmt.Errorf("email is invalid"))
			return
		}
	}

	ctx, cancel := requestContext(c)
	defer cancel()
	registrationLimits := []userSecurityActionLimit{
		{action: "user_registration_ip", scope: "ip:" + c.ClientIP(), limit: registrationIPLimit, window: registrationIPWindow},
		{action: "user_registration_global", scope: "global", limit: registrationGlobalLimit, window: registrationGlobalWindow},
	}
	allowed, retryAfter, errAllowed := h.allowUserSecurityActions(ctx, time.Now().UTC(), registrationLimits)
	if errAllowed != nil {
		respondError(c, http.StatusInternalServerError, "user_create_failed", errAllowed)
		return
	}
	if !allowed {
		c.Header("Retry-After", retryAfterHeaderValue(retryAfter))
		respondError(c, http.StatusTooManyRequests, "registration_rate_limited", fmt.Errorf("registration rate limit exceeded"))
		return
	}
	if _, errExisting := h.repo.GetUserByUsername(ctx, username); errExisting == nil {
		respondError(c, http.StatusConflict, "user_exists", fmt.Errorf("user already exists"))
		return
	} else if !errors.Is(errExisting, gorm.ErrRecordNotFound) {
		respondError(c, http.StatusInternalServerError, "user_load_failed", errExisting)
		return
	}
	hashed, errHash := hashPassword(password)
	if errHash != nil {
		respondPasswordHashError(c, errHash)
		return
	}
	update := cluster.UserUpdate{
		Username: &username,
		Password: &hashed,
	}
	if email != "" {
		update.Email = &email
	}
	record, errCreate := h.repo.CreateUser(ctx, update)
	if errCreate != nil {
		if cluster.IsUserConflictError(errCreate) {
			respondError(c, http.StatusConflict, "user_exists", fmt.Errorf("user already exists"))
			return
		}
		respondError(c, http.StatusInternalServerError, "user_create_failed", errCreate)
		return
	}
	if email != "" {
		if _, _, errEnqueue := h.enqueueEmailVerification(ctx, record, c.ClientIP(), time.Now().UTC()); errEnqueue != nil {
			log.WithFields(log.Fields{
				"user_id":    record.ID,
				"purpose":    cluster.UserSecurityTokenPurposeEmailVerification,
				"error_type": fmt.Sprintf("%T", errEnqueue),
			}).Warn("user mail enqueue failed")
		}
	}
	h.respondLogin(c, ctx, record)
}

// Login handles password login without TOTP verification.
func (h *Handler) Login(c *gin.Context) {
	var body loginRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.userByPassword(c, ctx, body.authFields)
	if !ok {
		return
	}
	if h.requirePasskeyLogin(c, record) {
		return
	}
	if _, enabled := loadTOTP(record.MFA); enabled {
		respondError(c, http.StatusUnauthorized, "totp_required", fmt.Errorf("totp code is required"))
		return
	}
	h.respondLogin(c, ctx, record)
}

// LoginTOTP handles password login with TOTP verification.
func (h *Handler) LoginTOTP(c *gin.Context) {
	var body loginRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.userByPassword(c, ctx, body.authFields)
	if !ok {
		return
	}
	totp, enabled := loadTOTP(record.MFA)
	if !enabled {
		if h.requirePasskeyLogin(c, record) {
			return
		}
		respondError(c, http.StatusBadRequest, "totp_not_enabled", fmt.Errorf("totp is not enabled"))
		return
	}
	if !verifyTOTPCode(totp.Secret, body.totpCode(), time.Now().UTC()) {
		respondError(c, http.StatusUnauthorized, "invalid_totp", fmt.Errorf("invalid totp code"))
		return
	}
	h.respondLogin(c, ctx, record)
}

// LoginPasskey handles passkey login.
func (h *Handler) LoginPasskey(c *gin.Context) {
	var body passkeyRequest
	rawBody, okDecode := decodeJSONBodyWithRaw(c, &body)
	if !okDecode {
		return
	}
	username := body.username()
	challengeID := body.challengeID()
	credentialRaw := passkeyCredentialPayload(rawBody, body.Credential)
	if username == "" || challengeID == "" || len(credentialRaw) == 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("username, challenge_id, and credential are required"))
		return
	}

	ctx, cancel := requestContext(c)
	defer cancel()
	record, errUser := h.repo.GetUserByUsername(ctx, username)
	if errUser != nil {
		respondAuthError(c, errUser)
		return
	}
	if errLogin := h.finishPasskeyLogin(c, ctx, record, challengeID, credentialRaw); errLogin != nil {
		respondError(c, http.StatusUnauthorized, "invalid_passkey", fmt.Errorf("invalid passkey"))
		return
	}
	h.respondLogin(c, ctx, record)
}

// CurrentUser returns the authenticated user profile.
func (h *Handler) CurrentUser(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": h.userResponseWithPasskeys(record), "status": "ok"})
}

// ChangePassword updates the authenticated user's password.
func (h *Handler) ChangePassword(c *gin.Context) {
	var body passwordRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	newPassword := body.newPassword()
	if newPassword == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("new_password is required"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	hashed, errHash := hashPassword(newPassword)
	if errHash != nil {
		respondPasswordHashError(c, errHash)
		return
	}
	update := cluster.UserUpdate{Password: &hashed}
	updated, errUpdate := h.repo.UpdateUser(ctx, record.ID, update)
	if errUpdate != nil {
		respondUserError(c, "password_update_failed", errUpdate)
		return
	}
	h.respondLogin(c, ctx, updated)
}

// ShowTOTP returns a TOTP setup secret and otpauth URL.
func (h *Handler) ShowTOTP(c *gin.Context) {
	var body totpRequest
	if c.Request != nil && c.Request.Method != http.MethodGet {
		if !decodeJSONBody(c, &body) {
			return
		}
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}

	issuer := firstNonEmpty(body.Issuer, c.Query("issuer"), defaultTOTPIssuer)
	totp, enabled := loadTOTP(record.MFA)
	secret := ""
	if enabled && !body.Regenerate && !parseBoolQuery(c, "regenerate") {
		secret = totp.Secret
		if strings.TrimSpace(totp.Issuer) != "" {
			issuer = totp.Issuer
		}
	}
	if secret == "" {
		nextSecret, errSecret := generateTOTPSecret()
		if errSecret != nil {
			respondError(c, http.StatusInternalServerError, "totp_secret_failed", errSecret)
			return
		}
		secret = nextSecret
	}
	c.JSON(http.StatusOK, gin.H{
		"secret":       secret,
		"otp_auth_url": otpauthURL(record.Username, secret, issuer),
		"issuer":       normalizeTOTPIssuer(issuer),
		"period":       defaultTOTPPeriod,
		"digits":       defaultTOTPDigits,
		"algorithm":    defaultTOTPAlgorithm,
		"enabled":      enabled,
	})
}

// BindTOTP verifies and stores a TOTP secret for the authenticated user.
func (h *Handler) BindTOTP(c *gin.Context) {
	var body totpRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	secret := normalizeTOTPSecret(body.Secret)
	code := body.totpCode()
	if secret == "" || code == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("secret and code are required"))
		return
	}
	if !verifyTOTPCode(secret, code, time.Now().UTC()) {
		respondError(c, http.StatusUnauthorized, "invalid_totp", fmt.Errorf("invalid totp code"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	mfa, errMFA := marshalTOTP(record.Username, secret, body.Issuer)
	if errMFA != nil {
		respondError(c, http.StatusInternalServerError, "totp_bind_failed", errMFA)
		return
	}
	updated, errUpdate := h.repo.UpdateUser(ctx, record.ID, cluster.UserUpdate{MFA: &mfa})
	if errUpdate != nil {
		respondUserError(c, "totp_bind_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": h.userResponse(updated), "status": "ok"})
}

// DeleteTOTP removes the authenticated user's TOTP configuration.
func (h *Handler) DeleteTOTP(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	var mfa cluster.JSONB
	updated, errUpdate := h.repo.UpdateUser(ctx, record.ID, cluster.UserUpdate{MFA: &mfa})
	if errUpdate != nil {
		respondUserError(c, "totp_delete_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": h.userResponse(updated), "status": "ok"})
}

// ListAPIKeys lists API keys owned by the authenticated user.
func (h *Handler) ListAPIKeys(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	records, errRecords := h.repo.ListAPIKeyRecordsForUser(ctx, record.ID)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "api_key_load_failed", errRecords)
		return
	}
	items, ok := apiKeyRecordsResponse(c, records)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": items, "items": items})
}

// CreateAPIKey creates an API key owned by the authenticated user.
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var body apiKeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	key := body.apiKeyValue()
	if key == "" {
		generated, errGenerate := generateAPIKey()
		if errGenerate != nil {
			respondError(c, http.StatusInternalServerError, "api_key_generate_failed", errGenerate)
			return
		}
		key = generated
	}
	update := cluster.APIKeyUserUpdate{
		APIKey:      &key,
		Channels:    body.Channels,
		ModelGroups: body.modelGroups(),
	}
	apiKeyRecord, errCreate := h.repo.CreateAPIKeyForUser(ctx, record.ID, update)
	if errCreate != nil {
		respondAPIKeyWriteError(c, "api_key_create_failed", errCreate)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	item, ok := apiKeyRecordResponse(c, apiKeyRecord)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_key": item})
}

// UpdateAPIKey updates an API key owned by the authenticated user.
func (h *Handler) UpdateAPIKey(c *gin.Context) {
	var body apiKeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	id, ok := apiKeyIDFromRequest(c, body)
	if !ok {
		return
	}
	targetKey := body.targetAPIKey(c)
	newKey := body.newAPIKey(id)
	update := cluster.APIKeyUserUpdate{
		APIKey:      newKey,
		Channels:    body.Channels,
		ModelGroups: body.modelGroups(),
	}
	apiKeyRecord, errUpdate := h.repo.UpdateAPIKeyForUser(ctx, record.ID, id, targetKey, update)
	if errUpdate != nil {
		respondAPIKeyWriteError(c, "api_key_update_failed", errUpdate)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	item, ok := apiKeyRecordResponse(c, apiKeyRecord)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_key": item})
}

// DeleteAPIKey deletes an API key owned by the authenticated user.
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	var body apiKeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	id, ok := apiKeyIDFromRequest(c, body)
	if !ok {
		return
	}
	targetKey := body.targetAPIKey(c)
	if errDelete := h.repo.DeleteAPIKeyForUser(ctx, record.ID, id, targetKey); errDelete != nil {
		respondAPIKeyWriteError(c, "api_key_delete_failed", errDelete)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// BeginPasskeyLogin returns WebAuthn assertion options for passkey login.
func (h *Handler) BeginPasskeyLogin(c *gin.Context) {
	var body passkeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	username := body.username()
	if username == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("username is required"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, errUser := h.repo.GetUserByUsername(ctx, username)
	if errUser != nil {
		respondAuthError(c, errUser)
		return
	}
	assertion, challengeID, errBegin := h.beginPasskeyLogin(c, ctx, record)
	if errBegin != nil {
		respondError(c, http.StatusInternalServerError, "passkey_begin_failed", errBegin)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"challenge_id": challengeID,
		"options":      assertion,
		"publicKey":    assertion.Response,
	})
}

// BeginPasskeyRegistration returns WebAuthn creation options for the authenticated user.
func (h *Handler) BeginPasskeyRegistration(c *gin.Context) {
	var body passkeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	creation, challengeID, errBegin := h.beginPasskeyRegistration(c, ctx, record)
	if errBegin != nil {
		respondError(c, http.StatusInternalServerError, "passkey_begin_failed", errBegin)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"challenge_id": challengeID,
		"options":      creation,
		"publicKey":    creation.Response,
	})
}

// CreatePasskey creates a passkey entry for the authenticated user.
func (h *Handler) CreatePasskey(c *gin.Context) {
	var body passkeyRequest
	rawBody, okDecode := decodeJSONBodyWithRaw(c, &body)
	if !okDecode {
		return
	}
	challengeID := body.challengeID()
	credentialRaw := passkeyCredentialPayload(rawBody, body.Credential)
	if challengeID == "" || len(credentialRaw) == 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("challenge_id and credential are required"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	entry, errCreate := h.finishPasskeyRegistration(c, ctx, record, challengeID, credentialRaw, body.Name)
	if errCreate != nil {
		respondError(c, http.StatusUnauthorized, "invalid_passkey", fmt.Errorf("invalid passkey"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"passkey": passkeyPublic(entry), "status": "ok"})
}

// DeletePasskey deletes a passkey entry for the authenticated user.
func (h *Handler) DeletePasskey(c *gin.Context) {
	var body passkeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, ok := h.authenticatedUser(c, ctx, body.authFields)
	if !ok {
		return
	}
	passkeyID := firstNonEmpty(strings.TrimSpace(c.Param("id")), c.Query("id"), body.passkeyID())
	if passkeyID == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("passkey id is required"))
		return
	}
	entries, errPasskeys := loadPasskeys(record.Passkey)
	if errPasskeys != nil {
		respondError(c, http.StatusInternalServerError, "passkey_load_failed", errPasskeys)
		return
	}
	next := make([]passkeyEntry, 0, len(entries))
	deleted := false
	for _, entry := range entries {
		if entry.ID == passkeyID {
			deleted = true
			continue
		}
		next = append(next, entry)
	}
	if !deleted {
		respondError(c, http.StatusNotFound, "not_found", fmt.Errorf("passkey not found"))
		return
	}
	raw, errMarshal := marshalPasskeys(next)
	if errMarshal != nil {
		respondError(c, http.StatusInternalServerError, "passkey_save_failed", errMarshal)
		return
	}
	if _, errUpdate := h.repo.UpdateUser(ctx, record.ID, cluster.UserUpdate{Passkey: &raw}); errUpdate != nil {
		respondUserError(c, "passkey_delete_failed", errUpdate)
		return
	}
	respondOK(c)
}

func (h *Handler) authenticatedUser(c *gin.Context, ctx context.Context, fields authFields) (*cluster.UserRecord, bool) {
	_ = fields
	token := bearerToken(c)
	if token == "" {
		respondError(c, http.StatusUnauthorized, "bearer_token_required", fmt.Errorf("bearer token is required"))
		return nil, false
	}
	claims, errToken := h.bearerTokenClaims(ctx, token)
	if errToken != nil {
		respondError(c, http.StatusUnauthorized, "invalid_token", fmt.Errorf("invalid token"))
		return nil, false
	}
	record, errUser := h.repo.GetUser(ctx, claims.UserID)
	if errUser != nil {
		respondAuthError(c, errUser)
		return nil, false
	}
	if claims.SessionVersion != record.SessionVersion {
		respondError(c, http.StatusUnauthorized, "invalid_token", fmt.Errorf("invalid token"))
		return nil, false
	}
	return record, true
}

func (h *Handler) userByPassword(c *gin.Context, ctx context.Context, fields authFields) (*cluster.UserRecord, bool) {
	username := fields.username()
	password := fields.Password
	if username == "" || password == "" {
		respondError(c, http.StatusUnauthorized, "invalid_credentials", fmt.Errorf("invalid credentials"))
		return nil, false
	}
	record, errUser := h.repo.GetUserByUsername(ctx, username)
	if errUser != nil {
		respondAuthError(c, errUser)
		return nil, false
	}
	if !passwordMatches(record.Password, password) {
		respondError(c, http.StatusUnauthorized, "invalid_credentials", fmt.Errorf("invalid credentials"))
		return nil, false
	}
	return record, true
}

func (h *Handler) requirePasskeyLogin(c *gin.Context, record *cluster.UserRecord) bool {
	enabled, errPasskeys := passkeyEnabled(record.Passkey)
	if errPasskeys != nil {
		respondError(c, http.StatusInternalServerError, "passkey_load_failed", errPasskeys)
		return true
	}
	if !enabled {
		return false
	}
	respondError(c, http.StatusUnauthorized, "passkey_required", fmt.Errorf("passkey is required"))
	return true
}

func (h *Handler) respondLogin(c *gin.Context, ctx context.Context, record *cluster.UserRecord) {
	token, expiresAt, errToken := h.createBearerToken(ctx, record.ID, record.SessionVersion, defaultSessionTTL)
	if errToken != nil {
		respondError(c, http.StatusInternalServerError, "token_create_failed", errToken)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_at": expiresAt,
		"user":       h.userResponse(record),
	})
}

func (h *Handler) refreshConfig(ctx context.Context) error {
	if h == nil || h.runtime == nil {
		return nil
	}
	cfg, payload, errConfig := h.repo.LoadConfigAsRuntimeConfig(ctx)
	if errConfig != nil {
		return errConfig
	}
	if errApply := h.runtime.ApplyConfigFromCluster(ctx, cfg); errApply != nil {
		return errApply
	}
	h.runtime.PublishConfigYAML(payload)
	return nil
}

func (h *Handler) userResponse(record *cluster.UserRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	_, totpEnabled := loadTOTP(record.MFA)
	passkeys, _ := loadPasskeys(record.Passkey)
	return gin.H{
		"id":            record.ID,
		"username":      record.Username,
		"credits":       record.Credits,
		"totp_enabled":  totpEnabled,
		"passkey_count": len(passkeys),
		"email_status":  userEmailStatusResponse(record, h.userEmailEnabled()),
		"created_at":    record.CreatedAt,
		"updated_at":    record.UpdatedAt,
	}
}

func (h *Handler) userResponseWithPasskeys(record *cluster.UserRecord) gin.H {
	out := h.userResponse(record)
	if record == nil {
		return out
	}
	passkeys, _ := loadPasskeys(record.Passkey)
	items := make([]map[string]any, 0, len(passkeys))
	for _, entry := range passkeys {
		items = append(items, passkeyPublic(entry))
	}
	out["passkeys"] = items
	return out
}

func apiKeyRecordsResponse(c *gin.Context, records []cluster.APIKeyRecord) ([]gin.H, bool) {
	items := make([]gin.H, 0, len(records))
	for i := range records {
		item, ok := apiKeyRecordResponse(c, &records[i])
		if !ok {
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

func apiKeyRecordResponse(c *gin.Context, record *cluster.APIKeyRecord) (gin.H, bool) {
	entry, errEntry := cluster.APIKeyEntryFromRecord(record)
	if errEntry != nil {
		respondError(c, http.StatusInternalServerError, "api_key_load_failed", errEntry)
		return nil, false
	}
	channels := append([]uint(nil), entry.Channels...)
	if channels == nil {
		channels = []uint{}
	}
	modelGroups := append([]uint(nil), entry.ModelGroups...)
	if modelGroups == nil {
		modelGroups = []uint{}
	}
	return gin.H{
		"id":           record.ID,
		"api-key":      entry.APIKey,
		"api_key":      entry.APIKey,
		"channels":     channels,
		"model_groups": modelGroups,
		"created_at":   record.CreatedAt,
		"updated_at":   record.UpdatedAt,
	}, true
}

func decodeJSONBody(c *gin.Context, out any) bool {
	_, ok := decodeJSONBodyWithRaw(c, out)
	return ok
}

func decodeJSONBodyWithRaw(c *gin.Context, out any) ([]byte, bool) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, true
	}
	data, errRead := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxUserJSONBodyBytes))
	if errRead != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(errRead, &maxBytesError) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Errorf("request body exceeds %d bytes", maxUserJSONBodyBytes))
			return nil, false
		}
		respondError(c, http.StatusBadRequest, "invalid_body", errRead)
		return nil, false
	}
	if strings.TrimSpace(string(data)) == "" {
		return data, true
	}
	if errUnmarshal := json.Unmarshal(data, out); errUnmarshal != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errUnmarshal)
		return nil, false
	}
	return data, true
}

func requestContext(c *gin.Context) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if c != nil && c.Request != nil && c.Request.Context() != nil {
		ctx = c.Request.Context()
	}
	return context.WithTimeout(ctx, 10*time.Second)
}

func respondError(c *gin.Context, status int, code string, err error) {
	message := strings.TrimSpace(code)
	if status >= http.StatusInternalServerError {
		message = strings.ToLower(http.StatusText(http.StatusInternalServerError))
		fields := log.Fields{"error_code": code}
		if err != nil {
			fields["error_type"] = fmt.Sprintf("%T", err)
		}
		if c != nil && c.Request != nil {
			fields["method"] = c.Request.Method
			fields["path"] = c.FullPath()
		}
		log.WithFields(fields).Error("user API request failed")
	} else if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	c.JSON(status, gin.H{"error": code, "message": message})
}

func respondPasswordHashError(c *gin.Context, err error) {
	if errors.Is(err, errPasswordTooLong) {
		respondError(c, http.StatusBadRequest, "invalid_password", err)
		return
	}
	respondError(c, http.StatusInternalServerError, "password_hash_failed", err)
}

func respondOK(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func respondAuthError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, http.StatusUnauthorized, "invalid_credentials", fmt.Errorf("invalid credentials"))
		return
	}
	respondError(c, http.StatusInternalServerError, "user_load_failed", err)
}

func respondUserError(c *gin.Context, code string, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, http.StatusNotFound, "not_found", err)
		return
	}
	respondError(c, http.StatusInternalServerError, code, err)
}

func respondAPIKeyWriteError(c *gin.Context, code string, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, http.StatusNotFound, "not_found", err)
		return
	}
	if errors.Is(err, cluster.ErrAPIKeyExists) {
		respondError(c, http.StatusConflict, "api_key_exists", err)
		return
	}
	if errors.Is(err, cluster.ErrUserNotFound) {
		respondError(c, http.StatusNotFound, "user_not_found", err)
		return
	}
	respondError(c, http.StatusInternalServerError, code, err)
}

func (f authFields) username() string {
	return firstNonEmpty(f.Username, f.UserName, f.UserNameDash)
}

func (f authFields) totpCode() string {
	return firstNonEmpty(f.TOTPCode, f.TOTPCodeDash, f.TOTP, f.Code)
}

func (r passwordRequest) newPassword() string {
	return firstRawNonEmpty(r.NewPassword, r.NewPasswordDash)
}

func (r totpRequest) totpCode() string {
	return firstNonEmpty(r.Code, r.TOTPCode, r.TOTPCodeDash, r.TOTP)
}

func (r apiKeyRequest) apiKeyValue() string {
	return stringPtrValue(firstStringPtr(r.APIKey, r.APIKeyDash, r.Key, r.Value))
}

func (r apiKeyRequest) targetAPIKey(c *gin.Context) string {
	return firstNonEmpty(
		stringPtrValue(r.Old),
		stringPtrValue(r.APIKey),
		stringPtrValue(r.APIKeyDash),
		stringPtrValue(r.Key),
		queryValue(c, "api_key"),
		queryValue(c, "api-key"),
		queryValue(c, "key"),
		queryValue(c, "value"),
	)
}

func (r apiKeyRequest) newAPIKey(id uint) *string {
	if value := firstStringPtr(r.NewAPIKey, r.NewAPIKeyDash, r.New); value != nil {
		next := strings.TrimSpace(*value)
		return &next
	}
	if id > 0 {
		if value := firstStringPtr(r.APIKey, r.APIKeyDash, r.Key, r.Value); value != nil {
			next := strings.TrimSpace(*value)
			return &next
		}
	}
	return nil
}

func (r apiKeyRequest) modelGroups() *[]uint {
	if r.ModelGroups != nil {
		return r.ModelGroups
	}
	return r.ModelGroupsDash
}

func (r passkeyRequest) passkeyID() string {
	return firstNonEmpty(r.PasskeyID, r.PasskeyIDDash, r.ID)
}

func (r passkeyRequest) challengeID() string {
	return firstNonEmpty(r.ChallengeID, r.ChallengeDash, r.SessionID, r.SessionDash, r.State)
}

func apiKeyIDFromRequest(c *gin.Context, body apiKeyRequest) (uint, bool) {
	idRaw := firstNonEmpty(strings.TrimSpace(c.Param("id")), queryValue(c, "id"))
	if idRaw != "" {
		parsed, errParse := strconv.ParseUint(idRaw, 10, 64)
		if errParse != nil || parsed == 0 {
			respondError(c, http.StatusBadRequest, "invalid_id", fmt.Errorf("invalid api key id"))
			return 0, false
		}
		return uint(parsed), true
	}
	if body.ID != nil {
		if *body.ID == 0 {
			respondError(c, http.StatusBadRequest, "invalid_id", fmt.Errorf("invalid api key id"))
			return 0, false
		}
		return *body.ID, true
	}
	if body.targetAPIKey(c) == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("api key id or value is required"))
		return 0, false
	}
	return 0, true
}

func generateAPIKey() (string, error) {
	token, errToken := randomToken(randomTokenBytes)
	if errToken != nil {
		return "", errToken
	}
	return "cpah_" + token, nil
}

func bearerToken(c *gin.Context) string {
	value := headerValue(c, "Authorization")
	if value == "" {
		return ""
	}
	parts := strings.Fields(value)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func headerValue(c *gin.Context, key string) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.GetHeader(key))
}

func queryValue(c *gin.Context, key string) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Query(key))
}

func parseBoolQuery(c *gin.Context, key string) bool {
	value := queryValue(c, key)
	if value == "" {
		return false
	}
	parsed, errParse := strconv.ParseBool(value)
	return errParse == nil && parsed
}

func firstStringPtr(values ...*string) *string {
	for _, value := range values {
		if value == nil {
			continue
		}
		if strings.TrimSpace(*value) != "" {
			return value
		}
	}
	return nil
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func firstRawNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
