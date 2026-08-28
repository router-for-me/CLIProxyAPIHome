package oautherror

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewResponseErrorPreservesStatusAndBodyExactly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{name: "json", body: []byte(`{"error":"invalid_request","message":"retry later"}`)},
		{name: "text", body: []byte("provider request rejected")},
		{name: "multiline", body: []byte("first line\r\nsecond line\n")},
		{name: "empty", body: []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]byte(nil), tt.body...)
			errResponse := NewResponseError(429, input)
			if got := errResponse.StatusCode(); got != 429 {
				t.Fatalf("StatusCode() = %d, want 429", got)
			}
			if got := []byte(errResponse.Error()); !bytes.Equal(got, tt.body) {
				t.Fatalf("Error() = %q, want exact body %q", got, tt.body)
			}
			if got := errResponse.ResponseBody(); !bytes.Equal(got, tt.body) {
				t.Fatalf("ResponseBody() = %q, want %q", got, tt.body)
			}
			if len(input) > 0 {
				input[0] ^= 0xff
				if got := errResponse.ResponseBody(); !bytes.Equal(got, tt.body) {
					t.Fatalf("stored body changed with caller input: %q", got)
				}
			}
			returned := errResponse.ResponseBody()
			if len(returned) > 0 {
				returned[0] ^= 0xff
				if got := errResponse.ResponseBody(); !bytes.Equal(got, tt.body) {
					t.Fatalf("stored body changed through ResponseBody result: %q", got)
				}
			}
		})
	}
}

func TestNewResponseErrorParsesClassificationFieldsWithoutChangingBody(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"error":{"type":"invalid_grant","code":42901,"message":"Refresh token expired","detail":"sign in again","request_id":"req-nested"}
	}`)
	errResponse := NewResponseError(400, body)
	if got := errResponse.Error(); got != string(body) {
		t.Fatalf("Error() = %q, want exact body %q", got, body)
	}
	if errResponse.OAuthError() != "invalid_grant" || errResponse.Code() != "42901" || errResponse.Message() != "Refresh token expired" || errResponse.Detail() != "sign in again" {
		t.Fatalf("classification fields = error %q code %q message %q detail %q", errResponse.OAuthError(), errResponse.Code(), errResponse.Message(), errResponse.Detail())
	}
	diagnostic := errResponse.Diagnostic()
	for _, want := range []string{"status 400", `error="invalid_grant"`, `code="42901"`, `request_id="req-nested"`, "reason=token_expired"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("Diagnostic() = %q, want %q", diagnostic, want)
		}
	}
	if strings.Contains(diagnostic, "sign in again") || strings.Contains(diagnostic, "Refresh token expired") {
		t.Fatalf("Diagnostic() retained free-form provider detail: %q", diagnostic)
	}
}
