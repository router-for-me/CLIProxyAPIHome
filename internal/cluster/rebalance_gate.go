package cluster

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ClusterRebalanceGateRecord serializes plan transitions and load reservations,
// independently of election and lease renewal.
type ClusterRebalanceGateRecord struct {
	ID int `gorm:"column:id;primaryKey;autoIncrement:false"`
}

func (ClusterRebalanceGateRecord) TableName() string { return "cluster_rebalance_gate" }

var errRebalanceStale = errors.New("rebalance leadership or snapshot changed")

// lockMasterGate excludes election writers only for election itself or the final
// rebalance fence. NO KEY UPDATE remains compatible with the legacy guard below.
func lockMasterGate(tx *gorm.DB) (ClusterMasterGateRecord, time.Time, error) {
	startedAt := time.Now()
	gate := ClusterMasterGateRecord{ID: 1}
	errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&gate, "id = ?", 1).Error
	if errors.Is(errLock, gorm.ErrRecordNotFound) {
		if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&gate).Error; errCreate != nil {
			return gate, time.Time{}, errCreate
		}
		errLock = tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&gate, "id = ?", 1).Error
	}
	if trace, ok := tx.Logger.(*gateTransactionTrace); ok {
		trace.gateWait = time.Since(startedAt)
		if errLock == nil {
			trace.lockedAt = time.Now()
		}
	}
	if errLock != nil {
		return gate, time.Time{}, errLock
	}
	// Never use transaction-start time for a lease decision after waiting for a lock.
	now, errNow := DatabaseNow(tx.Statement.Context, tx)
	return gate, now, errNow
}

// lockRebalanceGate protects both new and old Home writers during rolling
// upgrades. Old Homes use FOR UPDATE on the master gate; KEY SHARE blocks them,
// but permits new NO KEY UPDATE elections throughout planning and local waits.
// All new plan writers acquire the independent mutex before this guard.
func lockRebalanceGate(tx *gorm.DB) (ClusterMasterGateRecord, time.Time, error) {
	startedAt := time.Now()
	state := ClusterRebalanceGateRecord{ID: 1}
	errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&state, "id = ?", 1).Error
	if errors.Is(errLock, gorm.ErrRecordNotFound) {
		if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; errCreate != nil {
			return ClusterMasterGateRecord{}, time.Time{}, errCreate
		}
		errLock = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&state, "id = ?", 1).Error
	}
	if trace, ok := tx.Logger.(*gateTransactionTrace); ok {
		trace.rebalanceWait = time.Since(startedAt)
		if errLock == nil {
			trace.rebalanceLockedAt = time.Now()
		}
	}
	if errLock != nil {
		return ClusterMasterGateRecord{}, time.Time{}, errLock
	}
	var gate ClusterMasterGateRecord
	errGuard := tx.Clauses(clause.Locking{Strength: "KEY SHARE"}).First(&gate, "id = ?", 1).Error
	if errors.Is(errGuard, gorm.ErrRecordNotFound) {
		// Only recovery takes the stronger lock early; normal startup seeds both rows.
		return lockMasterGate(tx)
	}
	if errGuard != nil {
		return gate, time.Time{}, errGuard
	}
	now, errNow := DatabaseNow(tx.Statement.Context, tx)
	return gate, now, errNow
}

// fenceRebalanceLeadership is the last database work before commit. All plan,
// client and confirmation writes (and their potentially blocking locks) happen
// before it. A failed fence rolls those writes back rather than authorizing a
// decision made by an old master or from an expired snapshot.
func fenceRebalanceLeadership(tx *gorm.DB, expected ClusterMasterGateRecord, snapshot rebalanceHomeSnapshot, validUntil time.Time, timeout time.Duration, balanceAt time.Time) error {
	gate, now, errGate := lockMasterGate(tx)
	if errGate != nil {
		return errGate
	}
	if gate.Term != expected.Term || gate.MasterKey != expected.MasterKey || gate.Term <= 0 || !now.Before(gate.LeaseUntil) || !now.Before(validUntil) {
		return errRebalanceStale
	}
	current, errHomes := loadRebalanceHomes(tx, now, timeout)
	if errHomes != nil {
		return errHomes
	}
	if !current.leaderValid(gate, now) || len(current.homes) != len(snapshot.homes) {
		return errRebalanceStale
	}
	for key, home := range snapshot.homes {
		// The key already identifies the address and process-start instant.
		candidate, exists := current.homes[key]
		if !exists || candidate.capable != home.capable || candidate.latencyCapable != home.latencyCapable {
			return errRebalanceStale
		}
	}
	if !balanceAt.IsZero() {
		// Keep cadence visible to legacy planners too. Election never takes the
		// rebalance mutex, so this final small write cannot invert lock ordering.
		return tx.Model(&gate).Update("last_balance_at", balanceAt).Error
	}
	return nil
}
