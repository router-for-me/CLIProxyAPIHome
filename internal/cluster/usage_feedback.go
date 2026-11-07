package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// Pending feedback is bounded per credential/model. A full batch is drained by
// the reporting call before CPA retries; no background worker or new table is used.
const (
	maxPendingUsageFeedback = 64
	// Separate lock domains prevent dedupe/scope hash collisions reversing lock order.
	usageFeedbackLockNamespace int32 = 0x55534642 // "USFB"
)

var ErrUsageFeedbackBacklog = errors.New("pending usage feedback limit reached")

type usageFeedbackEvent struct {
	Key                  string          `json:"key"`
	Result               coreauth.Result `json:"result"`
	UsageStored          bool            `json:"usage_stored"`
	QuotaIdentityVersion int64           `json:"quota_identity_version"`
}

type usageFeedbackScope struct {
	Pending []usageFeedbackEvent `json:"pending"`
}

func prepareUsageFeedback(payload, dedupeKey string, record *UsageRecord, feedback *coreauth.UsageFeedback) {
	if feedback == nil {
		return
	}
	*feedback = coreauth.UsageFeedback{}
	model := coreauth.CanonicalModelID(record.Model)
	if record.AuthIndex == "" || model == "" {
		return
	}
	status := record.FailStatusCode
	if status <= 0 {
		status = 200
		if record.Failed {
			status = 500
		}
	}
	code := strings.TrimSpace(gjson.Get(payload, "fail.code").String())
	result := coreauth.NewUsageResultWithCode(record.AuthIndex, record.Provider, model, status, record.FailBody, code)
	if result.RequestScoped() {
		return
	}
	result.AccessTokenSHA256 = strings.TrimSpace(gjson.Get(payload, "access_token_sha256").String())
	// A retry must not restart a relative provider reset period.
	if status == 429 {
		if resetAt := result.QuotaResetTime(record.CreatedAt); !resetAt.IsZero() {
			result.ResetAt, result.RetryAfter = &resetAt, nil
		}
	}
	feedback.Result = result
	event := sha256.Sum256([]byte(dedupeKey))
	feedback.EventKey = fmt.Sprintf("internal:usage-feedback:event:%x", event)
}

// loadUsageFeedbackTx holds the same scope lock during ingestion and auth
// mutation. MutateAuth selects eligible transitions in admission order after
// checking their generation against the locked auth row.
func loadUsageFeedbackTx(ctx context.Context, tx *gorm.DB) (*KVRecord, *usageFeedbackScope, error) {
	feedback := coreauth.UsageFeedbackFromContext(ctx)
	if feedback == nil || feedback.ScopeKey == "" {
		return nil, nil, nil
	}
	feedback.Pending = nil
	if errLock := lockUsageIngestionTx(ctx, tx, usageFeedbackLockNamespace, feedback.ScopeKey); errLock != nil {
		return nil, nil, errLock
	}
	record := &KVRecord{Key: feedback.ScopeKey}
	scope := &usageFeedbackScope{Pending: []usageFeedbackEvent{}}
	errScope := tx.Where("key = ?", feedback.ScopeKey).First(record).Error
	if errors.Is(errScope, gorm.ErrRecordNotFound) {
		return record, scope, nil
	}
	if errScope != nil {
		return nil, nil, errScope
	}
	scope.Pending = nil
	if errDecode := json.Unmarshal(record.Value, scope); errDecode != nil {
		return nil, nil, fmt.Errorf("decode usage feedback: %w", errDecode)
	}
	// Require the pending field: an unknown or corrupt encoding must not be
	// acknowledged as an empty batch.
	if scope.Pending == nil || len(scope.Pending) > maxPendingUsageFeedback {
		return nil, nil, fmt.Errorf("invalid usage feedback batch")
	}
	seen := make(map[string]bool, len(scope.Pending))
	for _, event := range scope.Pending {
		if event.Key == "" || seen[event.Key] || event.QuotaIdentityVersion <= 0 || event.Result.AuthID != feedback.Result.AuthID ||
			coreauth.CanonicalModelID(event.Result.Model) != coreauth.CanonicalModelID(feedback.Result.Model) {
			return nil, nil, fmt.Errorf("invalid usage feedback event")
		}
		seen[event.Key] = true
	}
	return record, scope, nil
}

func stageUsageFeedbackTx(ctx context.Context, tx *gorm.DB, stored bool) (*KVRecord, *usageFeedbackScope, error) {
	feedback := coreauth.UsageFeedbackFromContext(ctx)
	if feedback == nil || feedback.EventKey == "" {
		return nil, nil, nil
	}
	// Historical usage for deleted/unknown credentials remains ingestible, but
	// must not accumulate feedback which no auth state machine can consume.
	auth, found, errResolve := resolveQuotaCredentialByAuthIndex(ctx, tx, feedback.Result.AuthIndex)
	if errResolve != nil {
		return nil, nil, errResolve
	}
	if !found {
		*feedback = coreauth.UsageFeedback{}
		return nil, nil, nil
	}
	feedback.Result.AuthID = auth.UUID
	// Group feedback by the authoritative credential identity.
	scopeKey := sha256.Sum256([]byte(auth.UUID + "\x00" + feedback.Result.Model))
	feedback.ScopeKey = fmt.Sprintf("internal:usage-feedback:scope:%x", scopeKey)
	record, scope, errLoad := loadUsageFeedbackTx(ctx, tx)
	if errLoad != nil || scope == nil {
		return nil, nil, errLoad
	}
	var receipt KVRecord
	errReceipt := tx.Where("key = ?", feedback.EventKey).First(&receipt).Error
	if errReceipt == nil {
		if string(receipt.Value) != "applied" {
			return nil, nil, fmt.Errorf("invalid usage feedback receipt")
		}
		feedback.Applied = true
		return record, nil, nil
	}
	if !errors.Is(errReceipt, gorm.ErrRecordNotFound) {
		return nil, nil, errReceipt
	}
	for i := range scope.Pending {
		if scope.Pending[i].Key == feedback.EventKey {
			feedback.NeedsFeedback = true
			return record, scope, nil
		}
	}
	if stored {
		// Previously committed events without pending feedback remain duplicates.
		return record, nil, nil
	}
	if len(scope.Pending) >= maxPendingUsageFeedback {
		return nil, nil, ErrUsageFeedbackBacklog
	}
	feedback.NeedsFeedback = true
	// Capture the identity once; retries must not retag an existing event.
	scope.Pending = append(scope.Pending, usageFeedbackEvent{
		Key: feedback.EventKey, Result: feedback.Result,
		QuotaIdentityVersion: quotaCredentialIdentityVersion(auth),
	})
	return record, scope, nil
}

func storeUsageFeedbackTx(tx *gorm.DB, record *KVRecord, scope *usageFeedbackScope, feedback *coreauth.UsageFeedback, stored bool) error {
	if scope != nil {
		for i := range scope.Pending {
			if scope.Pending[i].Key == feedback.EventKey {
				scope.Pending[i].UsageStored = stored
			}
		}
		value, errEncode := json.Marshal(scope)
		if errEncode != nil {
			return fmt.Errorf("encode usage feedback: %w", errEncode)
		}
		if record.Version == 0 {
			if errCreate := tx.Create(&KVRecord{Key: record.Key, Value: value, Version: 1}).Error; errCreate != nil {
				return errCreate
			}
		} else if errUpdate := tx.Model(&KVRecord{}).Where("key = ?", record.Key).Updates(map[string]any{"value": value, "version": record.Version + 1}).Error; errUpdate != nil {
			return errUpdate
		}
	}
	if feedback != nil && feedback.Applied && stored {
		return tx.Where("key = ?", feedback.EventKey).Delete(&KVRecord{}).Error
	}
	return nil
}

// Completion and the auth update commit together, including retirement of
// obsolete identities. Events whose usage write still needs retry retain a
// receipt so they cannot be admitted again under a new identity. Any failure
// rolls back both availability and completion.
func completeUsageFeedbackTx(tx *gorm.DB, record *KVRecord, scope *usageFeedbackScope) error {
	if scope == nil || len(scope.Pending) == 0 {
		return nil
	}
	for _, event := range scope.Pending {
		if !event.UsageStored {
			if errCreate := tx.Create(&KVRecord{Key: event.Key, Value: []byte("applied"), Version: 1}).Error; errCreate != nil {
				return errCreate
			}
		}
	}
	return tx.Where("key = ?", record.Key).Delete(&KVRecord{}).Error
}
