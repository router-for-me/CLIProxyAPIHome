// Package oautherror provides OAuth response diagnostics.
package oautherror

import (
	"encoding/json"
)

// ResponseError preserves an upstream OAuth error response while exposing
// parsed fields for refresh-failure classification.
type ResponseError struct {
	statusCode       int
	body             []byte
	oauthError       string
	code             string
	errorDescription string
	message          string
	detail           string
	requestID        string
}

// NewResponseError extracts diagnostics from an OAuth error response.
func NewResponseError(statusCode int, body []byte) *ResponseError {
	result := &ResponseError{
		statusCode: statusCode,
		body:       append([]byte(nil), body...),
	}
	var payload struct {
		Error            json.RawMessage `json:"error"`
		Code             json.RawMessage `json:"code"`
		ErrorDescription json.RawMessage `json:"error_description"`
		Message          json.RawMessage `json:"message"`
		Detail           json.RawMessage `json:"detail"`
		RequestID        json.RawMessage `json:"request_id"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return result
	}

	result.oauthError = diagnosticString(payload.Error)
	result.code = diagnosticString(payload.Code)
	result.errorDescription = diagnosticString(payload.ErrorDescription)
	result.message = diagnosticString(payload.Message)
	result.detail = diagnosticString(payload.Detail)
	result.requestID = diagnosticString(payload.RequestID)

	if result.oauthError == "" && len(payload.Error) > 0 {
		var nested struct {
			Error            json.RawMessage `json:"error"`
			Type             json.RawMessage `json:"type"`
			Code             json.RawMessage `json:"code"`
			ErrorDescription json.RawMessage `json:"error_description"`
			Message          json.RawMessage `json:"message"`
			Detail           json.RawMessage `json:"detail"`
			RequestID        json.RawMessage `json:"request_id"`
		}
		if errNested := json.Unmarshal(payload.Error, &nested); errNested == nil {
			result.oauthError = firstDiagnosticString(nested.Error, nested.Type)
			if result.code == "" {
				result.code = diagnosticString(nested.Code)
			}
			if result.errorDescription == "" {
				result.errorDescription = diagnosticString(nested.ErrorDescription)
			}
			if result.message == "" {
				result.message = diagnosticString(nested.Message)
			}
			if result.detail == "" {
				result.detail = diagnosticString(nested.Detail)
			}
			if result.requestID == "" {
				result.requestID = diagnosticString(nested.RequestID)
			}
		}
	}
	return result
}

// Error returns the upstream response body unchanged.
func (e *ResponseError) Error() string {
	if e == nil {
		return "upstream request failed"
	}
	return string(e.body)
}

// StatusCode returns the upstream HTTP response status.
func (e *ResponseError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

// ResponseBody returns a copy of the upstream HTTP response body.
func (e *ResponseError) ResponseBody() []byte {
	if e == nil {
		return nil
	}
	return append([]byte(nil), e.body...)
}

// OAuthError returns the OAuth error identifier.
func (e *ResponseError) OAuthError() string {
	if e == nil {
		return ""
	}
	return e.oauthError
}

// Code returns the provider error code.
func (e *ResponseError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

// ErrorDescription returns the OAuth error description.
func (e *ResponseError) ErrorDescription() string {
	if e == nil {
		return ""
	}
	return e.errorDescription
}

// Message returns the provider message.
func (e *ResponseError) Message() string {
	if e == nil {
		return ""
	}
	return e.message
}

// Detail returns the provider detail.
func (e *ResponseError) Detail() string {
	if e == nil {
		return ""
	}
	return e.detail
}

func firstDiagnosticString(values ...json.RawMessage) string {
	for _, value := range values {
		if result := diagnosticString(value); result != "" {
			return result
		}
	}
	return ""
}

func diagnosticString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		var number json.Number
		if errNumber := json.Unmarshal(raw, &number); errNumber != nil {
			return ""
		}
		value = number.String()
	}
	return value
}
