package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct {
	db            *gorm.DB
	cpaSnapshotMu sync.Mutex
}

type UpsertResult string

const (
	UpsertResultCreated   UpsertResult = "created"
	UpsertResultUpdated   UpsertResult = "updated"
	UpsertResultUnchanged UpsertResult = "unchanged"
	UpsertResultRestored  UpsertResult = "restored"
)

const clusterEventAdvisoryLockKey int64 = 749327842680272317

type APIKeyUpsertStats struct {
	Created   int
	Updated   int
	Unchanged int
	Restored  int
	Removed   int
}

// Changed reports whether api key rows were mutated.
func (s APIKeyUpsertStats) Changed() bool {
	return s.Created != 0 || s.Updated != 0 || s.Restored != 0 || s.Removed != 0
}

// NewRepository creates a new repository.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// DialectName returns the repository database dialect.
func (r *Repository) DialectName() string {
	db, errDB := r.database()
	if errDB != nil || db.Dialector == nil {
		return ""
	}
	return db.Dialector.Name()
}

// AuthToRecord converts auth to record.
func AuthToRecord(auth *coreauth.Auth) (*AuthRecord, error) {
	// Normalize auth state before updating runtime indexes.
	if auth == nil {
		return nil, fmt.Errorf("auth is nil")
	}
	if strings.TrimSpace(auth.ID) == "" {
		return nil, fmt.Errorf("auth id is required")
	}
	if strings.TrimSpace(auth.Index) == "" {
		return nil, fmt.Errorf("auth index is required")
	}
	if auth.ID != auth.Index {
		return nil, fmt.Errorf("auth id %q and index %q must match", auth.ID, auth.Index)
	}

	rawJSON, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		return nil, errMarshal
	}

	record := &AuthRecord{
		UUID:        auth.ID,
		AuthJSON:    JSONB(rawJSON),
		Version:     1,
		ID:          auth.ID,
		Index:       auth.Index,
		Provider:    auth.Provider,
		Label:       auth.Label,
		Prefix:      auth.Prefix,
		Status:      auth.Status,
		Disabled:    auth.Disabled,
		Unavailable: auth.Unavailable,
		CreatedAt:   auth.CreatedAt,
		UpdatedAt:   auth.UpdatedAt,
	}

	if auth.Attributes != nil {
		record.BaseURL = strings.TrimSpace(auth.Attributes["base_url"])
		record.APIKeyHash = hashValue(auth.Attributes["api_key"])
		record.CompatName = strings.TrimSpace(auth.Attributes["compat_name"])
		record.ProviderKey = strings.TrimSpace(auth.Attributes["provider_key"])
		record.ModelsHash = strings.TrimSpace(auth.Attributes["models_hash"])
	}
	record.LastRefreshedAt = timePtrOrNil(auth.LastRefreshedAt)
	record.NextRefreshAfter = timePtrOrNil(auth.NextRefreshAfter)
	record.NextRetryAfter = authNextRetryAfterTime(auth)

	return record, nil
}

// RecordToAuth records a to auth.
func RecordToAuth(record *AuthRecord) (*coreauth.Auth, error) {
	// Resolve credential context before calling upstream OAuth services.
	if record == nil {
		return nil, fmt.Errorf("auth record is nil")
	}
	if strings.TrimSpace(record.UUID) == "" {
		return nil, fmt.Errorf("auth record uuid is required")
	}
	if record.ID != record.UUID {
		return nil, fmt.Errorf("auth record id %q must match uuid %q", record.ID, record.UUID)
	}
	if record.Index != record.UUID {
		return nil, fmt.Errorf("auth record index %q must match uuid %q", record.Index, record.UUID)
	}

	auth := &coreauth.Auth{}
	if errUnmarshal := json.Unmarshal([]byte(record.AuthJSON), auth); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if auth.ID != record.UUID {
		return nil, fmt.Errorf("auth json id %q must match uuid %q", auth.ID, record.UUID)
	}
	auth.Index = record.Index
	auth.StateVersion = record.Version

	return auth, nil
}

// UpsertAuth inserts or updates an auth.
func (r *Repository) UpsertAuth(ctx context.Context, auth *coreauth.Auth, op string) (*AuthRecord, error) {
	record, _, errUpsertAuth := r.UpsertAuthWithResult(ctx, auth, op)
	return record, errUpsertAuth
}

// UpsertAuthWithResult inserts or updates an auth and reports the mutation result.
func (r *Repository) UpsertAuthWithResult(ctx context.Context, auth *coreauth.Auth, op string) (*AuthRecord, UpsertResult, error) {
	return r.upsertAuthWithResult(ctx, auth, op, false)
}

// UpsertAuthPreservingDisabled inserts or updates an auth without re-enabling an existing disabled record.
func (r *Repository) UpsertAuthPreservingDisabled(ctx context.Context, auth *coreauth.Auth, op string) (*AuthRecord, error) {
	record, _, errUpsert := r.upsertAuthWithResult(ctx, auth, op, true)
	return record, errUpsert
}

func (r *Repository) upsertAuthWithResult(ctx context.Context, auth *coreauth.Auth, op string, preserveDisabled bool) (*AuthRecord, UpsertResult, error) {
	// Normalize auth state before updating runtime indexes.
	db, errDB := r.database()
	if errDB != nil {
		return nil, "", errDB
	}
	record, errRecord := AuthToRecord(auth)
	if errRecord != nil {
		return nil, "", errRecord
	}
	if strings.TrimSpace(op) == "" {
		op = "upsert"
	}

	ctx = contextOrBackground(ctx)
	result := UpsertResultUnchanged
	out := record
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		existing := AuthRecord{}
		query := tx.Unscoped().Where("uuid = ?", record.UUID)
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		errFirst := query.First(&existing).Error
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			record.Version = 1
			if errCreate := tx.Create(record).Error; errCreate != nil {
				return errCreate
			}
			result = UpsertResultCreated
			out = record
		case errFirst != nil:
			return errFirst
		default:
			if auth.StateVersion > 0 {
				switch {
				case existing.DeletedAt.Valid, existing.Version > auth.StateVersion:
					result = UpsertResultUnchanged
					out = &existing
					return nil
				case existing.Version < auth.StateVersion:
					return fmt.Errorf("auth state version %d is ahead of persisted version %d", auth.StateVersion, existing.Version)
				}
			}
			if preserveDisabled {
				current, errCurrent := RecordToAuth(&existing)
				if errCurrent != nil {
					return errCurrent
				}
				next := auth.Clone()
				next.Disabled = current.Disabled
				next.Status = current.Status
				next.StatusMessage = current.StatusMessage
				preservedRecord, errRecordPreserved := AuthToRecord(next)
				if errRecordPreserved != nil {
					return errRecordPreserved
				}
				record = preservedRecord
			}
			sameJSON, errJSONEqual := semanticJSONEqual([]byte(existing.AuthJSON), []byte(record.AuthJSON))
			if errJSONEqual != nil {
				return errJSONEqual
			}
			if !existing.DeletedAt.Valid && sameJSON {
				result = UpsertResultUnchanged
				out = &existing
				return nil
			}
			record.Version = existing.Version + 1
			record.DeletedAt = gorm.DeletedAt{}
			if errUpdate := tx.Unscoped().Select("*").Where("uuid = ?", record.UUID).Updates(record).Error; errUpdate != nil {
				return errUpdate
			}
			if existing.DeletedAt.Valid || quotaCredentialIdentityChanged(existing, *record) {
				if errDeleteWindows := tx.Where("credential_id = ?", record.UUID).Delete(&QuotaWindowRecord{}).Error; errDeleteWindows != nil {
					return errDeleteWindows
				}
				if errDeleteSnapshot := tx.Where("credential_id = ?", record.UUID).Delete(&QuotaSnapshotRecord{}).Error; errDeleteSnapshot != nil {
					return errDeleteSnapshot
				}
			}
			if existing.DeletedAt.Valid {
				result = UpsertResultRestored
			} else {
				result = UpsertResultUpdated
			}
			out = record
		}
		return appendEvent(tx, "auth", op, record.UUID, record.Version)
	})
	if errTransaction != nil {
		return nil, "", errTransaction
	}

	return out, result, nil
}

// GetAuth returns an auth.
func (r *Repository) GetAuth(ctx context.Context, uuid string) (*coreauth.Auth, *AuthRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}

	record := &AuthRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("uuid = ?", uuid).First(record).Error
	if errFirst != nil {
		return nil, nil, errFirst
	}

	auth, errAuth := RecordToAuth(record)
	if errAuth != nil {
		return nil, nil, errAuth
	}
	return auth, record, nil
}

// WithAuthRefreshLock runs a short credential mutation under the database row lock.
// Deprecated: OAuth refresh network calls must use the lease flow instead.
func (r *Repository) WithAuthRefreshLock(ctx context.Context, uuid string, fn func(tx *Repository, auth *coreauth.Auth) (*coreauth.Auth, error)) (*coreauth.Auth, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return nil, fmt.Errorf("cluster auth uuid is required")
	}
	if fn == nil {
		return nil, fmt.Errorf("cluster auth refresh lock callback is nil")
	}

	var out *coreauth.Auth
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(txDB *gorm.DB) error {
		record := &AuthRecord{}
		query := txDB.Where("uuid = ?", uuid)
		if txDB.Dialector != nil && txDB.Dialector.Name() == "sqlite" {
			if errLock := txDB.Exec(`UPDATE "auth" SET "version" = "version" WHERE "uuid" = ?`, uuid).Error; errLock != nil {
				return errLock
			}
		} else {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if errFirst := query.First(record).Error; errFirst != nil {
			return errFirst
		}
		auth, errAuth := RecordToAuth(record)
		if errAuth != nil {
			return errAuth
		}
		auth.ID = uuid
		auth.Index = uuid
		refreshed, errRefresh := fn(&Repository{db: txDB}, auth)
		if errRefresh != nil {
			return errRefresh
		}
		out = refreshed
		return nil
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return out, nil
}

// MutateAuth loads an auth row under a write lock, applies mutate to the
// decoded auth, and persists the result in the same transaction when mutate
// reports a change. Concurrent mutations from other Home nodes serialize on
// the row lock, which keeps read-modify-write transitions such as quota
// backoff escalation atomic across the cluster. The returned auth and record
// reflect the post-transaction row state.
func (r *Repository) MutateAuth(ctx context.Context, uuid string, op string, mutate func(auth *coreauth.Auth) bool) (*coreauth.Auth, *AuthRecord, bool, error) {
	// Keep validation before state changes so failures leave existing data intact.
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, false, errDB
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return nil, nil, false, fmt.Errorf("cluster auth uuid is required")
	}
	if mutate == nil {
		return nil, nil, false, fmt.Errorf("cluster auth mutate callback is nil")
	}
	if strings.TrimSpace(op) == "" {
		op = "update"
	}

	var outAuth *coreauth.Auth
	var outRecord *AuthRecord
	changed := false
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		existing := &AuthRecord{}
		errFirst := txDB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("uuid = ?", uuid).First(existing).Error
		if errFirst != nil {
			return errFirst
		}

		auth, errAuth := RecordToAuth(existing)
		if errAuth != nil {
			return errAuth
		}
		auth.ID = uuid
		auth.Index = uuid

		changed = mutate(auth)
		if !changed {
			outAuth = auth
			outRecord = existing
			return nil
		}

		record, errRecord := AuthToRecord(auth)
		if errRecord != nil {
			return errRecord
		}
		record.Version = existing.Version + 1
		auth.StateVersion = record.Version
		record.CreatedAt = existing.CreatedAt
		if errUpdate := txDB.Select("*").Where("uuid = ?", uuid).Updates(record).Error; errUpdate != nil {
			return errUpdate
		}
		outAuth = auth
		outRecord = record
		return appendEvent(txDB, "auth", op, uuid, record.Version)
	})
	if errTransaction != nil {
		return nil, nil, false, errTransaction
	}
	return outAuth, outRecord, changed, nil
}

// ListAuthIndex returns an auth index.
func (r *Repository) ListAuthIndex(ctx context.Context) ([]AuthIndex, error) {
	// Normalize auth state before updating runtime indexes.
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []AuthRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}

	auths := make([]*coreauth.Auth, 0, len(records))
	for _, record := range records {
		auth, errAuth := RecordToAuth(&record)
		if errAuth != nil {
			return nil, errAuth
		}
		auth.ID = record.UUID
		auth.Index = record.UUID
		auths = append(auths, auth)
	}

	out := make([]AuthIndex, 0, len(records))
	for i, record := range records {
		auth := auths[i]
		out = append(out, AuthIndex{
			UUID:           record.UUID,
			Version:        record.Version,
			ID:             record.ID,
			Index:          record.Index,
			Provider:       record.Provider,
			Label:          record.Label,
			Prefix:         record.Prefix,
			Status:         auth.Status,
			Disabled:       auth.Disabled,
			Unavailable:    auth.Unavailable,
			NextRetryAfter: auth.NextRetryAfter,
			Quota:          auth.Quota,
			ModelStates:    auth.ModelStates,
			BaseURL:        record.BaseURL,
			ModelsHash:     record.ModelsHash,
			Attributes:     auth.Attributes,
			ModelMetadata:  modelMetadataFromAuth(auth),
		})
	}
	return out, nil
}

// LiveClusterNodeByIPAndSecret handles a live cluster node by ip and secret.
func (r *Repository) LiveClusterNodeByIPAndSecret(ctx context.Context, ip string, secret string, cutoff time.Time) (*ClusterNodeRecord, error) {
	// Keep validation before state changes so failures leave existing data intact.
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil, nil
	}
	secretHash := nodeSecretHash(secret)
	if secretHash == "" {
		return nil, nil
	}
	if cutoff.IsZero() {
		cutoff = time.Now().UTC().Add(-defaultHeartbeatTimeout)
	}

	record := &ClusterNodeRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).
		Where("ip = ? AND secret_hash = ? AND port > ? AND last_seen_at >= ?", ip, secretHash, 0, cutoff).
		Order("started_at ASC, port ASC").
		First(record).Error
	if errFirst != nil {
		if errors.Is(errFirst, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, errFirst
	}
	return record, nil
}

// ListLiveClusterNodes returns live cluster nodes.
func (r *Repository) ListLiveClusterNodes(ctx context.Context, cutoff time.Time) ([]ClusterNodeRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if cutoff.IsZero() {
		cutoff = time.Now().UTC().Add(-defaultHeartbeatTimeout)
	}

	var records []ClusterNodeRecord
	errFind := db.WithContext(contextOrBackground(ctx)).
		Where("port > ? AND last_seen_at >= ?", 0, cutoff).
		Order("client_count ASC, started_at ASC, ip ASC, port ASC").
		Find(&records).Error
	if errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// ListClusterNodes returns known Home cluster nodes, optionally bounded by last seen time.
func (r *Repository) ListClusterNodes(ctx context.Context, cutoff time.Time) ([]ClusterNodeRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []ClusterNodeRecord
	query := db.WithContext(contextOrBackground(ctx)).Where("port > ?", 0)
	if !cutoff.IsZero() {
		query = query.Where("last_seen_at >= ?", cutoff)
	}
	errFind := query.Order("started_at ASC, ip ASC, port ASC").Find(&records).Error
	if errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// ReplaceCPANodeSnapshot replaces the active CPA connection snapshot owned by one Home node.
func (r *Repository) ReplaceCPANodeSnapshot(ctx context.Context, homeIP string, homePort int, nodes []node.Node, seenAt time.Time) error {
	return r.replaceCPANodeSnapshot(ctx, HomeIncarnationID{IP: homeIP, Port: homePort}, nodes, seenAt, false)
}

// ReplaceCPANodeSnapshotForIncarnation writes a snapshot only for the active exact Home process incarnation.
func (r *Repository) ReplaceCPANodeSnapshotForIncarnation(ctx context.Context, home HomeIncarnationID, nodes []node.Node, seenAt time.Time) error {
	return r.replaceCPANodeSnapshot(ctx, home, nodes, seenAt, true)
}

func (r *Repository) replaceCPANodeSnapshot(ctx context.Context, home HomeIncarnationID, nodes []node.Node, seenAt time.Time, requireActiveIncarnation bool) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	r.cpaSnapshotMu.Lock()
	defer r.cpaSnapshotMu.Unlock()
	home.IP = strings.TrimSpace(home.IP)
	if home.IP == "" {
		return fmt.Errorf("home ip is required")
	}
	if home.Port <= 0 {
		return fmt.Errorf("home port must be greater than 0")
	}
	if requireActiveIncarnation && home.StartedAt.IsZero() {
		return fmt.Errorf("Home incarnation is required")
	}
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	} else {
		seenAt = seenAt.UTC()
	}

	records := make([]CPANodeRecord, 0, len(nodes))
	seenKeys := make(map[string]struct{}, len(nodes))
	for _, item := range nodes {
		key := cpaNodeKey(item)
		if key == "" {
			continue
		}
		if _, ok := seenKeys[key]; ok {
			continue
		}
		seenKeys[key] = struct{}{}
		clientCount := item.ClientCount
		connectedAt := item.Connected
		if connectedAt.IsZero() {
			connectedAt = seenAt
		} else {
			connectedAt = connectedAt.UTC()
		}
		if clientCount <= 0 && item.OpenConnections <= 0 && item.ActiveHandlers <= 0 {
			continue
		}
		records = append(records, CPANodeRecord{
			HomeIP:                 home.IP,
			HomePort:               home.Port,
			HomeStartedAt:          home.StartedAt,
			NodeKey:                key,
			NodeID:                 strings.TrimSpace(item.NodeID),
			ClientIP:               strings.TrimSpace(item.IP),
			ClientCount:            clientCount,
			CertificateFingerprint: strings.TrimSpace(item.CertificateFingerprint),
			OpenConnections:        item.OpenConnections,
			ActiveHandlers:         item.ActiveHandlers,
			LatestCancelRevision:   item.LatestCancelRevision,
			ConnectedAt:            connectedAt,
			LastSeenAt:             seenAt,
		})
	}

	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		if requireActiveIncarnation {
			if errHome := verifyActiveHomeIncarnation(tx, home); errHome != nil {
				return errHome
			}
		}
		if errDelete := tx.Where("home_ip = ? AND home_port = ? AND home_started_at = ?", home.IP, home.Port, home.StartedAt).Delete(&CPANodeRecord{}).Error; errDelete != nil {
			return errDelete
		}
		if len(records) == 0 {
			return nil
		}
		return tx.Create(&records).Error
	})
}

// DeleteExpiredCPANodeSnapshots removes derived snapshots after the retention window.
func (r *Repository) DeleteExpiredCPANodeSnapshots(ctx context.Context, retention time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if retention <= 0 {
		return fmt.Errorf("CPA node snapshot retention must be greater than 0")
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		return tx.Where("last_seen_at < ?", now.Add(-retention)).Delete(&CPANodeRecord{}).Error
	})
}

// ListActiveCPAMemberships returns the authoritative active CPA owners.
func (r *Repository) ListActiveCPAMemberships(ctx context.Context) ([]CPANodeMembershipRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []CPANodeMembershipRecord
	errFind := db.WithContext(contextOrBackground(ctx)).
		Where("state = ?", MembershipStateActive).
		Order("certificate_fingerprint ASC").
		Find(&records).Error
	if errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// ListLiveCPANodes returns live CPA node snapshots reported by active Home nodes.
func (r *Repository) ListLiveCPANodes(ctx context.Context, cutoff time.Time) ([]CPANodeRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if cutoff.IsZero() {
		cutoff = time.Now().UTC().Add(-defaultHeartbeatTimeout)
	}

	var records []CPANodeRecord
	errFind := db.WithContext(contextOrBackground(ctx)).
		Where("last_seen_at >= ?", cutoff).
		Order("connected_at ASC, client_ip ASC, node_id ASC, home_ip ASC, home_port ASC").
		Find(&records).Error
	if errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// ListCPANodeSnapshots returns known CPA node snapshots, optionally bounded by last seen time.
func (r *Repository) ListCPANodeSnapshots(ctx context.Context, cutoff time.Time) ([]CPANodeRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []CPANodeRecord
	query := db.WithContext(contextOrBackground(ctx))
	if !cutoff.IsZero() {
		query = query.Where("last_seen_at >= ?", cutoff)
	}
	errFind := query.Order("home_ip ASC, home_port ASC, connected_at ASC, client_ip ASC, node_id ASC").Find(&records).Error
	if errFind != nil {
		return nil, errFind
	}
	return records, nil
}

func cpaNodeKey(item node.Node) string {
	fingerprint := strings.TrimSpace(item.CertificateFingerprint)
	if fingerprint != "" {
		return "fingerprint:" + fingerprint
	}
	nodeID := strings.TrimSpace(item.NodeID)
	if nodeID != "" {
		return "node:" + nodeID
	}
	clientIP := strings.TrimSpace(item.IP)
	if clientIP == "" {
		return ""
	}
	return "ip:" + clientIP
}

// ListAuths returns an auths.
func (r *Repository) ListAuths(ctx context.Context) ([]*coreauth.Auth, error) {
	// Normalize auth state before updating runtime indexes.
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []AuthRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}

	auths := make([]*coreauth.Auth, 0, len(records))
	for _, record := range records {
		auth, errAuth := RecordToAuth(&record)
		if errAuth != nil {
			return nil, errAuth
		}
		auth.ID = record.UUID
		auth.Index = record.UUID
		auths = append(auths, auth)
	}
	return auths, nil
}

// SoftDeleteAuth handles a soft delete auth.
func (r *Repository) SoftDeleteAuth(ctx context.Context, uuid string) error {
	_, errDelete := r.SoftDeleteAuthWithVersion(ctx, uuid)
	return errDelete
}

// SoftDeleteAuthWithVersion soft-deletes an auth and returns its tombstone revision.
func (r *Repository) SoftDeleteAuthWithVersion(ctx context.Context, uuid string) (int64, error) {
	// Validate request inputs before mutating persisted state.
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}

	ctx = contextOrBackground(ctx)
	deletedVersion := int64(0)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := AuthRecord{}
		query := tx.Where("uuid = ?", uuid)
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if errFirst := query.First(&record).Error; errFirst != nil {
			return errFirst
		}
		newVersion := record.Version + 1
		if errUpdate := tx.Model(&record).Update("version", newVersion).Error; errUpdate != nil {
			return errUpdate
		}
		record.Version = newVersion
		if errDelete := tx.Delete(&record).Error; errDelete != nil {
			return errDelete
		}
		if errDeleteWindows := tx.Where("credential_id = ?", uuid).Delete(&QuotaWindowRecord{}).Error; errDeleteWindows != nil {
			return errDeleteWindows
		}
		if errDeleteSnapshot := tx.Where("credential_id = ?", uuid).Delete(&QuotaSnapshotRecord{}).Error; errDeleteSnapshot != nil {
			return errDeleteSnapshot
		}
		if errEvent := appendEvent(tx, "auth", "delete", record.UUID, record.Version); errEvent != nil {
			return errEvent
		}
		deletedVersion = record.Version
		return nil
	})
	if errTransaction != nil {
		return 0, errTransaction
	}
	return deletedVersion, nil
}

const configAPIKeysRootKey = "api-keys"

// normalizeAPIKeys normalizes API keys sourced from config.yaml.
func normalizeAPIKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		if _, exists := seen[trimmedKey]; exists {
			continue
		}
		seen[trimmedKey] = struct{}{}
		normalized = append(normalized, trimmedKey)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// normalizeAPIKeysFromAny converts a config.yaml api-keys value into a normalized string slice.
func normalizeAPIKeysFromAny(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return normalizeAPIKeys([]string{typed})
	case []string:
		return normalizeAPIKeys(typed)
	case []any:
		keys := make([]string, 0, len(typed))
		for _, item := range typed {
			str, ok := item.(string)
			if !ok {
				continue
			}
			keys = append(keys, str)
		}
		return normalizeAPIKeys(keys)
	default:
		return nil
	}
}

// replaceAPIKeysTx replaces the active API keys in the api_key table using soft deletes.
func replaceAPIKeysTx(ctx context.Context, tx *gorm.DB, keys []string) error {
	_, errReplace := replaceAPIKeysTxWithStats(ctx, tx, keys)
	return errReplace
}

// UpsertAPIKeys inserts or restores API keys without deleting keys missing from the input.
func (r *Repository) UpsertAPIKeys(ctx context.Context, keys []string) (APIKeyUpsertStats, error) {
	db, errDB := r.database()
	if errDB != nil {
		return APIKeyUpsertStats{}, errDB
	}

	ctx = contextOrBackground(ctx)
	var stats APIKeyUpsertStats
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var errUpsertAPIKeys error
		stats, errUpsertAPIKeys = upsertAPIKeysTxWithStats(ctx, tx, keys)
		if errUpsertAPIKeys != nil {
			return errUpsertAPIKeys
		}
		deleteResult := tx.Delete(&ConfigRecord{}, "key = ?", configAPIKeysRootKey)
		if deleteResult.Error != nil {
			return deleteResult.Error
		}
		if !stats.Changed() && deleteResult.RowsAffected == 0 {
			return nil
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	return stats, errTransaction
}

// replaceAPIKeysTxWithStats replaces the active API keys in the api_key table using soft deletes.
func replaceAPIKeysTxWithStats(ctx context.Context, tx *gorm.DB, keys []string) (APIKeyUpsertStats, error) {
	// Normalize source data before building the derived payload.
	if tx == nil {
		return APIKeyUpsertStats{}, fmt.Errorf("database connection is nil")
	}
	keys = normalizeAPIKeys(keys)

	var existing []APIKeyRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).Unscoped().Order("id").Find(&existing).Error; errFind != nil {
		return APIKeyUpsertStats{}, errFind
	}

	stats := APIKeyUpsertStats{}
	existingByKey := make(map[string]*APIKeyRecord, len(existing))
	for i := range existing {
		record := &existing[i]
		key := strings.TrimSpace(record.APIKey)
		if key == "" {
			continue
		}
		existingByKey[key] = record
	}

	keep := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keep[key] = struct{}{}
		if record := existingByKey[key]; record != nil {
			if record.DeletedAt.Valid {
				if errRestore := tx.WithContext(contextOrBackground(ctx)).Unscoped().
					Model(&APIKeyRecord{}).
					Where("id = ?", record.ID).
					Updates(map[string]any{"deleted_at": nil}).Error; errRestore != nil {
					return APIKeyUpsertStats{}, errRestore
				}
				stats.Restored++
			} else {
				stats.Unchanged++
			}
			continue
		}
		if errCreate := tx.WithContext(contextOrBackground(ctx)).Create(&APIKeyRecord{APIKey: key, Channels: emptyAPIKeyChannelsJSON(), ModelGroups: emptyAPIKeyModelGroupsJSON()}).Error; errCreate != nil {
			return APIKeyUpsertStats{}, errCreate
		}
		stats.Created++
	}

	for _, record := range existing {
		key := strings.TrimSpace(record.APIKey)
		if key == "" {
			continue
		}
		if _, ok := keep[key]; ok {
			continue
		}
		if record.DeletedAt.Valid {
			continue
		}
		if errDelete := tx.WithContext(contextOrBackground(ctx)).Delete(&APIKeyRecord{}, "id = ?", record.ID).Error; errDelete != nil {
			return APIKeyUpsertStats{}, errDelete
		}
		stats.Removed++
	}
	return stats, nil
}

// upsertAPIKeysTxWithStats inserts or restores API keys without deleting missing keys.
func upsertAPIKeysTxWithStats(ctx context.Context, tx *gorm.DB, keys []string) (APIKeyUpsertStats, error) {
	// Normalize source data before building the derived payload.
	if tx == nil {
		return APIKeyUpsertStats{}, fmt.Errorf("database connection is nil")
	}
	keys = normalizeAPIKeys(keys)
	stats := APIKeyUpsertStats{}
	for _, key := range keys {
		record := APIKeyRecord{}
		errFirst := tx.WithContext(contextOrBackground(ctx)).Unscoped().Where("api_key = ?", key).First(&record).Error
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			if errCreate := tx.WithContext(contextOrBackground(ctx)).Create(&APIKeyRecord{APIKey: key, Channels: emptyAPIKeyChannelsJSON(), ModelGroups: emptyAPIKeyModelGroupsJSON()}).Error; errCreate != nil {
				return APIKeyUpsertStats{}, errCreate
			}
			stats.Created++
		case errFirst != nil:
			return APIKeyUpsertStats{}, errFirst
		case record.DeletedAt.Valid:
			if errRestore := tx.WithContext(contextOrBackground(ctx)).Unscoped().
				Model(&APIKeyRecord{}).
				Where("id = ?", record.ID).
				Updates(map[string]any{"deleted_at": nil}).Error; errRestore != nil {
				return APIKeyUpsertStats{}, errRestore
			}
			stats.Restored++
		default:
			stats.Unchanged++
		}
	}
	return stats, nil
}

// UpsertConfigValue inserts or updates a config value.
func (r *Repository) UpsertConfigValue(ctx context.Context, key string, value any) error {
	_, errUpsertConfigValue := r.UpsertConfigValueWithResult(ctx, key, value)
	return errUpsertConfigValue
}

// UpsertConfigValueWithResult inserts or updates a config value and reports the mutation result.
func (r *Repository) UpsertConfigValueWithResult(ctx context.Context, key string, value any) (UpsertResult, error) {
	// Normalize source data before building the derived payload.
	db, errDB := r.database()
	if errDB != nil {
		return "", errDB
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("config key is required")
	}

	if key == configAPIKeysRootKey {
		ctx = contextOrBackground(ctx)
		apiKeys := normalizeAPIKeysFromAny(value)
		result := UpsertResultUnchanged
		errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			stats, errReplace := replaceAPIKeysTxWithStats(ctx, tx, apiKeys)
			if errReplace != nil {
				return errReplace
			}
			deleteResult := tx.Delete(&ConfigRecord{}, "key = ?", configAPIKeysRootKey)
			if deleteResult.Error != nil {
				return deleteResult.Error
			}
			if !stats.Changed() && deleteResult.RowsAffected == 0 {
				result = UpsertResultUnchanged
				return nil
			}
			result = apiKeyStatsResult(stats)
			if result == UpsertResultUnchanged {
				result = UpsertResultUpdated
			}
			return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
		})
		return result, errTransaction
	}

	rawJSON, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return "", errMarshal
	}

	ctx = contextOrBackground(ctx)
	result := UpsertResultUnchanged
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := ConfigRecord{}
		errFirst := tx.Where("key = ?", key).First(&record).Error
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			record = ConfigRecord{Key: key, Value: JSONB(rawJSON), Version: 1}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			result = UpsertResultCreated
		case errFirst != nil:
			return errFirst
		default:
			sameJSON, errJSONEqual := semanticJSONEqual([]byte(record.Value), rawJSON)
			if errJSONEqual != nil {
				return errJSONEqual
			}
			if sameJSON {
				result = UpsertResultUnchanged
				return nil
			}
			record.Value = JSONB(rawJSON)
			record.Version++
			if errSave := tx.Save(&record).Error; errSave != nil {
				return errSave
			}
			result = UpsertResultUpdated
		}
		return appendEvent(tx, "config", "upsert", key, record.Version)
	})
	return result, errTransaction
}

// apiKeyStatsResult maps API key mutation stats to an upsert result.
func apiKeyStatsResult(stats APIKeyUpsertStats) UpsertResult {
	switch {
	case stats.Created != 0:
		return UpsertResultCreated
	case stats.Restored != 0:
		return UpsertResultRestored
	case stats.Updated != 0:
		return UpsertResultUpdated
	case stats.Removed != 0:
		return UpsertResultUpdated
	default:
		return UpsertResultUnchanged
	}
}

// semanticJSONEqual compares JSON values after decoding so storage formatting is ignored.
func semanticJSONEqual(left []byte, right []byte) (bool, error) {
	if bytes.Equal(left, right) {
		return true, nil
	}
	var leftValue any
	if errUnmarshalLeft := json.Unmarshal(left, &leftValue); errUnmarshalLeft != nil {
		return false, fmt.Errorf("unmarshal left json: %w", errUnmarshalLeft)
	}
	var rightValue any
	if errUnmarshalRight := json.Unmarshal(right, &rightValue); errUnmarshalRight != nil {
		return false, fmt.Errorf("unmarshal right json: %w", errUnmarshalRight)
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

// ReplaceConfigSnapshot handles a replace config snapshot.
func (r *Repository) ReplaceConfigSnapshot(ctx context.Context, values map[string]any) error {
	nodeHeartbeatTimeout, errTimeout := r.lifecycleConfigNodeHeartbeatTimeout(ctx)
	if errTimeout != nil {
		return errTimeout
	}
	return r.ReplaceConfigSnapshotWithLifecycleConfig(ctx, nodeHeartbeatTimeout, values)
}

// ReplaceConfigSnapshotWithLifecycleConfig replaces a snapshot after applying its lifecycle configuration.
func (r *Repository) ReplaceConfigSnapshotWithLifecycleConfig(ctx context.Context, nodeHeartbeatTimeout time.Duration, values map[string]any) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	ctx = contextOrBackground(ctx)
	lifecycleValue, lifecycleProvided := values["credential-concurrency"]
	var nextLifecycle appconfig.CredentialConcurrencyConfig
	if lifecycleProvided {
		var errConfig error
		nextLifecycle, errConfig = credentialConcurrencyConfigFromValue(lifecycleValue)
		if errConfig != nil {
			return errConfig
		}
	}
	return withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
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
		apiKeys, clean, errPrepare := prepareConfigSnapshotReplace(values)
		if errPrepare != nil {
			return errPrepare
		}
		return replaceConfigSnapshotTx(ctx, tx, apiKeys, clean)
	})
}

func prepareConfigSnapshotReplace(values map[string]any) ([]string, map[string]json.RawMessage, error) {
	apiKeys := normalizeAPIKeysFromAny(values[configAPIKeysRootKey])
	clean := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if key == configAPIKeysRootKey || key == "credential-concurrency" || key == credentialConcurrencyPoliciesRootKey {
			continue
		}
		rawJSON, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return nil, nil, errMarshal
		}
		clean[key] = rawJSON
	}
	return apiKeys, clean, nil
}

func replaceConfigSnapshotTx(ctx context.Context, tx *gorm.DB, apiKeys []string, clean map[string]json.RawMessage) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if errReplaceAPIKeys := replaceAPIKeysTx(ctx, tx, apiKeys); errReplaceAPIKeys != nil {
		return errReplaceAPIKeys
	}

	var existing []ConfigRecord
	if errFind := tx.Find(&existing).Error; errFind != nil {
		return errFind
	}

	seen := make(map[string]struct{}, len(clean))
	eventVersion := int64(0)
	for key, rawJSON := range clean {
		seen[key] = struct{}{}
		record := ConfigRecord{}
		errFirst := tx.Where("key = ?", key).First(&record).Error
		switch {
		case errors.Is(errFirst, gorm.ErrRecordNotFound):
			record = ConfigRecord{Key: key, Value: JSONB(rawJSON), Version: 1}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
		case errFirst != nil:
			return errFirst
		default:
			record.Value = JSONB(rawJSON)
			record.Version++
			if errSave := tx.Save(&record).Error; errSave != nil {
				return errSave
			}
		}
		if record.Version > eventVersion {
			eventVersion = record.Version
		}
	}

	for _, record := range existing {
		if _, ok := seen[record.Key]; ok {
			continue
		}
		nextVersion := record.Version + 1
		if errDelete := tx.Delete(&ConfigRecord{}, "key = ?", record.Key).Error; errDelete != nil {
			return errDelete
		}
		if nextVersion > eventVersion {
			eventVersion = nextVersion
		}
	}
	if eventVersion == 0 {
		eventVersion = 1
	}
	return appendEvent(tx, "config", "replace", "config", eventVersion)
}

// LoadConfigSnapshot loads a config snapshot.
func (r *Repository) LoadConfigSnapshot(ctx context.Context) (map[string]json.RawMessage, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	var records []ConfigRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Find(&records).Error; errFind != nil {
		return nil, errFind
	}

	out := make(map[string]json.RawMessage, len(records)+1)
	for _, record := range records {
		if strings.TrimSpace(record.Key) == configAPIKeysRootKey {
			continue
		}
		out[record.Key] = json.RawMessage(record.Value)
	}

	var apiKeyRecords []APIKeyRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Order("id").Find(&apiKeyRecords).Error; errFind != nil {
		return nil, errFind
	}
	if len(apiKeyRecords) > 0 {
		keys := make([]string, 0, len(apiKeyRecords))
		for _, record := range apiKeyRecords {
			key := strings.TrimSpace(record.APIKey)
			if key == "" {
				continue
			}
			keys = append(keys, key)
		}
		keys = normalizeAPIKeys(keys)
		if len(keys) > 0 {
			rawJSON, errMarshal := json.Marshal(keys)
			if errMarshal != nil {
				return nil, errMarshal
			}
			out[configAPIKeysRootKey] = rawJSON
		}
	}
	return out, nil
}

// AppendEvent appends an event.
func (r *Repository) AppendEvent(ctx context.Context, scope, op, entity string, version int64) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		return appendEvent(tx, scope, op, entity, version)
	})
}

// MaxEventID handles a max event id.
func (r *Repository) MaxEventID(ctx context.Context) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	var maxID int64
	if errScan := db.WithContext(contextOrBackground(ctx)).Model(&ClusterEventRecord{}).Select("COALESCE(MAX(id), 0)").Scan(&maxID).Error; errScan != nil {
		return 0, errScan
	}
	return maxID, nil
}

// database handles a database.
func (r *Repository) database() (*gorm.DB, error) {
	if r == nil {
		return nil, fmt.Errorf("cluster repository is nil")
	}
	if r.db == nil {
		return nil, fmt.Errorf("cluster repository database is nil")
	}
	return r.db, nil
}

// appendEvent appends an event.
func appendEvent(db *gorm.DB, scope, op, entity string, version int64) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if errLock := lockClusterEventTransaction(db); errLock != nil {
		return errLock
	}
	event := ClusterEventRecord{
		Scope:      scope,
		Op:         op,
		EntityUUID: entity,
		Version:    version,
		CreatedAt:  time.Now().UTC(),
	}
	return db.Create(&event).Error
}

func lockClusterEventTransaction(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if db.Dialector == nil || db.Dialector.Name() != "postgres" {
		return nil
	}
	// Keep event ID allocation serialized until commit so watchers cannot skip a later-committing lower ID.
	return db.Exec("SELECT pg_advisory_xact_lock(?)", clusterEventAdvisoryLockKey).Error
}

// contextOrBackground handles a context or background.
func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// timePtrOrNil handles a time ptr or nil.
func timePtrOrNil(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utcValue := value.UTC()
	return &utcValue
}

func authNextRetryAfterTime(auth *coreauth.Auth) *time.Time {
	if auth == nil {
		return nil
	}
	var earliest time.Time
	remember := func(value time.Time) {
		if value.IsZero() {
			return
		}
		value = value.UTC()
		if earliest.IsZero() || value.Before(earliest) {
			earliest = value
		}
	}
	remember(auth.NextRetryAfter)
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		remember(state.NextRetryAfter)
	}
	return timePtrOrNil(earliest)
}

// hashValue reports whether h value is present.
func hashValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
