package cluster

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func installCodexJournalReadFailure(t *testing.T, repo *Repository, fail *atomic.Bool) {
	t.Helper()
	if errRegister := repo.db.Callback().Query().Before("gorm:query").Register("test:fail-fence-read", func(db *gorm.DB) {
		if db.Statement.Table == "codex_refresh_journal" && fail.Load() {
			db.AddError(io.ErrClosedPipe)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
}

func TestCodexLegacyFenceSurvivesJournalObservations(t *testing.T) {
	for _, stage := range []string{"claim", "pending", "receipt_read"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			repo := newRefreshTestRepository(t)
			var fail atomic.Bool
			fail.Store(true)
			if stage == "claim" {
				if errRegister := repo.db.Callback().Create().Before("gorm:create").Register("test:fail-legacy-fence-claim", func(db *gorm.DB) {
					if db.Statement.Table == "codex_refresh_journal" && fail.Load() {
						db.AddError(io.ErrClosedPipe)
					}
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
			} else if stage == "receipt_read" {
				installCodexJournalReadFailure(t, repo, &fail)
			}
			var owner string
			if stage != "claim" {
				var errClaim error
				owner, errClaim = repo.RefreshClaim(ctx, codexJournalTestKey(t.Name()))
				if errClaim != nil || owner == "" {
					t.Fatalf("claim grant: %v", errClaim)
				}
			}
			var calls atomic.Int32
			transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return codexJournalTestResponse(req, "unexpected-replay"), nil
			})
			installCodexJournalTransport(t, transport)
			auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "previous", "refresh_token": t.Name()},
			}
			codexauth.RecordRefreshOutcomeUnknown(auth.Metadata, codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, t.Name(), "response_read", io.ErrUnexpectedEOF))
			runtime := newRefreshTestRuntime(t, repo, auth, transport)
			snapshot, errRefresh := runtime.CoreManager().RefreshAuthCredential(ctx, auth)
			var authErr *coreauth.Error
			if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 0 {
				t.Fatalf("legacy fence was not respected: err=%v calls=%d", errRefresh, calls.Load())
			}
			if _, errUpsert := repo.UpsertAuth(ctx, snapshot, "failed-fence-persistence"); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			fail.Store(false)
			if owner != "" {
				// This owner never submitted a request, but that does not resolve the
				// historical exchange recorded by the legacy metadata fence.
				if errRelease := repo.RefreshRelease(ctx, codexJournalTestKey(t.Name()), owner); errRelease != nil {
					t.Fatal(errRelease)
				}
			}
			persisted, _, errGet := repo.GetAuth(ctx, auth.ID)
			if errGet != nil {
				t.Fatal(errGet)
			}
			_, errRefresh = runtime.CoreManager().RefreshAuthCredential(ctx, persisted)
			if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 0 {
				t.Fatalf("journal observation weakened historical uncertainty: err=%v calls=%d", errRefresh, calls.Load())
			}
		})
	}
}

func TestCodexPendingObserverCanRecoverAfterUnsubmittedOwnerReleasesClaim(t *testing.T) {
	for _, stage := range []string{"pending", "receipt_read"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			repo := newRefreshTestRepository(t)
			key := codexJournalTestKey(t.Name())
			owner, errClaim := repo.RefreshClaim(ctx, key)
			if errClaim != nil || owner == "" {
				t.Fatalf("claim grant: %v", errClaim)
			}
			var failRead atomic.Bool
			if stage == "receipt_read" {
				failRead.Store(true)
				installCodexJournalReadFailure(t, repo, &failRead)
			}
			var calls atomic.Int32
			transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return codexJournalTestResponse(req, "rotated"), nil
			})
			installCodexJournalTransport(t, transport)
			auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
				Metadata: map[string]any{"access_token": "previous", "refresh_token": t.Name()},
			}
			runtime := newRefreshTestRuntime(t, repo, auth, transport)
			snapshot, errRefresh := runtime.CoreManager().RefreshAuthCredential(ctx, auth)
			var authErr *coreauth.Error
			if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 0 {
				t.Fatalf("pending owner was not protected: err=%v calls=%d", errRefresh, calls.Load())
			}
			failRead.Store(false)
			// The owner proved that no HTTP request was submitted and released its claim.
			if errRelease := repo.RefreshRelease(ctx, key, owner); errRelease != nil {
				t.Fatal(errRelease)
			}
			result, errRefresh := runtime.CoreManager().RefreshAuthCredential(ctx, snapshot)
			if errRefresh != nil {
				t.Fatal(errRefresh)
			}
			if result.Metadata["refresh_token"] != "rotated" || calls.Load() != 1 {
				t.Fatalf("a stale observer recreated a permanent pending claim: calls=%d", calls.Load())
			}
		})
	}
}
