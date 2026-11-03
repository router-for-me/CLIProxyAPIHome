package cluster

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestInFlightProgressReadsPayloadsOnlyAtCompletion(t *testing.T) {
	for _, partCount := range []int{1, 4} {
		t.Run(strconv.Itoa(partCount), func(t *testing.T) {
			repo, identity := newActiveInFlightTestRepository(t)
			var payloadReads atomic.Int64
			if errRegister := repo.db.Callback().Query().After("gorm:query").Register("test:in-flight-payload-reads", func(tx *gorm.DB) {
				if _, allParts := tx.Statement.Dest.(*[]CPAInFlightSnapshotPartRecord); allParts && !tx.DryRun {
					payloadReads.Add(1)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			var expectedBytes int64
			for index := partCount - 1; index >= 0; index-- {
				raw := mustInFlightJSON(t, inFlightPart(1, index, partCount, nil, nil))
				expectedBytes += int64(len(raw))
				result, errIngest := repo.IngestInFlightFrame(t.Context(), identity, raw, DefaultInFlightLimits())
				if errIngest != nil || !result.Accepted || result.Published != (index == 0) {
					t.Fatalf("part %d result = %#v, error = %v", index, result, errIngest)
				}
				attempt := loadInFlightAttempt(t, repo)
				if attempt.ReceivedPartCount != partCount-index || attempt.EncodedBytes != expectedBytes {
					t.Fatalf("part %d progress = %#v, want %d parts and %d bytes", index, attempt, partCount-index, expectedBytes)
				}
				if index > 0 && payloadReads.Load() != 0 {
					t.Fatal("incomplete revision reread staged payloads")
				}
				duplicate, errDuplicate := repo.IngestInFlightFrame(t.Context(), identity, raw, DefaultInFlightLimits())
				if errDuplicate != nil || !duplicate.Accepted || duplicate.Published {
					t.Fatalf("duplicate result = %#v, error = %v", duplicate, errDuplicate)
				}
				if afterDuplicate := loadInFlightAttempt(t, repo); afterDuplicate.ReceivedPartCount != attempt.ReceivedPartCount || afterDuplicate.EncodedBytes != attempt.EncodedBytes {
					t.Fatal("duplicate changed progress")
				}
			}
			wantReads := int64(0)
			if partCount > 1 {
				wantReads = 1
			}
			if got := payloadReads.Load(); got != wantReads {
				t.Fatalf("payload reads = %d, want %d", got, wantReads)
			}
		})
	}
}

func TestPostgresConcurrentFirstInFlightFrames(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		name := "different-parts"
		if duplicate {
			name = "duplicate-part"
		}
		t.Run(name, func(t *testing.T) {
			pgRepo := newPostgresQuiescenceRepository(t)
			repo, identity := newActiveInFlightTestRepositoryWithDatabase(t, pgRepo.db)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			arrived := make(chan struct{}, 2)
			proceed := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(proceed) }) }
			t.Cleanup(release)
			if errRegister := repo.db.Callback().Query().After("gorm:query").Register("test:concurrent-first-attempt", func(tx *gorm.DB) {
				if tx.Statement.Table != (CPAInFlightSnapshotAttemptRecord{}).TableName() || !errors.Is(tx.Error, gorm.ErrRecordNotFound) {
					return
				}
				arrived <- struct{}{}
				select {
				case <-proceed:
				case <-ctx.Done():
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			type outcome struct {
				result InFlightIngestResult
				err    error
			}
			done := make(chan outcome, 2)
			for index := range 2 {
				partIndex, partCount := index, 2
				if duplicate {
					partIndex, partCount = 0, 1
				}
				raw := mustInFlightJSON(t, inFlightPart(1, partIndex, partCount, nil, nil))
				go func() {
					result, errIngest := repo.IngestInFlightFrame(ctx, identity, raw, DefaultInFlightLimits())
					done <- outcome{result: result, err: errIngest}
				}()
			}
			for range 2 {
				select {
				case <-arrived:
				case <-ctx.Done():
					t.Fatal("first frames did not reach creation concurrently:", ctx.Err())
				}
			}
			release()
			published := 0
			for range 2 {
				select {
				case result := <-done:
					if result.err != nil || !result.result.Accepted {
						t.Fatalf("ingest = %#v, error = %v", result.result, result.err)
					}
					if result.result.Published {
						published++
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if published != 1 || loadInFlightAttempt(t, repo).State != "complete" {
				t.Fatalf("published %d revisions, want exactly one complete revision", published)
			}
		})
	}
}

func TestPostgresInFlightWriteAllowsHeartbeatsAndAdmission(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	identity := InFlightIngestIdentity{
		CertificateFingerprint: lifetime.Fingerprint, NodeID: "node-a", MembershipConnectedAt: lifetime.ConnectedAt,
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	if errRegister := repo.db.Callback().Create().After("gorm:create").Register("test:hold-snapshot-part", func(tx *gorm.DB) {
		if tx.Statement.Table != (CPAInFlightSnapshotPartRecord{}).TableName() {
			return
		}
		close(locked)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	raw := mustInFlightJSON(t, inFlightPart(1, 0, 1, nil, nil))
	done := make(chan error, 1)
	go func() {
		_, errIngest := repo.IngestInFlightFrame(ctx, identity, raw, DefaultInFlightLimits())
		done <- errIngest
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if errHeartbeat := repo.RefreshCPALiveness(ctx, lifetime); errHeartbeat != nil {
		t.Fatal("CPA heartbeat blocked by snapshot:", errHeartbeat)
	}
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, lifetime.Home); errHeartbeat != nil {
		t.Fatal("Home heartbeat blocked by snapshot:", errHeartbeat)
	}
	if _, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
		CredentialID: "cred-a", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1,
	}); errAdmit != nil {
		t.Fatal("admission blocked by snapshot:", errAdmit)
	}
	finish()
	select {
	case errIngest := <-done:
		if errIngest != nil {
			t.Fatal(errIngest)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
