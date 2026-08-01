package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func TestRuntimeAdapterMutateAuthStateRefreshesCacheAfterNoop(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-cache-auth"
	authA := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-a"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, authA, "create"); errUpsert != nil {
		t.Fatalf("UpsertAuth(token A) error = %v", errUpsert)
	}

	adapter := NewRuntimeAdapter(repo, "")
	cachedA, errGet := adapter.GetFullAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetFullAuth(token A) error = %v", errGet)
	}
	if got := coreauth.AccessTokenSHA256(cachedA); got != coreauth.AccessTokenSHA256(authA) {
		t.Fatalf("cached token A fingerprint = %q", got)
	}

	authB := authA.Clone()
	authB.Metadata["access_token"] = "token-b"
	if _, errUpsert := repo.UpsertAuth(ctx, authB, "update"); errUpsert != nil {
		t.Fatalf("UpsertAuth(token B) error = %v", errUpsert)
	}

	authoritative, errMutate := adapter.MutateAuthState(ctx, authID, func(*coreauth.Auth) bool {
		return false
	})
	if errMutate != nil {
		t.Fatalf("MutateAuthState(no-op) error = %v", errMutate)
	}
	wantHash := coreauth.AccessTokenSHA256(authB)
	if got := coreauth.AccessTokenSHA256(authoritative); got != wantHash {
		t.Fatalf("authoritative fingerprint = %q, want %q", got, wantHash)
	}
	adapter.mu.RLock()
	knownVersion := adapter.versions[authID]
	indexVersion := adapter.index[authID].Version
	adapter.mu.RUnlock()
	if knownVersion != authoritative.StateVersion || indexVersion != authoritative.StateVersion {
		t.Fatalf("no-op known/index versions = %d/%d, want %d", knownVersion, indexVersion, authoritative.StateVersion)
	}

	cachedB, errGet := adapter.GetFullAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetFullAuth(token B) error = %v", errGet)
	}
	if got := coreauth.AccessTokenSHA256(cachedB); got != wantHash {
		t.Fatalf("cache retained stale token fingerprint %q, want %q", got, wantHash)
	}
}

func TestRuntimeAdapterRejectsOutOfOrderMutationSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-version-auth"
	base := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-a"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, base, "create"); errUpsert != nil {
		t.Fatalf("UpsertAuth(token A) error = %v", errUpsert)
	}

	authB := base.Clone()
	authB.Metadata["access_token"] = "token-b"
	recordB, errUpsert := repo.UpsertAuth(ctx, authB, "update")
	if errUpsert != nil {
		t.Fatalf("UpsertAuth(token B) error = %v", errUpsert)
	}
	storedB, _, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth(token B) error = %v", errGet)
	}

	authC := authB.Clone()
	authC.Metadata["access_token"] = "token-c"
	recordC, errUpsert := repo.UpsertAuth(ctx, authC, "update")
	if errUpsert != nil {
		t.Fatalf("UpsertAuth(token C) error = %v", errUpsert)
	}
	storedC, _, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth(token C) error = %v", errGet)
	}
	if recordB.Version >= recordC.Version {
		t.Fatalf("record versions = B:%d C:%d, want increasing", recordB.Version, recordC.Version)
	}

	adapter := NewRuntimeAdapter(repo, "")
	if !adapter.cacheAuthSnapshot(authID, recordB, storedB) {
		t.Fatal("initial snapshot was not cached")
	}
	if !adapter.observeAuthSnapshot(authID, recordC, storedC) {
		t.Fatal("newer no-op snapshot was not observed")
	}
	if adapter.cacheAuthSnapshot(authID, recordB, storedB) {
		t.Fatal("older mutation snapshot overwrote newer observed state")
	}

	cached, errGet := adapter.GetFullAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetFullAuth() error = %v", errGet)
	}
	if got, want := coreauth.AccessTokenSHA256(cached), coreauth.AccessTokenSHA256(authC); got != want {
		t.Fatalf("cached fingerprint = %q, want newest %q", got, want)
	}
	adapter.mu.RLock()
	cachedVersion := adapter.index[authID].Version
	adapter.mu.RUnlock()
	if cachedVersion != recordC.Version {
		t.Fatalf("cached version = %d, want %d", cachedVersion, recordC.Version)
	}
}

func TestRuntimeAdapterLoadIndexPreservesNewerEvent(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-load-index-auth"
	authA := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-a"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, authA, "create"); errUpsert != nil {
		t.Fatalf("UpsertAuth(token A) error = %v", errUpsert)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var blockFirst atomic.Bool
	blockFirst.Store(true)
	const callbackName = "runtime_state_load_index_after_query"
	if errCallback := repo.db.Callback().Query().After("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if db.Statement != nil && db.Statement.Table == "auth" && blockFirst.CompareAndSwap(true, false) {
			close(started)
			<-release
		}
	}); errCallback != nil {
		t.Fatalf("register query callback error = %v", errCallback)
	}
	t.Cleanup(func() { _ = repo.db.Callback().Query().Remove(callbackName) })

	adapter := NewRuntimeAdapter(repo, "")
	loaded := make(chan error, 1)
	go func() { loaded <- adapter.LoadIndex(ctx) }()
	<-started

	authB := authA.Clone()
	authB.Metadata["access_token"] = "token-b"
	recordB, errUpsert := repo.UpsertAuth(ctx, authB, "update")
	if errUpsert != nil {
		close(release)
		<-loaded
		t.Fatalf("UpsertAuth(token B) error = %v", errUpsert)
	}
	if errRefresh := adapter.RefreshAuthIndex(ctx, authID); errRefresh != nil {
		close(release)
		<-loaded
		t.Fatalf("RefreshAuthIndex(token B) error = %v", errRefresh)
	}
	close(release)
	if errLoad := <-loaded; errLoad != nil {
		t.Fatalf("LoadIndex() error = %v", errLoad)
	}

	adapter.mu.RLock()
	knownVersion := adapter.versions[authID]
	indexVersion := adapter.index[authID].Version
	adapter.mu.RUnlock()
	if knownVersion != recordB.Version || indexVersion != recordB.Version {
		t.Fatalf("loaded versions = %d/%d, want %d", knownVersion, indexVersion, recordB.Version)
	}
}

func TestRuntimeAdapterSaveStampsAndFencesCallerVersion(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-save-version-auth"
	authA := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-a"},
	}
	adapter := NewRuntimeAdapter(repo, "")
	if _, errSave := adapter.Save(ctx, authA); errSave != nil {
		t.Fatalf("Save(token A) error = %v", errSave)
	}
	if authA.StateVersion <= 0 {
		t.Fatalf("Save(token A) version = %d, want positive", authA.StateVersion)
	}
	versionA := authA.StateVersion

	authB := authA.Clone()
	authB.Metadata["access_token"] = "token-b"
	authB.Disabled = true
	authB.Status = coreauth.StatusDisabled
	recordB, errUpsert := repo.UpsertAuth(ctx, authB, "update")
	if errUpsert != nil {
		t.Fatalf("UpsertAuth(token B) error = %v", errUpsert)
	}
	if errRefresh := adapter.RefreshAuthIndex(ctx, authID); errRefresh != nil {
		t.Fatalf("RefreshAuthIndex(token B) error = %v", errRefresh)
	}
	if _, errSave := adapter.Save(ctx, authA); errSave != nil {
		t.Fatalf("Save(stale token A) error = %v", errSave)
	}
	if authA.StateVersion != versionA {
		t.Fatalf("stale save stamped caller version = %d, want unchanged %d", authA.StateVersion, versionA)
	}

	persisted, record, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth() error = %v", errGet)
	}
	if record.Version != recordB.Version || !persisted.Disabled || persisted.Status != coreauth.StatusDisabled {
		t.Fatalf("stale save replaced newer lifecycle state: version=%d auth=%#v", record.Version, persisted)
	}
	if got, want := coreauth.AccessTokenSHA256(persisted), coreauth.AccessTokenSHA256(authB); got != want {
		t.Fatalf("persisted fingerprint = %q, want newer %q", got, want)
	}
}

func TestRuntimeAdapterStaleSaveDoesNotRestoreDeletedAuth(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-stale-save-auth"
	auth := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-before-delete"},
	}
	adapter := NewRuntimeAdapter(repo, "")
	if _, errSave := adapter.Save(ctx, auth); errSave != nil {
		t.Fatalf("Save(create) error = %v", errSave)
	}
	if auth.StateVersion <= 0 {
		t.Fatalf("Save() caller state version = %d, want positive", auth.StateVersion)
	}
	staleVersioned := auth.Clone()
	staleUnversioned := auth.Clone()
	staleUnversioned.StateVersion = 0
	if errDelete := adapter.Delete(ctx, authID); errDelete != nil {
		t.Fatalf("Delete() error = %v", errDelete)
	}
	if _, errSave := adapter.Save(ctx, staleUnversioned); errSave != nil {
		t.Fatalf("Save(stale unversioned) error = %v", errSave)
	}
	if _, errSave := adapter.Save(ctx, staleVersioned); errSave != nil {
		t.Fatalf("Save(stale versioned) error = %v", errSave)
	}
	if _, _, errGet := repo.GetAuth(ctx, authID); !errors.Is(errGet, gorm.ErrRecordNotFound) {
		t.Fatalf("GetAuth(after stale save) error = %v, want record not found", errGet)
	}
	adapter.mu.RLock()
	_, indexed := adapter.index[authID]
	_, cached := adapter.fullCache[authID]
	adapter.mu.RUnlock()
	if indexed || cached {
		t.Fatalf("stale save restored runtime auth: indexed=%v cached=%v", indexed, cached)
	}
}

func TestRuntimeAdapterStaleNotFoundDoesNotEraseRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-stale-not-found-auth"
	auth := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-before-delete"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	adapter := NewRuntimeAdapter(repo, "")
	if _, errGet := adapter.GetFullAuth(ctx, authID); errGet != nil {
		t.Fatalf("GetFullAuth() error = %v", errGet)
	}
	if errDelete := adapter.Delete(ctx, authID); errDelete != nil {
		t.Fatalf("Delete() error = %v", errDelete)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var blockFirst atomic.Bool
	blockFirst.Store(true)
	const callbackName = "runtime_state_stale_not_found_after_query"
	if errCallback := repo.db.Callback().Query().After("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if db.Statement != nil && db.Statement.Table == "auth" && blockFirst.CompareAndSwap(true, false) {
			close(started)
			<-release
		}
	}); errCallback != nil {
		t.Fatalf("register query callback error = %v", errCallback)
	}
	t.Cleanup(func() { _ = repo.db.Callback().Query().Remove(callbackName) })

	type getResult struct {
		auth *coreauth.Auth
		err  error
	}
	result := make(chan getResult, 1)
	go func() {
		loaded, errGet := adapter.GetFullAuth(ctx, authID)
		result <- getResult{auth: loaded, err: errGet}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("stale not-found query did not start: %v", ctx.Err())
	}

	restored := auth.Clone()
	restored.Metadata["access_token"] = "token-after-restore"
	recordRestored, errUpsert := repo.UpsertAuth(ctx, restored, "restore")
	if errUpsert != nil {
		close(release)
		<-result
		t.Fatalf("UpsertAuth(restore) error = %v", errUpsert)
	}
	if errRefresh := adapter.RefreshAuthIndex(ctx, authID); errRefresh != nil {
		close(release)
		<-result
		t.Fatalf("RefreshAuthIndex(restore) error = %v", errRefresh)
	}
	close(release)

	var loaded getResult
	select {
	case loaded = <-result:
	case <-ctx.Done():
		t.Fatalf("stale not-found query did not finish: %v", ctx.Err())
	}
	if loaded.err != nil {
		t.Fatalf("GetFullAuth() error = %v", loaded.err)
	}
	if got, want := coreauth.AccessTokenSHA256(loaded.auth), coreauth.AccessTokenSHA256(restored); got != want {
		t.Fatalf("restored fingerprint = %q, want %q", got, want)
	}
	adapter.mu.RLock()
	knownVersion := adapter.versions[authID]
	_, indexed := adapter.index[authID]
	adapter.mu.RUnlock()
	if knownVersion != recordRestored.Version || !indexed {
		t.Fatalf("restored runtime state = version %d indexed %v, want %d/true", knownVersion, indexed, recordRestored.Version)
	}
}

func TestRuntimeAdapterDeleteTombstoneRejectsInFlightSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	authID := "runtime-state-delete-auth"
	auth := &coreauth.Auth{
		ID:       authID,
		Index:    authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token-before-delete"},
	}
	record, errUpsert := repo.UpsertAuth(ctx, auth, "create")
	if errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	stored, _, errGet := repo.GetAuth(ctx, authID)
	if errGet != nil {
		t.Fatalf("GetAuth() error = %v", errGet)
	}

	adapter := NewRuntimeAdapter(repo, "")
	if !adapter.cacheAuthSnapshot(authID, record, stored) {
		t.Fatal("initial snapshot was not cached")
	}
	deletedVersion, errDelete := repo.SoftDeleteAuthWithVersion(ctx, authID)
	if errDelete != nil {
		t.Fatalf("SoftDeleteAuthWithVersion() error = %v", errDelete)
	}
	if errEvent := adapter.ApplyEvent(ctx, ClusterEventRecord{Scope: "auth", Op: "delete", EntityUUID: authID, Version: deletedVersion}); errEvent != nil {
		t.Fatalf("ApplyEvent(delete) error = %v", errEvent)
	}
	if errLoad := adapter.LoadIndex(ctx); errLoad != nil {
		t.Fatalf("LoadIndex(after delete) error = %v", errLoad)
	}
	if adapter.cacheAuthSnapshot(authID, record, stored) {
		t.Fatal("in-flight pre-delete snapshot resurrected deleted auth")
	}

	adapter.mu.RLock()
	knownVersion := adapter.versions[authID]
	_, indexed := adapter.index[authID]
	_, cached := adapter.fullCache[authID]
	adapter.mu.RUnlock()
	if knownVersion != deletedVersion || indexed || cached {
		t.Fatalf("delete tombstone = version %d indexed %v cached %v, want %d/false/false", knownVersion, indexed, cached, deletedVersion)
	}
	if _, errGet = adapter.GetFullAuth(ctx, authID); !errors.Is(errGet, coreauth.ErrFullAuthNotFound) {
		t.Fatalf("GetFullAuth(deleted) error = %v, want ErrFullAuthNotFound", errGet)
	}
}
