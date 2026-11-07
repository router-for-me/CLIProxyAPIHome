package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	log "github.com/sirupsen/logrus"
)

type codexDeviceAuthURLResponse struct {
	Status    string `json:"status"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Flow      string `json:"flow"`
	UserCode  string `json:"user_code"`
	ExpiresIn int    `json:"expires_in"`
}

// RequestCodexDeviceToken starts the Codex device authorization flow.
func (h *Handler) RequestCodexDeviceToken(c *gin.Context) {
	deadline := time.Now().Add(codex.DeviceFlowTimeout)
	// Keep proxy settings fixed across issuance, polling, and exchange.
	oauthClient := h.oauthHTTPClient()
	state, errState := generateOAuthState("cdx")
	if errState != nil {
		log.Errorf("cluster oauth: failed to generate codex device state: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to start device authorization"})
		return
	}
	if errRegister := h.registerOAuthSession(c, "codex", state, map[string]any{"flow": "device"}); errRegister != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errRegister)
		return
	}

	requestCtx, cancelRequest := context.WithDeadline(requestContextOrBackground(c), deadline)
	defer cancelRequest()
	deviceFlow, errDevice := codex.RequestDeviceCode(requestCtx, oauthClient)
	if errDevice != nil {
		if errors.Is(requestCtx.Err(), context.Canceled) {
			if errCancel := h.cancelCodexDeviceSession(state); errCancel != nil {
				log.Errorf("cluster oauth: failed to cancel disconnected codex device session: %v", errCancel)
			}
			return
		}
		h.failCodexDeviceStartup(state, errDevice)
		c.JSON(http.StatusBadGateway, gin.H{"status": "error", "error": "failed to start device authorization"})
		return
	}
	if errRequest := requestCtx.Err(); errRequest != nil {
		if errors.Is(errRequest, context.Canceled) {
			if errCancel := h.cancelCodexDeviceSession(state); errCancel != nil {
				log.Errorf("cluster oauth: failed to cancel disconnected codex device session: %v", errCancel)
			}
			return
		}
		h.failCodexDeviceStartup(state, errRequest)
		return
	}

	if errMerge := h.repo.MergeOAuthSessionData(requestCtx, state, map[string]any{
		"device_auth_id": deviceFlow.DeviceAuthID,
		"user_code":      deviceFlow.UserCode,
	}); errMerge != nil {
		if requestCtx.Err() != nil {
			if errCancel := h.cancelCodexDeviceSession(state); errCancel != nil {
				log.Errorf("cluster oauth: failed to cancel disconnected codex device session: %v", errCancel)
			}
			return
		}
		respondOAuthSessionMergeError(c, errMerge)
		h.failCodexDeviceStartup(state, errMerge)
		return
	}

	if !time.Now().Before(deadline) {
		h.failCodexDeviceStartup(state, context.DeadlineExceeded)
		c.JSON(http.StatusGatewayTimeout, gin.H{"status": "error", "error": "device authorization expired"})
		return
	}
	workerCtx, cancelWorker := h.oauthSessionContextUntil(context.Background(), state, deadline)
	if errWorker := workerCtx.Err(); errWorker != nil {
		cancelWorker()
		if errors.Is(errWorker, context.DeadlineExceeded) {
			h.failCodexDeviceStartup(state, errWorker)
			c.JSON(http.StatusGatewayTimeout, gin.H{"status": "error", "error": "device authorization expired"})
			return
		}
		if errCancel := h.cancelCodexDeviceSession(state); errCancel != nil {
			log.Errorf("cluster oauth: failed to cancel codex device session after worker startup failure: %v", errCancel)
		}
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "oauth flow is not pending"})
		return
	}

	worker := func() {
		defer cancelWorker()
		if errWait := h.waitForCodexDeviceAuthorization(workerCtx, oauthClient, state, deviceFlow); errWait != nil {
			if errorsIsContextDeadline(errWait, workerCtx.Err()) {
				h.setCodexDeviceSessionError(state, "Authentication failed")
				return
			}
			if workerCtx.Err() != nil {
				return
			}
			h.setCodexDeviceSessionError(state, "Authentication failed")
			log.Errorf("cluster oauth: codex device authorization failed: %s", logging.SafeErrorDiagnostic(errWait))
		}
	}

	expiresIn := int(time.Until(deadline).Seconds())
	if expiresIn <= 0 {
		cancelWorker()
		h.failCodexDeviceStartup(state, context.DeadlineExceeded)
		c.JSON(http.StatusGatewayTimeout, gin.H{"status": "error", "error": "device authorization expired"})
		return
	}
	response := codexDeviceAuthURLResponse{
		Status:    "ok",
		URL:       codex.DeviceVerificationURL,
		State:     state,
		Flow:      "device",
		UserCode:  deviceFlow.UserCode,
		ExpiresIn: expiresIn,
	}
	if errRequest := requestCtx.Err(); errRequest != nil {
		cancelWorker()
		if errCancel := h.cancelCodexDeviceSession(state); errCancel != nil {
			log.Errorf("cluster oauth: failed to cancel disconnected codex device session before response handoff: %v", errCancel)
		}
		return
	}
	if errWrite := writeCodexDeviceResponse(c, http.StatusOK, response); errWrite != nil {
		cancelWorker()
		if errCancel := h.cancelCodexDeviceSession(state); errCancel != nil {
			log.Errorf("cluster oauth: failed to cancel undelivered codex device session: %v", errCancel)
		}
		log.Errorf("cluster oauth: failed to deliver codex device authorization response: %v", errWrite)
		return
	}
	h.runCodexDeviceWorker(worker)
}

func (h *Handler) waitForCodexDeviceAuthorization(ctx context.Context, client *http.Client, state string, deviceFlow *codex.DeviceFlow) error {
	deviceToken, errWait := codex.WaitForDeviceAuthorization(ctx, client, deviceFlow)
	if errWait != nil {
		return errWait
	}
	if deviceToken == nil || deviceToken.AuthorizationCode == "" || deviceToken.CodeVerifier == "" || deviceToken.CodeChallenge == "" {
		return fmt.Errorf("codex device flow token response missing required fields")
	}
	return h.exchangeCodexCallbackWithClient(ctx, client, deviceToken.AuthorizationCode, map[string]any{
		"flow":           "device",
		"code_verifier":  deviceToken.CodeVerifier,
		"code_challenge": deviceToken.CodeChallenge,
	})
}

func (h *Handler) failCodexDeviceStartup(state string, err error) {
	if errSet := h.setCodexDeviceSessionError(state, "Authentication failed"); errSet != nil {
		log.Errorf("cluster oauth: failed to mark codex device startup failure: %v", errSet)
	}
	if err != nil {
		log.Errorf("cluster oauth: codex device flow startup failed: %s", logging.SafeErrorDiagnostic(err))
	}
}

func (h *Handler) setCodexDeviceSessionError(state, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.repo.SetOAuthSessionError(ctx, state, message)
}

func (h *Handler) cancelCodexDeviceSession(state string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, errCancel := h.repo.CancelOAuthSession(ctx, state)
	return errCancel
}

func writeCodexDeviceResponse(c *gin.Context, status int, response codexDeviceAuthURLResponse) error {
	body, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return errMarshal
	}
	body = append(body, '\n')
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.Writer.WriteHeader(status)
	written, errWrite := c.Writer.Write(body)
	if errWrite != nil {
		return errWrite
	}
	if written != len(body) {
		return fmt.Errorf("wrote %d of %d response bytes", written, len(body))
	}
	writer := http.ResponseWriter(c.Writer)
	if unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter }); ok {
		writer = unwrapper.Unwrap()
	}
	return http.NewResponseController(writer).Flush()
}

func errorsIsContextDeadline(err error, contextErr error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(contextErr, context.DeadlineExceeded)
}
