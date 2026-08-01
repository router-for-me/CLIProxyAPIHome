package cluster

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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
	if !adapter.cacheAuthSnapshot(authID, recordC, storedC) {
		t.Fatal("newer snapshot was not cached")
	}
	if adapter.cacheAuthSnapshot(authID, recordB, storedB) {
		t.Fatal("older mutation snapshot overwrote newer event state")
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
