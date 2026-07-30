package node

import (
	"testing"
	"time"
)

func TestRegistryExportsFingerprintConnectionState(t *testing.T) {
	registry := NewRegistry()
	registry.UpdateFingerprintState("fp-a", "node-a", "127.0.0.1", 2, 3, 7)

	nodes := registry.List()
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(nodes))
	}
	if nodes[0].CertificateFingerprint != "fp-a" || nodes[0].OpenConnections != 2 || nodes[0].ActiveHandlers != 3 || nodes[0].LatestCancelRevision != 7 {
		t.Fatalf("node = %#v", nodes[0])
	}
}

func TestRegistryKeepsFingerprintStatesSeparateForSameNodeID(t *testing.T) {
	registry := NewRegistry()
	registry.UpdateFingerprintState("fp-a", "node-a", "127.0.0.1", 1, 0, 0)
	registry.UpdateFingerprintState("fp-b", "node-a", "127.0.0.1", 1, 0, 0)

	nodes := registry.List()
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2: %#v", len(nodes), nodes)
	}
	fingerprints := map[string]int{}
	for _, item := range nodes {
		fingerprints[item.CertificateFingerprint]++
	}
	if fingerprints["fp-a"] != 1 || fingerprints["fp-b"] != 1 {
		t.Fatalf("fingerprints = %#v", fingerprints)
	}
}

func TestRegistryRetainsFingerprintMetadataWhenOnlyRevisionChanges(t *testing.T) {
	registry := NewRegistry()
	registry.UpdateFingerprintState("fp-a", "node-a", "127.0.0.1", 1, 0, 0)
	registry.UpdateFingerprintState("fp-a", "", "", 0, 0, 4)

	nodes := registry.List()
	if len(nodes) != 1 || nodes[0].NodeID != "node-a" || nodes[0].IP != "127.0.0.1" || nodes[0].LatestCancelRevision != 4 {
		t.Fatalf("nodes = %#v", nodes)
	}
}

func TestRegistryKeepsOpenHandlerAfterSubscriptionDrains(t *testing.T) {
	registry := NewRegistry()
	registry.UpdateFingerprintSubscription("fp-a", "node-a", "127.0.0.1", 1)
	registry.UpdateFingerprintState("fp-a", "node-a", "127.0.0.1", 0, 1, 0)
	registry.UpdateFingerprintSubscription("fp-a", "node-a", "127.0.0.1", -1)

	nodes := registry.List()
	if len(nodes) != 1 || nodes[0].ClientCount != 0 || nodes[0].ActiveHandlers != 1 {
		t.Fatalf("nodes = %#v, want zero subscriptions with one open handler", nodes)
	}
}

func TestRegistryDropsFingerprintStateAfterConnectionsDrainDespiteRevision(t *testing.T) {
	registry := NewRegistry()
	registry.UpdateFingerprintState("fp-a", "node-a", "127.0.0.1", 1, 1, 9)
	registry.UpdateFingerprintState("fp-a", "node-a", "127.0.0.1", -1, -1, 9)

	nodes := registry.List()
	if len(nodes) != 0 {
		t.Fatalf("nodes = %#v, want revision-only state removed", nodes)
	}
}

func TestRegistryListDoesNotUseNodeKeyAsIP(t *testing.T) {
	registry := NewRegistry()
	connectedAt := time.Now().UTC()

	registry.AddWithNodeID("", "node-1", connectedAt)

	nodes := registry.List()
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(nodes))
	}
	if nodes[0].NodeID != "node-1" {
		t.Fatalf("node_id = %q, want node-1", nodes[0].NodeID)
	}
	if nodes[0].IP != "" {
		t.Fatalf("ip = %q, want empty when no client IP was recorded", nodes[0].IP)
	}
	if nodes[0].ClientCount != 1 {
		t.Fatalf("client_count = %d, want 1", nodes[0].ClientCount)
	}
}
