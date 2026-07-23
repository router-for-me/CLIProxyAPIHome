package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const inFlightMaximumEnvelopeBytes = 16 * 1024 * 1024

type inFlightEnvelope struct {
	Revision int64
}

// RequireActiveCPAMembership locks and verifies the membership lifetime that owns an in-flight frame.
func (r *Repository) RequireActiveCPAMembership(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity) (CPANodeMembershipRecord, error) {
	if tx == nil {
		return CPANodeMembershipRecord{}, fmt.Errorf("database transaction is nil")
	}
	var member CPANodeMembershipRecord
	errMember := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("certificate_fingerprint = ? AND connected_at = ? AND state = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt, MembershipStateActive).
		First(&member).Error
	if errMember != nil || member.NodeID != identity.NodeID {
		return CPANodeMembershipRecord{}, ErrInFlightLifetimeMismatch
	}
	return member, nil
}

// IngestInFlightFrame stages a multipart in-flight snapshot and publishes it only after all parts arrive.
func (r *Repository) IngestInFlightFrame(ctx context.Context, identity InFlightIngestIdentity, raw []byte, limits InFlightLimits) (InFlightIngestResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return InFlightIngestResult{}, errDB
	}
	if ctx == nil {
		return InFlightIngestResult{}, fmt.Errorf("in-flight ingest context is nil")
	}

	envelope, errEnvelope := decodeInFlightEnvelope(raw)
	if errEnvelope != nil && envelope.Revision <= 0 {
		return InFlightIngestResult{}, errEnvelope
	}

	result := InFlightIngestResult{Revision: envelope.Revision}
	var protocolErr error
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, errMembership := r.RequireActiveCPAMembership(ctx, tx, identity); errMembership != nil {
			return errors.Join(ErrInFlightLifetimeMismatch, errMembership)
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		if errEnvelope != nil {
			errReject := r.markInFlightRejected(ctx, tx, identity, envelope.Revision, now, &result)
			if errReject != nil {
				if isInFlightProtocolError(errReject) {
					protocolErr = errReject
					return nil
				}
				return errReject
			}
			protocolErr = errEnvelope
			return nil
		}

		frame, errFrame := decodeInFlightFrameStrict(raw, limits)
		if errFrame != nil {
			errReject := r.markInFlightRejected(ctx, tx, identity, envelope.Revision, now, &result)
			if errReject != nil {
				if isInFlightProtocolError(errReject) {
					protocolErr = errReject
					return nil
				}
				return errReject
			}
			protocolErr = errFrame
			return nil
		}
		if errStage := r.stageInFlightFrame(ctx, tx, identity, frame, raw, now, limits, &result); errStage != nil {
			if isInFlightProtocolError(errStage) {
				protocolErr = errStage
				return nil
			}
			return errStage
		}
		return nil
	})
	if errTransaction != nil {
		return InFlightIngestResult{}, errTransaction
	}
	return result, protocolErr
}

func isInFlightProtocolError(err error) bool {
	return errors.Is(err, ErrInFlightFrameInvalid) ||
		errors.Is(err, ErrInFlightRevisionStale) ||
		errors.Is(err, ErrInFlightRevisionConflict) ||
		errors.Is(err, ErrInFlightRevisionOverflow)
}

func decodeInFlightEnvelope(raw []byte) (inFlightEnvelope, error) {
	if len(raw) == 0 || len(raw) > inFlightMaximumEnvelopeBytes {
		return inFlightEnvelope{}, ErrInFlightFrameInvalid
	}
	fields, errFields := decodeInFlightObject(raw, nil, nil)
	revision, errRevision := decodeInFlightInt64(fields["revision"])
	envelope := inFlightEnvelope{Revision: revision}
	if errRevision != nil || revision <= 0 {
		return inFlightEnvelope{}, ErrInFlightFrameInvalid
	}
	if errFields != nil {
		return envelope, ErrInFlightFrameInvalid
	}
	return envelope, nil
}

func decodeInFlightFrameStrict(raw []byte, limits InFlightLimits) (InFlightSnapshotFrame, error) {
	fields, errFields := decodeInFlightObject(raw, map[string]struct{}{
		"kind": {}, "revision": {}, "observed_at": {}, "barrier_revision": {},
		"part_index": {}, "part_count": {}, "details_truncated": {}, "aggregates": {}, "details": {}, "aggregate_group_count": {},
	}, []string{"kind", "revision", "observed_at", "barrier_revision"})
	if errFields != nil {
		return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
	}

	kind, errKind := decodeInFlightString(fields["kind"])
	revision, errRevision := decodeInFlightInt64(fields["revision"])
	observedAt, errObservedAt := decodeInFlightTime(fields["observed_at"])
	barrierRevision, errBarrierRevision := decodeInFlightInt64(fields["barrier_revision"])
	if errKind != nil || errRevision != nil || errObservedAt != nil || errBarrierRevision != nil {
		return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
	}
	frame := InFlightSnapshotFrame{
		Kind:            InFlightFrameKind(kind),
		Revision:        revision,
		ObservedAt:      observedAt,
		BarrierRevision: barrierRevision,
	}

	switch frame.Kind {
	case InFlightFramePart:
		if _, exists := fields["aggregate_group_count"]; exists {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		if _, exists := fields["part_index"]; !exists {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		if _, exists := fields["part_count"]; !exists {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		partIndex, errPartIndex := decodeInFlightInt(fields["part_index"])
		partCount, errPartCount := decodeInFlightInt(fields["part_count"])
		if errPartIndex != nil || errPartCount != nil {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		frame.PartIndex = &partIndex
		frame.PartCount = &partCount
		if rawTruncated, exists := fields["details_truncated"]; exists {
			detailsTruncated, errTruncated := decodeInFlightBool(rawTruncated)
			if errTruncated != nil {
				return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
			}
			frame.DetailsTruncated = detailsTruncated
		}
		aggregates, errAggregates := decodeInFlightAggregates(fields["aggregates"])
		details, errDetails := decodeInFlightDetails(fields["details"])
		if errAggregates != nil || errDetails != nil {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		frame.Aggregates = aggregates
		frame.Details = details
	case InFlightFrameOverflow:
		for _, field := range []string{"part_index", "part_count", "details_truncated", "aggregates", "details"} {
			if _, exists := fields[field]; exists {
				return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
			}
		}
		rawGroupCount, exists := fields["aggregate_group_count"]
		if !exists {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		groupCount, errGroupCount := decodeInFlightInt(rawGroupCount)
		if errGroupCount != nil {
			return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
		}
		frame.AggregateGroupCount = groupCount
	default:
		return InFlightSnapshotFrame{}, ErrInFlightFrameInvalid
	}

	if errValidate := validateInFlightFrame(frame, len(raw), limits); errValidate != nil {
		return InFlightSnapshotFrame{}, errValidate
	}
	return frame, nil
}

func decodeInFlightObject(raw []byte, allowed map[string]struct{}, required []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, errToken := decoder.Token()
	if errToken != nil {
		return nil, errToken
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, ErrInFlightFrameInvalid
	}
	fields := make(map[string]json.RawMessage)
	var firstErr error
	for decoder.More() {
		token, errToken = decoder.Token()
		if errToken != nil {
			return fields, errToken
		}
		key, ok := token.(string)
		if !ok {
			return fields, ErrInFlightFrameInvalid
		}
		var value json.RawMessage
		if errDecode := decoder.Decode(&value); errDecode != nil {
			return fields, errDecode
		}
		if _, exists := fields[key]; exists && firstErr == nil {
			firstErr = ErrInFlightFrameInvalid
		} else if !exists {
			fields[key] = value
		}
		if allowed != nil {
			if _, allowedKey := allowed[key]; !allowedKey && firstErr == nil {
				firstErr = ErrInFlightFrameInvalid
			}
		}
	}
	token, errToken = decoder.Token()
	if errToken != nil {
		return fields, errToken
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return fields, ErrInFlightFrameInvalid
	}
	if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
		return fields, ErrInFlightFrameInvalid
	}
	for _, key := range required {
		if _, exists := fields[key]; !exists && firstErr == nil {
			firstErr = ErrInFlightFrameInvalid
		}
	}
	return fields, firstErr
}

func decodeInFlightAggregates(raw json.RawMessage) ([]InFlightAggregate, error) {
	if raw == nil {
		return nil, nil
	}
	entries, errEntries := decodeInFlightArray(raw)
	if errEntries != nil {
		return nil, errEntries
	}
	aggregates := make([]InFlightAggregate, 0, len(entries))
	for _, entry := range entries {
		fields, errFields := decodeInFlightObject(entry, map[string]struct{}{"credential_id": {}, "model": {}, "status": {}, "count": {}}, []string{"credential_id", "model", "status", "count"})
		if errFields != nil {
			return nil, errFields
		}
		credentialID, errCredentialID := decodeInFlightString(fields["credential_id"])
		model, errModel := decodeInFlightString(fields["model"])
		status, errStatus := decodeInFlightString(fields["status"])
		count, errCount := decodeInFlightInt64(fields["count"])
		if errCredentialID != nil || errModel != nil || errStatus != nil || errCount != nil {
			return nil, ErrInFlightFrameInvalid
		}
		aggregates = append(aggregates, InFlightAggregate{CredentialID: credentialID, Model: model, Status: InFlightAccountedStatus(status), Count: count})
	}
	return aggregates, nil
}

func decodeInFlightDetails(raw json.RawMessage) ([]InFlightRequestDetail, error) {
	if raw == nil {
		return nil, nil
	}
	entries, errEntries := decodeInFlightArray(raw)
	if errEntries != nil {
		return nil, errEntries
	}
	details := make([]InFlightRequestDetail, 0, len(entries))
	for _, entry := range entries {
		fields, errFields := decodeInFlightObject(entry, map[string]struct{}{"request_id": {}, "credential_id": {}, "model": {}, "request_kind": {}, "started_at": {}}, []string{"request_id", "credential_id", "model", "request_kind", "started_at"})
		if errFields != nil {
			return nil, errFields
		}
		requestID, errRequestID := decodeInFlightString(fields["request_id"])
		credentialID, errCredentialID := decodeInFlightString(fields["credential_id"])
		model, errModel := decodeInFlightString(fields["model"])
		requestKind, errRequestKind := decodeInFlightString(fields["request_kind"])
		startedAt, errStartedAt := decodeInFlightTime(fields["started_at"])
		if errRequestID != nil || errCredentialID != nil || errModel != nil || errRequestKind != nil || errStartedAt != nil {
			return nil, ErrInFlightFrameInvalid
		}
		details = append(details, InFlightRequestDetail{RequestID: requestID, CredentialID: credentialID, Model: model, RequestKind: requestKind, StartedAt: startedAt})
	}
	return details, nil
}

func decodeInFlightArray(raw json.RawMessage) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, ErrInFlightFrameInvalid
	}
	var entries []json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &entries); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return entries, nil
}

func decodeInFlightString(raw json.RawMessage) (string, error) {
	var value string
	if raw == nil || json.Unmarshal(raw, &value) != nil {
		return "", ErrInFlightFrameInvalid
	}
	return value, nil
}

func decodeInFlightInt64(raw json.RawMessage) (int64, error) {
	var value int64
	if raw == nil || json.Unmarshal(raw, &value) != nil {
		return 0, ErrInFlightFrameInvalid
	}
	return value, nil
}

func decodeInFlightInt(raw json.RawMessage) (int, error) {
	value, errValue := decodeInFlightInt64(raw)
	if errValue != nil || value > int64(math.MaxInt) || value < int64(math.MinInt) {
		return 0, ErrInFlightFrameInvalid
	}
	return int(value), nil
}

func decodeInFlightBool(raw json.RawMessage) (bool, error) {
	var value bool
	if raw == nil || json.Unmarshal(raw, &value) != nil {
		return false, ErrInFlightFrameInvalid
	}
	return value, nil
}

func decodeInFlightTime(raw json.RawMessage) (time.Time, error) {
	var value time.Time
	if raw == nil || json.Unmarshal(raw, &value) != nil || value.IsZero() || value.Location() != time.UTC {
		return time.Time{}, ErrInFlightFrameInvalid
	}
	return value, nil
}

func validateInFlightFrame(frame InFlightSnapshotFrame, encodedBytes int, limits InFlightLimits) error {
	if frame.Revision <= 0 || frame.ObservedAt.IsZero() || frame.ObservedAt.Location() != time.UTC || frame.BarrierRevision < 0 {
		return ErrInFlightFrameInvalid
	}
	if limits.MaxRevisionBytes > 0 && encodedBytes > limits.MaxRevisionBytes {
		return ErrInFlightRevisionOverflow
	}
	switch frame.Kind {
	case InFlightFramePart:
		if frame.PartIndex == nil || frame.PartCount == nil {
			return ErrInFlightFrameInvalid
		}
		if limits.MaxPartBytes > 0 && encodedBytes > limits.MaxPartBytes {
			return ErrInFlightFrameInvalid
		}
		if *frame.PartCount <= 0 || *frame.PartIndex < 0 || *frame.PartIndex >= *frame.PartCount || (limits.MaxPartCount > 0 && *frame.PartCount > limits.MaxPartCount) {
			return ErrInFlightFrameInvalid
		}
		if (limits.MaxAggregateGroups > 0 && len(frame.Aggregates) > limits.MaxAggregateGroups) || len(frame.Details) > limits.MaxDetails {
			return ErrInFlightFrameInvalid
		}
		for _, aggregate := range frame.Aggregates {
			if !validInFlightString(aggregate.CredentialID, limits) || !validInFlightString(aggregate.Model, limits) || (aggregate.Status != InFlightAccounted && aggregate.Status != InFlightUnaccounted) || aggregate.Count < 0 {
				return ErrInFlightFrameInvalid
			}
		}
		for _, detail := range frame.Details {
			if !validInFlightString(detail.RequestID, limits) || !validInFlightString(detail.CredentialID, limits) || !validInFlightString(detail.Model, limits) || !validInFlightString(detail.RequestKind, limits) || detail.StartedAt.IsZero() || detail.StartedAt.Location() != time.UTC {
				return ErrInFlightFrameInvalid
			}
		}
	case InFlightFrameOverflow:
		if frame.AggregateGroupCount <= 0 {
			return ErrInFlightFrameInvalid
		}
	default:
		return ErrInFlightFrameInvalid
	}
	return nil
}

func validInFlightString(value string, limits InFlightLimits) bool {
	return strings.TrimSpace(value) != "" && (limits.MaxStringBytes <= 0 || len(value) <= limits.MaxStringBytes)
}

func (r *Repository) stageInFlightFrame(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity, frame InFlightSnapshotFrame, raw []byte, now time.Time, limits InFlightLimits, result *InFlightIngestResult) error {
	attempt, found, errAttempt := r.lockInFlightAttempt(ctx, tx, identity)
	if errAttempt != nil {
		return errAttempt
	}
	if !found {
		if frame.Kind == InFlightFrameOverflow {
			attempt = newInFlightAttempt(identity, frame, now)
			attempt.State = "overflow"
			attempt.AggregateGroupCount = frame.AggregateGroupCount
			if errCreate := tx.Create(&attempt).Error; errCreate != nil {
				return errCreate
			}
			result.Accepted = true
			result.State = "overflow"
			return nil
		}
		attempt = newInFlightAttempt(identity, frame, now)
		if errCreate := tx.Create(&attempt).Error; errCreate != nil {
			return errCreate
		}
		return r.stageInFlightPart(ctx, tx, identity, &attempt, frame, raw, now, limits, result)
	}

	if frame.Revision < attempt.HighestSeenRevision {
		result.State = attempt.State
		return ErrInFlightRevisionStale
	}
	if frame.Revision > attempt.HighestSeenRevision {
		if errDelete := deleteInFlightParts(ctx, tx, identity); errDelete != nil {
			return errDelete
		}
		attempt = newInFlightAttempt(identity, frame, now)
		if frame.Kind == InFlightFrameOverflow {
			attempt.State = "overflow"
			attempt.AggregateGroupCount = frame.AggregateGroupCount
		}
		if errSave := tx.Save(&attempt).Error; errSave != nil {
			return errSave
		}
		if frame.Kind == InFlightFrameOverflow {
			result.Accepted = true
			result.State = "overflow"
			return nil
		}
		return r.stageInFlightPart(ctx, tx, identity, &attempt, frame, raw, now, limits, result)
	}

	switch attempt.State {
	case "overflow", "rejected":
		result.State = attempt.State
		return ErrInFlightRevisionStale
	case "complete":
		if frame.Kind != InFlightFramePart {
			result.State = attempt.State
			return ErrInFlightRevisionConflict
		}
		return r.verifyPublishedInFlightReplay(ctx, tx, identity, frame, raw, result)
	case "staging":
		if frame.Kind == InFlightFrameOverflow {
			if errReject := rejectInFlightAttempt(ctx, tx, &attempt, identity, now); errReject != nil {
				return errReject
			}
			result.State = "rejected"
			return ErrInFlightRevisionConflict
		}
		return r.stageInFlightPart(ctx, tx, identity, &attempt, frame, raw, now, limits, result)
	default:
		return fmt.Errorf("unknown in-flight attempt state %q", attempt.State)
	}
}

func (r *Repository) stageInFlightPart(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity, attempt *CPAInFlightSnapshotAttemptRecord, frame InFlightSnapshotFrame, raw []byte, now time.Time, limits InFlightLimits, result *InFlightIngestResult) error {
	part := CPAInFlightSnapshotPartRecord{}
	errPart := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("certificate_fingerprint = ? AND membership_connected_at = ? AND revision = ? AND part_index = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt, frame.Revision, *frame.PartIndex).
		First(&part).Error
	if errPart == nil {
		if bytes.Equal(part.Payload, raw) {
			result.Accepted = true
			result.State = attempt.State
			return nil
		}
		if errReject := rejectInFlightAttempt(ctx, tx, attempt, identity, now); errReject != nil {
			return errReject
		}
		result.State = "rejected"
		return ErrInFlightRevisionConflict
	}
	if !errors.Is(errPart, gorm.ErrRecordNotFound) {
		return errPart
	}
	if !inFlightFrameMatchesAttempt(frame, *attempt) {
		if errReject := rejectInFlightAttempt(ctx, tx, attempt, identity, now); errReject != nil {
			return errReject
		}
		result.State = "rejected"
		return ErrInFlightFrameInvalid
	}

	part = CPAInFlightSnapshotPartRecord{
		CertificateFingerprint: identity.CertificateFingerprint,
		MembershipConnectedAt:  identity.MembershipConnectedAt,
		Revision:               frame.Revision,
		PartIndex:              *frame.PartIndex,
		Payload:                append([]byte(nil), raw...),
		EncodedBytes:           len(raw),
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	if errCreate := tx.Create(&part).Error; errCreate != nil {
		return errCreate
	}

	var parts []CPAInFlightSnapshotPartRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND revision = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt, frame.Revision).Order("part_index ASC").Find(&parts).Error; errFind != nil {
		return errFind
	}
	encodedBytes, errEncodedBytes := inFlightPartsEncodedBytes(parts)
	if errEncodedBytes != nil {
		return errEncodedBytes
	}
	attempt.ReceivedPartCount = len(parts)
	attempt.EncodedBytes = encodedBytes
	attempt.UpdatedAt = now
	if limits.MaxRevisionBytes > 0 && encodedBytes > int64(limits.MaxRevisionBytes) {
		if errReject := rejectInFlightAttempt(ctx, tx, attempt, identity, now); errReject != nil {
			return errReject
		}
		result.State = "rejected"
		return ErrInFlightRevisionOverflow
	}
	if errSave := tx.Save(attempt).Error; errSave != nil {
		return errSave
	}

	if len(parts) != attempt.PartCount {
		result.Accepted = true
		result.State = "staging"
		return nil
	}
	payload, errPayload := completeInFlightPayload(parts, frame, limits)
	if errPayload != nil {
		if errReject := rejectInFlightAttempt(ctx, tx, attempt, identity, now); errReject != nil {
			return errReject
		}
		result.State = "rejected"
		return errPayload
	}
	payloadJSON, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return errMarshal
	}
	visible := CPAInFlightSnapshotRecord{
		CertificateFingerprint: identity.CertificateFingerprint,
		NodeID:                 identity.NodeID,
		MembershipConnectedAt:  identity.MembershipConnectedAt,
		Revision:               frame.Revision,
		ObservedAt:             frame.ObservedAt.UTC(),
		BarrierRevision:        frame.BarrierRevision,
		DetailsTruncated:       frame.DetailsTruncated,
		Payload:                JSONB(payloadJSON),
		UpdatedAt:              now,
	}
	if errUpsert := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "certificate_fingerprint"}},
		DoUpdates: clause.AssignmentColumns([]string{"node_id", "membership_connected_at", "revision", "observed_at", "barrier_revision", "details_truncated", "payload", "updated_at"}),
	}).Create(&visible).Error; errUpsert != nil {
		return errUpsert
	}
	attempt.State = "complete"
	attempt.UpdatedAt = now
	if errSave := tx.Save(attempt).Error; errSave != nil {
		return errSave
	}
	result.Accepted = true
	result.Published = true
	result.State = "complete"
	return nil
}

func inFlightFrameMatchesAttempt(frame InFlightSnapshotFrame, attempt CPAInFlightSnapshotAttemptRecord) bool {
	return frame.PartCount != nil && *frame.PartCount == attempt.PartCount && frame.ObservedAt.Equal(attempt.ObservedAt) && frame.BarrierRevision == attempt.BarrierRevision && frame.DetailsTruncated == attempt.DetailsTruncated
}

func (r *Repository) verifyPublishedInFlightReplay(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity, frame InFlightSnapshotFrame, raw []byte, result *InFlightIngestResult) error {
	var part CPAInFlightSnapshotPartRecord
	errPart := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("certificate_fingerprint = ? AND membership_connected_at = ? AND revision = ? AND part_index = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt, frame.Revision, *frame.PartIndex).
		First(&part).Error
	if errors.Is(errPart, gorm.ErrRecordNotFound) || (errPart == nil && !bytes.Equal(part.Payload, raw)) {
		result.State = "complete"
		return ErrInFlightRevisionConflict
	}
	if errPart != nil {
		return errPart
	}
	result.Accepted = true
	result.State = "complete"
	return nil
}

func (r *Repository) markInFlightRejected(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity, revision int64, now time.Time, result *InFlightIngestResult) error {
	attempt, found, errAttempt := r.lockInFlightAttempt(ctx, tx, identity)
	if errAttempt != nil {
		return errAttempt
	}
	if !found {
		attempt = newRejectedInFlightAttempt(identity, revision, now)
		if errCreate := tx.Create(&attempt).Error; errCreate != nil {
			return errCreate
		}
		result.State = "rejected"
		return nil
	}
	if revision < attempt.HighestSeenRevision {
		result.State = attempt.State
		return ErrInFlightRevisionStale
	}
	if revision == attempt.HighestSeenRevision {
		switch attempt.State {
		case "staging":
			if errReject := rejectInFlightAttempt(ctx, tx, &attempt, identity, now); errReject != nil {
				return errReject
			}
			result.State = "rejected"
			return nil
		case "complete":
			result.State = "complete"
			return ErrInFlightRevisionConflict
		case "overflow", "rejected":
			result.State = attempt.State
			return ErrInFlightRevisionStale
		default:
			return fmt.Errorf("unknown in-flight attempt state %q", attempt.State)
		}
	}
	if errDelete := deleteInFlightParts(ctx, tx, identity); errDelete != nil {
		return errDelete
	}
	attempt = newRejectedInFlightAttempt(identity, revision, now)
	if errSave := tx.Save(&attempt).Error; errSave != nil {
		return errSave
	}
	result.State = "rejected"
	return nil
}

func (r *Repository) lockInFlightAttempt(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity) (CPAInFlightSnapshotAttemptRecord, bool, error) {
	attempt := CPAInFlightSnapshotAttemptRecord{}
	errAttempt := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).
		First(&attempt).Error
	if errors.Is(errAttempt, gorm.ErrRecordNotFound) {
		return CPAInFlightSnapshotAttemptRecord{}, false, nil
	}
	if errAttempt != nil {
		return CPAInFlightSnapshotAttemptRecord{}, false, errAttempt
	}
	return attempt, true, nil
}

func newInFlightAttempt(identity InFlightIngestIdentity, frame InFlightSnapshotFrame, now time.Time) CPAInFlightSnapshotAttemptRecord {
	partCount := 0
	if frame.PartCount != nil {
		partCount = *frame.PartCount
	}
	return CPAInFlightSnapshotAttemptRecord{
		CertificateFingerprint: identity.CertificateFingerprint,
		MembershipConnectedAt:  identity.MembershipConnectedAt,
		HighestSeenRevision:    frame.Revision,
		State:                  "staging",
		ObservedAt:             frame.ObservedAt.UTC(),
		BarrierRevision:        frame.BarrierRevision,
		PartCount:              partCount,
		DetailsTruncated:       frame.DetailsTruncated,
		UpdatedAt:              now,
	}
}

func newRejectedInFlightAttempt(identity InFlightIngestIdentity, revision int64, now time.Time) CPAInFlightSnapshotAttemptRecord {
	return CPAInFlightSnapshotAttemptRecord{
		CertificateFingerprint: identity.CertificateFingerprint,
		MembershipConnectedAt:  identity.MembershipConnectedAt,
		HighestSeenRevision:    revision,
		State:                  "rejected",
		ObservedAt:             now.UTC(),
		UpdatedAt:              now,
	}
}

func completeInFlightPayload(parts []CPAInFlightSnapshotPartRecord, first InFlightSnapshotFrame, limits InFlightLimits) (InFlightSnapshotPayload, error) {
	payload := InFlightSnapshotPayload{Aggregates: []InFlightAggregate{}, Details: []InFlightRequestDetail{}, DetailsTruncated: first.DetailsTruncated}
	if len(parts) != *first.PartCount {
		return payload, ErrInFlightFrameInvalid
	}
	encodedBytes, errEncodedBytes := inFlightPartsEncodedBytes(parts)
	if errEncodedBytes != nil {
		return payload, errEncodedBytes
	}
	for index, part := range parts {
		if part.PartIndex != index {
			return payload, ErrInFlightFrameInvalid
		}
		frame, errDecode := decodeInFlightFrameStrict(part.Payload, limits)
		if errDecode != nil || frame.Kind != InFlightFramePart || frame.Revision != first.Revision || frame.PartIndex == nil || *frame.PartIndex != index || frame.PartCount == nil || *frame.PartCount != *first.PartCount || !frame.ObservedAt.Equal(first.ObservedAt) || frame.BarrierRevision != first.BarrierRevision || frame.DetailsTruncated != first.DetailsTruncated {
			return payload, ErrInFlightFrameInvalid
		}
		payload.Aggregates = append(payload.Aggregates, frame.Aggregates...)
		payload.Details = append(payload.Details, frame.Details...)
	}
	if limits.MaxRevisionBytes > 0 && encodedBytes > int64(limits.MaxRevisionBytes) {
		return payload, ErrInFlightRevisionOverflow
	}
	if errValidate := validateCompleteInFlightPayload(payload, limits); errValidate != nil {
		return payload, errValidate
	}
	return payload, nil
}

type inFlightAggregateKey struct {
	CredentialID string
	Model        string
	Status       InFlightAccountedStatus
}

type inFlightCredentialModelKey struct {
	CredentialID string
	Model        string
}

func validateCompleteInFlightPayload(payload InFlightSnapshotPayload, limits InFlightLimits) error {
	if (limits.MaxAggregateGroups > 0 && len(payload.Aggregates) > limits.MaxAggregateGroups) || len(payload.Details) > limits.MaxDetails {
		return ErrInFlightFrameInvalid
	}
	aggregateKeys := make(map[inFlightAggregateKey]struct{}, len(payload.Aggregates))
	modelTotals := make(map[inFlightCredentialModelKey]int64, len(payload.Aggregates))
	var total int64
	for _, aggregate := range payload.Aggregates {
		if !validInFlightString(aggregate.CredentialID, limits) || !validInFlightString(aggregate.Model, limits) || (aggregate.Status != InFlightAccounted && aggregate.Status != InFlightUnaccounted) {
			return ErrInFlightFrameInvalid
		}
		key := inFlightAggregateKey{CredentialID: aggregate.CredentialID, Model: aggregate.Model, Status: aggregate.Status}
		if _, exists := aggregateKeys[key]; exists {
			return ErrInFlightFrameInvalid
		}
		aggregateKeys[key] = struct{}{}
		modelKey := inFlightCredentialModelKey{CredentialID: aggregate.CredentialID, Model: aggregate.Model}
		nextModelTotal, errModelTotal := checkedInFlightAdd(modelTotals[modelKey], aggregate.Count)
		if errModelTotal != nil {
			return errModelTotal
		}
		nextTotal, errTotal := checkedInFlightAdd(total, aggregate.Count)
		if errTotal != nil {
			return errTotal
		}
		modelTotals[modelKey] = nextModelTotal
		total = nextTotal
	}
	for _, detail := range payload.Details {
		if !validInFlightString(detail.RequestID, limits) || !validInFlightString(detail.CredentialID, limits) || !validInFlightString(detail.Model, limits) || !validInFlightString(detail.RequestKind, limits) || detail.StartedAt.IsZero() || detail.StartedAt.Location() != time.UTC {
			return ErrInFlightFrameInvalid
		}
	}
	return nil
}

func checkedInFlightAdd(left int64, right int64) (int64, error) {
	if right < 0 || left > math.MaxInt64-right {
		return 0, ErrInFlightRevisionOverflow
	}
	return left + right, nil
}

func inFlightPartsEncodedBytes(parts []CPAInFlightSnapshotPartRecord) (int64, error) {
	var encodedBytes int64
	for _, part := range parts {
		next, errNext := checkedInFlightAdd(encodedBytes, int64(part.EncodedBytes))
		if errNext != nil {
			return 0, errNext
		}
		encodedBytes = next
	}
	return encodedBytes, nil
}

func rejectInFlightAttempt(ctx context.Context, tx *gorm.DB, attempt *CPAInFlightSnapshotAttemptRecord, identity InFlightIngestIdentity, now time.Time) error {
	if attempt == nil {
		return fmt.Errorf("in-flight attempt is nil")
	}
	if errDelete := tx.WithContext(contextOrBackground(ctx)).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND revision = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt, attempt.HighestSeenRevision).Delete(&CPAInFlightSnapshotPartRecord{}).Error; errDelete != nil {
		return errDelete
	}
	attempt.State = "rejected"
	attempt.ReceivedPartCount = 0
	attempt.EncodedBytes = 0
	attempt.AggregateGroupCount = 0
	attempt.UpdatedAt = now
	return tx.Save(attempt).Error
}

func deleteInFlightParts(ctx context.Context, tx *gorm.DB, identity InFlightIngestIdentity) error {
	return tx.WithContext(contextOrBackground(ctx)).Where("certificate_fingerprint = ? AND membership_connected_at = ?", identity.CertificateFingerprint, identity.MembershipConnectedAt).Delete(&CPAInFlightSnapshotPartRecord{}).Error
}
