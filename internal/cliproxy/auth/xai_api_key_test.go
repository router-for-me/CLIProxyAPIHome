package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestXAIAPIKeyDoesNotUseOAuthModelAliases(t *testing.T) {
	if channel := OAuthModelAliasChannel("xai", "api_key"); channel != "" {
		t.Fatalf("OAuthModelAliasChannel(xai, api_key) = %q, want empty", channel)
	}
}

func TestDispatchResolvesXAIAPIKeyModelAlias(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAIKey: []internalconfig.XAIKey{{
			APIKey:  "xai-key",
			BaseURL: "https://api.x.ai/v1",
			Models: []internalconfig.XAIModel{{
				Name:         "grok-4.5",
				Alias:        "grok-latest",
				ForceMapping: true,
			}},
		}},
	})
	auth := &Auth{
		ID:       "xai-api-key-auth",
		Provider: "xai",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":  "xai-key",
			"base_url": "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			homeConfigModelsMetadataKey: []map[string]any{{
				"id":            "grok-latest",
				"name":          "grok-4.5",
				"user_defined":  true,
				"force_mapping": true,
			}},
		},
	}
	registerDispatchTestAuth(t, manager, auth, "grok-latest")

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"xai"}, "grok-latest", Options{})
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != auth.ID {
		t.Fatalf("Dispatch() decision = %#v", decision)
	}
	if decision.Provider != "xai" || decision.UpstreamModel != "grok-4.5" {
		t.Fatalf("Dispatch() provider/model = %q/%q, want xai/grok-4.5", decision.Provider, decision.UpstreamModel)
	}
	if !decision.ForceMapping || decision.OriginalAlias != "grok-latest" {
		t.Fatalf("Dispatch() force mapping = %t/%q, want true/grok-latest", decision.ForceMapping, decision.OriginalAlias)
	}
}

func TestDispatchResolvesOpenAICompatibilityModelAlias(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{{
			Name:    "zai-coding",
			BaseURL: "https://api.example.test/v1",
			APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{
				APIKey: "zai-key",
			}},
			Models: []internalconfig.OpenAICompatibilityModel{{
				Name:  "glm-5.3",
				Alias: "fractalops-coding",
			}},
		}},
	})
	auth := &Auth{
		ID:       "zai-api-key-auth",
		Provider: "zai-coding",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":     "zai-key",
			"base_url":    "https://api.example.test/v1",
			"compat_name": "zai-coding",
		},
		Metadata: map[string]any{
			homeConfigModelsMetadataKey: []map[string]any{{
				"id":           "fractalops-coding",
				"name":         "glm-5.3",
				"user_defined": true,
			}},
		},
	}
	registerDispatchTestAuth(t, manager, auth, "fractalops-coding")

	decision, errDispatch := manager.Dispatch(
		context.Background(),
		[]string{"zai-coding"},
		"fractalops-coding",
		Options{},
	)
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != auth.ID {
		t.Fatalf("Dispatch() decision = %#v", decision)
	}
	if decision.Provider != "zai-coding" || decision.UpstreamModel != "glm-5.3" {
		t.Fatalf(
			"Dispatch() provider/model = %q/%q, want zai-coding/glm-5.3",
			decision.Provider,
			decision.UpstreamModel,
		)
	}
}
