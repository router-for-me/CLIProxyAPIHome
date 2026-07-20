package home

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestRuntimeAuthenticateRequestFailClosedWithoutAccessManager(t *testing.T) {
	rt := &Runtime{}
	_, authErr := rt.authenticateRequest(context.Background(), http.Header{})
	if !access.IsAuthErrorCode(authErr, access.AuthErrorCodeNoCredentials) {
		t.Fatalf("authenticateRequest() error = %v, want no credentials", authErr)
	}
}

func TestDispatchLeaseTTLUsesLeaseCadence(t *testing.T) {
	lastRenewedAt := time.Now().UTC()
	lease := &InFlightLease{
		LastRenewedAt: lastRenewedAt,
		ExpiresAt:     lastRenewedAt.Add(time.Minute),
	}
	if got := dispatchLeaseTTL(lease, 30*time.Minute); got != time.Minute {
		t.Fatalf("dispatchLeaseTTL() = %v, want %v", got, time.Minute)
	}
	if got := dispatchLeaseTTL(nil, 30*time.Minute); got != 30*time.Minute {
		t.Fatalf("dispatchLeaseTTL(nil) = %v, want %v", got, 30*time.Minute)
	}
}

func TestDispatchMetadataAllowsAuthID(t *testing.T) {
	metadata := map[string]any{
		coreauth.AllowedAuthIDsMetadataKey: []string{"auth-a"},
	}
	if !dispatchMetadataAllowsAuthID(metadata, "auth-a") {
		t.Fatal("dispatchMetadataAllowsAuthID(auth-a) = false, want true")
	}
	if dispatchMetadataAllowsAuthID(metadata, "auth-b") {
		t.Fatal("dispatchMetadataAllowsAuthID(auth-b) = true, want false")
	}
	if !dispatchMetadataAllowsAuthID(nil, "auth-b") {
		t.Fatal("dispatchMetadataAllowsAuthID(unrestricted) = false, want true")
	}
}
