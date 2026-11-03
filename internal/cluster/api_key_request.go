package cluster

import (
	"context"
	"sync"

	"gorm.io/gorm"
)

type apiKeyRequestCacheContextKey struct{}

type apiKeyRequestCache struct {
	mu     sync.Mutex
	repo   *Repository
	record APIKeyRecord
}

// WithAPIKeyRequestCache shares the authenticated key row with access resolution.
// Call once per request, never on a connection or a long-lived runtime context.
// Billing and group permissions are deliberately not cached.
func WithAPIKeyRequestCache(ctx context.Context) context.Context {
	return context.WithValue(contextOrBackground(ctx), apiKeyRequestCacheContextKey{}, &apiKeyRequestCache{})
}

func (r *Repository) findAPIKeyForAccess(ctx context.Context, db *gorm.DB, apiKey string) (APIKeyRecord, error) {
	ctx = contextOrBackground(ctx)
	if errContext := ctx.Err(); errContext != nil {
		return APIKeyRecord{}, errContext
	}
	cache, _ := ctx.Value(apiKeyRequestCacheContextKey{}).(*apiKeyRequestCache)
	if cache != nil {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.repo == r && cache.record.APIKey == apiKey {
			return cache.record, nil
		}
	}

	var record APIKeyRecord
	if errFirst := db.WithContext(ctx).Where("api_key = ?", apiKey).First(&record).Error; errFirst != nil {
		return APIKeyRecord{}, errFirst
	}
	if cache != nil {
		cache.repo = r
		cache.record = record
	}
	return record, nil
}
