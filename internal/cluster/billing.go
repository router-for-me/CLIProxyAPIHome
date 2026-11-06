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
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/billingexpr"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
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
	BillingModelSourceRequest        = "request"
	BillingModelSourceResponse       = "response"
	BillingReportTimezoneUTC         = "UTC"
	billingSettingsKVKey             = "billing.settings"

	billingIDRandomBytes    = 12
	billingOverviewTopLimit = 10

	billingPriceModelSourceModel         = "model"
	billingPriceModelSourceResponseModel = "response_model"
	billingPriceModelSourceAlias         = "alias"

	// BillingCostDiagnosticUnpricedModel marks a charge whose usage matched no enabled price rule.
	BillingCostDiagnosticUnpricedModel = "unpriced_model"
	// BillingCostDiagnosticZeroPrice marks billable usage that matched a rule but cost nothing.
	BillingCostDiagnosticZeroPrice = "zero_price"
	// BillingCostDiagnosticPricingExprError marks a charge whose pricing expression
	// failed at runtime and fell back to the rule's price columns.
	BillingCostDiagnosticPricingExprError = "pricing_expr_error"
)

var (
	ErrBillingDuplicateModelPrice = errors.New("billing model price already exists")
	ErrBillingInvalidModelPrice   = errors.New("invalid billing model price")
)

type BillingModelPriceRecord struct {
	ID                        string  `gorm:"column:id;primaryKey"`
	Provider                  string  `gorm:"column:provider;not null;index:idx_billing_model_price_lookup,priority:1"`
	Model                     string  `gorm:"column:model;not null;index:idx_billing_model_price_lookup,priority:2"`
	ServiceTier               string  `gorm:"column:service_tier;not null;default:*"`
	MinInputTokens            int64   `gorm:"column:min_input_tokens;not null;default:0"`
	InputPricePerMillion      float64 `gorm:"column:input_price_per_million;not null;default:0"`
	OutputPricePerMillion     float64 `gorm:"column:output_price_per_million;not null;default:0"`
	CacheReadPricePerMillion  float64 `gorm:"column:cache_read_price_per_million;not null;default:0"`
	CacheWritePricePerMillion float64 `gorm:"column:cache_write_price_per_million;not null;default:0"`
	RequestPrice              float64 `gorm:"column:request_price;not null;default:0"`
	BillingExtraPrices        `gorm:"embedded"`
	// ReasoningEffortMultipliers maps a normalized reasoning effort to a token-cost multiplier.
	ReasoningEffortMultipliers JSONB `gorm:"column:reasoning_effort_multipliers"`
	// PricingExpr, when set, replaces the price columns (including request_price).
	PricingExpr string         `gorm:"column:pricing_expr;type:text"`
	Source      string         `gorm:"column:source;not null;default:manual"`
	Enabled     bool           `gorm:"column:enabled;not null;index:idx_billing_model_price_enabled"`
	Note        string         `gorm:"column:note;type:text"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at"`
	Revision    int64          `gorm:"column:revision;not null;default:1"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index"`
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
	ID            string  `gorm:"column:id;primaryKey;index:idx_billing_charge_time_order,priority:2,sort:desc"`
	UsageID       uint    `gorm:"column:usage_id;not null;uniqueIndex"`
	PayloadHash   string  `gorm:"column:payload_hash;not null;uniqueIndex"`
	UserID        *uint   `gorm:"column:user_id;index:idx_billing_charge_user_time,priority:1"`
	APIKeyID      *uint   `gorm:"column:api_key_id;index"`
	APIKeyLabel   string  `gorm:"column:api_key_label"`
	APIKeyMasked  string  `gorm:"column:api_key_masked"`
	Provider      string  `gorm:"column:provider;index:idx_billing_charge_provider_model_time,priority:1"`
	Model         string  `gorm:"column:model;index:idx_billing_charge_provider_model_time,priority:2"`
	OriginalModel string  `gorm:"column:original_model"`
	ActualModel   string  `gorm:"column:actual_model"`
	RequestID     string  `gorm:"column:request_id;index"`
	Endpoint      string  `gorm:"column:endpoint;index"`
	InputTokens   int64   `gorm:"column:input_tokens;not null;default:0"`
	OutputTokens  int64   `gorm:"column:output_tokens;not null;default:0"`
	CacheTokens   int64   `gorm:"column:cache_tokens;not null;default:0"`
	Amount        float64 `gorm:"column:amount;not null;default:0"`
	// BaseAmount is the list value before the user billing multiplier; summing it
	// by AuthIndex values credential output independently of customer discounts.
	BaseAmount        float64 `gorm:"column:base_amount;not null;default:0"`
	BillingMultiplier float64 `gorm:"column:billing_multiplier;not null"`
	AuthIndex         string  `gorm:"column:auth_index;not null;default:'';index"`
	BalanceBefore     float64 `gorm:"column:balance_before;not null;default:0"`
	BalanceAfter      float64 `gorm:"column:balance_after;not null;default:0"`
	MatchedPriceRule  string  `gorm:"column:matched_price_rule"`
	CostDiagnostic    string  `gorm:"column:cost_diagnostic;not null;default:'';index"`
	PriceSnapshot     JSONB   `gorm:"column:price_snapshot;not null"`
	RequestSummary    string  `gorm:"column:request_summary;type:text"`
	// Display-only cache split filled by ListBillingCharges from the linked usage
	// row's v2 token accounting; nil when the split is unknown.
	CacheReadTokens  *int64 `gorm:"-"`
	CacheWriteTokens *int64 `gorm:"-"`
	// Billed* buckets are the canonical token counts priced at charge time; they
	// stay NULL for legacy accounting and are immutable like the amount.
	BilledCacheReadTokens  *int64 `gorm:"column:billed_cache_read_tokens"`
	BilledCacheWriteTokens *int64 `gorm:"column:billed_cache_write_tokens"`
	BilledReasoningTokens  *int64 `gorm:"column:billed_reasoning_tokens"`
	// CostBreakdown itemizes the amount by priced token bucket.
	CostBreakdown JSONB     `gorm:"column:cost_breakdown"`
	CreatedAt     time.Time `gorm:"column:created_at;not null;index:idx_billing_charge_time_order,priority:1,sort:desc;index:idx_billing_charge_user_time,priority:2,sort:desc;index:idx_billing_charge_provider_model_time,priority:3,sort:desc"`
}

func (BillingChargeRecord) TableName() string { return "billing_charge" }

type BillingModelPriceUpdate struct {
	Provider                   string
	Model                      string
	ServiceTier                string
	MinInputTokens             int64
	InputPricePerMillion       float64
	OutputPricePerMillion      float64
	CacheReadPricePerMillion   float64
	CacheWritePricePerMillion  float64
	RequestPrice               float64
	Extra                      BillingExtraPrices
	ReasoningEffortMultipliers map[string]float64
	PricingExpr                string
	Source                     string
	Enabled                    bool
	Note                       string
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
	// ReasoningEffortMultipliers replaces the effort multiplier map; an empty map clears it.
	ReasoningEffortMultipliers *map[string]float64
	// PricingExpr sets the pricing expression; an empty string clears it.
	PricingExpr *string
	// Extra sets optional prices by field name; a present nil value clears the price.
	Extra   map[string]*float64
	Source  *string
	Enabled *bool
	Note    *string
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
	// AuthIndex filters charges by the serving credential.
	AuthIndex string
	// CostDiagnostic filters charges by diagnostic; "any" matches every non-empty diagnostic.
	CostDiagnostic string
	Limit          int
	Offset         int
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
	BillingExtraPrices
	ReasoningEffortMultipliers map[string]float64 `json:"reasoning_effort_multipliers,omitempty"`
	PricingExpr                string             `json:"pricing_expr,omitempty"`
	MatchedServiceTier         string             `json:"matched_service_tier"`
	MinInputTokens             int64              `json:"min_input_tokens"`
	ContextInputTokens         int64              `json:"context_input_tokens"`
	ModelSource                string             `json:"model_source,omitempty"`
	PriceModelSource           string             `json:"price_model_source,omitempty"`
	PriceMatch                 string             `json:"price_match,omitempty"`
	RequestedServiceTier       string             `json:"requested_service_tier"`
	ResponseServiceTier        string             `json:"response_service_tier,omitempty"`
	ServiceTierSource          string             `json:"service_tier_source"`
	EffectiveServiceTier       string             `json:"effective_service_tier"`
	ResponseTierFallback       bool               `json:"response_tier_fallback"`
	OutboundServiceTier        string             `json:"outbound_service_tier,omitempty"`
	// FailedRequest marks a failed request billed for consumed tokens only.
	FailedRequest  bool   `json:"failed_request,omitempty"`
	FailStatusCode int    `json:"fail_status_code,omitempty"`
	CostDiagnostic string `json:"cost_diagnostic,omitempty"`
}

type BillingSettings struct {
	ModelSource       string `json:"model_source"`
	ServiceTierSource string `json:"service_tier_source"`
	ReportTimezone    string `json:"report_timezone"`
	// AllowUnpricedModels lets user-owned API keys call models without any enabled
	// price rule. It defaults to false so a missing rule never silently bills $0;
	// an explicit rule with zero prices is still allowed.
	AllowUnpricedModels bool `json:"allow_unpriced_models"`
	// PublicModelPricing exposes model price ladders and availability metrics on
	// the unauthenticated public catalog endpoint (/user/models) when enabled.
	PublicModelPricing bool `json:"public_model_pricing"`
}

type BillingSettingsPatch struct {
	ModelSource         *string
	ServiceTierSource   *string
	ReportTimezone      *string
	AllowUnpricedModels *bool
	PublicModelPricing  *bool
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
	if patch.ModelSource != nil {
		modelSource, errModelSource := normalizeBillingModelSource(*patch.ModelSource)
		if errModelSource != nil {
			return BillingSettings{}, errModelSource
		}
		settings.ModelSource = modelSource
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
	if patch.AllowUnpricedModels != nil {
		settings.AllowUnpricedModels = *patch.AllowUnpricedModels
	}
	if patch.PublicModelPricing != nil {
		settings.PublicModelPricing = *patch.PublicModelPricing
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
	reasoningEffortMultipliers, errMultipliers := billingEffortMultipliersJSONB(update.ReasoningEffortMultipliers)
	if errMultipliers != nil {
		return nil, fmt.Errorf("%w: %v", ErrBillingInvalidModelPrice, errMultipliers)
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	record := &BillingModelPriceRecord{
		ID:                         billingID("price"),
		Provider:                   strings.ToLower(strings.TrimSpace(update.Provider)),
		Model:                      strings.TrimSpace(update.Model),
		ServiceTier:                normalizeBillingPriceServiceTier(update.ServiceTier),
		MinInputTokens:             update.MinInputTokens,
		InputPricePerMillion:       update.InputPricePerMillion,
		OutputPricePerMillion:      update.OutputPricePerMillion,
		CacheReadPricePerMillion:   update.CacheReadPricePerMillion,
		CacheWritePricePerMillion:  update.CacheWritePricePerMillion,
		RequestPrice:               update.RequestPrice,
		BillingExtraPrices:         update.Extra,
		ReasoningEffortMultipliers: reasoningEffortMultipliers,
		PricingExpr:                strings.TrimSpace(update.PricingExpr),
		Source:                     billingPriceSource(update.Source),
		Enabled:                    update.Enabled,
		Note:                       strings.TrimSpace(update.Note),
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
	update.Amount = billingRoundAmount(update.Amount)
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

	var rows []struct {
		BillingChargeRecord     `gorm:"embedded"`
		DisplayInputTokens      int64
		DisplayOutputTokens     int64
		DisplayCacheReadTokens  *int64
		DisplayCacheWriteTokens *int64
	}
	if errFind := scope.
		Joins(`LEFT JOIN "usage" AS "billing_usage" ON "billing_usage"."id" = "billing_charge"."usage_id"`).
		Select(`"billing_charge".*, ` + billingDisplayTokenSQL("input_tokens") + ` AS display_input_tokens, ` + billingDisplayTokenSQL("output_tokens") + ` AS display_output_tokens, ` + billingDisplayCacheSplitSQL("cache_read_tokens") + ` AS display_cache_read_tokens, ` + billingDisplayCacheSplitSQL("cache_write_tokens") + ` AS display_cache_write_tokens`).
		Order(`"billing_charge"."created_at" DESC, "billing_charge"."id" DESC`).Limit(query.Limit).Offset(query.Offset).Find(&rows).Error; errFind != nil {
		return BillingChargeResult{}, errFind
	}
	records := make([]BillingChargeRecord, len(rows))
	for index, row := range rows {
		records[index] = row.BillingChargeRecord
		records[index].InputTokens = row.DisplayInputTokens
		records[index].OutputTokens = row.DisplayOutputTokens
		records[index].CacheReadTokens = row.DisplayCacheReadTokens
		records[index].CacheWriteTokens = row.DisplayCacheWriteTokens
	}
	return BillingChargeResult{
		Records: records,
		Total:   total,
	}, nil
}

// billingDisplayTokenSQL keeps list and aggregate presentation consistent without
// rewriting charge snapshots or changing the counters used to calculate charges.
// column is an internal token column name, never request input.
func billingDisplayTokenSQL(column string) string {
	return fmt.Sprintf(`CASE WHEN "billing_usage"."token_accounting_version" = %d AND "billing_usage"."token_accounting_quality" = '%s'
		THEN "billing_usage"."accounting_%s" ELSE "billing_charge"."%s" END`,
		UsageTokenAccountingSchemaVersion, UsageTokenAccountingQualityComplete, column, column)
}

// billingDisplayCacheSplitSQL exposes the canonical cache read/write buckets only
// for rows with complete v2 token accounting. Other rows return NULL so clients
// keep the combined cache counter instead of showing a fabricated split.
func billingDisplayCacheSplitSQL(column string) string {
	return fmt.Sprintf(`CASE WHEN "billing_usage"."token_accounting_version" = %d AND "billing_usage"."token_accounting_quality" = '%s'
		THEN "billing_usage"."accounting_%s" ELSE NULL END`,
		UsageTokenAccountingSchemaVersion, UsageTokenAccountingQualityComplete, column)
}

func (r *Repository) BillingOverview(ctx context.Context, query BillingOverviewQuery) (BillingOverview, error) {
	db, errDB := r.database()
	if errDB != nil {
		return BillingOverview{}, errDB
	}
	ctx = contextOrBackground(ctx)

	chargeQuery := billingChargeQueryFromOverview(query)
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
	if errScan := billingOverviewTotalsQuery(db.WithContext(ctx), chargeQuery).Scan(&chargeTotals).Error; errScan != nil {
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
	topModels, topProviders, errRankings := billingTopModelsAndProviders(ctx, db, chargeQuery)
	if errRankings != nil {
		return BillingOverview{}, errRankings
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

	wanted := make([]string, 0, len(modelIDs)*2)
	seen := make(map[string]struct{}, len(modelIDs)*2)
	addWanted := func(raw string) {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		wanted = append(wanted, trimmed)
	}
	for _, modelID := range modelIDs {
		addWanted(modelID)
		addWanted(billingStripChannelPrefix(modelID))
	}
	if len(wanted) == 0 {
		return []BillingModelPriceRecord{}, nil
	}

	lowerWanted := make([]string, 0, len(wanted))
	for _, modelID := range wanted {
		lowerWanted = append(lowerWanted, strings.ToLower(modelID))
	}
	var records []BillingModelPriceRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Model(&BillingModelPriceRecord{}).
		Where("enabled = ?", true).
		Where("LOWER(model) IN ? OR model LIKE ?", lowerWanted, "%"+billingModelWildcard).
		Order("provider ASC, model ASC, service_tier ASC, min_input_tokens ASC, id ASC").
		Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	filtered := make([]BillingModelPriceRecord, 0, len(records))
	for _, record := range records {
		for _, modelID := range wanted {
			if billingModelMatches(record.Model, modelID, false) || billingModelMatches(record.Model, modelID, true) {
				filtered = append(filtered, record)
				break
			}
		}
	}
	return filtered, nil
}

func (r *Repository) createBillingChargeForUsageTx(ctx context.Context, tx *gorm.DB, usage *UsageRecord, payload string) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if usage == nil {
		return fmt.Errorf("usage record is nil")
	}
	// Failed requests are billed only for tokens the upstream reports as consumed,
	// such as a client-cancelled (499) stream after prompt processing.
	if usage.Failed && !billingUsageConsumedTokens(usage) {
		return nil
	}

	ctx = contextOrBackground(ctx)
	payloadHash := billingPayloadHash(payload)
	if hashCandidates := billingPayloadHashCandidates(payload, string(usage.PayloadJSON)); len(hashCandidates) > 1 {
		var existingCount int64
		if errCheck := tx.WithContext(ctx).Model(&BillingChargeRecord{}).
			Where("payload_hash IN ?", hashCandidates).
			Count(&existingCount).Error; errCheck != nil {
			return errCheck
		}
		if existingCount > 0 {
			return nil
		}
	}
	apiKey, errAPIKey := billingAPIKeyByValue(ctx, tx, usage.APIKey)
	if errAPIKey != nil {
		return errAPIKey
	}
	price, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, tx, usage)
	if errSnapshot != nil {
		return errSnapshot
	}
	if usage.Failed {
		snapshot = billingFailedUsageSnapshot(usage, snapshot)
	}
	cost := billingChargeCost(usage, snapshot)
	if errIter := billingApplyAdvisorIterationsTx(ctx, tx, usage, &cost); errIter != nil {
		return errIter
	}
	if !billingUsageHasBillableActivity(usage, cost.Total) {
		return nil
	}

	var userID *uint
	if apiKey != nil {
		userID = normalizeOptionalUserID(apiKey.UserID)
	}
	if userID != nil {
		multiplier, errMultiplier := billingUserMultiplierTx(ctx, tx, *userID)
		if errMultiplier != nil {
			return errMultiplier
		}
		cost.applyBillingMultiplier(multiplier)
	}
	amount := cost.Total
	snapshot.CostDiagnostic = billingCostDiagnostic(price, cost)
	priceSnapshot, errPriceSnapshot := billingJSONB(snapshot)
	if errPriceSnapshot != nil {
		return errPriceSnapshot
	}
	costBreakdown, errCostBreakdown := billingJSONB(cost)
	if errCostBreakdown != nil {
		return errCostBreakdown
	}
	cacheReadTokens, cacheWriteTokens, reasoningTokens := billingChargeTokenSplit(usage)

	charge := &BillingChargeRecord{
		ID:                     billingID("charge"),
		UsageID:                usage.ID,
		PayloadHash:            payloadHash,
		UserID:                 userID,
		APIKeyID:               billingAPIKeyID(apiKey),
		APIKeyLabel:            billingAPIKeyLabel(apiKey),
		APIKeyMasked:           maskBillingAPIKey(usage.APIKey),
		Provider:               strings.ToLower(strings.TrimSpace(usage.Provider)),
		Model:                  strings.TrimSpace(usage.Model),
		OriginalModel:          firstNonEmptyBillingString(usage.Alias, usage.Model),
		ActualModel:            firstNonEmptyBillingString(usagePayloadString(payload, "response_model"), usage.Model),
		RequestID:              strings.TrimSpace(usage.RequestID),
		Endpoint:               strings.TrimSpace(usage.Endpoint),
		InputTokens:            usage.InputTokens,
		OutputTokens:           usage.OutputTokens,
		CacheTokens:            billingCacheTokens(usage),
		BilledCacheReadTokens:  cacheReadTokens,
		BilledCacheWriteTokens: cacheWriteTokens,
		BilledReasoningTokens:  reasoningTokens,
		CostBreakdown:          costBreakdown,
		Amount:                 amount,
		BaseAmount:             cost.BaseTotal,
		BillingMultiplier:      cost.BillingMultiplier,
		AuthIndex:              strings.TrimSpace(usage.AuthIndex),
		MatchedPriceRule:       billingMatchedPriceRule(price),
		CostDiagnostic:         snapshot.CostDiagnostic,
		PriceSnapshot:          priceSnapshot,
		CreatedAt:              usage.Timestamp.UTC(),
	}
	insert := tx.WithContext(ctx).Select("*").Clauses(clause.OnConflict{
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
	if errPattern := validateBillingPricePattern(update.Provider, update.Model); errPattern != nil {
		return errPattern
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
	if errMultipliers := validateBillingEffortMultipliers(update.ReasoningEffortMultipliers); errMultipliers != nil {
		return errMultipliers
	}
	if expression := strings.TrimSpace(update.PricingExpr); expression != "" {
		if errExpr := billingexpr.Validate(expression); errExpr != nil {
			return fmt.Errorf("pricing_expr: %v", errExpr)
		}
	}
	return validateBillingExtraPrices(update.Extra.Values())
}

func validateBillingModelPricePatch(patch BillingModelPricePatch) error {
	if patch.Provider != nil && strings.TrimSpace(*patch.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if patch.Model != nil && strings.TrimSpace(*patch.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if patch.Provider != nil {
		if errPattern := validateBillingPricePattern(*patch.Provider, "model"); errPattern != nil {
			return errPattern
		}
	}
	if patch.Model != nil {
		if errPattern := validateBillingPricePattern("provider", *patch.Model); errPattern != nil {
			return errPattern
		}
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
	if patch.ReasoningEffortMultipliers != nil {
		if errMultipliers := validateBillingEffortMultipliers(*patch.ReasoningEffortMultipliers); errMultipliers != nil {
			return errMultipliers
		}
	}
	if patch.PricingExpr != nil {
		if expression := strings.TrimSpace(*patch.PricingExpr); expression != "" {
			if errExpr := billingexpr.Validate(expression); errExpr != nil {
				return fmt.Errorf("pricing_expr: %v", errExpr)
			}
		}
	}
	return validateBillingExtraPrices(patch.Extra)
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
	if patch.ReasoningEffortMultipliers != nil {
		multipliers, _ := billingEffortMultipliersJSONB(*patch.ReasoningEffortMultipliers)
		updates["reasoning_effort_multipliers"] = multipliers
	}
	if patch.PricingExpr != nil {
		updates["pricing_expr"] = strings.TrimSpace(*patch.PricingExpr)
	}
	for name, value := range patch.Extra {
		if value == nil {
			updates[name] = nil
			continue
		}
		updates[name] = *value
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
	delta = billingRoundAmount(delta)
	user := &UserRecord{}
	if errFirst := tx.WithContext(ctx).Select("id", "credits", "credits_unlimited").Where("id = ?", userID).First(user).Error; errFirst != nil {
		return 0, 0, errFirst
	}
	if delta == 0 || user.CreditsUnlimited {
		return user.Credits, user.Credits, nil
	}
	// Apply the delta inside SQL so concurrent writers serialize on the row lock
	// instead of retrying an exact float comparison that can exhaust under load.
	updated := &UserRecord{ID: userID}
	update := tx.WithContext(ctx).
		Model(updated).
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "credits"}}}).
		Where("credits_unlimited = ?", false).
		Update("credits", gorm.Expr("ROUND(CAST(credits AS NUMERIC) + CAST(? AS NUMERIC), 8)", delta))
	if update.Error != nil {
		return 0, 0, update.Error
	}
	if update.RowsAffected == 0 {
		// The user switched to unlimited credits after the first read.
		current := &UserRecord{}
		if errCurrent := tx.WithContext(ctx).Select("id", "credits").Where("id = ?", userID).First(current).Error; errCurrent != nil {
			return 0, 0, errCurrent
		}
		return current.Credits, current.Credits, nil
	}
	balanceAfter := updated.Credits
	return billingRoundAmount(balanceAfter - delta), balanceAfter, nil
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
	if authIndex := strings.TrimSpace(query.AuthIndex); authIndex != "" {
		scope = scope.Where(`"billing_charge"."auth_index" = ?`, authIndex)
	}
	switch diagnostic := strings.ToLower(strings.TrimSpace(query.CostDiagnostic)); diagnostic {
	case "":
	case "any":
		scope = scope.Where(`"billing_charge"."cost_diagnostic" <> ''`)
	default:
		scope = scope.Where(`"billing_charge"."cost_diagnostic" = ?`, diagnostic)
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
	// CPA reports the tier of the final outbound request after config overrides.
	// It is what the upstream was asked to serve, so it beats the client tier.
	outboundTierRaw := billingOutboundServiceTier(usage)
	effectiveTier := requestedTier
	if outboundTierRaw != "" {
		effectiveTier = normalizeBillingEffectiveServiceTier(outboundTierRaw)
	}
	responseFallback := false
	if settings.ServiceTierSource == BillingServiceTierSourceResponse {
		if responseTierRaw == "" {
			responseFallback = true
		} else {
			effectiveTier = normalizeBillingEffectiveServiceTier(responseTierRaw)
		}
	}
	contextInputTokens := billingContextInputTokens(usage)
	primaryCandidates, fallbackCandidates := billingPriceModelCandidateGroups(usage, settings.ModelSource)
	price, priceModelSource, priceMatch, errFind := billingFindPrice(ctx, tx, provider, primaryCandidates, effectiveTier, contextInputTokens)
	if errFind != nil {
		return nil, BillingPriceSnapshot{}, errFind
	}
	if price == nil && len(fallbackCandidates) > 0 {
		price, priceModelSource, priceMatch, errFind = billingFindPrice(ctx, tx, provider, fallbackCandidates, effectiveTier, contextInputTokens)
		if errFind != nil {
			return nil, BillingPriceSnapshot{}, errFind
		}
	}
	if price == nil {
		return nil, BillingPriceSnapshot{
			Provider:             provider,
			Model:                model,
			ContextInputTokens:   contextInputTokens,
			ModelSource:          settings.ModelSource,
			RequestedServiceTier: requestedTier,
			ResponseServiceTier:  responseTierRaw,
			ServiceTierSource:    settings.ServiceTierSource,
			EffectiveServiceTier: effectiveTier,
			ResponseTierFallback: responseFallback,
			OutboundServiceTier:  outboundTierRaw,
		}, nil
	}
	return price, BillingPriceSnapshot{
		Provider:                   price.Provider,
		Model:                      price.Model,
		InputPricePerMillion:       price.InputPricePerMillion,
		OutputPricePerMillion:      price.OutputPricePerMillion,
		CacheReadPricePerMillion:   price.CacheReadPricePerMillion,
		CacheWritePricePerMillion:  price.CacheWritePricePerMillion,
		RequestPrice:               price.RequestPrice,
		BillingExtraPrices:         price.BillingExtraPrices,
		ReasoningEffortMultipliers: billingEffortMultipliersFromJSONB(price.ReasoningEffortMultipliers),
		PricingExpr:                price.PricingExpr,
		MatchedServiceTier:         price.ServiceTier,
		MinInputTokens:             price.MinInputTokens,
		ContextInputTokens:         contextInputTokens,
		ModelSource:                settings.ModelSource,
		PriceModelSource:           priceModelSource,
		PriceMatch:                 priceMatch,
		RequestedServiceTier:       requestedTier,
		ResponseServiceTier:        responseTierRaw,
		ServiceTierSource:          settings.ServiceTierSource,
		EffectiveServiceTier:       effectiveTier,
		ResponseTierFallback:       responseFallback,
		OutboundServiceTier:        outboundTierRaw,
	}, nil
}

type billingPriceModelCandidate struct {
	model  string
	source string
}

// billingStripChannelPrefix returns the bare model name after a leading "prefix/"
// segment when present, or empty when the model has no prefix.
func billingStripChannelPrefix(model string) string {
	model = strings.TrimSpace(model)
	slash := strings.IndexByte(model, '/')
	if slash <= 0 || slash >= len(model)-1 {
		return ""
	}
	return strings.TrimSpace(model[slash+1:])
}

// billingPriceModelCandidateGroups splits price lookup candidates into primary and
// fallback groups according to modelSource ("request" by default, or "response").
func billingPriceModelCandidateGroups(usage *UsageRecord, modelSource string) ([]billingPriceModelCandidate, []billingPriceModelCandidate) {
	model := strings.TrimSpace(usage.Model)
	alias := strings.TrimSpace(usage.Alias)
	requestedModel := strings.TrimSpace(usagePayloadString(string(usage.PayloadJSON), "requested_model"))
	responseModel := strings.TrimSpace(usagePayloadString(string(usage.PayloadJSON), "response_model"))

	buildGroup := func(seen map[string]struct{}, items ...billingPriceModelCandidate) []billingPriceModelCandidate {
		var out []billingPriceModelCandidate
		for _, item := range items {
			name := strings.TrimSpace(item.model)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, billingPriceModelCandidate{model: name, source: item.source})
		}
		return out
	}

	reqItems := []billingPriceModelCandidate{
		{model: model, source: billingPriceModelSourceModel},
		{model: alias, source: billingPriceModelSourceAlias},
		{model: requestedModel, source: billingPriceModelSourceAlias},
		{model: billingStripChannelPrefix(model), source: billingPriceModelSourceModel},
		{model: billingStripChannelPrefix(alias), source: billingPriceModelSourceAlias},
		{model: billingStripChannelPrefix(requestedModel), source: billingPriceModelSourceAlias},
	}
	respItems := []billingPriceModelCandidate{
		{model: responseModel, source: billingPriceModelSourceResponseModel},
		{model: billingStripChannelPrefix(responseModel), source: billingPriceModelSourceResponseModel},
	}

	seen := make(map[string]struct{}, 8)
	if strings.EqualFold(strings.TrimSpace(modelSource), BillingModelSourceResponse) {
		primary := buildGroup(seen, respItems...)
		fallback := buildGroup(seen, reqItems...)
		if len(primary) == 0 {
			return fallback, nil
		}
		return primary, fallback
	}
	primary := buildGroup(seen, reqItems...)
	fallback := buildGroup(seen, respItems...)
	return primary, fallback
}

func billingPriceModelCandidates(usage *UsageRecord) []billingPriceModelCandidate {
	primary, fallback := billingPriceModelCandidateGroups(usage, BillingModelSourceRequest)
	return append(primary, fallback...)
}

// billingContextInputTokens returns the full prompt context used for context-band
// matching: uncached input plus cache reads and cache writes. Claude-style usage
// reports cache tokens outside input_tokens, so raw input_tokens undercounts it.
func billingContextInputTokens(usage *UsageRecord) int64 {
	if usage == nil {
		return 0
	}
	breakdown := usageTokenBreakdownForRecord(usage)
	if breakdown.Quality == UsageTokenAccountingQualityComplete {
		return breakdown.Input.TotalTokens
	}
	inputTokens := usage.InputTokens
	if !billingCacheTokensIncludedInInput(usage) {
		cacheReadTokens := normalizedUsageCacheReadTokens(usage.Provider, usage.ExecutorType, usage.CachedTokens, usage.CacheReadTokens, usage.CacheReadTokensPresent)
		inputTokens += cacheReadTokens + usage.CacheCreationTokens
	}
	if inputTokens < 0 {
		return 0
	}
	return inputTokens
}

// billingAmountScale quantizes money to 8 decimal places before it is stored so
// balances and charge sums stay reconcilable across SQLite and PostgreSQL.
const billingAmountScale = 1e8

// billingRoundAmount rounds a USD amount to 8 decimal places.
func billingRoundAmount(value float64) float64 {
	return math.Round(value*billingAmountScale) / billingAmountScale
}

// BillingCostItem is one priced token bucket of a charge.
type BillingCostItem struct {
	Tokens          int64   `json:"tokens"`
	PricePerMillion float64 `json:"price_per_million"`
	Cost            float64 `json:"cost"`
}

// BillingCostBreakdown itemizes how a charge amount was computed.
type BillingCostBreakdown struct {
	Request float64                    `json:"request,omitempty"`
	Items   map[string]BillingCostItem `json:"items,omitempty"`
	// ReasoningEffortMultiplier scales the token cost (not the request price).
	ReasoningEffort           string  `json:"reasoning_effort,omitempty"`
	ReasoningEffortMultiplier float64 `json:"reasoning_effort_multiplier,omitempty"`
	// PricingExpr is set when the amount came from the rule's pricing expression.
	PricingExpr string `json:"pricing_expr,omitempty"`
	// MatchedTier records the tier name selected by tier(name, expr) in pricing_expr.
	MatchedTier string `json:"matched_tier,omitempty"`
	// ExpressionError records a runtime expression failure that fell back to columns.
	ExpressionError string `json:"expression_error,omitempty"`
	// Surcharges are per-unit fees (web search calls, images); like the request
	// price they are not scaled by the reasoning effort multiplier.
	Surcharges float64 `json:"surcharges,omitempty"`
	// BaseTotal is the list amount before the user billing multiplier.
	BaseTotal         float64 `json:"base_total"`
	BillingMultiplier float64 `json:"billing_multiplier"`
	Total             float64 `json:"total"`
}

func (b *BillingCostBreakdown) add(name string, tokens int64, pricePerMillion float64) {
	if tokens <= 0 {
		return
	}
	if b.Items == nil {
		b.Items = make(map[string]BillingCostItem)
	}
	cost := float64(tokens) * pricePerMillion / 1000000
	b.Items[name] = BillingCostItem{Tokens: tokens, PricePerMillion: pricePerMillion, Cost: cost}
	b.Total += cost
}

// addUnits records a per-unit surcharge item.
func (b *BillingCostBreakdown) addUnits(name string, units int64, unitPrice *float64) {
	if units <= 0 || unitPrice == nil {
		return
	}
	if b.Items == nil {
		b.Items = make(map[string]BillingCostItem)
	}
	cost := float64(units) * *unitPrice
	b.Items[name] = BillingCostItem{Tokens: units, PricePerMillion: *unitPrice, Cost: cost}
	b.Surcharges += cost
}

// addSurcharges bills per-unit counters with the rule's per-unit prices.
func (b *BillingCostBreakdown) addSurcharges(details billingTokenDetails, snapshot BillingPriceSnapshot) {
	b.addUnits("web_search_calls", details.WebSearchCalls, snapshot.WebSearchPricePerCall)
	if strings.EqualFold(strings.TrimSpace(snapshot.Provider), "xai") {
		b.addUnits("x_search_calls", details.XSearchCalls, snapshot.WebSearchPricePerCall)
		b.addUnits("code_execution_calls", details.CodeExecutionCalls, snapshot.WebSearchPricePerCall)
	}
	b.addUnits("images", details.Images, snapshot.ImagePricePerImage)
}

func (b *BillingCostBreakdown) finish(requestPrice float64) {
	tokenCost := b.Total
	if b.ReasoningEffort != "" {
		tokenCost *= b.ReasoningEffortMultiplier
	}
	b.Request = requestPrice
	b.BaseTotal = billingRoundAmount(tokenCost + requestPrice + b.Surcharges)
	b.BillingMultiplier = 1
	b.Total = b.BaseTotal
}

// applyBillingMultiplier scales the list amount by a customer multiplier.
func (b *BillingCostBreakdown) applyBillingMultiplier(multiplier float64) {
	if multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		multiplier = 1
	}
	b.BillingMultiplier = multiplier
	b.Total = billingRoundAmount(b.BaseTotal * multiplier)
}

func billingChargeAmount(usage *UsageRecord, snapshot BillingPriceSnapshot) float64 {
	return billingChargeCost(usage, snapshot).Total
}

// billingChargeCost prices usage against a snapshot and returns the itemized cost.
func billingChargeCost(usage *UsageRecord, snapshot BillingPriceSnapshot) BillingCostBreakdown {
	cost := BillingCostBreakdown{}
	if usage == nil {
		return cost
	}
	cost.ReasoningEffort, cost.ReasoningEffortMultiplier = billingEffortMultiplier(usage.Effort, snapshot.ReasoningEffortMultipliers)
	if expression := strings.TrimSpace(snapshot.PricingExpr); expression != "" {
		errExpr := billingExpressionChargeCost(&cost, usage, snapshot, expression)
		if errExpr == nil {
			cost.finish(0)
			return cost
		}
		// A runtime failure falls back to the price columns and is flagged.
		cost = BillingCostBreakdown{
			ReasoningEffort:           cost.ReasoningEffort,
			ReasoningEffortMultiplier: cost.ReasoningEffortMultiplier,
			ExpressionError:           errExpr.Error(),
		}
	}
	breakdown := usageTokenBreakdownForRecord(usage)
	if breakdown.Quality != UsageTokenAccountingQualityComplete {
		billingLegacyChargeCost(&cost, usage, snapshot)
		cost.addSurcharges(billingTokenDetailsForUsage(usage), snapshot)
		cost.finish(snapshot.RequestPrice)
		return cost
	}
	details := billingTokenDetailsForUsage(usage)
	billingCompleteChargeCost(&cost, breakdown, details, snapshot)
	cost.addSurcharges(details, snapshot)
	cost.finish(snapshot.RequestPrice)
	return cost
}

// billingCompleteChargeCost prices complete v2 accounting. Optional detail buckets
// are carved out of their canonical parent bucket, so totals never change and
// unset detail prices fall back to the parent price.
func billingCompleteChargeCost(cost *BillingCostBreakdown, breakdown UsageTokenBreakdown, details billingTokenDetails, snapshot BillingPriceSnapshot) {
	uncached := breakdown.Input.UncachedTokens
	cacheWrite := breakdown.Input.CacheWriteTokens
	cacheWrite30m := clampBillingTokens(details.CacheWrite30m, cacheWrite)
	cacheWrite1h := clampBillingTokens(details.CacheWrite1h, cacheWrite-cacheWrite30m)
	cacheWrite12h := clampBillingTokens(details.CacheWrite12h, cacheWrite-cacheWrite30m-cacheWrite1h)
	cacheWrite5m := cacheWrite - cacheWrite30m - cacheWrite1h - cacheWrite12h
	imageInput, audioInput, _, _, _, _, _ := billingCarveInputModalities(uncached, breakdown.Input.CacheReadTokens, details)
	textInput := uncached - imageInput - audioInput

	// A zero cache-write rate means the 5m write bucket keeps the ordinary input
	// price, matching the existing billing contract without provider branching.
	cacheWritePrice := snapshot.CacheWritePricePerMillion
	if cacheWritePrice <= 0 {
		textInput += cacheWrite5m
		cacheWrite5m = 0
		cacheWritePrice = snapshot.InputPricePerMillion
	}
	cacheWrite30mPrice := billingPriceOr(snapshot.CacheWrite30mPricePerMillion, cacheWritePrice)
	cacheWrite1hPrice := billingPriceOr(snapshot.CacheWrite1hPricePerMillion, cacheWritePrice)
	cacheWrite12hPrice := billingPriceOr(snapshot.CacheWrite12hPricePerMillion, billingPriceOr(snapshot.CacheWrite1hPricePerMillion, cacheWrite30mPrice))

	reasoning := breakdown.Output.ReasoningTokens
	nonReasoning := breakdown.Output.NonReasoningTokens
	imageOutput := clampBillingTokens(details.ImageOutput, nonReasoning)
	audioOutput := clampBillingTokens(details.AudioOutput, nonReasoning-imageOutput)
	textOutput := nonReasoning - imageOutput - audioOutput
	if snapshot.ReasoningPricePerMillion == nil {
		textOutput += reasoning
		reasoning = 0
	}

	cost.add("input", textInput, snapshot.InputPricePerMillion)
	cost.add("image_input", imageInput, billingPriceOr(snapshot.ImageInputPricePerMillion, snapshot.InputPricePerMillion))
	cost.add("audio_input", audioInput, billingPriceOr(snapshot.AudioInputPricePerMillion, snapshot.InputPricePerMillion))
	cost.add("cache_read", breakdown.Input.CacheReadTokens, snapshot.CacheReadPricePerMillion)
	cost.add("cache_write", cacheWrite5m, snapshot.CacheWritePricePerMillion)
	cost.add("cache_write_30m", cacheWrite30m, cacheWrite30mPrice)
	cost.add("cache_write_1h", cacheWrite1h, cacheWrite1hPrice)
	cost.add("cache_write_12h", cacheWrite12h, cacheWrite12hPrice)
	cost.add("output", textOutput, snapshot.OutputPricePerMillion)
	cost.add("reasoning", reasoning, billingPriceOr(snapshot.ReasoningPricePerMillion, snapshot.OutputPricePerMillion))
	cost.add("image_output", imageOutput, billingPriceOr(snapshot.ImageOutputPricePerMillion, snapshot.OutputPricePerMillion))
	cost.add("audio_output", audioOutput, billingPriceOr(snapshot.AudioOutputPricePerMillion, snapshot.OutputPricePerMillion))
}

func billingLegacyChargeCost(cost *BillingCostBreakdown, usage *UsageRecord, snapshot BillingPriceSnapshot) {
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
	cost.add("input", inputTokens, snapshot.InputPricePerMillion)
	cost.add("cache_read", cacheReadTokens, snapshot.CacheReadPricePerMillion)
	cost.add("cache_write", cacheWriteTokens, snapshot.CacheWritePricePerMillion)
	cost.add("output", usage.OutputTokens, snapshot.OutputPricePerMillion)
}

// billingChargeTokenSplit returns the canonical cache and reasoning buckets for
// complete v2 accounting, or nil values when the split is unknown.
func billingChargeTokenSplit(usage *UsageRecord) (*int64, *int64, *int64) {
	breakdown := usageTokenBreakdownForRecord(usage)
	if breakdown.Quality != UsageTokenAccountingQualityComplete {
		return nil, nil, nil
	}
	cacheRead := breakdown.Input.CacheReadTokens
	cacheWrite := breakdown.Input.CacheWriteTokens
	reasoning := breakdown.Output.ReasoningTokens
	return &cacheRead, &cacheWrite, &reasoning
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
	if amount != 0 ||
		usage.AccountingTotalTokens > 0 ||
		usage.TotalTokens > 0 ||
		usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.CacheReadTokens > 0 ||
		usage.CacheCreationTokens > 0 ||
		usage.CachedTokens > 0 {
		return true
	}
	details := billingTokenDetailsForUsage(usage)
	return details.hasUnitActivity()
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

// billingCostDiagnostic explains why billable usage produced no charge.
func billingCostDiagnostic(price *BillingModelPriceRecord, cost BillingCostBreakdown) string {
	amount := cost.Total
	if price == nil {
		return BillingCostDiagnosticUnpricedModel
	}
	if cost.ExpressionError != "" {
		return BillingCostDiagnosticPricingExprError
	}
	if amount == 0 {
		return BillingCostDiagnosticZeroPrice
	}
	return ""
}

// billingModelHasEnabledPrice reports whether any enabled price rule names the model.
// Provider is not known before dispatch selects a credential, so any provider counts.
func billingModelHasEnabledPrice(ctx context.Context, db *gorm.DB, model string) (bool, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return false, nil
	}
	bareModel := billingStripChannelPrefix(model)
	names := []string{strings.ToLower(model)}
	if bareModel != "" {
		names = append(names, strings.ToLower(bareModel))
	}
	var models []string
	if errFind := db.WithContext(contextOrBackground(ctx)).
		Model(&BillingModelPriceRecord{}).
		Where("enabled = ? AND (LOWER(model) IN ? OR model LIKE ?)", true, names, "%"+billingModelWildcard).
		Distinct().
		Pluck("model", &models).Error; errFind != nil {
		return false, errFind
	}
	for _, ruleModel := range models {
		if billingModelMatches(ruleModel, model, false) || billingModelMatches(ruleModel, model, true) {
			return true, nil
		}
		if bareModel != "" && (billingModelMatches(ruleModel, bareModel, false) || billingModelMatches(ruleModel, bareModel, true)) {
			return true, nil
		}
	}
	return false, nil
}

// ensureAPIKeyModelPriced rejects dispatch for user-owned API keys when the requested
// model has no enabled price rule, unless unpriced models are explicitly allowed.
func ensureAPIKeyModelPriced(ctx context.Context, db *gorm.DB, record *APIKeyRecord, model string) error {
	if record == nil || normalizeOptionalUserID(record.UserID) == nil {
		return nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	settings, errSettings := billingSettingsFromDB(ctx, db)
	if errSettings != nil {
		return errSettings
	}
	if settings.AllowUnpricedModels {
		return nil
	}
	priced, errPriced := billingModelHasEnabledPrice(ctx, db, model)
	if errPriced != nil {
		return errPriced
	}
	if priced {
		return nil
	}
	// CPA maps model_not_found to a non-retryable client error.
	return fmt.Errorf("%s: %s", homeerrors.TypeModelNotFound, fmt.Sprintf(homeerrors.MessageModelPriceNotConfiguredFmt, model))
}

func billingMatchedPriceRule(record *BillingModelPriceRecord) string {
	if record == nil {
		return "default:zero"
	}
	return fmt.Sprintf("%s:%s:%s:%d", record.Provider, record.Model, record.ServiceTier, record.MinInputTokens)
}

func billingSettingsFromDB(ctx context.Context, db *gorm.DB) (BillingSettings, error) {
	defaults := BillingSettings{
		ModelSource:       BillingModelSourceRequest,
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
	modelSource, errModelSource := normalizeBillingModelSource(settings.ModelSource)
	if errModelSource != nil {
		settings.ModelSource = BillingModelSourceRequest
	} else {
		settings.ModelSource = modelSource
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

func normalizeBillingModelSource(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", BillingModelSourceRequest:
		return BillingModelSourceRequest, nil
	case BillingModelSourceResponse:
		return BillingModelSourceResponse, nil
	default:
		return "", fmt.Errorf("model_source must be request or response")
	}
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
	if dedupeKey, errDedupe := usagePayloadDedupeKey(payload); errDedupe == nil && dedupeKey != "" {
		sum := sha256.Sum256([]byte(dedupeKey))
		return hex.EncodeToString(sum[:])
	}
	return legacyBillingPayloadHash(payload)
}

func legacyBillingPayloadHash(payload string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(payload)))
	return hex.EncodeToString(sum[:])
}

func billingPayloadHashCandidates(payloads ...string) []string {
	seen := make(map[string]struct{}, len(payloads)*2)
	candidates := make([]string, 0, len(payloads)*2)
	add := func(hash string) {
		if hash == "" {
			return
		}
		if _, exists := seen[hash]; exists {
			return
		}
		seen[hash] = struct{}{}
		candidates = append(candidates, hash)
	}
	for _, payload := range payloads {
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			continue
		}
		add(billingPayloadHash(trimmed))
		add(legacyBillingPayloadHash(trimmed))
	}
	return candidates
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
	if errBaseAmount := db.Model(&BillingChargeRecord{}).
		Where("base_amount = ? AND amount <> ? AND billing_multiplier = ?", 0, 0, 1).
		Update("base_amount", gorm.Expr("amount")).Error; errBaseAmount != nil {
		return errBaseAmount
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_billing_model_price_active_unique ON billing_model_price (provider, model, service_tier, min_input_tokens) WHERE deleted_at IS NULL AND enabled = TRUE`).Error
}
