package home_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	homeruntime "github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

type failingInFlightAdapter struct {
	*cluster.RuntimeAdapter
}

func (a *failingInFlightAdapter) ReserveInFlightLease(context.Context, homeruntime.InFlightReserveInput) (*homeruntime.InFlightLease, error) {
	return nil, fmt.Errorf("in-flight store unavailable")
}

func (a *failingInFlightAdapter) RenewInFlightLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, fmt.Errorf("in-flight store unavailable")
}

func (a *failingInFlightAdapter) ReleaseInFlightLease(context.Context, string, string, string) (bool, error) {
	return false, fmt.Errorf("in-flight store unavailable")
}

func (a *failingInFlightAdapter) PurgeInFlightLeases(context.Context, time.Time, time.Duration, int) (int64, error) {
	return 0, fmt.Errorf("in-flight store unavailable")
}

type selectiveFailInFlightAdapter struct {
	*cluster.RuntimeAdapter
	failCredentialID string
}

func (a *selectiveFailInFlightAdapter) ReserveInFlightLease(ctx context.Context, input homeruntime.InFlightReserveInput) (*homeruntime.InFlightLease, error) {
	if input.CredentialID == a.failCredentialID {
		return nil, fmt.Errorf("in-flight store unavailable")
	}
	return a.RuntimeAdapter.ReserveInFlightLease(ctx, input)
}

func TestDispatchWithLeaseSkipsSaturatedCredential(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	})

	repo := cluster.NewRepository(db)
	now := time.Now().UTC()
	for _, auth := range []*coreauth.Auth{
		{
			ID:          "auth-a",
			Index:       "auth-a",
			Provider:    "codex",
			Status:      coreauth.StatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
			MaxInFlight: 1,
			Metadata:    map[string]any{"type": "codex", "priority": 10},
		},
		{
			ID:        "auth-b",
			Index:     "auth-b",
			Provider:  "codex",
			Status:    coreauth.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Metadata:  map[string]any{"type": "codex", "priority": 0},
		},
	} {
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
		}
	}
	runtime, errRuntime := homeruntime.NewRuntime(&config.Config{
		AuthDir: t.TempDir(),
		Routing: config.RoutingConfig{Strategy: "fill-first", SessionAffinity: true},
	})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.10"))
	if errReload := runtime.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}
	headers := http.Header{"X-Session-ID": []string{"sticky-session"}}
	first, errFirst := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", headers, "", homeruntime.DispatchLeaseContext{
		RequestID:  "request-first",
		DispatchID: "dispatch-first",
		CPANodeID:  "node-a",
	})
	if errFirst != nil || first == nil || first.AuthID != "auth-a" || first.LeaseID == "" {
		t.Fatalf("first sticky dispatch = %#v, %v", first, errFirst)
	}

	result, errDispatch := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", headers, "", homeruntime.DispatchLeaseContext{
		RequestID:  "request-new",
		DispatchID: "dispatch-new",
		CPANodeID:  "node-a",
	})
	if errDispatch != nil || result == nil {
		t.Fatalf("DispatchForAPIKeyWithLease() = %#v, %v", result, errDispatch)
	}
	if result.AuthID != "auth-b" || result.LeaseID == "" {
		t.Fatalf("dispatch result = %#v, want tracked auth-b fallback", result)
	}
	if _, errRelease := repo.ReleaseInFlightLease(ctx, result.LeaseID, "node-a", "completed"); errRelease != nil {
		t.Fatalf("release fallback lease: %v", errRelease)
	}
	replayed, errReplay := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", headers, "", homeruntime.DispatchLeaseContext{
		RequestID:  "request-new",
		DispatchID: "dispatch-new",
		CPANodeID:  "node-a",
	})
	var replayErr *homeruntime.DispatchReplayError
	if replayed != nil || !errors.As(errReplay, &replayErr) {
		t.Fatalf("released dispatch replay = %#v, %v", replayed, errReplay)
	}

	unlimited, errUnlimited := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", headers, "", homeruntime.DispatchLeaseContext{})
	if errUnlimited != nil || unlimited == nil || unlimited.AuthID != "auth-b" || unlimited.LeaseID != "" {
		t.Fatalf("missing identity unlimited dispatch = %#v, %v", unlimited, errUnlimited)
	}

	// Remove the unlimited fallback before validating the capped-only fail-closed path.
	authB, _, errGet := repo.GetAuth(ctx, "auth-b")
	if errGet != nil {
		t.Fatalf("GetAuth(auth-b) error = %v", errGet)
	}
	authB.Disabled = true
	authB.Status = coreauth.StatusDisabled
	if _, errUpsert := repo.UpsertAuth(ctx, authB, "disable"); errUpsert != nil {
		t.Fatalf("disable auth-b: %v", errUpsert)
	}
	if errReload := runtime.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("ReloadAuths() after disable error = %v", errReload)
	}
	limitedOnly, errLimited := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", headers, "", homeruntime.DispatchLeaseContext{})
	var authErr *coreauth.Error
	if limitedOnly != nil || !errors.As(errLimited, &authErr) || authErr.Code != "concurrency_identity_required" {
		t.Fatalf("capped-only missing identity = %#v, %v", limitedOnly, errLimited)
	}
}

func TestDispatchTrackerFailureIsBestEffortOnlyWhenUnlimited(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	})

	repo := cluster.NewRepository(db)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:        "auth-tracker-failure",
		Index:     "auth-tracker-failure",
		Provider:  "codex",
		Status:    coreauth.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  map[string]any{"type": "codex"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	runtime, errRuntime := homeruntime.NewRuntime(&config.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	baseAdapter := cluster.NewRuntimeAdapter(repo, "192.0.2.10")
	runtime.SetClusterAdapter(&failingInFlightAdapter{RuntimeAdapter: baseAdapter})
	if errReload := runtime.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}

	unlimited, errUnlimited := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", nil, "", homeruntime.DispatchLeaseContext{
		RequestID:  "request-unlimited",
		DispatchID: "dispatch-unlimited",
		CPANodeID:  "node-a",
	})
	if errUnlimited != nil || unlimited == nil || unlimited.AuthID != auth.ID || unlimited.LeaseID != "" {
		t.Fatalf("unlimited tracker failure = %#v, %v", unlimited, errUnlimited)
	}

	auth.MaxInFlight = 1
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "limit"); errUpsert != nil {
		t.Fatalf("limit auth: %v", errUpsert)
	}
	missingIdentity, errMissingIdentity := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", nil, "", homeruntime.DispatchLeaseContext{})
	var missingIdentityErr *coreauth.Error
	if missingIdentity != nil || !errors.As(errMissingIdentity, &missingIdentityErr) || missingIdentityErr.Code != "concurrency_identity_required" {
		t.Fatalf("stale runtime missing identity = %#v, %v", missingIdentity, errMissingIdentity)
	}
	limited, errLimited := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", nil, "", homeruntime.DispatchLeaseContext{
		RequestID:  "request-limited",
		DispatchID: "dispatch-limited",
		CPANodeID:  "node-a",
	})
	var authErr *coreauth.Error
	if limited != nil || !errors.As(errLimited, &authErr) || authErr.Code != "concurrency_tracker_unavailable" {
		t.Fatalf("limited tracker failure = %#v, %v", limited, errLimited)
	}
}

func TestDispatchTrackerFailureTakesPrecedenceOverPartialSaturation(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	})

	repo := cluster.NewRepository(db)
	now := time.Now().UTC()
	for _, auth := range []*coreauth.Auth{
		{
			ID:          "auth-saturated",
			Index:       "auth-saturated",
			Provider:    "codex",
			Status:      coreauth.StatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
			MaxInFlight: 1,
			Metadata:    map[string]any{"type": "codex", "priority": 10},
		},
		{
			ID:          "auth-tracker-error",
			Index:       "auth-tracker-error",
			Provider:    "codex",
			Status:      coreauth.StatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
			MaxInFlight: 1,
			Metadata:    map[string]any{"type": "codex", "priority": 0},
		},
	} {
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
		}
	}
	if _, errReserve := repo.ReserveInFlightLease(ctx, homeruntime.InFlightReserveInput{
		DispatchID:   "dispatch-existing",
		RequestID:    "request-existing",
		CredentialID: "auth-saturated",
		Provider:     "codex",
		Model:        "gpt-5.2",
		CPANodeID:    "node-a",
		TTL:          time.Minute,
	}); errReserve != nil {
		t.Fatalf("reserve saturated credential: %v", errReserve)
	}

	runtime, errRuntime := homeruntime.NewRuntime(&config.Config{
		AuthDir: t.TempDir(),
		Routing: config.RoutingConfig{Strategy: "fill-first"},
	})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	baseAdapter := cluster.NewRuntimeAdapter(repo, "192.0.2.10")
	runtime.SetClusterAdapter(&selectiveFailInFlightAdapter{
		RuntimeAdapter:   baseAdapter,
		failCredentialID: "auth-tracker-error",
	})
	if errReload := runtime.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}

	result, errDispatch := runtime.DispatchForAPIKeyWithLease(ctx, "gpt-5.2", nil, "", homeruntime.DispatchLeaseContext{
		RequestID:  "request-new",
		DispatchID: "dispatch-new",
		CPANodeID:  "node-a",
	})
	var authErr *coreauth.Error
	if result != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "concurrency_tracker_unavailable" {
		t.Fatalf("dispatch = %#v, %v, want concurrency_tracker_unavailable", result, errDispatch)
	}
}
