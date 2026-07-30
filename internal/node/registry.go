package node

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Node struct {
	NodeID                 string    `json:"node_id,omitempty"`
	IP                     string    `json:"ip"`
	Connected              time.Time `json:"connected_time"`
	ClientCount            int       `json:"client_count"`
	CertificateFingerprint string    `json:"certificate_fingerprint,omitempty"`
	OpenConnections        int       `json:"open_connections"`
	ActiveHandlers         int       `json:"active_handlers"`
	LatestCancelRevision   int64     `json:"latest_cancel_revision"`
}

type Registry struct {
	mu    sync.RWMutex
	nodes map[string]nodeEntry
}

type nodeEntry struct {
	nodeID                 string
	ip                     string
	connectedAt            time.Time
	count                  int
	certificateFingerprint string
	openConnections        int
	activeHandlers         int
	latestCancelRevision   int64
}

// NewRegistry creates a new registry.
func NewRegistry() *Registry {
	return &Registry{
		nodes: make(map[string]nodeEntry),
	}
}

// Add adds the value.
func (r *Registry) Add(ip string, connectedAt time.Time) {
	r.AddWithNodeID(ip, "", connectedAt)
}

// AddWithNodeID adds the value with an optional node id.
func (r *Registry) AddWithNodeID(ip string, nodeID string, connectedAt time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	if r == nil {
		return
	}
	ip = strings.TrimSpace(ip)
	nodeID = strings.TrimSpace(nodeID)
	key := nodeRegistryKey(ip, nodeID)
	if key == "" {
		return
	}
	if connectedAt.IsZero() {
		connectedAt = time.Now()
	}
	r.mu.Lock()
	if r.nodes == nil {
		r.nodes = make(map[string]nodeEntry)
	}
	entry := r.nodes[key]
	if entry.count <= 0 {
		entry.nodeID = nodeID
		entry.ip = ip
		entry.connectedAt = connectedAt
		entry.count = 1
		r.nodes[key] = entry
		r.mu.Unlock()
		return
	}
	if nodeID != "" {
		entry.nodeID = nodeID
	}
	if ip != "" {
		entry.ip = ip
	}
	entry.count++
	r.nodes[key] = entry
	r.mu.Unlock()
}

// Remove removes the value.
// UpdateFingerprintState applies local connection and handler deltas for a certificate fingerprint.
func (r *Registry) UpdateFingerprintState(fingerprint string, nodeID string, ip string, openConnections int, activeHandlers int, revision int64) {
	r.updateFingerprintState(fingerprint, nodeID, ip, 0, openConnections, activeHandlers, revision)
}

// UpdateFingerprintSubscription applies a subscription delta for a certificate fingerprint.
func (r *Registry) UpdateFingerprintSubscription(fingerprint string, nodeID string, ip string, subscriptions int) {
	r.updateFingerprintState(fingerprint, nodeID, ip, subscriptions, 0, 0, 0)
}

func (r *Registry) updateFingerprintState(fingerprint string, nodeID string, ip string, subscriptions int, openConnections int, activeHandlers int, revision int64) {
	if r == nil {
		return
	}
	fingerprint = strings.TrimSpace(fingerprint)
	nodeID = strings.TrimSpace(nodeID)
	ip = strings.TrimSpace(ip)
	if fingerprint == "" {
		return
	}
	key := "fingerprint:" + fingerprint
	r.mu.Lock()
	if r.nodes == nil {
		r.nodes = make(map[string]nodeEntry)
	}
	entry := r.nodes[key]
	if nodeID != "" {
		entry.nodeID = nodeID
	}
	if ip != "" {
		entry.ip = ip
	}
	if entry.connectedAt.IsZero() {
		entry.connectedAt = time.Now()
	}
	entry.certificateFingerprint = fingerprint
	entry.count += subscriptions
	if entry.count < 0 {
		entry.count = 0
	}
	entry.openConnections += openConnections
	if entry.openConnections < 0 {
		entry.openConnections = 0
	}
	entry.activeHandlers += activeHandlers
	if entry.activeHandlers < 0 {
		entry.activeHandlers = 0
	}
	if revision > entry.latestCancelRevision {
		entry.latestCancelRevision = revision
	}
	if entry.count == 0 && entry.openConnections == 0 && entry.activeHandlers == 0 {
		delete(r.nodes, key)
	} else {
		r.nodes[key] = entry
	}
	r.mu.Unlock()
}

func (r *Registry) Remove(ip string) {
	r.RemoveWithNodeID(ip, "")
}

// RemoveWithNodeID removes the value with an optional node id.
func (r *Registry) RemoveWithNodeID(ip string, nodeID string) {
	// Keep validation before state changes so failures leave existing data intact.
	if r == nil {
		return
	}
	ip = strings.TrimSpace(ip)
	nodeID = strings.TrimSpace(nodeID)
	key := nodeRegistryKey(ip, nodeID)
	if key == "" {
		return
	}
	r.mu.Lock()
	entry, ok := r.nodes[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	entry.count--
	if entry.count <= 0 {
		delete(r.nodes, key)
		r.mu.Unlock()
		return
	}
	r.nodes[key] = entry
	r.mu.Unlock()
}

// List returns the available entries.
func (r *Registry) List() []Node {
	// Keep validation before state changes so failures leave existing data intact.
	if r == nil {
		return nil
	}
	r.mu.RLock()
	snapshot := make([]Node, 0, len(r.nodes))
	for _, entry := range r.nodes {
		snapshot = append(snapshot, Node{
			NodeID:                 entry.nodeID,
			IP:                     strings.TrimSpace(entry.ip),
			Connected:              entry.connectedAt,
			ClientCount:            entry.count,
			CertificateFingerprint: entry.certificateFingerprint,
			OpenConnections:        entry.openConnections,
			ActiveHandlers:         entry.activeHandlers,
			LatestCancelRevision:   entry.latestCancelRevision,
		})
	}
	r.mu.RUnlock()

	sort.Slice(snapshot, func(i, j int) bool {
		if snapshot[i].Connected.Equal(snapshot[j].Connected) {
			return snapshot[i].IP < snapshot[j].IP
		}
		return snapshot[i].Connected.Before(snapshot[j].Connected)
	})

	return snapshot
}

// TotalCount returns the current number of active client connections.
func (r *Registry) TotalCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	total := 0
	for _, entry := range r.nodes {
		if entry.count > 0 {
			total += entry.count
		}
	}
	r.mu.RUnlock()
	return total
}

func nodeRegistryKey(ip string, nodeID string) string {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID != "" {
		return "node:" + nodeID
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	return "ip:" + ip
}
