package home

import (
	"errors"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestAntigravityTOSPauseWindowDefaultsToDisabled(t *testing.T) {
	if got := (&Runtime{}).antigravityTOSPauseWindow(); got != 0 {
		t.Fatalf("nil config pause = %v, want disabled", got)
	}
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 0}, {"0", 0}, {"off", 0}, {"false", 0}, {"none", 0}, {"disabled", 0},
		{" OFF ", 0}, {"invalid", 0}, {"-1h", 0}, {"0s", 0}, {"6h", 6 * time.Hour}, {" 30m ", 30 * time.Minute},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			runtime := &Runtime{cfg: &config.Config{Antigravity: config.AntigravityConfig{TOSPauseWindow: tc.raw}}}
			if got := runtime.antigravityTOSPauseWindow(); got != tc.want {
				t.Fatalf("pause window = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAntigravityTOSPauseDefaultDoesNotReadOrWriteFence(t *testing.T) {
	adapter := &tosPauseFailureAdapter{
		tosPauseTestAdapter: tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}},
		readErr:             errors.New("disabled pause must not read KV"),
		writeErr:            errors.New("disabled pause must not write KV"),
	}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	auth := &coreauth.Auth{ID: "default-pause", Index: "default-pause", Provider: "antigravity", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	runtime := &Runtime{cfg: &config.Config{}, coreManager: manager, clusterAdapter: adapter}
	result := coreauth.NewUsageResult(auth.ID, auth.Provider, "model", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`)
	if errRecord := runtime.recordAntigravityTOSPause(t.Context(), result, false); errRecord != nil {
		t.Fatal(errRecord)
	}
	if deadline, errRead := runtime.antigravityTOSPauseDeadline(t.Context()); errRead != nil || !deadline.IsZero() {
		t.Fatalf("default pause deadline = %v, %v", deadline, errRead)
	}
	if adapter.value != nil {
		t.Fatal("default configuration armed a pool pause")
	}
}
