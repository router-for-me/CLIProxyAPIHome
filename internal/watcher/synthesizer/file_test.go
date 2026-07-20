package synthesizer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestSynthesizeAuthFileUsesPluginMultiAuthParser(t *testing.T) {
	now := time.Date(2026, time.June, 24, 12, 0, 0, 0, time.UTC)
	authDir := t.TempDir()
	fullPath := filepath.Join(authDir, "acme.json")
	parser := multiAuthParser{
		t: t,
		parseAuths: func(_ context.Context, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			if req.Provider != "acme" || req.Path != fullPath || req.FileName != "acme.json" || len(req.RawJSON) == 0 {
				t.Fatalf("request = %+v, want file context", req)
			}
			return []*coreauth.Auth{
				{ID: "acme-a", Provider: "acme", Label: "A"},
				{ID: "acme-b", Provider: "acme", Label: "B"},
			}, true, nil
		},
	}

	auths := SynthesizeAuthFile(&SynthesisContext{
		Config:           &config.Config{},
		AuthDir:          authDir,
		Now:              now,
		PluginAuthParser: parser,
	}, fullPath, []byte(`{"type":"acme","excluded_models":["skip-me"]}`))

	if len(auths) != 2 {
		t.Fatalf("len(auths) = %d, want 2", len(auths))
	}
	for index, auth := range auths {
		if auth == nil {
			t.Fatalf("auths[%d] = nil", index)
		}
		if !auth.CreatedAt.Equal(now) || !auth.UpdatedAt.Equal(now) {
			t.Fatalf("auths[%d] timestamps = %s/%s, want %s", index, auth.CreatedAt, auth.UpdatedAt, now)
		}
		if auth.Attributes["path"] != fullPath || auth.Attributes["source"] != fullPath {
			t.Fatalf("auths[%d] path/source attrs = %+v, want %s", index, auth.Attributes, fullPath)
		}
		if auth.Attributes["excluded_models"] != "skip-me" || auth.Attributes["auth_kind"] != "oauth" {
			t.Fatalf("auths[%d] excluded attrs = %+v, want skip-me oauth", index, auth.Attributes)
		}
	}
	if auths[0].ID != "acme-a" || auths[1].ID != "acme-b" {
		t.Fatalf("auth ids = %q/%q, want acme-a/acme-b", auths[0].ID, auths[1].ID)
	}
}

func TestSynthesizeAuthFileReadsConcurrencyLimits(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	auths := SynthesizeAuthFile(&SynthesisContext{
		Config:  &config.Config{},
		AuthDir: t.TempDir(),
		Now:     now,
	}, "codex.json", []byte(`{"type":"codex","max_in_flight":4,"max_in_flight_by_model":{"gpt-5.5":3,"gpt-5.5(high)":2,"ignored":0}}`))
	if len(auths) != 1 || auths[0] == nil {
		t.Fatalf("auths = %#v, want one auth", auths)
	}
	if auths[0].MaxInFlight != 4 {
		t.Fatalf("MaxInFlight = %d, want 4", auths[0].MaxInFlight)
	}
	if len(auths[0].MaxInFlightByModel) != 1 || auths[0].MaxInFlightByModel["gpt-5.5"] != 2 {
		t.Fatalf("MaxInFlightByModel = %#v, want gpt-5.5=2", auths[0].MaxInFlightByModel)
	}
}

func TestSynthesizeAuthFileRejectsInvalidConcurrencyLimits(t *testing.T) {
	for _, payload := range []string{
		`{"type":"codex","max_in_flight":-1}`,
		`{"type":"codex","max_in_flight":1.5}`,
		`{"type":"codex","max_in_flight":"2"}`,
		`{"type":"codex","max_in_flight_by_model":[]}`,
		`{"type":"codex","max_in_flight_by_model":{" ":1}}`,
		`{"type":"codex","max_in_flight_by_model":{"gpt-5":null}}`,
		`{"type":"codex","max_in_flight_by_model":{"gpt-5":-1}}`,
	} {
		auths := SynthesizeAuthFile(&SynthesisContext{
			Now:         time.Now(),
			IDGenerator: NewStableIDGenerator(),
		}, "codex.json", []byte(payload))
		if len(auths) != 0 {
			t.Fatalf("SynthesizeAuthFile(%s) = %#v, want rejected credential", payload, auths)
		}
	}
}

type multiAuthParser struct {
	t          *testing.T
	parseAuths func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error)
}

func (p multiAuthParser) ParseAuth(ctx context.Context, req pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
	p.t.Helper()
	p.t.Fatalf("ParseAuth(%+v) called, want ParseAuths", req)
	return nil, false, nil
}

func (p multiAuthParser) ParseAuths(ctx context.Context, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
	p.t.Helper()
	return p.parseAuths(ctx, req)
}
