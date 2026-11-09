package auth

import (
	"crypto/sha256"
	"time"
)

// Request counters are process-local. Bound their execution replay history just
// like weighted accounting; neither cache is a durable usage or billing ledger.
const maxUsageRequestStatsEvents = 65536

type usageRequestStatsCache struct {
	seen  map[[32]byte]struct{}
	order [][32]byte
	next  int
}

// recordResultRequestStatsLocked updates replay protection and statistics under
// the same manager lock. A later transition failure must not undo this marker.
// Missing credentials never reach this point and remain countable after loading.
func (m *Manager) recordResultRequestStatsLocked(auth *Auth, result Result, now time.Time) {
	if result.RequestStatsEventID == "" {
		if result.SkipRequestStats {
			return
		}
	} else {
		key := sha256.Sum256([]byte(result.RequestStatsEventID))
		if _, seen := m.usageStats.seen[key]; seen {
			return
		}
		if m.usageStats.seen == nil {
			m.usageStats.seen = make(map[[32]byte]struct{})
		}
		if len(m.usageStats.order) < maxUsageRequestStatsEvents {
			m.usageStats.order = append(m.usageStats.order, key)
		} else {
			delete(m.usageStats.seen, m.usageStats.order[m.usageStats.next])
			m.usageStats.order[m.usageStats.next] = key
			m.usageStats.next = (m.usageStats.next + 1) % maxUsageRequestStatsEvents
		}
		m.usageStats.seen[key] = struct{}{}
	}
	auth.recordRecentRequest(now, result.Success)
	if result.Success {
		auth.Success++
	} else {
		auth.Failed++
	}
}
