package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrDuplicateCPACertificate         = errors.New("CPA certificate is already owned by an active membership")
	ErrConcurrencyProtocolRequired     = errors.New("credential concurrency protocol version 1 is required")
	ErrLifecycleConfigRevisionMismatch = errors.New("lifecycle configuration revision does not match")
	ErrMembershipNotActive             = errors.New("CPA membership is not active")
	ErrMembershipTakeoverUnavailable   = errors.New("membership_takeover_unavailable")
)

// ConnectionLifetime identifies the membership lifetime associated with a connection.
type ConnectionLifetime struct {
	Fingerprint  string
	ConnectedAt  time.Time
	InstanceID   string
	Home         HomeIncarnationID
	Controlled   bool
	Subscription bool
}

// SubscribeMembershipRequest contains the subscription identity supplied by a CPA node.
type SubscribeMembershipRequest struct {
	Fingerprint             string
	NodeID                  string
	Home                    HomeIncarnationID
	ProtocolVersion         int
	LifecycleConfigRevision int64
	Takeover                bool
}

func membershipOwnedByHome(member CPANodeMembershipRecord, home HomeIncarnationID) bool {
	return strings.TrimSpace(member.HomeIP) == strings.TrimSpace(home.IP) &&
		member.HomePort == home.Port && member.HomeStartedAt.Equal(home.StartedAt)
}

// ClassifyConnection determines whether a connection belongs to an active membership.
func (r *Repository) ClassifyConnection(ctx context.Context, fingerprint string, home HomeIncarnationID) (ConnectionLifetime, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	lifetime := ConnectionLifetime{Fingerprint: fingerprint, Home: home}
	if fingerprint == "" {
		return lifetime, nil
	}

	db, errDB := r.database()
	if errDB != nil {
		return ConnectionLifetime{}, errDB
	}
	member := CPANodeMembershipRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("certificate_fingerprint = ?", fingerprint).First(&member).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return lifetime, nil
	}
	if errFirst != nil {
		return ConnectionLifetime{}, errFirst
	}
	if member.State != MembershipStateActive || !membershipOwnedByHome(member, home) {
		return lifetime, nil
	}

	lifetime.ConnectedAt = member.ConnectedAt
	lifetime.Controlled = true
	if errParticipation := r.RecordParticipation(ctx, lifetime); errParticipation != nil {
		return ConnectionLifetime{}, errParticipation
	}
	return lifetime, nil
}

// SubscribeMembership creates a new active membership for a certificate fingerprint.
func (r *Repository) SubscribeMembership(ctx context.Context, request SubscribeMembershipRequest) (CPANodeMembershipRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return CPANodeMembershipRecord{}, errDB
	}
	request.Fingerprint = strings.TrimSpace(request.Fingerprint)
	request.NodeID = strings.TrimSpace(request.NodeID)
	request.Home.IP = strings.TrimSpace(request.Home.IP)
	if request.Fingerprint == "" {
		return CPANodeMembershipRecord{}, fmt.Errorf("CPA certificate fingerprint is required")
	}
	if request.NodeID == "" {
		return CPANodeMembershipRecord{}, fmt.Errorf("CPA node ID is required")
	}
	if request.Home.IP == "" || request.Home.Port <= 0 || request.Home.StartedAt.IsZero() {
		return CPANodeMembershipRecord{}, fmt.Errorf("Home incarnation is required")
	}
	if request.ProtocolVersion != 0 && request.ProtocolVersion != 1 {
		return CPANodeMembershipRecord{}, fmt.Errorf("unsupported membership protocol version %d", request.ProtocolVersion)
	}

	accepted := CPANodeMembershipRecord{}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		gate := ConcurrencyActivationGateRecord{ID: 1}
		if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&gate).Error; errCreate != nil {
			return errCreate
		}
		if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&gate, "id = ?", 1).Error; errLock != nil {
			return errLock
		}

		lifecycle := LifecycleConfigRecord{}
		if errLifecycle := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lifecycle, "id = ?", 1).Error; errLifecycle != nil {
			return errLifecycle
		}
		cfg := config.CredentialConcurrencyConfig{}
		if errUnmarshal := json.Unmarshal(lifecycle.Payload, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
		if request.ProtocolVersion == 0 && gate.ActivePolicyCount != 0 {
			return ErrConcurrencyProtocolRequired
		}
		if request.ProtocolVersion == 1 && request.LifecycleConfigRevision != lifecycle.Revision {
			return ErrLifecycleConfigRevisionMismatch
		}

		previous := CPANodeMembershipRecord{}
		errPrevious := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("certificate_fingerprint = ?", request.Fingerprint).First(&previous).Error
		if request.Takeover && (errors.Is(errPrevious, gorm.ErrRecordNotFound) || (errPrevious == nil && previous.State == MembershipStateClosed)) {
			return ErrMembershipTakeoverUnavailable
		}
		if errPrevious == nil && previous.State != MembershipStateClosed {
			if !request.Takeover || previous.NodeID != request.NodeID {
				return ErrDuplicateCPACertificate
			}
			if previous.LifecycleConfigRevision != request.LifecycleConfigRevision {
				return ErrMembershipTakeoverUnavailable
			}
		}
		if errPrevious != nil && !errors.Is(errPrevious, gorm.ErrRecordNotFound) {
			return errPrevious
		}
		if errHome := verifyActiveHomeIncarnation(tx, request.Home); errHome != nil {
			return errHome
		}
		if errPrevious == nil && previous.State != MembershipStateClosed {
			oldLifetime := "certificate_fingerprint = ? AND membership_connected_at = ?"
			oldArgs := []any{previous.CertificateFingerprint, previous.ConnectedAt}
			if errDelete := tx.Where(oldLifetime, oldArgs...).Delete(&CPANodeParticipationRecord{}).Error; errDelete != nil {
				return errDelete
			}
			if errDelete := tx.Where(oldLifetime, oldArgs...).Delete(&CPANodeQuiescenceRecord{}).Error; errDelete != nil {
				return errDelete
			}
			if errDelete := tx.Where(oldLifetime, oldArgs...).Delete(&CPAInFlightSnapshotPartRecord{}).Error; errDelete != nil {
				return errDelete
			}
			if errDelete := tx.Where(oldLifetime, oldArgs...).Delete(&CPAInFlightSnapshotAttemptRecord{}).Error; errDelete != nil {
				return errDelete
			}
			if errDelete := tx.Where(oldLifetime, oldArgs...).Delete(&CPAInFlightSnapshotRecord{}).Error; errDelete != nil {
				return errDelete
			}
		}

		dbNow, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		connectedAt := dbNow
		if !previous.ConnectedAt.IsZero() && !connectedAt.After(previous.ConnectedAt) {
			connectedAt = previous.ConnectedAt.Add(databaseTimestampStep(tx))
		}
		accepted = CPANodeMembershipRecord{
			CertificateFingerprint:  request.Fingerprint,
			NodeID:                  request.NodeID,
			HomeIP:                  request.Home.IP,
			HomePort:                request.Home.Port,
			HomeStartedAt:           request.Home.StartedAt,
			ProtocolVersion:         request.ProtocolVersion,
			State:                   MembershipStateActive,
			ConnectedAt:             connectedAt,
			LifecycleConfigRevision: lifecycle.Revision,
			CPAHeartbeatTimeout:     cfg.CPAHeartbeatTimeout,
			CPACancelBound:          cfg.CPACancelBound,
			ReclaimGrace:            cfg.ReclaimGrace,
			LastSeenAt:              dbNow,
			UpdatedAt:               dbNow,
		}
		return tx.Save(&accepted).Error
	})
	return accepted, errTransaction
}

// RecordParticipation records a Home's connection to an active membership lifetime.
func (r *Repository) RecordParticipation(ctx context.Context, lifetime ConnectionLifetime) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	fingerprint := strings.TrimSpace(lifetime.Fingerprint)
	if fingerprint == "" || lifetime.ConnectedAt.IsZero() || strings.TrimSpace(lifetime.Home.IP) == "" || lifetime.Home.Port <= 0 || lifetime.Home.StartedAt.IsZero() {
		return fmt.Errorf("membership participation lifetime is invalid")
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		member := CPANodeMembershipRecord{}
		errMember := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("certificate_fingerprint = ?", fingerprint).
			First(&member).Error
		if errors.Is(errMember, gorm.ErrRecordNotFound) {
			return ErrMembershipNotActive
		}
		if errMember != nil {
			return errMember
		}
		if member.State != MembershipStateActive || !member.ConnectedAt.Equal(lifetime.ConnectedAt) {
			return ErrMembershipNotActive
		}

		home := HomeProcessIncarnationRecord{}
		errHome := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&home, "home_ip = ? AND home_port = ? AND started_at = ?", strings.TrimSpace(lifetime.Home.IP), lifetime.Home.Port, lifetime.Home.StartedAt).Error
		if errors.Is(errHome, gorm.ErrRecordNotFound) {
			return ErrHomeIncarnationNotFound
		}
		if errHome != nil {
			return errHome
		}
		if home.State == HomeIncarnationFenced {
			return ErrHomeIncarnationFenced
		}
		if home.State != HomeIncarnationActive {
			return ErrHomeIncarnationInactive
		}

		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		record := CPANodeParticipationRecord{
			CertificateFingerprint: fingerprint,
			MembershipConnectedAt:  lifetime.ConnectedAt,
			HomeIP:                 strings.TrimSpace(lifetime.Home.IP),
			HomePort:               lifetime.Home.Port,
			HomeStartedAt:          lifetime.Home.StartedAt,
			CreatedAt:              now,
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
	})
}
