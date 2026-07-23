package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestIngestInFlightFramePublishesOnlyCompleteRevision(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	first := inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil)
	result, errFirst := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, first), DefaultInFlightLimits())
	if errFirst != nil || result.Published {
		t.Fatalf("first result = %#v error = %v", result, errFirst)
	}
	if count := visibleInFlightSnapshotCount(t, repo); count != 0 {
		t.Fatalf("visible count = %d, want 0", count)
	}

	second := inFlightPart(2, 1, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightUnaccounted, Count: 2}}, nil)
	result, errSecond := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, second), DefaultInFlightLimits())
	if errSecond != nil || !result.Published {
		t.Fatalf("second result = %#v error = %v", result, errSecond)
	}
	snapshot := loadVisibleInFlightSnapshot(t, repo)
	if snapshot.Revision != 2 || len(snapshot.Payload.Aggregates) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestIngestInFlightFrameEmptyPartClearsVisibleSnapshot(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightUnaccounted, Count: 1}})

	empty := inFlightPart(2, 0, 1, []InFlightAggregate{}, []InFlightRequestDetail{})
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, empty), DefaultInFlightLimits())
	if errIngest != nil || !result.Published {
		t.Fatalf("result = %#v error = %v", result, errIngest)
	}
	snapshot := loadVisibleInFlightSnapshot(t, repo)
	if len(snapshot.Payload.Aggregates) != 0 || len(snapshot.Payload.Details) != 0 {
		t.Fatalf("payload = %#v", snapshot.Payload)
	}
}

func TestIngestInFlightFrameDuplicateRawPartIsIdempotent(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	raw := []byte(`{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":2,"aggregates":[],"details":[]}`)

	first, errFirst := repo.IngestInFlightFrame(context.Background(), identity, raw, DefaultInFlightLimits())
	if errFirst != nil || !first.Accepted || first.Published {
		t.Fatalf("first = %#v error = %v", first, errFirst)
	}
	duplicate, errDuplicate := repo.IngestInFlightFrame(context.Background(), identity, raw, DefaultInFlightLimits())
	if errDuplicate != nil || !duplicate.Accepted || duplicate.Published {
		t.Fatalf("duplicate = %#v error = %v", duplicate, errDuplicate)
	}

	var part CPAInFlightSnapshotPartRecord
	if errPart := repo.db.First(&part).Error; errPart != nil {
		t.Fatalf("load part error = %v", errPart)
	}
	if !bytes.Equal(part.Payload, raw) {
		t.Fatalf("stored raw payload = %q, want %q", part.Payload, raw)
	}
}

func TestIngestInFlightFrameRejectsConflictingDuplicatePart(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	first := mustInFlightJSON(t, inFlightPart(2, 0, 2, []InFlightAggregate{}, nil))
	if _, errFirst := repo.IngestInFlightFrame(context.Background(), identity, first, DefaultInFlightLimits()); errFirst != nil {
		t.Fatalf("first ingest error = %v", errFirst)
	}
	conflicting := mustInFlightJSON(t, inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil))
	result, errConflict := repo.IngestInFlightFrame(context.Background(), identity, conflicting, DefaultInFlightLimits())
	if !errors.Is(errConflict, ErrInFlightRevisionConflict) || result.State != "rejected" {
		t.Fatalf("result = %#v error = %v, want rejected conflict", result, errConflict)
	}

	var partCount int64
	if errCount := repo.db.Model(&CPAInFlightSnapshotPartRecord{}).Count(&partCount).Error; errCount != nil {
		t.Fatalf("count parts error = %v", errCount)
	}
	if partCount != 0 {
		t.Fatalf("part count = %d, want 0", partCount)
	}
}

func TestInFlightHigherRevisionReplacesIncompleteAndAdvancesWatermark(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	oldPart := inFlightPart(3, 0, 2, []InFlightAggregate{{CredentialID: "old", Model: "m", Status: InFlightUnaccounted, Count: 1}}, nil)
	if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, oldPart), DefaultInFlightLimits()); errIngest != nil {
		t.Fatalf("old ingest error = %v", errIngest)
	}
	next := inFlightPart(4, 0, 1, []InFlightAggregate{{CredentialID: "new", Model: "m", Status: InFlightAccounted, Count: 1}}, nil)
	if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, next), DefaultInFlightLimits()); errIngest != nil {
		t.Fatalf("new ingest error = %v", errIngest)
	}
	if _, errOld := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, oldPart), DefaultInFlightLimits()); !errors.Is(errOld, ErrInFlightRevisionStale) {
		t.Fatalf("old retry error = %v", errOld)
	}
	attempt := loadInFlightAttempt(t, repo)
	if attempt.HighestSeenRevision != 4 || attempt.State != "complete" {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func TestInFlightCPACompatibleOverflowCountOneAdvancesWatermarkAndKeepsVisibleSnapshot(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 4, []InFlightAggregate{{CredentialID: "visible", Model: "m", Status: InFlightUnaccounted, Count: 1}})
	overflow := `{"kind":"overflow","revision":5,"observed_at":"2026-07-21T12:00:05Z","barrier_revision":2,"aggregate_group_count":1}`
	result, errOverflow := repo.IngestInFlightFrame(context.Background(), identity, []byte(overflow), DefaultInFlightLimits())
	if errOverflow != nil || result.State != "overflow" || result.Published {
		t.Fatalf("overflow result = %#v error = %v", result, errOverflow)
	}
	if got := loadVisibleInFlightSnapshot(t, repo); got.Revision != 4 {
		t.Fatalf("visible revision = %d, want 4", got.Revision)
	}
	if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 5 || attempt.State != "overflow" || attempt.AggregateGroupCount != 1 {
		t.Fatalf("attempt = %#v", attempt)
	}
	stale := inFlightPart(4, 0, 1, nil, nil)
	if _, errStale := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, stale), DefaultInFlightLimits()); !errors.Is(errStale, ErrInFlightRevisionStale) {
		t.Fatalf("stale error = %v", errStale)
	}
}

func TestInFlightRejectsInvalidFramesAndAdvancesWatermark(t *testing.T) {
	validPart := `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[]}`
	longString := strings.Repeat("a", 257)
	tests := []struct {
		name string
		raw  string
		err  error
	}{
		{name: "duplicate top-level key", raw: `{"kind":"part","kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "unknown top-level key", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[],"fingerprint":"leak"}`, err: ErrInFlightFrameInvalid},
		{name: "missing part count", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"aggregates":[],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "unknown aggregate key", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[{"credential_id":"cred","model":"m","status":"accounted","count":1,"token":"secret"}],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "duplicate aggregate key", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[{"credential_id":"cred","credential_id":"other","model":"m","status":"accounted","count":1}],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "missing aggregate count", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[{"credential_id":"cred","model":"m","status":"accounted"}],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "negative count", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[{"credential_id":"cred","model":"m","status":"accounted","count":-1}],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "long string", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[{"credential_id":"` + longString + `","model":"m","status":"accounted","count":1}],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "part count limit", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":65,"aggregates":[],"details":[]}`, err: ErrInFlightFrameInvalid},
		{name: "overflow part field", raw: `{"kind":"overflow","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"aggregate_group_count":100001,"part_index":0}`, err: ErrInFlightFrameInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, identity := newActiveInFlightTestRepository(t)
			publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "visible", Model: "m", Status: InFlightAccounted, Count: 1}})
			result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, []byte(test.raw), DefaultInFlightLimits())
			if !errors.Is(errIngest, test.err) || result.State != "rejected" {
				t.Fatalf("result = %#v error = %v", result, errIngest)
			}
			if snapshot := loadVisibleInFlightSnapshot(t, repo); snapshot.Revision != 1 {
				t.Fatalf("visible revision = %d, want 1", snapshot.Revision)
			}
			if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
				t.Fatalf("attempt = %#v", attempt)
			}
			if _, errStale := repo.IngestInFlightFrame(context.Background(), identity, []byte(validPart), DefaultInFlightLimits()); !errors.Is(errStale, ErrInFlightRevisionStale) {
				t.Fatalf("same revision retry error = %v", errStale)
			}
		})
	}
}

func TestInFlightMaxDetailsZeroRejectsDetailsAndPreservesVisibleSnapshot(t *testing.T) {
	detail := InFlightRequestDetail{
		RequestID:    "request",
		CredentialID: "cred",
		Model:        "m",
		RequestKind:  "chat",
		StartedAt:    time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name   string
		frames []InFlightSnapshotFrame
	}{
		{
			name: "single part",
			frames: []InFlightSnapshotFrame{
				inFlightPart(2, 0, 1, nil, []InFlightRequestDetail{detail}),
			},
		},
		{
			name: "multipart",
			frames: []InFlightSnapshotFrame{
				inFlightPart(2, 0, 2, nil, nil),
				inFlightPart(2, 1, 2, nil, []InFlightRequestDetail{detail}),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, identity := newActiveInFlightTestRepository(t)
			publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "visible", Model: "m", Status: InFlightAccounted, Count: 1}})
			limits := DefaultInFlightLimits()
			limits.MaxDetails = 0

			for index, frame := range test.frames {
				result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, frame), limits)
				if index < len(test.frames)-1 {
					if errIngest != nil || result.State != "staging" {
						t.Fatalf("part %d result = %#v error = %v", index, result, errIngest)
					}
					continue
				}
				if !errors.Is(errIngest, ErrInFlightFrameInvalid) || result.State != "rejected" {
					t.Fatalf("part %d result = %#v error = %v", index, result, errIngest)
				}
			}

			if count := stagedInFlightPartCount(t, repo); count != 0 {
				t.Fatalf("staged part count = %d, want 0", count)
			}
			if snapshot := loadVisibleInFlightSnapshot(t, repo); snapshot.Revision != 1 {
				t.Fatalf("visible revision = %d, want 1", snapshot.Revision)
			}
			if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
				t.Fatalf("attempt = %#v", attempt)
			}
			if _, errStale := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, inFlightPart(2, 0, 1, nil, nil)), limits); !errors.Is(errStale, ErrInFlightRevisionStale) {
				t.Fatalf("same revision retry error = %v", errStale)
			}
		})
	}
}

func TestInFlightRejectsInvalidCompletePayloadAndCleansUp(t *testing.T) {
	tests := []struct {
		name   string
		first  InFlightSnapshotFrame
		second InFlightSnapshotFrame
		err    error
	}{
		{
			name:   "duplicate aggregate identity",
			first:  inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil),
			second: inFlightPart(2, 1, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil),
			err:    ErrInFlightFrameInvalid,
		},
		{
			name:   "checked aggregate sum overflow",
			first:  inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: math.MaxInt64}}, nil),
			second: inFlightPart(2, 1, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightUnaccounted, Count: 1}}, nil),
			err:    ErrInFlightRevisionOverflow,
		},
		{
			name:  "common metadata conflict",
			first: inFlightPart(2, 0, 2, nil, nil),
			second: func() InFlightSnapshotFrame {
				frame := inFlightPart(2, 1, 2, nil, nil)
				frame.BarrierRevision = 2
				return frame
			}(),
			err: ErrInFlightFrameInvalid,
		},
		{
			name:   "part count conflict before completion",
			first:  inFlightPart(2, 0, 2, nil, nil),
			second: inFlightPart(2, 2, 3, nil, nil),
			err:    ErrInFlightFrameInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, identity := newActiveInFlightTestRepository(t)
			publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "visible", Model: "m", Status: InFlightAccounted, Count: 1}})
			if _, errFirst := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, test.first), DefaultInFlightLimits()); errFirst != nil {
				t.Fatalf("first ingest error = %v", errFirst)
			}
			result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, test.second), DefaultInFlightLimits())
			if !errors.Is(errIngest, test.err) || result.State != "rejected" {
				t.Fatalf("result = %#v error = %v", result, errIngest)
			}
			if count := stagedInFlightPartCount(t, repo); count != 0 {
				t.Fatalf("staged parts = %d, want 0", count)
			}
			if snapshot := loadVisibleInFlightSnapshot(t, repo); snapshot.Revision != 1 {
				t.Fatalf("visible revision = %d, want 1", snapshot.Revision)
			}
			if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
				t.Fatalf("attempt = %#v", attempt)
			}
		})
	}
}

func TestInFlightPartByteLimitRejectsAndAdvancesWatermark(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	limits := DefaultInFlightLimits()
	limits.MaxStringBytes = limits.MaxPartBytes + 1
	frame := inFlightPart(2, 0, 1, []InFlightAggregate{{CredentialID: strings.Repeat("a", limits.MaxPartBytes), Model: "m", Status: InFlightAccounted, Count: 1}}, nil)
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, frame), limits)
	if !errors.Is(errIngest, ErrInFlightFrameInvalid) || result.State != "rejected" {
		t.Fatalf("result = %#v error = %v", result, errIngest)
	}
	if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func TestValidateCompleteInFlightPayloadRejectsAggregateLimit(t *testing.T) {
	payload := InFlightSnapshotPayload{Aggregates: make([]InFlightAggregate, DefaultInFlightLimits().MaxAggregateGroups+1)}
	for index := range payload.Aggregates {
		payload.Aggregates[index] = InFlightAggregate{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}
	}
	if errValidate := validateCompleteInFlightPayload(payload, DefaultInFlightLimits()); !errors.Is(errValidate, ErrInFlightFrameInvalid) {
		t.Fatalf("validate error = %v", errValidate)
	}
}

func TestInFlightRevisionByteOverflowRejectsAndAdvancesWatermark(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	limits := DefaultInFlightLimits()
	limits.MaxRevisionBytes = 100
	frame := inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil)
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, frame), limits)
	if !errors.Is(errIngest, ErrInFlightRevisionOverflow) || result.State != "rejected" {
		t.Fatalf("result = %#v error = %v", result, errIngest)
	}
	if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func TestIngestInFlightFrameReplacesVisibleSnapshotOnlyWhenNewRevisionCompletes(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "old", Model: "m", Status: InFlightAccounted, Count: 1}})

	if _, errFirst := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, inFlightPart(2, 0, 2, nil, nil)), DefaultInFlightLimits()); errFirst != nil {
		t.Fatalf("first part error = %v", errFirst)
	}
	if snapshot := loadVisibleInFlightSnapshot(t, repo); snapshot.Revision != 1 {
		t.Fatalf("visible revision = %d, want 1", snapshot.Revision)
	}
	if _, errSecond := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, inFlightPart(2, 1, 2, nil, nil)), DefaultInFlightLimits()); errSecond != nil {
		t.Fatalf("second part error = %v", errSecond)
	}
	if snapshot := loadVisibleInFlightSnapshot(t, repo); snapshot.Revision != 2 {
		t.Fatalf("visible revision = %d, want 2", snapshot.Revision)
	}
}

func TestInFlightDatabaseFailureAfterPartDeleteRollsBackWatermark(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	oldRaw := mustInFlightJSON(t, inFlightPart(2, 0, 2, nil, nil))
	if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, oldRaw, DefaultInFlightLimits()); errIngest != nil {
		t.Fatalf("old ingest error = %v", errIngest)
	}

	injectedErr := errors.New("injected delete failure")
	callbackName := "test:in-flight-fail-after-part-delete"
	if errRegister := repo.db.Callback().Delete().After("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == (CPAInFlightSnapshotPartRecord{}).TableName() {
			tx.AddError(injectedErr)
		}
	}); errRegister != nil {
		t.Fatalf("register delete callback error = %v", errRegister)
	}
	t.Cleanup(func() {
		if errRemove := repo.db.Callback().Delete().Remove(callbackName); errRemove != nil {
			t.Errorf("remove delete callback error = %v", errRemove)
		}
	})

	invalidRaw := []byte(`{"kind":"part","revision":3,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":2,"aggregates":[],"details":[],"fingerprint":"untrusted"}`)
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, invalidRaw, DefaultInFlightLimits())
	if !errors.Is(errIngest, injectedErr) || result != (InFlightIngestResult{}) {
		t.Fatalf("result = %#v error = %v, want database failure", result, errIngest)
	}
	if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "staging" {
		t.Fatalf("attempt = %#v, want unchanged staging revision", attempt)
	}
	var part CPAInFlightSnapshotPartRecord
	if errPart := repo.db.First(&part).Error; errPart != nil {
		t.Fatalf("load rolled-back part error = %v", errPart)
	}
	if !bytes.Equal(part.Payload, oldRaw) {
		t.Fatalf("part payload = %q, want %q", part.Payload, oldRaw)
	}
}

func TestValidateCompleteInFlightPayloadAllowsNULSeparatedDistinctKeys(t *testing.T) {
	payload := InFlightSnapshotPayload{Aggregates: []InFlightAggregate{
		{CredentialID: "a\x00b", Model: "c", Status: InFlightAccounted, Count: 1},
		{CredentialID: "a", Model: "b\x00c", Status: InFlightAccounted, Count: 1},
	}}
	if errValidate := validateCompleteInFlightPayload(payload, DefaultInFlightLimits()); errValidate != nil {
		t.Fatalf("validateCompleteInFlightPayload() error = %v", errValidate)
	}
}

func TestInFlightOverflowConfiguredByteLimitRejectsAndAdvancesWatermark(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	publishInFlightSnapshot(t, repo, identity, 1, []InFlightAggregate{{CredentialID: "visible", Model: "m", Status: InFlightAccounted, Count: 1}})
	base := []byte(`{"kind":"overflow","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"aggregate_group_count":100001}`)
	raw := append(append([]byte(nil), base...), bytes.Repeat([]byte(" "), 64)...)
	limits := DefaultInFlightLimits()
	limits.MaxRevisionBytes = len(base) + 63

	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, raw, limits)
	if !errors.Is(errIngest, ErrInFlightRevisionOverflow) || result.State != "rejected" {
		t.Fatalf("result = %#v error = %v", result, errIngest)
	}
	if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
		t.Fatalf("attempt = %#v", attempt)
	}
	if snapshot := loadVisibleInFlightSnapshot(t, repo); snapshot.Revision != 1 {
		t.Fatalf("visible revision = %d, want 1", snapshot.Revision)
	}
}

func TestInFlightCompleteReplayRequiresExactRawParts(t *testing.T) {
	repo, identity := newActiveInFlightTestRepository(t)
	firstRaw := mustInFlightJSON(t, inFlightPart(2, 0, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightAccounted, Count: 1}}, nil))
	secondRaw := mustInFlightJSON(t, inFlightPart(2, 1, 2, []InFlightAggregate{{CredentialID: "cred", Model: "m", Status: InFlightUnaccounted, Count: 1}}, nil))
	if _, errIngest := repo.IngestInFlightFrame(context.Background(), identity, firstRaw, DefaultInFlightLimits()); errIngest != nil {
		t.Fatalf("first ingest error = %v", errIngest)
	}
	if result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, secondRaw, DefaultInFlightLimits()); errIngest != nil || !result.Published {
		t.Fatalf("complete result = %#v error = %v", result, errIngest)
	}
	for _, raw := range [][]byte{firstRaw, secondRaw} {
		result, errReplay := repo.IngestInFlightFrame(context.Background(), identity, raw, DefaultInFlightLimits())
		if errReplay != nil || !result.Accepted || result.Published || result.State != "complete" {
			t.Fatalf("replay result = %#v error = %v", result, errReplay)
		}
	}
	conflictingRaw := bytes.Replace(firstRaw, []byte(`"kind":"part"`), []byte(`"kind": "part"`), 1)
	result, errConflict := repo.IngestInFlightFrame(context.Background(), identity, conflictingRaw, DefaultInFlightLimits())
	if !errors.Is(errConflict, ErrInFlightRevisionConflict) || result.State != "complete" {
		t.Fatalf("conflict result = %#v error = %v", result, errConflict)
	}
	if attempt := loadInFlightAttempt(t, repo); attempt.State != "complete" || attempt.HighestSeenRevision != 2 {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func TestInFlightDetailStrictJSONRejectsInvalidDetails(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "unknown sensitive field", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[{"request_id":"request","credential_id":"cred","model":"m","request_kind":"http","started_at":"2026-07-21T12:00:00Z","header":"secret"}]}`},
		{name: "duplicate request id", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[{"request_id":"request","request_id":"other","credential_id":"cred","model":"m","request_kind":"http","started_at":"2026-07-21T12:00:00Z"}]}`},
		{name: "missing credential id", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[{"request_id":"request","model":"m","request_kind":"http","started_at":"2026-07-21T12:00:00Z"}]}`},
		{name: "missing model", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[{"request_id":"request","credential_id":"cred","request_kind":"http","started_at":"2026-07-21T12:00:00Z"}]}`},
		{name: "missing request kind", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[{"request_id":"request","credential_id":"cred","model":"m","started_at":"2026-07-21T12:00:00Z"}]}`},
		{name: "missing started at", raw: `{"kind":"part","revision":2,"observed_at":"2026-07-21T12:00:00Z","barrier_revision":1,"part_index":0,"part_count":1,"aggregates":[],"details":[{"request_id":"request","credential_id":"cred","model":"m","request_kind":"http"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, identity := newActiveInFlightTestRepository(t)
			result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, []byte(test.raw), DefaultInFlightLimits())
			if !errors.Is(errIngest, ErrInFlightFrameInvalid) || result.State != "rejected" {
				t.Fatalf("result = %#v error = %v", result, errIngest)
			}
			if attempt := loadInFlightAttempt(t, repo); attempt.HighestSeenRevision != 2 || attempt.State != "rejected" {
				t.Fatalf("attempt = %#v", attempt)
			}
		})
	}
}

func newActiveInFlightTestRepository(t *testing.T) (*Repository, InFlightIngestIdentity) {
	t.Helper()
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return newActiveInFlightTestRepositoryWithDatabase(t, db)
}

func newActiveInFlightTestRepositoryWithDatabase(t *testing.T, db *gorm.DB) (*Repository, InFlightIngestIdentity) {
	t.Helper()
	connectedAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	member := CPANodeMembershipRecord{
		CertificateFingerprint: "fingerprint",
		NodeID:                 "node-a",
		ProtocolVersion:        1,
		State:                  MembershipStateActive,
		ConnectedAt:            connectedAt,
	}
	if errCreate := db.Create(&member).Error; errCreate != nil {
		t.Fatalf("create membership error = %v", errCreate)
	}
	return NewRepository(db), InFlightIngestIdentity{
		CertificateFingerprint: member.CertificateFingerprint,
		NodeID:                 member.NodeID,
		MembershipConnectedAt:  member.ConnectedAt,
	}
}

func inFlightPart(revision int64, partIndex, partCount int, aggregates []InFlightAggregate, details []InFlightRequestDetail) InFlightSnapshotFrame {
	return InFlightSnapshotFrame{
		Kind:            InFlightFramePart,
		Revision:        revision,
		ObservedAt:      time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
		BarrierRevision: 1,
		PartIndex:       &partIndex,
		PartCount:       &partCount,
		Aggregates:      aggregates,
		Details:         details,
	}
}

func mustInFlightJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	return raw
}

func publishInFlightSnapshot(t *testing.T, repo *Repository, identity InFlightIngestIdentity, revision int64, aggregates []InFlightAggregate) {
	t.Helper()
	result, errIngest := repo.IngestInFlightFrame(context.Background(), identity, mustInFlightJSON(t, inFlightPart(revision, 0, 1, aggregates, nil)), DefaultInFlightLimits())
	if errIngest != nil || !result.Published {
		t.Fatalf("publish result = %#v error = %v", result, errIngest)
	}
}

func loadInFlightAttempt(t *testing.T, repo *Repository) CPAInFlightSnapshotAttemptRecord {
	t.Helper()
	var attempt CPAInFlightSnapshotAttemptRecord
	if errFirst := repo.db.First(&attempt).Error; errFirst != nil {
		t.Fatalf("load attempt error = %v", errFirst)
	}
	return attempt
}

func stagedInFlightPartCount(t *testing.T, repo *Repository) int64 {
	t.Helper()
	var count int64
	if errCount := repo.db.Model(&CPAInFlightSnapshotPartRecord{}).Count(&count).Error; errCount != nil {
		t.Fatalf("count parts error = %v", errCount)
	}
	return count
}

func visibleInFlightSnapshotCount(t *testing.T, repo *Repository) int64 {
	t.Helper()
	var count int64
	if errCount := repo.db.Model(&CPAInFlightSnapshotRecord{}).Count(&count).Error; errCount != nil {
		t.Fatalf("count visible snapshots error = %v", errCount)
	}
	return count
}

type visibleInFlightSnapshot struct {
	Revision int64
	Payload  InFlightSnapshotPayload
}

func loadVisibleInFlightSnapshot(t *testing.T, repo *Repository) visibleInFlightSnapshot {
	t.Helper()
	var record CPAInFlightSnapshotRecord
	if errFirst := repo.db.First(&record).Error; errFirst != nil {
		t.Fatalf("load visible snapshot error = %v", errFirst)
	}
	var payload InFlightSnapshotPayload
	if errUnmarshal := json.Unmarshal(record.Payload, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal payload error = %v", errUnmarshal)
	}
	return visibleInFlightSnapshot{Revision: record.Revision, Payload: payload}
}
