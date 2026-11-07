package auth

import (
	"errors"
	"net/http"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
)

func TestCodexUnknownOutcomeDoesNotInheritCauseRejection(t *testing.T) {
	refusal := &Error{Code: "invalid_grant", Message: "401 unauthorized", HTTPStatus: http.StatusUnauthorized}
	for _, cause := range []error{refusal, errors.Join(refusal, errors.New("transport diagnostic"))} {
		outcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "transport", cause)
		for _, errRefresh := range []error{outcome, builtInRefreshError("codex", "provider_refresh", outcome)} {
			if isTerminalRefreshAuthError(errRefresh) {
				t.Fatal("an uncertain exchange inherited terminal rejection from its diagnostic cause")
			}
		}
	}
	if !isTerminalRefreshAuthError(refusal) {
		t.Fatal("an explicit grant rejection became non-terminal")
	}
}
