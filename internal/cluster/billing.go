package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	BillingBalanceTypeRecharge = "recharge"
	BillingBalanceTypeDeduct   = "deduct"

	BillingPriceSourceManual  = "manual"
	BillingPriceSourceDefault = "default"
	BillingPriceSourceSync    = "sync"

	BillingServiceTierWildcard       = "*"
	BillingServiceTierSourceRequest  = "request"
	BillingServiceTierSourceResponse = "response"
	BillingReportTimezoneUTC         = "UTC"
	billingSettingsKVKey             = "billing.settings"

	billingIDRandomBytes    = 12
	billingCreditMaxRetries = 5
	billingOverviewTopLimit = 10
)

var (
	ErrBillingDuplicateModelPrice = errors.New("billing model price already exists")
	ErrBillingInvalidModelPrice   = errors.New("invalid billing model price")
)

type BillingModelPriceRecord struct {
	ID                        string         `gorm:"column:id;primaryKey"`
	Provider                  string         `gorm:"column:provider;not null;index:idx_billing_model_price_lookup,priority:1"`
	Model                     string         `gorm:"column:model;not null;index:idx_billing_model_price_lookup,priority:2"`
	ServiceTier               string         `gorm:"column:service_tier;not null;default:*"`
	MinInputTokens            int64          `gorm:"column:min_input_tokens;not null;default:0"`
	InputPricePerMillion      float64        `gorm:"column:input_price_per_million;not null;default:0"`
	OutputPricePerMillion     float64        `gorm:"column:output_price_per_million;not null;default:0"`
	CacheReadPricePerMillion  float64        `gorm:"column:cache_read_price_per_million;not null;default:0"`
	CacheWritePricePerMillion float64        `gorm:"column:cache_write_price_per_million;not null;default:0"`
	RequestPrice              float64        `gorm:"column:request_price;not null;default:0"`
	Source                    string         `gorm:"column:source;not null;default:manual"`
	Enabled                   bool           `gorm:"column:enabled;not null;index:idx_billing_model_price_enabled"`
	Note                      string         `gorm:"column:note;type:text"`
	CreatedAt                 time.Time      `gorm:"column:created_at"`
	UpdatedAt                 time.Time      `gorm:"column:updated_at"`
	Revision                  int64          `gorm:"column:revision;not null;default:1"`
	DeletedAt                 gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

func (BillingModelPriceRecord) TableName() string { return "billing_model_price" }

type BillingBalanceRecord struct {
	ID            string    `gorm:"column:id;primaryKey"`
	UserID        uint      `gorm:"column:user_id;not null;index:idx_billing_balance_user_time,priority:1"`
	Type          string    `gorm:"column:type;not null;index:idx_billing_balance_type_time,priority:1"`
	Amount        float64   `gorm:"column:amount;not null"`
	BalanceBefore float64   `gorm:"column:balance_before;not null"`
	BalanceAfter  float64   `gorm:"column:balance_after;not null"`
	Operator      string    `gorm:"column:operator;not null"`
	Note          string    `gorm:"column:note;type:text"`
	CreatedAt     time.Time `gorm:"column:created_at;not null;index:idx_billing_balance_user_time,priority:2,sort:desc;index:idx_billing_balance_type_time,priority:2,sort:desc"`
}

func (BillingBalanceRecord) TableName() string { return "billing_balance_record" }

type BillingChargeRecord struct {
	ID               string    `gorm:"column:id;primaryKey"`
	UsageID          uint      `gorm:"column:usage_id;not null;uniqueIndex"`
	PayloadHash      string    `gorm:"column:payload_hash;not null;uniqueIndex"`
	UserID           *uint     `gorm:"column:user_id;index:idx_billing_charge_user_time,priority:1"`
	APIKeyID         *uint     `gorm:"column:api_key_id;index"`
	APIKeyLabel      string    `gorm:"column:api_key_label"`
	APIKeyMasked     string    `gorm:"column:api_key_masked"`
	Provider         string    `gorm:"column:provider;index:idx_billing_charge_provider_model_time,priority:1"`
	Model            string    `gorm:"column:model;index:idx_billing_charge_provider_model_time,priority:2"`
	OriginalModel    string    `gorm:"column:original_model"`
	ActualModel      string    `gorm:"column:actual_model"`
	RequestID        string    `gorm:"column:request_id;index"`
	Endpoint         string    `gorm:"column:endpoint;index"`
	InputTokens      int64     `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens     int64     `gorm:"column:output_tokens;not null;default:0"`
	CacheTokens      int64     `gorm:"column:cache_tokens;not null;default:0"`
	Amount           float64   `gorm:"column:amount;not null;default:0"`
	BalanceBefore    float64   `gorm:"column:balance_before;not null;default:0"`
	BalanceAfter     float64   `gorm:"column:balance_after;not null;default:0"`
	MatchedPriceRule string    `gorm:"column:matched_price_rule"`
	PriceSnapshot    JSONB     `gorm:"column:price_snapshot;not null"`
	RequestSummary   string    `gorm:"column:request_summary;type:text"`
	CreatedAt        time.Time `gorm:"column:created_at;not null;index:idx_billing_charge_user_time,priority:2,sort:desc;index:idx_billing_charge_provider_model_time,priority:3,sort:desc"`
}

func (BillingChargeRecord) TableName() string { return "billing_charge" }

type BillingModelPriceUpdate struct {
	Provider                  string
	Model                     string
	ServiceTier               string
	MinInputTokens            int64
	InputPricePerMillion      float64
	OutputPricePerMillion     float64
	CacheReadPricePerMillion  float64
	CacheWritePricePerMillion float64
	RequestPrice              float64
	Source                    string
	Enabled                   bool
	Note                      string
}

type BillingModelPricePatch struct {
	Provider                  *string
	Model                     *string
	ServiceTier               *string
	MinInputTokens            *int64
	InputPricePerMillion      *float64
	OutputPricePerMillion     *float64
	CacheReadPricePerMillion  *float64
	CacheWritePricePerMillion *float64
	RequestPrice              *float64
	Source                    *string
	Enabled                   *bool
	Note                      *string
}

type BillingBalanceUpdate struct {
	UserID   uint
	Type     string
	Amount   float64
	Operator string
	Note     string
}

type BillingOverviewQuery struct {
	From        *time.Time
	ToExclusive *time.Time
	Timezone    string
	UserText    string
	UserID      *uint
	Provider    string
	Model       string
}

type BillingOverview struct {
	Range               BillingRange
	TotalChargeAmount   float64
	TotalRechargeAmount float64
	TotalDeductAmount   float64
	TotalBalance        float64
	RequestCount        int64
	InputTokens         int64
	OutputTokens        int64
	CacheTokens         int64
	ActiveUserCount     int64
	DailyTrend          []BillingTrendPoint
	TopUsers            []BillingTopItem
	TopModels           []BillingTopItem
	TopProviders        []BillingTopItem
}

type BillingRange struct {
	From          string
	To            string
	FromAt        *time.Time
	ToAtExclusive *time.Time
	Timezone      string
}

type BillingTrendPoint struct {
	Date         string
	ChargeAmount float64
	RequestCount int64
}

type BillingTopItem struct {
	ID           string
	Label        string
	Amount       float64
	RequestCount int64
}

type BillingBalanceQuery struct {
	From        *time.Time
	ToExclusive *time.Time
	UserText    string
	UserID      *uint
	Limit       int
	Offset      int
}

type BillingBalanceResult struct {
	Records []BillingBalanceRecord
	Total   int64
}

type BillingModelPriceQuery struct {
	Provider string
	Model    string
	Enabled  *bool
}

type BillingChargeQuery struct {
	From        *time.Time
	ToExclusive *time.Time
	UserText    string
	UserID      *uint
	Provider    string
	Model       string
	Limit       int
	Offset      int
}

type BillingChargeResult struct {
	Records []BillingChargeRecord
	Total   int64
}

type BillingPriceSnapshot struct {
	Provider                  string  `json:"provider"`
	Model                     string  `json:"model"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million"`
	RequestPrice              float64 `json:"request_price"`
	MatchedServiceTier        string  `json:"matched_service_tier"`
	MinInputTokens            int64   `json:"min_input_tokens"`
	RequestedServiceTier      string  `json:"requested_service_tier"`
	ResponseServiceTier       string  `json:"response_service_tier,omitempty"`
	ServiceTierSource         string  `json:"service_tier_source"`
	EffectiveServiceTier      string  `json:"effective_service_tier"`
	ResponseTierFallback      bool    `json:"response_tier_fallback"`
}

type BillingSettings struct {
	ServiceTierSource string `json:"service_tier_source"`
	ReportTimezone    string `json:"report_timezone"`
}

type BillingSettingsPatch struct {
	ServiceTierSource *string
	ReportTimezone    *string
}

func (r *Repository) GetBillingSettings(ctx context.Context) (BillingSettings, error) {
	db, errDB := r.database()
	if errDB != nil {
		return BillingSettings{}, errDB
	}
	return billingSettingsFromDB(ctx, db)
}

func (r *Repository) UpdateBillingSettings(ctx context.Context, patch BillingSettingsPatch) (BillingSettings, error) {
	settings, errSettings := r.GetBillingSettings(ctx)
	if errSettings != nil {
		return BillingSettings{}, errSettings
	}
	if patch.ServiceTierSource != nil {
		source, errSource := normalizeBillingServiceTierSource(*patch.ServiceTierSource)
		if errSource != nil {
			return BillingSettings{}, errSource
		}
		settings.ServiceTierSource = source
	}
	if patch.ReportTimezone != nil {
		timezone, errTimezone := normalizeBillingReportTimezone(*patch.ReportTimezone)
		if errTimezone != nil {
			return BillingSettings{}, errTimezone
		}
		settings.ReportTimezone = timezone
	}
	raw, errMarshal := json.Marshal(settings)
	if errMarshal != nil {
		return BillingSettings{}, errMarshal
	}
	if _, errSet := r.KVSet(ctx, billingSettingsKVKey, raw, 0, KVSetModeAlways); errSet != nil {
		return BillingSettings{}, errSet
	}
	return settings, nil
}

func (r *Repository) CreateBillingModelPrice(ctx context.Context, update BillingModelPriceUpdate) (*BillingModelPriceRecord, error) {
	if errValidate := validateBillingModelPriceUpdate(update); errValidate != nil {
		return nil, fmt.Errorf("%w: %v", ErrBillingInvalidModelPrice, errValidate)
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	record := &BillingModelPriceRecord{
		ID:                        billingID("price"),
		Provider:                  strings.ToLower(strings.TrimSpace(update.Provider)),
		Model:                     strings.TrimSpace(update.Model),
		ServiceTier:               normalizeBillingPriceServiceTier(update.ServiceTier),
		MinInputTokens:            update.MinInputTokens,
		InputPricePerMillion:      update.InputPricePerMillion,
		OutputPricePerMillion:     update.OutputPricePerMillion,
		CacheReadPricePerMillion:  update.CacheReadPricePerMillion,
		CacheWritePricePerMillion: update.CacheWritePricePerMillion,
		RequestPrice:              update.RequestPrice,
		Source:                    billingPriceSource(update.Source),
		Enabled:                   update.Enabled,
		Note:                      strings.TrimSpace(update.Note),
	}
	if errCreate := db.WithContext(contextOrBackground(ctx)).Create(record).Error; errCreate != nil {
		if isBillingModelPriceConflict(errCreate) {
			return nil, ErrBillingDuplicateModelPrice
		}
		return nil, errCreate
	}
	return record, nil
}

func (r *Repository) GetBillingModelPrice(ctx context.Context, id string) (*BillingModelPriceRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, gorm.ErrRecordNotFound
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	record := &BillingModelPriceRecord{}
	if errFirst := db.WithContext(contextOrBackground(ctx)).First(record, "id = ?", id).Error; errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

func (r *Repository) UpdateBillingModelPrice(ctx context.Context, id string, patch BillingModelPricePatch) (*BillingModelPriceRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, gorm.ErrRecordNotFound
	}
	if errValidate := validateBillingModelPricePatch(patch); errValidate != nil {
		return nil, fmt.Errorf("%w: %v", ErrBillingInvalidModelPrice, errValidate)
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	if errFirst := db.WithContext(ctx).First(&BillingModelPriceRecord{}, "id = ?", id).Error; errFirst != nil {
		return nil, errFirst
	}

	updates := billingModelPricePatchUpdates(patch)
	if len(updates) == 0 {
		return r.GetBillingModelPrice(ctx, id)
	}
	updates["revision"] = gorm.Expr("revision + ?", 1)
	update := db.WithContext(ctx).Model(&BillingModelPriceRecord{}).Where("id = ?", id).Updates(updates)
	if update.Error != nil {
		if isBillingModelPriceConflict(update.Error) {
			return nil, ErrBillingDuplicateModelPrice
		}
		return nil, update.Error
	}
	if update.RowsAffected == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return r.GetBillingModelPrice(ctx, id)
}

func (r *Repository) DeleteBillingModelPrice(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return gorm.ErrRecordNotFound
	}
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	deleteResult := db.WithContext(contextOrBackground(ctx)).Delete(&BillingModelPriceRecord{}, "id = ?", id)
	if deleteResult.Error != nil {
		return deleteResult.Error
	}
	if deleteResult.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (r *Repository) ApplyBillingBalanceRecord(ctx context.Context, update BillingBalanceUpdate) (*BillingBalanceRecord, error) {
	update.Type = strings.ToLower(strings.TrimSpace(update.Type))
	update.Operator = strings.TrimSpace(update.Operator)
	update.Note = strings.TrimSpace(update.Note)
	if update.UserID == 0 {
		return nil, fmt.Errorf("user id is required")
	}
	if update.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	switch update.Type {
	case BillingBalanceTypeRecharge:
	case BillingBalanceTypeDeduct:
		if update.Note == "" {
			return nil, fmt.Errorf("note is required for deduct balance record")
		}
	default:
		return nil, fmt.Errorf("unsupported billing balance type %q", update.Type)
	}
	if update.Operator == "" {
		update.Operator = "admin"
	}

	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	record := &BillingBalanceRecord{}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		delta := update.Amount
		if update.Type == BillingBalanceTypeDeduct {
			delta = -update.Amount
		}
		balanceBefore, balanceAfter, errCredits := billingApplyUserCreditDeltaTx(ctx, tx, update.UserID, delta)
		if errCredits != nil {
			return errCredits
		}
		*record = BillingBalanceRecord{
			ID:            billingID("balance"),
			UserID:        update.UserID,
			Type:          update.Type,
			Amount:        update.Amount,
			BalanceBefore: balanceBefore,
			BalanceAfter:  balanceAfter,
			Operator:      update.Operator,
			Note:          update.Note,
			CreatedAt:     time.Now().UTC(),
		}
		return tx.WithContext(ctx).Create(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

func (r *Repository) ListBillingCharges(ctx context.Context, query BillingChargeQuery) (BillingChargeResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return BillingChargeResult{}, errDB
	}
	query.Limit, query.Offset = normalizeBillingPagination(query.Limit, query.Offset)

	scope := billingChargeQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingChargeRecord{}), query)
	var total int64
	if errCount := scope.Count(&total).Error; errCount != nil {
		return BillingChargeResult{}, errCount
	}

	var records []BillingChargeRecord
	if errFind := scope.Order(`"billing_charge"."created_at" DESC, "billing_charge"."id" DESC`).Limit(query.Limit).Offset(query.Offset).Find(&records).Error; errFind != nil {
		return BillingChargeResult{}, errFind
	}
	return BillingChargeResult{
		Records: records,
		Total:   total,
	}, nil
}

func (r *Repository) BillingOverview(ctx context.Context, query BillingOverviewQuery) (BillingOverview, error) {
	db, errDB := r.database()
	if errDB != nil {
		return BillingOverview{}, errDB
	}
	ctx = contextOrBackground(ctx)

	chargeQuery := billingChargeQueryFromOverview(query)
	chargeScope := billingChargeQueryScope(db.WithContext(ctx).Model(&BillingChargeRecord{}), chargeQuery)
	var chargeTotals struct {
		TotalChargeAmount float64
		RequestCount      int64
		InputTokens       int64
		OutputTokens      int64
		CacheTokens       int64
		ActiveUserCount   int64
		MinCreatedAt      sql.NullString `gorm:"column:min_created_at"`
		MaxCreatedAt      sql.NullString `gorm:"column:max_created_at"`
	}
	if errScan := chargeScope.Select(`
		COALESCE(SUM("billing_charge"."amount"), 0) AS total_charge_amount,
		COUNT(*) AS request_count,
		COALESCE(SUM("billing_charge"."input_tokens"), 0) AS input_tokens,
		COALESCE(SUM("billing_charge"."output_tokens"), 0) AS output_tokens,
		COALESCE(SUM("billing_charge"."cache_tokens"), 0) AS cache_tokens,
		COUNT(DISTINCT "billing_charge"."user_id") AS active_user_count,
		MIN("billing_charge"."created_at") AS min_created_at,
		MAX("billing_charge"."created_at") AS max_created_at`,
	).Scan(&chargeTotals).Error; errScan != nil {
		return BillingOverview{}, errScan
	}

	var totalBalance float64
	balanceScope := billingUserBalanceQueryScope(db.WithContext(ctx).Model(&UserRecord{}), query)
	if errBalance := balanceScope.Select(`COALESCE(SUM("user"."credits"), 0)`).Scan(&totalBalance).Error; errBalance != nil {
		return BillingOverview{}, errBalance
	}

	dailyTrend, errDailyTrend := billingDailyTrend(ctx, db, chargeQuery, query.Timezone, billingTrendBounds{
		MinCreatedAt: chargeTotals.MinCreatedAt,
		MaxCreatedAt: chargeTotals.MaxCreatedAt,
	})
	if errDailyTrend != nil {
		return BillingOverview{}, errDailyTrend
	}
	topUsers, errTopUsers := billingTopUsers(ctx, db, chargeQuery)
	if errTopUsers != nil {
		return BillingOverview{}, errTopUsers
	}
	topModels, errTopModels := billingTopModels(ctx, db, chargeQuery)
	if errTopModels != nil {
		return BillingOverview{}, errTopModels
	}
	topProviders, errTopProviders := billingTopProviders(ctx, db, chargeQuery)
	if errTopProviders != nil {
		return BillingOverview{}, errTopProviders
	}
	ledgerTotals, errLedger := billingLedgerTotals(ctx, db, query)
	if errLedger != nil {
		return BillingOverview{}, errLedger
	}

	return BillingOverview{
		Range:               billingRange(query.From, query.ToExclusive, query.Timezone),
		TotalChargeAmount:   chargeTotals.TotalChargeAmount,
		TotalRechargeAmount: ledgerTotals.RechargeAmount,
		TotalDeductAmount:   ledgerTotals.DeductAmount,
		TotalBalance:        totalBalance,
		RequestCount:        chargeTotals.RequestCount,
		InputTokens:         chargeTotals.InputTokens,
		OutputTokens:        chargeTotals.OutputTokens,
		CacheTokens:         chargeTotals.CacheTokens,
		ActiveUserCount:     chargeTotals.ActiveUserCount,
		DailyTrend:          dailyTrend,
		TopUsers:            topUsers,
		TopModels:           topModels,
		TopProviders:        topProviders,
	}, nil
}

func (r *Repository) ListBillingBalanceRecords(ctx context.Context, query BillingBalanceQuery) (BillingBalanceResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return BillingBalanceResult{}, errDB
	}
	query.Limit, query.Offset = normalizeBillingPagination(query.Limit, query.Offset)

	scope := billingBalanceQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingBalanceRecord{}), query)
	var total int64
	if errCount := scope.Count(&total).Error; errCount != nil {
		return BillingBalanceResult{}, errCount
	}

	var records []BillingBalanceRecord
	if errFind := scope.Order(`"billing_balance_record"."created_at" DESC, "billing_balance_record"."id" DESC`).Limit(query.Limit).Offset(query.Offset).Find(&records).Error; errFind != nil {
		return BillingBalanceResult{}, errFind
	}
	return BillingBalanceResult{
		Records: records,
		Total:   total,
	}, nil
}

func (r *Repository) ListBillingModelPrices(ctx context.Context, query BillingModelPriceQuery) ([]BillingModelPriceRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	scope := db.WithContext(contextOrBackground(ctx)).Model(&BillingModelPriceRecord{})
	if provider := strings.ToLower(strings.TrimSpace(query.Provider)); provider != "" {
		scope = scope.Where("provider = ?", provider)
	}
	if model := strings.TrimSpace(query.Model); model != "" {
		scope = scope.Where("model LIKE ?", "%"+model+"%")
	}
	if query.Enabled != nil {
		scope = scope.Where("enabled = ?", *query.Enabled)
	}

	var records []BillingModelPriceRecord
	if errFind := scope.Order("provider ASC, model ASC, service_tier ASC, min_input_tokens ASC, created_at DESC, id DESC").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// ListEnabledBillingModelPricesForModels returns the enabled price rules for an
// exact set of model identifiers.
//
// ListBillingModelPrices matches models with LIKE because it backs an operator
// search box; a caller pricing a catalog needs exact identity, or "gpt-4" would
// drag in every model whose name contains it. The ordering mirrors how the rules
// are applied at charge time, so a client can read a returned group top to
// bottom as the ladder a request climbs.
func (r *Repository) ListEnabledBillingModelPricesForModels(ctx context.Context, modelIDs []string) ([]BillingModelPriceRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	wanted := make([]string, 0, len(modelIDs))
	seen := make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		trimmed := strings.TrimSpace(modelID)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		wanted = append(wanted, trimmed)
	}
	if len(wanted) == 0 {
		return []BillingModelPriceRecord{}, nil
	}

	var records []BillingModelPriceRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Model(&BillingModelPriceRecord{}).
		Where("enabled = ?", true).
		Where("model IN ?", wanted).
		Order("provider ASC, model ASC, service_tier ASC, min_input_tokens ASC, id ASC").
		Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

func (r *Repository) createBillingChargeForUsageTx(ctx context.Context, tx *gorm.DB, usage *UsageRecord, payload string) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if usage == nil {
		return fmt.Errorf("usage record is nil")
	}
	if usage.Failed {
		return nil
	}

	ctx = contextOrBackground(ctx)
	payloadHash := billingPayloadHash(payload)
	apiKey, errAPIKey := billingAPIKeyByValue(ctx, tx, usage.APIKey)
	if errAPIKey != nil {
		return errAPIKey
	}
	price, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, tx, usage)
	if errSnapshot != nil {
		return errSnapshot
	}
	amount := billingChargeAmount(usage, snapshot)
	if !billingUsageHasBillableActivity(usage, amount) {
		return nil
	}
	priceSnapshot, errPriceSnapshot := billingJSONB(snapshot)
	if errPriceSnapshot != nil {
		return errPriceSnapshot
	}

	var userID *uint
	if apiKey != nil {
		userID = normalizeOptionalUserID(apiKey.UserID)
	}

	charge := &BillingChargeRecord{
		ID:               billingID("charge"),
		UsageID:          usage.ID,
		PayloadHash:      payloadHash,
		UserID:           userID,
		APIKeyID:         billingAPIKeyID(apiKey),
		APIKeyLabel:      billingAPIKeyLabel(apiKey),
		APIKeyMasked:     maskBillingAPIKey(usage.APIKey),
		Provider:         strings.ToLower(strings.TrimSpace(usage.Provider)),
		Model:            strings.TrimSpace(usage.Model),
		OriginalModel:    firstNonEmptyBillingString(usage.Alias, usage.Model),
		ActualModel:      strings.TrimSpace(usage.Model),
		RequestID:        strings.TrimSpace(usage.RequestID),
		Endpoint:         strings.TrimSpace(usage.Endpoint),
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheTokens:      billingCacheTokens(usage),
		Amount:           amount,
		MatchedPriceRule: billingMatchedPriceRule(price),
		PriceSnapshot:    priceSnapshot,
		CreatedAt:        usage.Timestamp.UTC(),
	}
	insert := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "payload_hash"}},
		DoNothing: true,
	}).Create(charge)
	if insert.Error != nil {
		return insert.Error
	}
	if insert.RowsAffected == 0 {
		return nil
	}
	if userID == nil {
		return nil
	}

	// Open first_use period windows on the first billable charge (not on dispatch probes).
	if errOpen := openUserFirstUseWindowsOnChargeTx(ctx, tx, *userID, charge.CreatedAt); errOpen != nil {
		return errOpen
	}

	balanceBefore, balanceAfter, errCredits := billingApplyUserCreditDeltaTx(ctx, tx, *userID, -amount)
	if errCredits != nil {
		return errCredits
	}
	return tx.WithContext(ctx).
		Model(&BillingChargeRecord{}).
		Where("id = ?", charge.ID).
		Updates(map[string]any{
			"balance_before": balanceBefore,
			"balance_after":  balanceAfter,
		}).Error
}

func validateBillingModelPriceUpdate(update BillingModelPriceUpdate) error {
	if strings.TrimSpace(update.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if strings.TrimSpace(update.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if errTier := validateBillingPriceServiceTier(update.ServiceTier); errTier != nil {
		return errTier
	}
	if update.MinInputTokens < 0 {
		return fmt.Errorf("min_input_tokens must be non-negative")
	}
	for name, value := range map[string]float64{
		"input_price_per_million":       update.InputPricePerMillion,
		"output_price_per_million":      update.OutputPricePerMillion,
		"cache_read_price_per_million":  update.CacheReadPricePerMillion,
		"cache_write_price_per_million": update.CacheWritePricePerMillion,
		"request_price":                 update.RequestPrice,
	} {
		if value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	return nil
}

func validateBillingModelPricePatch(patch BillingModelPricePatch) error {
	if patch.Provider != nil && strings.TrimSpace(*patch.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if patch.Model != nil && strings.TrimSpace(*patch.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if patch.ServiceTier != nil {
		if errTier := validateBillingPriceServiceTier(*patch.ServiceTier); errTier != nil {
			return errTier
		}
	}
	if patch.MinInputTokens != nil && *patch.MinInputTokens < 0 {
		return fmt.Errorf("min_input_tokens must be non-negative")
	}
	for name, value := range map[string]*float64{
		"input_price_per_million":       patch.InputPricePerMillion,
		"output_price_per_million":      patch.OutputPricePerMillion,
		"cache_read_price_per_million":  patch.CacheReadPricePerMillion,
		"cache_write_price_per_million": patch.CacheWritePricePerMillion,
		"request_price":                 patch.RequestPrice,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	return nil
}

func billingModelPricePatchUpdates(patch BillingModelPricePatch) map[string]any {
	updates := make(map[string]any)
	if patch.Provider != nil {
		updates["provider"] = strings.ToLower(strings.TrimSpace(*patch.Provider))
	}
	if patch.Model != nil {
		updates["model"] = strings.TrimSpace(*patch.Model)
	}
	if patch.ServiceTier != nil {
		updates["service_tier"] = normalizeBillingPriceServiceTier(*patch.ServiceTier)
	}
	if patch.MinInputTokens != nil {
		updates["min_input_tokens"] = *patch.MinInputTokens
	}
	if patch.InputPricePerMillion != nil {
		updates["input_price_per_million"] = *patch.InputPricePerMillion
	}
	if patch.OutputPricePerMillion != nil {
		updates["output_price_per_million"] = *patch.OutputPricePerMillion
	}
	if patch.CacheReadPricePerMillion != nil {
		updates["cache_read_price_per_million"] = *patch.CacheReadPricePerMillion
	}
	if patch.CacheWritePricePerMillion != nil {
		updates["cache_write_price_per_million"] = *patch.CacheWritePricePerMillion
	}
	if patch.RequestPrice != nil {
		updates["request_price"] = *patch.RequestPrice
	}
	if patch.Source != nil {
		updates["source"] = billingPriceSource(*patch.Source)
	}
	if patch.Enabled != nil {
		updates["enabled"] = *patch.Enabled
	}
	if patch.Note != nil {
		updates["note"] = strings.TrimSpace(*patch.Note)
	}
	return updates
}

func normalizeBillingPagination(limit int, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func billingApplyUserCreditDeltaTx(ctx context.Context, tx *gorm.DB, userID uint, delta float64) (float64, float64, error) {
	if tx == nil {
		return 0, 0, fmt.Errorf("database transaction is nil")
	}
	if userID == 0 {
		return 0, 0, fmt.Errorf("user id is required")
	}
	ctx = contextOrBackground(ctx)
	for attempt := 0; attempt < billingCreditMaxRetries; attempt++ {
		user := &UserRecord{}
		if errFirst := tx.WithContext(ctx).Select("id", "credits", "credits_unlimited").Where("id = ?", userID).First(user).Error; errFirst != nil {
			return 0, 0, errFirst
		}
		balanceBefore := user.Credits
		balanceAfter := balanceBefore + delta
		if delta == 0 || user.CreditsUnlimited {
			return balanceBefore, balanceBefore, nil
		}
		update := tx.WithContext(ctx).
			Model(&UserRecord{}).
			Where("id = ? AND credits = ?", userID, balanceBefore).
			Update("credits", balanceAfter)
		if update.Error != nil {
			return 0, 0, update.Error
		}
		if update.RowsAffected == 1 {
			return balanceBefore, balanceAfter, nil
		}
	}
	return 0, 0, fmt.Errorf("update user credits conflict after %d retries", billingCreditMaxRetries)
}

func billingChargeQueryScope(scope *gorm.DB, query BillingChargeQuery) *gorm.DB {
	if query.From != nil {
		scope = scope.Where(`"billing_charge"."created_at" >= ?`, query.From.UTC())
	}
	if query.ToExclusive != nil {
		scope = scope.Where(`"billing_charge"."created_at" < ?`, query.ToExclusive.UTC())
	}
	if userID := normalizeOptionalUserID(query.UserID); userID != nil {
		scope = scope.Where(`"billing_charge"."user_id" = ?`, *userID)
	}
	if userText := strings.TrimSpace(query.UserText); userText != "" {
		matcher := "%" + userText + "%"
		scope = scope.Joins(`LEFT JOIN "user" ON "user"."id" = "billing_charge"."user_id"`).
			Where(`"user"."username" LIKE ? OR CAST("user"."id" AS TEXT) LIKE ?`, matcher, matcher)
	}
	if provider := strings.ToLower(strings.TrimSpace(query.Provider)); provider != "" {
		scope = scope.Where(`"billing_charge"."provider" = ?`, provider)
	}
	if model := strings.TrimSpace(query.Model); model != "" {
		scope = scope.Where(`"billing_charge"."model" LIKE ?`, "%"+model+"%")
	}
	return scope
}

func billingChargeQueryFromOverview(query BillingOverviewQuery) BillingChargeQuery {
	return BillingChargeQuery{
		From:        query.From,
		ToExclusive: query.ToExclusive,
		UserText:    query.UserText,
		UserID:      query.UserID,
		Provider:    query.Provider,
		Model:       query.Model,
	}
}

type billingLedgerSummary struct {
	RechargeAmount float64
	DeductAmount   float64
}

type billingTrendBounds struct {
	MinCreatedAt sql.NullString
	MaxCreatedAt sql.NullString
}

func billingRange(from *time.Time, toExclusive *time.Time, timezone string) BillingRange {
	timezone, location := billingTimezoneLocation(timezone)
	result := BillingRange{Timezone: timezone}
	if from != nil {
		fromUTC := from.UTC()
		result.FromAt = &fromUTC
		result.From = fromUTC.In(location).Format(time.DateOnly)
	}
	if toExclusive != nil {
		toExclusiveUTC := toExclusive.UTC()
		result.ToAtExclusive = &toExclusiveUTC
		calendarEnd := toExclusiveUTC
		if from == nil || toExclusiveUTC.After(from.UTC()) {
			calendarEnd = calendarEnd.Add(-time.Nanosecond)
		}
		result.To = calendarEnd.In(location).Format(time.DateOnly)
	}
	return result
}

func billingLedgerTotals(ctx context.Context, db *gorm.DB, query BillingOverviewQuery) (billingLedgerSummary, error) {
	if db == nil {
		return billingLedgerSummary{}, fmt.Errorf("database connection is nil")
	}
	scope := billingBalanceQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingBalanceRecord{}), BillingBalanceQuery{
		From:        query.From,
		ToExclusive: query.ToExclusive,
		UserText:    query.UserText,
		UserID:      query.UserID,
	})
	var rows []struct {
		Type  string
		Total float64
	}
	if errScan := scope.Select(`"billing_balance_record"."type" AS type, COALESCE(SUM("billing_balance_record"."amount"), 0) AS total`).Group(`"billing_balance_record"."type"`).Scan(&rows).Error; errScan != nil {
		return billingLedgerSummary{}, errScan
	}

	summary := billingLedgerSummary{}
	for _, row := range rows {
		switch row.Type {
		case BillingBalanceTypeRecharge:
			summary.RechargeAmount = row.Total
		case BillingBalanceTypeDeduct:
			summary.DeductAmount = row.Total
		}
	}
	return summary, nil
}

func billingDailyTrend(ctx context.Context, db *gorm.DB, query BillingChargeQuery, timezone string, bounds billingTrendBounds) ([]BillingTrendPoint, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	dateExpr, dateArgs := billingDateExpression(db, `"billing_charge"."created_at"`, timezone, bounds)
	scope := billingChargeQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingChargeRecord{}), query)
	var rows []struct {
		Date         string
		ChargeAmount float64
		RequestCount int64
	}
	if errScan := scope.Select(fmt.Sprintf(
		`%s AS date, COALESCE(SUM("billing_charge"."amount"), 0) AS charge_amount, COUNT(*) AS request_count`,
		dateExpr,
	), dateArgs...).Group("date").Order("date ASC").Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	out := make([]BillingTrendPoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, BillingTrendPoint{
			Date:         row.Date,
			ChargeAmount: row.ChargeAmount,
			RequestCount: row.RequestCount,
		})
	}
	return out, nil
}

func billingTopUsers(ctx context.Context, db *gorm.DB, query BillingChargeQuery) ([]BillingTopItem, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	scope := billingChargeQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingChargeRecord{}), query).
		Where(`"billing_charge"."user_id" IS NOT NULL`)
	var rows []struct {
		UserID       uint
		Amount       float64
		RequestCount int64
	}
	if errScan := scope.Select(`"billing_charge"."user_id" AS user_id, COALESCE(SUM("billing_charge"."amount"), 0) AS amount, COUNT(*) AS request_count`).
		Group(`"billing_charge"."user_id"`).
		Order("amount DESC, request_count DESC, user_id ASC").
		Limit(billingOverviewTopLimit).
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	ids := make([]uint, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.UserID)
	}
	labels, errLabels := billingUserLabels(ctx, db, ids)
	if errLabels != nil {
		return nil, errLabels
	}
	out := make([]BillingTopItem, 0, len(rows))
	for _, row := range rows {
		id := strconv.FormatUint(uint64(row.UserID), 10)
		label := labels[row.UserID]
		if strings.TrimSpace(label) == "" {
			label = "user-" + id
		}
		out = append(out, BillingTopItem{
			ID:           id,
			Label:        label,
			Amount:       row.Amount,
			RequestCount: row.RequestCount,
		})
	}
	return out, nil
}

func billingTopModels(ctx context.Context, db *gorm.DB, query BillingChargeQuery) ([]BillingTopItem, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	scope := billingChargeQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingChargeRecord{}), query)
	var rows []struct {
		Provider     string
		Model        string
		Amount       float64
		RequestCount int64
	}
	if errScan := scope.Select(`"billing_charge"."provider" AS provider, "billing_charge"."model" AS model, COALESCE(SUM("billing_charge"."amount"), 0) AS amount, COUNT(*) AS request_count`).
		Group(`"billing_charge"."provider", "billing_charge"."model"`).
		Order("amount DESC, request_count DESC, provider ASC, model ASC").
		Limit(billingOverviewTopLimit).
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	out := make([]BillingTopItem, 0, len(rows))
	for _, row := range rows {
		model := strings.TrimSpace(row.Model)
		provider := strings.TrimSpace(row.Provider)
		id := model
		if provider != "" && model != "" {
			id = provider + "/" + model
		}
		out = append(out, BillingTopItem{
			ID:           id,
			Label:        model,
			Amount:       row.Amount,
			RequestCount: row.RequestCount,
		})
	}
	return out, nil
}

func billingTopProviders(ctx context.Context, db *gorm.DB, query BillingChargeQuery) ([]BillingTopItem, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	scope := billingChargeQueryScope(db.WithContext(contextOrBackground(ctx)).Model(&BillingChargeRecord{}), query)
	var rows []struct {
		Provider     string
		Amount       float64
		RequestCount int64
	}
	if errScan := scope.Select(`"billing_charge"."provider" AS provider, COALESCE(SUM("billing_charge"."amount"), 0) AS amount, COUNT(*) AS request_count`).
		Group(`"billing_charge"."provider"`).
		Order("amount DESC, request_count DESC, provider ASC").
		Limit(billingOverviewTopLimit).
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}
	out := make([]BillingTopItem, 0, len(rows))
	for _, row := range rows {
		provider := strings.TrimSpace(row.Provider)
		out = append(out, BillingTopItem{
			ID:           provider,
			Label:        provider,
			Amount:       row.Amount,
			RequestCount: row.RequestCount,
		})
	}
	return out, nil
}

func billingUserLabels(ctx context.Context, db *gorm.DB, ids []uint) (map[uint]string, error) {
	out := make(map[uint]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var users []UserRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Unscoped().Where("id IN ?", ids).Find(&users).Error; errFind != nil {
		return nil, errFind
	}
	for _, user := range users {
		out[user.ID] = strings.TrimSpace(user.Username)
	}
	return out, nil
}

func billingUserBalanceQueryScope(scope *gorm.DB, query BillingOverviewQuery) *gorm.DB {
	if userID := normalizeOptionalUserID(query.UserID); userID != nil {
		scope = scope.Where(`"user"."id" = ?`, *userID)
	}
	if userText := strings.TrimSpace(query.UserText); userText != "" {
		matcher := "%" + userText + "%"
		scope = scope.Where(`"user"."username" LIKE ? OR CAST("user"."id" AS TEXT) LIKE ?`, matcher, matcher)
	}
	return scope
}

func billingDateExpression(db *gorm.DB, column string, timezone string, bounds billingTrendBounds) (string, []any) {
	timezone, location := billingTimezoneLocation(timezone)
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return fmt.Sprintf("TO_CHAR(timezone(?, %s), 'YYYY-MM-DD')", column), []any{timezone}
	}
	start, end := billingTrendRange(bounds)
	reference := start
	if reference.IsZero() {
		reference = end
	}
	if reference.IsZero() {
		reference = time.Now().UTC()
	}
	_, offsetSeconds := reference.In(location).Zone()
	fallback := fmt.Sprintf("strftime('%%Y-%%m-%%d', %s, '%+d seconds')", column, offsetSeconds)
	if start.IsZero() || end.IsZero() || !usageObservabilityTimezoneOffsetChanges(location, start, end) {
		return fallback, nil
	}
	segments := usageObservabilityTrendBucketCaseSegments("day", location, start, end)
	if len(segments) == 0 {
		return fallback, nil
	}
	unixExpr := fmt.Sprintf("CAST(strftime('%%s', %s) AS INTEGER)", column)
	var builder strings.Builder
	builder.WriteString("CASE")
	for _, segment := range segments {
		date := time.Unix(segment.BucketUnix, 0).UTC().In(location).Format(time.DateOnly)
		builder.WriteString(fmt.Sprintf(" WHEN %s >= %d AND %s < %d THEN '%s'", unixExpr, segment.StartUnix, unixExpr, segment.EndUnix, date))
	}
	builder.WriteString(" ELSE ")
	builder.WriteString(fallback)
	builder.WriteString(" END")
	return builder.String(), nil
}

func billingTimezoneLocation(timezone string) (string, *time.Location) {
	value := strings.TrimSpace(timezone)
	if value == "" {
		return "UTC", time.UTC
	}
	location, errLoad := time.LoadLocation(value)
	if errLoad != nil {
		return "UTC", time.UTC
	}
	return value, location
}

func billingTrendRange(bounds billingTrendBounds) (time.Time, time.Time) {
	var start time.Time
	var end time.Time
	if bounds.MinCreatedAt.Valid {
		if parsedStart, ok := usageObservabilityAggregateTime(bounds.MinCreatedAt.String); ok {
			start = parsedStart.UTC()
		}
	}
	if bounds.MaxCreatedAt.Valid {
		if parsedEnd, ok := usageObservabilityAggregateTime(bounds.MaxCreatedAt.String); ok {
			end = parsedEnd.UTC()
		}
	}
	return start, end
}

func billingBalanceQueryScope(scope *gorm.DB, query BillingBalanceQuery) *gorm.DB {
	if query.From != nil {
		scope = scope.Where(`"billing_balance_record"."created_at" >= ?`, query.From.UTC())
	}
	if query.ToExclusive != nil {
		scope = scope.Where(`"billing_balance_record"."created_at" < ?`, query.ToExclusive.UTC())
	}
	if userID := normalizeOptionalUserID(query.UserID); userID != nil {
		scope = scope.Where(`"billing_balance_record"."user_id" = ?`, *userID)
	}
	if userText := strings.TrimSpace(query.UserText); userText != "" {
		matcher := "%" + userText + "%"
		scope = scope.Joins(`LEFT JOIN "user" ON "user"."id" = "billing_balance_record"."user_id"`).
			Where(`"user"."username" LIKE ? OR CAST("user"."id" AS TEXT) LIKE ?`, matcher, matcher)
	}
	return scope
}

func billingAPIKeyByValue(ctx context.Context, tx *gorm.DB, apiKey string) (*APIKeyRecord, error) {
	if tx == nil {
		return nil, fmt.Errorf("database transaction is nil")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, nil
	}
	record := &APIKeyRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).Unscoped().Where("api_key = ?", apiKey).First(record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

func billingPriceSnapshotForUsage(ctx context.Context, tx *gorm.DB, usage *UsageRecord) (*BillingModelPriceRecord, BillingPriceSnapshot, error) {
	if tx == nil {
		return nil, BillingPriceSnapshot{}, fmt.Errorf("database transaction is nil")
	}
	if usage == nil {
		return nil, BillingPriceSnapshot{}, fmt.Errorf("usage record is nil")
	}
	provider := strings.ToLower(strings.TrimSpace(usage.Provider))
	model := strings.TrimSpace(usage.Model)
	settings, errSettings := billingSettingsFromDB(ctx, tx)
	if errSettings != nil {
		return nil, BillingPriceSnapshot{}, errSettings
	}
	requestedTier := normalizeBillingEffectiveServiceTier(usage.ServiceTier)
	responseTierRaw := strings.TrimSpace(usage.ResponseServiceTier)
	effectiveTier := requestedTier
	responseFallback := false
	if settings.ServiceTierSource == BillingServiceTierSourceResponse {
		if responseTierRaw == "" {
			responseFallback = true
		} else {
			effectiveTier = normalizeBillingEffectiveServiceTier(responseTierRaw)
		}
	}
	price := &BillingModelPriceRecord{}
	result := tx.WithContext(contextOrBackground(ctx)).
		Where("provider = ? AND model = ? AND enabled = ? AND service_tier = ? AND min_input_tokens <= ?", provider, model, true, effectiveTier, usage.InputTokens).
		Order("min_input_tokens DESC, id ASC").
		Limit(1).
		Find(price)
	if result.Error != nil {
		return nil, BillingPriceSnapshot{}, result.Error
	}
	if result.RowsAffected == 0 {
		result = tx.WithContext(contextOrBackground(ctx)).
			Where("provider = ? AND model = ? AND enabled = ? AND service_tier = ? AND min_input_tokens <= ?", provider, model, true, BillingServiceTierWildcard, usage.InputTokens).
			Order("min_input_tokens DESC, id ASC").
			Limit(1).
			Find(price)
		if result.Error != nil {
			return nil, BillingPriceSnapshot{}, result.Error
		}
	}
	if result.RowsAffected == 0 {
		return nil, BillingPriceSnapshot{
			Provider:             provider,
			Model:                model,
			RequestedServiceTier: requestedTier,
			ResponseServiceTier:  responseTierRaw,
			ServiceTierSource:    settings.ServiceTierSource,
			EffectiveServiceTier: effectiveTier,
			ResponseTierFallback: responseFallback,
		}, nil
	}
	return price, BillingPriceSnapshot{
		Provider:                  price.Provider,
		Model:                     price.Model,
		InputPricePerMillion:      price.InputPricePerMillion,
		OutputPricePerMillion:     price.OutputPricePerMillion,
		CacheReadPricePerMillion:  price.CacheReadPricePerMillion,
		CacheWritePricePerMillion: price.CacheWritePricePerMillion,
		RequestPrice:              price.RequestPrice,
		MatchedServiceTier:        price.ServiceTier,
		MinInputTokens:            price.MinInputTokens,
		RequestedServiceTier:      requestedTier,
		ResponseServiceTier:       responseTierRaw,
		ServiceTierSource:         settings.ServiceTierSource,
		EffectiveServiceTier:      effectiveTier,
		ResponseTierFallback:      responseFallback,
	}, nil
}

func billingChargeAmount(usage *UsageRecord, snapshot BillingPriceSnapshot) float64 {
	if usage == nil {
		return 0
	}
	breakdown := usageTokenBreakdownForRecord(usage)
	if breakdown.Quality != UsageTokenAccountingQualityComplete {
		return billingLegacyChargeAmount(usage, snapshot)
	}
	inputTokens := breakdown.Input.UncachedTokens
	cacheReadTokens := breakdown.Input.CacheReadTokens
	cacheWriteTokens := breakdown.Input.CacheWriteTokens
	// A zero cache-write rate means the write bucket keeps the ordinary input
	// price, matching the existing billing contract without provider branching.
	if snapshot.CacheWritePricePerMillion <= 0 {
		inputTokens += cacheWriteTokens
		cacheWriteTokens = 0
	}
	return snapshot.RequestPrice +
		float64(inputTokens)*snapshot.InputPricePerMillion/1000000 +
		float64(breakdown.Output.TotalTokens)*snapshot.OutputPricePerMillion/1000000 +
		float64(cacheReadTokens)*snapshot.CacheReadPricePerMillion/1000000 +
		float64(cacheWriteTokens)*snapshot.CacheWritePricePerMillion/1000000
}

func billingLegacyChargeAmount(usage *UsageRecord, snapshot BillingPriceSnapshot) float64 {
	inputTokens := usage.InputTokens
	cacheReadTokens := normalizedUsageCacheReadTokens(usage.Provider, usage.ExecutorType, usage.CachedTokens, usage.CacheReadTokens, usage.CacheReadTokensPresent)
	cacheWriteTokens := usage.CacheCreationTokens
	if billingCacheTokensIncludedInInput(usage) {
		inputTokens -= cacheReadTokens
		if snapshot.CacheWritePricePerMillion > 0 {
			inputTokens -= cacheWriteTokens
		}
		if inputTokens < 0 {
			inputTokens = 0
		}
	}
	if snapshot.CacheWritePricePerMillion <= 0 {
		cacheWriteTokens = 0
	}
	return snapshot.RequestPrice +
		float64(inputTokens)*snapshot.InputPricePerMillion/1000000 +
		float64(usage.OutputTokens)*snapshot.OutputPricePerMillion/1000000 +
		float64(cacheReadTokens)*snapshot.CacheReadPricePerMillion/1000000 +
		float64(cacheWriteTokens)*snapshot.CacheWritePricePerMillion/1000000
}

// billingCacheTokensIncludedInInput reports whether cache read/write counts are
// already included inside usage.InputTokens (OpenAI/Codex/OpenAI-compatible).
func billingCacheTokensIncludedInInput(usage *UsageRecord) bool {
	if usage == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(usage.Provider))
	executorType := strings.ToLower(strings.TrimSpace(usage.ExecutorType))
	if executorType == "openaicompatexecutor" || provider == "openai-compatibility" || strings.HasPrefix(provider, "openai-compatible-") {
		return true
	}
	return !strings.Contains(provider, "claude") &&
		!strings.Contains(provider, "anthropic") &&
		!strings.Contains(executorType, "claude") &&
		!strings.Contains(executorType, "anthropic")
}

func billingUsageHasBillableActivity(usage *UsageRecord, amount float64) bool {
	if usage == nil {
		return false
	}
	return amount != 0 ||
		usage.AccountingTotalTokens > 0 ||
		usage.TotalTokens > 0 ||
		usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.CacheReadTokens > 0 ||
		usage.CacheCreationTokens > 0 ||
		usage.CachedTokens > 0
}

func billingCacheTokens(usage *UsageRecord) int64 {
	if usage == nil {
		return 0
	}
	breakdown := usageTokenBreakdownForRecord(usage)
	if breakdown.Quality != UsageTokenAccountingQualityComplete {
		return normalizedUsageCacheReadTokens(usage.Provider, usage.ExecutorType, usage.CachedTokens, usage.CacheReadTokens, usage.CacheReadTokensPresent) + usage.CacheCreationTokens
	}
	return breakdown.Input.CacheReadTokens + breakdown.Input.CacheWriteTokens
}

func billingAPIKeyID(record *APIKeyRecord) *uint {
	if record == nil || record.ID == 0 {
		return nil
	}
	value := record.ID
	return &value
}

func billingAPIKeyLabel(record *APIKeyRecord) string {
	if record == nil || record.ID == 0 {
		return ""
	}
	return fmt.Sprintf("api-key-%d", record.ID)
}

func billingMatchedPriceRule(record *BillingModelPriceRecord) string {
	if record == nil {
		return "default:zero"
	}
	return fmt.Sprintf("%s:%s:%s:%d", record.Provider, record.Model, record.ServiceTier, record.MinInputTokens)
}

func billingSettingsFromDB(ctx context.Context, db *gorm.DB) (BillingSettings, error) {
	defaults := BillingSettings{
		ServiceTierSource: BillingServiceTierSourceRequest,
		ReportTimezone:    BillingReportTimezoneUTC,
	}
	settings := defaults
	if db == nil {
		return settings, fmt.Errorf("database connection is nil")
	}
	record := KVRecord{}
	result := db.WithContext(contextOrBackground(ctx)).Where("key = ?", billingSettingsKVKey).Limit(1).Find(&record)
	if result.Error != nil {
		return settings, result.Error
	}
	if result.RowsAffected == 0 {
		return settings, nil
	}
	if errUnmarshal := json.Unmarshal(record.Value, &settings); errUnmarshal != nil {
		return defaults, nil
	}
	source, errSource := normalizeBillingServiceTierSource(settings.ServiceTierSource)
	if errSource != nil {
		settings.ServiceTierSource = BillingServiceTierSourceRequest
	} else {
		settings.ServiceTierSource = source
	}
	timezone, errTimezone := normalizeBillingReportTimezone(settings.ReportTimezone)
	if errTimezone != nil {
		settings.ReportTimezone = BillingReportTimezoneUTC
	} else {
		settings.ReportTimezone = timezone
	}
	return settings, nil
}

func normalizeBillingServiceTierSource(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", BillingServiceTierSourceRequest:
		return BillingServiceTierSourceRequest, nil
	case BillingServiceTierSourceResponse:
		return BillingServiceTierSourceResponse, nil
	default:
		return "", fmt.Errorf("service_tier_source must be request or response")
	}
}

func normalizeBillingReportTimezone(value string) (string, error) {
	timezone := strings.TrimSpace(value)
	if timezone == "" {
		return BillingReportTimezoneUTC, nil
	}
	if _, errLoad := time.LoadLocation(timezone); errLoad != nil {
		return "", fmt.Errorf("report_timezone must be a supported IANA timezone")
	}
	return timezone, nil
}

func normalizeBillingEffectiveServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto", "default", "standard":
		return "standard"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeBillingPriceServiceTier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return BillingServiceTierWildcard
	}
	if value == BillingServiceTierWildcard {
		return value
	}
	return normalizeBillingEffectiveServiceTier(value)
}

func validateBillingPriceServiceTier(value string) error {
	if strings.ContainsAny(strings.TrimSpace(value), " ,") {
		return fmt.Errorf("service_tier must be one normalized tier or *")
	}
	return nil
}

func firstNonEmptyBillingString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func maskBillingAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	if len(apiKey) <= 8 {
		return "****"
	}
	return apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
}

func billingPriceSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	switch source {
	case BillingPriceSourceDefault, BillingPriceSourceSync:
		return source
	default:
		return BillingPriceSourceManual
	}
}

func isBillingModelPriceConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "idx_billing_model_price_active_unique") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key")
}

func billingID(prefix string) string {
	var raw [billingIDRandomBytes]byte
	if _, errRead := rand.Read(raw[:]); errRead != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", prefix, time.Now().UnixNano())))
		return prefix + "_" + hex.EncodeToString(sum[:billingIDRandomBytes])
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func billingPayloadHash(payload string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(payload)))
	return hex.EncodeToString(sum[:])
}

func billingJSONB(value any) (JSONB, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return JSONB(raw), nil
}

func migrateBillingIndexes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if errDrop := db.Exec(`DROP INDEX IF EXISTS idx_billing_model_price_active_unique`).Error; errDrop != nil {
		return errDrop
	}
	if errDefaults := db.Model(&BillingModelPriceRecord{}).Where("service_tier IS NULL OR service_tier = ?", "").Update("service_tier", BillingServiceTierWildcard).Error; errDefaults != nil {
		return errDefaults
	}
	if errDefaults := db.Model(&BillingModelPriceRecord{}).Where("revision IS NULL OR revision < ?", 1).Update("revision", 1).Error; errDefaults != nil {
		return errDefaults
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_billing_model_price_active_unique ON billing_model_price (provider, model, service_tier, min_input_tokens) WHERE deleted_at IS NULL AND enabled = TRUE`).Error
}
