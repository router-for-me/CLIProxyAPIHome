package get

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

// handleModels handles a models.
func handleModels(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	if len(args) != 2 {
		return dispatch.Err("wrong number of arguments for 'get' command")
	}

	if env.Runtime == nil || env.Runtime.CoreManager() == nil {
		return dispatch.Err("runtime not ready")
	}

	routes, _, errRoutes := env.Runtime.LocalExecutorRoutes(ctx, "")
	if errRoutes != nil {
		return dispatch.Err(errRoutes.Error())
	}
	payload, errBuild := buildModelsJSON(env, nil, routes)
	if errBuild != nil {
		return dispatch.Err(errBuild.Error())
	}
	return dispatch.BulkString([]byte(payload))
}

// buildModelsJSON assembles the models payload grouped by provider bucket.
// Authentication is the caller's responsibility.
func buildModelsJSON(env dispatch.Env, policy *coreauth.ModelAccess, localRoutes map[string]home.LocalExecutorRoute) (string, error) {
	if env.Runtime == nil || env.Runtime.CoreManager() == nil {
		return "", errRuntimeNotReady
	}

	resolveGroupKey := func(auth *coreauth.Auth) string {
		if auth == nil {
			return ""
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider == "" {
			return ""
		}
		if provider != "codex" {
			return provider
		}
		planType := ""
		if auth.Attributes != nil {
			planType = strings.ToLower(strings.TrimSpace(auth.Attributes["plan_type"]))
		}
		switch planType {
		case "free":
			return "codex-free"
		case "plus":
			return "codex-plus"
		case "team", "business", "go":
			return "codex-team"
		case "pro", "":
			return "codex-pro"
		default:
			return "codex-pro"
		}
	}

	type modelBucket struct {
		byID map[string]*registry.ModelInfo
		// toolSearch ANDs native tool search support across every credential
		// that serves a model in the Claude bucket.
		toolSearch map[string]bool
	}

	buckets := make(map[string]*modelBucket)
	manager := env.Runtime.CoreManager()
	addModel := func(groupKey string, auth *coreauth.Auth, model *registry.ModelInfo) {
		groupKey = strings.TrimSpace(groupKey)
		if groupKey == "" || model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		idKey := id
		if auth != nil {
			idKey = strings.ToLower(id)
		}
		b := buckets[groupKey]
		if b == nil {
			b = &modelBucket{byID: make(map[string]*registry.ModelInfo), toolSearch: make(map[string]bool)}
			buckets[groupKey] = b
		}
		if groupKey == claudeModelsGroupKey && auth != nil {
			supported := home.ClaudeNativeToolSearchSupported(auth, manager.ResolveExecutionModel(auth, id))
			if previous, seen := b.toolSearch[idKey]; seen {
				supported = previous && supported
			}
			b.toolSearch[idKey] = supported
		}
		if _, exists := b.byID[idKey]; exists {
			return
		}
		b.byID[idKey] = model
	}

	env.Runtime.PruneCopilotModels()
	reg := registry.GetGlobalRegistry()
	auths := manager.List()
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.ID == "" {
			continue
		}
		groupKey := resolveGroupKey(auth)
		if groupKey == "" {
			continue
		}
		models := reg.GetModelsForClient(auth.ID)
		for _, model := range models {
			if model != nil && policy.Allows(auth.ID, model.ID) {
				addModel(groupKey, auth, model)
			}
		}
	}

	for public, route := range localRoutes {
		if policy.Allows("", public) {
			addModel(route.Plugin, nil, &route.Model)
		}
	}

	out := make(map[string][]*registry.ModelInfo, len(buckets))
	for key, b := range buckets {
		if b == nil || len(b.byID) == 0 {
			continue
		}
		models := make([]*registry.ModelInfo, 0, len(b.byID))
		for idKey, model := range b.byID {
			if toolSearch, computed := b.toolSearch[idKey]; computed {
				model = withToolSearchCapability(model, toolSearch)
			}
			models = append(models, model)
		}
		sort.Slice(models, func(i, j int) bool {
			idI := strings.ToLower(strings.TrimSpace(models[i].ID))
			idJ := strings.ToLower(strings.TrimSpace(models[j].ID))
			return idI < idJ
		})
		out[key] = models
	}
	raw, errMarshal := json.Marshal(out)
	return string(raw), errMarshal
}

// claudeModelsGroupKey is the models payload bucket served by Claude credentials.
const claudeModelsGroupKey = "claude"

// withToolSearchCapability returns a copy of model carrying the route's native
// tool search capability, leaving the registry's model untouched.
func withToolSearchCapability(model *registry.ModelInfo, supported bool) *registry.ModelInfo {
	copyModel := *model
	capabilities := registry.NativeCapabilities{}
	if model.NativeCapabilities != nil {
		capabilities = *model.NativeCapabilities
	}
	capabilities.ToolSearch = &supported
	copyModel.NativeCapabilities = &capabilities
	return &copyModel
}
