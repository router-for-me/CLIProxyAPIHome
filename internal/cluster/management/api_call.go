package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/proxyutil"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const defaultAPICallTimeout = 60 * time.Second

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

// APICall proxies a management API call through a stored auth entry.
func (h *Handler) APICall(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	var body apiCallRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing method"})
		return
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing url"})
		return
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}

	reqHeaders := body.Header
	if reqHeaders == nil {
		reqHeaders = map[string]string{}
	}

	ctx := context.Background()
	if c != nil && c.Request != nil && c.Request.Context() != nil {
		ctx = c.Request.Context()
	}

	authIndex := apiCallFirstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth, errAuth := h.apiCallAuthByIndex(ctx, authIndex)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}

	requiresToken := apiCallHeadersNeedToken(reqHeaders) || strings.Contains(body.Data, "$TOKEN$")
	if requiresToken {
		if authIndex == "" || auth == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth not found"})
			return
		}
		token, errToken := h.apiCallToken(ctx, authIndex, auth)
		if errToken != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth token refresh failed"})
			return
		}
		if token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth token not found"})
			return
		}
		for key, value := range reqHeaders {
			reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
		}
		body.Data = apiCallBodyWithToken(body.Data, token)
	}

	var requestBody io.Reader
	if body.Data != "" {
		requestBody = strings.NewReader(body.Data)
	}

	req, errNewRequest := http.NewRequestWithContext(ctx, method, urlStr, requestBody)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}

	hostOverride := ""
	for key, value := range reqHeaders {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		req.Header.Set(key, value)
	}
	if hostOverride != "" {
		req.Host = hostOverride
	}

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	if requiresToken {
		httpClient.CheckRedirect = proxyutil.SameOriginRedirectPolicy(nil)
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("cluster management APICall request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := io.ReadAll(resp.Body)
	if errReadAll != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	c.JSON(http.StatusOK, apiCallResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       string(respBody),
	})
}

// apiCallAuthByIndex handles an api call auth by index.
func (h *Handler) apiCallAuthByIndex(ctx context.Context, authIndex string) (*coreauth.Auth, error) {
	// Validate request inputs before mutating persisted state.
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil || h.repo == nil {
		return nil, nil
	}

	auth, _, errAuth := h.repo.GetAuth(ctx, authIndex)
	if errAuth == nil {
		auth.ID = strings.TrimSpace(auth.ID)
		if auth.ID == "" {
			auth.ID = authIndex
		}
		auth.Index = auth.ID
		return auth, nil
	}
	if !errors.Is(errAuth, gorm.ErrRecordNotFound) {
		return nil, errAuth
	}

	auths, errList := h.repo.ListAuths(ctx)
	if errList != nil {
		return nil, errList
	}
	for _, item := range auths {
		if item == nil {
			continue
		}
		if apiCallAuthMatches(item, authIndex) {
			return item, nil
		}
	}
	return nil, nil
}

// apiCallToken converts api call token.
func (h *Handler) apiCallToken(ctx context.Context, authIndex string, auth *coreauth.Auth) (string, error) {
	if token := apiCallTokenValueForAuth(auth); token != "" {
		return token, nil
	}
	if h == nil || h.runtime == nil || strings.TrimSpace(authIndex) == "" {
		return "", nil
	}

	payload, errRefresh := h.runtime.RefreshNow(ctx, authIndex)
	if errRefresh != nil {
		return "", errRefresh
	}
	return apiCallTokenValueFromRefreshPayload(payload), nil
}

// apiCallTransport handles an api call transport.
func (h *Handler) apiCallTransport(auth *coreauth.Auth) http.RoundTripper {
	// Validate request inputs before mutating persisted state.
	proxyCandidates := make([]string, 0, 2)
	if auth != nil {
		if proxyStr := apiCallAuthProxyURL(auth); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}
	if h != nil && h.runtime != nil {
		if cfg := h.runtime.Config(); cfg != nil {
			if proxyStr := strings.TrimSpace(cfg.ProxyURL); proxyStr != "" {
				proxyCandidates = append(proxyCandidates, proxyStr)
			}
		}
	}

	for _, proxyStr := range proxyCandidates {
		transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
		if errBuild != nil {
			log.WithError(errBuild).Debug("cluster management APICall proxy transport failed")
			continue
		}
		if transport != nil {
			return transport
		}
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		return &http.Transport{Proxy: nil}
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return clone
}

// apiCallAuthMatches reports whether an API call auth selector refers to auth.
func apiCallAuthMatches(auth *coreauth.Auth, authIndex string) bool {
	if auth == nil {
		return false
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return false
	}
	for _, candidate := range []string{
		strings.TrimSpace(auth.ID),
		strings.TrimSpace(auth.Index),
		strings.TrimSpace(auth.EnsureIndex()),
		authFileDisplayName(auth),
		authFileName(auth),
		strings.TrimSuffix(authFileName(auth), ".json"),
	} {
		if strings.TrimSpace(candidate) == authIndex {
			return true
		}
	}
	return false
}

// apiCallAuthProxyURL returns the effective auth-scoped proxy URL.
func apiCallAuthProxyURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
		return proxyStr
	}
	if auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"proxy_url", "proxy-url"} {
		if proxyStr := stringFromAny(auth.Metadata[key]); proxyStr != "" {
			return proxyStr
		}
	}
	return ""
}

// apiCallHeadersNeedToken converts api call headers need token.
func apiCallHeadersNeedToken(headers map[string]string) bool {
	for _, value := range headers {
		if strings.Contains(value, "$TOKEN$") {
			return true
		}
	}
	return false
}

func apiCallBodyWithToken(data, token string) string {
	if !strings.Contains(data, "$TOKEN$") {
		return data
	}
	replacement := token
	if json.Valid([]byte(data)) {
		if encoded, errMarshal := json.Marshal(token); errMarshal == nil && len(encoded) >= 2 {
			replacement = string(encoded[1 : len(encoded)-1])
		}
	}
	return strings.ReplaceAll(data, "$TOKEN$", replacement)
}

// apiCallFirstNonEmptyString handles an api call first non empty string.
func apiCallFirstNonEmptyString(values ...*string) string {
	for _, value := range values {
		if value == nil {
			continue
		}
		if out := strings.TrimSpace(*value); out != "" {
			return out
		}
	}
	return ""
}

// apiCallTokenValueForAuth converts api call token value for auth.
func apiCallTokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := apiCallTokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	return ""
}

// apiCallTokenValueFromRefreshPayload converts api call token value from refresh payload.
func apiCallTokenValueFromRefreshPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var body struct {
		Auth struct {
			Attributes map[string]string `json:"attributes"`
			Metadata   map[string]any    `json:"metadata"`
		} `json:"auth"`
	}
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return ""
	}
	auth := &coreauth.Auth{
		Attributes: body.Auth.Attributes,
		Metadata:   body.Auth.Metadata,
	}
	return apiCallTokenValueForAuth(auth)
}

// apiCallTokenValueFromMetadata converts api call token value from metadata.
func apiCallTokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"accessToken", "access_token"} {
		if v := stringFromAny(metadata[key]); v != "" {
			return v
		}
	}
	for _, key := range []string{"token", "Token"} {
		if v := apiCallTokenValueFromNestedToken(metadata[key]); v != "" {
			return v
		}
	}
	for _, key := range []string{"id_token", "cookie"} {
		if v := stringFromAny(metadata[key]); v != "" {
			return v
		}
	}
	for _, key := range []string{"api_key", "session_token"} {
		if v := stringFromAny(metadata[key]); v != "" {
			return v
		}
	}
	return ""
}

// apiCallTokenValueFromNestedToken converts api call token value from nested token.
func apiCallTokenValueFromNestedToken(tokenRaw any) string {
	switch typed := tokenRaw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, key := range []string{"access_token", "accessToken"} {
			if v := stringFromAny(typed[key]); v != "" {
				return v
			}
		}
	case map[string]string:
		for _, key := range []string{"access_token", "accessToken"} {
			if v := strings.TrimSpace(typed[key]); v != "" {
				return v
			}
		}
	}
	return ""
}
