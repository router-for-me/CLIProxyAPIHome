package get

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errRuntimeNotReady = errors.New("runtime not ready")

// handleDefault handles a default.
func handleDefault(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	// Validate request inputs before mutating persisted state.
	if env.Runtime == nil {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageRuntimeNotReady)))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) != 2 {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageWrongNumberOfArgumentsGet)))
	}

	jsonArg := strings.TrimSpace(args[1])
	if !looksLikeJSONObject(jsonArg) {
		value, found, errGet := env.Runtime.KVGet(ctx, args[1])
		if errGet != nil {
			return dispatch.Err(errGet.Error())
		}
		if !found {
			return dispatch.BulkString(nil)
		}
		return dispatch.BulkString(value)
	}
	if jsonArg == "" || !gjson.Valid(jsonArg) {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageInvalidRequestJSON)))
	}
	typeValue := strings.ToLower(strings.TrimSpace(gjson.Get(jsonArg, "type").String()))

	switch typeValue {
	case "models":
		return handleDefaultModels(ctx, env, jsonArg)
	case "refresh":
		return handleDefaultRefresh(ctx, env, jsonArg)
	default:
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageUnsupportedType)))
	}
}

// handleDefaultModels authenticates the downstream client and returns the models payload.
func handleDefaultModels(ctx context.Context, env dispatch.Env, jsonArg string) dispatch.Reply {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if errReq != nil {
		return dispatch.BulkString([]byte(buildErrorJSON(errReq.Error())))
	}
	req.Header = parseHeaders(jsonArg)
	req.URL.RawQuery = parseQuery(jsonArg).Encode()

	if _, authErr := env.Runtime.AuthenticateHTTPRequest(ctx, req); authErr != nil {
		return dispatch.BulkString([]byte(buildAuthErrorJSON(authErr)))
	}

	payload, errBuild := buildModelsJSON(env)
	if errBuild != nil {
		return dispatch.BulkString([]byte(buildErrorJSON(errBuild.Error())))
	}
	return dispatch.BulkString([]byte(payload))
}

// handleDefaultRefresh handles a refresh.
func handleDefaultRefresh(ctx context.Context, env dispatch.Env, jsonArg string) dispatch.Reply {
	authIndex := strings.TrimSpace(gjson.Get(jsonArg, "auth_index").String())
	if authIndex == "" {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageMissingAuthIndex)))
	}

	observedAccessTokenSHA256 := strings.TrimSpace(gjson.Get(jsonArg, "access_token_sha256").String())
	payload, errRefresh := env.Runtime.RefreshNowObserved(ctx, authIndex, observedAccessTokenSHA256)
	if errRefresh != nil {
		return dispatch.BulkString([]byte(buildRefreshErrorJSON(errRefresh)))
	}
	if len(payload) == 0 {
		return dispatch.BulkString([]byte(buildErrorJSON(homeerrors.MessageAuthNotFound)))
	}
	return dispatch.BulkString(payload)
}

func buildRefreshErrorJSON(errRefresh error) string {
	var authErr *coreauth.Error
	if errors.As(errRefresh, &authErr) && authErr != nil {
		errorType := strings.TrimSpace(authErr.Code)
		if errorType == "" {
			errorType = "error"
		}
		message := strings.TrimSpace(authErr.Message)
		if message == "" {
			message = "credential refresh failed"
		}
		return buildTypedErrorJSON(errorType, message)
	}
	if errRefresh != nil && strings.Contains(strings.ToLower(errRefresh.Error()), "auth not found") {
		return buildTypedErrorJSON("auth_not_found", "credential not found")
	}
	return buildTypedErrorJSON("refresh_temporarily_unavailable", "credential refresh temporarily unavailable")
}

func buildTypedErrorJSON(errorType, message string) string {
	out := "{}"
	out, _ = sjson.Set(out, "error.type", errorType)
	out, _ = sjson.Set(out, "error.message", message)
	return out
}

func looksLikeJSONObject(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 2 && value[0] == '{' && value[len(value)-1] == '}'
}

// buildAuthErrorJSON renders an access error as the standard {error:{type,message}} envelope,
// preserving the structured access error code (e.g. no_credentials, invalid_credential) so the
// downstream proxy can map it to the correct HTTP status.
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

// buildErrorJSON builds a error json.
func buildErrorJSON(message string) string {
	errorType, errorMessage := homeerrors.SplitRedisErrorMessage(message)
	out := "{}"
	out, _ = sjson.Set(out, "error.type", errorType)
	out, _ = sjson.Set(out, "error.message", errorMessage)
	return out
}
