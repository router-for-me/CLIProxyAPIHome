package cluster

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// A single persisted confirmation survives leadership changes without treating old data as new evidence.
type CPALatencyConfirmationRecord struct {
	ID              string `gorm:"primaryKey"`
	Signature       string
	FirstSeenAt     time.Time
	FirstEvidenceAt time.Time
	SourceAAt       time.Time
	TargetAAt       time.Time
	SourceBAt       time.Time
	TargetBAt       time.Time
	EvidenceCount   int
	UpdatedAt       time.Time `gorm:"index;autoUpdateTime:false"`
}

func (CPALatencyConfirmationRecord) TableName() string { return "cpa_latency_confirmations" }

func newRebalancePlan(member CPANodeMembershipRecord, target HomeIncarnationID, term int64, now time.Time) CPARebalancePlanRecord {
	return CPARebalancePlanRecord{ID: uuid.NewString(), Term: term, Fingerprint: member.CertificateFingerprint,
		SourceConnectedAt: member.ConnectedAt, SourceIP: member.HomeIP, SourcePort: member.HomePort, SourceStartedAt: member.HomeStartedAt,
		TargetIP: target.IP, TargetPort: target.Port, TargetStartedAt: target.StartedAt,
		State: rebalancePending, ExpiresAt: now.Add(rebalancePlanTTL), CreatedAt: now, UpdatedAt: now}
}

func planLatencySwap(tx *gorm.DB, gate ClusterMasterGateRecord, now time.Time, homes map[string]rebalanceHome, members []CPANodeMembershipRecord, loads map[string]int, clients map[string]CPARebalanceClientRecord, samples map[string]CPALatencyRecord, active int) error {
	var confirmation CPALatencyConfirmationRecord
	errFirst := tx.First(&confirmation, "id = ?", "active").Error
	if errFirst != nil && errFirst != gorm.ErrRecordNotFound {
		return errFirst
	}
	// Wait for all count moves to settle, and never optimize a mixed-version cluster.
	eligible := active == 0
	low, high := int(^uint(0)>>1), 0
	for key, home := range homes {
		eligible = eligible && home.capable && home.latencyCapable
		low, high = min(low, loads[key]), max(high, loads[key])
	}
	eligible = eligible && high-low <= 1
	var a, b CPANodeMembershipRecord
	if eligible {
		retainedSignature := confirmation.Signature
		if now.Sub(confirmation.UpdatedAt) > rebalanceLatencyTTL {
			retainedSignature = ""
		}
		a, b = selectLatencySwap(now, homes, members, clients, samples, retainedSignature)
	}
	if a.CertificateFingerprint == "" {
		if errFirst == gorm.ErrRecordNotFound {
			return nil
		}
		return tx.Where("id = ?", "active").Delete(&CPALatencyConfirmationRecord{}).Error
	}
	aHome := HomeIncarnationID{IP: a.HomeIP, Port: a.HomePort, StartedAt: a.HomeStartedAt}
	bHome := HomeIncarnationID{IP: b.HomeIP, Port: b.HomePort, StartedAt: b.HomeStartedAt}
	signature := latencySwapSignature(a, b)
	evidence := [4]time.Time{samples[latencyKey(a.CertificateFingerprint, a.HomeIP, a.HomePort)].SampledAt,
		samples[latencyKey(a.CertificateFingerprint, b.HomeIP, b.HomePort)].SampledAt,
		samples[latencyKey(b.CertificateFingerprint, b.HomeIP, b.HomePort)].SampledAt,
		samples[latencyKey(b.CertificateFingerprint, a.HomeIP, a.HomePort)].SampledAt}
	if errFirst != nil || confirmation.Signature != signature || now.Sub(confirmation.UpdatedAt) > rebalanceLatencyTTL {
		latest := evidence[0]
		for _, sampledAt := range evidence[1:] {
			if sampledAt.After(latest) {
				latest = sampledAt
			}
		}
		confirmation = CPALatencyConfirmationRecord{ID: "active", Signature: signature, FirstSeenAt: now, FirstEvidenceAt: latest, SourceAAt: evidence[0], TargetAAt: evidence[1], SourceBAt: evidence[2], TargetBAt: evidence[3], EvidenceCount: 1}
	} else if evidence[0].After(confirmation.SourceAAt) && evidence[1].After(confirmation.TargetAAt) && evidence[2].After(confirmation.SourceBAt) && evidence[3].After(confirmation.TargetBAt) {
		confirmation.SourceAAt, confirmation.TargetAAt, confirmation.SourceBAt, confirmation.TargetBAt = evidence[0], evidence[1], evidence[2], evidence[3]
		confirmation.EvidenceCount++
	}
	confirmation.UpdatedAt = now
	if errSave := tx.Save(&confirmation).Error; errSave != nil {
		return errSave
	}
	earliest := minTime(minTime(confirmation.SourceAAt, confirmation.TargetAAt), minTime(confirmation.SourceBAt, confirmation.TargetBAt))
	if now.Sub(confirmation.FirstSeenAt) < rebalanceLatencyTTL || earliest.Sub(confirmation.FirstEvidenceAt) < rebalanceLatencyTTL || confirmation.EvidenceCount < 3 {
		return nil
	}
	groupID := uuid.NewString()
	plans := []CPARebalancePlanRecord{newRebalancePlan(a, bHome, gate.Term, now), newRebalancePlan(b, aHome, gate.Term, now)}
	for index := range plans {
		plans[index].SwapGroupID = groupID
	}
	if errCreate := tx.Create(&plans).Error; errCreate != nil {
		return errCreate
	}
	if trace, ok := tx.Logger.(*gateTransactionTrace); ok {
		trace.planCount += len(plans)
	}
	return tx.Delete(&confirmation).Error
}

// selectLatencySwap first retains the exact confirmed lifetimes, then searches in
// O(CPA * Home + Home^2). Gains are additive and opposite directions use disjoint
// source memberships, so only the best eligible CPA per direction is needed.
func selectLatencySwap(now time.Time, homes map[string]rebalanceHome, members []CPANodeMembershipRecord, clients map[string]CPARebalanceClientRecord, samples map[string]CPALatencyRecord, retainedSignature string) (CPANodeMembershipRecord, CPANodeMembershipRecord) {
	if retainedSignature != "" {
		var a, b CPANodeMembershipRecord
		// Match the existing persisted signature without adding schema fields or
		// enumerating pairs. The full comparison below also verifies both lifetimes.
		for _, member := range members {
			home := HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}
			identity := fmt.Sprintf("%s/%s/%d", member.CertificateFingerprint, rebalanceHomeKey(home), member.ConnectedAt.UnixNano())
			if strings.HasPrefix(retainedSignature, identity+"/") {
				a = member
			}
			if strings.HasSuffix(retainedSignature, "/"+identity) {
				b = member
			}
		}
		if a.CertificateFingerprint != "" && b.CertificateFingerprint != "" &&
			(a.HomeIP != b.HomeIP || a.HomePort != b.HomePort) &&
			latencySwapSignature(a, b) == retainedSignature &&
			latencyClientEligible(clients[a.CertificateFingerprint], a, now) && latencyClientEligible(clients[b.CertificateFingerprint], b, now) {
			aHome := HomeIncarnationID{IP: a.HomeIP, Port: a.HomePort, StartedAt: a.HomeStartedAt}
			bHome := HomeIncarnationID{IP: b.HomeIP, Port: b.HomePort, StartedAt: b.HomeStartedAt}
			_, aOK := latencyImproves(samples, a, bHome)
			_, bOK := latencyImproves(samples, b, aHome)
			if aOK && bOK {
				return a, b
			}
		}
	}

	type candidate struct {
		member *CPANodeMembershipRecord
		gain   time.Duration
	}
	bestByDirection := make(map[[2]string]candidate)
	for index := range members {
		member := &members[index]
		if !latencyClientEligible(clients[member.CertificateFingerprint], *member, now) {
			continue
		}
		sourceKey := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
		for targetKey, home := range homes {
			if sourceKey == targetKey {
				continue
			}
			if _, improves := latencyImproves(samples, *member, home.id); !improves {
				continue
			}
			gain, _, _ := latencyBenefit(samples, *member, home.id)
			direction := [2]string{sourceKey, targetKey}
			previous, exists := bestByDirection[direction]
			if !exists || gain > previous.gain || (gain == previous.gain && member.CertificateFingerprint < previous.member.CertificateFingerprint) {
				bestByDirection[direction] = candidate{member: member, gain: gain}
			}
		}
	}

	var a, b CPANodeMembershipRecord
	var best time.Duration
	for direction, forward := range bestByDirection {
		if direction[0] >= direction[1] {
			continue
		}
		reverse, exists := bestByDirection[[2]string{direction[1], direction[0]}]
		if !exists {
			continue
		}
		left, right := forward.member, reverse.member
		if left.CertificateFingerprint > right.CertificateFingerprint {
			left, right = right, left
		}
		gain := forward.gain + reverse.gain
		// Match the legacy fingerprint-ordered exhaustive search, independent of map iteration.
		if gain > best || (gain == best && (left.CertificateFingerprint < a.CertificateFingerprint ||
			(left.CertificateFingerprint == a.CertificateFingerprint && right.CertificateFingerprint < b.CertificateFingerprint))) {
			a, b, best = *left, *right, gain
		}
	}
	return a, b
}

func latencySwapSignature(a, b CPANodeMembershipRecord) string {
	aHome := HomeIncarnationID{IP: a.HomeIP, Port: a.HomePort, StartedAt: a.HomeStartedAt}
	bHome := HomeIncarnationID{IP: b.HomeIP, Port: b.HomePort, StartedAt: b.HomeStartedAt}
	return fmt.Sprintf("%s/%s/%d/%s/%s/%d", a.CertificateFingerprint, rebalanceHomeKey(aHome), a.ConnectedAt.UnixNano(), b.CertificateFingerprint, rebalanceHomeKey(bHome), b.ConnectedAt.UnixNano())
}

func latencyClientEligible(client CPARebalanceClientRecord, member CPANodeMembershipRecord, now time.Time) bool {
	return member.ProtocolVersion == 1 && client.ConnectedAt.Equal(member.ConnectedAt) && client.LatencyConnectedAt.Equal(member.ConnectedAt) &&
		!now.Before(client.NextEligibleAt) && !now.Before(client.OptimizationEligibleAt)
}

// validateSwapClaim checks the reciprocal reservation instead of weakening the ordinary count check.
func validateSwapClaim(tx *gorm.DB, plan CPARebalancePlanRecord, gate ClusterMasterGateRecord, now time.Time, homes map[string]rebalanceHome, members map[string]CPANodeMembershipRecord) (bool, error) {
	for _, home := range homes {
		if !home.capable || !home.latencyCapable {
			return false, nil
		}
	}
	var group []CPARebalancePlanRecord
	if errFind := tx.Where("swap_group_id = ?", plan.SwapGroupID).Find(&group).Error; errFind != nil {
		return false, errFind
	}
	if len(group) != 2 {
		return false, nil
	}
	for _, peer := range group {
		if peer.ID == plan.ID {
			continue
		}
		if peer.Term != gate.Term || peer.Fingerprint == plan.Fingerprint || rebalanceSource(peer) != rebalanceTarget(plan) || rebalanceTarget(peer) != rebalanceSource(plan) {
			return false, nil
		}
		if peer.State != rebalancePending && peer.State != rebalanceExecuting && peer.State != rebalanceCompleted {
			return false, nil
		}
		if peer.State != rebalanceCompleted && !now.Before(peer.ExpiresAt) {
			return false, nil
		}
		if peer.State == rebalanceExecuting {
			if member, exists := members[peer.Fingerprint]; exists {
				sourceMatches := membershipOwnedByHome(member, rebalanceSource(peer)) && member.ConnectedAt.Equal(peer.SourceConnectedAt)
				targetMatches := membershipOwnedByHome(member, rebalanceTarget(peer)) && member.ConnectedAt.After(peer.SourceConnectedAt)
				if !sourceMatches && !targetMatches {
					return false, nil
				}
			}
		}
		if peer.State == rebalanceCompleted {
			member, exists := members[peer.Fingerprint]
			if !exists || !membershipOwnedByHome(member, rebalanceTarget(peer)) || !member.ConnectedAt.After(peer.SourceConnectedAt) {
				return false, nil
			}
		}
		if peer.State == rebalancePending {
			member, exists := members[peer.Fingerprint]
			if !exists || !member.ConnectedAt.Equal(peer.SourceConnectedAt) || !membershipOwnedByHome(member, rebalanceSource(peer)) {
				return false, nil
			}
		}
	}
	return true, nil
}
