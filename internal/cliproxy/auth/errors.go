package auth

// UpstreamResponse contains the exact HTTP response returned by a provider.
type UpstreamResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
}

// Error describes an authentication related failure in a provider agnostic format.
type Error struct {
	// Code is a short machine readable identifier.
	Code string `json:"code,omitempty"`
	// Message is a human readable description of the failure.
	Message string `json:"message"`
	// Diagnostic is a redacted internal summary for trusted transports and logs.
	Diagnostic string `json:"diagnostic,omitempty"`
	// Retryable indicates whether a retry might fix the issue automatically.
	Retryable bool `json:"retryable"`
	// HTTPStatus optionally records an HTTP-like status code for the error.
	HTTPStatus int `json:"http_status,omitempty"`
	// Upstream preserves the provider response independently from internal
	// scheduling classification.
	Upstream *UpstreamResponse `json:"upstream,omitempty"`
}

// Error returns the error message.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Upstream != nil {
		return string(e.Upstream.Body)
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// Is preserves sentinel matching for structured unsupported-refresh errors.
func (e *Error) Is(target error) bool {
	return e != nil && target == ErrRefreshUnsupported && e.Code == refreshUnsupportedCode
}

// StatusCode returns the HTTP status code.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}
