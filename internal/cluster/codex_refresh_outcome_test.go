package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCodexRefreshControllerPersistsUnknownOutcomeInDatabase(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	token := t.TempDir() + "-refresh"
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex", "access_token": "previous", "refresh_token": token},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	var calls atomic.Int32
	transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
			Body: io.NopCloser(strings.NewReader(`{"access_token":`))}, nil
	})
	originalTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9389, Secret: "test"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	_, errRefresh := controller.RefreshNow(ctx, auth.ID)
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 1 {
		t.Fatalf("refresh outcome: err=%v calls=%d", errRefresh, calls.Load())
	}
	persisted, _, errGet := repo.GetAuth(ctx, auth.ID)
	if errGet != nil || codexauth.RefreshOutcomeUnknown(persisted.Metadata, false) == nil || persisted.Disabled {
		t.Fatalf("unresolved outcome was not persisted separately from rejection: %v", errGet)
	}
	// Simulate a restarted owner with no process-cache entry for this new grant.
	persisted.Metadata["refresh_token"] = t.Name() + "-persisted-only"
	codexauth.RecordRefreshOutcomeUnknown(persisted.Metadata, codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name()+"-persisted-only", "response_read", io.ErrUnexpectedEOF))
	persisted.NextRefreshAfter = time.Time{}
	persisted.NextRetryAfter = time.Time{}
	if _, errUpsert := repo.UpsertAuth(ctx, persisted, "restart"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	_, errRefresh = controller.RefreshNow(ctx, auth.ID)
	if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 1 {
		t.Fatalf("persisted grant was replayed: err=%v calls=%d", errRefresh, calls.Load())
	}
}

func TestCodexUnknownRefreshMergeDoesNotFenceConcurrentReplacement(t *testing.T) {
	base := &coreauth.Auth{Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "unchanged-access", "refresh_token": "original"}}
	refreshed := base.Clone()
	errOutcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, "original", "response_decode", errors.New("invalid JSON"))
	errRefresh := coreauth.ApplyRefreshFailureState(refreshed, errOutcome, time.Now())
	current := base.Clone()
	current.Metadata["refresh_token"] = "replacement"
	merged := mergeClusterRefreshOutcome(current, base, refreshed, errRefresh, time.Now())
	if merged.Metadata["refresh_token"] != "replacement" || codexauth.RefreshOutcomeUnknown(merged.Metadata, false) != nil {
		t.Fatal("stale failure fenced a concurrent replacement")
	}
	if merged.Disabled || merged.Unavailable || merged.RuntimeRefreshBlocked || merged.LastRefreshError != nil {
		t.Fatal("a stale unknown outcome blocked the replacement grant")
	}
}

func TestCodexUnknownOutcomeFinalizePreservesDisabledCredential(t *testing.T) {
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
	outcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "response_read", io.ErrUnexpectedEOF)
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
		codexauth.RefreshOutcomeUnknown(stored.Metadata, false) == nil || stored.Attributes[refreshLeaseAttribute] != "" {
		t.Fatal("disabling the credential discarded its unresolved grant or changed its lifecycle state")
	}
}

func TestCodexUnknownOutcomeFinalizeRecognizesRefreshTokenReplacement(t *testing.T) {
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
	outcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, "original", "transport", errors.New("acceptance unknown"))
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

func TestCodexUnknownOutcomeFinalizePreservesAccessOnlyReplacement(t *testing.T) {
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
	outcome := codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "transport", errors.New("acceptance unknown"))
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
		codexauth.RefreshOutcomeUnknown(stored.Metadata, false) == nil || !stored.NextRefreshAfter.IsZero() {
		t.Fatal("access replacement discarded the unresolved grant or inherited stale failure state")
	}
}
