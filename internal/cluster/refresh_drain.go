package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// A provider may rotate a refresh token before Home writes the new one back. Stopping between
// those two steps loses the token, so a stopping Home first waits for claimed refreshes.
var (
	refreshesInFlight atomic.Int64
	refreshDraining   atomic.Bool
)

// A refused refresh reads as cancelled, so the refresh loop logs it as a stop and not a failure.
var errRefreshDraining = fmt.Errorf("cluster refresh: Home is stopping: %w", context.Canceled)

var credentialUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// RefreshesInFlight reports refreshes this process has started and not yet written back.
func RefreshesInFlight() int64 {
	return refreshesInFlight.Load()
}

// DrainRefreshes refuses new refresh claims and waits up to timeout for claimed ones to finish.
func DrainRefreshes(timeout time.Duration) bool {
	refreshDraining.Store(true)
	deadline := time.Now().Add(timeout)
	for refreshesInFlight.Load() > 0 {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// beginRefresh counts a refresh before its claim, so a concurrent drain either waits for it or
// the refresh sees the drain and does not start.
func beginRefresh() (func(), error) {
	done := holdRefresh()
	if refreshDraining.Load() {
		done()
		return nil, errRefreshDraining
	}
	return done, nil
}

// holdRefresh counts work that continues a refresh already counted, so a drain in progress still
// waits for it.
func holdRefresh() func() {
	refreshesInFlight.Add(1)
	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			refreshesInFlight.Add(-1)
		}
	}
}

// noteRefreshRejection logs one closed line when this refresh's own provider rejection disabled
// the credential, so an operator knows which account needs signing in again. A credential
// disabled meanwhile by someone else, or a superseded refresh, logs nothing.
func noteRefreshRejection(targetUUID string, errRefresh error, finalized *coreauth.Auth, finalizedLease, superseded, disabledTarget bool) {
	if errRefresh == nil || finalized == nil || !finalizedLease || superseded || disabledTarget {
		return
	}
	if finalized.Disabled || finalized.Status == coreauth.StatusDisabled {
		log.Warnf("credential refresh rejected credential=%s reason=%s", credentialReference(targetUUID), terminalRefreshReason(errRefresh))
	}
}

// credentialReference names a credential without its identifying text: native UUIDs as they
// are, any other identifier only as its SHA-256.
func credentialReference(id string) string {
	id = strings.TrimSpace(id)
	if credentialUUID.MatchString(id) {
		return id
	}
	digest := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// terminalRefreshReason maps a provider refresh rejection onto a closed reason.
func terminalRefreshReason(errRefresh error) string {
	if errRefresh == nil {
		return "unauthorized"
	}
	texts := []string{errRefresh.Error()}
	var authErr *coreauth.Error
	if errors.As(errRefresh, &authErr) && authErr != nil {
		texts = append(texts, authErr.Code, authErr.Message)
	}
	type oauthResponseDiagnostics interface {
		OAuthError() string
		Code() string
		ErrorDescription() string
	}
	var upstream oauthResponseDiagnostics
	if errors.As(errRefresh, &upstream) && upstream != nil {
		texts = append(texts, upstream.OAuthError(), upstream.Code(), upstream.ErrorDescription())
	}
	joined := strings.ToLower(strings.Join(texts, " "))
	for _, reason := range []string{"refresh_token_reused", "refresh_token_revoked", "refresh_token_expired", "invalid_grant"} {
		if strings.Contains(joined, reason) {
			return reason
		}
	}
	if strings.Contains(joined, "reused") {
		return "refresh_token_reused"
	}
	return "unauthorized"
}
