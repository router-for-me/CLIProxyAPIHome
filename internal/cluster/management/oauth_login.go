package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	devinauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/devin"
	kimiauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/kimi"
	metaauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/meta"
	xaiauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	maxOAuthStateLength = 128

	anthropicAuthURL     = "https://claude.ai/oauth/authorize"
	anthropicRedirectURI = "http://localhost:54545/callback"

	codexAuthURL     = "https://auth.openai.com/oauth/authorize"
	codexRedirectURI = "http://localhost:1455/auth/callback"

	antigravityAuthEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	antigravityTokenEndpoint    = "https://oauth2.googleapis.com/token"
	antigravityUserInfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	antigravityCallbackPort     = 51121
	antigravityAPIEndpoint      = "https://cloudcode-pa.googleapis.com"
	antigravityDailyAPIEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	antigravityAPIVersion       = "v1internal"
	antigravityFallbackVersion  = "1.21.9"
	antigravityNodeAPIClientUA  = "google-api-nodejs-client/10.3.0"
	antigravityGoogAPIClientUA  = "gl-node/22.21.1"
)

var (
	errInvalidOAuthState   = errors.New("invalid oauth state")
	errUnsupportedProvider = errors.New("unsupported oauth provider")
	antigravityScopes      = []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cclog", "https://www.googleapis.com/auth/experimentsandconfigs"}
)

type pkceCodes struct {
	CodeVerifier  string
	CodeChallenge string
}

type oauthCallbackRequest struct {
	Provider    string `json:"provider"`
	RedirectURL string `json:"redirect_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
	Error       string `json:"error"`
}

// RequestAnthropicToken handles request anthropic token.
func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	pkce, errPKCE := generatePKCECodes()
	if errPKCE != nil {
		log.Errorf("cluster oauth: failed to generate anthropic PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}
	state, errState := generateOAuthState("cla")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate anthropic state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	authURL := buildAnthropicAuthURL(state, pkce)
	if errRegister := h.registerOAuthSession(c, "anthropic", state, map[string]any{
		"code_verifier":  pkce.CodeVerifier,
		"code_challenge": pkce.CodeChallenge,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestCodexToken handles request codex token.
func (h *Handler) RequestCodexToken(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	pkce, errPKCE := generatePKCECodes()
	if errPKCE != nil {
		log.Errorf("cluster oauth: failed to generate codex PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}
	state, errState := generateOAuthState("cdx")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate codex state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	authURL := buildCodexAuthURL(state, pkce)
	if errRegister := h.registerOAuthSession(c, "codex", state, map[string]any{
		"code_verifier":  pkce.CodeVerifier,
		"code_challenge": pkce.CodeChallenge,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestAntigravityToken handles request antigravity token.
func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	state, errState := generateOAuthState("agv")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate antigravity state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", antigravityCallbackPort)
	authURL := buildAntigravityAuthURL(state, redirectURI)
	if errRegister := h.registerOAuthSession(c, "antigravity", state, map[string]any{
		"redirect_uri": redirectURI,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestKimiToken handles request kimi token.
func (h *Handler) RequestKimiToken(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	cfg := h.oauthConfig()
	ctx, cancel := context.WithTimeout(requestContextOrBackground(c), 30*time.Second)
	defer cancel()

	state, errState := generateOAuthState("kmi")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate kimi state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	provider, domain := "kimi", kimiauth.KimiDefaultDomain
	if strings.EqualFold(strings.TrimSpace(c.Query("provider")), "kimi-ai") {
		provider, domain = "kimi-ai", kimiauth.KimiAIDomain
	}
	kimiAuth := kimiauth.NewKimiAuth(cfg, domain)
	deviceFlow, errDevice := kimiAuth.StartDeviceFlow(ctx)
	if errDevice != nil {
		log.Errorf("cluster oauth: failed to start kimi device flow: %v", errDevice)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := strings.TrimSpace(deviceFlow.VerificationURIComplete)
	if authURL == "" {
		authURL = strings.TrimSpace(deviceFlow.VerificationURI)
	}

	if errRegister := h.registerOAuthSession(c, provider, state, map[string]any{
		"device_code": deviceFlow.DeviceCode,
		"user_code":   deviceFlow.UserCode,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}

	go h.waitForKimiAuthorization(state, kimiAuth, deviceFlow)

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestXAIToken handles request xAI token.
func (h *Handler) RequestXAIToken(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	ctx, cancel := context.WithTimeout(requestContextOrBackground(c), 30*time.Second)
	defer cancel()

	pkceCodes, errPKCE := xaiauth.GeneratePKCECodes()
	if errPKCE != nil {
		log.Errorf("cluster oauth: failed to generate xai PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}
	state, errState := generateOAuthState("xai")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate xai state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	nonce, errNonce := randomURLSafe(24)
	if errNonce != nil {
		log.Errorf("cluster oauth: failed to generate xai nonce: %v", errNonce)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate nonce parameter"})
		return
	}

	authSvc := xaiauth.NewXAIAuth(h.oauthConfig())
	discovery, errDiscover := authSvc.Discover(ctx)
	if errDiscover != nil {
		log.Errorf("cluster oauth: failed to discover xai OAuth endpoints: %v", errDiscover)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to discover oauth endpoints"})
		return
	}

	redirectURI := fmt.Sprintf("http://%s:%d%s", xaiauth.RedirectHost, xaiauth.CallbackPort, xaiauth.RedirectPath)
	authURL, errAuthURL := xaiauth.BuildAuthorizeURL(xaiauth.AuthorizeURLParams{
		AuthorizationEndpoint: discovery.AuthorizationEndpoint,
		RedirectURI:           redirectURI,
		CodeChallenge:         pkceCodes.CodeChallenge,
		State:                 state,
		Nonce:                 nonce,
	})
	if errAuthURL != nil {
		log.Errorf("cluster oauth: failed to generate xai authorization URL: %v", errAuthURL)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	if errRegister := h.registerOAuthSession(c, "xai", state, map[string]any{
		"code_verifier":          pkceCodes.CodeVerifier,
		"code_challenge":         pkceCodes.CodeChallenge,
		"nonce":                  nonce,
		"redirect_uri":           redirectURI,
		"authorization_endpoint": discovery.AuthorizationEndpoint,
		"token_endpoint":         discovery.TokenEndpoint,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestDevinToken handles request Devin token.
func (h *Handler) RequestDevinToken(c *gin.Context) {
	pkceCodes, errPKCE := devinauth.GeneratePKCECodes()
	if errPKCE != nil {
		log.Errorf("cluster oauth: failed to generate devin PKCE codes: %v", errPKCE)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}
	state, errState := generateOAuthState("dvn")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate devin state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	authSvc := devinauth.NewDevinAuthService(h.oauthConfig())
	redirectURI := devinauth.DefaultRedirectURI()
	authURL := authSvc.BuildAuthorizationURL(redirectURI, pkceCodes.CodeChallenge, state)
	if errRegister := h.registerOAuthSession(c, "devin", state, map[string]any{
		"code_verifier":  pkceCodes.CodeVerifier,
		"code_challenge": pkceCodes.CodeChallenge,
		"redirect_uri":   redirectURI,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

// RequestMetaToken handles request Meta token.
func (h *Handler) RequestMetaToken(c *gin.Context) {
	cfg := h.oauthConfig()
	ctx, cancel := context.WithTimeout(requestContextOrBackground(c), 30*time.Second)
	defer cancel()

	state, errState := generateOAuthState("mta")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate meta state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	metaAuth := metaauth.NewMetaAuth(cfg)
	deviceFlow, errDevice := metaAuth.StartDeviceFlow(ctx)
	if errDevice != nil {
		log.Errorf("cluster oauth: failed to start meta device flow: %v", errDevice)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL, userCode, errView := deviceFlowAuthView(deviceFlow.VerificationURIComplete, deviceFlow.VerificationURI, deviceFlow.UserCode)
	if errView != nil {
		log.Errorf("cluster oauth: meta device flow missing verification target: %v", errView)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	if errRegister := h.registerOAuthSession(c, "meta", state, map[string]any{
		"device_code": deviceFlow.DeviceCode,
		"user_code":   userCode,
	}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}

	go h.waitForMetaAuthorization(state, metaAuth, deviceFlow)

	payload := gin.H{"status": "ok", "url": authURL, "state": state}
	if userCode != "" {
		payload["user_code"] = userCode
	}
	c.JSON(http.StatusOK, payload)
}

// GetAuthStatus returns an auth status.
func (h *Handler) GetAuthStatus(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if errState := validateOAuthState(state); errState != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	session, errSession := h.repo.GetOAuthSession(ctx, state)
	if errSession != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errSession)
		return
	}
	if session == nil {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if strings.EqualFold(session.Status, "complete") {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if strings.EqualFold(session.Status, "error") {
		errMsg := strings.TrimSpace(session.Error)
		if errMsg == "" {
			errMsg = "Authentication failed"
		}
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": errMsg})
		return
	}
	data, errData := cluster.OAuthSessionData(session)
	if errData != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errData)
		return
	}
	if h.respondPluginAuthStatus(c, ctx, session, data) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "wait"})
}

// handleOAuthCallback handles an o auth callback.
func (h *Handler) handleOAuthCallback(c *gin.Context, rejectCodexDeviceFlow bool) {
	// Resolve credential context before calling upstream OAuth services.
	var req oauthCallbackRequest
	if c.Request.Method == http.MethodGet {
		req.State = c.Query("state")
		req.Code = c.Query("code")
		req.Error = c.Query("error")
		if req.Error == "" {
			req.Error = c.Query("error_description")
		}
		req.Provider = c.Query("provider")
	} else if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}

	state := strings.TrimSpace(req.State)
	code := strings.TrimSpace(req.Code)
	errorMessage := strings.TrimSpace(req.Error)
	var redirectErr error
	if rawRedirect := strings.TrimSpace(req.RedirectURL); rawRedirect != "" {
		parsedURL, errParse := url.Parse(rawRedirect)
		if errParse != nil {
			if state == "" {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid redirect_url"})
				return
			}
			redirectErr = errParse
		} else {
			query := parsedURL.Query()
			if state == "" {
				state = strings.TrimSpace(query.Get("state"))
			}
			if code == "" {
				code = strings.TrimSpace(query.Get("code"))
			}
			if errorMessage == "" {
				errorMessage = strings.TrimSpace(query.Get("error"))
				if errorMessage == "" {
					errorMessage = strings.TrimSpace(query.Get("error_description"))
				}
			}
		}
	}

	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}
	if errState := validateOAuthState(state); errState != nil {
		if redirectErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid redirect_url"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	if redirectErr != nil || (code == "" && errorMessage == "") {
		if rejectCodexDeviceFlow && h.rejectCodexDeviceCallback(c, state) {
			return
		}
		if redirectErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid redirect_url"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "code or error is required"})
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	session, errSession := h.repo.GetOAuthSession(ctx, state)
	if errSession != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errSession)
		return
	}
	if session == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if strings.TrimSpace(session.Status) != "" {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is not pending"})
		return
	}
	data, errData := cluster.OAuthSessionData(session)
	if errData != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errData)
		return
	}
	if rejectCodexDeviceFlow && oauthSessionIsCodexDevice(session, data) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth callback is not supported for device flow"})
		return
	}

	providerInput := strings.TrimSpace(req.Provider)
	if providerInput == "" {
		providerInput = strings.TrimSpace(session.Provider)
	}
	isPlugin := oauthSessionIsPlugin(data)
	var provider string
	var errProvider error
	if isPlugin {
		provider, errProvider = normalizePluginOAuthProvider(providerInput)
	} else {
		provider, errProvider = normalizeOAuthProvider(providerInput)
	}
	if errProvider != nil || (!isPlugin && (provider == "kimi" || provider == "kimi-ai" || provider == "meta")) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "unsupported provider"})
		return
	}
	if !strings.EqualFold(session.Provider, provider) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "provider does not match state"})
		return
	}

	if errorMessage != "" {
		if errMerge := h.repo.MergeOAuthSessionData(ctx, state, map[string]any{
			"callback_received_at": time.Now().UTC().Format(time.RFC3339),
			"callback_error":       errorMessage,
		}); errMerge != nil {
			respondOAuthSessionMergeError(c, errMerge)
			return
		}
		if errSet := h.repo.SetOAuthSessionError(ctx, state, "Authentication failed"); errSet != nil {
			respondError(c, http.StatusInternalServerError, "oauth_session_failed", errSet)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	if isPlugin {
		if errMerge := h.repo.MergeOAuthSessionData(ctx, state, pluginOAuthCallbackSessionData(code, provider)); errMerge != nil {
			respondOAuthSessionMergeError(c, errMerge)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	if errMerge := h.repo.MergeOAuthSessionData(ctx, state, map[string]any{
		"callback_received_at": time.Now().UTC().Format(time.RFC3339),
	}); errMerge != nil {
		respondOAuthSessionMergeError(c, errMerge)
		return
	}

	go h.processOAuthCallback(provider, state, code)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) rejectCodexDeviceCallback(c *gin.Context, state string) bool {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	session, errSession := h.repo.GetOAuthSession(ctx, state)
	if errSession != nil || session == nil {
		return false
	}
	data, errData := cluster.OAuthSessionData(session)
	if errData != nil || !oauthSessionIsCodexDevice(session, data) {
		return false
	}
	message := "oauth callback is not supported for device flow"
	if strings.TrimSpace(session.Status) != "" {
		message = "oauth flow is not pending"
	}
	c.JSON(http.StatusConflict, gin.H{"status": "error", "error": message})
	return true
}

func oauthSessionIsCodexDevice(session *cluster.OAuthSessionRecord, data map[string]any) bool {
	return session != nil && strings.EqualFold(strings.TrimSpace(session.Provider), "codex") &&
		strings.EqualFold(strings.TrimSpace(stringFromAny(data["flow"])), "device")
}

func respondOAuthSessionMergeError(c *gin.Context, errMerge error) {
	if errors.Is(errMerge, cluster.ErrOAuthSessionNotPending) {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is not pending"})
		return
	}
	respondError(c, http.StatusInternalServerError, "oauth_session_failed", errMerge)
}

// processOAuthCallback handles a process o auth callback.
func (h *Handler) processOAuthCallback(provider, state, code string) {
	// Resolve credential context before calling upstream OAuth services.
	ctx, cancel := h.oauthSessionContext(context.Background(), state, 5*time.Minute)
	defer cancel()

	session, errSession := h.repo.GetOAuthSession(ctx, state)
	if errSession != nil {
		log.Errorf("cluster oauth: load session failed: %v", errSession)
		return
	}
	if session == nil || session.Status != "" || ctx.Err() != nil {
		return
	}
	data, errData := cluster.OAuthSessionData(session)
	if errData != nil {
		_ = h.repo.SetOAuthSessionError(ctx, state, "Authentication failed")
		log.Errorf("cluster oauth: decode session data failed: %v", errData)
		return
	}

	var errProcess error
	switch provider {
	case "anthropic":
		errProcess = h.exchangeAnthropicCallback(ctx, state, code, data)
	case "codex":
		errProcess = h.exchangeCodexCallback(ctx, code, data)
	case "antigravity":
		errProcess = h.exchangeAntigravityCallback(ctx, code, data)
	case "xai":
		errProcess = h.exchangeXAICallback(ctx, code, data)
	case "devin":
		errProcess = h.exchangeDevinCallback(ctx, code, data)
	default:
		errProcess = fmt.Errorf("unsupported provider: %s", provider)
	}
	if errProcess != nil {
		_ = h.repo.SetOAuthSessionError(ctx, state, authStatusMessage(provider, errProcess))
		log.Errorf("cluster oauth: %s callback failed: %v", provider, errProcess)
		return
	}
}

// waitForKimiAuthorization returns a wait for kimi authorization.
func (h *Handler) waitForKimiAuthorization(state string, kimiAuth *kimiauth.KimiAuth, deviceFlow *kimiauth.DeviceCodeResponse) {
	// Validate request inputs before mutating persisted state.
	ctx, cancel := h.oauthSessionContext(context.Background(), state, 16*time.Minute)
	defer cancel()

	authBundle, errWait := kimiAuth.WaitForAuthorization(ctx, deviceFlow)
	if errWait != nil {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Authentication failed")
		log.Errorf("cluster oauth: kimi authorization failed: %v", errWait)
		return
	}
	if authBundle == nil || authBundle.TokenData == nil {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Authentication failed")
		return
	}

	tokenData := authBundle.TokenData
	provider := "kimi"
	if authBundle.Domain == kimiauth.KimiAIDomain {
		provider = "kimi-ai"
	}
	metadata := map[string]any{
		"type":          provider,
		"domain":        authBundle.Domain,
		"base_url":      kimiauth.ResolveKimiAPIBaseURL(authBundle.Domain),
		"access_token":  tokenData.AccessToken,
		"refresh_token": tokenData.RefreshToken,
		"token_type":    tokenData.TokenType,
		"scope":         tokenData.Scope,
		"timestamp":     time.Now().UnixMilli(),
	}
	if tokenData.ExpiresAt > 0 {
		metadata["expired"] = time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if deviceID := strings.TrimSpace(authBundle.DeviceID); deviceID != "" {
		metadata["device_id"] = deviceID
	}

	storeCtx, cancelStore := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStore()
	fileName := fmt.Sprintf("%s-%d.json", provider, time.Now().UnixMilli())
	if errStore := h.storeOAuthMetadataWithContext(storeCtx, metadata, fileName); errStore != nil {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Failed to save authentication tokens")
		log.Errorf("cluster oauth: store kimi token failed: %v", errStore)
		return
	}
}

// waitForMetaAuthorization waits for Meta device-flow authorization to complete.
func (h *Handler) waitForMetaAuthorization(state string, metaAuth *metaauth.MetaAuth, deviceFlow *metaauth.DeviceCodeResponse) {
	ctx, cancel := h.oauthSessionContext(context.Background(), state, 16*time.Minute)
	defer cancel()

	authBundle, errWait := metaAuth.WaitForAuthorization(ctx, deviceFlow)
	if errWait != nil {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Authentication failed")
		log.Errorf("cluster oauth: meta authorization failed: %v", errWait)
		return
	}
	if authBundle == nil || authBundle.TokenData == nil {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Authentication failed")
		return
	}

	tokenStorage := metaAuth.CreateTokenStorage(authBundle)
	if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Authentication failed")
		return
	}

	storeCtx, cancelStore := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStore()
	fileName := metaauth.CredentialFileName(tokenStorage.Email, tokenStorage.DCAToken)
	if errStore := h.storeOAuthMetadataWithContext(storeCtx, metaauth.BuildOAuthMetadata(tokenStorage), fileName); errStore != nil {
		_ = h.repo.SetOAuthSessionError(context.Background(), state, "Failed to save authentication tokens")
		log.Errorf("cluster oauth: store meta token failed: %v", errStore)
		return
	}
}

// registerOAuthSession handles a register o auth session.
func (h *Handler) registerOAuthSession(c *gin.Context, provider, state string, data map[string]any) error {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := cluster.NewOAuthSessionRecord(provider, state, data, time.Now().UTC())
	if errRecord != nil {
		return errRecord
	}
	return h.repo.UpsertOAuthSession(ctx, record)
}

// exchangeAnthropicCallback handles an exchange anthropic callback.
func (h *Handler) exchangeAnthropicCallback(ctx context.Context, state, code string, data map[string]any) error {
	// Validate request inputs before mutating persisted state.
	pkce := pkceCodes{
		CodeVerifier:  stringFromAny(data["code_verifier"]),
		CodeChallenge: stringFromAny(data["code_challenge"]),
	}
	if pkce.CodeVerifier == "" {
		return fmt.Errorf("missing PKCE verifier")
	}

	reqBody := map[string]any{
		"code":          strings.Split(strings.TrimSpace(code), "#")[0],
		"state":         state,
		"grant_type":    "authorization_code",
		"client_id":     claude.ClientID,
		"redirect_uri":  anthropicRedirectURI,
		"code_verifier": pkce.CodeVerifier,
	}
	if parts := strings.Split(strings.TrimSpace(code), "#"); len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		reqBody["state"] = strings.TrimSpace(parts[1])
	}

	rawBody, errMarshal := json.Marshal(reqBody)
	if errMarshal != nil {
		return errMarshal
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, claude.TokenURL, bytes.NewReader(rawBody))
	if errRequest != nil {
		return errRequest
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := claude.NewAnthropicHttpClient(&h.oauthConfig().SDKConfig)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("anthropic oauth: response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Account      struct {
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	if errUnmarshal := json.Unmarshal(body, &tokenResp); errUnmarshal != nil {
		return errUnmarshal
	}
	now := time.Now()
	metadata := map[string]any{
		"type":          "claude",
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"last_refresh":  now.Format(time.RFC3339),
		"email":         tokenResp.Account.EmailAddress,
		"expired":       now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	return h.storeOAuthMetadataWithContext(ctx, metadata, claudeCredentialFileName(tokenResp.Account.EmailAddress))
}

// exchangeCodexCallback handles an exchange codex callback.
func (h *Handler) exchangeCodexCallback(ctx context.Context, code string, data map[string]any) error {
	return h.exchangeCodexCallbackWithClient(ctx, h.oauthHTTPClient(), code, data)
}

func (h *Handler) exchangeCodexCallbackWithClient(ctx context.Context, client *http.Client, code string, data map[string]any) error {
	// Validate request inputs before mutating persisted state.
	pkce := pkceCodes{
		CodeVerifier:  stringFromAny(data["code_verifier"]),
		CodeChallenge: stringFromAny(data["code_challenge"]),
	}
	if pkce.CodeVerifier == "" {
		return fmt.Errorf("missing PKCE verifier")
	}
	if client == nil {
		return fmt.Errorf("codex oauth HTTP client is required")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", codex.ClientID)
	form.Set("code", strings.TrimSpace(code))
	redirectURI := codexRedirectURI
	if strings.EqualFold(strings.TrimSpace(stringFromAny(data["flow"])), "device") {
		redirectURI = codex.DeviceTokenExchangeRedirectURI
	}
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", pkce.CodeVerifier)

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, codex.TokenURL, strings.NewReader(form.Encode()))
	if errRequest != nil {
		return errRequest
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex oauth: response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if errUnmarshal := json.Unmarshal(body, &tokenResp); errUnmarshal != nil {
		return errUnmarshal
	}

	accountID := ""
	email := ""
	planType := ""
	hashAccountID := ""
	if claims, errParse := codex.ParseJWTToken(tokenResp.IDToken); errParse == nil && claims != nil {
		accountID = claims.GetAccountID()
		email = claims.GetUserEmail()
		planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
		if accountID != "" {
			digest := sha256.Sum256([]byte(accountID))
			hashAccountID = hex.EncodeToString(digest[:])[:8]
		}
	}

	now := time.Now()
	metadata := map[string]any{
		"type":          "codex",
		"id_token":      tokenResp.IDToken,
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"account_id":    accountID,
		"last_refresh":  now.Format(time.RFC3339),
		"email":         email,
		"expired":       now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	return h.storeOAuthMetadataWithContext(ctx, metadata, codexCredentialFileName(email, planType, hashAccountID, true))
}

// exchangeAntigravityCallback handles an exchange antigravity callback.
func (h *Handler) exchangeAntigravityCallback(ctx context.Context, code string, data map[string]any) error {
	// Validate request inputs before mutating persisted state.
	redirectURI := stringFromAny(data["redirect_uri"])
	if redirectURI == "" {
		redirectURI = fmt.Sprintf("http://localhost:%d/oauth-callback", antigravityCallbackPort)
	}

	form := url.Values{}
	form.Set("code", strings.TrimSpace(code))
	form.Set("client_id", antigravity.ClientID)
	form.Set("client_secret", antigravity.ClientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, antigravityTokenEndpoint, strings.NewReader(form.Encode()))
	if errRequest != nil {
		return errRequest
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, errDo := h.oauthHTTPClient().Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity oauth: response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(body, &tokenResp); errUnmarshal != nil {
		return errUnmarshal
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return fmt.Errorf("token exchange returned empty access token")
	}

	email, errEmail := h.fetchAntigravityEmail(ctx, tokenResp.AccessToken)
	if errEmail != nil {
		return errEmail
	}
	projectID := ""
	if fetchedProjectID, errProject := h.fetchAntigravityProjectID(ctx, tokenResp.AccessToken); errProject != nil {
		log.Warnf("cluster oauth: failed to fetch antigravity project ID: %v", errProject)
	} else {
		projectID = strings.TrimSpace(fetchedProjectID)
	}
	now := time.Now()
	metadata := map[string]any{
		"type":          "antigravity",
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"expires_in":    tokenResp.ExpiresIn,
		"timestamp":     now.UnixMilli(),
		"expired":       now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
		"email":         email,
	}
	if projectID != "" {
		metadata["project_id"] = projectID
	}
	return h.storeOAuthMetadataWithContext(ctx, metadata, antigravityCredentialFileName(email))
}

// exchangeXAICallback handles an exchange xAI callback.
func (h *Handler) exchangeXAICallback(ctx context.Context, code string, data map[string]any) error {
	// Validate request inputs before mutating persisted state.
	pkce := &xaiauth.PKCECodes{
		CodeVerifier:  stringFromAny(data["code_verifier"]),
		CodeChallenge: stringFromAny(data["code_challenge"]),
	}
	if pkce.CodeVerifier == "" {
		return fmt.Errorf("missing PKCE verifier")
	}
	redirectURI := stringFromAny(data["redirect_uri"])
	if redirectURI == "" {
		redirectURI = fmt.Sprintf("http://%s:%d%s", xaiauth.RedirectHost, xaiauth.CallbackPort, xaiauth.RedirectPath)
	}
	tokenEndpoint := stringFromAny(data["token_endpoint"])

	authSvc := xaiauth.NewXAIAuth(h.oauthConfig())
	bundle, errExchange := authSvc.ExchangeCodeForTokens(ctx, strings.TrimSpace(code), redirectURI, pkce, tokenEndpoint)
	if errExchange != nil {
		return errExchange
	}
	if bundle == nil || strings.TrimSpace(bundle.TokenData.AccessToken) == "" {
		return fmt.Errorf("token exchange returned empty access token")
	}

	tokenData := bundle.TokenData
	metadata := map[string]any{
		"type":           "xai",
		"access_token":   tokenData.AccessToken,
		"refresh_token":  tokenData.RefreshToken,
		"id_token":       tokenData.IDToken,
		"token_type":     tokenData.TokenType,
		"expires_in":     tokenData.ExpiresIn,
		"expired":        tokenData.Expire,
		"last_refresh":   bundle.LastRefresh,
		"base_url":       firstNonEmptyString(bundle.BaseURL, xaiauth.DefaultAPIBaseURL),
		"redirect_uri":   bundle.RedirectURI,
		"token_endpoint": bundle.TokenEndpoint,
		"auth_kind":      "oauth",
	}
	if tokenData.Email != "" {
		metadata["email"] = tokenData.Email
	}
	if tokenData.Subject != "" {
		metadata["sub"] = tokenData.Subject
	}
	return h.storeOAuthMetadataWithContext(ctx, metadata, xaiauth.CredentialFileName(tokenData.Email, tokenData.Subject))
}

// exchangeDevinCallback handles a Devin OAuth callback.
func (h *Handler) exchangeDevinCallback(ctx context.Context, code string, data map[string]any) error {
	codeVerifier := stringFromAny(data["code_verifier"])
	if codeVerifier == "" {
		return fmt.Errorf("missing PKCE verifier")
	}
	authSvc := devinauth.NewDevinAuthService(h.oauthConfig())
	token, errExchange := authSvc.ExchangeCodeForToken(ctx, strings.TrimSpace(code), codeVerifier)
	if errExchange != nil {
		return errExchange
	}
	sessionToken := devinauth.FormatSessionToken(token)
	if sessionToken == "" {
		return fmt.Errorf("token exchange returned empty session token")
	}
	userName, userID, orgID, errSelf := authSvc.FetchSelfProfile(ctx, sessionToken)
	if errSelf != nil {
		log.Warnf("cluster oauth: failed to fetch devin user profile: %v", errSelf)
	}
	return h.storeOAuthMetadataWithContext(ctx, devinauth.BuildOAuthMetadata(sessionToken, userName, userID, orgID), devinauth.CredentialFileName(userName, userID))
}

// storeOAuthMetadataWithContext stores an o auth metadata with context.
func (h *Handler) storeOAuthMetadataWithContext(ctx context.Context, metadata map[string]any, originalFilename string) error {
	raw, errMarshal := json.MarshalIndent(metadata, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	raw = append(raw, '\n')
	_, errStore := h.storeOAuthPayloadWithContext(ctx, raw, originalFilename)
	return errStore
}

// buildAnthropicAuthURL builds an anthropic auth url.
func buildAnthropicAuthURL(state string, pkce *pkceCodes) string {
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {claude.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {anthropicRedirectURI},
		"scope":                 {"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"},
		"code_challenge":        {pkce.CodeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return anthropicAuthURL + "?" + params.Encode()
}

// buildCodexAuthURL builds a codex auth url.
func buildCodexAuthURL(state string, pkce *pkceCodes) string {
	params := url.Values{
		"client_id":                  {codex.ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {codexRedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {pkce.CodeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return codexAuthURL + "?" + params.Encode()
}

// buildAntigravityAuthURL builds an antigravity auth url.
func buildAntigravityAuthURL(state, redirectURI string) string {
	params := url.Values{}
	params.Set("access_type", "offline")
	params.Set("client_id", antigravity.ClientID)
	params.Set("prompt", "consent")
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(antigravityScopes, " "))
	params.Set("state", state)
	return antigravityAuthEndpoint + "?" + params.Encode()
}

// geminiProjectIDFromMap derives gemini project id from map.
func geminiProjectIDFromMap(values map[string]any) string {
	if values == nil {
		return ""
	}
	switch projectValue := values["cloudaicompanionProject"].(type) {
	case string:
		return strings.TrimSpace(projectValue)
	case map[string]any:
		return stringFromAny(projectValue["id"])
	default:
		return ""
	}
}

// fetchAntigravityEmail fetches an antigravity email.
func (h *Handler) fetchAntigravityEmail(ctx context.Context, accessToken string) (string, error) {
	// Validate request inputs before mutating persisted state.
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, antigravityUserInfoEndpoint, nil)
	if errRequest != nil {
		return "", errRequest
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	resp, errDo := h.oauthHTTPClient().Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity userinfo: response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("userinfo request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	email := strings.TrimSpace(gjson.GetBytes(body, "email").String())
	if email == "" {
		return "", fmt.Errorf("userinfo response missing email")
	}
	return email, nil
}

// fetchAntigravityProjectID fetches an antigravity project id.
func (h *Handler) fetchAntigravityProjectID(ctx context.Context, accessToken string) (string, error) {
	// Validate request inputs before mutating persisted state.
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", fmt.Errorf("antigravity project: missing access token")
	}
	userAgent := cpasdkauth.AntigravityRequestUserAgent("")
	loadReqBody := map[string]any{
		"metadata": map[string]string{
			"ideType": "ANTIGRAVITY",
		},
	}
	rawBody, errMarshal := json.Marshal(loadReqBody)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal request body: %w", errMarshal)
	}
	endpointURL := fmt.Sprintf("%s/%s:loadCodeAssist", antigravityDailyAPIEndpoint, antigravityAPIVersion)
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(rawBody))
	if errRequest != nil {
		return "", fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, errDo := h.oauthHTTPClient().Do(req)
	if errDo != nil {
		return "", fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity loadCodeAssist: response body close error: %v", errClose)
		}
	}()
	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", fmt.Errorf("read response: %w", errRead)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	var loadResp map[string]any
	if errDecode := json.Unmarshal(bodyBytes, &loadResp); errDecode != nil {
		return "", fmt.Errorf("decode response: %w", errDecode)
	}
	projectID := geminiProjectIDFromMap(loadResp)
	if projectID != "" {
		return projectID, nil
	}

	tierID := "legacy-tier"
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
				if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}
	return h.onboardAntigravityUser(ctx, accessToken, tierID)
}

// onboardAntigravityUser handles an onboard antigravity user.
func (h *Handler) onboardAntigravityUser(ctx context.Context, accessToken string, tierID string) (string, error) {
	// Validate request inputs before mutating persisted state.
	if ctx == nil {
		ctx = context.Background()
	}
	userAgent := antigravityOnboardUserUserAgent()
	requestBody := map[string]any{
		"tierId": strings.TrimSpace(tierID),
		"metadata": map[string]string{
			"ide_type":    "ANTIGRAVITY",
			"ide_version": antigravityVersionFromUserAgent(userAgent),
			"ide_name":    "antigravity",
		},
	}
	rawBody, errMarshal := json.Marshal(requestBody)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal request body: %w", errMarshal)
	}

	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		reqCtx := ctx
		if reqCtx == nil {
			reqCtx = context.Background()
		}
		reqCtx, cancel := context.WithTimeout(reqCtx, 30*time.Second)
		endpointURL := fmt.Sprintf("%s/%s:onboardUser", antigravityAPIEndpoint, antigravityAPIVersion)
		req, errRequest := http.NewRequestWithContext(reqCtx, http.MethodPost, endpointURL, bytes.NewReader(rawBody))
		if errRequest != nil {
			cancel()
			return "", fmt.Errorf("create request: %w", errRequest)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("X-Goog-Api-Client", antigravityGoogAPIClientUA)

		resp, errDo := h.oauthHTTPClient().Do(req)
		if errDo != nil {
			cancel()
			return "", fmt.Errorf("execute request: %w", errDo)
		}
		bodyBytes, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity onboardUser: response body close error: %v", errClose)
		}
		cancel()
		if errRead != nil {
			return "", fmt.Errorf("read response: %w", errRead)
		}
		if resp.StatusCode != http.StatusOK {
			responsePreview := strings.TrimSpace(string(bodyBytes))
			if len(responsePreview) > 200 {
				responsePreview = responsePreview[:200]
			}
			return "", fmt.Errorf("http %d: %s", resp.StatusCode, responsePreview)
		}
		var data map[string]any
		if errDecode := json.Unmarshal(bodyBytes, &data); errDecode != nil {
			return "", fmt.Errorf("decode response: %w", errDecode)
		}
		if done, okDone := data["done"].(bool); okDone && done {
			if responseData, okResp := data["response"].(map[string]any); okResp {
				projectID := geminiProjectIDFromMap(responseData)
				if projectID != "" {
					return projectID, nil
				}
			}
			return "", fmt.Errorf("no project_id in response")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", nil
}

// antigravityOnboardUserUserAgent preserves the legacy onboarding fingerprint.
func antigravityOnboardUserUserAgent() string {
	return fmt.Sprintf("antigravity/%s darwin/arm64 %s", antigravityFallbackVersion, antigravityNodeAPIClientUA)
}

// antigravityVersionFromUserAgent derives antigravity version from user agent.
func antigravityVersionFromUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	lower := strings.ToLower(userAgent)
	if !strings.HasPrefix(lower, "antigravity/") {
		return antigravityFallbackVersion
	}
	rest := userAgent[len("antigravity/"):]
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = rest[:idx]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return antigravityFallbackVersion
	}
	return rest
}

// generatePKCECodes generates a pkce codes.
func generatePKCECodes() (*pkceCodes, error) {
	verifier, errVerifier := randomURLSafe(96)
	if errVerifier != nil {
		return nil, errVerifier
	}
	hash := sha256.Sum256([]byte(verifier))
	return &pkceCodes{
		CodeVerifier:  verifier,
		CodeChallenge: base64.RawURLEncoding.EncodeToString(hash[:]),
	}, nil
}

// generateOAuthState generates an o auth state.
func generateOAuthState(prefix string) (string, error) {
	value, errState := randomURLSafe(24)
	if errState != nil {
		return "", errState
	}
	prefix = strings.Trim(prefix, "-_ .")
	if prefix == "" {
		return value, nil
	}
	return prefix + "-" + value, nil
}

// randomURLSafe handles a random url safe.
func randomURLSafe(size int) (string, error) {
	if size <= 0 {
		size = 32
	}
	raw := make([]byte, size)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// validateOAuthState validates an o auth state.
func validateOAuthState(state string) error {
	// Resolve credential context before calling upstream OAuth services.
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", errInvalidOAuthState)
	}
	if len(trimmed) > maxOAuthStateLength {
		return fmt.Errorf("%w: too long", errInvalidOAuthState)
	}
	if strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\") {
		return fmt.Errorf("%w: contains path separator", errInvalidOAuthState)
	}
	if strings.Contains(trimmed, "..") {
		return fmt.Errorf("%w: contains '..'", errInvalidOAuthState)
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("%w: invalid character", errInvalidOAuthState)
		}
	}
	return nil
}

// deviceFlowAuthView picks the client-facing verification URL and user code.
// verification_uri_complete is preferred. A bare verification_uri requires user_code.
func deviceFlowAuthView(completeURI, verificationURI, userCode string) (authURL, code string, err error) {
	code = strings.TrimSpace(userCode)
	completeURI = strings.TrimSpace(completeURI)
	verificationURI = strings.TrimSpace(verificationURI)
	if completeURI != "" {
		return completeURI, code, nil
	}
	if verificationURI != "" {
		if code == "" {
			return "", "", fmt.Errorf("device flow missing user code")
		}
		return verificationURI, code, nil
	}
	return "", "", fmt.Errorf("device flow missing verification url")
}

// normalizeOAuthProvider normalizes an o auth provider.
func normalizeOAuthProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "anthropic", nil
	case "codex", "openai":
		return "codex", nil
	case "antigravity", "anti-gravity":
		return "antigravity", nil
	case "kimi":
		return "kimi", nil
	case "kimi-ai":
		return "kimi-ai", nil
	case "xai", "x-ai", "x.ai", "grok":
		return "xai", nil
	case "devin":
		return "devin", nil
	case "meta", "muse":
		return "meta", nil
	default:
		return "", errUnsupportedProvider
	}
}

// authStatusMessage handles an auth status message.
func authStatusMessage(provider string, err error) string {
	if err == nil {
		return ""
	}
	switch provider {
	case "anthropic", "codex", "xai", "devin":
		return "Failed to exchange authorization code for tokens"
	case "antigravity":
		return "Failed to exchange token"
	default:
		return "Authentication failed"
	}
}

// oauthConfig handles an oauth config.
func (h *Handler) oauthConfig() *config.Config {
	if h != nil && h.runtime != nil {
		if cfg := h.runtime.Config(); cfg != nil {
			return cfg
		}
	}
	return &config.Config{}
}

// oauthHTTPClient handles an oauth http client.
func (h *Handler) oauthHTTPClient() *http.Client {
	if h != nil && h.oauthHTTPClientOverride != nil {
		return h.oauthHTTPClientOverride
	}
	cfg := h.oauthConfig()
	sdkCfg := cfg.SDKConfig
	if strings.TrimSpace(sdkCfg.ProxyURL) == "" {
		sdkCfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	return util.SetProxy(&sdkCfg, &http.Client{Timeout: 60 * time.Second})
}

// requestContextOrBackground handles a request context or background.
func requestContextOrBackground(c *gin.Context) context.Context {
	if c != nil && c.Request != nil && c.Request.Context() != nil {
		return c.Request.Context()
	}
	return context.Background()
}

// claudeCredentialFileName handles a claude credential file name.
func claudeCredentialFileName(email string) string {
	return fmt.Sprintf("claude-%s.json", strings.TrimSpace(email))
}

// codexCredentialFileName handles a codex credential file name.
func codexCredentialFileName(email, planType, hashAccountID string, includeProviderPrefix bool) string {
	email = strings.TrimSpace(email)
	plan := normalizePlanTypeForFilename(planType)
	prefix := ""
	if includeProviderPrefix {
		prefix = "codex"
	}
	if plan == "" {
		return fmt.Sprintf("%s-%s.json", prefix, email)
	}
	if plan == "team" {
		return fmt.Sprintf("%s-%s-%s-%s.json", prefix, hashAccountID, email, plan)
	}
	return fmt.Sprintf("%s-%s-%s.json", prefix, email, plan)
}

// normalizePlanTypeForFilename normalizes a plan type for filename.
func normalizePlanTypeForFilename(planType string) string {
	planType = strings.TrimSpace(planType)
	if planType == "" {
		return ""
	}
	parts := strings.FieldsFunc(planType, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(parts) == 0 {
		return ""
	}
	for i, part := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(part))
	}
	return strings.Join(parts, "-")
}

// antigravityCredentialFileName handles an antigravity credential file name.
func antigravityCredentialFileName(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "antigravity.json"
	}
	return fmt.Sprintf("antigravity-%s.json", email)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// oauthSessionStateKey binds credential persistence to the session that authorized it.
type oauthSessionStateKey struct{}

// oauthSessionContext stops in-flight acquisition when another Home node cancels the session.
// The database transaction remains the authority for credential persistence.
func (h *Handler) oauthSessionContext(parent context.Context, state string, timeout time.Duration) (context.Context, context.CancelFunc) {
	return h.oauthSessionContextUntil(parent, state, time.Now().Add(timeout))
}

func (h *Handler) oauthSessionContextUntil(parent context.Context, state string, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(context.WithValue(parent, oauthSessionStateKey{}, state), deadline)
	session, errSession := h.repo.GetOAuthSession(ctx, state)
	if errSession != nil || session == nil || session.Status != "" {
		cancel()
		return ctx, cancel
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, errCurrent := h.repo.GetOAuthSession(ctx, state)
				if errCurrent != nil || current == nil || current.Status == "error" {
					cancel()
					return
				}
				if current.Status == "complete" {
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}
