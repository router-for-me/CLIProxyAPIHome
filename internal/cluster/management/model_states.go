package management

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// CredentialModelStatesItem describes a snapshot held by the current Home node.
type CredentialModelStatesItem struct {
	CredentialID   string                     `json:"credential_id"`
	Provider       string                     `json:"provider"`
	Label          string                     `json:"label,omitempty"`
	Prefix         string                     `json:"prefix,omitempty"`
	StateVersion   int64                      `json:"state_version"`
	Status         coreauth.Status            `json:"status"`
	StatusMessage  string                     `json:"status_message,omitempty"`
	Disabled       bool                       `json:"disabled"`
	Unavailable    bool                       `json:"unavailable"`
	RefreshBlocked bool                       `json:"refresh_blocked"`
	NextRetryAfter time.Time                  `json:"next_retry_after,omitzero"`
	LastError      *coreauth.Error            `json:"last_error,omitempty"`
	Models         []CredentialModelStateInfo `json:"models"`
}

// CredentialModelStateInfo keeps recorded errors separate from effective scheduling gates.
type CredentialModelStateInfo struct {
	Model             string               `json:"model"`
	UpstreamModel     string               `json:"upstream_model"`
	StateKey          string               `json:"state_key"`
	DisplayName       string               `json:"display_name,omitempty"`
	Registered        bool                 `json:"registered"`
	Status            coreauth.Status      `json:"status"`
	StatusMessage     string               `json:"status_message,omitempty"`
	Unavailable       bool                 `json:"unavailable"`
	Blocked           bool                 `json:"blocked"`
	BlockReason       string               `json:"block_reason"`
	NextRetryAfter    time.Time            `json:"next_retry_after,omitzero"`
	RemainingCooldown string               `json:"remaining_cooldown,omitempty"`
	LastError         *coreauth.Error      `json:"last_error,omitempty"`
	Quota             *coreauth.QuotaState `json:"quota,omitempty"`
	UpdatedAt         time.Time            `json:"updated_at,omitzero"`
}

// GetCredentialModelStates handles both the list route and the single-credential path.
// It reads runtime snapshots only; persisted credentials are never merged into them.
func (h *Handler) GetCredentialModelStates(c *gin.Context) {
	if h.runtime == nil || h.runtime.CoreManager() == nil {
		respondError(c, http.StatusServiceUnavailable, "runtime_unavailable", errors.New("core auth manager unavailable"))
		return
	}
	manager := h.runtime.CoreManager()
	credentialID, single := c.Params.Get("credential_id")
	credentialID = strings.TrimSpace(credentialID)
	if single && credentialID == "" {
		respondError(c, http.StatusNotFound, "credential_not_found", errors.New("credential not found"))
		return
	}
	if !single {
		credentialID = strings.TrimSpace(c.Query("credential_id"))
	}
	var auths []*coreauth.Auth
	if credentialID != "" {
		if auth, ok := manager.GetByID(credentialID); ok {
			auths = []*coreauth.Auth{auth}
		}
	} else {
		auths = manager.List()
	}

	now := time.Now().UTC()
	provider := strings.TrimSpace(c.Query("provider"))
	model := strings.TrimSpace(c.Query("model"))
	items := make([]CredentialModelStatesItem, 0, len(auths))
	for _, auth := range auths {
		if provider != "" && !strings.EqualFold(auth.Provider, provider) {
			continue
		}
		item := buildCredentialModelStatesItem(manager, auth, model, now)
		if !single && model != "" && len(item.Models) == 0 {
			continue
		}
		items = append(items, item)
	}
	if single && len(items) == 0 {
		respondError(c, http.StatusNotFound, "credential_not_found", errors.New("credential not found"))
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CredentialID < items[j].CredentialID })
	response := gin.H{
		"status":      "ok",
		"source":      "runtime",
		"node":        gin.H{"ip": h.nodeIP, "port": h.nodePort},
		"observed_at": now,
	}
	if single {
		response["credential"] = items[0]
	} else {
		response["total"] = len(items)
		response["credentials"] = items
	}
	c.JSON(http.StatusOK, response)
}

func buildCredentialModelStatesItem(manager *coreauth.Manager, auth *coreauth.Auth, modelFilter string, now time.Time) CredentialModelStatesItem {
	item := CredentialModelStatesItem{
		CredentialID:   auth.ID,
		Provider:       auth.Provider,
		Label:          auth.Label,
		Prefix:         auth.Prefix,
		StateVersion:   auth.StateVersion,
		Status:         auth.Status,
		StatusMessage:  auth.StatusMessage,
		Disabled:       auth.Disabled || auth.Status == coreauth.StatusDisabled,
		Unavailable:    auth.Unavailable,
		RefreshBlocked: coreauth.RefreshBlocksDispatch(auth),
		NextRetryAfter: auth.NextRetryAfter,
		LastError:      auth.LastError,
		Models:         make([]CredentialModelStateInfo, 0),
	}
	for _, view := range manager.DescribeModelStates(auth, modelFilter, now) {
		info := CredentialModelStateInfo{
			Model:          view.Model,
			UpstreamModel:  view.UpstreamModel,
			StateKey:       view.StateKey,
			DisplayName:    view.DisplayName,
			Registered:     view.Registered,
			Status:         coreauth.StatusActive,
			Blocked:        view.Blocked,
			BlockReason:    view.BlockReason,
			NextRetryAfter: view.NextRetryAfter,
		}
		if state := view.State; state != nil {
			info.Status = state.Status
			info.StatusMessage = state.StatusMessage
			info.Unavailable = state.Unavailable
			info.LastError = state.LastError
			info.UpdatedAt = state.UpdatedAt
			if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
				quota := state.Quota
				info.Quota = &quota
			}
		} else if !view.Registered {
			info.Status = coreauth.StatusUnknown
		}
		if !view.NextRetryAfter.IsZero() {
			remaining := view.NextRetryAfter.Sub(now)
			info.RemainingCooldown = ((remaining + time.Second - 1) / time.Second * time.Second).String()
		}
		item.Models = append(item.Models, info)
	}
	return item
}
