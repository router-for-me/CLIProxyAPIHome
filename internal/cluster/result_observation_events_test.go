package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func TestClusterTimedResultsWriteOnlyStateChangesAndQuotaFences(t *testing.T) {
	ctx := context.Background()
	repo := newQuotaTestRepository(t)
	const authID, model = "observation-events", "gpt-5"
	seed := &coreauth.Auth{
		ID: authID, Index: authID, Provider: "codex", Status: coreauth.StatusActive,
		Metadata:    map[string]any{"access_token": "test-token"},
		ModelStates: map[string]*coreauth.ModelState{model: {Status: coreauth.StatusActive}},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, seed, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	nodeA := newQuotaTestNode(t, repo, authID)
	nodeB := newQuotaTestNode(t, repo, authID)
	base := time.Now().UTC().Add(-5 * time.Minute)
	tokenHash := coreauth.AccessTokenSHA256(seed)
	success := func(at time.Time) coreauth.Result {
		return coreauth.Result{AuthID: authID, Provider: "codex", Model: model, Success: true, ObservedAt: at, AccessTokenSHA256: tokenHash}
	}
	snapshot := func() (*coreauth.Auth, int64, int64) {
		t.Helper()
		auth, record, errGet := repo.GetAuth(ctx, authID)
		if errGet != nil {
			t.Fatal(errGet)
		}
		eventID, errEvent := repo.MaxEventID(ctx)
		if errEvent != nil {
			t.Fatal(errEvent)
		}
		return auth, record.Version, eventID
	}
	mark := func(node *coreauth.Manager, result coreauth.Result) {
		t.Helper()
		if errMark := node.MarkResult(ctx, result); errMark != nil {
			t.Fatal(errMark)
		}
	}
	_, initialVersion, initialEvent := snapshot()

	// Clean successes keep the pre-fence cost: no auth writes and no reload events.
	for i := 1; i <= 100; i++ {
		node := nodeA
		if i%2 == 0 {
			node = nodeB
		}
		mark(node, success(base.Add(time.Duration(i)*time.Second)))
	}
	if _, version, eventID := snapshot(); version != initialVersion || eventID != initialEvent {
		t.Fatalf("clean successes wrote state: version=%d want=%d event=%d want=%d", version, initialVersion, eventID, initialEvent)
	}

	// Opening a cooldown still publishes availability to peers.
	quota := clusterQuotaResult(authID, model)
	quota.ObservedAt = base.Add(101 * time.Second)
	retry := time.Hour
	quota.RetryAfter = &retry
	mark(nodeA, quota)
	staleSnapshot, quotaVersion, quotaEvent := snapshot()
	opened := staleSnapshot.ModelStates[model]
	if quotaEvent <= initialEvent || opened == nil || !opened.Quota.Exceeded {
		t.Fatalf("quota transition did not publish: event=%d state=%+v", quotaEvent, opened)
	}

	// An in-window failure on the node that opened the window persists only its
	// fence: the revision advances without a reload event.
	newer := clusterQuotaResult(authID, model)
	newer.ObservedAt = base.Add(102 * time.Second)
	mark(nodeA, newer)
	fenced, fencedVersion, fencedEvent := snapshot()
	state := fenced.ModelStates[model]
	if fencedVersion != quotaVersion+1 || fencedEvent != quotaEvent || !state.LastObservedAt.Equal(newer.ObservedAt) ||
		!state.Quota.NextRecoverAt.Equal(opened.Quota.NextRecoverAt) || state.Quota.BackoffLevel != opened.Quota.BackoffLevel {
		t.Fatalf("in-window fence write: version=%d want=%d event=%d want=%d state=%+v", fencedVersion, quotaVersion+1, fencedEvent, quotaEvent, state)
	}

	// Quiet revisions still reject delayed full-snapshot saves.
	staleAdapter := NewRuntimeAdapter(repo, "192.0.2.12")
	if _, version, errSave := staleAdapter.SaveWithStateVersion(ctx, staleSnapshot); errSave != nil || version != 0 {
		t.Fatalf("stale save version=%d err=%v, want ignored", version, errSave)
	}
	if afterSave, version, _ := snapshot(); version != fencedVersion || !afterSave.ModelStates[model].LastObservedAt.Equal(newer.ObservedAt) {
		t.Fatal("stale full snapshot overwrote a quietly persisted fence")
	}

	// A delayed availability event loads the newest row, including quiet fences.
	var event ClusterEventRecord
	if errFirst := repo.db.First(&event, quotaEvent).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	peerAdapter := NewRuntimeAdapter(repo, "192.0.2.13")
	if errApply := peerAdapter.ApplyEvent(ctx, event); errApply != nil {
		t.Fatal(errApply)
	}
	peerAuths := peerAdapter.ListMinimalAuths()
	if len(peerAuths) != 1 || !peerAuths[0].ModelStates[model].Quota.Exceeded || !peerAuths[0].ModelStates[model].LastObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("peer lost the cooldown or newer fence: %+v", peerAuths)
	}

	// A node holding the pre-fence cooldown checks an older success against the
	// persisted fence and must not clear the newer failure.
	lagging := coreauth.NewManager(NewRuntimeAdapter(repo, "192.0.2.14"), nil, nil)
	t.Cleanup(lagging.Shutdown)
	if _, errRegister := lagging.Register(coreauth.WithSkipPersist(ctx), staleSnapshot.Clone()); errRegister != nil {
		t.Fatal(errRegister)
	}
	mark(lagging, success(quota.ObservedAt.Add(500*time.Millisecond)))
	if cooled, version, _ := snapshot(); version != fencedVersion || !cooled.ModelStates[model].Quota.Exceeded {
		t.Fatalf("older success changed shared state: version=%d want=%d", version, fencedVersion)
	}

	// Recovery publishes. A node whose local view is still cooled writes at most
	// one quiet fence, then adopts the clean state and stays database-free.
	mark(nodeA, success(base.Add(103*time.Second)))
	cleared, clearVersion, clearEvent := snapshot()
	if clearEvent <= quotaEvent || cleared.ModelStates[model].Quota.Exceeded {
		t.Fatalf("cooldown recovery did not publish: event=%d state=%+v", clearEvent, cleared.ModelStates[model])
	}
	mark(lagging, success(base.Add(104*time.Second)))
	if _, version, eventID := snapshot(); version != clearVersion+1 || eventID != clearEvent {
		t.Fatalf("lagging success: version=%d want=%d event=%d want=%d", version, clearVersion+1, eventID, clearEvent)
	}
	mark(lagging, success(base.Add(105*time.Second)))
	if _, version, eventID := snapshot(); version != clearVersion+1 || eventID != clearEvent {
		t.Fatalf("clean success after adoption wrote state: version=%d event=%d", version, eventID)
	}
}

func TestRuntimeMutationPreservesNotFoundErrorChain(t *testing.T) {
	repo := newQuotaTestRepository(t)
	adapter := NewRuntimeAdapter(repo, "192.0.2.10")
	mutate := func(*coreauth.Auth) coreauth.StateMutation {
		t.Fatal("missing credential must not invoke the mutation callback")
		return coreauth.StateMutationUnchanged
	}
	_, errMutate := adapter.MutateAuthState(context.Background(), "missing", mutate)
	if !errors.Is(errMutate, coreauth.ErrAuthNotFound) || !errors.Is(errMutate, gorm.ErrRecordNotFound) {
		t.Fatalf("mutation lost an error classification: %v", errMutate)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errCanceled := adapter.MutateAuthState(ctx, "missing", mutate)
	if !errors.Is(errCanceled, context.Canceled) || errors.Is(errCanceled, coreauth.ErrAuthNotFound) {
		t.Fatalf("canceled query was classified as a deleted credential: %v", errCanceled)
	}
}
