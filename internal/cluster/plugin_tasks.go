package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/gorm"
)

// CreatePluginTask stores a plugin task and emits a cluster event to wake peers.
func (r *Repository) CreatePluginTask(ctx context.Context, task node.PluginTask) (node.PluginTask, error) {
	db, errDB := r.database()
	if errDB != nil {
		return node.PluginTask{}, errDB
	}
	task, errTask := normalizePluginTask(task)
	if errTask != nil {
		return node.PluginTask{}, errTask
	}

	ctx = contextOrBackground(ctx)
	var out node.PluginTask
	if errCreate := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var errCreateTask error
		out, errCreateTask = createPluginTaskTx(tx, task)
		return errCreateTask
	}); errCreate != nil {
		return node.PluginTask{}, errCreate
	}
	return out, nil
}

// ReplaceConfigSnapshotAndCreatePluginTask replaces config and creates a plugin task atomically.
func (r *Repository) ReplaceConfigSnapshotAndCreatePluginTask(ctx context.Context, values map[string]any, task node.PluginTask) (node.PluginTask, error) {
	nodeHeartbeatTimeout, errTimeout := r.lifecycleConfigNodeHeartbeatTimeout(ctx)
	if errTimeout != nil {
		return node.PluginTask{}, errTimeout
	}
	return r.ReplaceConfigSnapshotWithLifecycleConfigAndCreatePluginTask(ctx, nodeHeartbeatTimeout, values, task)
}

// ReplaceConfigSnapshotWithLifecycleConfigAndCreatePluginTask applies lifecycle config before replacing a snapshot and creating a task.
func (r *Repository) ReplaceConfigSnapshotWithLifecycleConfigAndCreatePluginTask(ctx context.Context, nodeHeartbeatTimeout time.Duration, values map[string]any, task node.PluginTask) (node.PluginTask, error) {
	db, errDB := r.database()
	if errDB != nil {
		return node.PluginTask{}, errDB
	}
	task, errTask := normalizePluginTask(task)
	if errTask != nil {
		return node.PluginTask{}, errTask
	}
	ctx = contextOrBackground(ctx)
	lifecycleValue, lifecycleProvided := values["credential-concurrency"]
	var nextLifecycle config.CredentialConcurrencyConfig
	if lifecycleProvided {
		var errConfig error
		nextLifecycle, errConfig = credentialConcurrencyConfigFromValue(lifecycleValue)
		if errConfig != nil {
			return node.PluginTask{}, errConfig
		}
	}
	var out node.PluginTask
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		gate, errGate := lockConcurrencyActivationGate(tx)
		if errGate != nil {
			return errGate
		}
		if lifecycleProvided {
			if _, _, errUpdate := updateLifecycleConfigTx(ctx, tx, nodeHeartbeatTimeout, nextLifecycle); errUpdate != nil {
				return errUpdate
			}
		} else if _, errEnsure := ensureLifecycleConfigTx(tx, nodeHeartbeatTimeout); errEnsure != nil {
			return errEnsure
		}
		if errReconcile := r.reconcileConfigSnapshotProviderAuthsTx(ctx, tx, values); errReconcile != nil {
			return mapCredentialConcurrencyOrphan(errReconcile)
		}
		if errImportPolicies := r.importCredentialConcurrencyPoliciesWithLockedActivationGateTx(ctx, tx, gate, values); errImportPolicies != nil {
			return errImportPolicies
		}
		apiKeys, clean, errSnapshot := prepareConfigSnapshotReplace(values)
		if errSnapshot != nil {
			return errSnapshot
		}
		if errReplace := replaceConfigSnapshotTx(ctx, tx, apiKeys, clean); errReplace != nil {
			return errReplace
		}
		var errCreateTask error
		out, errCreateTask = createPluginTaskTx(tx, task)
		return errCreateTask
	})
	if errTransaction != nil {
		return node.PluginTask{}, errTransaction
	}
	return out, nil
}

func normalizePluginTask(task node.PluginTask) (node.PluginTask, error) {
	operation, errOperation := normalizePluginTaskOperation(task.Operation)
	if errOperation != nil {
		return node.PluginTask{}, errOperation
	}
	pluginID := strings.TrimSpace(task.PluginID)
	if pluginID == "" {
		return node.PluginTask{}, fmt.Errorf("plugin task plugin id is required")
	}
	targetNodeType, targetNodeID, errTarget := normalizePluginTaskTarget(task.TargetNodeType, task.TargetNodeID)
	if errTarget != nil {
		return node.PluginTask{}, errTarget
	}
	return node.PluginTask{
		ID:             task.ID,
		Operation:      operation,
		PluginID:       pluginID,
		TargetNodeType: targetNodeType,
		TargetNodeID:   targetNodeID,
	}, nil
}

func createPluginTaskTx(tx *gorm.DB, task node.PluginTask) (node.PluginTask, error) {
	if tx == nil {
		return node.PluginTask{}, fmt.Errorf("database connection is nil")
	}
	record := PluginTaskRecord{
		Operation:      task.Operation,
		PluginID:       task.PluginID,
		TargetNodeType: task.TargetNodeType,
		TargetNodeID:   task.TargetNodeID,
	}
	if errInsert := tx.Create(&record).Error; errInsert != nil {
		return node.PluginTask{}, errInsert
	}
	if errEvent := appendEvent(tx, "plugin-task", task.Operation, task.PluginID, int64(record.ID)); errEvent != nil {
		return node.PluginTask{}, errEvent
	}
	return pluginTaskFromRecord(record), nil
}

// ListPendingPluginTasks returns tasks that the given node has not acked yet.
func (r *Repository) ListPendingPluginTasks(ctx context.Context, nodeType string, nodeID string) ([]node.PluginTask, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	nodeType, errType := normalizePluginStatusNodeType(nodeType)
	if errType != nil {
		return nil, errType
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, fmt.Errorf("plugin task node id is required")
	}

	var records []PluginTaskRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Where("operation = ?", node.PluginTaskOperationDelete).
		Where("(target_node_type = ? OR target_node_type = ? OR target_node_type = '')", node.PluginTaskTargetAll, nodeType).
		Where("(target_node_id = '' OR target_node_id = ?)", nodeID).
		Order("id").
		Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	if len(records) == 0 {
		return []node.PluginTask{}, nil
	}

	taskIDs := make([]uint, 0, len(records))
	for _, record := range records {
		taskIDs = append(taskIDs, record.ID)
	}
	var ackRecords []PluginStatusRecord
	ackPluginIDs := make([]string, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		if ackPluginID := pluginStatusAckPluginID(taskID); ackPluginID != "" {
			ackPluginIDs = append(ackPluginIDs, ackPluginID)
		}
	}
	if errAcks := db.WithContext(contextOrBackground(ctx)).
		Where("node_type = ? AND node_id = ? AND plugin_id IN ?", nodeType, nodeID, ackPluginIDs).
		Find(&ackRecords).Error; errAcks != nil {
		return nil, errAcks
	}
	acked := make(map[uint]struct{}, len(ackRecords))
	for _, ack := range ackRecords {
		if ack.TaskID == 0 {
			continue
		}
		acked[ack.TaskID] = struct{}{}
	}

	tasks := make([]node.PluginTask, 0, len(records))
	for _, record := range records {
		if _, ok := acked[record.ID]; ok {
			continue
		}
		tasks = append(tasks, pluginTaskFromRecord(record))
	}
	return tasks, nil
}

func normalizePluginTaskOperation(operation string) (string, error) {
	operation = strings.ToLower(strings.TrimSpace(operation))
	switch operation {
	case node.PluginTaskOperationDelete:
		return operation, nil
	default:
		return "", fmt.Errorf("unsupported plugin task operation %q", operation)
	}
}

func normalizePluginTaskTarget(targetNodeType string, targetNodeID string) (string, string, error) {
	targetNodeID = strings.TrimSpace(targetNodeID)
	targetNodeType = strings.ToLower(strings.TrimSpace(targetNodeType))
	if targetNodeType == "" {
		if targetNodeID == "" {
			targetNodeType = node.PluginTaskTargetAll
		} else {
			targetNodeType = node.PluginStatusNodeTypeCPA
		}
	}
	if targetNodeType == node.PluginTaskTargetAll {
		if targetNodeID != "" {
			return "", "", fmt.Errorf("plugin task target node id must be empty for all targets")
		}
		return targetNodeType, "", nil
	}
	normalizedType, errType := normalizePluginStatusNodeType(targetNodeType)
	if errType != nil {
		return "", "", errType
	}
	if targetNodeID == "" {
		return "", "", fmt.Errorf("plugin task target node id is required")
	}
	return normalizedType, targetNodeID, nil
}

func pluginTaskFromRecord(record PluginTaskRecord) node.PluginTask {
	return node.PluginTask{
		ID:             record.ID,
		Operation:      strings.TrimSpace(record.Operation),
		PluginID:       strings.TrimSpace(record.PluginID),
		TargetNodeType: strings.TrimSpace(record.TargetNodeType),
		TargetNodeID:   strings.TrimSpace(record.TargetNodeID),
		CreatedAt:      record.CreatedAt.UTC(),
		UpdatedAt:      record.UpdatedAt.UTC(),
	}
}
