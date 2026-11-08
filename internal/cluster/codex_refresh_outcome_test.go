package cluster

import (
	"context"
	"io"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCodexTransientFailureFinalizePreservesDisabledCredential(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	leaseID := "test-lease"
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{refreshLeaseAttribute: leaseID},
		Metadata:   map[string]any{"access_token": "access", "refresh_token": t.Name()},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	refreshed := auth.Clone()
	outcome := io.ErrUnexpectedEOF
	failure := coreauth.ApplyRefreshFailureState(refreshed, outcome, time.Now())
	if _, _, _, errMutate := repo.MutateAuth(ctx, auth.ID, "disable", func(current *coreauth.Auth) bool {
		current.Disabled = true
		current.Unavailable = true
		current.Status = coreauth.StatusDisabled
		current.StatusMessage = "disabled by operator"
		return true
	}); errMutate != nil {
		t.Fatal(errMutate)
	}
	controller := &RefreshController{repo: repo}
	_, finalized, _, disabled, errFinalize := controller.finalizeRefreshLease(ctx, auth.ID, leaseID, auth, auth, refreshed, failure)
	if errFinalize != nil || !finalized || !disabled {
		t.Fatalf("finalization failed: err=%v finalized=%v disabled=%v", errFinalize, finalized, disabled)
	}
	stored, _, errGet := repo.GetAuth(ctx, auth.ID)
	if errGet != nil || !stored.Disabled || stored.Status != coreauth.StatusDisabled || stored.StatusMessage != "disabled by operator" ||
		stored.Attributes[refreshLeaseAttribute] != "" {
		t.Fatal("refresh failure changed an operator-disabled credential")
	}
}

func TestCodexTransientFailureFinalizeRecognizesRefreshTokenReplacement(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	now := time.Now()
	before := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "unchanged-access", "refresh_token": "original"},
	}
	leased := before.Clone()
	leased.Attributes = map[string]string{refreshLeaseAttribute: "test-lease"}
	leased.NextRefreshAfter = now.Add(time.Hour)
	if _, errUpsert := repo.UpsertAuth(ctx, leased, "claim"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	refreshed := leased.Clone()
	outcome := io.ErrUnexpectedEOF
	failure := coreauth.ApplyRefreshFailureState(refreshed, outcome, now)
	if _, _, _, errMutate := repo.MutateAuth(ctx, leased.ID, "replace-refresh-token", func(current *coreauth.Auth) bool {
		current.Metadata["refresh_token"] = "replacement"
		return true
	}); errMutate != nil {
		t.Fatal(errMutate)
	}
	controller := &RefreshController{repo: repo}
	_, finalized, superseded, disabled, errFinalize := controller.finalizeRefreshLease(ctx, leased.ID, "test-lease", leased, before, refreshed, failure)
	if errFinalize != nil || !finalized || !superseded || disabled {
		t.Fatalf("replacement was not recognized: err=%v finalized=%v superseded=%v disabled=%v", errFinalize, finalized, superseded, disabled)
	}
	stored, _, errGet := repo.GetAuth(ctx, leased.ID)
	if errGet != nil || stored.Metadata["refresh_token"] != "replacement" || stored.Unavailable || stored.RuntimeRefreshBlocked ||
		stored.LastRefreshError != nil || !stored.NextRefreshAfter.IsZero() || stored.Attributes[refreshLeaseAttribute] != "" {
		t.Fatal("stale outcome or lease backoff leaked onto the replacement grant")
	}
}

func TestCodexTransientFailureFinalizePreservesAccessOnlyReplacement(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	before := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "old-access", "refresh_token": t.Name()},
	}
	leased := before.Clone()
	leased.Attributes = map[string]string{refreshLeaseAttribute: "test-lease"}
	leased.NextRefreshAfter = time.Now().Add(time.Hour)
	if _, errUpsert := repo.UpsertAuth(ctx, leased, "claim"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	refreshed := leased.Clone()
	outcome := io.ErrUnexpectedEOF
	failure := coreauth.ApplyRefreshFailureState(refreshed, outcome, time.Now())
	if _, _, _, errMutate := repo.MutateAuth(ctx, leased.ID, "replace-access-token", func(current *coreauth.Auth) bool {
		current.Metadata["access_token"] = "new-access"
		current.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
		return true
	}); errMutate != nil {
		t.Fatal(errMutate)
	}
	controller := &RefreshController{repo: repo}
	_, finalized, superseded, disabled, errFinalize := controller.finalizeRefreshLease(ctx, leased.ID, "test-lease", leased, before, refreshed, failure)
	if errFinalize != nil || !finalized || !superseded || disabled {
		t.Fatalf("access replacement was not recognized: err=%v finalized=%v superseded=%v disabled=%v", errFinalize, finalized, superseded, disabled)
	}
	stored, _, errGet := repo.GetAuth(ctx, leased.ID)
	if errGet != nil || stored.Metadata["access_token"] != "new-access" || stored.Unavailable || stored.LastRefreshError != nil ||
		!stored.NextRefreshAfter.IsZero() {
		t.Fatal("access replacement inherited stale failure state")
	}
}
