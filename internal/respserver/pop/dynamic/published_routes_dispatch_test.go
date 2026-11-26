package dynamic

import (
	"fmt"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/tidwall/gjson"
)

func TestHandleAuthDevinAndCopilotWireFormat(t *testing.T) {
	const (
		devinSource   = "devin/swe-2"
		copilotSource = "copilot/gpt-5.3-codex"
		copilotBase   = "https://api.enterprise.githubcopilot.com"
	)

	t.Run("devin RPOP ordinary and stream", func(t *testing.T) {
		runtime, _ := newConcurrencyDispatchRuntime(t, nil)
		t.Cleanup(runtime.Stop)
		cfg := runtime.Config()
		cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{
			"devin": {{Name: devinSource, Alias: "team/chat", Fork: true, ForceMapping: true}},
		}
		runtime.CoreManager().SetOAuthModelAlias(cfg.OAuthModelAlias)

		auth := &coreauth.Auth{
			ID:         "devin-wire-auth",
			Index:      "devin-wire-auth",
			Provider:   "devin",
			Prefix:     "team",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"auth_kind": "oauth", "api_key": "devin-token"},
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "devin", []*registry.ModelInfo{
			{
				ID:                 "team/chat",
				SourceModelID:      devinSource,
				MetadataModelID:    "swe-2",
				ResponseModelAlias: "team/chat",
				Type:               "devin",
				ContextLength:      400000,
			},
			{
				ID:                 "team/team/chat",
				SourceModelID:      devinSource,
				MetadataModelID:    "swe-2",
				ResponseModelAlias: "team/chat",
				Type:               "devin",
				ContextLength:      400000,
			},
		})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, errRegister := runtime.CoreManager().Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}

		for _, stream := range []bool{false, true} {
			for _, route := range []string{"team/chat(high)", "team/team/chat(high)"} {
				t.Run(fmt.Sprintf("stream=%t/%s", stream, route), func(t *testing.T) {
					req := fmt.Sprintf(
						`{"type":"auth","model":%q,"concurrency_protocol":1,"scheduler_context":{"stream":%t},"headers":{"x-api-key":"dispatch-client-key"}}`,
						route, stream,
					)
					reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "devin-wire"), []string{"RPOP", "auth", req})
					raw := string(reply.BulkString)
					if gjson.Get(raw, "error").Exists() || !reply.AccountedAdmission {
						t.Fatalf("unexpected error reply: %s", raw)
					}
					if got := gjson.Get(raw, "model").String(); got != devinSource+"(high)" {
						t.Errorf("model = %q, want %q", got, devinSource+"(high)")
					}
					if got := gjson.Get(raw, "model_info.id").String(); got != devinSource {
						t.Errorf("model_info.id = %q, want %q", got, devinSource)
					}
					if got := gjson.Get(raw, "model_info.context_length").Int(); got != 400000 {
						t.Errorf("model_info.context_length = %d, want 400000", got)
					}
					if !gjson.Get(raw, "force_mapping").Bool() || gjson.Get(raw, "original_alias").String() != "team/chat" {
						t.Errorf("force_mapping=%v original_alias=%q, want true / team/chat", gjson.Get(raw, "force_mapping").Bool(), gjson.Get(raw, "original_alias").String())
					}
					for _, forbidden := range []string{"source_model_id", "metadata_model_id", "response_model_alias", "SourceModelID", "MetadataModelID", "ResponseModelAlias"} {
						if strings.Contains(raw, forbidden) {
							t.Errorf("forbidden internal field %q leaked in reply: %s", forbidden, raw)
						}
					}
				})
			}
		}
	})

	t.Run("copilot dispatch payload omits supported_endpoints and internal fields", func(t *testing.T) {
		result := &home.DispatchResult{
			Model:         copilotSource + "(high)",
			Provider:      "copilot",
			ForceMapping:  true,
			OriginalAlias: "friendly",
			ModelInfo: &home.DispatchModelInfo{
				ID:            copilotSource,
				Type:          "copilot",
				ContextLength: 524288,
				Thinking:      &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
			},
			Auth: &coreauth.Auth{
				ID:       "copilot-wire-auth",
				Index:    "copilot-wire-auth",
				Provider: "copilot",
				Metadata: map[string]any{
					"access_token":        "short-token",
					"refresh_token":       "secret-github-token",
					"base_url":            copilotBase,
					"copilot_catalog":     map[string]any{"models": []string{"gpt-5.3-codex"}},
					"supported_endpoints": []string{"/responses"},
				},
			},
			Concurrency: home.ConcurrencyAdmissionResult{CredentialID: "copilot-wire-auth", Model: copilotSource},
		}
		unaccounted, accounted, errPrepare := prepareDispatchResponse(result, "dispatch-client-key", &config.Config{})
		if errPrepare != nil {
			t.Fatal(errPrepare)
		}
		for _, payload := range [][]byte{unaccounted, accounted} {
			raw := string(payload)
			if got := gjson.Get(raw, "model").String(); got != copilotSource+"(high)" {
				t.Errorf("model = %q, want %q", got, copilotSource+"(high)")
			}
			if got := gjson.Get(raw, "model_info.id").String(); got != copilotSource {
				t.Errorf("model_info.id = %q, want %q", got, copilotSource)
			}
			if got := gjson.Get(raw, "model_info.context_length").Int(); got != 524288 {
				t.Errorf("model_info.context_length = %d, want 524288", got)
			}
			if !gjson.Get(raw, "force_mapping").Bool() || gjson.Get(raw, "original_alias").String() != "friendly" {
				t.Errorf("force_mapping=%v original_alias=%q, want true / friendly", gjson.Get(raw, "force_mapping").Bool(), gjson.Get(raw, "original_alias").String())
			}
			if got := gjson.Get(raw, "auth.metadata.base_url").String(); got != copilotBase {
				t.Errorf("auth.metadata.base_url = %q, want %q", got, copilotBase)
			}
			for _, forbidden := range []string{"supported_endpoints", "copilot_catalog", "refresh_token", "source_model_id", "metadata_model_id", "response_model_alias"} {
				if strings.Contains(raw, forbidden) {
					t.Errorf("forbidden field %q leaked in dispatch payload: %s", forbidden, raw)
				}
			}
		}
	})
}
