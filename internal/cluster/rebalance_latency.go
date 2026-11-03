package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	rebalanceLatencyCapability = "cpa_latency_swap_v1"
	rebalanceLatencyTTL        = 3 * time.Minute
	rebalanceLatencyCooldown   = 10 * time.Minute
	rebalanceLatencyMinimum    = 20 * time.Millisecond
)

// CPALatencyRecord stores only the latest summary, never individual heartbeats.
type CPALatencyRecord struct {
	Fingerprint    string `gorm:"primaryKey"`
	Address        string `gorm:"primaryKey"`
	ProcessID      string
	Sequence       int64
	ObservedAt     time.Time
	SampledAt      time.Time `gorm:"index"`
	RTT            time.Duration
	Valid          bool
	FailureBackoff time.Duration
	ProbeRetryAt   time.Time
	UpdatedAt      time.Time `gorm:"index;autoUpdateTime:false"`
}

func (CPALatencyRecord) TableName() string { return "cpa_latency_summaries" }

type latencySample struct {
	IP         string    `json:"ip"`
	Port       int       `json:"port"`
	Sequence   int64     `json:"sequence"`
	ObservedAt time.Time `json:"observed_at"`
	AgeMS      int64     `json:"age_ms"`
	RTTUS      int64     `json:"rtt_us"`
	Valid      bool      `json:"valid"`
}

type latencyReport struct {
	ProcessID string          `json:"process_id"`
	Samples   []latencySample `json:"samples"`
}

func latencyAddress(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }

func latencyKey(fingerprint, ip string, port int) string {
	return fingerprint + "/" + latencyAddress(ip, port)
}

func loadRebalanceLatencies(tx *gorm.DB, now time.Time) (map[string]CPALatencyRecord, error) {
	var records []CPALatencyRecord
	if errFind := tx.Where("sampled_at >= ?", now.Add(-rebalanceLatencyTTL)).Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	result := make(map[string]CPALatencyRecord, len(records))
	for _, record := range records {
		if record.Valid && record.RTT > 0 && !record.SampledAt.After(now) {
			result[record.Fingerprint+"/"+record.Address] = record
		}
	}
	return result, nil
}

func latencyBenefit(samples map[string]CPALatencyRecord, member CPANodeMembershipRecord, target HomeIncarnationID) (time.Duration, time.Time, bool) {
	source, sourceOK := samples[latencyKey(member.CertificateFingerprint, member.HomeIP, member.HomePort)]
	destination, targetOK := samples[latencyKey(member.CertificateFingerprint, target.IP, target.Port)]
	if !sourceOK || !targetOK {
		return 0, time.Time{}, false
	}
	return source.RTT - destination.RTT, minTime(source.SampledAt, destination.SampledAt), true
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func latencyImproves(samples map[string]CPALatencyRecord, member CPANodeMembershipRecord, target HomeIncarnationID) (time.Time, bool) {
	gain, evidence, valid := latencyBenefit(samples, member, target)
	source := samples[latencyKey(member.CertificateFingerprint, member.HomeIP, member.HomePort)]
	return evidence, valid && gain >= rebalanceLatencyMinimum && float64(gain) >= float64(source.RTT)*0.30
}

// acceptLatencyReport requires the active lifetime and this CPA's rebalance client row to be locked.
// Reports do not acquire the cluster election gate.
// Age and observation time both bound freshness; resending a sample cannot renew it.
func acceptLatencyReport(tx *gorm.DB, lifetime ConnectionLifetime, raw string, now time.Time, homes map[string]rebalanceHome, client CPARebalanceClientRecord) (*RebalanceTarget, error) {
	if len(raw) > 32768 {
		return nil, fmt.Errorf("cluster latency: report too large")
	}
	var report latencyReport
	if errJSON := json.Unmarshal([]byte(raw), &report); errJSON != nil {
		return nil, errJSON
	}
	if report.ProcessID == "" || len(report.ProcessID) > 128 || len(report.Samples) > 64 {
		return nil, fmt.Errorf("cluster latency: invalid report")
	}
	addresses := make(map[string]bool, len(homes))
	for _, home := range homes {
		addresses[latencyAddress(home.id.IP, home.id.Port)] = true
	}
	var history []CPALatencyRecord
	if errFind := tx.Where("fingerprint = ?", lifetime.Fingerprint).Find(&history).Error; errFind != nil {
		return nil, errFind
	}
	byAddress := make(map[string]CPALatencyRecord, len(history))
	for _, record := range history {
		byAddress[record.Address] = record
	}
	changed := make(map[string]CPALatencyRecord)
	for _, sample := range report.Samples {
		address := latencyAddress(sample.IP, sample.Port)
		if !addresses[address] || sample.Sequence <= 0 || sample.AgeMS < 0 || sample.AgeMS > rebalanceLatencyTTL.Milliseconds() || sample.ObservedAt.IsZero() || sample.ObservedAt.After(now.Add(5*time.Second)) || sample.RTTUS < 0 || sample.RTTUS > time.Minute.Microseconds() || (sample.Valid && sample.RTTUS == 0) {
			continue
		}
		sampledAt := minTime(now.Add(-time.Duration(sample.AgeMS)*time.Millisecond), sample.ObservedAt)
		if sampledAt.Before(now.Add(-rebalanceLatencyTTL)) {
			continue
		}
		record := CPALatencyRecord{Fingerprint: lifetime.Fingerprint, Address: address, ProcessID: report.ProcessID, Sequence: sample.Sequence, ObservedAt: sample.ObservedAt, SampledAt: sampledAt, RTT: time.Duration(sample.RTTUS) * time.Microsecond, Valid: sample.Valid, UpdatedAt: now}
		previous, exists := byAddress[address]
		if exists && ((previous.ProcessID == report.ProcessID && sample.Sequence <= previous.Sequence) || !sample.ObservedAt.After(previous.ObservedAt)) {
			continue
		}
		if !record.Valid {
			record.FailureBackoff = min(max(time.Minute, previous.FailureBackoff*2), 10*time.Minute)
			record.ProbeRetryAt = sampledAt.Add(record.FailureBackoff)
		}
		byAddress[address], changed[address] = record, record
	}
	if len(changed) > 0 {
		records := make([]CPALatencyRecord, 0, len(changed))
		for _, record := range changed {
			records = append(records, record)
		}
		if errCreate := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "fingerprint"}, {Name: "address"}}, UpdateAll: true}).Create(&records).Error; errCreate != nil {
			return nil, errCreate
		}
	}
	if !client.LatencyConnectedAt.Equal(lifetime.ConnectedAt) {
		if errUpdate := tx.Model(&CPARebalanceClientRecord{}).Where("fingerprint = ? AND connected_at = ?", lifetime.Fingerprint, lifetime.ConnectedAt).Updates(map[string]any{"latency_connected_at": lifetime.ConnectedAt, "updated_at": now}).Error; errUpdate != nil {
			return nil, errUpdate
		}
	}
	// Ask for at most one communication-only probe, and only when a meaningful gain is possible.
	if now.Before(client.NextEligibleAt) || now.Before(client.OptimizationEligibleAt) {
		return nil, nil
	}
	samples := make(map[string]CPALatencyRecord, len(byAddress))
	for _, record := range byAddress {
		if record.Valid && record.RTT > 0 && !record.SampledAt.After(now) && !record.SampledAt.Before(now.Add(-rebalanceLatencyTTL)) {
			samples[record.Fingerprint+"/"+record.Address] = record
		}
	}
	current, valid := samples[latencyKey(lifetime.Fingerprint, lifetime.Home.IP, lifetime.Home.Port)]
	if !valid || current.RTT < rebalanceLatencyMinimum || now.Before(current.SampledAt) {
		return nil, nil
	}
	for _, home := range homes {
		if !home.capable || !home.latencyCapable {
			return nil, nil
		}
	}
	promising := func(home HomeIncarnationID) bool {
		sample, exists := samples[latencyKey(lifetime.Fingerprint, home.IP, home.Port)]
		gain := current.RTT - sample.RTT
		return exists && gain >= rebalanceLatencyMinimum && float64(gain) >= float64(current.RTT)*0.30
	}
	var candidates []HomeIncarnationID
	for _, home := range homes {
		if home.id.IP == lifetime.Home.IP && home.id.Port == lifetime.Home.Port {
			continue
		}
		record := byAddress[latencyAddress(home.id.IP, home.id.Port)]
		if now.Before(record.ProbeRetryAt) || (now.Sub(record.SampledAt) < rebalanceLatencyTTL && record.Valid && !promising(home.id)) {
			continue
		}
		candidates = append(candidates, home.id)
	}
	preferred := ""
	if len(candidates) > 1 {
		// Probe results arrive in the next report. Alternating equally promising targets
		// can therefore leave a two-minute sample expired before its replacement arrives.
		// This is an unlocked snapshot: telemetry must not wait for the planner's locks.
		var confirmation CPALatencyConfirmationRecord
		errFirst := tx.First(&confirmation, "id = ?", "active").Error
		if errFirst != nil && !errors.Is(errFirst, gorm.ErrRecordNotFound) {
			return nil, errFirst
		}
		if errFirst == nil && !confirmation.UpdatedAt.After(now) && now.Sub(confirmation.UpdatedAt) <= rebalanceLatencyTTL {
			identity := fmt.Sprintf("%s/%s/%d", lifetime.Fingerprint, rebalanceHomeKey(lifetime.Home), lifetime.ConnectedAt.UnixNano())
			peer, participant := strings.CutPrefix(confirmation.Signature, identity+"/")
			if !participant {
				peer, participant = strings.CutSuffix(confirmation.Signature, "/"+identity)
			}
			if participant {
				for _, candidate := range candidates {
					key := rebalanceHomeKey(candidate)
					// Match the peer's live Home incarnation within its persisted identity;
					// no extra probe is authorized, and failure backoff still applies above.
					if strings.Contains(peer, "/"+key+"/") {
						preferred = key
						break
					}
				}
			}
		}
	}
	// Reserve one minute out of four for oldest-first exploration. The other three
	// keep the most recently sampled promising target stable while delayed, staggered
	// reports reach the planner. Pending confirmations always override this schedule.
	explore := now.Unix()/int64(time.Minute/time.Second)%4 == 0
	sort.Slice(candidates, func(i, j int) bool {
		leftPreferred, rightPreferred := rebalanceHomeKey(candidates[i]) == preferred, rebalanceHomeKey(candidates[j]) == preferred
		if leftPreferred != rightPreferred {
			return leftPreferred
		}
		leftPromising, rightPromising := promising(candidates[i]), promising(candidates[j])
		if !explore && leftPromising != rightPromising {
			return leftPromising
		}
		a := byAddress[latencyAddress(candidates[i].IP, candidates[i].Port)].SampledAt
		b := byAddress[latencyAddress(candidates[j].IP, candidates[j].Port)].SampledAt
		if a.Equal(b) {
			return rebalanceHomeKey(candidates[i]) < rebalanceHomeKey(candidates[j])
		}
		if !explore && leftPromising {
			return a.After(b)
		}
		return a.Before(b)
	})
	if len(candidates) == 0 {
		return nil, nil
	}
	target := candidates[0]
	if sample, exists := samples[latencyKey(lifetime.Fingerprint, target.IP, target.Port)]; exists && now.Sub(sample.SampledAt) < 30*time.Second {
		return nil, nil
	}
	return &RebalanceTarget{IP: target.IP, Port: target.Port}, nil
}
