package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestDispatchResolvesOAuthModelAliasForceMapping(t *testing.T) {
	const (
		aliasModel    = "gemini-3.5-flash"
		upstreamModel = "gemini-3-flash-agent"
	)

	manager := NewManager(nil, nil, nil)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"antigravity": {{
			Name:         upstreamModel,
			Alias:        aliasModel,
			Fork:         true,
			ForceMapping: true,
		}},
	})
	auth := &Auth{
		ID:       "antigravity-oauth-auth",
		Provider: "antigravity",
		Status:   StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}
	registerDispatchTestAuth(t, manager, auth, aliasModel)

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"antigravity"}, aliasModel, Options{})
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != auth.ID {
		t.Fatalf("Dispatch() decision = %#v", decision)
	}
	if decision.Provider != "antigravity" || decision.UpstreamModel != upstreamModel {
		t.Fatalf("Dispatch() provider/model = %q/%q, want antigravity/%s", decision.Provider, decision.UpstreamModel, upstreamModel)
	}
	if !decision.ForceMapping || decision.OriginalAlias != aliasModel {
		t.Fatalf("Dispatch() force mapping = %t/%q, want true/%q", decision.ForceMapping, decision.OriginalAlias, aliasModel)
	}
}

func TestDispatchOAuthModelAliasWithoutForceMappingOmitsResponseMapping(t *testing.T) {
	const (
		aliasModel    = "gemini-3.5-flash"
		upstreamModel = "gemini-3-flash-agent"
	)

	manager := NewManager(nil, nil, nil)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"antigravity": {{
			Name:  upstreamModel,
			Alias: aliasModel,
			Fork:  true,
		}},
	})
	auth := &Auth{
		ID:       "antigravity-oauth-auth-no-force",
		Provider: "antigravity",
		Status:   StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}
	registerDispatchTestAuth(t, manager, auth, aliasModel)

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"antigravity"}, aliasModel, Options{})
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.UpstreamModel != upstreamModel {
		t.Fatalf("Dispatch() decision = %#v, want upstream model %q", decision, upstreamModel)
	}
	if decision.ForceMapping || decision.OriginalAlias != "" {
		t.Fatalf("Dispatch() force mapping = %t/%q, want false/empty", decision.ForceMapping, decision.OriginalAlias)
	}
}
