package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func codexJournalTestKey(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(codexauth.ClientID+"\x00"+token)))
}

func installCodexJournalTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
}

func codexJournalTestResponse(req *http.Request, token string) *http.Response {
	body, _ := json.Marshal(map[string]any{"access_token": "access-" + token, "refresh_token": token, "expires_in": 3600})
	return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}
}

func TestCodexRefreshJournalReusesReceiptsAcrossAuthRecordsAndRotations(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	original := t.Name()
	auth := &coreauth.Auth{ID: original, Index: original, Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "previous", "refresh_token": original},
	}
	other := auth.Clone()
	other.ID, other.Index = original+"-copy", original+"-copy"
	for _, entry := range []*coreauth.Auth{auth, other} {
		if _, errUpsert := repo.UpsertAuth(ctx, entry, "register"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
	}
	var calls atomic.Int32
	transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if errParse := req.ParseForm(); errParse != nil {
			return nil, errParse
		}
		token := req.Form.Get("refresh_token")
		pending, errRead := repo.RefreshRead(ctx, codexJournalTestKey(token))
		if errRead != nil || !strings.Contains(string(pending), `"pending"`) {
			t.Errorf("request was sent without a durable pending claim: %v", errRead)
		}
		return codexJournalTestResponse(req, token+"-next"), nil
	})
	installCodexJournalTransport(t, transport)
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(ctx), other.Clone()); errRegister != nil {
		t.Fatal(errRegister)
	}
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9389, Secret: "test"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	for i := 0; i < 2; i++ {
		if i > 0 {
			if _, _, _, errMutate := repo.MutateAuth(ctx, auth.ID, "expire-backoff", func(current *coreauth.Auth) bool {
				current.NextRefreshAfter = time.Time{}
				return true
			}); errMutate != nil {
				t.Fatal(errMutate)
			}
		}
		if _, errRefresh := controller.RefreshNow(ctx, auth.ID); errRefresh != nil {
			t.Fatal(errRefresh)
		}
	}
	// An expired intermediate receipt must still lead to its newer rotation.
	first, errRead := repo.RefreshRead(ctx, codexJournalTestKey(original))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var receipt map[string]any
	if errDecode := json.Unmarshal(first, &receipt); errDecode != nil {
		t.Fatal(errDecode)
	}
	receipt["tokens"].(map[string]any)["expired"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	expired, _ := json.Marshal(receipt)
	if errUpdate := repo.db.Model(&CodexRefreshRecord{}).Where("grant_sha256 = ?", codexJournalTestKey(original)).Update("receipt", JSONB(expired)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	// A stale pending observer must recover from the owner's later receipt.
	codexauth.RecordRefreshOutcomeUnknown(other.Metadata, codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, original, "pending", nil))
	if _, errUpsert := repo.UpsertAuth(ctx, other, "stale-observer"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if _, errRefresh := controller.RefreshNow(ctx, other.ID); errRefresh != nil {
		t.Fatal(errRefresh)
	}
	stored, _, errGet := repo.GetAuth(ctx, other.ID)
	if errGet != nil || stored.Metadata["refresh_token"] != original+"-next-next" || calls.Load() != 2 {
		t.Fatalf("old grant was resubmitted or latest receipt was lost: err=%v calls=%d", errGet, calls.Load())
	}
}

func TestCodexRefreshJournalSurvivesInterruptedOwnerAndAuthReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "home.db")
	repo := openRefreshTestRepositoryAt(t, path, true)
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "previous", "refresh_token": t.Name()},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	var calls atomic.Int32
	transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			// Stop execution after the durable claim and simulated submission,
			// before the response or auth finalization can be persisted.
			panic("injected refresh owner interruption")
		}
		return codexJournalTestResponse(req, "unexpected-replay"), nil
	})
	installCodexJournalTransport(t, transport)
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9389, Secret: "test"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("refresh owner was not interrupted")
			}
		}()
		_, _ = controller.RefreshNow(ctx, auth.ID)
	}()
	// A new connection and runtime have no dependency on the original owner.
	restarted := openRefreshTestRepositoryAt(t, path, false)
	stored, _, errGet := restarted.GetAuth(ctx, auth.ID)
	if errGet != nil {
		t.Fatal(errGet)
	}
	stored.NextRefreshAfter = time.Now().Add(-refreshLeaseDuration)
	if _, errUpsert := restarted.UpsertAuth(ctx, stored, "expire-lease"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	restartedRuntime := newRefreshTestRuntime(t, restarted, stored, transport)
	restartedController := NewRefreshController(coordinator, restartedRuntime, restarted, nil)
	_, errRefresh := restartedController.RefreshNow(ctx, auth.ID)
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 1 {
		t.Fatalf("orphaned claim was replayed after lease expiry: err=%v calls=%d", errRefresh, calls.Load())
	}
	if errDelete := restarted.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	// Re-import without metadata fences under a different identity.
	imported := auth.Clone()
	imported.ID, imported.Index = auth.ID+"-imported", auth.ID+"-imported"
	if _, errUpsert := restarted.UpsertAuth(ctx, imported, "import"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	_, errRefresh = restartedRuntime.CoreManager().RefreshAuthCredential(ctx, imported)
	if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 1 {
		t.Fatalf("auth replacement discarded the grant claim: err=%v calls=%d", errRefresh, calls.Load())
	}
}

type faultCodexJournal struct {
	*Repository
	failClaim bool
	failWrite bool
}

func (j *faultCodexJournal) RefreshClaim(ctx context.Context, key string) (string, error) {
	if j.failClaim {
		return "", io.ErrClosedPipe
	}
	return j.Repository.RefreshClaim(ctx, key)
}

func (j *faultCodexJournal) RefreshComplete(ctx context.Context, key, owner string, receipt []byte) error {
	if j.failWrite {
		return io.ErrClosedPipe
	}
	return j.Repository.RefreshComplete(ctx, key, owner, receipt)
}

func TestCodexRefreshJournalFailsClosedOnStorageFailure(t *testing.T) {
	for _, stage := range []string{"claim", "receipt_write"} {
		t.Run(stage, func(t *testing.T) {
			repo := newRefreshTestRepository(t)
			journal := &faultCodexJournal{Repository: repo, failClaim: stage == "claim", failWrite: stage == "receipt_write"}
			var calls atomic.Int32
			installCodexJournalTransport(t, refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return codexJournalTestResponse(req, "rotated"), nil
			}))
			svc := codexauth.NewCodexAuthWithProxyURL(nil, "")
			_, errRefresh := svc.RefreshTokens(codexauth.WithRefreshJournal(context.Background(), journal), t.Name())
			var unknown *codexauth.RefreshOutcomeUnknownError
			if !errors.As(errRefresh, &unknown) || unknown.Stage() != stage {
				t.Fatalf("storage failure was not fail-closed: %v", errRefresh)
			}
			wantCalls := int32(0)
			if stage == "receipt_write" {
				wantCalls = 1
				_, errRefresh = svc.RefreshTokens(codexauth.WithRefreshJournal(context.Background(), repo), t.Name())
				if !codexauth.IsRefreshOutcomeUnknown(errRefresh) {
					t.Fatalf("failed receipt persistence released the grant: %v", errRefresh)
				}
			}
			if calls.Load() != wantCalls {
				t.Fatalf("upstream submissions=%d, want %d", calls.Load(), wantCalls)
			}
		})
	}
}

func TestCodexRefreshJournalPersistsSuccessAfterCallerCancellation(t *testing.T) {
	repo := newRefreshTestRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	installCodexJournalTransport(t, refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return codexJournalTestResponse(req, "rotated"), nil
	}))
	svc := codexauth.NewCodexAuthWithProxyURL(nil, "")
	if _, errRefresh := svc.RefreshTokens(codexauth.WithRefreshJournal(ctx, repo), t.Name()); errRefresh != nil {
		t.Fatal(errRefresh)
	}
	result, errRefresh := svc.RefreshTokens(codexauth.WithRefreshJournal(context.Background(), repo), t.Name())
	if errRefresh != nil || result.RefreshToken != "rotated" || calls.Load() != 1 {
		t.Fatalf("cancellation discarded a successful receipt: err=%v calls=%d", errRefresh, calls.Load())
	}
}

func TestCodexRefreshJournalClaimFailureCanRecoverWithoutFencingUnsubmittedGrant(t *testing.T) {
	repo := newRefreshTestRepository(t)
	var failClaim atomic.Bool
	failClaim.Store(true)
	if errRegister := repo.db.Callback().Create().Before("gorm:create").Register("test:fail-refresh-claim", func(db *gorm.DB) {
		if db.Statement.Table == "codex_refresh_journal" && failClaim.Load() {
			db.AddError(io.ErrClosedPipe)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
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
	snapshot, errRefresh := runtime.CoreManager().RefreshAuthCredential(context.Background(), auth)
	var authErr *coreauth.Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != coreauth.ErrorCodeRefreshOutcomeUnknown || calls.Load() != 0 {
		t.Fatalf("claim failure submitted credentials: err=%v calls=%d", errRefresh, calls.Load())
	}
	failClaim.Store(false)
	_, errRefresh = runtime.CoreManager().RefreshAuthCredential(context.Background(), snapshot)
	if errRefresh != nil || calls.Load() != 1 {
		t.Fatalf("recovered database could not refresh an unsubmitted grant: err=%v calls=%d", errRefresh, calls.Load())
	}
}

func TestCodexRefreshJournalRestoresUnknownAndRejectedOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		unknown bool
	}{
		{"unknown", http.StatusOK, `{"access_token":`, true},
		{"unqualified_401", http.StatusUnauthorized, `{"error":"unauthorized"}`, true},
		{"rejected", http.StatusBadRequest, `{"error":"invalid_grant"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRefreshTestRepository(t)
			var calls atomic.Int32
			installCodexJournalTransport(t, refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: tc.status, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			for i := 0; i < 2; i++ {
				journal := NewRepository(repo.db)
				svc := codexauth.NewCodexAuthWithProxyURL(nil, "")
				_, errRefresh := svc.RefreshTokensWithRetry(codexauth.WithRefreshJournal(context.Background(), journal), t.Name(), 3)
				if errRefresh == nil || codexauth.IsRefreshOutcomeUnknown(errRefresh) != tc.unknown || calls.Load() != 1 {
					t.Fatalf("recorded outcome was lost or replayed: err=%v calls=%d", errRefresh, calls.Load())
				}
			}
		})
	}
}

func TestCodexRefreshJournalReleasesOnlyUnsubmittedGrants(t *testing.T) {
	repo := newRefreshTestRepository(t)
	ctx := codexauth.WithRefreshJournal(context.Background(), repo)
	var calls atomic.Int32
	installCodexJournalTransport(t, refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: io.ErrClosedPipe}
		}
		return codexJournalTestResponse(req, "rotated"), nil
	}))
	svc := codexauth.NewCodexAuthWithProxyURL(nil, "")
	if _, errRefresh := svc.RefreshTokens(ctx, t.Name()); errRefresh == nil || codexauth.IsRefreshOutcomeUnknown(errRefresh) {
		t.Fatalf("proven pre-submission failure became unresolved: %v", errRefresh)
	}
	if _, errRefresh := svc.RefreshTokens(ctx, t.Name()); errRefresh != nil || calls.Load() != 2 {
		t.Fatalf("safe retry was blocked: err=%v calls=%d", errRefresh, calls.Load())
	}
}

func TestCodexRefreshJournalExpiredSameTokenReceiptRequiresRecovery(t *testing.T) {
	repo := newRefreshTestRepository(t)
	ctx := context.Background()
	key := codexJournalTestKey(t.Name())
	owner, errClaim := repo.RefreshClaim(ctx, key)
	if errClaim != nil {
		t.Fatal(errClaim)
	}
	receipt, _ := json.Marshal(map[string]any{"tokens": &codexauth.CodexTokenData{AccessToken: "expired", RefreshToken: t.Name(), Expire: time.Now().Add(-time.Hour).Format(time.RFC3339)}})
	if errComplete := repo.RefreshComplete(ctx, key, owner, receipt); errComplete != nil {
		t.Fatal(errComplete)
	}
	var calls atomic.Int32
	installCodexJournalTransport(t, refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return codexJournalTestResponse(req, "unexpected"), nil
	}))
	svc := codexauth.NewCodexAuthWithProxyURL(nil, "")
	_, errRefresh := svc.RefreshTokens(codexauth.WithRefreshJournal(ctx, repo), t.Name())
	var unknown *codexauth.RefreshOutcomeUnknownError
	if !errors.As(errRefresh, &unknown) || unknown.Stage() != "receipt_expired" || calls.Load() != 0 {
		t.Fatalf("expired receipt renewed or replayed its grant: err=%v calls=%d", errRefresh, calls.Load())
	}
}

func TestCodexRefreshJournalClaimsAreAtomicPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var winners atomic.Int32
	var owner string
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Independent repositories bypass local singleflight and auth locks.
			claimed, errClaim := NewRepository(repo.db).RefreshClaim(ctx, codexJournalTestKey(t.Name()))
			if errClaim != nil {
				t.Error(errClaim)
			} else if claimed != "" {
				winners.Add(1)
				owner = claimed
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("grant had %d claim owners, want 1", winners.Load())
	}
	key := codexJournalTestKey(t.Name())
	if errComplete := repo.RefreshComplete(ctx, key, "other-owner", []byte(`{"stage":"transport"}`)); errComplete == nil {
		t.Fatal("a non-owner overwrote the claim")
	}
	if errRelease := repo.RefreshRelease(ctx, key, "other-owner"); errRelease == nil {
		t.Fatal("a non-owner released the claim")
	}
	if errComplete := repo.RefreshComplete(ctx, key, owner, []byte(`{"stage":"transport"}`)); errComplete != nil {
		t.Fatal(errComplete)
	}
	if errRelease := repo.RefreshRelease(ctx, key, owner); errRelease == nil {
		t.Fatal("a completed claim could be released")
	}
}

func TestCodexRefreshJournalMigratesOnlyGrantMatchedLegacyFences(t *testing.T) {
	for _, postgres := range []bool{false, true} {
		t.Run(fmt.Sprintf("postgres=%t", postgres), func(t *testing.T) {
			repo := newRefreshTestRepository(t)
			if postgres {
				repo = newPostgresQuiescenceRepository(t)
			}
			ctx := context.Background()
			for _, state := range []string{"unknown", "replacement", "completed"} {
				token := t.Name() + "-" + state
				auth := &coreauth.Auth{ID: state, Index: state, Provider: "codex", Status: coreauth.StatusActive,
					Metadata: map[string]any{"refresh_token": token},
				}
				if state == "unknown" {
					codexauth.RecordRefreshOutcomeUnknown(auth.Metadata, codexauth.NewRefreshOutcomeUnknownError(codexauth.ClientID, token, "response_read", io.ErrUnexpectedEOF))
				} else {
					auth.Attributes = map[string]string{refreshLeaseAttribute: "old-owner"}
				}
				if state == "replacement" {
					// The auth now holds a new grant but still has its old owner's
					// lease. A UUID lease cannot establish which token was sent.
					if _, errClaim := repo.RefreshClaim(ctx, codexJournalTestKey(token+"-old")); errClaim != nil {
						t.Fatal(errClaim)
					}
				}
				if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
					t.Fatal(errUpsert)
				}
				if state == "unknown" {
					if errDelete := repo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
						t.Fatal(errDelete)
					}
					if errUpdate := repo.db.Unscoped().Model(&AuthRecord{}).Where("uuid = ?", auth.ID).Update("id", "legacy-logical-id").Error; errUpdate != nil {
						t.Fatal(errUpdate)
					}
				}
			}
			key := codexJournalTestKey(t.Name() + "-completed")
			owner, errClaim := repo.RefreshClaim(ctx, key)
			if errClaim != nil {
				t.Fatal(errClaim)
			}
			done := []byte(`{"tokens":{"access_token":"kept","expired":"2099-01-01T00:00:00Z"}}`)
			if errComplete := repo.RefreshComplete(ctx, key, owner, done); errComplete != nil {
				t.Fatal(errComplete)
			}
			// The real startup migration runs under the schema transaction on PG.
			for i := 0; i < 2; i++ {
				if errMigrate := repo.db.Transaction(migrateCodexRefreshOutcomes); errMigrate != nil {
					t.Fatal(errMigrate)
				}
			}
			for _, state := range []string{"unknown", "replacement", "completed"} {
				receipt, errRead := repo.RefreshRead(ctx, codexJournalTestKey(t.Name()+"-"+state))
				if state == "replacement" {
					if errRead != nil || len(receipt) != 0 {
						t.Fatalf("a stale lease fenced the replacement grant: %v", errRead)
					}
					continue
				}
				if errRead != nil || len(receipt) == 0 {
					t.Fatalf("legacy %s state was lost: %v", state, errRead)
				}
				if state == "completed" && !strings.Contains(string(receipt), "kept") {
					t.Fatal("orphaned auth lease overwrote a completed receipt")
				}
				if state != "completed" && !strings.Contains(string(receipt), `"stage"`) {
					t.Fatalf("legacy %s state did not block the grant", state)
				}
				if nextOwner, errClaim := repo.RefreshClaim(ctx, codexJournalTestKey(t.Name()+"-"+state)); errClaim != nil || nextOwner != "" {
					t.Fatalf("migrated %s grant could be claimed again: %v", state, errClaim)
				}
			}
		})
	}
}

func TestCodexRefreshJournalSnapshotPreservesHistoryAndV16Compatibility(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		version  int
		postgres bool
	}{{16, false}, {currentDatabaseVersion, false}, {currentDatabaseVersion, true}} {
		t.Run(fmt.Sprintf("v%d/postgres=%t", tc.version, tc.postgres), func(t *testing.T) {
			version := tc.version
			repo := newRefreshTestRepository(t)
			for _, key := range []string{"pending", "success"} {
				owner, errClaim := repo.RefreshClaim(ctx, key)
				if errClaim != nil {
					t.Fatal(errClaim)
				}
				if key == "success" {
					if errComplete := repo.RefreshComplete(ctx, key, owner, []byte(`{"tokens":{"access_token":"secret","expired":"2099-01-01T00:00:00Z"}}`)); errComplete != nil {
						t.Fatal(errComplete)
					}
				}
			}
			path := filepath.Join(t.TempDir(), "snapshot.zip")
			if _, errExport := exportDatabaseSnapshotVersion(ctx, repo.db, path, version); errExport != nil {
				t.Fatal(errExport)
			}
			snapshot, errOpen := OpenDatabaseSnapshot(ctx, path)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			t.Cleanup(func() {
				if errClose := snapshot.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			target := newRefreshTestRepository(t)
			if tc.postgres {
				target = newPostgresQuiescenceRepository(t)
			}
			if _, errImport := ImportDatabaseSnapshot(ctx, target.db, snapshot, nil); errImport != nil {
				t.Fatal(errImport)
			}
			for _, key := range []string{"pending", "success"} {
				before, errRead := repo.RefreshRead(ctx, key)
				if errRead != nil {
					t.Fatal(errRead)
				}
				after, errRead := target.RefreshRead(ctx, key)
				var beforeValue, afterValue any
				if version == currentDatabaseVersion {
					if errDecode := json.Unmarshal(before, &beforeValue); errDecode != nil {
						t.Fatal(errDecode)
					}
					if errDecode := json.Unmarshal(after, &afterValue); errDecode != nil {
						t.Fatal(errDecode)
					}
				}
				if errRead != nil || (version == currentDatabaseVersion && !reflect.DeepEqual(beforeValue, afterValue)) || (version == 16 && len(after) != 0) {
					t.Fatalf("snapshot journal compatibility failed: version=%d err=%v", version, errRead)
				}
			}
		})
	}
}
