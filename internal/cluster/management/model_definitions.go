package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// GetAuthFileModels returns an auth file models.
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	h.getCredentialModels(c, false)
}

// GetCredentialModelsV8 includes the IDs accepted by credential exclusion rules.
func (h *Handler) GetCredentialModelsV8(c *gin.Context) {
	h.getCredentialModels(c, true)
}

func (h *Handler) getCredentialModels(c *gin.Context, includeCandidates bool) {
	// Normalize source data before building the derived payload.
	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuths)
		return
	}

	authID := ""
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if auth.ID == name || auth.Index == name || authFileDisplayName(auth) == name || authFileName(auth) == name {
			authID = auth.ID
			break
		}
		if strings.TrimSuffix(name, ".json") == auth.ID {
			authID = auth.ID
			break
		}
	}
	if authID == "" {
		authID = name
	}

	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	response := gin.H{"models": credentialModelEntries(models)}
	if includeCandidates {
		candidates := registry.GetGlobalRegistry().GetModelCandidatesForClient(authID)
		response["exclusion_models"] = nil
		if candidates != nil {
			response["exclusion_models"] = credentialModelEntries(candidates)
		}
	}
	c.JSON(http.StatusOK, response)
}

func credentialModelEntries(models []*registry.ModelInfo) []gin.H {
	result := make([]gin.H, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		entry := gin.H{"id": model.ID}
		if model.DisplayName != "" {
			entry["display_name"] = model.DisplayName
		}
		if model.Type != "" {
			entry["type"] = model.Type
		}
		if model.OwnedBy != "" {
			entry["owned_by"] = model.OwnedBy
		}
		result = append(result, entry)
	}
	return result
}

// GetModels returns static or currently available model definitions.
func (h *Handler) GetModels(c *gin.Context) {
	h.getModels(c, false)
}

// GetModelsV8 includes the execution identities used by billing for public aliases.
func (h *Handler) GetModelsV8(c *gin.Context) {
	h.getModels(c, true)
}

func (h *Handler) getModels(c *gin.Context, includeBillingModels bool) {
	scope := strings.ToLower(strings.TrimSpace(firstNonEmptyQuery(c, "scope", "source", "mode", "type")))
	if scope == "" {
		scope = "available"
	}

	switch scope {
	case "available", "active", "current":
		if includeBillingModels {
			c.JSON(http.StatusOK, gin.H{"scope": "available", "models": h.availableBillingModelDefinitions()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"scope":  "available",
			"models": registry.GetGlobalRegistry().GetAvailableModelDefinitions(),
		})
	case "static", "all-static", "definitions":
		channel := strings.TrimSpace(firstNonEmptyQuery(c, "channel", "provider"))
		if channel == "" {
			c.JSON(http.StatusOK, gin.H{
				"scope":  "static",
				"models": registry.GetAllStaticModelDefinitions(),
			})
			return
		}
		models := registry.GetStaticModelDefinitionsByChannel(channel)
		if models == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown channel", "channel": channel})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"scope":   "static",
			"channel": strings.ToLower(strings.TrimSpace(channel)),
			"models":  models,
		})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scope", "scope": scope})
	}
}

// GetStaticModelDefinitions returns a static model definitions.
func (h *Handler) GetStaticModelDefinitions(c *gin.Context) {
	channel := strings.TrimSpace(c.Param("channel"))
	if channel == "" {
		channel = strings.TrimSpace(c.Query("channel"))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel is required"})
		return
	}

	models := registry.GetStaticModelDefinitionsByChannel(channel)
	if models == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown channel", "channel": channel})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"channel": strings.ToLower(strings.TrimSpace(channel)),
		"models":  models,
	})
}

// Billing identities are resolved per credential, because one public alias may
// refer to different upstream models even within the same provider.
type billingModelTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type billingModelDefinition struct {
	*registry.ModelInfo
	BillingModels []billingModelTarget `json:"billing_models"`
}

func (h *Handler) availableBillingModelDefinitions() []billingModelDefinition {
	modelRegistry := registry.GetGlobalRegistry()
	models := modelRegistry.GetAvailableModelDefinitions()
	targets := make(map[string]map[billingModelTarget]struct{}, len(models))
	for _, model := range models {
		targets[model.ID] = make(map[billingModelTarget]struct{})
	}
	if h != nil && h.runtime != nil && h.runtime.CoreManager() != nil {
		manager := h.runtime.CoreManager()
		for _, auth := range manager.List() {
			if auth == nil || auth.Disabled {
				continue
			}
			provider, routes := modelRegistry.GetClientModelRoutes(auth.ID)
			if provider == "" {
				continue
			}
			for _, route := range routes {
				set, exists := targets[route]
				if !exists {
					continue
				}
				model := coreauth.CanonicalModelID(manager.ResolveExecutionModel(auth, route))
				if model != "" {
					set[billingModelTarget{Provider: provider, Model: model}] = struct{}{}
				}
			}
		}
	}
	out := make([]billingModelDefinition, 0, len(models))
	for _, model := range models {
		items := make([]billingModelTarget, 0, len(targets[model.ID]))
		for target := range targets[model.ID] {
			items = append(items, target)
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Provider != items[j].Provider {
				return items[i].Provider < items[j].Provider
			}
			return items[i].Model < items[j].Model
		})
		out = append(out, billingModelDefinition{ModelInfo: model, BillingModels: items})
	}
	return out
}
