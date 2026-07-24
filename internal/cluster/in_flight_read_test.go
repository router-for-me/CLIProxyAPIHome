package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestReadInFlightObservationJoinsExactActiveLifetime(t *testing.T) {
	repo, oldIdentity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, oldIdentity, 1, []InFlightAggregate{{CredentialID: "old", Model: "m", Status: InFlightUnaccounted, Count: 7}})
	moveInFlightMembershipToNewLifetime(t, repo, oldIdentity.CertificateFingerprint, oldIdentity.MembershipConnectedAt.Add(time.Second))

	read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if len(read.Credentials) != 0 || read.CoverageComplete || read.AggregatesComplete {
		t.Fatalf("read = %#v, want no old-lifetime data and incomplete coverage", read)
	}
}

func TestReadInFlightObservationReportsProtocolCoverageAndMinimumBarrier(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}})
	makeInFlightSnapshotFresh(t, repo)

	read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if !read.ProtocolCoverageComplete || read.MinimumProcessedBarrierRevision == nil || *read.MinimumProcessedBarrierRevision != 1 {
		t.Fatalf("read = %#v, want protocol coverage and barrier 1", read)
	}

	if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", identity.CertificateFingerprint).Update("protocol_version", 0).Error; errUpdate != nil {
		t.Fatalf("set legacy protocol: %v", errUpdate)
	}
	read, errRead = repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if read.ProtocolCoverageComplete {
		t.Fatalf("read = %#v, want incomplete protocol coverage", read)
	}
}

func TestReadInFlightObservationSeparatesAccountedAndUnaccounted(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{
		{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 2},
		{CredentialID: "cred", Model: "m", Status: InFlightUnaccounted, Count: 3},
	})
	makeInFlightSnapshotFresh(t, repo)

	read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if !read.CoverageComplete || !read.AggregatesComplete || read.Stale || len(read.Credentials) != 1 {
		t.Fatalf("read = %#v, want fresh complete observation", read)
	}
	item := read.Credentials[0]
	if item.ObservedInFlight != 5 || item.ObservedAccounted != 2 || item.ObservedUnaccounted != 3 {
		t.Fatalf("item = %#v", item)
	}
	if len(item.Models) != 1 || item.Models[0].ObservedInFlight != 5 || item.Models[0].ObservedAccounted != 2 || item.Models[0].ObservedUnaccounted != 3 {
		t.Fatalf("models = %#v", item.Models)
	}
}

func TestSnapshotUpdatedAtStaleExactBoundaryIsFresh(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 10, 0, time.UTC)
	if snapshotUpdatedAtStale(now, now.Add(-10*time.Second), 10*time.Second) {
		t.Fatal("snapshot at the stale threshold is stale")
	}
	if !snapshotUpdatedAtStale(now, now.Add(-10*time.Second-time.Nanosecond), 10*time.Second) {
		t.Fatal("snapshot older than the stale threshold is fresh")
	}
}

func TestReadInFlightObservationUsesDatabaseIngestTimeForFreshness(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}})
	makeInFlightSnapshotFresh(t, repo)

	read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() fresh error = %v", errRead)
	}
	if read.Stale || !read.CoverageComplete {
		t.Fatalf("fresh read = %#v", read)
	}
	var snapshot CPAInFlightSnapshotRecord
	if errSnapshot := repo.db.Where("certificate_fingerprint = ?", identity.CertificateFingerprint).First(&snapshot).Error; errSnapshot != nil {
		t.Fatalf("read in-flight snapshot: %v", errSnapshot)
	}
	wantFreshUntil := snapshot.UpdatedAt.Add(10 * time.Second).UTC()
	if read.FreshUntil == nil || !read.FreshUntil.Equal(wantFreshUntil) {
		t.Fatalf("FreshUntil = %v, want %s", read.FreshUntil, wantFreshUntil)
	}

	makeInFlightSnapshotUpdatedAt(t, repo, databaseNowForInFlightTest(t, repo).Add(-11*time.Second))
	read, errRead = repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() stale error = %v", errRead)
	}
	if !read.Stale || read.CoverageComplete {
		t.Fatalf("stale read = %#v", read)
	}
}

func TestReadInFlightObservationRejectsCrossMembershipAggregateOverflow(t *testing.T) {
	repo, first := newActiveInFlightTestRepository(t)
	second := InFlightIngestIdentity{
		CertificateFingerprint: "fingerprint-second",
		NodeID:                 "node-b",
		MembershipConnectedAt:  first.MembershipConnectedAt.Add(time.Second),
	}
	if errCreate := repo.db.Create(&CPANodeMembershipRecord{
		CertificateFingerprint: second.CertificateFingerprint,
		NodeID:                 second.NodeID,
		State:                  MembershipStateActive,
		ConnectedAt:            second.MembershipConnectedAt,
	}).Error; errCreate != nil {
		t.Fatalf("create second membership: %v", errCreate)
	}
	publishInFlightSnapshot(t, repo, first, 1, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: math.MaxInt64}})
	publishInFlightSnapshot(t, repo, second, 1, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}})
	makeInFlightSnapshotFresh(t, repo)

	_, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if !errors.Is(errRead, ErrInFlightRevisionOverflow) {
		t.Fatalf("ReadInFlightObservation() error = %v, want %v", errRead, ErrInFlightRevisionOverflow)
	}
}

func TestReadInFlightObservationIncompleteAttemptMarksVisibleSnapshotStale(t *testing.T) {
	tests := []struct {
		name  string
		stage func(t *testing.T, repo *Repository, identity InFlightIngestIdentity)
	}{
		{
			name: "staging",
			stage: func(t *testing.T, repo *Repository, identity InFlightIngestIdentity) {
				t.Helper()
				part := inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "new", Model: "m", Status: InFlightAccounted, Count: 1}}, nil)
				if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, part), DefaultInFlightLimits()); errIngest != nil {
					t.Fatalf("stage frame: %v", errIngest)
				}
			},
		},
		{
			name: "overflow",
			stage: func(t *testing.T, repo *Repository, identity InFlightIngestIdentity) {
				t.Helper()
				frame := InFlightSnapshotFrame{Kind: InFlightFrameOverflow, Revision: 2, ObservedAt: time.Date(2026, 7, 21, 12, 0, 1, 0, time.UTC), BarrierRevision: 1, AggregateGroupCount: 1}
				if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, frame), DefaultInFlightLimits()); errIngest != nil {
					t.Fatalf("stage overflow: %v", errIngest)
				}
			},
		},
		{
			name: "rejected",
			stage: func(t *testing.T, repo *Repository, identity InFlightIngestIdentity) {
				t.Helper()
				raw := []byte(`{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:01Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[],"token":"secret"}`)
				if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, raw, DefaultInFlightLimits()); errIngest == nil {
					t.Fatal("invalid frame was accepted")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, identity := newActiveInFlightTestRepository(t)
			publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "visible", Model: "m", Status: InFlightAccounted, Count: 1}})
			makeInFlightSnapshotFresh(t, repo)
			test.stage(t, repo, identity)

			read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
			if errRead != nil {
				t.Fatalf("ReadInFlightObservation() error = %v", errRead)
			}
			if !read.Stale || read.CoverageComplete || read.AggregatesComplete || len(read.Credentials) != 1 {
				t.Fatalf("read = %#v, want stale incomplete visible observation", read)
			}
		})
	}
}

func TestReadInFlightObservationCancelingMembershipDoesNotMeanZero(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}})
	makeInFlightSnapshotFresh(t, repo)
	if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", identity.CertificateFingerprint).Update("state", MembershipStateCanceling).Error; errUpdate != nil {
		t.Fatalf("cancel membership: %v", errUpdate)
	}

	read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if read.CoverageComplete || len(read.Credentials) != 0 {
		t.Fatalf("read = %#v, canceling membership must not become zero", read)
	}
}

func TestReadInFlightObservationReturnsSortedDetailsAndTruncation(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	startedAt := time.Date(2026, 7, 21, 12, 0, 1, 0, time.UTC)
	part := inFlightPart(1, 0, 1, []InFlightAggregate{
		{CredentialID: "z", Model: "z-model", Status: InFlightUnaccounted, Count: 1},
		{CredentialID: "a", Model: "z-model", Status: InFlightAccounted, Count: 1},
		{CredentialID: "a", Model: "a-model", Status: InFlightAccounted, Count: 1},
	}, []InFlightRequestDetail{
		{RequestID: "later", CredentialID: "z", Model: "z-model", RequestKind: "http", StartedAt: startedAt.Add(time.Second)},
		{RequestID: "z-request", CredentialID: "a", Model: "a", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "z", Model: "a", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "a", Model: "z", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "a", Model: "a", RequestKind: "sse", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "a", Model: "a", RequestKind: "http", StartedAt: startedAt},
	})
	part.DetailsTruncated = true
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, part), DefaultInFlightLimits())
	if errIngest != nil || !result.Published {
		t.Fatalf("publish result = %#v error = %v", result, errIngest)
	}
	makeInFlightSnapshotFresh(t, repo)

	read, errRead := repo.ReadInFlightObservation(context.Background(), 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if !read.DetailsTruncated || len(read.Credentials) != 2 || read.Credentials[0].CredentialID != "a" || read.Credentials[0].Models[0].Model != "a-model" {
		t.Fatalf("credentials = %#v", read.Credentials)
	}
	want := []InFlightRequestDetail{
		{RequestID: "a-request", CredentialID: "a", Model: "a", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "a", Model: "a", RequestKind: "sse", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "a", Model: "z", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "a-request", CredentialID: "z", Model: "a", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "z-request", CredentialID: "a", Model: "a", RequestKind: "http", StartedAt: startedAt},
		{RequestID: "later", CredentialID: "z", Model: "z-model", RequestKind: "http", StartedAt: startedAt.Add(time.Second)},
	}
	if len(read.Details) != len(want) {
		t.Fatalf("details = %#v, want %#v", read.Details, want)
	}
	for index := range want {
		if read.Details[index] != want[index] {
			t.Fatalf("details[%d] = %#v, want %#v", index, read.Details[index], want[index])
		}
	}
}

func TestReadInFlightObservationCloseReopenDuringReadSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.db")
	readerDB, errOpenReader := OpenSQLite(context.Background(), path)
	if errOpenReader != nil {
		t.Fatalf("open reader SQLite database: %v", errOpenReader)
	}
	if errMigrate := AutoMigrate(readerDB); errMigrate != nil {
		t.Fatalf("migrate reader SQLite database: %v", errMigrate)
	}
	writerDB, errOpenWriter := OpenSQLite(context.Background(), path)
	if errOpenWriter != nil {
		t.Fatalf("open writer SQLite database: %v", errOpenWriter)
	}
	repo, identity := newActiveInFlightTestRepositoryWithDatabase(t, readerDB)
	testReadInFlightObservationCloseReopenDuringRead(t, repo, writerDB, identity)
}

func TestReadInFlightObservationCloseReopenDuringReadPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	repo, identity := newActiveInFlightTestRepositoryWithDatabase(t, repo.db)
	testReadInFlightObservationCloseReopenDuringRead(t, repo, repo.db, identity)
}

func testReadInFlightObservationCloseReopenDuringRead(t *testing.T, repo *Repository, writerDB *gorm.DB, identity InFlightIngestIdentity) {
	t.Helper()
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "old", Model: "m", Status: InFlightAccounted, Count: 1}})
	makeInFlightSnapshotFresh(t, repo)

	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()
	writerStart := make(chan struct{})
	writerDone := make(chan error, 1)
	var fired atomic.Bool
	const callbackName = "test:in-flight-observation-close-reopen"
	if errRegister := repo.db.Callback().Query().Before("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if db.Statement.Schema == nil || db.Statement.Schema.Table != (CPAInFlightSnapshotRecord{}).TableName() || !fired.CompareAndSwap(false, true) {
			return
		}
		select {
		case writerStart <- struct{}{}:
		case <-ctx.Done():
			db.AddError(ctx.Err())
			return
		}
		select {
		case errWriter := <-writerDone:
			if errWriter != nil {
				db.AddError(errWriter)
			}
		case <-ctx.Done():
			db.AddError(ctx.Err())
		}
	}); errRegister != nil {
		t.Fatalf("register read barrier: %v", errRegister)
	}
	defer func() {
		if errRemove := repo.db.Callback().Query().Remove(callbackName); errRemove != nil {
			t.Errorf("remove read barrier: %v", errRemove)
		}
	}()
	go func() {
		select {
		case <-writerStart:
			writerDone <- closeAndReopenInFlightMembership(ctx, writerDB, identity)
		case <-ctx.Done():
			writerDone <- ctx.Err()
		}
	}()

	read, errRead := repo.ReadInFlightObservation(ctx, 10*time.Second)
	if errRead != nil {
		t.Fatalf("ReadInFlightObservation() error = %v", errRead)
	}
	if !fired.Load() {
		t.Fatal("read barrier did not run")
	}
	for _, credential := range read.Credentials {
		if credential.CredentialID == "new" {
			t.Fatalf("read joined a new-lifetime snapshot to an old membership: %#v", read)
		}
	}
	oldSnapshot := len(read.Credentials) == 1 && read.Credentials[0].CredentialID == "old" && read.CoverageComplete && read.AggregatesComplete && !read.Stale
	if !oldSnapshot && read.CoverageComplete {
		t.Fatalf("read = %#v, want the old snapshot or incomplete coverage", read)
	}
}

func closeAndReopenInFlightMembership(ctx context.Context, db *gorm.DB, identity InFlightIngestIdentity) error {
	connectedAt := identity.MembershipConnectedAt.Add(time.Second)
	payload, errMarshal := json.Marshal(InFlightSnapshotPayload{
		Aggregates: []InFlightAggregate{{CredentialID: "new", Model: "m", Status: InFlightAccounted, Count: 1}},
		Details:    []InFlightRequestDetail{},
	})
	if errMarshal != nil {
		return errMarshal
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		if errClose := tx.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", identity.CertificateFingerprint).Updates(map[string]any{"state": MembershipStateClosed, "updated_at": now}).Error; errClose != nil {
			return errClose
		}
		if errReopen := tx.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", identity.CertificateFingerprint).Updates(map[string]any{"state": MembershipStateActive, "connected_at": connectedAt, "last_seen_at": now, "updated_at": now}).Error; errReopen != nil {
			return errReopen
		}
		if errSnapshot := tx.Model(&CPAInFlightSnapshotRecord{}).Where("certificate_fingerprint = ?", identity.CertificateFingerprint).Updates(map[string]any{
			"membership_connected_at": connectedAt,
			"revision":                int64(2),
			"observed_at":             now,
			"payload":                 JSONB(payload),
			"updated_at":              now,
		}).Error; errSnapshot != nil {
			return errSnapshot
		}
		if errDelete := tx.Where("certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).Delete(&CPAInFlightSnapshotAttemptRecord{}).Error; errDelete != nil {
			return errDelete
		}
		return tx.Create(&CPAInFlightSnapshotAttemptRecord{
			CertificateFingerprint: identity.CertificateFingerprint,
			MembershipConnectedAt:  connectedAt,
			HighestSeenRevision:    2,
			State:                  "complete",
			ObservedAt:             now,
			BarrierRevision:        1,
			PartCount:              1,
			ReceivedPartCount:      1,
			EncodedBytes:           int64(len(payload)),
			AggregateGroupCount:    1,
			UpdatedAt:              now,
		}).Error
	})
}

func moveInFlightMembershipToNewLifetime(t *testing.T, repo *Repository, fingerprint string, connectedAt time.Time) {
	t.Helper()
	if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", fingerprint).Updates(map[string]any{"connected_at": connectedAt, "state": MembershipStateActive}).Error; errUpdate != nil {
		t.Fatalf("move membership: %v", errUpdate)
	}
}

func databaseNowForInFlightTest(t *testing.T, repo *Repository) time.Time {
	t.Helper()
	var value time.Time
	if errTransaction := repo.db.Transaction(func(tx *gorm.DB) error {
		var errNow error
		value, errNow = DatabaseNow(context.Background(), tx)
		return errNow
	}); errTransaction != nil {
		t.Fatalf("DatabaseNow(): %v", errTransaction)
	}
	return value
}

func makeInFlightSnapshotFresh(t *testing.T, repo *Repository) {
	t.Helper()
	makeInFlightSnapshotUpdatedAt(t, repo, databaseNowForInFlightTest(t, repo))
}

func makeInFlightSnapshotUpdatedAt(t *testing.T, repo *Repository, updatedAt time.Time) {
	t.Helper()
	if errUpdate := repo.db.Model(&CPAInFlightSnapshotRecord{}).Where("1 = 1").Update("updated_at", updatedAt.UTC()).Error; errUpdate != nil {
		t.Fatalf("update snapshot time: %v", errUpdate)
	}
}
