package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	log "github.com/sirupsen/logrus"
)

type RefreshController struct {
	coordinator      *Coordinator
	runtime          *home.Runtime
	repo             *Repository
	forwardTLSConfig *tls.Config
	forwardRefresh   func(context.Context, *ClusterNodeRecord, string, string, *tls.Config) ([]byte, error)
}

// NewRefreshController creates a new refresh controller.
func NewRefreshController(coordinator *Coordinator, runtime *home.Runtime, repo *Repository, forwardTLSConfig *tls.Config) *RefreshController {
	return &RefreshController{
		coordinator:      coordinator,
		runtime:          runtime,
		repo:             repo,
		forwardTLSConfig: forwardTLSConfig,
		forwardRefresh:   ForwardRefreshToMaster,
	}
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
	// Resolve credential context before calling upstream OAuth services.
	if c == nil || c.runtime == nil {
		return nil, fmt.Errorf("cluster refresh: runtime is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.coordinator == nil || c.repo == nil {
		return c.runtime.RefreshNowLocal(ctx, authIndex)
	}
	master, errMaster := c.coordinator.CurrentMaster(ctx)
	if errMaster != nil {
		return nil, fmt.Errorf("cluster refresh master lookup: %w", errMaster)
	}
	if c.isSelf(master) {
		return c.refreshLocalWithLock(ctx, authIndex)
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
			forwardRefresh = ForwardRefreshToMaster
		}
		if payload, errForward := forwardRefresh(ctx, master, forwardAuthUUID, nodeSecret, c.forwardTLSConfig); errForward == nil {
			return payload, nil
		} else {
			var authErr *coreauth.Error
			if errors.As(errForward, &authErr) && authErr != nil && strings.TrimSpace(authErr.Code) != "" {
				if errSync := c.syncForwardedAuth(ctx, forwardAuthUUID); errSync != nil {
					log.Warnf("cluster refresh forwarded auth sync failed | auth=%s err=%v", forwardAuthUUID, errSync)
					if strings.EqualFold(strings.TrimSpace(authErr.Code), "authentication_error") {
						if errDisable := c.disableForwardedAuthInMemory(ctx, forwardAuthUUID, authErr); errDisable != nil {
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

// refreshLocalWithLock refreshes a local with lock.
func (c *RefreshController) refreshLocalWithLock(ctx context.Context, authIndex string) ([]byte, error) {
	// Resolve credential context before calling upstream OAuth services.
	if c == nil || c.runtime == nil || c.repo == nil {
		return nil, fmt.Errorf("cluster refresh: controller is not ready")
	}
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

	var refreshErr error
	updated, errLock := c.repo.WithAuthRefreshLock(ctx, targetUUID, func(tx *Repository, auth *coreauth.Auth) (*coreauth.Auth, error) {
		refreshed, errRefresh := core.RefreshAuthCredential(ctx, auth)
		refreshErr = errRefresh
		if refreshed == nil {
			return nil, errRefresh
		}
		if _, errSave := tx.UpsertAuth(ctx, refreshed, "update"); errSave != nil {
			return nil, errSave
		}
		return refreshed, nil
	})
	if errLock != nil {
		return nil, errLock
	}
	if updated == nil {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	if errIndex := c.runtime.RefreshClusterAuthIndex(ctx, targetUUID); errIndex != nil {
		if refreshErr != nil {
			log.Warnf("cluster refresh index sync failed after persisted refresh failure | auth=%s err=%v", targetUUID, errIndex)
			if _, errFailClosed := c.runtime.UpdateAuthInMemory(ctx, updated); errFailClosed != nil {
				log.Warnf("cluster refresh fail-closed memory update failed | auth=%s err=%v", targetUUID, errFailClosed)
			}
			return nil, refreshErr
		}
		return nil, errIndex
	}
	if _, errUpdate := c.runtime.UpdateAuthInMemory(ctx, updated); errUpdate != nil {
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
