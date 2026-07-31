package management

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
)

const (
	pluginInstallStatusFailed    = "failed"
	pluginInstallStatusInstalled = "installed"
	pluginInstallStatusSkipped   = "skipped"
	pluginLoadStatusFailed       = "failed"
	pluginLoadStatusLoaded       = "loaded"
)

type activeCPANode struct {
	NodeID      string
	IP          string
	Connected   time.Time
	ClientCount int
	HomeIP      string
	HomePort    int
	LastSeenAt  time.Time
	Health      string
}

// ListNodes returns a nodes.
func (h *Handler) ListNodes(c *gin.Context) {
	if c == nil {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	taskStatuses, errStatuses := h.pluginTaskStatuses(ctx)
	if errStatuses != nil {
		respondError(c, http.StatusInternalServerError, "plugin_status_load_failed", errStatuses)
		return
	}
	statusesByNodeID, statusesByIP := pluginStatusesByNode(taskStatuses)
	requiredPluginIDs := h.pluginTaskRequiredIDs(ctx)
	activeNodes, errNodes := h.activeCPANodes(ctx)
	if errNodes != nil {
		respondError(c, http.StatusInternalServerError, "node_load_failed", errNodes)
		return
	}
	nodes := make([]gin.H, 0, len(activeNodes))
	for _, activeNode := range activeNodes {
		statuses := statusesByNodeID[strings.TrimSpace(activeNode.NodeID)]
		if len(statuses) == 0 {
			statuses = statusesByIP[activeNode.IP]
		}
		state := pluginReportState(statuses, requiredPluginIDs)
		homeID := topologyHomeID(activeNode.HomeIP, activeNode.HomePort)
		nodes = append(nodes, gin.H{
			"node_id":                activeNode.NodeID,
			"ip":                     activeNode.IP,
			"connected_time":         activeNode.Connected,
			"last_seen_at":           activeNode.LastSeenAt,
			"client_count":           activeNode.ClientCount,
			"healthy":                activeNode.Health == topologyHealthHealthy,
			"home_id":                homeID,
			"home_ip":                activeNode.HomeIP,
			"home_port":              activeNode.HomePort,
			"plugin_report_state":    state,
			"plugin_report_statuses": statuses,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"nodes":                  nodes,
		"plugin_report_required": len(requiredPluginIDs) > 0,
		"plugin_report_statuses": taskStatuses,
	})
}

func (h *Handler) activeCPANodes(ctx context.Context) ([]activeCPANode, error) {
	nodesByKey := make(map[string]activeCPANode)
	if h != nil && h.repo != nil {
		now, errNow := h.repo.CurrentDatabaseTime(ctx)
		if errNow != nil {
			return nil, errNow
		}
		cutoff := time.Time{}
		if h.heartbeatTimeout > 0 {
			cutoff = now.Add(-topologySnapshotRetention(h.heartbeatTimeout))
		}
		records, errRecords := h.repo.ListCPANodeSnapshots(ctx, cutoff)
		if errRecords != nil {
			return nil, errRecords
		}
		memberships, errMemberships := h.repo.ListActiveCPAMemberships(ctx)
		if errMemberships != nil {
			return nil, errMemberships
		}
		membershipsByFingerprint := make(map[string]cluster.CPANodeMembershipRecord, len(memberships))
		for _, membership := range memberships {
			membershipsByFingerprint[strings.TrimSpace(membership.CertificateFingerprint)] = membership
		}
		homeRecords, errHomes := h.repo.ListClusterNodes(ctx, cutoff)
		if errHomes != nil {
			return nil, errHomes
		}
		healthCutoff := now.Add(-h.heartbeatTimeout)
		homeHealth := make(map[string]string, len(homeRecords))
		for _, homeRecord := range homeRecords {
			homeKey := topologyHomeIncarnationKey(homeRecord.IP, homeRecord.Port, homeRecord.StartedAt)
			homeHealth[homeKey] = topologyHealth(homeRecord.LastSeenAt, healthCutoff)
		}
		for _, record := range records {
			fingerprint := strings.TrimSpace(record.CertificateFingerprint)
			membership, hasMembership := membershipsByFingerprint[fingerprint]
			if topologyCPASnapshotState(record, membership, hasMembership) != topologyCPAActive {
				continue
			}
			homeKey := topologyHomeIncarnationKey(record.HomeIP, record.HomePort, record.HomeStartedAt)
			key := fingerprint
			if key == "" {
				continue
			}
			nodesByKey[key] = activeCPANode{
				NodeID:      strings.TrimSpace(record.NodeID),
				IP:          strings.TrimSpace(record.ClientIP),
				Connected:   record.ConnectedAt,
				ClientCount: record.ClientCount,
				HomeIP:      strings.TrimSpace(record.HomeIP),
				HomePort:    record.HomePort,
				LastSeenAt:  record.LastSeenAt,
				Health:      topologyCPASnapshotHealth(topologyCPAActive, membership, homeHealth[homeKey], now),
			}
		}
	}

	nodes := make([]activeCPANode, 0, len(nodesByKey))
	for _, item := range nodesByKey {
		nodes = append(nodes, item)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Connected.Equal(nodes[j].Connected) {
			if nodes[i].IP == nodes[j].IP {
				if nodes[i].NodeID == nodes[j].NodeID {
					if nodes[i].HomeIP == nodes[j].HomeIP {
						return nodes[i].HomePort < nodes[j].HomePort
					}
					return nodes[i].HomeIP < nodes[j].HomeIP
				}
				return nodes[i].NodeID < nodes[j].NodeID
			}
			return nodes[i].IP < nodes[j].IP
		}
		return nodes[i].Connected.Before(nodes[j].Connected)
	})
	return nodes, nil
}

func (h *Handler) pluginTaskStatuses(ctx context.Context) ([]node.PluginTaskStatus, error) {
	if h == nil || h.repo == nil {
		return nil, nil
	}
	return h.repo.ListPluginStatuses(ctx, node.PluginStatusNodeTypeCPA)
}

func (h *Handler) pluginTaskRequiredIDs(ctx context.Context) []string {
	cfg, _, errConfig := h.currentConfig(ctx)
	if errConfig != nil || cfg == nil || !cfg.Plugins.Enabled {
		return nil
	}
	ids := make([]string, 0, len(cfg.Plugins.Configs))
	for id, item := range cfg.Plugins.Configs {
		if !pluginInstanceEnabled(item) {
			continue
		}
		manifest, configured := configuredPluginStoreManifest(id, cfg)
		if !configured {
			continue
		}
		pluginID := strings.TrimSpace(manifest.ID)
		if pluginID == "" {
			pluginID = strings.TrimSpace(id)
		}
		if pluginID != "" {
			ids = append(ids, pluginID)
		}
	}
	sort.Strings(ids)
	return ids
}

func pluginReportState(statuses []node.PluginTaskStatus, requiredPluginIDs []string) string {
	if len(requiredPluginIDs) == 0 {
		return "not_required"
	}
	if len(statuses) == 0 {
		return "missing_report"
	}

	plugins := make(map[string]node.PluginTaskPlugin)
	seen := make(map[string]struct{})
	for _, status := range statuses {
		for _, plugin := range status.Plugins {
			id := strings.TrimSpace(plugin.ID)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			plugins[id] = plugin
			seen[id] = struct{}{}
		}
	}

	for _, status := range statuses {
		if len(status.Plugins) == 0 && (!status.OK || !strings.EqualFold(strings.TrimSpace(status.Status), "success")) {
			return "reported_failed"
		}
	}

	for _, id := range requiredPluginIDs {
		plugin, ok := plugins[id]
		if !ok {
			return "reported_partial"
		}
		state := pluginReportPluginState(plugin)
		if state == "reported_failed" {
			return "reported_failed"
		}
		if state != "reported_ok" {
			return "reported_partial"
		}
	}
	return "reported_ok"
}

func pluginReportPluginState(plugin node.PluginTaskPlugin) string {
	if strings.TrimSpace(plugin.Error) != "" {
		return "reported_failed"
	}
	installStatus := strings.ToLower(strings.TrimSpace(plugin.InstallStatus))
	loadStatus := strings.ToLower(strings.TrimSpace(plugin.LoadStatus))
	if installStatus == pluginInstallStatusFailed || loadStatus == pluginLoadStatusFailed {
		return "reported_failed"
	}
	if installStatus != pluginInstallStatusInstalled && installStatus != pluginInstallStatusSkipped {
		return "reported_partial"
	}
	if loadStatus != pluginLoadStatusLoaded {
		return "reported_partial"
	}
	return "reported_ok"
}
