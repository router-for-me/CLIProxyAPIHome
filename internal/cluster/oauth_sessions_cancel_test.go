package cluster

import (
	"errors"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestOAuthSessionAtomicCredentialCompletion(t *testing.T) {
	for _, outcome := range []string{"success", "cancelled", "expired", "rollback"} {
		t.Run(outcome, func(t *testing.T) {
			repo, ctx := newOAuthSessionTestRepository(t)
			session, err := NewOAuthSessionRecord("codex", "atomic-state", map[string]any{"verifier": "fixture"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "expired" {
				session.ExpiresAt = time.Now().Add(-time.Minute)
			}
			if err = repo.UpsertOAuthSession(ctx, session); err != nil {
				t.Fatal(err)
			}
			if outcome == "cancelled" {
				// A separate repository represents cancellation from another Home node.
				if ok, errCancel := NewRepository(repo.db).CancelOAuthSession(ctx, session.State); !ok || errCancel != nil {
					t.Fatalf("cancel: %v %v", ok, errCancel)
				}
			}
			auth := &coreauth.Auth{ID: "oauth-atomic", Index: "oauth-atomic", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex", "access_token": "fixture"}}
			auths := []*coreauth.Auth{auth}
			if outcome == "rollback" {
				auths = append(auths, &coreauth.Auth{ID: "invalid-index"})
			}
			err = repo.CompleteOAuthSessionWithAuths(ctx, session.State, auths)
			if outcome == "success" && err != nil {
				t.Fatal(err)
			}
			if outcome == "rollback" && err == nil {
				t.Fatal("invalid credential did not abort transaction")
			}
			if (outcome == "cancelled" || outcome == "expired") && !errors.Is(err, ErrOAuthSessionNotPending) {
				t.Fatalf("completion error = %v", err)
			}
			stored, errList := repo.ListAuths(ctx)
			if errList != nil {
				t.Fatal(errList)
			}
			wantCount := 0
			if outcome == "success" {
				wantCount = 1
			}
			if len(stored) != wantCount {
				t.Fatalf("persisted credentials = %d, want %d", len(stored), wantCount)
			}
			current, errGet := repo.GetOAuthSession(ctx, session.State)
			if errGet != nil {
				t.Fatal(errGet)
			}
			switch outcome {
			case "success":
				if current.Status != "complete" || string(current.Data) != `{"auth_ids":["oauth-atomic"]}` {
					t.Fatalf("session = %+v", current)
				}
				if errRepeat := repo.CompleteOAuthSessionWithAuths(ctx, session.State, auths); !errors.Is(errRepeat, ErrOAuthSessionNotPending) {
					t.Fatalf("repeated save = %v", errRepeat)
				}
				if cancelled, errCancel := repo.CancelOAuthSession(ctx, session.State); cancelled || errCancel != nil {
					t.Fatalf("completed cancellation = %v %v", cancelled, errCancel)
				}
			case "cancelled":
				if errLate := repo.SetOAuthSessionError(ctx, session.State, "late provider error"); errLate != nil {
					t.Fatal(errLate)
				}
				if errLate := repo.CompleteOAuthSession(ctx, session.State); !errors.Is(errLate, ErrOAuthSessionNotPending) {
					t.Fatalf("late completion = %v", errLate)
				}
				current, _ = repo.GetOAuthSession(ctx, session.State)
				if current.Status != "error" || current.Error != "Authentication cancelled" || len(current.Data) != 0 {
					t.Fatalf("cancellation overwritten: %+v", current)
				}
			case "rollback":
				if current.Status != "" || len(current.Data) == 0 {
					t.Fatalf("session was not rolled back: %+v", current)
				}
			case "expired":
				if current.Status != "error" {
					t.Fatalf("expired session = %+v", current)
				}
			}
		})
	}
}
