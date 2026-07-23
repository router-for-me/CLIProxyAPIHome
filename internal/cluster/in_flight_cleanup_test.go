package cluster

import (
	"context"
	"testing"
	"time"
)

func TestCleanupInFlightLifetimeCannotDeleteNewLifetime(t *testing.T) {
	repo, oldIdentity := newActiveInFlightTestRepository(t)
	oldConnectedAt := oldIdentity.MembershipConnectedAt
	newIdentity := rotateInFlightMembership(t, repo, oldIdentity)
	publishInFlightSnapshot(t, repo, newIdentity, 1, []InFlightAggregate{{CredentialID: "new", Model: "m", Status: InFlightUnaccounted, Count: 1}})

	if errCleanup := repo.CleanupInFlightLifetime(context.Background(), oldIdentity.CertificateFingerprint, oldConnectedAt); errCleanup != nil {
		t.Fatalf("CleanupInFlightLifetime() error = %v", errCleanup)
	}
	snapshot := loadVisibleInFlightSnapshotRecord(t, repo)
	if !snapshot.MembershipConnectedAt.Equal(newIdentity.MembershipConnectedAt) {
		t.Fatalf("snapshot lifetime = %v, want %v", snapshot.MembershipConnectedAt, newIdentity.MembershipConnectedAt)
	}
}

func TestCleanupExpiredInFlightStagingKeepsVisibleSnapshot(t *testing.T) {
	repo, oldIdentity := newActiveInFlightTestRepository(t)
	stageExpiredInFlightPart(t, repo, oldIdentity, 2)
	newIdentity := rotateInFlightMembership(t, repo, oldIdentity)
	publishInFlightSnapshot(t, repo, newIdentity, 1, []InFlightAggregate{{CredentialID: "new", Model: "m", Status: InFlightUnaccounted, Count: 1}})

	if errCleanup := repo.CleanupExpiredInFlightStaging(context.Background(), time.Minute); errCleanup != nil {
		t.Fatalf("CleanupExpiredInFlightStaging() error = %v", errCleanup)
	}
	if got := loadVisibleInFlightSnapshot(t, repo); got.Revision != 1 {
		t.Fatalf("visible revision = %d, want 1", got.Revision)
	}
	if count := inFlightPartsForLifetime(t, repo, oldIdentity); count != 0 {
		t.Fatalf("expired staged parts = %d, want 0", count)
	}
	attempt := loadInFlightAttemptForLifetime(t, repo, oldIdentity)
	if attempt.State != "rejected" || attempt.ReceivedPartCount != 0 || attempt.EncodedBytes != 0 {
		t.Fatalf("expired attempt = %#v", attempt)
	}
}

func TestSweepRetainsActiveLifetimeWatermarkAndHighestRevisionParts(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	stageExpiredInFlightPart(t, repo, identity, 2)

	if errCleanup := repo.CleanupExpiredInFlightStaging(context.Background(), time.Minute); errCleanup != nil {
		t.Fatalf("CleanupExpiredInFlightStaging() error = %v", errCleanup)
	}
	if count := inFlightPartsForLifetime(t, repo, identity); count != 1 {
		t.Fatalf("active staged parts = %d, want 1", count)
	}
	attempt := loadInFlightAttemptForLifetime(t, repo, identity)
	if attempt.State != "staging" || attempt.HighestSeenRevision != 2 {
		t.Fatalf("active attempt = %#v", attempt)
	}
}

func TestSweepAndIngestSerializeOnAttempt(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	stageExpiredInFlightPart(t, repo, identity, 2)

	errSweep := make(chan error, 1)
	go func() {
		errSweep <- repo.CleanupExpiredInFlightStaging(context.Background(), time.Minute)
	}()
	next := inFlightPart(2, 1, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil)
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, next), DefaultInFlightLimits())
	if errIngest != nil || !result.Published {
		t.Fatalf("ingest result = %#v error = %v", result, errIngest)
	}
	if errCleanup := <-errSweep; errCleanup != nil {
		t.Fatalf("CleanupExpiredInFlightStaging() error = %v", errCleanup)
	}
	if attempt := loadInFlightAttemptForLifetime(t, repo, identity); attempt.State != "complete" || attempt.HighestSeenRevision != 2 {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func rotateInFlightMembership(t *testing.T, repo *Repository, identity InFlightIngestIdentity) InFlightIngestIdentity {
	t.Helper()
	connectedAt := identity.MembershipConnectedAt.Add(time.Second)
	if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).
		Where("certificate_fingerprint = ?", identity.CertificateFingerprint).
		Updates(map[string]any{"state": MembershipStateActive, "connected_at": connectedAt}).Error; errUpdate != nil {
		t.Fatalf("rotate membership error = %v", errUpdate)
	}
	identity.MembershipConnectedAt = connectedAt
	return identity
}

func stageExpiredInFlightPart(t *testing.T, repo *Repository, identity InFlightIngestIdentity, revision int64) {
	t.Helper()
	frame := inFlightPart(revision, 0, 2, []InFlightAggregate{{CredentialID: "staged", Model: "m", Status: InFlightUnaccounted, Count: 1}}, nil)
	if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, frame), DefaultInFlightLimits()); errIngest != nil {
		t.Fatalf("stage in-flight part error = %v", errIngest)
	}
	expiredAt := time.Now().UTC().Add(-2 * time.Minute)
	if errUpdate := repo.db.Model(&CPAInFlightSnapshotAttemptRecord{}).
		Where("certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).
		Update("updated_at", expiredAt).Error; errUpdate != nil {
		t.Fatalf("expire attempt error = %v", errUpdate)
	}
	if errUpdate := repo.db.Model(&CPAInFlightSnapshotPartRecord{}).
		Where("certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).
		Update("updated_at", expiredAt).Error; errUpdate != nil {
		t.Fatalf("expire part error = %v", errUpdate)
	}
}

func inFlightPartsForLifetime(t *testing.T, repo *Repository, identity InFlightIngestIdentity) int64 {
	t.Helper()
	var count int64
	if errCount := repo.db.Model(&CPAInFlightSnapshotPartRecord{}).
		Where("certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).
		Count(&count).Error; errCount != nil {
		t.Fatalf("count in-flight parts error = %v", errCount)
	}
	return count
}

func loadInFlightAttemptForLifetime(t *testing.T, repo *Repository, identity InFlightIngestIdentity) CPAInFlightSnapshotAttemptRecord {
	t.Helper()
	var attempt CPAInFlightSnapshotAttemptRecord
	if errFirst := repo.db.First(&attempt, "certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).Error; errFirst != nil {
		t.Fatalf("load in-flight attempt error = %v", errFirst)
	}
	return attempt
}

func loadVisibleInFlightSnapshotRecord(t *testing.T, repo *Repository) CPAInFlightSnapshotRecord {
	t.Helper()
	var record CPAInFlightSnapshotRecord
	if errFirst := repo.db.First(&record).Error; errFirst != nil {
		t.Fatalf("load visible snapshot record error = %v", errFirst)
	}
	return record
}
