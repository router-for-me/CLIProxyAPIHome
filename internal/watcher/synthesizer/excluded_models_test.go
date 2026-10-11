package synthesizer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func mustSynthesizeAuthFile(t *testing.T, ctx *SynthesisContext, fullPath string, raw string) []*coreauth.Auth {
	t.Helper()
	auths, errSynthesize := SynthesizeAuthFile(ctx, fullPath, []byte(raw))
	if errSynthesize != nil {
		t.Fatalf("SynthesizeAuthFile(%s) error = %v", fullPath, errSynthesize)
	}
	return auths
}

func TestExtractExcludedModelsFromMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     []string
	}{
		{name: "absent", metadata: map[string]any{}},
		{name: "null", metadata: map[string]any{"excluded_models": nil}},
		{name: "empty array", metadata: map[string]any{"excluded_models": []any{}}, want: []string{}},
		{name: "array", metadata: map[string]any{"excluded_models": []any{" a ", "", "b"}}, want: []string{"a", "b"}},
		{name: "string slice", metadata: map[string]any{"excluded_models": []string{"a"}}, want: []string{"a"}},
		{name: "single string", metadata: map[string]any{"excluded_models": "gemini-pro-agent"}, want: []string{"gemini-pro-agent"}},
		{name: "comma string", metadata: map[string]any{"excluded_models": " gemini-pro-agent , ,gemini-3-flash"}, want: []string{"gemini-pro-agent", "gemini-3-flash"}},
		{name: "blank string", metadata: map[string]any{"excluded_models": "  "}, want: []string{}},
		{name: "legacy key string", metadata: map[string]any{"excluded-models": "a,b"}, want: []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, errExtract := ExtractExcludedModelsFromMetadata(tc.metadata)
			if errExtract != nil {
				t.Fatalf("ExtractExcludedModelsFromMetadata() error = %v", errExtract)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractExcludedModelsFromMetadata() = %#v, want %#v", got, tc.want)
			}
		})
	}
	for _, value := range []any{123, true, map[string]any{}, []any{"a", 123}, []any{nil}} {
		if got, errExtract := ExtractExcludedModelsFromMetadata(map[string]any{"excluded_models": value}); errExtract == nil {
			t.Fatalf("ExtractExcludedModelsFromMetadata(%#v) = %v, want error", value, got)
		}
	}
}

func TestSynthesizeAuthFileRejectsInvalidExcludedModels(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: authDir, Now: time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)}
	badPath := filepath.Join(authDir, "bad.json")
	badPayload := `{"type":"claude","email":"bad@example.test","excluded_models":123}`
	auths, errSynthesize := SynthesizeAuthFile(ctx, badPath, []byte(badPayload))
	if errSynthesize == nil || !strings.Contains(errSynthesize.Error(), badPath) || len(auths) != 0 {
		t.Fatalf("SynthesizeAuthFile(bad) = (%d auths, %v), want error naming %s", len(auths), errSynthesize, badPath)
	}

	// A directory scan must surface the same error instead of silently dropping the file.
	if errWrite := os.WriteFile(badPath, []byte(badPayload), 0o600); errWrite != nil {
		t.Fatalf("write bad auth file: %v", errWrite)
	}
	if scanned, errScan := NewFileSynthesizer().Synthesize(ctx); errScan == nil || scanned != nil {
		t.Fatalf("FileSynthesizer.Synthesize() = (%v, %v), want invalid file error", scanned, errScan)
	}

	auths = mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "legacy.json"), `{"type":"claude","email":"legacy@example.test","excluded_models":"Model-A,model-b"}`)
	if len(auths) != 1 || auths[0].Attributes["excluded_models"] != "model-a,model-b" {
		t.Fatalf("legacy string auths = %+v, want comma string split into two rules", auths)
	}
}

func TestSynthesizeAuthFileDoesNotPersistGlobalOAuthExclusions(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{
		Config:  &config.Config{OAuthExcludedModels: map[string][]string{"claude": {"global-model"}}},
		AuthDir: authDir,
		Now:     time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC),
	}

	inherited := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "inherit.json"), `{"type":"claude","email":"inherit@example.test"}`)
	if len(inherited) != 1 {
		t.Fatalf("len(inherited) = %d, want 1", len(inherited))
	}
	if value, ok := inherited[0].Attributes["excluded_models"]; ok {
		t.Fatalf("inherited excluded_models = %q, want absent so global config is resolved at runtime", value)
	}
	if value, ok := inherited[0].Attributes["excluded_models_hash"]; ok {
		t.Fatalf("inherited excluded_models_hash = %q, want absent", value)
	}

	own := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "own.json"), `{"type":"claude","email":"own@example.test","excluded_models":["Own-Model"]}`)
	if len(own) != 1 {
		t.Fatalf("len(own) = %d, want 1", len(own))
	}
	if got, want := own[0].Attributes["excluded_models"], "own-model"; got != want {
		t.Fatalf("own excluded_models = %q, want %q", got, want)
	}
}
