package home

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

// errSchedulerUnavailable refuses a pick while a required scheduler plugin is not active.
var errSchedulerUnavailable = &coreauth.Error{
	Code:       "scheduler_unavailable",
	Message:    "required scheduler plugin is not active",
	HTTPStatus: http.StatusServiceUnavailable,
}

// schedulerHost is the plugin host's scheduling surface.
type schedulerHost interface {
	PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
	HasScheduler() bool
}

func (r *Runtime) schedulerSource() schedulerHost {
	if r.schedulerHost != nil {
		return r.schedulerHost
	}
	if r.pluginHost == nil {
		return nil
	}
	return r.pluginHost
}

// schedulerRequired reports whether an enabled Home-loaded plugin instance sets
// scheduler-required: such a plugin enforces policy, so dispatch must never fall back to the
// built-in selector while no scheduler plugin is active.
func (r *Runtime) schedulerRequired() bool {
	cfg := r.Config()
	if cfg == nil {
		return false
	}
	for id, item := range cfg.Plugins.Configs {
		if item.Enabled == nil || !*item.Enabled || !pluginInstanceLoadsInHome(id, item) {
			continue
		}
		if boolFromYAMLNode(yamlMappingValue(&item.Raw, "scheduler-required")) {
			return true
		}
	}
	return false
}
