package cluster

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	log "github.com/sirupsen/logrus"
)

const (
	credentialRefreshTimeout = 30 * time.Second
	refreshLeaseAttribute    = "__home_refresh_lease"
	refreshLeaseDuration     = 5 * time.Minute
	refreshFinalizeTimeout   = 10 * time.Second
)

type RefreshController struct {
	coordinator      *Coordinator
	runtime          *home.Runtime
	repo             *Repository
	forwardTLSConfig *tls.Config
	forwardRefresh   func(context.Context, *ClusterNodeRecord, string, string, string, *tls.Config) ([]byte, error)
	refreshLocks     sync.Map
}

type contextRefreshLock struct {
	semaphore chan struct{}
}

func newContextRefreshLock() *contextRefreshLock {
	lock := &contextRefreshLock{semaphore: make(chan struct{}, 1)}
	lock.semaphore <- struct{}{}
	return lock
}

func (l *contextRefreshLock) Lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.semaphore:
		return nil
	}
}

func (l *contextRefreshLock) Unlock() {
	l.semaphore <- struct{}{}
}

// NewRefreshController creates a new refresh controller.
func NewRefreshController(coordinator *Coordinator, runtime *home.Runtime, repo *Repository, forwardTLSConfig *tls.Config) *RefreshController {
	controller := &RefreshController{
		coordinator:      coordinator,
		runtime:          runtime,
		repo:             repo,
		forwardTLSConfig: forwardTLSConfig,
		forwardRefresh:   ForwardRefreshToMasterObserved,
	}
	if runtime != nil && runtime.CoreManager() != nil {
		runtime.CoreManager().SetAutoRefreshHandler(func(ctx context.Context, auth *coreauth.Auth) error {
			if auth == nil {
				return coreauth.ErrRefreshUnsupported
			}
			authIndex := strings.TrimSpace(auth.Index)
			if authIndex == "" {
				authIndex = strings.TrimSpace(auth.ID)
			}
			_, errRefresh := controller.refreshLocalWithLock(ctx, authIndex, coreauth.AccessTokenSHA256(auth))
			return errRefresh
		})
	}
	return controller
}

// OnMasterChanged handles an on master changed.
func (c *RefreshController) OnMasterChanged(isMaster bool) {
	if c == nil || c.runtime == nil {
		return
	}
	if isMaster {
		if c.CanAutoRefresh() {
			c.runtime.StartAutoRefresh(context.Background())
		}
		return
	}
	c.runtime.StopAutoRefresh()
}

// CanAutoRefresh reports whether can auto refresh.
func (c *RefreshController) CanAutoRefresh() bool {
	if c == nil || c.coordinator == nil {
		return false
	}
	return c.coordinator.IsMaster()
}

// RefreshNow refreshes refresh now.
func (c *RefreshController) RefreshNow(ctx context.Context, authIndex string) ([]byte, error) {
	return c.RefreshNowObserved(ctx, authIndex, "")
}

// RefreshNowObserved refreshes unless the master already stores a newer token.
func (c *RefreshController) RefreshNowObserved(ctx context.Context, authIndex, observedAccessTokenSHA256 string) ([]byte, error) {
	if c == nil || c.runtime == nil {
		return nil, fmt.Errorf("cluster refresh: runtime is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshCtx, cancelRefresh := context.WithTimeout(ctx, credentialRefreshTimeout)
	defer cancelRefresh()
	if c.coordinator == nil || c.repo == nil {
		return c.runtime.RefreshNowLocalObserved(refreshCtx, authIndex, observedAccessTokenSHA256)
	}
	master, errMaster := c.coordinator.CurrentMaster(refreshCtx)
	if errMaster != nil {
		return nil, fmt.Errorf("cluster refresh master lookup: %w", errMaster)
	}
	if c.isSelf(master) {
		return c.refreshLocalWithLock(refreshCtx, authIndex, observedAccessTokenSHA256)
	}

	forwardAuthUUID := strings.TrimSpace(authIndex)
	if core := c.runtime.CoreManager(); core != nil {
		if targetUUID, _, errTarget := c.refreshTarget(core, authIndex); errTarget == nil && strings.TrimSpace(targetUUID) != "" {
			forwardAuthUUID = targetUUID
		}
	}
	if master != nil && strings.TrimSpace(master.IP) != "" && master.Port > 0 {
		nodeSecret := c.coordinator.NodeSecret()
		if strings.TrimSpace(nodeSecret) == "" {
			return nil, fmt.Errorf("cluster refresh node secret is unavailable")
		}
		forwardRefresh := c.forwardRefresh
		if forwardRefresh == nil {
			forwardRefresh = ForwardRefreshToMasterObserved
		}
		if payload, errForward := forwardRefresh(refreshCtx, master, forwardAuthUUID, nodeSecret, observedAccessTokenSHA256, c.forwardTLSConfig); errForward == nil {
			if errSync := c.syncForwardedAuth(refreshCtx, forwardAuthUUID); errSync != nil {
				return nil, fmt.Errorf("cluster refresh sync after master success: %w", errSync)
			}
			return payload, nil
		} else {
			var authErr *coreauth.Error
			if errors.As(errForward, &authErr) && authErr != nil && strings.TrimSpace(authErr.Code) != "" {
				if errSync := c.syncForwardedAuth(refreshCtx, forwardAuthUUID); errSync != nil {
					log.Warnf("cluster refresh forwarded auth sync failed | auth=%s err=%v", forwardAuthUUID, errSync)
					if strings.EqualFold(strings.TrimSpace(authErr.Code), "authentication_error") {
						if errDisable := c.disableForwardedAuthInMemory(refreshCtx, forwardAuthUUID, authErr); errDisable != nil {
							log.Warnf("cluster refresh forwarded auth fail-closed update failed | auth=%s err=%v", forwardAuthUUID, errDisable)
						}
					}
				}
				return nil, errForward
			}
			return nil, fmt.Errorf("cluster refresh forward to master: %w", errForward)
		}
	} else {
		return nil, fmt.Errorf("cluster refresh master is unavailable")
	}
	return nil, fmt.Errorf("cluster refresh master is unavailable")
}

// syncForwardedAuth applies the state already persisted by the master without
// retrying the provider refresh on the standby.
func (c *RefreshController) syncForwardedAuth(ctx context.Context, authUUID string) error {
	if c == nil || c.runtime == nil || c.repo == nil {
		return fmt.Errorf("cluster refresh: controller is not ready")
	}
	authUUID = strings.TrimSpace(authUUID)
	if authUUID == "" {
		return fmt.Errorf("cluster auth uuid is required")
	}
	auth, _, errAuth := c.repo.GetAuth(ctx, authUUID)
	if errAuth != nil {
		return errAuth
	}
	if errIndex := c.runtime.RefreshClusterAuthIndex(ctx, authUUID); errIndex != nil {
		return errIndex
	}
	_, errUpdate := c.runtime.UpdateAuthInMemory(ctx, auth)
	return errUpdate
}

// disableForwardedAuthInMemory fails closed when the master persisted a
// terminal authentication failure but the standby cannot reload that state.
func (c *RefreshController) disableForwardedAuthInMemory(ctx context.Context, authUUID string, authErr *coreauth.Error) error {
	if c == nil || c.runtime == nil {
		return fmt.Errorf("cluster refresh: runtime is nil")
	}
	core := c.runtime.CoreManager()
	if core == nil {
		return fmt.Errorf("cluster refresh: core manager is nil")
	}
	auth, ok := core.GetByIndex(authUUID)
	if !ok || auth == nil {
		auth, ok = core.GetByID(authUUID)
	}
	if !ok || auth == nil {
		return nil
	}
	now := time.Now().UTC()
	auth.Disabled = true
	auth.Unavailable = true
	auth.Status = coreauth.StatusDisabled
	auth.StatusMessage = "unauthorized"
	auth.NextRetryAfter = time.Time{}
	auth.NextRefreshAfter = time.Time{}
	auth.Quota = coreauth.QuotaState{}
	auth.UpdatedAt = now
	auth.LastError = &coreauth.Error{
		Code:       "authentication_error",
		Message:    "credential unauthorized",
		HTTPStatus: http.StatusUnauthorized,
	}
	if authErr != nil {
		if code := strings.TrimSpace(authErr.Code); code != "" {
			auth.LastError.Code = code
		}
		if message := strings.TrimSpace(authErr.Message); message != "" {
			auth.LastError.Message = message
		}
		if authErr.HTTPStatus > 0 {
			auth.LastError.HTTPStatus = authErr.HTTPStatus
		}
	}
	_, errUpdate := c.runtime.UpdateAuthInMemory(ctx, auth)
	return errUpdate
}

// isSelf reports whether self.
func (c *RefreshController) isSelf(node *ClusterNodeRecord) bool {
	if c == nil || c.coordinator == nil || node == nil {
		return false
	}
	return strings.TrimSpace(node.IP) == strings.TrimSpace(c.coordinator.node.IP) && node.Port == c.coordinator.node.Port
}

// refreshLocalWithLock claims a short persisted lease, performs OAuth outside
// the transaction, then applies the result only while the lease is still owned.
func (c *RefreshController) refreshLocalWithLock(ctx context.Context, authIndex, observedAccessTokenSHA256 string) ([]byte, error) {
	if c == nil || c.runtime == nil || c.repo == nil {
		return nil, fmt.Errorf("cluster refresh: controller is not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshCtx, cancelRefresh := context.WithTimeout(ctx, credentialRefreshTimeout)
	defer cancelRefresh()
	core := c.runtime.CoreManager()
	if core == nil {
		return nil, fmt.Errorf("cluster refresh: core manager is nil")
	}

	requestedIndex := strings.TrimSpace(authIndex)
	if requestedIndex == "" {
		return nil, fmt.Errorf("auth manager: missing auth index")
	}
	targetUUID, targetIndex, errTarget := c.refreshTarget(core, requestedIndex)
	if errTarget != nil {
		return nil, errTarget
	}

	lockValue, _ := c.refreshLocks.LoadOrStore(targetUUID, newContextRefreshLock())
	refreshLock := lockValue.(*contextRefreshLock)
	if errLock := refreshLock.Lock(refreshCtx); errLock != nil {
		return nil, errLock
	}
	defer refreshLock.Unlock()

	now := time.Now().UTC()
	leaseID := newRefreshLeaseID()
	var refreshErr error
	var beforeClaim *coreauth.Auth
	skipRefresh := false
	leased, _, leaseClaimed, errClaim := c.repo.MutateAuth(refreshCtx, targetUUID, "refresh-claim", func(auth *coreauth.Auth) bool {
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			refreshErr = newClusterUnauthorizedRefreshError()
			skipRefresh = true
			return false
		}
		if coreauth.AuthIsNewerThanObserved(auth, observedAccessTokenSHA256) {
			skipRefresh = true
			return false
		}
		if coreauth.RefreshRetryBackoffOpen(auth, now) || refreshLeaseActive(auth, now) {
			refreshErr = coreauth.NewTransientRefreshError()
			skipRefresh = true
			return false
		}
		beforeClaim = auth.Clone()
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes[refreshLeaseAttribute] = leaseID
		coreauth.ApplyRefreshPendingState(auth, now)
		auth.NextRefreshAfter = now.Add(refreshLeaseDuration)
		return true
	})
	if errClaim != nil {
		return nil, errClaim
	}
	updated := leased
	if !skipRefresh && leaseClaimed {
		refreshed, errRefresh := core.RefreshAuthCredential(refreshCtx, leased.Clone())
		refreshErr = errRefresh
		if refreshed == nil {
			refreshed = leased.Clone()
		}

		finalizeCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(refreshCtx), refreshFinalizeTimeout)
		finalized, finalizedLease, refreshSuperseded, disabledTarget, errFinalize := c.retryRefreshFinalize(finalizeCtx, targetUUID, leaseID, leased, beforeClaim, refreshed, refreshErr)
		cancelFinalize()
		if errFinalize != nil {
			go c.continueRefreshFinalize(targetUUID, leaseID, leased.Clone(), beforeClaim.Clone(), refreshed.Clone(), refreshErr, now.Add(refreshLeaseDuration))
			return nil, errFinalize
		}
		updated = finalized
		switch {
		case disabledTarget:
			refreshErr = newClusterUnauthorizedRefreshError()
		case refreshSuperseded:
			refreshErr = nil
		case !finalizedLease:
			refreshErr = coreauth.NewTransientRefreshError()
		}
	}

	if updated == nil {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	syncCtx, cancelSync := context.WithTimeout(context.WithoutCancel(refreshCtx), refreshFinalizeTimeout)
	defer cancelSync()
	if errIndex := c.runtime.RefreshClusterAuthIndex(syncCtx, targetUUID); errIndex != nil {
		if refreshErr != nil {
			log.Warnf("cluster refresh index sync failed after persisted refresh failure | auth=%s err=%v", targetUUID, errIndex)
			if _, errFailClosed := c.runtime.UpdateAuthInMemory(syncCtx, updated); errFailClosed != nil {
				log.Warnf("cluster refresh fail-closed memory update failed | auth=%s err=%v", targetUUID, errFailClosed)
			}
			return nil, refreshErr
		}
		return nil, errIndex
	}
	if _, errUpdate := c.runtime.UpdateAuthInMemory(syncCtx, updated); errUpdate != nil {
		if refreshErr != nil {
			log.Warnf("cluster refresh memory sync failed after persisted refresh failure | auth=%s err=%v", targetUUID, errUpdate)
			return nil, refreshErr
		}
		return nil, errUpdate
	}
	if refreshErr != nil {
		return nil, refreshErr
	}
	if targetIndex != requestedIndex {
		if requested, ok := core.GetByIndex(requestedIndex); ok && requested != nil {
			return home.BuildRefreshPayload(requested)
		}
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	return home.BuildRefreshPayload(updated)
}

func (c *RefreshController) retryRefreshFinalize(ctx context.Context, targetUUID, leaseID string, leased, beforeClaim, refreshed *coreauth.Auth, errRefresh error) (*coreauth.Auth, bool, bool, bool, error) {
	for {
		finalized, finalizedLease, refreshSuperseded, disabledTarget, errFinalize := c.finalizeRefreshLease(ctx, targetUUID, leaseID, leased, beforeClaim, refreshed, errRefresh)
		if errFinalize == nil {
			return finalized, finalizedLease, refreshSuperseded, disabledTarget, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, false, false, errFinalize
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *RefreshController) finalizeRefreshLease(ctx context.Context, targetUUID, leaseID string, leased, beforeClaim, refreshed *coreauth.Auth, errRefresh error) (*coreauth.Auth, bool, bool, bool, error) {
	refreshSuperseded := false
	disabledTarget := false
	finalized, _, finalizedLease, errFinalize := c.repo.MutateAuth(ctx, targetUUID, "refresh-finalize", func(current *coreauth.Auth) bool {
		if current.Attributes == nil || current.Attributes[refreshLeaseAttribute] != leaseID {
			return false
		}
		if current.Disabled || current.Status == coreauth.StatusDisabled {
			clearRefreshLease(current)
			disabledTarget = true
			return true
		}
		if coreauth.AuthIsNewerThanObserved(current, coreauth.AccessTokenSHA256(leased)) {
			clearRefreshLease(current)
			if beforeClaim != nil && current.NextRefreshAfter.Equal(leased.NextRefreshAfter) {
				current.NextRefreshAfter = beforeClaim.NextRefreshAfter
			}
			refreshSuperseded = true
			return true
		}
		merged := mergeClusterRefreshOutcome(current, leased, refreshed, errRefresh, time.Now().UTC())
		clearRefreshLease(merged)
		*current = *merged
		return true
	})
	return finalized, finalizedLease, refreshSuperseded, disabledTarget, errFinalize
}

func (c *RefreshController) continueRefreshFinalize(targetUUID, leaseID string, leased, beforeClaim, refreshed *coreauth.Auth, errRefresh error, deadline time.Time) {
	if c == nil || c.repo == nil || c.runtime == nil || leased == nil || refreshed == nil {
		return
	}
	for time.Now().UTC().Before(deadline) {
		attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 5*time.Second)
		finalized, finalizedLease, _, _, errFinalize := c.finalizeRefreshLease(attemptCtx, targetUUID, leaseID, leased, beforeClaim, refreshed, errRefresh)
		cancelAttempt()
		if errFinalize == nil {
			if finalizedLease && finalized != nil {
				syncCtx, cancelSync := context.WithTimeout(context.Background(), refreshFinalizeTimeout)
				if errSync := c.syncForwardedAuth(syncCtx, targetUUID); errSync != nil {
					log.Warnf("cluster refresh background finalization sync failed | auth=%s err=%v", targetUUID, errSync)
				}
				cancelSync()
			}
			return
		}
		wait := 250 * time.Millisecond
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(wait)
		<-timer.C
	}
	log.Warnf("cluster refresh background finalization expired | auth=%s", targetUUID)
}

func newClusterUnauthorizedRefreshError() *coreauth.Error {
	return &coreauth.Error{Code: "authentication_error", Message: "credential unauthorized", HTTPStatus: http.StatusUnauthorized}
}

func newRefreshLeaseID() string {
	var raw [16]byte
	if _, errRead := rand.Read(raw[:]); errRead == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
}

func refreshLeaseActive(auth *coreauth.Auth, now time.Time) bool {
	return auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes[refreshLeaseAttribute]) != "" && auth.NextRefreshAfter.After(now)
}

func clearRefreshLease(auth *coreauth.Auth) {
	if auth == nil || auth.Attributes == nil {
		return
	}
	delete(auth.Attributes, refreshLeaseAttribute)
	if len(auth.Attributes) == 0 {
		auth.Attributes = nil
	}
}

func mergeClusterRefreshOutcome(current, base, refreshed *coreauth.Auth, errRefresh error, now time.Time) *coreauth.Auth {
	if current == nil {
		return refreshed
	}
	merged := current.Clone()
	if refreshed == nil {
		return merged
	}
	refreshed = refreshed.Clone()
	if errRefresh == nil {
		if base == nil {
			base = &coreauth.Auth{}
		}
		merged.Provider = mergeRefreshString(current.Provider, base.Provider, refreshed.Provider)
		merged.Prefix = mergeRefreshString(current.Prefix, base.Prefix, refreshed.Prefix)
		merged.Label = mergeRefreshString(current.Label, base.Label, refreshed.Label)
		merged.ProxyURL = mergeRefreshString(current.ProxyURL, base.ProxyURL, refreshed.ProxyURL)
		merged.Attributes = mergeRefreshStringMap(current.Attributes, base.Attributes, refreshed.Attributes)
		merged.Metadata = mergeRefreshMetadata(current.Metadata, base.Metadata, refreshed.Metadata)
		merged.Runtime = refreshed.Runtime
		coreauth.ApplyRefreshSuccessState(merged, now)
		if refreshed.NextRefreshAfter.After(now) {
			merged.NextRefreshAfter = refreshed.NextRefreshAfter
		}
		return merged
	}
	if refreshed.Disabled || refreshed.Status == coreauth.StatusDisabled {
		merged.Disabled = refreshed.Disabled
		merged.Unavailable = refreshed.Unavailable
		merged.Status = refreshed.Status
		merged.StatusMessage = refreshed.StatusMessage
		merged.LastError = refreshed.LastError
		merged.LastRefreshedAt = refreshed.LastRefreshedAt
		merged.NextRefreshAfter = refreshed.NextRefreshAfter
		merged.NextRetryAfter = refreshed.NextRetryAfter
		merged.UpdatedAt = refreshed.UpdatedAt
		return merged
	}

	// A transient acquisition failure only owns refresh scheduling fields. Keep
	// execution availability that may have changed while OAuth ran outside the transaction.
	merged.LastRefreshedAt = refreshed.LastRefreshedAt
	merged.NextRefreshAfter = refreshed.NextRefreshAfter
	if refreshExecutionStateEqual(current, base) {
		merged.LastError = refreshed.LastError
	}
	if merged.UpdatedAt.Before(refreshed.UpdatedAt) {
		merged.UpdatedAt = refreshed.UpdatedAt
	}
	return merged
}

func refreshExecutionStateEqual(left, right *coreauth.Auth) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Disabled == right.Disabled &&
		left.Unavailable == right.Unavailable &&
		left.Status == right.Status &&
		left.StatusMessage == right.StatusMessage &&
		left.NextRetryAfter.Equal(right.NextRetryAfter) &&
		reflect.DeepEqual(left.Quota, right.Quota) &&
		reflect.DeepEqual(left.LastError, right.LastError) &&
		reflect.DeepEqual(left.ModelStates, right.ModelStates)
}

func mergeRefreshString(current, base, refreshed string) string {
	if current == base && refreshed != base {
		return refreshed
	}
	return current
}

func mergeRefreshStringMap(current, base, refreshed map[string]string) map[string]string {
	merged := make(map[string]string, len(current)+len(refreshed))
	for key, value := range current {
		merged[key] = value
	}
	keys := make(map[string]struct{}, len(base)+len(refreshed))
	for key := range base {
		keys[key] = struct{}{}
	}
	for key := range refreshed {
		keys[key] = struct{}{}
	}
	for key := range keys {
		baseValue, baseOK := base[key]
		refreshedValue, refreshedOK := refreshed[key]
		if baseOK == refreshedOK && baseValue == refreshedValue {
			continue
		}
		currentValue, currentOK := current[key]
		if currentOK != baseOK || currentValue != baseValue {
			continue
		}
		if refreshedOK {
			merged[key] = refreshedValue
		} else {
			delete(merged, key)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func mergeRefreshMetadata(current, base, refreshed map[string]any) map[string]any {
	merged := make(map[string]any, len(current)+len(refreshed))
	for key, value := range current {
		merged[key] = value
	}
	keys := make(map[string]struct{}, len(base)+len(refreshed))
	for key := range base {
		keys[key] = struct{}{}
	}
	for key := range refreshed {
		keys[key] = struct{}{}
	}
	for key := range keys {
		baseValue, baseOK := base[key]
		refreshedValue, refreshedOK := refreshed[key]
		if baseOK == refreshedOK && reflect.DeepEqual(baseValue, refreshedValue) {
			continue
		}
		currentValue, currentOK := current[key]
		if currentOK != baseOK || !reflect.DeepEqual(currentValue, baseValue) {
			continue
		}
		if refreshedOK {
			merged[key] = refreshedValue
		} else {
			delete(merged, key)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// refreshTarget refreshes a target.
func (c *RefreshController) refreshTarget(core *coreauth.Manager, authIndex string) (string, string, error) {
	// Resolve credential context before calling upstream OAuth services.
	if core == nil {
		return "", "", fmt.Errorf("cluster refresh: core manager is nil")
	}
	requested, okRequested := core.GetByIndex(authIndex)
	if !okRequested || requested == nil {
		return "", "", fmt.Errorf("auth manager: auth not found")
	}

	target := requested
	targetIndex := authIndex

	uuid := strings.TrimSpace(target.ID)
	if uuid == "" {
		uuid = strings.TrimSpace(target.Index)
	}
	if uuid == "" {
		return "", "", fmt.Errorf("auth manager: auth not found")
	}
	if targetIndex == "" {
		targetIndex = uuid
	}
	return uuid, targetIndex, nil
}
