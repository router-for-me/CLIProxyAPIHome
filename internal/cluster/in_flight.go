package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultInFlightDetailLimit = 50

var errInFlightExclusiveLockRequired = errors.New("in-flight reservation requires an exclusive credential lock")

func (r *Repository) ReserveInFlightLease(ctx context.Context, input home.InFlightReserveInput) (*home.InFlightLease, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	input.DispatchID = strings.TrimSpace(input.DispatchID)
	input.RequestID = strings.TrimSpace(input.RequestID)
	input.CredentialID = strings.TrimSpace(input.CredentialID)
	input.Provider = strings.TrimSpace(input.Provider)
	input.RequestedModel = strings.TrimSpace(input.RequestedModel)
	input.Model = coreauth.CanonicalModelKey(input.Model)
	input.CPANodeID = strings.TrimSpace(input.CPANodeID)
	input.CPAIP = strings.TrimSpace(input.CPAIP)
	input.CPALabel = strings.TrimSpace(input.CPALabel)
	input.OriginalAlias = strings.TrimSpace(input.OriginalAlias)
	if input.DispatchID == "" {
		return nil, fmt.Errorf("dispatch_id is required")
	}
	if input.RequestID == "" {
		return nil, fmt.Errorf("request_id is required")
	}
	if input.CredentialID == "" {
		return nil, fmt.Errorf("credential_id is required")
	}
	if input.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if input.TTL <= 0 {
		return nil, fmt.Errorf("lease ttl must be positive")
	}

	var reserved *home.InFlightLease
	isPostgres := db.Dialector != nil && db.Dialector.Name() == "postgres"
	reserve := func(lockStrength string) error {
		return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
			authRecord := AuthRecord{}
			if isPostgres {
				if errLock := tx.Clauses(clause.Locking{Strength: lockStrength}).Where("uuid = ?", input.CredentialID).First(&authRecord).Error; errLock != nil {
					return errLock
				}
			} else {
				// SQLite begins transactions in deferred mode. A no-op write acquires its
				// database write lock before counts are inspected without changing data.
				lockResult := tx.Model(&AuthRecord{}).
					Where("uuid = ? AND deleted_at IS NULL", input.CredentialID).
					UpdateColumn("version", gorm.Expr("version"))
				if lockResult.Error != nil {
					return lockResult.Error
				}
				if lockResult.RowsAffected == 0 {
					return gorm.ErrRecordNotFound
				}
				if errAuth := tx.Where("uuid = ?", input.CredentialID).First(&authRecord).Error; errAuth != nil {
					return errAuth
				}
			}
			now := time.Now().UTC()

			existing := InFlightLeaseRecord{}
			errExisting := tx.Where("dispatch_id = ?", input.DispatchID).First(&existing).Error
			if errExisting == nil {
				if existing.Status == InFlightLeaseStatusActive && existing.ExpiresAt.After(now) {
					if !inFlightLeaseReplayMatches(&existing, input) {
						return &home.DispatchReplayError{DispatchID: input.DispatchID}
					}
					reserved = inFlightLeaseFromRecord(&existing)
					reserved.Reused = true
					return nil
				}
				if existing.Status == InFlightLeaseStatusActive {
					closedAt := now
					if errExpire := tx.Model(&existing).Updates(map[string]any{
						"status":       InFlightLeaseStatusExpired,
						"closed_at":    &closedAt,
						"close_reason": "ttl_expired",
					}).Error; errExpire != nil {
						return errExpire
					}
				}
				return &home.DispatchReplayError{DispatchID: input.DispatchID}
			}
			if !errors.Is(errExisting, gorm.ErrRecordNotFound) {
				return errExisting
			}

			auth, errDecode := RecordToAuth(&authRecord)
			if errDecode != nil {
				return errDecode
			}
			modelLimit := normalizedModelLimit(auth.MaxInFlightByModel, input.Model)
			if isPostgres && lockStrength == "SHARE" && (auth.MaxInFlight > 0 || modelLimit > 0) {
				// Unlimited reservations only need a shared credential lock, allowing
				// concurrent observation inserts for the same credential. A cap mutation
				// conflicts with that lock. If a relevant cap is already visible, retry
				// under an exclusive lock so count-and-insert remains atomic.
				return errInFlightExclusiveLockRequired
			}

			if auth.MaxInFlight > 0 {
				var totalCount int64
				if errCount := tx.Model(&InFlightLeaseRecord{}).
					Where("credential_id = ? AND status = ? AND expires_at > ?", input.CredentialID, InFlightLeaseStatusActive, now).
					Count(&totalCount).Error; errCount != nil {
					return errCount
				}
				if totalCount >= int64(auth.MaxInFlight) {
					return &home.ConcurrencyExceededError{
						Scope:        "credential",
						CredentialID: input.CredentialID,
						Current:      totalCount,
						Limit:        auth.MaxInFlight,
					}
				}
			}

			if modelLimit > 0 {
				var modelCount int64
				if errCount := tx.Model(&InFlightLeaseRecord{}).
					Where("credential_id = ? AND model = ? AND status = ? AND expires_at > ?", input.CredentialID, input.Model, InFlightLeaseStatusActive, now).
					Count(&modelCount).Error; errCount != nil {
					return errCount
				}
				if modelCount >= int64(modelLimit) {
					return &home.ConcurrencyExceededError{
						Scope:        "model",
						CredentialID: input.CredentialID,
						Model:        input.Model,
						Current:      modelCount,
						Limit:        modelLimit,
					}
				}
			}

			expiresAt := now.Add(input.TTL)
			record := InFlightLeaseRecord{
				LeaseID:        input.DispatchID,
				DispatchID:     input.DispatchID,
				RequestID:      input.RequestID,
				CredentialID:   input.CredentialID,
				Provider:       input.Provider,
				RequestedModel: input.RequestedModel,
				Model:          input.Model,
				CPANodeID:      input.CPANodeID,
				CPAIP:          input.CPAIP,
				CPALabel:       input.CPALabel,
				ForceMapping:   input.ForceMapping,
				OriginalAlias:  input.OriginalAlias,
				Status:         InFlightLeaseStatusActive,
				StartedAt:      now,
				LastRenewedAt:  now,
				ExpiresAt:      expiresAt,
			}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			reserved = inFlightLeaseFromRecord(&record)
			return nil
		})
	}

	lockStrength := "UPDATE"
	if isPostgres {
		lockStrength = "SHARE"
	}
	errTx := reserve(lockStrength)
	if isPostgres && errors.Is(errTx, errInFlightExclusiveLockRequired) {
		errTx = reserve("UPDATE")
	}
	if errTx != nil {
		// Two duplicate dispatches may initially select different credentials and
		// therefore lock different auth rows. The unique dispatch index decides the
		// winner; reconcile the losing transaction to the committed lease so retries
		// remain idempotent instead of falling back to an untracked dispatch.
		existing := InFlightLeaseRecord{}
		errExisting := db.WithContext(contextOrBackground(ctx)).Where("dispatch_id = ?", input.DispatchID).First(&existing).Error
		if errExisting == nil {
			if existing.Status == InFlightLeaseStatusActive && existing.ExpiresAt.After(time.Now().UTC()) {
				if !inFlightLeaseReplayMatches(&existing, input) {
					return nil, &home.DispatchReplayError{DispatchID: input.DispatchID}
				}
				reused := inFlightLeaseFromRecord(&existing)
				reused.Reused = true
				return reused, nil
			}
			return nil, &home.DispatchReplayError{DispatchID: input.DispatchID}
		}
		return nil, errTx
	}
	return reserved, nil
}

func (r *Repository) RenewInFlightLease(ctx context.Context, leaseID string, nodeID string, ttl time.Duration) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	leaseID = strings.TrimSpace(leaseID)
	nodeID = strings.TrimSpace(nodeID)
	if leaseID == "" || ttl <= 0 {
		return false, nil
	}
	db = db.WithContext(contextOrBackground(ctx))
	for attempt := 0; attempt < 4; attempt++ {
		record := InFlightLeaseRecord{}
		if errFirst := db.Where("lease_id = ?", leaseID).First(&record).Error; errFirst != nil {
			if errors.Is(errFirst, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, errFirst
		}
		if record.Status != InFlightLeaseStatusActive {
			return false, nil
		}
		now := time.Now().UTC()
		if !record.ExpiresAt.After(now) {
			closedAt := now
			result := db.Model(&InFlightLeaseRecord{}).
				Where("id = ? AND status = ? AND last_renewed_at = ? AND expires_at = ? AND expires_at <= ?", record.ID, InFlightLeaseStatusActive, record.LastRenewedAt, record.ExpiresAt, now).
				Updates(map[string]any{
					"status":       InFlightLeaseStatusExpired,
					"closed_at":    &closedAt,
					"close_reason": "ttl_expired",
				})
			if result.Error != nil {
				return false, result.Error
			}
			if result.RowsAffected == 1 {
				return false, nil
			}
			continue
		}
		if !inFlightLeaseNodeMatches(&record, nodeID) {
			return false, fmt.Errorf("lease belongs to a different CPA node")
		}
		if record.LastRenewedAt.After(now) {
			return true, nil
		}
		renewTTL := ttl
		if existingTTL := record.ExpiresAt.Sub(record.LastRenewedAt); existingTTL > renewTTL {
			// CPA schedules renewal from the TTL returned at dispatch time. A
			// runtime config decrease must not shorten an existing lease below
			// that cadence, or a healthy long-running request could expire early.
			renewTTL = existingTTL
		}
		result := db.Model(&InFlightLeaseRecord{}).
			Where("id = ? AND status = ? AND last_renewed_at = ? AND expires_at = ? AND expires_at > ?", record.ID, InFlightLeaseStatusActive, record.LastRenewedAt, record.ExpiresAt, now).
			Updates(map[string]any{
				"last_renewed_at": now,
				"expires_at":      now.Add(renewTTL),
			})
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected == 1 {
			return true, nil
		}
	}

	record := InFlightLeaseRecord{}
	if errFirst := db.Where("lease_id = ?", leaseID).First(&record).Error; errFirst != nil {
		if errors.Is(errFirst, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, errFirst
	}
	if record.Status != InFlightLeaseStatusActive || !record.ExpiresAt.After(time.Now().UTC()) {
		return false, nil
	}
	if !inFlightLeaseNodeMatches(&record, nodeID) {
		return false, fmt.Errorf("lease belongs to a different CPA node")
	}
	return true, nil
}

func (r *Repository) ReleaseInFlightLease(ctx context.Context, leaseID string, nodeID string, reason string) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	leaseID = strings.TrimSpace(leaseID)
	nodeID = strings.TrimSpace(nodeID)
	reason = strings.TrimSpace(reason)
	if leaseID == "" {
		return false, nil
	}
	if reason == "" {
		reason = "terminal"
	}
	now := time.Now().UTC()
	released := false
	errTx := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		record := InFlightLeaseRecord{}
		if errFirst := tx.Where("lease_id = ?", leaseID).First(&record).Error; errFirst != nil {
			if errors.Is(errFirst, gorm.ErrRecordNotFound) {
				return nil
			}
			return errFirst
		}
		if record.Status != InFlightLeaseStatusActive {
			return nil
		}
		if !inFlightLeaseNodeMatches(&record, nodeID) {
			return fmt.Errorf("lease belongs to a different CPA node")
		}
		released = true
		return tx.Model(&record).Updates(map[string]any{
			"status":       InFlightLeaseStatusReleased,
			"closed_at":    &now,
			"close_reason": reason,
		}).Error
	})
	return released, errTx
}

func (r *Repository) PurgeInFlightLeases(ctx context.Context, now time.Time, retention time.Duration, limit int) (int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if retention <= 0 {
		retention = 10 * time.Minute
	}
	if limit <= 0 {
		limit = 1000
	}
	db = db.WithContext(contextOrBackground(ctx))
	if errExpire := expireInFlightLeasesTx(db, "", now); errExpire != nil {
		return 0, errExpire
	}
	cutoff := now.Add(-retention)
	ids := db.Model(&InFlightLeaseRecord{}).
		Select("id").
		Where("status <> ? AND closed_at IS NOT NULL AND closed_at < ?", InFlightLeaseStatusActive, cutoff).
		Order("id").
		Limit(limit)
	result := db.Where("id IN (?)", ids).Delete(&InFlightLeaseRecord{})
	return result.RowsAffected, result.Error
}

func (r *Repository) ListInFlightCredentialSummaries(ctx context.Context) ([]home.InFlightCredentialSummary, time.Time, error) {
	now := time.Now().UTC()
	auths, errAuths := r.ListAuths(ctx)
	if errAuths != nil {
		return nil, time.Time{}, errAuths
	}
	counts, errCounts := r.inFlightCounts(ctx, now)
	if errCounts != nil {
		return nil, time.Time{}, errCounts
	}
	summaries := make([]home.InFlightCredentialSummary, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		summaries = append(summaries, buildInFlightCredentialSummary(auth, counts[auth.ID]))
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CredentialID < summaries[j].CredentialID
	})
	return summaries, now, nil
}

func (r *Repository) GetInFlightCredentialDetail(ctx context.Context, credentialID string, cursor uint, limit int) (*home.InFlightCredentialDetail, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return nil, fmt.Errorf("credential_id is required")
	}
	now := time.Now().UTC()
	auth, _, errAuth := r.GetAuth(ctx, credentialID)
	if errAuth != nil {
		return nil, errAuth
	}
	counts, errCounts := r.inFlightCountsForCredential(ctx, credentialID, now)
	if errCounts != nil {
		return nil, errCounts
	}
	page, errPage := r.listInFlightLeases(ctx, credentialID, cursor, limit, now)
	if errPage != nil {
		return nil, errPage
	}
	return &home.InFlightCredentialDetail{
		Summary:    buildInFlightCredentialSummary(auth, counts),
		Requests:   page.Requests,
		NextCursor: page.NextCursor,
		ObservedAt: page.ObservedAt,
	}, nil
}

func (r *Repository) ListInFlightLeases(ctx context.Context, credentialID string, cursor uint, limit int) (*home.InFlightLeasePage, error) {
	return r.listInFlightLeases(ctx, strings.TrimSpace(credentialID), cursor, limit, time.Now().UTC())
}

func (r *Repository) listInFlightLeases(ctx context.Context, credentialID string, cursor uint, limit int, now time.Time) (*home.InFlightLeasePage, error) {
	if limit <= 0 {
		limit = defaultInFlightDetailLimit
	}
	if limit > 200 {
		limit = 200
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	query := db.WithContext(contextOrBackground(ctx)).
		Where("status = ? AND expires_at > ?", InFlightLeaseStatusActive, now).
		Order("id ASC").
		Limit(limit + 1)
	if credentialID != "" {
		query = query.Where("credential_id = ?", credentialID)
	}
	if cursor > 0 {
		query = query.Where("id > ?", cursor)
	}
	records := make([]InFlightLeaseRecord, 0, limit+1)
	if errFind := query.Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	nextCursor := ""
	if len(records) > limit {
		nextCursor = fmt.Sprintf("%d", records[limit-1].ID)
		records = records[:limit]
	}
	requests := make([]home.InFlightLease, 0, len(records))
	for i := range records {
		lease := inFlightLeaseFromRecord(&records[i])
		requests = append(requests, *lease)
	}
	return &home.InFlightLeasePage{
		Requests:   requests,
		NextCursor: nextCursor,
		ObservedAt: now,
	}, nil
}

type inFlightModelCount struct {
	Model string
	Count int64
}

func (r *Repository) inFlightCounts(ctx context.Context, now time.Time) (map[string]map[string]int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	type row struct {
		CredentialID string
		Model        string
		Count        int64
	}
	rows := make([]row, 0)
	if errScan := db.WithContext(contextOrBackground(ctx)).Model(&InFlightLeaseRecord{}).
		Select("credential_id, model, COUNT(*) AS count").
		Where("status = ? AND expires_at > ?", InFlightLeaseStatusActive, now).
		Group("credential_id, model").
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	out := make(map[string]map[string]int64)
	for _, item := range rows {
		if out[item.CredentialID] == nil {
			out[item.CredentialID] = make(map[string]int64)
		}
		out[item.CredentialID][item.Model] = item.Count
	}
	return out, nil
}

func (r *Repository) inFlightCountsForCredential(ctx context.Context, credentialID string, now time.Time) (map[string]int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	rows := make([]inFlightModelCount, 0)
	if errScan := db.WithContext(contextOrBackground(ctx)).Model(&InFlightLeaseRecord{}).
		Select("model, COUNT(*) AS count").
		Where("credential_id = ? AND status = ? AND expires_at > ?", credentialID, InFlightLeaseStatusActive, now).
		Group("model").
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.Model] = item.Count
	}
	return out, nil
}

func buildInFlightCredentialSummary(auth *coreauth.Auth, counts map[string]int64) home.InFlightCredentialSummary {
	summary := home.InFlightCredentialSummary{CredentialID: strings.TrimSpace(auth.ID)}
	for _, count := range counts {
		summary.InFlight += count
	}
	if auth.MaxInFlight > 0 {
		limit := auth.MaxInFlight
		remaining := int64(limit) - summary.InFlight
		if remaining < 0 {
			remaining = 0
		}
		summary.MaxInFlight = &limit
		summary.Remaining = &remaining
		summary.TotalSaturated = summary.InFlight >= int64(limit)
	}
	modelSet := make(map[string]struct{}, len(counts)+len(auth.MaxInFlightByModel))
	for model := range counts {
		if trimmed := strings.TrimSpace(model); trimmed != "" {
			modelSet[trimmed] = struct{}{}
		}
	}
	for model, limit := range auth.MaxInFlightByModel {
		if canonical := coreauth.CanonicalModelKey(model); canonical != "" && limit > 0 {
			modelSet[canonical] = struct{}{}
		}
	}
	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		current := counts[model]
		item := home.InFlightModelSummary{Model: model, InFlight: current}
		modelLimit := normalizedModelLimit(auth.MaxInFlightByModel, model)
		var effectiveRemaining *int64
		if modelLimit > 0 {
			limit := modelLimit
			remaining := int64(limit) - current
			if remaining < 0 {
				remaining = 0
			}
			item.MaxInFlight = &limit
			effectiveRemaining = &remaining
			if current >= int64(limit) {
				summary.SaturatedModelCount++
			}
		}
		if summary.Remaining != nil && (effectiveRemaining == nil || *summary.Remaining < *effectiveRemaining) {
			remaining := *summary.Remaining
			effectiveRemaining = &remaining
		}
		item.Remaining = effectiveRemaining
		item.Saturated = summary.TotalSaturated || (item.MaxInFlight != nil && current >= int64(*item.MaxInFlight))
		summary.Models = append(summary.Models, item)
	}
	return summary
}

func normalizedModelLimit(limits map[string]int, model string) int {
	model = coreauth.CanonicalModelKey(model)
	if model == "" {
		return 0
	}
	matchedLimit := 0
	for candidate, limit := range limits {
		if coreauth.CanonicalModelKey(candidate) == model && limit > 0 {
			if matchedLimit == 0 || limit < matchedLimit {
				matchedLimit = limit
			}
		}
	}
	return matchedLimit
}

func expireInFlightLeasesTx(tx *gorm.DB, credentialID string, now time.Time) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	closedAt := now.UTC()
	query := tx.Model(&InFlightLeaseRecord{}).
		Where("status = ? AND expires_at <= ?", InFlightLeaseStatusActive, closedAt)
	if strings.TrimSpace(credentialID) != "" {
		query = query.Where("credential_id = ?", strings.TrimSpace(credentialID))
	}
	return query.Updates(map[string]any{
		"status":       InFlightLeaseStatusExpired,
		"closed_at":    &closedAt,
		"close_reason": "ttl_expired",
	}).Error
}

func inFlightLeaseNodeMatches(record *InFlightLeaseRecord, nodeID string) bool {
	if record == nil {
		return false
	}
	expected := strings.TrimSpace(record.CPANodeID)
	actual := strings.TrimSpace(nodeID)
	return expected == "" || expected == actual
}

func inFlightLeaseReplayMatches(record *InFlightLeaseRecord, input home.InFlightReserveInput) bool {
	if record == nil || !inFlightLeaseNodeMatches(record, input.CPANodeID) {
		return false
	}
	return strings.TrimSpace(record.RequestID) == strings.TrimSpace(input.RequestID) &&
		strings.TrimSpace(record.RequestedModel) == strings.TrimSpace(input.RequestedModel)
}

func inFlightLeaseFromRecord(record *InFlightLeaseRecord) *home.InFlightLease {
	if record == nil {
		return nil
	}
	return &home.InFlightLease{
		ID:             record.ID,
		LeaseID:        record.LeaseID,
		DispatchID:     record.DispatchID,
		RequestID:      record.RequestID,
		CredentialID:   record.CredentialID,
		Provider:       record.Provider,
		RequestedModel: record.RequestedModel,
		Model:          record.Model,
		CPANodeID:      record.CPANodeID,
		CPAIP:          record.CPAIP,
		CPALabel:       record.CPALabel,
		ForceMapping:   record.ForceMapping,
		OriginalAlias:  record.OriginalAlias,
		StartedAt:      record.StartedAt,
		LastRenewedAt:  record.LastRenewedAt,
		ExpiresAt:      record.ExpiresAt,
	}
}
