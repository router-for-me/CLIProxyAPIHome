package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	rebalanceCapability = "cpa_rebalance_v1"
	rebalanceInterval   = 30 * time.Second
	rebalancePlanTTL    = 90 * time.Second
	rebalanceCooldown   = 3 * time.Minute
	rebalancePending    = "pending"
	rebalanceExecuting  = "executing"
	rebalanceCompleted  = "completed"
	rebalanceFailed     = "failed"
	rebalanceCanceled   = "canceled"
	rebalanceMaxActive  = 4
)

type CPARebalanceClientRecord struct {
	Fingerprint            string    `gorm:"column:fingerprint;primaryKey"`
	ConnectedAt            time.Time `gorm:"column:connected_at;not null"`
	LatencyConnectedAt     time.Time `gorm:"column:latency_connected_at"`
	OptimizationEligibleAt time.Time `gorm:"column:optimization_eligible_at"`
	// NextEligibleAt gates all destinations, including failure backoff and success cooldown.
	NextEligibleAt time.Time `gorm:"column:next_eligible_at"`
	// FailedTarget and TargetRetryAt describe the latest failure, not independent target gates.
	FailedTarget   string        `gorm:"column:failed_target"`
	TargetRetryAt  time.Time     `gorm:"column:target_retry_at"`
	FailureBackoff time.Duration `gorm:"column:failure_backoff;not null;default:0"`
	UpdatedAt      time.Time     `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (CPARebalanceClientRecord) TableName() string { return "cpa_rebalance_clients" }

type CPARebalancePlanRecord struct {
	ID                string    `gorm:"column:id;primaryKey"`
	SwapGroupID       string    `gorm:"column:swap_group_id;index"`
	Term              int64     `gorm:"column:term;not null"`
	Fingerprint       string    `gorm:"column:fingerprint;not null;index"`
	SourceConnectedAt time.Time `gorm:"column:source_connected_at;not null"`
	SourceIP          string    `gorm:"column:source_ip;not null"`
	SourcePort        int       `gorm:"column:source_port;not null"`
	SourceStartedAt   time.Time `gorm:"column:source_started_at;not null"`
	TargetIP          string    `gorm:"column:target_ip;not null"`
	TargetPort        int       `gorm:"column:target_port;not null"`
	TargetStartedAt   time.Time `gorm:"column:target_started_at;not null"`
	State             string    `gorm:"column:state;not null;index:idx_rebalance_active,priority:1"`
	ExpiresAt         time.Time `gorm:"column:expires_at;not null;index:idx_rebalance_active,priority:2"`
	CreatedAt         time.Time `gorm:"column:created_at;not null"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null;index;autoUpdateTime:false"`
}

func (CPARebalancePlanRecord) TableName() string { return "cpa_rebalance_plans" }

type RebalanceInstruction struct {
	ID     string          `json:"id"`
	Term   int64           `json:"term"`
	Target RebalanceTarget `json:"target"`
}

type RebalanceTarget struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type rebalanceHome struct {
	id             HomeIncarnationID
	capable        bool
	latencyCapable bool
}

func rebalanceHomeKey(home HomeIncarnationID) string {
	return fmt.Sprintf("%s/%d", net.JoinHostPort(home.IP, strconv.Itoa(home.Port)), home.StartedAt.UnixNano())
}

type rebalanceHomeSnapshot struct {
	homes      map[string]rebalanceHome
	masterKey  string
	validUntil time.Time
}

func (s rebalanceHomeSnapshot) leaderValid(gate ClusterMasterGateRecord, now time.Time) bool {
	return gate.Term > 0 && now.Before(gate.LeaseUntil) && s.masterKey != "" && s.masterKey == gate.MasterKey
}

// loadRebalanceHomes reads election eligibility and Home capabilities in one snapshot.
func loadRebalanceHomes(tx *gorm.DB, now time.Time, timeout time.Duration) (rebalanceHomeSnapshot, error) {
	var records []struct {
		IP            string
		Port          int
		StartedAt     time.Time
		IsMaster      bool
		LastSeenAt    time.Time
		ProcessSeenAt time.Time
		ProcessActive bool
		Capabilities  JSONB
	}
	cutoff := now.Add(-timeout)
	errFind := tx.Table("cluster AS node").
		Select("node.ip, node.port, node.started_at, node.is_master, node.last_seen_at, process.last_seen_at AS process_seen_at, process.home_ip IS NOT NULL AS process_active, process.capabilities").
		Joins("LEFT JOIN home_process_incarnation AS process ON process.home_ip = node.ip AND process.home_port = node.port AND process.started_at = node.started_at AND process.state = ? AND process.last_seen_at >= ?", HomeIncarnationActive, cutoff).
		Where("node.last_seen_at >= ?", cutoff).Scan(&records).Error
	if errFind != nil {
		return rebalanceHomeSnapshot{}, errFind
	}
	snapshot := rebalanceHomeSnapshot{homes: make(map[string]rebalanceHome, len(records))}
	masterCount := 0
	for _, record := range records {
		// Count live master flags even when their process is inactive. Filtering
		// them out in the JOIN would hide an inconsistent multi-master state.
		if record.IsMaster {
			masterCount++
		}
		if !record.ProcessActive {
			continue
		}
		id := HomeIncarnationID{IP: record.IP, Port: record.Port, StartedAt: record.StartedAt}
		key := rebalanceHomeKey(id)
		until := minTime(record.LastSeenAt, record.ProcessSeenAt).Add(timeout)
		if snapshot.validUntil.IsZero() || until.Before(snapshot.validUntil) {
			snapshot.validUntil = until
		}
		var capabilities []string
		if errJSON := json.Unmarshal(record.Capabilities, &capabilities); errJSON != nil {
			return rebalanceHomeSnapshot{}, errJSON
		}
		snapshot.homes[key] = rebalanceHome{id: id, capable: slices.Contains(capabilities, rebalanceCapability), latencyCapable: slices.Contains(capabilities, rebalanceLatencyCapability)}
		if record.IsMaster {
			snapshot.masterKey = key
		}
	}
	if masterCount != 1 {
		snapshot.masterKey = ""
	}
	return snapshot, nil
}

func loadRebalanceMembers(tx *gorm.DB, homes map[string]rebalanceHome, now time.Time) ([]CPANodeMembershipRecord, map[string]int, error) {
	var members []CPANodeMembershipRecord
	if errFind := tx.Where("state = ?", MembershipStateActive).Order("certificate_fingerprint").Find(&members).Error; errFind != nil {
		return nil, nil, errFind
	}
	loads := make(map[string]int, len(homes))
	for key := range homes {
		loads[key] = 0
	}
	live := members[:0]
	for _, member := range members {
		key := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
		if _, exists := homes[key]; !exists || member.CPAHeartbeatTimeout <= 0 || !now.Before(member.LastSeenAt.Add(member.CPAHeartbeatTimeout)) {
			continue
		}
		loads[key]++
		live = append(live, member)
	}
	return live, loads, nil
}

func rebalanceSource(plan CPARebalancePlanRecord) HomeIncarnationID {
	return HomeIncarnationID{IP: plan.SourceIP, Port: plan.SourcePort, StartedAt: plan.SourceStartedAt}
}

func rebalanceTarget(plan CPARebalancePlanRecord) HomeIncarnationID {
	return HomeIncarnationID{IP: plan.TargetIP, Port: plan.TargetPort, StartedAt: plan.TargetStartedAt}
}

// reserveRebalanceLoad projects an authorized move without counting a disconnected CPA twice.
// The return value adds an executing CPA missing from the live membership total.
func reserveRebalanceLoad(plan CPARebalancePlanRecord, members map[string]CPANodeMembershipRecord, loads map[string]int) int {
	member, live := members[plan.Fingerprint]
	if plan.State != rebalanceExecuting && (!live || !member.ConnectedAt.Equal(plan.SourceConnectedAt) || !membershipOwnedByHome(member, rebalanceSource(plan))) {
		return 0
	}
	targetKey := rebalanceHomeKey(rebalanceTarget(plan))
	if _, targetLive := loads[targetKey]; !targetLive {
		// Keep the execution slot, but do not invent a live Home for load calculations.
		return 0
	}
	loads[targetKey]++
	if live {
		currentKey := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
		loads[currentKey]--
		return 0
	}
	return 1
}

// plans is the gate-protected working set, including any peer transitions already applied.
// Callers without a working set pass nil and load pending peers only when needed.
func finishRebalancePlan(tx *gorm.DB, plan *CPARebalancePlanRecord, state string, plans []CPARebalancePlanRecord) error {
	if plan.State != rebalancePending && plan.State != rebalanceExecuting {
		return nil
	}
	var client CPARebalanceClientRecord
	// Serialize read-modify-write state with telemetry for this CPA, not just other planners.
	if errFirst := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
		return errFirst
	}
	// Local telemetry can delay this lock without delaying election. Base cooldowns
	// and backoff on database time after that wait, not the planning snapshot.
	now, errNow := DatabaseNow(tx.Statement.Context, tx)
	if errNow != nil {
		return errNow
	}
	plan.State, plan.UpdatedAt = state, now
	if errSave := tx.Save(plan).Error; errSave != nil {
		return errSave
	}
	nextEligibleAt := now.Add(rebalanceInterval)
	if state == rebalanceCompleted {
		client.NextEligibleAt = now.Add(rebalanceCooldown)
		if plan.SwapGroupID != "" {
			client.OptimizationEligibleAt = now.Add(rebalanceLatencyCooldown)
		}
		client.FailureBackoff = 0
		client.FailedTarget = ""
		client.TargetRetryAt = time.Time{}
	} else if state == rebalanceFailed {
		client.FailureBackoff = min(max(time.Minute, client.FailureBackoff*2), 10*time.Minute)
		client.FailedTarget = rebalanceHomeKey(rebalanceTarget(*plan))
		client.TargetRetryAt = now.Add(client.FailureBackoff)
		nextEligibleAt = client.TargetRetryAt
	}
	// Changing targets or canceling another plan must not shorten an outstanding backoff.
	if state != rebalanceCompleted && client.NextEligibleAt.Before(nextEligibleAt) {
		client.NextEligibleAt = nextEligibleAt
	}
	client.UpdatedAt = now
	if errSave := tx.Save(&client).Error; errSave != nil {
		return errSave
	}
	if plan.SwapGroupID != "" && (state == rebalanceFailed || state == rebalanceCanceled) {
		if plans == nil {
			if errFind := tx.Where("swap_group_id = ? AND state = ?", plan.SwapGroupID, rebalancePending).Find(&plans).Error; errFind != nil {
				return errFind
			}
			if trace, ok := tx.Logger.(*gateTransactionTrace); ok {
				trace.planCount += len(plans)
			}
		}
		for index := range plans {
			peer := &plans[index]
			if peer.SwapGroupID != plan.SwapGroupID || peer.State != rebalancePending {
				continue
			}
			if errFinish := finishRebalancePlan(tx, peer, rebalanceCanceled, plans); errFinish != nil {
				return errFinish
			}
		}
	}
	return nil
}

// ReconcileRebalances serializes reservations independently of lease renewal.
// The election fence is acquired only after all potentially blocking plan work.
func (r *Repository) ReconcileRebalances(ctx context.Context, home HomeIncarnationID, timeout time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	trace := gateTransactionTrace{Interface: db.Logger, operation: "rebalance_plan", startedAt: time.Now()}
	errTransaction := trace.transaction(ctx, db, func(tx *gorm.DB) (errResult error) {
		gate, now, errGate := lockRebalanceGate(tx)
		if errGate != nil {
			return errGate
		}
		if gate.MasterKey != rebalanceHomeKey(home) || gate.Term <= 0 || !now.Before(gate.LeaseUntil) {
			return nil
		}
		snapshot, errHomes := loadRebalanceHomes(tx, now, timeout)
		if errHomes != nil {
			return errHomes
		}
		if !snapshot.leaderValid(gate, now) {
			return nil
		}
		var balanceAt time.Time
		plansChanged := false
		validUntil := snapshot.validUntil
		defer func() {
			// Read-only maintenance has no decision to fence or state to commit.
			if errResult == nil && (plansChanged || !balanceAt.IsZero()) {
				errResult = fenceRebalanceLeadership(tx, gate, snapshot, validUntil, timeout, balanceAt)
			}
		}()
		homes := snapshot.homes
		members, loads, errMembers := loadRebalanceMembers(tx, homes, now)
		if errMembers != nil {
			return errMembers
		}
		byFingerprint := make(map[string]CPANodeMembershipRecord, len(members))
		total := len(members)
		for _, member := range members {
			byFingerprint[member.CertificateFingerprint] = member
			validUntil = minTime(validUntil, member.LastSeenAt.Add(member.CPAHeartbeatTimeout))
		}
		var plans []CPARebalancePlanRecord
		if errFind := tx.Where("state IN ?", []string{rebalancePending, rebalanceExecuting}).Find(&plans).Error; errFind != nil {
			return errFind
		}
		trace.planCount = len(plans)
		busy := make(map[string]bool)
		active := 0
		for index := range plans {
			plan := &plans[index]
			if plan.State != rebalancePending && plan.State != rebalanceExecuting {
				continue
			}
			member, exists := byFingerprint[plan.Fingerprint]
			sourceMatches := exists && membershipOwnedByHome(member, rebalanceSource(*plan)) && member.ConnectedAt.Equal(plan.SourceConnectedAt)
			targetMatches := exists && membershipOwnedByHome(member, rebalanceTarget(*plan)) && member.ConnectedAt.After(plan.SourceConnectedAt)
			_, targetLive := homes[rebalanceHomeKey(rebalanceTarget(*plan))]
			state := ""
			// Source liveness can lapse while the CPA settles pending dispatches.
			// Executing plans keep capacity until completion, explicit failure, or expiry.
			switch {
			case plan.State == rebalanceExecuting && targetMatches:
				state = rebalanceCompleted
			case plan.SwapGroupID != "" && plan.State == rebalanceExecuting && exists && !sourceMatches:
				// A new lifetime elsewhere is a failed exchange leg, not an in-progress drain.
				state = rebalanceFailed
			case !now.Before(plan.ExpiresAt):
				state = rebalanceFailed
			case plan.State == rebalancePending && (!sourceMatches || plan.Term != gate.Term):
				state = rebalanceCanceled
			case plan.State == rebalancePending && !targetLive:
				state = rebalanceFailed
			}
			if state != "" {
				if errFinish := finishRebalancePlan(tx, plan, state, plans); errFinish != nil {
					return errFinish
				}
				plansChanged = true
				continue
			}
		}
		// Peer cancellations update this same working set, so reservations need no reload.
		for _, plan := range plans {
			if plan.State != rebalancePending && plan.State != rebalanceExecuting {
				continue
			}
			busy[plan.Fingerprint] = true
			active++
			total += reserveRebalanceLoad(plan, byFingerprint, loads)
		}
		if now.Sub(gate.LastBalanceAt) < rebalanceInterval {
			return nil
		}
		balanceAt = now
		if len(homes) < 2 {
			return nil
		}
		latencies, errLatencies := loadRebalanceLatencies(tx, now)
		if errLatencies != nil {
			return errLatencies
		}
		var clients []CPARebalanceClientRecord
		if errFind := tx.Find(&clients).Error; errFind != nil {
			return errFind
		}
		capabilities := make(map[string]CPARebalanceClientRecord, len(clients))
		for _, client := range clients {
			capabilities[client.Fingerprint] = client
		}
		average := float64(total) / float64(len(homes))
		quotas := make(map[string]int, len(homes))
		for key, count := range loads {
			if float64(count) > average {
				quotas[key] = max(1, int(math.Min(float64(count)*0.25, (float64(count)-average)*0.5)))
			}
		}
		// Reserve both ends immediately, then persist the selected plans in one batch.
		var newPlans []CPARebalancePlanRecord
		for active < rebalanceMaxActive {
			var chosen CPANodeMembershipRecord
			sourceKey, targetKey := "", ""
			var chosenGain time.Duration
			chosenKnown := false
			for _, member := range members {
				client, supported := capabilities[member.CertificateFingerprint]
				if !supported || busy[member.CertificateFingerprint] || !client.ConnectedAt.Equal(member.ConnectedAt) || now.Before(client.NextEligibleAt) || member.ProtocolVersion != 1 {
					continue
				}
				if _, live := byFingerprint[member.CertificateFingerprint]; !live {
					continue
				}
				key := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
				if quotas[key] <= 0 || !homes[key].capable || float64(loads[key]) <= average {
					continue
				}
				for destination, candidate := range homes {
					if !candidate.capable || loads[key]-loads[destination] < 2 || float64(loads[destination]) >= average {
						continue
					}
					gain, _, known := latencyBenefit(latencies, member, candidate.id)
					betterLoad := sourceKey == "" || loads[key] > loads[sourceKey] || (loads[key] == loads[sourceKey] && loads[destination] < loads[targetKey])
					sameLoad := sourceKey != "" && loads[key] == loads[sourceKey] && loads[destination] == loads[targetKey]
					betterLatency := known && (!chosenKnown || gain > chosenGain)
					fallback := known == chosenKnown && (!known || gain == chosenGain) && destination < targetKey
					if betterLoad || (sameLoad && (betterLatency || fallback)) {
						chosen, sourceKey, targetKey = member, key, destination
						chosenGain, chosenKnown = gain, known
					}
				}
			}
			if sourceKey == "" {
				break
			}
			target := homes[targetKey].id
			newPlans = append(newPlans, newRebalancePlan(chosen, target, gate.Term, now))
			busy[chosen.CertificateFingerprint] = true
			quotas[sourceKey]--
			loads[sourceKey]--
			loads[targetKey]++
			active++
		}
		if len(newPlans) > 0 {
			trace.planCount += len(newPlans)
			if errCreate := tx.Create(&newPlans).Error; errCreate != nil {
				return errCreate
			}
		}
		return planLatencySwap(tx, gate, now, homes, members, loads, capabilities, latencies, active)
	})
	trace.logResult(errTransaction)
	if errors.Is(errTransaction, errRebalanceStale) {
		return nil
	}
	return errTransaction
}

func (c *Coordinator) runRebalanceLoop(ctx context.Context) {
	ticker := time.NewTicker(rebalanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.IsMaster() {
				continue
			}
			home, initialized := c.HomeIncarnation()
			if !initialized {
				continue
			}
			if errReconcile := c.repo.ReconcileRebalances(ctx, home, c.heartbeatTimeout); errReconcile != nil && ctx.Err() == nil {
				log.Warnf("failed to reconcile Home rebalance plans: %v", errReconcile)
			}
		}
	}
}

// RebalanceInstruction reads candidate plans without locking the election gate.
// CLAIM revalidates leadership and membership under the gate before authorizing a move.
func (h *RESPHandler) RebalanceInstruction(ctx context.Context, lifetime ConnectionLifetime) ([]byte, error) {
	if h == nil || h.repo == nil || h.repo.db == nil || h.coordinator == nil {
		return nil, fmt.Errorf("cluster rebalance unavailable")
	}
	db := h.repo.db.WithContext(contextOrBackground(ctx))
	var gate ClusterMasterGateRecord
	if errGate := db.First(&gate, "id = ?", 1).Error; errGate != nil {
		return nil, errGate
	}
	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return nil, errNow
	}
	if gate.Term <= 0 || !now.Before(gate.LeaseUntil) {
		return nil, nil
	}
	snapshot, errHomes := loadRebalanceHomes(db, now, h.coordinator.heartbeatTimeout)
	if errHomes != nil {
		return nil, errHomes
	}
	if !snapshot.leaderValid(gate, now) {
		return nil, nil
	}
	var plan CPARebalancePlanRecord
	errFirst := db.Where("fingerprint = ? AND source_connected_at = ? AND source_ip = ? AND source_port = ? AND source_started_at = ? AND state = ? AND term = ? AND expires_at > ?", lifetime.Fingerprint, lifetime.ConnectedAt, lifetime.Home.IP, lifetime.Home.Port, lifetime.Home.StartedAt, rebalancePending, gate.Term, now).First(&plan).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if errFirst != nil {
		return nil, errFirst
	}
	return json.Marshal(RebalanceInstruction{ID: plan.ID, Term: plan.Term, Target: RebalanceTarget{IP: plan.TargetIP, Port: plan.TargetPort}})
}

// HandleRebalance uses the mTLS-classified lifetime, never a caller-supplied CPA identity.
func (h *RESPHandler) HandleRebalance(ctx context.Context, lifetime ConnectionLifetime, args []string) ([]byte, error) {
	if h == nil || h.repo == nil || h.repo.db == nil || h.coordinator == nil {
		return nil, fmt.Errorf("cluster rebalance unavailable")
	}
	if len(args) < 3 {
		return nil, fmt.Errorf("cluster rebalance: missing action")
	}
	action := strings.ToUpper(args[2])
	var term int64
	switch action {
	case "ENABLE":
		if len(args) != 3 {
			return nil, fmt.Errorf("cluster rebalance: invalid enable arguments")
		}
	case "CLAIM":
		if len(args) != 7 {
			return nil, fmt.Errorf("cluster rebalance: invalid claim arguments")
		}
		var errTerm error
		term, errTerm = strconv.ParseInt(args[4], 10, 64)
		if errTerm != nil {
			return nil, errTerm
		}
	case "RESULT":
		if len(args) != 5 || (args[4] != "success" && args[4] != "failed") {
			return nil, fmt.Errorf("cluster rebalance: invalid result arguments")
		}
	case "LATENCY":
		if len(args) != 4 {
			return nil, fmt.Errorf("cluster latency: invalid arguments")
		}
	default:
		return nil, fmt.Errorf("cluster rebalance: unsupported action")
	}
	accepted := false
	var probe *RebalanceTarget
	trace := gateTransactionTrace{Interface: h.repo.db.Logger, operation: "rebalance_" + strings.ToLower(action), startedAt: time.Now()}
	errTransaction := trace.transaction(ctx, h.repo.db, func(tx *gorm.DB) (errResult error) {
		// Local dispatch contention must not hold the cluster-wide election gate.
		if errLifetime := h.repo.LockActiveConcurrencyLifetimeTx(ctx, tx, lifetime); errLifetime != nil {
			return errLifetime
		}
		if action == "LATENCY" {
			// Serialize reports only for this CPA, including first observations and
			// read-modify-write failure backoff. Telemetry never takes the election gate.
			var client CPARebalanceClientRecord
			if errFirst := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ? AND connected_at = ?", lifetime.Fingerprint, lifetime.ConnectedAt).Error; errFirst != nil {
				return errFirst
			}
			now, errNow := DatabaseNow(ctx, tx)
			if errNow != nil {
				return errNow
			}
			snapshot, errHomes := loadRebalanceHomes(tx, now, h.coordinator.heartbeatTimeout)
			if errHomes != nil {
				return errHomes
			}
			var errReport error
			probe, errReport = acceptLatencyReport(tx, lifetime, args[3], now, snapshot.homes, client)
			if errReport != nil {
				return errReport
			}
			accepted = true
			return nil
		}
		// Read database time after the lifetime, rebalance, and compatibility locks.
		gate, now, errGate := lockRebalanceGate(tx)
		if errGate != nil {
			return errGate
		}
		switch action {
		case "ENABLE":
			var member CPANodeMembershipRecord
			if errFirst := tx.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
				return errFirst
			}
			if member.ProtocolVersion != 1 {
				return ErrConcurrencyProtocolRequired
			}
			client := CPARebalanceClientRecord{Fingerprint: lifetime.Fingerprint, ConnectedAt: lifetime.ConnectedAt, UpdatedAt: now}
			if errCreate := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "fingerprint"}}, DoUpdates: clause.Assignments(map[string]any{"connected_at": lifetime.ConnectedAt, "updated_at": now})}).Create(&client).Error; errCreate != nil {
				return errCreate
			}
			accepted = true
			return nil
		case "CLAIM":
			if gate.Term <= 0 || !now.Before(gate.LeaseUntil) {
				return nil
			}
			snapshot, errHomes := loadRebalanceHomes(tx, now, h.coordinator.heartbeatTimeout)
			if errHomes != nil {
				return errHomes
			}
			if !snapshot.leaderValid(gate, now) {
				return nil
			}
			homes := snapshot.homes
			var plan CPARebalancePlanRecord
			errFirst := tx.First(&plan, "id = ? AND fingerprint = ?", args[3], lifetime.Fingerprint).Error
			if errors.Is(errFirst, gorm.ErrRecordNotFound) {
				return nil
			}
			if errFirst != nil {
				return errFirst
			}
			if plan.State != rebalancePending || term != gate.Term || plan.Term != term || !now.Before(plan.ExpiresAt) || !lifetime.ConnectedAt.Equal(plan.SourceConnectedAt) || rebalanceHomeKey(lifetime.Home) != rebalanceHomeKey(rebalanceSource(plan)) || plan.TargetIP != args[5] || strconv.Itoa(plan.TargetPort) != args[6] {
				return nil
			}
			trace.planCount = 1
			if target, live := homes[rebalanceHomeKey(rebalanceTarget(plan))]; !live || !target.capable {
				return nil
			}
			validUntil := minTime(snapshot.validUntil, plan.ExpiresAt)
			defer func() {
				if errResult == nil {
					errResult = fenceRebalanceLeadership(tx, gate, snapshot, validUntil, h.coordinator.heartbeatTimeout, time.Time{})
				}
			}()
			members, loads, errMembers := loadRebalanceMembers(tx, homes, now)
			if errMembers != nil {
				return errMembers
			}
			current := make(map[string]CPANodeMembershipRecord, len(members))
			for _, member := range members {
				current[member.CertificateFingerprint] = member
				validUntil = minTime(validUntil, member.LastSeenAt.Add(member.CPAHeartbeatTimeout))
			}
			if _, healthy := current[lifetime.Fingerprint]; !healthy {
				return finishRebalancePlan(tx, &plan, rebalanceCanceled, nil)
			}
			var others []CPARebalancePlanRecord
			if errFind := tx.Where("id <> ? AND expires_at > ? AND (state = ? OR (state = ? AND term = ?))", plan.ID, now, rebalanceExecuting, rebalancePending, gate.Term).Find(&others).Error; errFind != nil {
				return errFind
			}
			for _, other := range others {
				reserveRebalanceLoad(other, current, loads)
			}
			if plan.SwapGroupID != "" {
				validSwap, errSwap := validateSwapClaim(tx, plan, gate, now, homes, current)
				if errSwap != nil {
					return errSwap
				}
				if !validSwap {
					return finishRebalancePlan(tx, &plan, rebalanceCanceled, nil)
				}
			} else if loads[rebalanceHomeKey(rebalanceSource(plan))]-loads[rebalanceHomeKey(rebalanceTarget(plan))] < 2 {
				return finishRebalancePlan(tx, &plan, rebalanceCanceled, nil)
			}
			plan.State, plan.UpdatedAt, plan.ExpiresAt = rebalanceExecuting, now, now.Add(rebalancePlanTTL)
			if errSave := tx.Save(&plan).Error; errSave != nil {
				return errSave
			}
			accepted = true
			return nil
		case "RESULT":
			var plan CPARebalancePlanRecord
			errFirst := tx.First(&plan, "id = ? AND fingerprint = ?", args[3], lifetime.Fingerprint).Error
			if errors.Is(errFirst, gorm.ErrRecordNotFound) {
				accepted = true
				return nil
			}
			if errFirst != nil {
				return errFirst
			}
			if plan.State != rebalancePending && plan.State != rebalanceExecuting {
				accepted = true
				return nil
			}
			state := rebalanceFailed
			if args[4] == "success" {
				if plan.State != rebalanceExecuting || rebalanceHomeKey(lifetime.Home) != rebalanceHomeKey(rebalanceTarget(plan)) || !lifetime.ConnectedAt.After(plan.SourceConnectedAt) {
					return nil
				}
				state = rebalanceCompleted
			}
			trace.planCount = 1
			if errFinish := finishRebalancePlan(tx, &plan, state, nil); errFinish != nil {
				return errFinish
			}
			accepted = true
			return nil
		default:
			return fmt.Errorf("cluster rebalance: unsupported action")
		}
	})
	trace.logResult(errTransaction)
	if errors.Is(errTransaction, errRebalanceStale) {
		accepted = false
		errTransaction = nil
	}
	if errTransaction != nil {
		return nil, errTransaction
	}
	return json.Marshal(struct {
		OK           bool             `json:"ok"`
		Capabilities []string         `json:"capabilities,omitempty"`
		Probe        *RebalanceTarget `json:"probe,omitempty"`
	}{OK: accepted, Capabilities: []string{rebalanceLatencyCapability}, Probe: probe})
}
