package cluster

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultHeartbeatInterval = 5 * time.Second
	defaultHeartbeatTimeout  = 20 * time.Second
)

// DefaultHeartbeatTimeout returns the default cluster heartbeat timeout.
func DefaultHeartbeatTimeout() time.Duration {
	return defaultHeartbeatTimeout
}

// StartupRecovery completes pending startup work before listeners accept traffic.
type StartupRecovery func(context.Context, HomeIncarnationID) error

type CoordinatorOptions struct {
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	OnMasterChanged   func(bool)
	StartupRecovery   StartupRecovery
}

type Coordinator struct {
	repo              *Repository
	node              NodeIdentity
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	onMasterChanged   func(bool)
	startupRecovery   StartupRecovery

	mu               sync.RWMutex
	masterCallbackMu sync.Mutex
	isMaster         bool
	masterKnown      bool
	masterLease      uint64
	masterTimer      *time.Timer
	initialized      bool
	homeIncarnation  HomeIncarnationID
}

type NodeIdentity struct {
	IP        string
	Port      int
	Secret    string
	StartedAt time.Time
}

// NewCoordinator creates a new coordinator.
func NewCoordinator(repo *Repository, node NodeIdentity, opts CoordinatorOptions) *Coordinator {
	// Keep validation before state changes so failures leave existing data intact.
	interval := opts.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	timeout := opts.HeartbeatTimeout
	if timeout <= 0 {
		timeout = defaultHeartbeatTimeout
	}
	if !node.StartedAt.IsZero() {
		node.StartedAt = node.StartedAt.UTC()
	}
	node.IP = strings.TrimSpace(node.IP)
	node.Secret = strings.TrimSpace(node.Secret)
	if node.Secret == "" {
		node.Secret = generateNodeSecret()
	}

	return &Coordinator{
		repo:              repo,
		node:              node,
		heartbeatInterval: interval,
		heartbeatTimeout:  timeout,
		onMasterChanged:   opts.OnMasterChanged,
		startupRecovery:   opts.StartupRecovery,
	}
}

// Initialize persists this Home process incarnation before listeners accept traffic.
func (c *Coordinator) Initialize(ctx context.Context) error {
	if errValidate := c.validate(); errValidate != nil {
		return errValidate
	}
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.RLock()
	initialized := c.initialized
	c.mu.RUnlock()
	if initialized {
		return nil
	}
	if _, errEnsure := c.repo.EnsureLifecycleConfig(ctx, c.heartbeatTimeout); errEnsure != nil {
		return errEnsure
	}
	incarnation, errRegister := c.repo.RegisterHomeIncarnation(ctx, c.node.IP, c.node.Port, []string{"credential_concurrency_foundation_v1", "credential_concurrency_limits_v2"})
	if errRegister != nil {
		return errRegister
	}
	if errHeartbeat := c.repo.HeartbeatHomeIncarnation(ctx, incarnation); errHeartbeat != nil {
		return c.retireFailedInitialization(ctx, incarnation, errHeartbeat)
	}
	if errRecovery := c.runStartupRecovery(ctx, incarnation); errRecovery != nil {
		return c.retireFailedInitialization(ctx, incarnation, errRecovery)
	}

	c.mu.Lock()
	c.node.StartedAt = incarnation.StartedAt
	c.mu.Unlock()
	if errBeat := c.heartbeatAndElect(ctx); errBeat != nil {
		return c.retireFailedInitialization(ctx, incarnation, errBeat)
	}
	c.mu.Lock()
	c.homeIncarnation = incarnation
	c.initialized = true
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) runStartupRecovery(ctx context.Context, incarnation HomeIncarnationID) error {
	if c.startupRecovery == nil {
		return nil
	}
	return c.startupRecovery(ctx, incarnation)
}

func (c *Coordinator) runLifecycleCleanup(ctx context.Context) error {
	if errCleanup := c.repo.CleanupStaleMemberships(ctx); errCleanup != nil {
		return errCleanup
	}
	home, initialized := c.HomeIncarnation()
	if !initialized {
		return fmt.Errorf("cluster coordinator is not initialized")
	}
	if errRecovery := c.runStartupRecovery(ctx, home); errRecovery != nil {
		return errRecovery
	}
	return c.repo.DeleteExpiredCPANodeSnapshots(ctx, cpaNodeSnapshotRetention(c.heartbeatTimeout))
}

func (c *Coordinator) retireFailedInitialization(ctx context.Context, incarnation HomeIncarnationID, initializationErr error) error {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(contextOrBackground(ctx)), c.heartbeatTimeout)
	defer cancelCleanup()
	if errRetire := c.repo.RetireHomeIncarnation(cleanupCtx, incarnation); errRetire != nil {
		return errors.Join(initializationErr, fmt.Errorf("retire Home incarnation: %w", errRetire))
	}
	return initializationErr
}

// HomeIncarnation returns the initialized Home process identity.
func (c *Coordinator) HomeIncarnation() (HomeIncarnationID, bool) {
	if c == nil {
		return HomeIncarnationID{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.homeIncarnation, c.initialized
}

// RetireHomeIncarnation retires the initialized Home process incarnation.
func (c *Coordinator) RetireHomeIncarnation(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("cluster coordinator is nil")
	}
	if c.repo == nil {
		return fmt.Errorf("cluster coordinator repository is nil")
	}
	c.mu.RLock()
	incarnation := c.homeIncarnation
	initialized := c.initialized
	c.mu.RUnlock()
	if !initialized {
		return nil
	}
	return c.repo.RetireHomeIncarnation(ctx, incarnation)
}

// Start starts subsequent coordinator heartbeats after initialization.
func (c *Coordinator) Start(ctx context.Context) error {
	if errValidate := c.validate(); errValidate != nil {
		return errValidate
	}
	c.mu.RLock()
	initialized := c.initialized
	c.mu.RUnlock()
	if !initialized {
		return fmt.Errorf("cluster coordinator is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cleanupCtx, cancelCleanup := context.WithCancel(ctx)
	defer cancelCleanup()
	go c.runLifecycleCleanupLoop(cleanupCtx)

	heartbeatTicker := time.NewTicker(c.heartbeatInterval)
	defer heartbeatTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.setMaster(false)
			return nil
		case <-heartbeatTicker.C:
			if ctx.Err() != nil {
				c.setMaster(false)
				return nil
			}
			if errHeartbeat := c.heartbeatHomeIncarnation(ctx); errHeartbeat != nil {
				c.setMaster(false)
				if ctx.Err() != nil {
					return nil
				}
				return errHeartbeat
			}
			if errBeat := c.heartbeatAndElect(ctx); errBeat != nil {
				c.setMaster(false)
				if ctx.Err() != nil {
					return nil
				}
				return errBeat
			}
		}
	}
}

func (c *Coordinator) runLifecycleCleanupLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		cleanupInterval := c.heartbeatInterval
		lifecycle, errLifecycle := c.repo.LifecycleConfig(ctx)
		if errLifecycle != nil {
			log.Warnf("failed to load lifecycle configuration for cleanup: %v", errLifecycle)
		} else if lifecycle.CleanupInterval > 0 {
			cleanupInterval = lifecycle.CleanupInterval
		} else {
			log.Warn("lifecycle cleanup interval must be positive")
		}

		timer := time.NewTimer(cleanupInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}

		if errCleanup := c.runLifecycleCleanup(ctx); errCleanup != nil {
			log.Warnf("failed to run lifecycle cleanup: %v", errCleanup)
		}
	}
}

// IsMaster reports whether is master.
func (c *Coordinator) IsMaster() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isMaster
}

// NodeSecret handles a node secret.
func (c *Coordinator) NodeSecret() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.node.Secret)
}

// UpdateClientCount stores the current active CPA client count for this node.
func (c *Coordinator) UpdateClientCount(ctx context.Context, clientCount int) error {
	if c == nil {
		return fmt.Errorf("cluster coordinator is nil")
	}
	if c.repo == nil {
		return fmt.Errorf("cluster coordinator repository is nil")
	}
	if clientCount < 0 {
		clientCount = 0
	}
	db, errDB := c.repo.database()
	if errDB != nil {
		return errDB
	}
	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return errNow
	}
	if errUpdate := db.WithContext(contextOrBackground(ctx)).
		Model(&ClusterNodeRecord{}).
		Where("ip = ? AND port = ?", c.node.IP, c.node.Port).
		Update("client_count", clientCount).Error; errUpdate != nil {
		return errUpdate
	}
	return c.syncCPANodeSnapshot(ctx, now)
}

// SetOnMasterChanged sets an on master changed.
func (c *Coordinator) SetOnMasterChanged(callback func(bool)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onMasterChanged = callback
	c.mu.Unlock()
}

// CurrentMaster returns a current master.
func (c *Coordinator) CurrentMaster(ctx context.Context) (*ClusterNodeRecord, error) {
	// Keep validation before state changes so failures leave existing data intact.
	if c == nil {
		return nil, fmt.Errorf("cluster coordinator is nil")
	}
	if c.repo == nil {
		return nil, fmt.Errorf("cluster coordinator repository is nil")
	}
	db, errDB := c.repo.database()
	if errDB != nil {
		return nil, errDB
	}

	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return nil, errNow
	}
	record := ClusterNodeRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).
		Where("is_master = ? AND last_seen_at >= ?", true, now.Add(-c.heartbeatTimeout)).
		Order("started_at ASC, ip ASC, port ASC").
		First(&record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if errFirst != nil {
		return nil, errFirst
	}
	return &record, nil
}

// heartbeatAndElect handles a heartbeat and elect.
func (c *Coordinator) heartbeatAndElect(ctx context.Context) error {
	// Keep validation before state changes so failures leave existing data intact.
	db, errDB := c.repo.database()
	if errDB != nil {
		return errDB
	}

	var elected ClusterNodeRecord
	var seenAt time.Time
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		gate := ClusterMasterGateRecord{ID: 1}
		if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&gate).Error; errCreate != nil {
			return errCreate
		}
		if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&gate, "id = ?", 1).Error; errLock != nil {
			return errLock
		}

		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		seenAt = now
		record := ClusterNodeRecord{
			IP:          c.node.IP,
			Port:        c.node.Port,
			SecretHash:  nodeSecretHash(c.node.Secret),
			ClientCount: node.GlobalRegistry().TotalCount(),
			StartedAt:   c.node.StartedAt,
			LastSeenAt:  now,
		}
		if errUpsert := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "ip"}, {Name: "port"}},
			DoUpdates: clause.Assignments(map[string]any{
				"started_at":   record.StartedAt,
				"last_seen_at": record.LastSeenAt,
				"secret_hash":  record.SecretHash,
				"client_count": record.ClientCount,
			}),
		}).Create(&record).Error; errUpsert != nil {
			return errUpsert
		}

		var liveNodes []ClusterNodeRecord
		if errFind := tx.Where("last_seen_at >= ?", now.Add(-c.heartbeatTimeout)).Find(&liveNodes).Error; errFind != nil {
			return errFind
		}
		if len(liveNodes) == 0 {
			return fmt.Errorf("cluster election found no live nodes")
		}
		sortClusterNodes(liveNodes)
		elected = liveNodes[0]
		if errClear := tx.Model(&ClusterNodeRecord{}).Where("is_master = ?", true).Update("is_master", false).Error; errClear != nil {
			return errClear
		}
		return tx.Model(&ClusterNodeRecord{}).
			Where("ip = ? AND port = ? AND started_at = ?", elected.IP, elected.Port, elected.StartedAt).
			Update("is_master", true).Error
	})
	if errTransaction != nil {
		return errTransaction
	}
	c.setMaster(clusterNodeMatches(elected, c.node.IP, c.node.Port))
	if errSnapshot := c.syncCPANodeSnapshot(ctx, seenAt); errSnapshot != nil {
		log.Warnf("failed to sync CPA node snapshot: %v", errSnapshot)
	}
	return nil
}

func (c *Coordinator) heartbeatHomeIncarnation(ctx context.Context) error {
	c.mu.RLock()
	incarnation := c.homeIncarnation
	initialized := c.initialized
	c.mu.RUnlock()
	if !initialized {
		return fmt.Errorf("cluster coordinator is not initialized")
	}
	return c.repo.HeartbeatHomeIncarnation(ctx, incarnation)
}

func (c *Coordinator) validate() error {
	if c == nil {
		return fmt.Errorf("cluster coordinator is nil")
	}
	if c.repo == nil {
		return fmt.Errorf("cluster coordinator repository is nil")
	}
	if strings.TrimSpace(c.node.IP) == "" {
		return fmt.Errorf("cluster node ip is required")
	}
	if c.node.Port <= 0 {
		return fmt.Errorf("cluster node port must be greater than 0")
	}
	return nil
}

func (c *Coordinator) syncCPANodeSnapshot(ctx context.Context, seenAt time.Time) error {
	if c == nil || c.repo == nil {
		return nil
	}
	home, initialized := c.HomeIncarnation()
	if !initialized {
		return fmt.Errorf("cluster coordinator is not initialized")
	}
	return c.repo.ReplaceCPANodeSnapshotForIncarnation(ctx, home, node.GlobalRegistry().List(), seenAt)
}

func cpaNodeSnapshotRetention(heartbeatTimeout time.Duration) time.Duration {
	if heartbeatTimeout <= 0 {
		heartbeatTimeout = defaultHeartbeatTimeout
	}
	return heartbeatTimeout * 6
}

// setMaster sets a master.
func (c *Coordinator) setMaster(next bool) {
	c.mu.Lock()
	changed := !c.masterKnown || c.isMaster != next
	c.isMaster = next
	c.masterKnown = true
	c.masterLease++
	lease := c.masterLease
	if c.masterTimer != nil {
		c.masterTimer.Stop()
		c.masterTimer = nil
	}
	if next {
		c.masterTimer = time.AfterFunc(c.heartbeatTimeout, func() {
			c.expireMasterLease(lease)
		})
	}
	c.mu.Unlock()

	if changed {
		c.notifyMasterChanged(next)
	}
}

func (c *Coordinator) expireMasterLease(lease uint64) {
	c.mu.Lock()
	if c.masterLease != lease || !c.isMaster {
		c.mu.Unlock()
		return
	}
	c.isMaster = false
	c.masterKnown = true
	c.masterTimer = nil
	c.mu.Unlock()
	c.notifyMasterChanged(false)
}

func (c *Coordinator) notifyMasterChanged(master bool) {
	c.masterCallbackMu.Lock()
	defer c.masterCallbackMu.Unlock()

	c.mu.RLock()
	if c.isMaster != master {
		c.mu.RUnlock()
		return
	}
	callback := c.onMasterChanged
	c.mu.RUnlock()
	if callback != nil {
		callback(master)
	}
}

// sortClusterNodes sorts a cluster nodes.
func sortClusterNodes(nodes []ClusterNodeRecord) {
	sort.Slice(nodes, func(i, j int) bool {
		if !nodes[i].StartedAt.Equal(nodes[j].StartedAt) {
			return nodes[i].StartedAt.Before(nodes[j].StartedAt)
		}
		return nodeSortKey(nodes[i]) < nodeSortKey(nodes[j])
	})
}

// nodeSortKey handles a node sort key.
func nodeSortKey(node ClusterNodeRecord) string {
	return fmt.Sprintf("%s:%d", strings.TrimSpace(node.IP), node.Port)
}

func clusterNodeMatches(node ClusterNodeRecord, ip string, port int) bool {
	return strings.TrimSpace(node.IP) == strings.TrimSpace(ip) && node.Port == port
}

// generateNodeSecret generates a node secret.
func generateNodeSecret() string {
	token := make([]byte, 32)
	if _, errRead := cryptorand.Read(token); errRead == nil {
		return hex.EncodeToString(token)
	}
	fallback := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(fallback[:])
}

// nodeSecretHash handles a node secret hash.
func nodeSecretHash(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
