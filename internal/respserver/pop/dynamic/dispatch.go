package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	typeAuth         = "auth"
	typeAuthValidate = "auth-validate"
)

// Register wires package handlers into the provided registry.
func Register(reg *dispatch.Registry) {
	if reg == nil {
		return
	}
	_ = reg.RegisterDynamic("RPOP", typeAuth, handleAuth)
	_ = reg.RegisterDynamic("RPOP", typeAuthValidate, handleAuthValidate)
	_ = reg.SetDynamicDefault("RPOP", handleAuth)
}

type authValidateResponse struct {
	Authenticated bool               `json:"authenticated"`
	Provider      string             `json:"provider,omitempty"`
	Principal     string             `json:"principal,omitempty"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	Error         *authValidateError `json:"error,omitempty"`
}

type authValidateError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type dispatchConcurrencyRequest struct {
	Protocol int `json:"concurrency_protocol"`
}

type dispatchConcurrencyResponse struct {
	Accounted    bool   `json:"accounted"`
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
}

// handleAuthValidate validates a downstream API key without dispatching auth.
func handleAuthValidate(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if env.Runtime == nil {
		return authValidateErrorReply("auth_unavailable", homeerrors.MessageRuntimeNotReady)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	jsonArg, ok := dispatch.ExtractJSONArgument(args, 1)
	if !ok {
		return authValidateErrorReply("invalid_request", homeerrors.MessageWrongNumberOfArgumentsRPOP)
	}
	jsonArg = strings.TrimSpace(jsonArg)
	if jsonArg == "" || !gjson.Valid(jsonArg) {
		return authValidateErrorReply("invalid_request", homeerrors.MessageInvalidRequestJSON)
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if errReq != nil {
		return authValidateErrorReply(string(access.AuthErrorCodeInternal), errReq.Error())
	}
	req.Header = parseHeaders(jsonArg)
	req.URL.RawQuery = parseQuery(jsonArg).Encode()

	authRes, authErr := env.Runtime.AuthenticateHTTPRequest(ctx, req)
	if authErr != nil {
		return authValidateErrorReply(string(authErr.Code), authErr.Message)
	}
	if authRes == nil {
		return authValidateErrorReply(string(access.AuthErrorCodeInvalidCredential), "Invalid API key")
	}

	return authValidateJSONReply(authValidateResponse{
		Authenticated: true,
		Provider:      strings.TrimSpace(authRes.Provider),
		Principal:     strings.TrimSpace(authRes.Principal),
		Metadata:      cloneMetadata(authRes.Metadata),
	})
}

// handleAuth handles an auth.
func handleAuth(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	result, _, errReply := dispatchRequest(ctx, env, args)
	if errReply != nil {
		return *errReply
	}
	if result == nil {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageNoDispatchResult)))
	}
	payload := result.UnaccountedReply
	if result.Concurrency.Accounted {
		payload = result.AccountedReply
	}
	reply := dispatch.BulkString(payload)
	reply.AccountedAdmission = result.Concurrency.Accounted
	if result.AdmissionFenceFailure != nil {
		reply.PreWriteError = result.AdmissionFenceFailure
		return reply
	}
	if result.Concurrency.Accounted && env.Conn != nil && env.Conn.AccountedReplyFailure != nil {
		reply.PreWriteError = env.Conn.AccountedReplyFailure()
	}
	return reply
}

func prepareDispatchResponse(result *home.DispatchResult, userAPIKey string) ([]byte, []byte, error) {
	if result == nil || result.Auth == nil {
		return nil, nil, errors.New(homeerrors.MessageNoAuthAvailable)
	}
	auth := home.SanitizeAuthForDownstream(result.Auth)
	if auth == nil {
		return nil, nil, errors.New(homeerrors.MessageNoAuthAvailable)
	}
	authJSON, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		return nil, nil, errMarshal
	}
	authIndex := strings.TrimSpace(auth.EnsureIndex())
	if authIndex == "" {
		return nil, nil, errors.New(homeerrors.MessageNoAuthAvailable)
	}
	build := func(accounted bool) ([]byte, error) {
		out := []byte("{}")
		set := func(path string, value any) error {
			var errSet error
			out, errSet = sjson.SetBytes(out, path, value)
			return errSet
		}
		if errSet := set("model", strings.TrimSpace(result.Model)); errSet != nil {
			return nil, errSet
		}
		if errSet := set("provider", strings.TrimSpace(result.Provider)); errSet != nil {
			return nil, errSet
		}
		if errSet := set("request_retry", result.RequestRetry); errSet != nil {
			return nil, errSet
		}
		if result.ForceMapping && strings.TrimSpace(result.OriginalAlias) != "" {
			if errSet := set("force_mapping", true); errSet != nil {
				return nil, errSet
			}
			if errSet := set("original_alias", strings.TrimSpace(result.OriginalAlias)); errSet != nil {
				return nil, errSet
			}
		}
		if result.ModelInfo != nil {
			if errSet := set("model_info", result.ModelInfo); errSet != nil {
				return nil, errSet
			}
		}
		if errSet := set("auth_index", authIndex); errSet != nil {
			return nil, errSet
		}
		if errSet := set("user_api_key", strings.TrimSpace(userAPIKey)); errSet != nil {
			return nil, errSet
		}
		var errSetAuth error
		out, errSetAuth = sjson.SetRawBytes(out, "auth", authJSON)
		if errSetAuth != nil {
			return nil, errSetAuth
		}
		if accounted {
			out, errSetAuth = sjson.SetBytes(out, "concurrency", dispatchConcurrencyResponse{Accounted: true, CredentialID: result.Concurrency.CredentialID, Model: result.Concurrency.Model})
			if errSetAuth != nil {
				return nil, errSetAuth
			}
		}
		return out, nil
	}
	unaccounted, errUnaccounted := build(false)
	if errUnaccounted != nil {
		return nil, nil, errUnaccounted
	}
	accounted, errAccounted := build(true)
	if errAccounted != nil {
		return nil, nil, errAccounted
	}
	return unaccounted, accounted, nil
}

// dispatchRequest handles a dispatch request.
func dispatchRequest(ctx context.Context, env dispatch.Env, args []string) (*home.DispatchResult, string, *dispatch.Reply) {
	// Build the candidate view before applying availability rules.
	if env.Runtime == nil {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageRuntimeNotReady)))
		return nil, "", &reply
	}
	if ctx == nil {
		ctx = context.Background()
	}

	jsonArg, ok := dispatch.ExtractJSONArgument(args, 1)
	if !ok {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageWrongNumberOfArgumentsRPOP)))
		return nil, "", &reply
	}
	jsonArg = strings.TrimSpace(jsonArg)
	if jsonArg == "" || !gjson.Valid(jsonArg) {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageInvalidRequestJSON)))
		return nil, "", &reply
	}

	model := strings.TrimSpace(gjson.Get(jsonArg, "model").String())
	if model == "" {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageMissingModel)))
		return nil, "", &reply
	}
	credentialPolicy, errPolicy := dispatchCredentialPolicy(jsonArg)
	if errPolicy != nil {
		reply := dispatch.BulkString([]byte(buildErrorJSON(errPolicy.Error())))
		return nil, "", &reply
	}
	count := dispatchCount(jsonArg)
	retryRound, retryRoundPresent, validRetryRound := dispatchRetryRound(jsonArg)
	if !validRetryRound {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageInvalidRequestJSON)))
		return nil, "", &reply
	}
	excludedAuthIDs, hasExcludedAuthIDs, validExcludedAuthIDs := dispatchExcludedAuthIDs(jsonArg)
	if !validExcludedAuthIDs {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageInvalidRequestJSON)))
		return nil, "", &reply
	}
	pinnedAuthID, validPinnedAuthID := dispatchPinnedAuthID(jsonArg)
	if !validPinnedAuthID {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageInvalidRequestJSON)))
		return nil, "", &reply
	}

	headers := parseHeaders(jsonArg)
	sessionID := strings.TrimSpace(gjson.Get(jsonArg, "session_id").String())
	if sessionID != "" && strings.TrimSpace(headers.Get("X-Session-ID")) == "" {
		headers.Set("X-Session-ID", sessionID)
	}
	authRes, authErr := env.Runtime.Authenticate(ctx, headers)
	if authErr != nil {
		reply := dispatch.BulkString([]byte(buildAuthErrorJSON(authErr)))
		return nil, "", &reply
	}

	userAPIKey := ""
	if authRes != nil {
		userAPIKey = authRes.Principal
	}

	// Older CPA nodes use count as the Home-side retry limit. Keep that
	// contract when the new per-round exclusion field is absent.
	if !hasExcludedAuthIDs && dispatchRetryExceeded(env.Runtime, count) {
		reply := dispatch.BulkString([]byte(buildErrorJSON(homeerrors.TypeRequestRetryExceeded + ": " + homeerrors.MessageRequestRetryExceeded)))
		return nil, "", &reply
	}

	concurrencyReq := dispatchConcurrencyRequest{Protocol: int(gjson.Get(jsonArg, "concurrency_protocol").Int())}
	result, errDispatch := env.Runtime.DispatchForAPIKeyWithConcurrency(ctx, model, headers, userAPIKey, credentialPolicy, excludedAuthIDs, pinnedAuthID, home.DispatchConcurrencyContext{
		Fingerprint:       env.ConnectionLifetime.Fingerprint,
		ConnectedAt:       env.ConnectionLifetime.ConnectedAt,
		Controlled:        env.ConnectionLifetime.Controlled,
		ProtocolVersion:   concurrencyReq.Protocol,
		RetryRound:        retryRound,
		RetryRoundPresent: retryRoundPresent,
		PrepareResponse: func(result *home.DispatchResult) ([]byte, []byte, error) {
			unaccounted, accounted, errPrepare := prepareDispatchResponse(result, userAPIKey)
			if errPrepare != nil {
				return nil, nil, errPrepare
			}
			if env.Conn != nil && env.Conn.PrepareDispatchReply != nil {
				if errHook := env.Conn.PrepareDispatchReply(); errHook != nil {
					return nil, nil, errHook
				}
			}
			return unaccounted, accounted, nil
		},
	})
	if errDispatch != nil {
		reply := dispatch.BulkString([]byte(buildDispatchErrorJSON(env.Runtime, errDispatch)))
		return nil, "", &reply
	}

	return result, userAPIKey, nil
}

func dispatchRetryRound(jsonArg string) (int, bool, bool) {
	value := gjson.Get(jsonArg, "retry_round")
	if !value.Exists() {
		return 0, false, true
	}
	if value.Type != gjson.Number {
		return 0, true, false
	}
	raw := strings.TrimSpace(value.Raw)
	parsed, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || parsed < 0 || parsed > int64(^uint(0)>>1) {
		return 0, true, false
	}
	return int(parsed), true, true
}

// dispatchCount handles a dispatch count.
func dispatchCount(jsonArg string) int {
	count := int(gjson.Get(jsonArg, "count").Int())
	if count <= 0 {
		return 1
	}
	return count
}

func dispatchExcludedAuthIDs(jsonArg string) ([]string, bool, bool) {
	values := gjson.Get(jsonArg, "excluded_auth_ids")
	if !values.Exists() {
		return nil, false, true
	}
	if !values.IsArray() {
		return nil, true, false
	}
	seen := make(map[string]struct{})
	excluded := make([]string, 0, len(values.Array()))
	valid := true
	values.ForEach(func(_, value gjson.Result) bool {
		if value.Type != gjson.String {
			valid = false
			return false
		}
		authID := strings.TrimSpace(value.String())
		if authID == "" {
			return true
		}
		if _, ok := seen[authID]; ok {
			return true
		}
		seen[authID] = struct{}{}
		excluded = append(excluded, authID)
		return true
	})
	if !valid {
		return nil, true, false
	}
	return excluded, true, true
}

func dispatchPinnedAuthID(jsonArg string) (string, bool) {
	value := gjson.Get(jsonArg, "pinned_auth_id")
	if !value.Exists() || value.Type == gjson.Null {
		return "", true
	}
	if value.Type != gjson.String {
		return "", false
	}
	return strings.TrimSpace(value.String()), true
}

func dispatchCredentialPolicy(jsonArg string) (string, error) {
	policy := strings.TrimSpace(gjson.Get(jsonArg, "credential_policy").String())
	normalized, okPolicy := coreauth.NormalizeCredentialPolicy(policy)
	if !okPolicy {
		return "", fmt.Errorf("unsupported_credential_policy: unsupported credential policy %q", policy)
	}
	return normalized, nil
}

// dispatchRetryExceeded preserves the legacy count-based contract for older
// CPA nodes that do not send per-round exclusions.
func dispatchRetryExceeded(rt *home.Runtime, count int) bool {
	if count <= 1 || rt == nil {
		return false
	}
	cfg := rt.Config()
	if cfg == nil {
		return false
	}
	requestRetry := cfg.RequestRetry
	if requestRetry < 0 {
		requestRetry = 0
	}
	return count-2 >= requestRetry
}

// parseHeaders parses a headers.
func parseHeaders(jsonArg string) http.Header {
	// Decode the wire frame before dispatching command handling.
	headersObj := gjson.Get(jsonArg, "headers")
	headers := http.Header{}
	if !headersObj.Exists() || !headersObj.IsObject() {
		return headers
	}

	headersObj.ForEach(func(k, v gjson.Result) bool {
		key := strings.TrimSpace(k.String())
		if key == "" {
			return true
		}

		if v.Type == gjson.String {
			headers.Add(key, v.String())
			return true
		}

		if v.IsArray() {
			v.ForEach(func(_, entry gjson.Result) bool {
				if entry.Type == gjson.String {
					headers.Add(key, entry.String())
					return true
				}
				if entry.Type != gjson.Null {
					headers.Add(key, entry.String())
				}
				return true
			})
			return true
		}

		if v.Type != gjson.Null {
			headers.Add(key, v.String())
		}
		return true
	})
	return headers
}

func parseQuery(jsonArg string) url.Values {
	queryObj := gjson.Get(jsonArg, "query")
	query := url.Values{}
	if !queryObj.Exists() || !queryObj.IsObject() {
		return query
	}

	queryObj.ForEach(func(k, v gjson.Result) bool {
		key := strings.TrimSpace(k.String())
		if key == "" {
			return true
		}
		if v.Type == gjson.String {
			query.Add(key, v.String())
			return true
		}
		if v.IsArray() {
			v.ForEach(func(_, entry gjson.Result) bool {
				if entry.Type == gjson.String {
					query.Add(key, entry.String())
					return true
				}
				if entry.Type != gjson.Null {
					query.Add(key, entry.String())
				}
				return true
			})
			return true
		}
		if v.Type != gjson.Null {
			query.Add(key, v.String())
		}
		return true
	})
	return query
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func authValidateErrorReply(errorType string, message string) dispatch.Reply {
	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		errorType = string(access.AuthErrorCodeInternal)
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "authentication error"
	}
	return authValidateJSONReply(authValidateResponse{
		Authenticated: false,
		Error: &authValidateError{
			Type:    errorType,
			Message: message,
		},
	})
}

func authValidateJSONReply(resp authValidateResponse) dispatch.Reply {
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		return dispatch.BulkString([]byte(buildErrorJSON(errMarshal.Error())))
	}
	return dispatch.BulkString(raw)
}

// buildErrorJSON builds an error json.
func buildErrorJSON(message string) string {
	errorType, errorMessage := homeerrors.SplitRedisErrorMessage(message)
	out := "{}"
	out, _ = sjson.Set(out, "error.type", errorType)
	out, _ = sjson.Set(out, "error.message", errorMessage)
	return out
}

// buildAuthErrorJSON renders an access error as the standard {error:{type,message}} envelope,
// preserving the structured access error code (e.g. no_credentials, invalid_credential) so the
// downstream proxy can map it to the correct HTTP status.
func buildDispatchErrorJSON(runtime *home.Runtime, errDispatch error) string {
	var admissionErr *cluster.ConcurrencyAdmissionError
	if errors.As(errDispatch, &admissionErr) {
		out := "{}"
		out, _ = sjson.Set(out, "error.type", admissionErr.Type)
		out, _ = sjson.Set(out, "error.message", concurrencyErrorMessage(admissionErr.Type))
		if cluster.IsConcurrencySaturated(admissionErr) {
			out, _ = sjson.Set(out, "error.retryable", true)
			out, _ = sjson.Set(out, "error.retry_after_ms", concurrencyRetryAfterMS(runtime, admissionErr.RetryAfterMS))
		}
		return out
	}
	var retryAfterErr interface {
		ErrorCode() string
		ErrorMessage() string
		RetryAfter() *time.Duration
	}
	if errors.As(errDispatch, &retryAfterErr) && retryAfterErr != nil {
		out := "{}"
		out, _ = sjson.Set(out, "error.type", retryAfterErr.ErrorCode())
		out, _ = sjson.Set(out, "error.message", retryAfterErr.ErrorMessage())
		out, _ = sjson.Set(out, "error.retryable", true)
		if retryAfter := retryAfterErr.RetryAfter(); retryAfter != nil && *retryAfter > 0 {
			retryAfterMS := retryAfter.Milliseconds()
			if *retryAfter%time.Millisecond != 0 {
				retryAfterMS++
			}
			out, _ = sjson.Set(out, "error.retry_after_ms", retryAfterMS)
		}
		var retryLimitErr interface {
			RequestRetryLimit() (int, bool)
		}
		if errors.As(errDispatch, &retryLimitErr) && retryLimitErr != nil {
			if retryLimit, ok := retryLimitErr.RequestRetryLimit(); ok && retryLimit >= 0 {
				out, _ = sjson.Set(out, "error.request_retry", retryLimit)
			}
		}
		return out
	}
	return buildErrorJSON(errDispatch.Error())
}

func concurrencyErrorMessage(errorType string) string {
	if errorType == "credential_model_concurrency_exceeded" {
		return "credential model concurrency limit exceeded"
	}
	return "credential concurrency limit reached"
}

func wholePositiveMilliseconds(value string) (int64, bool) {
	duration, errDuration := time.ParseDuration(value)
	if errDuration != nil || duration <= 0 || duration%time.Millisecond != 0 {
		return 0, false
	}
	return duration.Milliseconds(), true
}

func concurrencyRetryAfterMS(runtime *home.Runtime, retryAfterMS int64) int64 {
	if retryAfterMS > 0 {
		return retryAfterMS
	}
	min, max := int64(250), int64(1000)
	if runtime != nil {
		if runtimeConfig := runtime.Config(); runtimeConfig != nil {
			cfg := runtimeConfig.CredentialConcurrency
			if parsedMin, okMin := wholePositiveMilliseconds(cfg.BusyRetryMin); okMin {
				min = parsedMin
			}
			if parsedMax, okMax := wholePositiveMilliseconds(cfg.BusyRetryMax); okMax {
				max = parsedMax
			}
		}
	}
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	return min + time.Now().UnixNano()%(max-min+1)
}

func buildAuthErrorJSON(authErr *access.AuthError) string {
	errorType := string(access.AuthErrorCodeInternal)
	message := "authentication error"
	if authErr != nil {
		if code := strings.TrimSpace(string(authErr.Code)); code != "" {
			errorType = code
		}
		if msg := strings.TrimSpace(authErr.Message); msg != "" {
			message = msg
		}
	}
	out := "{}"
	out, _ = sjson.Set(out, "error.type", errorType)
	out, _ = sjson.Set(out, "error.message", message)
	return out
}
