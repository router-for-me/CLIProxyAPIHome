package cluster

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestConcurrencyAdmissionRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *ConcurrencyAdmissionError
		want time.Duration
	}{
		{"nil", nil, 0},
		{"credential capacity", &ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded", RetryAfterMS: 250}, 250 * time.Millisecond},
		{"model capacity", &ConcurrencyAdmissionError{Type: "credential_model_concurrency_exceeded", RetryAfterMS: 500}, 500 * time.Millisecond},
		{"protocol", &ConcurrencyAdmissionError{Type: "concurrency_protocol_required", RetryAfterMS: 250}, 0},
		{"missing hint", &ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded"}, 0},
		{"negative", &ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded", RetryAfterMS: -1}, 0},
		{"overflow", &ConcurrencyAdmissionError{Type: "credential_concurrency_exceeded", RetryAfterMS: math.MaxInt64}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint := tc.err.RetryAfter()
			if tc.want == 0 {
				if hint != nil {
					t.Fatalf("RetryAfter() = %v, want nil", *hint)
				}
			} else if hint == nil || *hint != tc.want {
				t.Fatalf("RetryAfter() = %v, want %v", hint, tc.want)
			}
		})
	}
}

func TestDatabaseSaturationRetryHintBeatsCooldown(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, modelLimit := range []bool{false, true} {
			name := "builtin/credential"
			if session {
				name = "session/credential"
			}
			if modelLimit {
				name += "-model"
			}
			t.Run(name, func(t *testing.T) {
				const busy, cooling = "db-retry-a", "db-retry-b"
				repo := newCredentialFoundationTestRepository(t)
				seedConcurrencyAdmissionLifetime(t, repo, "fp-retry", "home-retry")
				seedConcurrencyAdmissionAuth(t, repo, busy)
				wantType := "credential_concurrency_exceeded"
				if modelLimit {
					seedConcurrencyAdmissionPolicy(t, repo, busy, nil, map[string]int64{"gpt": 1})
					wantType = "credential_model_concurrency_exceeded"
				} else {
					seedConcurrencyAdmissionPolicy(t, repo, busy, int64Pointer(1), nil)
				}
				request := ConcurrencyAdmissionRequest{
					CredentialID: busy, Model: "gpt", ProtocolVersion: 1,
					Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-retry", "home-retry"),
				}
				if _, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request); errAdmit != nil {
					t.Fatal(errAdmit)
				}

				cfg := &config.Config{CredentialConcurrency: config.DefaultCredentialConcurrencyConfig()}
				cfg.CredentialConcurrency.BusyRetryMin = "17ms"
				cfg.CredentialConcurrency.BusyRetryMax = "17ms"
				runtime, errRuntime := home.NewRuntime(cfg)
				if errRuntime != nil {
					t.Fatal(errRuntime)
				}
				t.Cleanup(runtime.Stop)
				if session {
					selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.FillFirstSelector{}})
					t.Cleanup(selector.Stop)
					runtime.CoreManager().SetSelector(selector)
				} else {
					runtime.CoreManager().SetSelector(&coreauth.FillFirstSelector{})
				}
				for _, id := range []string{busy, cooling} {
					registry.GetGlobalRegistry().RegisterClient(id, "openai", []*registry.ModelInfo{{ID: "gpt"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					auth := &coreauth.Auth{ID: id, Provider: "openai", Status: coreauth.StatusActive}
					if id == cooling {
						deadline := time.Now().Add(time.Minute)
						auth.ModelStates = map[string]*coreauth.ModelState{"gpt": {
							Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: deadline,
							Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: deadline},
						}}
					}
					if _, errRegister := runtime.CoreManager().Register(t.Context(), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				var producedErr error
				runtime.SetConcurrencyAdmitter(home.ConcurrencyAdmitterFunc(func(ctx context.Context, req home.ConcurrencyAdmissionRequest) (home.ConcurrencyAdmissionResult, error) {
					if req.CredentialID != busy {
						t.Fatalf("unexpected admission for %s", req.CredentialID)
					}
					_, producedErr = repo.AdmitCredentialConcurrency(ctx, request)
					return home.ConcurrencyAdmissionResult{}, producedErr
				}))
				result, errDispatch := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", nil, "", "", nil, "", home.DispatchConcurrencyContext{ProtocolVersion: 1})
				var admissionErr *ConcurrencyAdmissionError
				if result != nil || !errors.As(errDispatch, &admissionErr) || admissionErr.Type != wantType || admissionErr.RetryAfterMS != 0 || !errors.Is(errDispatch, producedErr) {
					t.Fatalf("result=%+v error=%v, want original database %s with zero hint", result, errDispatch, wantType)
				}
				var timed interface{ RetryAfter() *time.Duration }
				if !errors.As(errDispatch, &timed) || timed.RetryAfter() == nil || *timed.RetryAfter() != 17*time.Millisecond {
					t.Fatalf("error=%v, want effective configured 17ms retry hint", errDispatch)
				}
			})
		}
	}
}
