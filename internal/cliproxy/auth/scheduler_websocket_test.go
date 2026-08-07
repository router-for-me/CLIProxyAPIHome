package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const websocketModel = "gpt-5-codex"

// websocketAuth builds a codex credential at the given priority with an explicit
// websocket capability flag.
func websocketAuth(id, priority string, websockets bool) *Auth {
	auth := &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			"priority":   priority,
			"websockets": "false",
		},
		ModelStates: map[string]*ModelState{websocketModel: {Status: StatusActive}},
	}
	if websockets {
		auth.Attributes["websockets"] = "true"
	}
	return auth
}

// newWebsocketScheduler registers the credentials with the shared model registry, which
// is what makes a model shard eligible, and then loads them into a scheduler.
func newWebsocketScheduler(t *testing.T, auths ...*Auth) *authScheduler {
	t.Helper()
	modelRegistry := registry.GetGlobalRegistry()
	scheduler := newAuthScheduler(&FillFirstSelector{}, nil)
	for _, auth := range auths {
		modelRegistry.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: websocketModel, Object: "model", Type: "openai"}})
		authID := auth.ID
		t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
		scheduler.upsertAuth(auth)
	}
	return scheduler
}

// TestDownstreamWebsocketPrefersWebsocketCredentialAcrossPriorities covers the
// preference CPA relies on for Codex: a websocket-capable credential is worth more
// than a higher priority HTTP-only one, because keeping the transport intact matters
// more than the tier. The signal reaches Home only through dispatch metadata, so
// without it this preference silently does nothing.
func TestDownstreamWebsocketPrefersWebsocketCredentialAcrossPriorities(t *testing.T) {
	httpOnly := websocketAuth("auth-http-high", "9", false)
	websocketCapable := websocketAuth("auth-ws-low", "1", true)
	scheduler := newWebsocketScheduler(t, httpOnly, websocketCapable)

	opts := Options{Metadata: map[string]any{DownstreamWebsocketMetadataKey: true}}
	picked, err := scheduler.pickSingle(context.Background(), "codex", websocketModel, opts, nil)
	if err != nil {
		t.Fatalf("pick failed: %v", err)
	}
	if picked.ID != websocketCapable.ID {
		t.Fatalf("picked %s, want the websocket credential %s", picked.ID, websocketCapable.ID)
	}
}

// TestWithoutDownstreamWebsocketPriorityWins keeps the preference scoped to websocket
// requests: an ordinary request must still follow credential priority.
func TestWithoutDownstreamWebsocketPriorityWins(t *testing.T) {
	httpOnly := websocketAuth("auth-http-high", "9", false)
	websocketCapable := websocketAuth("auth-ws-low", "1", true)
	scheduler := newWebsocketScheduler(t, httpOnly, websocketCapable)

	picked, err := scheduler.pickSingle(context.Background(), "codex", websocketModel, Options{}, nil)
	if err != nil {
		t.Fatalf("pick failed: %v", err)
	}
	if picked.ID != httpOnly.ID {
		t.Fatalf("picked %s, want the highest priority credential %s", picked.ID, httpOnly.ID)
	}
}

// TestDownstreamWebsocketIgnoredForOtherProviders keeps the preference limited to
// providers that can actually carry a websocket upstream.
func TestDownstreamWebsocketIgnoredForOtherProviders(t *testing.T) {
	httpOnly := websocketAuth("auth-http-high", "9", false)
	websocketCapable := websocketAuth("auth-ws-low", "1", true)
	httpOnly.Provider = "gemini"
	websocketCapable.Provider = "gemini"
	scheduler := newWebsocketScheduler(t, httpOnly, websocketCapable)

	opts := Options{Metadata: map[string]any{DownstreamWebsocketMetadataKey: true}}
	picked, err := scheduler.pickSingle(context.Background(), "gemini", websocketModel, opts, nil)
	if err != nil {
		t.Fatalf("pick failed: %v", err)
	}
	if picked.ID != httpOnly.ID {
		t.Fatalf("picked %s, want the highest priority credential %s", picked.ID, httpOnly.ID)
	}
}
