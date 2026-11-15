package home

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// This key and value format are shared with existing Home-mode CPA executors.
const antigravityTOSPauseKVKey = "cpa:antigravity:tos-pause:until"

func (r *Runtime) antigravityTOSPauseWindow() time.Duration {
	cfg := r.Config()
	raw := ""
	if cfg != nil {
		raw = strings.TrimSpace(cfg.Antigravity.TOSPauseWindow)
	}
	switch strings.ToLower(raw) {
	case "", "0", "off", "false", "none", "disabled":
		return 0
	}
	window, errParse := time.ParseDuration(raw)
	if errParse != nil || window <= 0 {
		return 0
	}
	return window
}

// antigravityTOSPauseDeadline reads the authoritative pool fence on every dispatch.
// No node-local cache may keep dispatching after another Home arms the pause.
func (r *Runtime) antigravityTOSPauseDeadline(ctx context.Context) (time.Time, error) {
	if r.antigravityTOSPauseWindow() <= 0 || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return time.Time{}, nil
	}
	raw, found, errGet := r.KVGet(ctx, antigravityTOSPauseKVKey)
	if errGet != nil || !found {
		return time.Time{}, errGet
	}
	nanos, errParse := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if errParse != nil {
		return time.Time{}, fmt.Errorf("invalid Antigravity TOS pause deadline: %w", errParse)
	}
	return time.Unix(0, nanos), nil
}

func (r *Runtime) recordAntigravityTOSPause(ctx context.Context, result coreauth.Result, replay bool) error {
	if result.Success || result.Error == nil || result.RequestScoped() || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return nil
	}
	auth, ok := r.coreManager.GetByID(result.AuthID)
	if !ok {
		auth, ok = r.coreManager.GetByIndex(result.AuthIndex)
	}
	if !ok || auth == nil || !strings.EqualFold(auth.Provider, "antigravity") {
		return nil
	}
	window := r.antigravityTOSPauseWindow()
	if window <= 0 || result.Error.HTTPStatus != http.StatusForbidden {
		return nil
	}
	body := result.Error.Message
	if result.Error.Upstream != nil {
		body = string(result.Error.Upstream.Body)
	}
	var response struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Details []struct {
				Type   string `json:"@type"`
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if errDecode := json.Unmarshal([]byte(body), &response); errDecode != nil || response.Error.Code != http.StatusForbidden || !strings.EqualFold(response.Error.Status, "PERMISSION_DENIED") {
		return nil
	}
	violation := false
	for _, detail := range response.Error.Details {
		if detail.Type == "type.googleapis.com/google.rpc.ErrorInfo" && strings.EqualFold(strings.TrimSpace(detail.Reason), "TOS_VIOLATION") {
			violation = true
			break
		}
	}
	if !violation {
		return nil
	}
	now := time.Now()
	observed := result.ObservedAt
	if observed.IsZero() {
		if replay {
			return nil
		}
		observed = now
	}
	until := observed.Add(window)
	if !until.After(now) {
		return nil
	}
	value := []byte(strconv.FormatInt(until.UnixNano(), 10))
	for {
		raw, found, errGet := r.KVGet(ctx, antigravityTOSPauseKVKey)
		if errGet != nil {
			return errGet
		}
		if found {
			nanos, errParse := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
			if errParse != nil {
				return fmt.Errorf("invalid Antigravity TOS pause deadline: %w", errParse)
			}
			// Duplicate deliveries must not extend a live pause, including the
			// pause already written by a compatible CPA before reporting usage.
			if time.Unix(0, nanos).After(now) {
				return nil
			}
		}
		ttl := time.Until(until)
		if ttl <= 0 {
			return nil
		}
		swapped, errSwap := r.KVCompareAndSwap(ctx, antigravityTOSPauseKVKey, raw, found, value, ttl+5*time.Second)
		if errSwap != nil {
			return errSwap
		}
		if swapped {
			return nil
		}
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
	}
}
