package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	quotaSnapshotSchemaVersion      = 1
	antigravityQuotaSnapshotVersion = 2
	codexQuotaSnapshotVersion       = 2
)

const quotaSnapshotFallbackFreshness = 30 * time.Minute

const (
	quotaCredentialIDMaxLength  = 128
	quotaWindowTextMaxLength    = 256
	quotaCurrencyMaxLength      = 16
	quotaResetCreditMaxItems    = 100
	quotaResetCreditIDMaxLength = 128
)

// ErrQuotaProbeLeaseLost reports that an in-flight probe no longer owns its completion lease.
var ErrQuotaProbeLeaseLost = errors.New("quota probe lease is no longer owned")

var (
	quotaBearerSecretPattern    = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;\]}]+`)
	quotaRequestIDSafePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+=-]{0,127}$`)
	quotaRequestIDSecretPattern = regexp.MustCompile(`(?i)(bearer|authorization|access[_-]?token|refresh[_-]?token|api[_-]?key|cookie|secret|^sk[-_]|^gh[pousr]_)`)
	quotaJWTSecretPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}$`)
)

// QuotaSnapshotVersion returns the active-collector snapshot version expected
// for a provider. Provider-specific versions let one collector invalidate its
// persisted representation without forcing unrelated providers to recollect.
func QuotaSnapshotVersion(provider string) int {
	switch normalizeQuotaProviderID(provider) {
	case "antigravity":
		return antigravityQuotaSnapshotVersion
	case "codex":
		return codexQuotaSnapshotVersion
	default:
		return quotaSnapshotSchemaVersion
	}
}

type QuotaSnapshotRecord struct {
	CredentialID        string     `gorm:"column:credential_id;primaryKey;size:128"`
	QuotaStatus         string     `gorm:"column:quota_status;not null;size:32;index"`
	CollectionStatus    string     `gorm:"column:collection_status;not null;size:32;index"`
	Source              string     `gorm:"column:source;size:32;index"`
	ObservedAt          *time.Time `gorm:"column:observed_at;index"`
	ExpiresAt           *time.Time `gorm:"column:expires_at;index"`
	LastAttemptAt       *time.Time `gorm:"column:last_attempt_at"`
	LastSuccessAt       *time.Time `gorm:"column:last_success_at"`
	NextProbeAt         *time.Time `gorm:"column:next_probe_at;index"`
	ConsecutiveFailure  int        `gorm:"column:consecutive_failures;not null;default:0"`
	ErrorCode           string     `gorm:"column:error_code;size:128"`
	ErrorMessage        string     `gorm:"column:error_message;size:500"`
	ErrorRetryable      bool       `gorm:"column:error_retryable;not null;default:false"`
	ErrorOccurredAt     *time.Time `gorm:"column:error_occurred_at"`
	ErrorStatusCode     int        `gorm:"column:error_status_code;not null;default:0"`
	ErrorRequestID      string     `gorm:"column:error_request_id;size:128"`
	HomeID              string     `gorm:"column:home_id;size:256"`
	HomeLabel           string     `gorm:"column:home_label;size:256"`
	CPANodeID           string     `gorm:"column:cpa_node_id;size:256"`
	CPANodeLabel        string     `gorm:"column:cpa_node_label;size:256"`
	PlanName            string     `gorm:"column:plan_name;size:64"`
	PlanPremium         *bool      `gorm:"column:plan_premium"`
	ResetCredits        JSONB      `gorm:"column:reset_credits"`
	ProbeLeaseOwner     string     `gorm:"column:probe_lease_owner;size:256"`
	ProbeLeaseExpiresAt *time.Time `gorm:"column:probe_lease_expires_at;index"`
	ParserVersion       int        `gorm:"column:parser_version;not null;default:1"`
	CollectorVersion    int        `gorm:"column:collector_version;not null;default:1"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

func (QuotaSnapshotRecord) TableName() string { return "quota_snapshot" }

type QuotaWindowRecord struct {
	CredentialID   string     `gorm:"column:credential_id;primaryKey;size:128;index:idx_quota_window_credential_order,priority:1"`
	WindowID       string     `gorm:"column:window_id;primaryKey;size:256;index:idx_quota_window_credential_order,priority:2"`
	Priority       int        `gorm:"column:priority;not null;default:0;index:idx_quota_window_credential_order,priority:3"`
	Label          string     `gorm:"column:label;size:256"`
	Scope          string     `gorm:"column:scope;not null;size:32"`
	ScopeID        string     `gorm:"column:scope_id;size:256"`
	Mode           string     `gorm:"column:mode;not null;size:32"`
	Status         string     `gorm:"column:status;not null;size:32"`
	Unit           string     `gorm:"column:unit;not null;size:32"`
	Currency       string     `gorm:"column:currency;size:16"`
	Used           *float64   `gorm:"column:used"`
	Remaining      *float64   `gorm:"column:remaining"`
	Limit          *float64   `gorm:"column:limit_value"`
	UsedRatio      *float64   `gorm:"column:used_ratio"`
	RemainingRatio *float64   `gorm:"column:remaining_ratio"`
	IsUnlimited    bool       `gorm:"column:is_unlimited;not null;default:false"`
	ResetAt        *time.Time `gorm:"column:reset_at;index"`
	WindowSeconds  *int64     `gorm:"column:window_seconds"`
	PeriodUnit     string     `gorm:"column:period_unit;not null;size:32"`
	PeriodValue    *float64   `gorm:"column:period_value"`
	Source         string     `gorm:"column:source;not null;size:32"`
	ObservedAt     time.Time  `gorm:"column:observed_at;not null;index"`
	ExpiresAt      *time.Time `gorm:"column:expires_at;index"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (QuotaWindowRecord) TableName() string { return "quota_window" }

type QuotaWindow struct {
	ID             string     `json:"id"`
	Label          *string    `json:"label"`
	Scope          string     `json:"scope"`
	ScopeID        *string    `json:"scope_id"`
	Mode           string     `json:"mode"`
	Status         string     `json:"status"`
	Unit           string     `json:"unit"`
	Currency       *string    `json:"currency"`
	Used           *float64   `json:"used"`
	Remaining      *float64   `json:"remaining"`
	Limit          *float64   `json:"limit"`
	UsedRatio      *float64   `json:"used_ratio"`
	RemainingRatio *float64   `json:"remaining_ratio"`
	IsUnlimited    bool       `json:"is_unlimited"`
	ResetAt        *time.Time `json:"reset_at"`
	WindowSeconds  *int64     `json:"window_seconds"`
	PeriodUnit     string     `json:"period_unit"`
	PeriodValue    *float64   `json:"period_value"`
	Source         string     `json:"source"`
	ObservedAt     time.Time  `json:"observed_at"`
	ExpiresAt      *time.Time `json:"-"`
	Priority       int        `json:"-"`
}

type QuotaCollectionError struct {
	Code               string     `json:"code"`
	Message            string     `json:"message"`
	Retryable          bool       `json:"retryable"`
	OccurredAt         *time.Time `json:"occurred_at"`
	UpstreamStatusCode *int       `json:"upstream_status_code"`
	RequestID          *string    `json:"request_id"`
}

type QuotaRuntime struct {
	HomeID       string `json:"home_id"`
	HomeLabel    string `json:"home_label"`
	CPANodeID    string `json:"cpa_node_id"`
	CPANodeLabel string `json:"cpa_node_label"`
}

// QuotaPlan carries subscription plan metadata derived from a provider's quota
// response (for example Codex Pro 20x or xAI SuperGrok Heavy).
type QuotaPlan struct {
	Name    string `json:"name"`
	Premium bool   `json:"premium"`
}

// QuotaResetCredit describes one available provider-issued quota reset credit.
type QuotaResetCredit struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	GrantedAt time.Time  `json:"granted_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// QuotaResetCredits carries the latest normalized reset-credit observation.
type QuotaResetCredits struct {
	AvailableCount *int               `json:"available_count"`
	ObservedAt     time.Time          `json:"observed_at"`
	Credits        []QuotaResetCredit `json:"credits"`
}

type QuotaCredentialSnapshot struct {
	CredentialID       string                `json:"credential_id"`
	AuthIndex          *string               `json:"auth_index"`
	Provider           string                `json:"provider"`
	CredentialType     string                `json:"credential_type"`
	Label              string                `json:"label"`
	Account            *string               `json:"account"`
	Project            *string               `json:"project"`
	CredentialStatus   string                `json:"credential_status"`
	QuotaStatus        string                `json:"quota_status"`
	Freshness          string                `json:"freshness"`
	CollectionStatus   string                `json:"collection_status"`
	Source             *string               `json:"source"`
	ObservedAt         *time.Time            `json:"observed_at"`
	ExpiresAt          *time.Time            `json:"expires_at"`
	EarliestResetAt    *time.Time            `json:"earliest_reset_at"`
	LastAttemptAt      *time.Time            `json:"last_attempt_at"`
	LastSuccessAt      *time.Time            `json:"last_success_at"`
	NextProbeAt        *time.Time            `json:"next_probe_at"`
	ConsecutiveFailure int                   `json:"consecutive_failures"`
	PrimaryWindows     []QuotaWindow         `json:"primary_windows"`
	WindowCount        int                   `json:"window_count"`
	Error              *QuotaCollectionError `json:"error"`
	Runtime            *QuotaRuntime         `json:"runtime"`
	Plan               *QuotaPlan            `json:"plan,omitempty"`
	ResetCredits       *QuotaResetCredits    `json:"-"`
	Windows            []QuotaWindow         `json:"-"`
}

type QuotaSnapshotWrite struct {
	CredentialID          string
	QuotaStatus           string
	CollectionStatus      string
	Source                string
	ObservedAt            *time.Time
	ReceivedAt            *time.Time
	MaxAcceptedObservedAt *time.Time
	ExpiresAt             *time.Time
	LastAttemptAt         *time.Time
	LastSuccessAt         *time.Time
	NextProbeAt           *time.Time
	ConsecutiveFailure    int
	Error                 *QuotaCollectionError
	Runtime               *QuotaRuntime
	Plan                  *QuotaPlan
	ReplacePlan           bool
	ResetCredits          *QuotaResetCredits
	ReplaceResetCredits   bool
	ParserVersion         int
	CollectorVersion      int
	ExpectedProbeOwner    string
	ClearProbeLease       bool
	ReplaceWindows        bool
	Windows               []QuotaWindow
}

type QuotaListQuery struct {
	Limit              int
	Offset             int
	Search             string
	IDs                map[string]struct{}
	Providers          map[string]struct{}
	QuotaStatuses      map[string]struct{}
	Freshness          map[string]struct{}
	Sources            map[string]struct{}
	CredentialStatuses map[string]struct{}
	CollectionStatuses map[string]struct{}
	Sort               string
	Now                time.Time
}

type QuotaSummary struct {
	TotalCredentials int        `json:"total_credentials"`
	Healthy          int        `json:"healthy"`
	Low              int        `json:"low"`
	Exhausted        int        `json:"exhausted"`
	Unknown          int        `json:"unknown"`
	Error            int        `json:"error"`
	Unsupported      int        `json:"unsupported"`
	Stale            int        `json:"stale"`
	Never            int        `json:"never"`
	Collecting       int        `json:"collecting"`
	NeedsAttention   int        `json:"needs_attention"`
	LastObservedAt   *time.Time `json:"last_observed_at"`
}

type QuotaFacetOption struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type QuotaFacets struct {
	Providers          []QuotaFacetOption `json:"providers"`
	QuotaStatuses      []QuotaFacetOption `json:"quota_statuses"`
	Freshness          []QuotaFacetOption `json:"freshness"`
	Sources            []QuotaFacetOption `json:"sources"`
	CredentialStatuses []QuotaFacetOption `json:"credential_statuses"`
	CollectionStatuses []QuotaFacetOption `json:"collection_statuses"`
}

type QuotaListResult struct {
	Items         []QuotaCredentialSnapshot
	Total         int
	Summary       QuotaSummary
	GlobalSummary QuotaSummary
	Facets        QuotaFacets
}

func (r *Repository) UpsertQuotaSnapshot(ctx context.Context, input QuotaSnapshotWrite) (bool, error) {
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	return upsertQuotaSnapshotDB(ctx, db, input)
}

func upsertQuotaSnapshotDB(ctx context.Context, db *gorm.DB, input QuotaSnapshotWrite) (bool, error) {
	if errValidate := validateQuotaSnapshotWrite(&input); errValidate != nil {
		return false, errValidate
	}
	if db == nil {
		return false, fmt.Errorf("quota snapshot database is nil")
	}
	ctx = contextOrBackground(ctx)
	updated := false
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing QuotaSnapshotRecord
		errExisting := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, "credential_id = ?", input.CredentialID).Error
		if errExisting != nil && !errors.Is(errExisting, gorm.ErrRecordNotFound) {
			return errExisting
		}
		if expectedOwner := strings.TrimSpace(input.ExpectedProbeOwner); expectedOwner != "" {
			if errors.Is(errExisting, gorm.ErrRecordNotFound) || existing.ProbeLeaseOwner != expectedOwner {
				return nil
			}
		}
		existingObservationInvalid := errExisting == nil && input.MaxAcceptedObservedAt != nil && existing.ObservedAt != nil && existing.ObservedAt.After(input.MaxAcceptedObservedAt.UTC())
		if errExisting == nil && !existingObservationInvalid && quotaObservationIsOlder(existing.ObservedAt, input.ObservedAt) {
			return nil
		}

		record, errRecord := quotaSnapshotRecordFromWrite(input)
		if errRecord != nil {
			return errRecord
		}
		if errExisting == nil {
			record.CreatedAt = existing.CreatedAt
			if record.HomeID == "" {
				record.HomeID = existing.HomeID
			}
			if record.HomeLabel == "" {
				record.HomeLabel = existing.HomeLabel
			}
			if record.CPANodeID == "" {
				record.CPANodeID = existing.CPANodeID
			}
			if record.CPANodeLabel == "" {
				record.CPANodeLabel = existing.CPANodeLabel
			}
			if input.Plan == nil && !input.ReplacePlan {
				record.PlanName = existing.PlanName
				record.PlanPremium = existing.PlanPremium
			}
			if !input.ReplaceResetCredits && len(existing.ResetCredits) > 0 {
				record.ResetCredits = append(JSONB(nil), existing.ResetCredits...)
			}
			if input.Source == "response_header" {
				// Passive observations must not downgrade an active collector
				// version after a provider-specific snapshot migration succeeds
				// or has already been attempted.
				if existing.ParserVersion > record.ParserVersion {
					record.ParserVersion = existing.ParserVersion
				}
				if existing.CollectorVersion > record.CollectorVersion {
					record.CollectorVersion = existing.CollectorVersion
				}
				preserveQuotaProbeSchedule(&record, existing, input.ObservedAt, input.ReceivedAt)
			}
			if !input.ClearProbeLease {
				record.ProbeLeaseOwner = existing.ProbeLeaseOwner
				record.ProbeLeaseExpiresAt = quotaUTC(existing.ProbeLeaseExpiresAt)
			}
		}
		accepted, errSave := createQuotaSnapshotRecord(tx, &record, input)
		if errSave != nil {
			return errSave
		}
		if !accepted {
			return nil
		}
		if existingObservationInvalid {
			if errDeleteWindows := tx.Where("credential_id = ?", input.CredentialID).Delete(&QuotaWindowRecord{}).Error; errDeleteWindows != nil {
				return errDeleteWindows
			}
		} else if input.MaxAcceptedObservedAt != nil {
			if errDeleteFutureWindows := tx.Where("credential_id = ? AND observed_at > ?", input.CredentialID, input.MaxAcceptedObservedAt.UTC()).Delete(&QuotaWindowRecord{}).Error; errDeleteFutureWindows != nil {
				return errDeleteFutureWindows
			}
		}

		if input.ReplaceWindows {
			windowIDs := make([]string, 0, len(input.Windows))
			for _, window := range input.Windows {
				windowIDs = append(windowIDs, window.ID)
			}
			deleteQuery := tx.Where("credential_id = ?", input.CredentialID)
			if len(windowIDs) > 0 {
				deleteQuery = deleteQuery.Where("window_id NOT IN ?", windowIDs)
			}
			if errDelete := deleteQuery.Delete(&QuotaWindowRecord{}).Error; errDelete != nil {
				return errDelete
			}
		} else if input.ObservedAt != nil {
			incomingObservedAt := input.ObservedAt.UTC()
			if errExisting == nil && existing.ExpiresAt != nil {
				if errBackfill := tx.Model(&QuotaWindowRecord{}).
					Where("credential_id = ? AND expires_at IS NULL", input.CredentialID).
					Update("expires_at", existing.ExpiresAt.UTC()).Error; errBackfill != nil {
					return errBackfill
				}
			} else if errExisting == nil {
				if errPruneUnknown := tx.Where("credential_id = ? AND expires_at IS NULL AND observed_at < ?", input.CredentialID, incomingObservedAt).
					Delete(&QuotaWindowRecord{}).Error; errPruneUnknown != nil {
					return errPruneUnknown
				}
			}
			if errPrune := tx.Where("credential_id = ? AND expires_at IS NOT NULL AND expires_at <= ?", input.CredentialID, incomingObservedAt).
				Delete(&QuotaWindowRecord{}).Error; errPrune != nil {
				return errPrune
			}
		}
		for _, window := range input.Windows {
			windowRecord := quotaWindowRecordFromDTO(input.CredentialID, window)
			var current QuotaWindowRecord
			errCurrent := tx.First(&current, "credential_id = ? AND window_id = ?", input.CredentialID, window.ID).Error
			if errCurrent != nil && !errors.Is(errCurrent, gorm.ErrRecordNotFound) {
				return errCurrent
			}
			currentObservationInvalid := errCurrent == nil && input.MaxAcceptedObservedAt != nil && current.ObservedAt.After(input.MaxAcceptedObservedAt.UTC())
			if errCurrent == nil && !currentObservationInvalid && current.ObservedAt.After(window.ObservedAt) {
				continue
			}
			if errCurrent == nil {
				windowRecord.CreatedAt = current.CreatedAt
			}
			if errSave := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "credential_id"}, {Name: "window_id"}},
				UpdateAll: true,
			}).Create(&windowRecord).Error; errSave != nil {
				return errSave
			}
		}
		if !input.ReplaceWindows && len(input.Windows) > 0 {
			var merged []QuotaWindowRecord
			mergedQuery := tx.Where("credential_id = ?", input.CredentialID)
			if input.ObservedAt != nil {
				mergedQuery = mergedQuery.Where("expires_at IS NULL OR expires_at > ?", input.ObservedAt.UTC())
			}
			if errFind := mergedQuery.Find(&merged).Error; errFind != nil {
				return errFind
			}
			if errUpdate := tx.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", input.CredentialID).Updates(map[string]any{
				"quota_status": quotaWindowRecordAggregateStatus(merged), "source": quotaWindowRecordAggregateSource(merged), "updated_at": time.Now().UTC(),
			}).Error; errUpdate != nil {
				return errUpdate
			}
		}
		updated = true
		return nil
	})
	return updated, errTransaction
}

func createQuotaSnapshotRecord(tx *gorm.DB, record *QuotaSnapshotRecord, input QuotaSnapshotWrite) (bool, error) {
	if tx == nil || record == nil {
		return false, fmt.Errorf("quota snapshot write target is nil")
	}
	onConflict := clause.OnConflict{
		Columns:   []clause.Column{{Name: "credential_id"}},
		UpdateAll: true,
	}
	conditions := []string{`"quota_snapshot"."observed_at" IS NULL`}
	values := make([]any, 0, 2)
	if input.ObservedAt != nil {
		conditions = append(conditions, `"quota_snapshot"."observed_at" <= ?`)
		values = append(values, input.ObservedAt.UTC())
	}
	if input.MaxAcceptedObservedAt != nil {
		conditions = append(conditions, `"quota_snapshot"."observed_at" > ?`)
		values = append(values, input.MaxAcceptedObservedAt.UTC())
	}
	onConflict.Where = clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: strings.Join(conditions, " OR "), Vars: values}}}
	resultSave := tx.Clauses(onConflict).Create(record)
	if resultSave.Error != nil {
		return false, resultSave.Error
	}
	return resultSave.RowsAffected > 0, nil
}

func preserveQuotaProbeSchedule(record *QuotaSnapshotRecord, existing QuotaSnapshotRecord, observedAt *time.Time, receivedAt *time.Time) {
	if record == nil {
		return
	}
	leaseReferenceAt := observedAt
	if receivedAt != nil {
		leaseReferenceAt = receivedAt
	}
	leaseActive := existing.ProbeLeaseOwner != "" && existing.ProbeLeaseExpiresAt != nil && leaseReferenceAt != nil && existing.ProbeLeaseExpiresAt.After(leaseReferenceAt.UTC())
	authoritativeSource := quotaAllowed(strings.TrimSpace(existing.Source), "active_probe", "mixed")
	if existing.NextProbeAt != nil && (leaseActive || authoritativeSource || existing.CollectionStatus == "failed") {
		record.NextProbeAt = quotaUTC(existing.NextProbeAt)
	}
	if authoritativeSource || leaseActive || existing.CollectionStatus == "failed" {
		record.LastAttemptAt = quotaUTC(existing.LastAttemptAt)
		record.LastSuccessAt = quotaUTC(existing.LastSuccessAt)
	}
	if leaseActive || existing.CollectionStatus == "failed" || authoritativeSource && existing.CollectionStatus == "partial" {
		preserveQuotaProbeResult(record, existing)
		return
	}
	if !quotaAuthoritativeSnapshotFreshAndComplete(existing, observedAt) {
		return
	}
	record.CollectionStatus = existing.CollectionStatus
	record.ExpiresAt = quotaUTC(existing.ExpiresAt)
}

func preserveQuotaProbeResult(record *QuotaSnapshotRecord, existing QuotaSnapshotRecord) {
	record.CollectionStatus = existing.CollectionStatus
	if existing.ExpiresAt != nil {
		record.ExpiresAt = quotaUTC(existing.ExpiresAt)
	}
	record.ConsecutiveFailure = existing.ConsecutiveFailure
	record.ErrorCode = existing.ErrorCode
	record.ErrorMessage = existing.ErrorMessage
	record.ErrorRetryable = existing.ErrorRetryable
	record.ErrorOccurredAt = quotaUTC(existing.ErrorOccurredAt)
	record.ErrorStatusCode = existing.ErrorStatusCode
	record.ErrorRequestID = existing.ErrorRequestID
}

func quotaAuthoritativeSnapshotFreshAndComplete(existing QuotaSnapshotRecord, observedAt *time.Time) bool {
	if observedAt == nil || existing.ExpiresAt == nil || !existing.ExpiresAt.After(observedAt.UTC()) {
		return false
	}
	if existing.CollectionStatus != "success" {
		return false
	}
	switch strings.TrimSpace(existing.Source) {
	case "active_probe", "mixed":
		return true
	default:
		return false
	}
}

func quotaWindowRecordAggregateStatus(windows []QuotaWindowRecord) string {
	status := "unknown"
	for _, window := range windows {
		switch window.Status {
		case "exhausted":
			return "exhausted"
		case "low":
			status = "low"
		case "error":
			if status != "low" {
				status = "error"
			}
		case "healthy":
			if status == "unknown" {
				status = "healthy"
			}
		}
	}
	return status
}

func quotaWindowRecordAggregateSource(windows []QuotaWindowRecord) string {
	source := ""
	for _, window := range windows {
		candidate := strings.TrimSpace(window.Source)
		if candidate == "" {
			continue
		}
		if source == "" {
			source = candidate
			continue
		}
		if source != candidate {
			return "mixed"
		}
	}
	return source
}

func (r *Repository) ClaimQuotaProbe(ctx context.Context, credentialID string, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	return r.claimQuotaProbe(ctx, credentialID, owner, now, leaseDuration, false, false)
}

// ClaimEligibleQuotaProbe claims a lease only while the current DB credential remains eligible.
func (r *Repository) ClaimEligibleQuotaProbe(ctx context.Context, credentialID string, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	return r.claimQuotaProbe(ctx, credentialID, owner, now, leaseDuration, true, false)
}

// ForceClaimEligibleQuotaProbe claims an eligible credential while ignoring its
// snapshot freshness and retry schedule. An unexpired probe lease still wins.
func (r *Repository) ForceClaimEligibleQuotaProbe(ctx context.Context, credentialID string, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	return r.claimQuotaProbe(ctx, credentialID, owner, now, leaseDuration, true, true)
}

func (r *Repository) claimQuotaProbe(ctx context.Context, credentialID string, owner string, now time.Time, leaseDuration time.Duration, requireEligible bool, force bool) (bool, error) {
	credentialID = strings.TrimSpace(credentialID)
	owner = strings.TrimSpace(owner)
	if credentialID == "" || owner == "" {
		return false, fmt.Errorf("quota probe credential and owner are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	claimed := false
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		targetCollectorVersion := quotaSnapshotSchemaVersion
		if requireEligible {
			var authRecord AuthRecord
			errAuth := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&authRecord, "uuid = ?", credentialID).Error
			if errors.Is(errAuth, gorm.ErrRecordNotFound) {
				return nil
			}
			if errAuth != nil {
				return errAuth
			}
			auth, errConvert := quotaAuthFromRecord(&authRecord)
			if errConvert != nil {
				return errConvert
			}
			provider := normalizeQuotaProviderID(authRecord.Provider)
			credentialType := quotaCredentialType(auth)
			if !quotaCredentialCollectorPlanned(provider, credentialType) || authRecord.Disabled || authRecord.Status == coreauth.StatusDisabled || authRecord.NextRetryAfter != nil && authRecord.NextRetryAfter.After(now) {
				return nil
			}
			targetCollectorVersion = QuotaSnapshotVersion(provider)
		}
		var record QuotaSnapshotRecord
		errFind := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "credential_id = ?", credentialID).Error
		if errFind != nil && !errors.Is(errFind, gorm.ErrRecordNotFound) {
			return errFind
		}
		observationInFuture := false
		upgradeClaim := false
		if errFind == nil {
			if record.ProbeLeaseExpiresAt != nil && record.ProbeLeaseExpiresAt.After(now) {
				return nil
			}
			upgradeClaim = record.CollectorVersion < targetCollectorVersion
			observationInFuture = record.ObservedAt != nil && record.ObservedAt.After(now.Add(quotaMaxFutureObservationSkew))
			if !force && !observationInFuture && !upgradeClaim {
				if record.NextProbeAt != nil && record.NextProbeAt.After(now) {
					return nil
				}
				if record.NextProbeAt == nil && record.ExpiresAt != nil && record.ExpiresAt.After(now) {
					return nil
				}
			}
		}
		leaseExpiresAt := now.Add(leaseDuration)
		if errors.Is(errFind, gorm.ErrRecordNotFound) {
			record = QuotaSnapshotRecord{
				CredentialID: credentialID, QuotaStatus: "unknown", CollectionStatus: "collecting",
				LastAttemptAt: &now, ProbeLeaseOwner: owner, ProbeLeaseExpiresAt: &leaseExpiresAt,
				ParserVersion: quotaSnapshotSchemaVersion, CollectorVersion: targetCollectorVersion,
				CreatedAt: now, UpdatedAt: now,
			}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
		} else {
			updates := map[string]any{
				"collection_status": "collecting", "last_attempt_at": now, "probe_lease_owner": owner,
				"probe_lease_expires_at": leaseExpiresAt, "updated_at": now,
			}
			if observationInFuture {
				if errDeleteWindows := tx.Where("credential_id = ?", credentialID).Delete(&QuotaWindowRecord{}).Error; errDeleteWindows != nil {
					return errDeleteWindows
				}
				updates["quota_status"] = "unknown"
				updates["source"] = ""
				updates["observed_at"] = nil
				updates["expires_at"] = nil
				updates["last_success_at"] = nil
				updates["next_probe_at"] = nil
				updates["consecutive_failures"] = 0
				updates["error_code"] = ""
				updates["error_message"] = ""
				updates["error_retryable"] = false
				updates["error_occurred_at"] = nil
				updates["error_status_code"] = 0
				updates["error_request_id"] = ""
			}
			if upgradeClaim {
				updates["collector_version"] = targetCollectorVersion
				// Mark the forced upgrade attempt before probing. A completed
				// failure replaces this immediate retry with the normal backoff;
				// a crashed attempt can retry after its lease expires.
				updates["next_probe_at"] = now
			}
			if errUpdate := tx.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", credentialID).Updates(updates).Error; errUpdate != nil {
				return errUpdate
			}
		}
		claimed = true
		return nil
	})
	return claimed, errTransaction
}

func (r *Repository) FailQuotaProbe(ctx context.Context, credentialID string, owner string, failure QuotaCollectionError, nextProbeAt time.Time) error {
	return r.FailQuotaProbeAt(ctx, credentialID, owner, failure, nextProbeAt, time.Now().UTC())
}

// FailQuotaProbeAt persists a failed probe using the supplied clock value.
func (r *Repository) FailQuotaProbeAt(ctx context.Context, credentialID string, owner string, failure QuotaCollectionError, nextProbeAt time.Time, now time.Time) error {
	credentialID = strings.TrimSpace(credentialID)
	owner = strings.TrimSpace(owner)
	if credentialID == "" || owner == "" {
		return fmt.Errorf("quota probe credential and owner are required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	updates := map[string]any{
		"collection_status": "failed", "last_attempt_at": now, "next_probe_at": quotaUTC(&nextProbeAt),
		"consecutive_failures": gorm.Expr("consecutive_failures + 1"), "error_code": strings.TrimSpace(failure.Code),
		"error_message": quotaSafeErrorMessage(failure.Message), "error_retryable": failure.Retryable,
		"error_occurred_at": quotaUTC(failure.OccurredAt), "error_status_code": 0, "error_request_id": "",
		"probe_lease_owner": "", "probe_lease_expires_at": nil, "updated_at": now,
	}
	updates["quota_status"] = gorm.Expr("CASE WHEN observed_at IS NULL THEN ? ELSE quota_status END", "error")
	if failure.UpstreamStatusCode != nil {
		updates["error_status_code"] = *failure.UpstreamStatusCode
	}
	if failure.RequestID != nil {
		updates["error_request_id"] = quotaBoundedRequestID(*failure.RequestID)
	}
	result := db.WithContext(contextOrBackground(ctx)).Model(&QuotaSnapshotRecord{}).
		Where("credential_id = ? AND probe_lease_owner = ?", credentialID, owner).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrQuotaProbeLeaseLost
	}
	return nil
}

func (r *Repository) ListQuotaCredentials(ctx context.Context, query QuotaListQuery) (QuotaListResult, error) {
	all, errList := r.loadQuotaCredentials(ctx, query.Now, nil, len(query.IDs) == 0)
	if errList != nil {
		return QuotaListResult{}, errList
	}
	candidates := all
	if len(query.IDs) > 0 {
		candidates, errList = r.loadQuotaCredentials(ctx, query.Now, query.IDs, true)
		if errList != nil {
			return QuotaListResult{}, errList
		}
	}
	filtered := make([]QuotaCredentialSnapshot, 0, len(candidates))
	for _, item := range candidates {
		if quotaCredentialMatches(item, query) {
			filtered = append(filtered, item)
		}
	}
	quotaSortCredentials(filtered, query.Sort)
	result := QuotaListResult{
		Total:         len(filtered),
		Summary:       quotaSummary(filtered),
		GlobalSummary: quotaSummary(all),
		Facets:        quotaFacets(filtered),
	}
	start := query.Offset
	if start < 0 {
		start = 0
	}
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + query.Limit
	if query.Limit <= 0 || end > len(filtered) {
		end = len(filtered)
	}
	result.Items = append([]QuotaCredentialSnapshot{}, filtered[start:end]...)
	return result, nil
}

func (r *Repository) GetQuotaCredential(ctx context.Context, credentialID string, now time.Time) (*QuotaCredentialSnapshot, error) {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	ctx = contextOrBackground(ctx)
	var record AuthRecord
	if errFind := db.WithContext(ctx).Where("uuid = ?", credentialID).First(&record).Error; errFind != nil {
		return nil, errFind
	}
	auth, errAuth := quotaAuthFromRecord(&record)
	if errAuth != nil {
		return nil, errAuth
	}
	var snapshot QuotaSnapshotRecord
	errSnapshot := db.WithContext(ctx).Where("credential_id = ?", credentialID).First(&snapshot).Error
	if errSnapshot != nil && !errors.Is(errSnapshot, gorm.ErrRecordNotFound) {
		return nil, errSnapshot
	}
	var windowRecords []QuotaWindowRecord
	if errWindows := db.WithContext(ctx).
		Where("credential_id = ?", credentialID).
		Order("priority ASC, reset_at ASC, window_id ASC").
		Find(&windowRecords).Error; errWindows != nil {
		return nil, errWindows
	}
	windows := make([]QuotaWindow, 0, len(windowRecords))
	for _, windowRecord := range windowRecords {
		windows = append(windows, quotaWindowDTO(windowRecord))
	}
	item := quotaCredentialFromAuth(record, auth, snapshot, windows, now)
	return &item, nil
}

func (r *Repository) loadQuotaCredentials(ctx context.Context, now time.Time, ids map[string]struct{}, includeWindows bool) ([]QuotaCredentialSnapshot, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	ctx = contextOrBackground(ctx)
	idValues := quotaExactSetValues(ids)
	var authRecords []AuthRecord
	authQuery := db.WithContext(ctx).Order("uuid ASC")
	if len(idValues) > 0 {
		authQuery = authQuery.Where("uuid IN ?", idValues)
	}
	if errFind := authQuery.Find(&authRecords).Error; errFind != nil {
		return nil, errFind
	}
	var snapshots []QuotaSnapshotRecord
	snapshotQuery := db.WithContext(ctx)
	if len(idValues) > 0 {
		snapshotQuery = snapshotQuery.Where("credential_id IN ?", idValues)
	}
	if errFind := snapshotQuery.Find(&snapshots).Error; errFind != nil {
		return nil, errFind
	}
	var windowRecords []QuotaWindowRecord
	if includeWindows {
		windowQuery := db.WithContext(ctx).Order("credential_id ASC, priority ASC, window_id ASC")
		if len(idValues) > 0 {
			windowQuery = windowQuery.Where("credential_id IN ?", idValues)
		}
		if errFind := windowQuery.Find(&windowRecords).Error; errFind != nil {
			return nil, errFind
		}
	}
	snapshotByCredential := make(map[string]QuotaSnapshotRecord, len(snapshots))
	for _, snapshot := range snapshots {
		snapshotByCredential[snapshot.CredentialID] = snapshot
	}
	windowsByCredential := make(map[string][]QuotaWindow)
	for _, record := range windowRecords {
		windowsByCredential[record.CredentialID] = append(windowsByCredential[record.CredentialID], quotaWindowDTO(record))
	}
	items := make([]QuotaCredentialSnapshot, 0, len(authRecords))
	for index := range authRecords {
		auth, errAuth := quotaAuthFromRecord(&authRecords[index])
		if errAuth != nil {
			return nil, errAuth
		}
		item := quotaCredentialFromAuth(authRecords[index], auth, snapshotByCredential[authRecords[index].UUID], windowsByCredential[authRecords[index].UUID], now)
		items = append(items, item)
	}
	return items, nil
}

func quotaAuthFromRecord(record *AuthRecord) (*coreauth.Auth, error) {
	if record == nil || strings.TrimSpace(record.UUID) == "" {
		return nil, fmt.Errorf("quota auth record uuid is required")
	}
	auth := &coreauth.Auth{}
	if errUnmarshal := json.Unmarshal([]byte(record.AuthJSON), auth); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	auth.ID = strings.TrimSpace(record.UUID)
	auth.Index = firstNonEmptyQuotaString(record.Index, record.UUID)
	auth.Provider = strings.TrimSpace(record.Provider)
	return auth, nil
}

func quotaCredentialFromAuth(record AuthRecord, auth *coreauth.Auth, snapshot QuotaSnapshotRecord, windows []QuotaWindow, now time.Time) QuotaCredentialSnapshot {
	credentialID := strings.TrimSpace(record.UUID)
	authIndex := quotaOptionalString(firstNonEmptyQuotaString(record.Index, record.UUID))
	provider := normalizeQuotaProviderID(record.Provider)
	label := quotaSafeDisplayLabel(record.Label)
	if label == "" && auth != nil {
		label = quotaSafeDisplayLabel(firstQuotaMetadataString(auth.Metadata, "name"))
		if label == "" {
			label = quotaMaskAccount(firstQuotaMetadataString(auth.Metadata, "email", "account", "username"))
		}
	}
	if label == "" {
		label = provider
		if label == "" {
			label = "credential"
		}
		label += " " + credentialID
	}
	credentialType := quotaCredentialType(auth)
	item := QuotaCredentialSnapshot{
		CredentialID:     credentialID,
		AuthIndex:        authIndex,
		Provider:         firstNonEmptyQuotaString(provider, "unknown"),
		CredentialType:   credentialType,
		Label:            label,
		Account:          quotaOptionalString(quotaMaskAccount(firstQuotaMetadataString(quotaAuthMetadata(auth), "email", "account", "username"))),
		Project:          quotaOptionalString(quotaMaskIdentifier(firstQuotaMetadataString(quotaAuthMetadata(auth), "project_id", "project", "organization_id", "organization"))),
		CredentialStatus: quotaCredentialStatus(record, now),
		QuotaStatus:      "unknown",
		Freshness:        "never",
		CollectionStatus: "idle",
		PrimaryWindows:   []QuotaWindow{},
		Windows:          []QuotaWindow{},
	}
	if strings.TrimSpace(snapshot.CredentialID) == "" {
		if !quotaCredentialCollectorPlanned(provider, credentialType) {
			item.QuotaStatus = "unsupported"
			item.CollectionStatus = "unsupported"
		}
		return item
	}
	item.QuotaStatus = firstNonEmptyQuotaString(snapshot.QuotaStatus, "unknown")
	item.CollectionStatus = firstNonEmptyQuotaString(snapshot.CollectionStatus, "idle")
	item.Source = quotaOptionalString(snapshot.Source)
	item.ObservedAt = quotaUTC(snapshot.ObservedAt)
	item.ExpiresAt = quotaUTC(snapshot.ExpiresAt)
	item.LastAttemptAt = quotaUTC(snapshot.LastAttemptAt)
	item.LastSuccessAt = quotaUTC(snapshot.LastSuccessAt)
	item.NextProbeAt = quotaUTC(snapshot.NextProbeAt)
	item.ConsecutiveFailure = snapshot.ConsecutiveFailure
	if item.ObservedAt != nil {
		item.Freshness = "stale"
		if item.ExpiresAt != nil && now.Before(*item.ExpiresAt) {
			item.Freshness = "fresh"
		}
	}
	displayWindows := append([]QuotaWindow{}, windows...)
	codexLegacyPending := provider == "codex" && snapshot.ParserVersion < QuotaSnapshotVersion(provider) && codexHasLegacyQuotaWindows(displayWindows)
	if codexLegacyPending {
		currentWindows := codexCurrentQuotaWindows(displayWindows)
		if len(currentWindows) > 0 {
			// A new passive observation can arrive before the authoritative
			// active recollection completes. Hide incompatible legacy IDs from
			// the API while retaining their raw rows for diagnosis.
			displayWindows = currentWindows
		}
	}
	antigravityLegacyPending := provider == "antigravity" && snapshot.ParserVersion < QuotaSnapshotVersion(provider) && antigravityHasLegacyQuotaWindows(displayWindows)
	if antigravityLegacyPending {
		currentWindows := antigravityCurrentQuotaWindows(displayWindows)
		if len(currentWindows) > 0 {
			displayWindows = currentWindows
		}
	}
	if item.Freshness == "fresh" {
		validWindows := make([]QuotaWindow, 0, len(displayWindows))
		for _, window := range displayWindows {
			if window.ExpiresAt == nil || now.Before(*window.ExpiresAt) {
				validWindows = append(validWindows, window)
			}
		}
		displayWindows = validWindows
		if len(displayWindows) > 0 {
			item.QuotaStatus = quotaWindowAggregateDTOStatus(displayWindows)
			item.Source = quotaOptionalString(quotaWindowAggregateDTOSource(displayWindows))
		}
	}
	item.Windows = displayWindows
	item.WindowCount = len(displayWindows)
	item.EarliestResetAt = quotaEarliestReset(displayWindows)
	item.Error = quotaCollectionErrorFromRecord(snapshot)
	if snapshot.HomeID != "" || snapshot.CPANodeID != "" || snapshot.HomeLabel != "" || snapshot.CPANodeLabel != "" {
		item.Runtime = &QuotaRuntime{HomeID: snapshot.HomeID, HomeLabel: snapshot.HomeLabel, CPANodeID: snapshot.CPANodeID, CPANodeLabel: snapshot.CPANodeLabel}
	}
	if snapshot.PlanName != "" {
		item.Plan = &QuotaPlan{Name: snapshot.PlanName, Premium: snapshot.PlanPremium != nil && *snapshot.PlanPremium}
	}
	item.ResetCredits = quotaResetCreditsFromJSON(snapshot.ResetCredits)
	item.PrimaryWindows = quotaPrimaryWindows(provider, displayWindows)
	legacyOnly := (codexLegacyPending && len(codexCurrentQuotaWindows(displayWindows)) == 0) ||
		(antigravityLegacyPending && len(antigravityCurrentQuotaWindows(displayWindows)) == 0)
	if legacyOnly {
		item.QuotaStatus = "unknown"
		item.Freshness = "stale"
		item.PrimaryWindows = []QuotaWindow{}
		item.EarliestResetAt = nil
	}
	if legacyOnly && snapshot.CollectorVersion >= QuotaSnapshotVersion(provider) && item.CollectionStatus == "failed" {
		item.QuotaStatus = "error"
		item.Freshness = "stale"
		item.PrimaryWindows = []QuotaWindow{}
	}
	return item
}

func quotaWindowAggregateDTOStatus(windows []QuotaWindow) string {
	status := "unknown"
	for _, window := range windows {
		switch window.Status {
		case "exhausted":
			return "exhausted"
		case "low":
			status = "low"
		case "error":
			if status != "low" {
				status = "error"
			}
		case "healthy":
			if status == "unknown" {
				status = "healthy"
			}
		}
	}
	return status
}

func quotaWindowAggregateDTOSource(windows []QuotaWindow) string {
	source := ""
	for _, window := range windows {
		candidate := strings.TrimSpace(window.Source)
		if candidate == "" {
			continue
		}
		if source == "" {
			source = candidate
			continue
		}
		if source != candidate {
			return "mixed"
		}
	}
	return source
}

func quotaCredentialCollectorPlanned(provider string, credentialType string) bool {
	if !quotaProviderPlanned(provider) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(credentialType)) {
	case "oauth", "file_auth":
		return true
	default:
		return false
	}
}

func validateQuotaSnapshotWrite(input *QuotaSnapshotWrite) error {
	if input == nil || strings.TrimSpace(input.CredentialID) == "" {
		return fmt.Errorf("quota credential id is required")
	}
	input.CredentialID = strings.TrimSpace(input.CredentialID)
	if len(input.CredentialID) > quotaCredentialIDMaxLength {
		return fmt.Errorf("quota credential id exceeds %d bytes", quotaCredentialIDMaxLength)
	}
	if input.ParserVersion <= 0 {
		input.ParserVersion = quotaSnapshotSchemaVersion
	}
	if input.CollectorVersion <= 0 {
		input.CollectorVersion = quotaSnapshotSchemaVersion
	}
	if !quotaAllowed(input.QuotaStatus, "healthy", "low", "exhausted", "unknown", "error", "unsupported") {
		return fmt.Errorf("unsupported quota status %q", input.QuotaStatus)
	}
	if !quotaAllowed(input.CollectionStatus, "idle", "collecting", "success", "partial", "failed", "unsupported") {
		return fmt.Errorf("unsupported quota collection status %q", input.CollectionStatus)
	}
	input.Source = strings.TrimSpace(input.Source)
	if input.Source != "" && !quotaAllowed(input.Source, "response_header", "active_probe", "mixed") {
		return fmt.Errorf("unsupported quota source %q", input.Source)
	}
	if input.Plan != nil {
		input.Plan.Name = quotaBoundedText(input.Plan.Name, 64)
		if input.Plan.Name == "" {
			return fmt.Errorf("quota plan name is required")
		}
	}
	if errResetCredits := normalizeQuotaResetCredits(input.ResetCredits); errResetCredits != nil {
		return errResetCredits
	}
	if input.ConsecutiveFailure < 0 {
		return fmt.Errorf("quota consecutive failures must be non-negative")
	}
	if input.MaxAcceptedObservedAt != nil {
		maxAcceptedObservedAt := input.MaxAcceptedObservedAt.UTC()
		input.MaxAcceptedObservedAt = &maxAcceptedObservedAt
	}
	if input.ReceivedAt != nil {
		if input.ReceivedAt.IsZero() {
			return fmt.Errorf("quota received_at is required when provided")
		}
		receivedAt := input.ReceivedAt.UTC()
		input.ReceivedAt = &receivedAt
	}
	if input.ObservedAt != nil {
		observedAt := input.ObservedAt.UTC()
		input.ObservedAt = &observedAt
		if input.ExpiresAt == nil {
			expiresAt := observedAt.Add(quotaSnapshotFallbackFreshness)
			input.ExpiresAt = &expiresAt
		}
	}
	seen := make(map[string]struct{}, len(input.Windows))
	for index := range input.Windows {
		window := &input.Windows[index]
		window.ID = quotaBoundedIdentifier(window.ID, quotaWindowTextMaxLength)
		if window.ID == "" {
			return fmt.Errorf("quota window id is required")
		}
		if _, ok := seen[window.ID]; ok {
			return fmt.Errorf("duplicate quota window id %q", window.ID)
		}
		seen[window.ID] = struct{}{}
		window.Label = quotaBoundedOptionalText(window.Label, quotaWindowTextMaxLength)
		window.ScopeID = quotaBoundedOptionalText(window.ScopeID, quotaWindowTextMaxLength)
		window.Currency = quotaBoundedOptionalText(window.Currency, quotaCurrencyMaxLength)
		window.Scope = firstNonEmptyQuotaString(window.Scope, "unknown")
		window.Mode = firstNonEmptyQuotaString(window.Mode, "unknown")
		window.Status = firstNonEmptyQuotaString(window.Status, "unknown")
		window.Unit = firstNonEmptyQuotaString(window.Unit, "unknown")
		window.PeriodUnit = firstNonEmptyQuotaString(window.PeriodUnit, "unknown")
		window.Source = firstNonEmptyQuotaString(window.Source, input.Source)
		if !quotaAllowed(window.Scope, "account", "project", "model", "organization", "unknown") {
			return fmt.Errorf("quota window %q has unsupported scope %q", window.ID, window.Scope)
		}
		if !quotaAllowed(window.Mode, "rolling", "fixed", "balance", "unknown") {
			return fmt.Errorf("quota window %q has unsupported mode %q", window.ID, window.Mode)
		}
		if !quotaAllowed(window.Status, "healthy", "low", "exhausted", "unknown", "error", "unsupported") {
			return fmt.Errorf("quota window %q has unsupported status %q", window.ID, window.Status)
		}
		if !quotaAllowed(window.Source, "response_header", "active_probe", "mixed") {
			return fmt.Errorf("quota window %q has unsupported source %q", window.ID, window.Source)
		}
		if window.ObservedAt.IsZero() {
			return fmt.Errorf("quota window %q observed_at is required", window.ID)
		}
		window.ObservedAt = window.ObservedAt.UTC()
		if !quotaFiniteOptional(window.Used) || !quotaFiniteOptional(window.Remaining) || !quotaFiniteOptional(window.Limit) || !quotaRatio(window.UsedRatio) || !quotaRatio(window.RemainingRatio) {
			return fmt.Errorf("quota window %q contains invalid numeric values", window.ID)
		}
		NormalizeQuotaWindowValues(window)
		if window.ExpiresAt == nil {
			window.ExpiresAt = quotaUTC(input.ExpiresAt)
		} else {
			window.ExpiresAt = quotaUTC(window.ExpiresAt)
		}
		if window.PeriodUnit == "unknown" && window.PeriodValue != nil {
			return fmt.Errorf("quota window %q period_value must be null for unknown period", window.ID)
		}
		if window.PeriodUnit != "unknown" && (window.PeriodValue == nil || *window.PeriodValue <= 0 || !quotaFiniteOptional(window.PeriodValue)) {
			return fmt.Errorf("quota window %q requires a positive period_value", window.ID)
		}
		if window.WindowSeconds != nil && *window.WindowSeconds <= 0 {
			return fmt.Errorf("quota window %q window_seconds must be positive", window.ID)
		}
	}
	return nil
}

func quotaSnapshotRecordFromWrite(input QuotaSnapshotWrite) (QuotaSnapshotRecord, error) {
	now := time.Now().UTC()
	record := QuotaSnapshotRecord{
		CredentialID: input.CredentialID, QuotaStatus: input.QuotaStatus, CollectionStatus: input.CollectionStatus,
		Source: strings.TrimSpace(input.Source), ObservedAt: quotaUTC(input.ObservedAt), ExpiresAt: quotaUTC(input.ExpiresAt),
		LastAttemptAt: quotaUTC(input.LastAttemptAt), LastSuccessAt: quotaUTC(input.LastSuccessAt), NextProbeAt: quotaUTC(input.NextProbeAt),
		ConsecutiveFailure: input.ConsecutiveFailure, ParserVersion: input.ParserVersion, CollectorVersion: input.CollectorVersion,
		CreatedAt: now, UpdatedAt: now,
	}
	if input.Runtime != nil {
		record.HomeID = quotaBoundedText(input.Runtime.HomeID, quotaWindowTextMaxLength)
		record.HomeLabel = quotaBoundedText(input.Runtime.HomeLabel, quotaWindowTextMaxLength)
		record.CPANodeID = quotaBoundedText(input.Runtime.CPANodeID, quotaWindowTextMaxLength)
		record.CPANodeLabel = quotaBoundedText(input.Runtime.CPANodeLabel, quotaWindowTextMaxLength)
	}
	if input.Plan != nil {
		record.PlanName = quotaBoundedText(input.Plan.Name, 64)
		premium := input.Plan.Premium
		record.PlanPremium = &premium
	}
	if input.ResetCredits != nil {
		resetCredits, errResetCredits := quotaResetCreditsJSON(input.ResetCredits)
		if errResetCredits != nil {
			return QuotaSnapshotRecord{}, errResetCredits
		}
		record.ResetCredits = resetCredits
	}
	if input.Error != nil {
		record.ErrorCode = strings.TrimSpace(input.Error.Code)
		record.ErrorMessage = quotaSafeErrorMessage(input.Error.Message)
		record.ErrorRetryable = input.Error.Retryable
		record.ErrorOccurredAt = quotaUTC(input.Error.OccurredAt)
		if input.Error.UpstreamStatusCode != nil {
			record.ErrorStatusCode = *input.Error.UpstreamStatusCode
		}
		if input.Error.RequestID != nil {
			record.ErrorRequestID = quotaBoundedRequestID(*input.Error.RequestID)
		}
	}
	return record, nil
}

func normalizeQuotaResetCredits(value *QuotaResetCredits) error {
	if value == nil {
		return nil
	}
	if value.ObservedAt.IsZero() {
		return fmt.Errorf("quota reset credits observed_at is required")
	}
	value.ObservedAt = value.ObservedAt.UTC()
	if value.AvailableCount != nil && (*value.AvailableCount < 0 || *value.AvailableCount > 1_000_000) {
		return fmt.Errorf("quota reset credits available_count is out of range")
	}
	if len(value.Credits) > quotaResetCreditMaxItems {
		return fmt.Errorf("quota reset credits exceed %d items", quotaResetCreditMaxItems)
	}
	seen := make(map[string]struct{}, len(value.Credits))
	for index := range value.Credits {
		credit := &value.Credits[index]
		credit.ID = quotaBoundedIdentifier(credit.ID, quotaResetCreditIDMaxLength)
		if credit.ID == "" {
			return fmt.Errorf("quota reset credit id is required")
		}
		if _, exists := seen[credit.ID]; exists {
			return fmt.Errorf("duplicate quota reset credit id %q", credit.ID)
		}
		seen[credit.ID] = struct{}{}
		credit.Status = strings.ToLower(strings.TrimSpace(credit.Status))
		if credit.Status != "available" {
			return fmt.Errorf("quota reset credit %q has unsupported status %q", credit.ID, credit.Status)
		}
		if credit.GrantedAt.IsZero() {
			return fmt.Errorf("quota reset credit %q granted_at is required", credit.ID)
		}
		credit.GrantedAt = credit.GrantedAt.UTC()
		credit.ExpiresAt = quotaUTC(credit.ExpiresAt)
	}
	sort.SliceStable(value.Credits, func(i, j int) bool {
		leftExpiry := value.Credits[i].ExpiresAt
		rightExpiry := value.Credits[j].ExpiresAt
		if leftExpiry == nil || rightExpiry == nil {
			if leftExpiry != nil {
				return true
			}
			if rightExpiry != nil {
				return false
			}
		} else if !leftExpiry.Equal(*rightExpiry) {
			return leftExpiry.Before(*rightExpiry)
		}
		return value.Credits[i].ID < value.Credits[j].ID
	})
	if value.Credits == nil {
		value.Credits = []QuotaResetCredit{}
	}
	if value.AvailableCount == nil || *value.AvailableCount < len(value.Credits) {
		availableCount := len(value.Credits)
		value.AvailableCount = &availableCount
	}
	return nil
}

func quotaResetCreditsJSON(value *QuotaResetCredits) (JSONB, error) {
	if value == nil {
		return nil, nil
	}
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode quota reset credits: %w", errMarshal)
	}
	return JSONB(raw), nil
}

func quotaResetCreditsFromJSON(raw JSONB) *QuotaResetCredits {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value QuotaResetCredits
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return nil
	}
	if errNormalize := normalizeQuotaResetCredits(&value); errNormalize != nil {
		return nil
	}
	return &value
}

func quotaWindowRecordFromDTO(credentialID string, window QuotaWindow) QuotaWindowRecord {
	now := time.Now().UTC()
	return QuotaWindowRecord{
		CredentialID: credentialID, WindowID: window.ID, Priority: window.Priority, Label: quotaStringValue(window.Label),
		Scope: window.Scope, ScopeID: quotaStringValue(window.ScopeID), Mode: window.Mode, Status: window.Status, Unit: window.Unit,
		Currency: quotaStringValue(window.Currency), Used: window.Used, Remaining: window.Remaining, Limit: window.Limit,
		UsedRatio: window.UsedRatio, RemainingRatio: window.RemainingRatio, IsUnlimited: window.IsUnlimited,
		ResetAt: quotaUTC(window.ResetAt), WindowSeconds: window.WindowSeconds, PeriodUnit: window.PeriodUnit, PeriodValue: window.PeriodValue,
		Source: window.Source, ObservedAt: window.ObservedAt.UTC(), ExpiresAt: quotaUTC(window.ExpiresAt), CreatedAt: now, UpdatedAt: now,
	}
}

func quotaWindowDTO(record QuotaWindowRecord) QuotaWindow {
	return QuotaWindow{
		ID: record.WindowID, Label: quotaOptionalString(record.Label), Scope: record.Scope, ScopeID: quotaOptionalString(record.ScopeID),
		Mode: record.Mode, Status: record.Status, Unit: record.Unit, Currency: quotaOptionalString(record.Currency), Used: record.Used,
		Remaining: record.Remaining, Limit: record.Limit, UsedRatio: record.UsedRatio, RemainingRatio: record.RemainingRatio,
		IsUnlimited: record.IsUnlimited, ResetAt: quotaUTC(record.ResetAt), WindowSeconds: record.WindowSeconds,
		PeriodUnit: record.PeriodUnit, PeriodValue: record.PeriodValue, Source: record.Source, ObservedAt: record.ObservedAt.UTC(), ExpiresAt: quotaUTC(record.ExpiresAt), Priority: record.Priority,
	}
}

func quotaCredentialMatches(item QuotaCredentialSnapshot, query QuotaListQuery) bool {
	if !quotaExactSetMatches(query.IDs, item.CredentialID) || !quotaSetMatches(query.Providers, item.Provider) || !quotaSetMatches(query.QuotaStatuses, item.QuotaStatus) ||
		!quotaSetMatches(query.Freshness, item.Freshness) || !quotaSetMatches(query.Sources, quotaSourceValue(item.Source)) ||
		!quotaSetMatches(query.CredentialStatuses, item.CredentialStatus) || !quotaSetMatches(query.CollectionStatuses, item.CollectionStatus) {
		return false
	}
	search := strings.ToLower(strings.TrimSpace(query.Search))
	if search == "" {
		return true
	}
	for _, value := range []string{item.Label, quotaStringValue(item.Account), quotaStringValue(item.Project), quotaStringValue(item.AuthIndex), item.Provider} {
		if strings.Contains(strings.ToLower(value), search) {
			return true
		}
	}
	return false
}

func quotaSortCredentials(items []QuotaCredentialSnapshot, sortValue string) {
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		switch sortValue {
		case "observed_at_desc", "observed_at_asc":
			if (left.ObservedAt == nil) != (right.ObservedAt == nil) {
				return left.ObservedAt != nil
			}
			comparison := quotaCompareTime(left.ObservedAt, right.ObservedAt)
			if comparison != 0 {
				if sortValue == "observed_at_desc" {
					return comparison > 0
				}
				return comparison < 0
			}
		case "reset_at_asc":
			comparison := quotaCompareTime(left.EarliestResetAt, right.EarliestResetAt)
			if comparison != 0 {
				return comparison < 0
			}
		case "provider_asc":
			if left.Provider != right.Provider {
				return left.Provider < right.Provider
			}
		case "label_asc":
			leftLabel, rightLabel := strings.ToLower(left.Label), strings.ToLower(right.Label)
			if leftLabel != rightLabel {
				return leftLabel < rightLabel
			}
		default:
			leftRisk, rightRisk := quotaRiskRank(left), quotaRiskRank(right)
			if leftRisk != rightRisk {
				return leftRisk < rightRisk
			}
		}
		return left.CredentialID < right.CredentialID
	})
}

func quotaSummary(items []QuotaCredentialSnapshot) QuotaSummary {
	summary := QuotaSummary{TotalCredentials: len(items)}
	for _, item := range items {
		switch item.QuotaStatus {
		case "healthy":
			summary.Healthy++
		case "low":
			summary.Low++
		case "exhausted":
			summary.Exhausted++
		case "error":
			summary.Error++
		case "unsupported":
			summary.Unsupported++
		default:
			summary.Unknown++
		}
		if item.Freshness == "stale" {
			summary.Stale++
		}
		if item.Freshness == "never" {
			summary.Never++
		}
		if item.CollectionStatus == "collecting" {
			summary.Collecting++
		}
		if item.QuotaStatus != "healthy" || item.Freshness != "fresh" || item.CollectionStatus == "partial" || item.CollectionStatus == "failed" {
			summary.NeedsAttention++
		}
		if item.ObservedAt != nil && (summary.LastObservedAt == nil || item.ObservedAt.After(*summary.LastObservedAt)) {
			value := item.ObservedAt.UTC()
			summary.LastObservedAt = &value
		}
	}
	return summary
}

func quotaFacets(items []QuotaCredentialSnapshot) QuotaFacets {
	providers, statuses, freshness, sources := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	credentialStatuses, collectionStatuses := map[string]int{}, map[string]int{}
	for _, item := range items {
		providers[item.Provider]++
		statuses[item.QuotaStatus]++
		freshness[item.Freshness]++
		sources[quotaSourceValue(item.Source)]++
		credentialStatuses[item.CredentialStatus]++
		collectionStatuses[item.CollectionStatus]++
	}
	return QuotaFacets{
		Providers: quotaFacetOptions(providers), QuotaStatuses: quotaFacetOptions(statuses), Freshness: quotaFacetOptions(freshness),
		Sources: quotaFacetOptions(sources), CredentialStatuses: quotaFacetOptions(credentialStatuses), CollectionStatuses: quotaFacetOptions(collectionStatuses),
	}
}

func quotaFacetOptions(counts map[string]int) []QuotaFacetOption {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]QuotaFacetOption, 0, len(keys))
	for _, key := range keys {
		items = append(items, QuotaFacetOption{Value: key, Count: counts[key]})
	}
	return items
}

func quotaPrimaryWindows(provider string, windows []QuotaWindow) []QuotaWindow {
	items := append([]QuotaWindow(nil), windows...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		leftRisk, rightRisk := quotaWindowRisk(items[i].Status), quotaWindowRisk(items[j].Status)
		if leftRisk != rightRisk {
			return leftRisk < rightRisk
		}
		return items[i].ID < items[j].ID
	})
	switch normalizeQuotaProviderID(provider) {
	case "antigravity":
		return antigravityPrimaryWindows(items)
	case "codex":
		return codexPrimaryWindows(items)
	}
	if len(items) > 2 {
		items = items[:2]
	}
	if items == nil {
		return []QuotaWindow{}
	}
	return items
}

func antigravityPrimaryWindows(items []QuotaWindow) []QuotaWindow {
	selected := make([]QuotaWindow, 0, 2)
	for _, scopeID := range []string{"gemini", "third-party"} {
		for _, window := range items {
			if window.ScopeID != nil && strings.EqualFold(strings.TrimSpace(*window.ScopeID), scopeID) {
				selected = append(selected, window)
				break
			}
		}
	}
	return selected
}

func codexPrimaryWindows(items []QuotaWindow) []QuotaWindow {
	selected := make([]QuotaWindow, 0, 2)
	selectedIDs := make(map[string]struct{}, 2)
	appendFamily := func(matches func(QuotaWindow) bool) {
		if len(selected) >= 2 {
			return
		}
		family := make([]QuotaWindow, 0, 2)
		for _, window := range items {
			if matches(window) {
				family = append(family, window)
			}
		}
		window, ok := quotaLongestWindow(family)
		if !ok {
			return
		}
		selected = append(selected, window)
		selectedIDs[window.ID] = struct{}{}
	}

	appendFamily(func(window QuotaWindow) bool {
		scopeID := strings.TrimSpace(quotaStringValue(window.ScopeID))
		return window.Scope == "account" && (scopeID == "" || strings.EqualFold(scopeID, "codex"))
	})
	appendFamily(func(window QuotaWindow) bool {
		return window.ScopeID != nil && strings.EqualFold(strings.TrimSpace(*window.ScopeID), "codex_bengalfox")
	})
	for _, window := range items {
		if len(selected) >= 2 {
			break
		}
		if _, exists := selectedIDs[window.ID]; exists {
			continue
		}
		selected = append(selected, window)
		selectedIDs[window.ID] = struct{}{}
	}
	if selected == nil {
		return []QuotaWindow{}
	}
	return selected
}

func quotaLongestWindow(windows []QuotaWindow) (QuotaWindow, bool) {
	if len(windows) == 0 {
		return QuotaWindow{}, false
	}
	items := append([]QuotaWindow(nil), windows...)
	sort.SliceStable(items, func(i, j int) bool {
		leftDuration, rightDuration := quotaWindowDurationRank(items[i]), quotaWindowDurationRank(items[j])
		if leftDuration != rightDuration {
			return leftDuration > rightDuration
		}
		leftRisk, rightRisk := quotaWindowRisk(items[i].Status), quotaWindowRisk(items[j].Status)
		if leftRisk != rightRisk {
			return leftRisk < rightRisk
		}
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		return items[i].ID < items[j].ID
	})
	return items[0], true
}

func quotaWindowDurationRank(window QuotaWindow) float64 {
	if window.WindowSeconds != nil && *window.WindowSeconds > 0 {
		return float64(*window.WindowSeconds)
	}
	if window.PeriodValue == nil || *window.PeriodValue <= 0 {
		return 0
	}
	multiplier := float64(0)
	switch window.PeriodUnit {
	case "minute":
		multiplier = 60
	case "hour":
		multiplier = 60 * 60
	case "day":
		multiplier = 24 * 60 * 60
	case "week":
		multiplier = 7 * 24 * 60 * 60
	case "month":
		multiplier = 30 * 24 * 60 * 60
	}
	return multiplier * *window.PeriodValue
}

func codexHasLegacyQuotaWindows(windows []QuotaWindow) bool {
	for _, window := range windows {
		if codexLegacyQuotaWindowID(window.ID) {
			return true
		}
	}
	return false
}

func codexCurrentQuotaWindows(windows []QuotaWindow) []QuotaWindow {
	items := make([]QuotaWindow, 0, len(windows))
	for _, window := range windows {
		if !codexLegacyQuotaWindowID(window.ID) {
			items = append(items, window)
		}
	}
	return items
}

func codexLegacyQuotaWindowID(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "codex-") && (strings.HasSuffix(value, "-primary") || strings.HasSuffix(value, "-secondary"))
}

func antigravityHasLegacyQuotaWindows(windows []QuotaWindow) bool {
	for _, window := range windows {
		if antigravityLegacyQuotaWindowID(window.ID) {
			return true
		}
	}
	return false
}

func antigravityCurrentQuotaWindows(windows []QuotaWindow) []QuotaWindow {
	items := make([]QuotaWindow, 0, len(windows))
	for _, window := range windows {
		if antigravityCurrentQuotaWindowID(window.ID) {
			items = append(items, window)
		}
	}
	return items
}

func antigravityLegacyQuotaWindowID(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "antigravity-model-")
}

func antigravityCurrentQuotaWindowID(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "antigravity-gemini-5h", "antigravity-gemini-weekly", "antigravity-3p-5h", "antigravity-3p-weekly":
		return true
	default:
		return false
	}
}

func quotaCollectionErrorFromRecord(record QuotaSnapshotRecord) *QuotaCollectionError {
	if record.ErrorCode == "" && record.ErrorMessage == "" {
		return nil
	}
	var status *int
	if record.ErrorStatusCode > 0 {
		value := record.ErrorStatusCode
		status = &value
	}
	requestID := quotaOptionalString(SafeQuotaRequestID(record.ErrorRequestID))
	return &QuotaCollectionError{Code: record.ErrorCode, Message: record.ErrorMessage, Retryable: record.ErrorRetryable, OccurredAt: quotaUTC(record.ErrorOccurredAt), UpstreamStatusCode: status, RequestID: requestID}
}

func quotaCredentialIdentityChanged(existing AuthRecord, next AuthRecord) bool {
	if normalizeQuotaProviderID(existing.Provider) != normalizeQuotaProviderID(next.Provider) {
		return true
	}
	existingAuth, errExisting := quotaAuthFromRecord(&existing)
	if errExisting != nil {
		return true
	}
	nextAuth, errNext := quotaAuthFromRecord(&next)
	if errNext != nil {
		return true
	}
	return quotaCredentialType(existingAuth) != quotaCredentialType(nextAuth)
}

func quotaCredentialType(auth *coreauth.Auth) string {
	if auth == nil {
		return "unknown"
	}
	if strings.Contains(strings.ToLower(auth.Provider), "vertex") {
		return "vertex"
	}
	if auth.Attributes != nil {
		if credentialType := normalizeQuotaCredentialType(auth.Attributes["auth_kind"]); credentialType != "" {
			return credentialType
		}
	}
	if credentialType := normalizeQuotaCredentialType(firstQuotaMetadataString(auth.Metadata, "auth_kind")); credentialType != "" {
		return credentialType
	}
	if auth.Attributes != nil && strings.HasPrefix(strings.TrimSpace(auth.Attributes["source"]), "config:") {
		return "provider_api_key"
	}
	if auth.Metadata != nil && strings.TrimSpace(firstQuotaMetadataString(auth.Metadata, "type")) != "" {
		return "oauth"
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return "provider_api_key"
	}
	return "file_auth"
}

func normalizeQuotaCredentialType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "apikey", "api_key", "provider_api_key":
		return "provider_api_key"
	case "oauth", "oauth2":
		return "oauth"
	default:
		return ""
	}
}

func quotaCredentialStatus(record AuthRecord, now time.Time) string {
	if record.Disabled || record.Status == coreauth.StatusDisabled {
		return "disabled"
	}
	if record.NextRetryAfter != nil && record.NextRetryAfter.After(now) {
		return "cooldown"
	}
	if record.Unavailable || record.Status == coreauth.StatusError {
		return "unavailable"
	}
	if record.Status == coreauth.StatusActive || record.Status == "" {
		return "enabled"
	}
	return "unknown"
}

func quotaProviderPlanned(provider string) bool {
	switch normalizeQuotaProviderID(provider) {
	case "claude", "antigravity", "codex", "kimi", "xai":
		return true
	default:
		return false
	}
}

func normalizeQuotaProviderID(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "claude"
	case "grok", "xai":
		return "xai"
	case "gemini_cli", "gemini-cli":
		return "gemini-cli"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func quotaRiskRank(item QuotaCredentialSnapshot) int {
	switch item.QuotaStatus {
	case "exhausted":
		return 0
	case "low":
		return 1
	case "error":
		return 2
	}
	if item.CollectionStatus == "failed" {
		return 2
	}
	if item.Freshness == "stale" || item.Freshness == "never" {
		return 3
	}
	if item.QuotaStatus == "unknown" {
		return 4
	}
	if item.QuotaStatus == "unsupported" {
		return 5
	}
	if item.CollectionStatus == "partial" {
		return 6
	}
	return 7
}

func quotaWindowRisk(status string) int {
	switch status {
	case "exhausted":
		return 0
	case "low":
		return 1
	case "error":
		return 2
	case "unknown":
		return 3
	case "unsupported":
		return 4
	default:
		return 5
	}
}
func quotaSourceValue(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "none"
	}
	return strings.TrimSpace(*value)
}
func quotaSetMatches(values map[string]struct{}, value string) bool {
	if len(values) == 0 {
		return true
	}
	_, ok := values[strings.ToLower(strings.TrimSpace(value))]
	return ok
}

func quotaExactSetMatches(values map[string]struct{}, value string) bool {
	if len(values) == 0 {
		return true
	}
	_, ok := values[strings.TrimSpace(value)]
	return ok
}

func quotaExactSetValues(values map[string]struct{}) []string {
	items := make([]string, 0, len(values))
	for value := range values {
		if normalized := strings.TrimSpace(value); normalized != "" {
			items = append(items, normalized)
		}
	}
	sort.Strings(items)
	return items
}
func quotaAllowed(value string, allowed ...string) bool {
	value = strings.TrimSpace(value)
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

// NormalizeQuotaWindowValues reconciles quantities, ratios, and derived status.
func NormalizeQuotaWindowValues(window *QuotaWindow) {
	if window == nil || window.Status == "error" || window.Status == "unsupported" {
		return
	}
	if window.IsUnlimited {
		window.Status = "healthy"
		window.UsedRatio = nil
		window.RemainingRatio = nil
		return
	}
	if window.Limit != nil {
		limit := math.Max(0, *window.Limit)
		window.Limit = quotaFloat64Pointer(limit)
		if limit == 0 {
			zero, one := 0.0, 1.0
			window.Used, window.Remaining = &zero, &zero
			window.UsedRatio, window.RemainingRatio = &one, &zero
			window.Status = "exhausted"
			return
		}
		used := math.NaN()
		switch {
		case window.Used != nil:
			used = math.Max(0, math.Min(limit, *window.Used))
		case window.Remaining != nil:
			used = limit - math.Max(0, math.Min(limit, *window.Remaining))
		case window.UsedRatio != nil:
			used = limit * math.Max(0, math.Min(1, *window.UsedRatio))
		case window.RemainingRatio != nil:
			used = limit * (1 - math.Max(0, math.Min(1, *window.RemainingRatio)))
		}
		if !math.IsNaN(used) {
			remaining := limit - used
			usedRatio := used / limit
			remainingRatio := 1 - usedRatio
			window.Used, window.Remaining = &used, &remaining
			window.UsedRatio, window.RemainingRatio = &usedRatio, &remainingRatio
			window.Status = quotaStatusFromRemainingRatio(remainingRatio)
		}
		return
	}
	if window.UsedRatio != nil || window.RemainingRatio != nil {
		usedRatio := 0.0
		if window.UsedRatio != nil {
			usedRatio = math.Max(0, math.Min(1, *window.UsedRatio))
		} else {
			usedRatio = 1 - math.Max(0, math.Min(1, *window.RemainingRatio))
		}
		remainingRatio := 1 - usedRatio
		window.UsedRatio, window.RemainingRatio = &usedRatio, &remainingRatio
		window.Status = quotaStatusFromRemainingRatio(remainingRatio)
		return
	}
	if window.Remaining != nil {
		if *window.Remaining == 0 {
			window.Status = "exhausted"
		} else if window.Status == "unknown" {
			window.Status = "healthy"
		}
	}
}

func quotaBoundedIdentifier(value string, maxLength int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if maxLength <= 0 || len(value) <= maxLength {
		return value
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	suffix := fmt.Sprintf("-%016x", hash.Sum64())
	prefixLength := maxLength - len(suffix)
	if prefixLength <= 0 {
		return suffix[len(suffix)-maxLength:]
	}
	prefix := strings.TrimRight(strings.ToValidUTF8(value[:prefixLength], ""), "-")
	return prefix + suffix
}

func quotaBoundedText(value string, maxLength int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if maxLength > 0 && len(value) > maxLength {
		value = strings.ToValidUTF8(value[:maxLength], "")
	}
	return value
}

func quotaBoundedOptionalText(value *string, maxLength int) *string {
	if value == nil {
		return nil
	}
	bounded := quotaBoundedText(*value, maxLength)
	if bounded == "" {
		return nil
	}
	return &bounded
}

func quotaFloat64Pointer(value float64) *float64 {
	return &value
}

func quotaMaskAccount(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	local, domain, found := strings.Cut(value, "@")
	if !found || strings.TrimSpace(domain) == "" {
		return quotaMaskIdentifier(value)
	}
	localRunes := []rune(local)
	visible := 2
	if len(localRunes) < visible {
		visible = len(localRunes)
	}
	if visible == 0 {
		return "***@" + domain
	}
	return string(localRunes[:visible]) + "***@" + domain
}

func quotaMaskIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	if len(runes) <= 8 {
		return string(runes[:2]) + "..." + string(runes[len(runes)-2:])
	}
	return string(runes[:4]) + "..." + string(runes[len(runes)-4:])
}

func quotaSafeDisplayLabel(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "@") {
		return quotaMaskAccount(value)
	}
	return value
}

// SafeQuotaRequestID accepts only bounded, non-secret request identifiers.
func SafeQuotaRequestID(value string) string {
	value = strings.TrimSpace(value)
	if !quotaRequestIDSafePattern.MatchString(value) || quotaRequestIDSecretPattern.MatchString(value) || quotaJWTSecretPattern.MatchString(value) {
		return ""
	}
	return value
}

func quotaBoundedRequestID(value string) string {
	return SafeQuotaRequestID(value)
}

func quotaFiniteOptional(value *float64) bool {
	return value == nil || (!math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0)
}
func quotaRatio(value *float64) bool {
	return quotaFiniteOptional(value) && (value == nil || *value <= 1)
}
func quotaObservationIsOlder(current, incoming *time.Time) bool {
	if current == nil {
		return false
	}
	return incoming == nil || current.After(incoming.UTC())
}
func quotaUTC(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}
func quotaOptionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
func quotaStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
func firstNonEmptyQuotaString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func quotaAuthMetadata(auth *coreauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	return auth.Metadata
}
func firstQuotaMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			switch typed := value.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					return strings.TrimSpace(typed)
				}
			case json.Number:
				return typed.String()
			}
		}
	}
	return ""
}
func quotaCompareTime(left, right *time.Time) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return 1
	}
	if right == nil {
		return -1
	}
	if left.Before(*right) {
		return -1
	}
	if left.After(*right) {
		return 1
	}
	return 0
}
func quotaEarliestReset(windows []QuotaWindow) *time.Time {
	var earliest *time.Time
	for _, window := range windows {
		if window.ResetAt != nil && (earliest == nil || window.ResetAt.Before(*earliest)) {
			value := window.ResetAt.UTC()
			earliest = &value
		}
	}
	return earliest
}
func quotaSafeErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	message = quotaBearerSecretPattern.ReplaceAllString(message, "[redacted]")
	message = usageObservabilitySecretPattern.ReplaceAllString(message, "[redacted]")
	replacer := strings.NewReplacer("Authorization", "[redacted]", "authorization", "[redacted]", "access_token", "[redacted]", "refresh_token", "[redacted]", "Cookie", "[redacted]", "cookie", "[redacted]")
	message = replacer.Replace(message)
	message = strings.ToValidUTF8(message, "")
	if len(message) > 500 {
		message = strings.ToValidUTF8(message[:500], "")
	}
	return message
}
