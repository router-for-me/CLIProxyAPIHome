package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	HomeIncarnationActive  = "active"
	HomeIncarnationExpired = "expired"
	HomeIncarnationRetired = "retired"
	HomeIncarnationFenced  = "fenced"
)

var (
	ErrConcurrencyHomeCapabilityRequired = errors.New("Home concurrency capability is required")
	ErrConcurrencySQLiteMultiHome        = errors.New("active concurrency limits require a single SQLite Home")
	ErrHomeIncarnationFenced             = errors.New("Home incarnation is fenced")
	ErrHomeIncarnationNotFound           = errors.New("Home incarnation not found")
	ErrHomeIncarnationInactive           = errors.New("Home incarnation is not active")
)

// HomeIncarnationID identifies one append-only Home process incarnation.
type HomeIncarnationID struct {
	IP        string
	Port      int
	StartedAt time.Time
}

const credentialConcurrencyLimitsCapability = "credential_concurrency_limits_v2"

// RegisterHomeIncarnation records a new active Home incarnation using database time.
func (r *Repository) RegisterHomeIncarnation(ctx context.Context, ip string, port int, capabilities []string) (HomeIncarnationID, error) {
	db, errDB := r.database()
	if errDB != nil {
		return HomeIncarnationID{}, errDB
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return HomeIncarnationID{}, fmt.Errorf("Home incarnation ip is required")
	}
	if port <= 0 {
		return HomeIncarnationID{}, fmt.Errorf("Home incarnation port must be greater than 0")
	}

	var id HomeIncarnationID
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		gate := ConcurrencyActivationGateRecord{ID: 1}
		if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&gate).Error; errCreate != nil {
			return errCreate
		}
		if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&gate, "id = ?", 1).Error; errLock != nil {
			return errLock
		}

		lifecycle, errLifecycle := homeIncarnationLifecycleConfig(tx)
		if errLifecycle != nil {
			return errLifecycle
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		if errExpire := expireStaleHomeIncarnations(ctx, tx, lifecycle.NodeHeartbeatTimeout); errExpire != nil {
			return errExpire
		}
		if gate.ActivePolicyCount > 0 && !slices.Contains(capabilities, credentialConcurrencyLimitsCapability) {
			return ErrConcurrencyHomeCapabilityRequired
		}
		if gate.ActivePolicyCount > 0 && tx.Dialector != nil && tx.Dialector.Name() == "sqlite" {
			var liveHomes int64
			if errCount := tx.Model(&HomeProcessIncarnationRecord{}).Where("state = ?", HomeIncarnationActive).Count(&liveHomes).Error; errCount != nil {
				return errCount
			}
			if liveHomes > 0 {
				return ErrConcurrencySQLiteMultiHome
			}
		}

		var latest HomeProcessIncarnationRecord
		errLatest := tx.Where("home_ip = ? AND home_port = ?", ip, port).Order("started_at DESC").First(&latest).Error
		if errLatest == nil && !now.After(latest.StartedAt) {
			now = latest.StartedAt.Add(databaseTimestampStep(tx))
		} else if errLatest != nil && !errors.Is(errLatest, gorm.ErrRecordNotFound) {
			return errLatest
		}

		normalizedCapabilities := slices.Clone(capabilities)
		slices.Sort(normalizedCapabilities)
		normalizedCapabilities = slices.Compact(normalizedCapabilities)
		encodedCapabilities, errCapabilities := json.Marshal(normalizedCapabilities)
		if errCapabilities != nil {
			return errCapabilities
		}
		record := HomeProcessIncarnationRecord{
			HomeIP:       ip,
			HomePort:     port,
			StartedAt:    now,
			LastSeenAt:   now,
			State:        HomeIncarnationActive,
			Capabilities: JSONB(encodedCapabilities),
		}
		if errCreate := tx.Create(&record).Error; errCreate != nil {
			return errCreate
		}
		id = HomeIncarnationID{IP: record.HomeIP, Port: record.HomePort, StartedAt: record.StartedAt}
		return nil
	})
	return id, errTransaction
}

// HeartbeatHomeIncarnation refreshes an active incarnation with database time.
func (r *Repository) HeartbeatHomeIncarnation(ctx context.Context, id HomeIncarnationID) error {
	return r.updateHomeIncarnation(ctx, id, func(tx *gorm.DB, record *HomeProcessIncarnationRecord, now time.Time) error {
		if record.State == HomeIncarnationFenced {
			return ErrHomeIncarnationFenced
		}
		if record.State != HomeIncarnationActive {
			return ErrHomeIncarnationInactive
		}
		return tx.Model(&HomeProcessIncarnationRecord{}).
			Where("home_ip = ? AND home_port = ? AND started_at = ? AND state = ?", id.IP, id.Port, id.StartedAt, HomeIncarnationActive).
			Update("last_seen_at", now).Error
	})
}

// RetireHomeIncarnation permanently retires an active incarnation.
func (r *Repository) RetireHomeIncarnation(ctx context.Context, id HomeIncarnationID) error {
	return r.updateHomeIncarnation(ctx, id, func(tx *gorm.DB, record *HomeProcessIncarnationRecord, _ time.Time) error {
		if record.State == HomeIncarnationFenced {
			return ErrHomeIncarnationFenced
		}
		if record.State != HomeIncarnationActive {
			return ErrHomeIncarnationInactive
		}
		return tx.Model(&HomeProcessIncarnationRecord{}).
			Where("home_ip = ? AND home_port = ? AND started_at = ? AND state = ?", id.IP, id.Port, id.StartedAt, HomeIncarnationActive).
			Update("state", HomeIncarnationRetired).Error
	})
}

// FenceHomeIncarnation prevents an incarnation from accepting further heartbeats.
func (r *Repository) FenceHomeIncarnation(ctx context.Context, id HomeIncarnationID, reason string) error {
	return r.updateHomeIncarnation(ctx, id, func(tx *gorm.DB, record *HomeProcessIncarnationRecord, now time.Time) error {
		if record.State == HomeIncarnationFenced {
			return nil
		}
		if record.State != HomeIncarnationActive {
			return ErrHomeIncarnationInactive
		}
		return tx.Model(&HomeProcessIncarnationRecord{}).
			Where("home_ip = ? AND home_port = ? AND started_at = ? AND state = ?", id.IP, id.Port, id.StartedAt, HomeIncarnationActive).
			Updates(map[string]any{
				"state":        HomeIncarnationFenced,
				"fenced_at":    now,
				"fence_reason": strings.TrimSpace(reason),
			}).Error
	})
}

func expireStaleHomeIncarnations(ctx context.Context, tx *gorm.DB, nodeHeartbeatTimeout time.Duration) error {
	if nodeHeartbeatTimeout <= 0 {
		return fmt.Errorf("node heartbeat timeout must be positive")
	}
	now, errNow := DatabaseNow(ctx, tx)
	if errNow != nil {
		return errNow
	}
	cutoff := now.Add(-nodeHeartbeatTimeout)
	return tx.Model(&HomeProcessIncarnationRecord{}).
		Where("state = ? AND last_seen_at < ?", HomeIncarnationActive, cutoff).
		Update("state", HomeIncarnationExpired).Error
}

func homeIncarnationLifecycleConfig(tx *gorm.DB) (LifecycleConfigRecord, error) {
	lifecycle := LifecycleConfigRecord{}
	errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lifecycle, "id = ?", 1).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return ensureLifecycleConfigTx(tx, DefaultHeartbeatTimeout())
	}
	if errFirst != nil {
		return LifecycleConfigRecord{}, errFirst
	}
	return lifecycle, nil
}

func (r *Repository) updateHomeIncarnation(ctx context.Context, id HomeIncarnationID, update func(*gorm.DB, *HomeProcessIncarnationRecord, time.Time) error) error {
	if update == nil {
		return fmt.Errorf("Home incarnation update is required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		record := HomeProcessIncarnationRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "home_ip = ? AND home_port = ? AND started_at = ?", strings.TrimSpace(id.IP), id.Port, id.StartedAt).Error
		if errors.Is(errFirst, gorm.ErrRecordNotFound) {
			return ErrHomeIncarnationNotFound
		}
		if errFirst != nil {
			return errFirst
		}
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		return update(tx, &record, now)
	})
}
