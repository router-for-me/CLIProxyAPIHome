package auth

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionEntry stores auth binding with expiration.
type sessionEntry struct {
	authID    string
	revision  string
	expiresAt time.Time
}

const defaultMaxSessionCacheEntries = 65536

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu       sync.RWMutex
	entries  map[string]sessionEntry
	ttl      time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl < time.Second {
		ttl = 30 * time.Minute
	}
	c := &SessionCache{
		entries: make(map[string]sessionEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		if cur, exists := c.entries[sessionID]; exists && time.Now().After(cur.expiresAt) {
			delete(c.entries, sessionID)
		}
		c.mu.Unlock()
		return "", false
	}
	return entry.authID, true
}

// GetAndRefresh retrieves the auth ID bound to a session and refreshes TTL on hit.
// This extends the binding lifetime for active sessions.
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	// Refresh TTL on successful access
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
	return entry.authID, true
}

// Set binds a session to an auth ID with TTL refresh.
func (c *SessionCache) Set(sessionID, authID string) {
	if sessionID == "" || authID == "" {
		return
	}
	c.mu.Lock()
	c.pruneLocked()
	c.entries[sessionID] = sessionEntry{
		authID:    authID,
		revision:  uuid.NewString(),
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// pruneLocked bounds the local cache. The caller must hold c.mu.
func (c *SessionCache) pruneLocked() {
	if len(c.entries) >= defaultMaxSessionCacheEntries {
		now := time.Now()
		pruned := 0
		for sid, entry := range c.entries {
			if now.After(entry.expiresAt) {
				delete(c.entries, sid)
				pruned++
			}
		}
		if len(c.entries) >= defaultMaxSessionCacheEntries {
			for sid := range c.entries {
				delete(c.entries, sid)
				pruned++
				if pruned >= defaultMaxSessionCacheEntries/8 {
					break
				}
			}
		}
	}
}

// LoadSessionBinding implements the atomic binding store for standalone selectors.
func (c *SessionCache) LoadSessionBinding(ctx context.Context, key string) (SessionBinding, error) {
	if ctx != nil && ctx.Err() != nil {
		return SessionBinding{}, ctx.Err()
	}
	c.mu.RLock()
	entry := c.entries[key]
	c.mu.RUnlock()
	return SessionBinding{AuthID: entry.authID, Revision: entry.revision, ExpiresAt: entry.expiresAt}, nil
}

// CompareAndSwapSessionBinding prevents concurrent first requests from returning
// independently selected credentials, including after a binding has been evicted.
func (c *SessionCache) CompareAndSwapSessionBinding(ctx context.Context, commit SessionAffinityCommit) (bool, error) {
	if ctx != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[commit.Key].revision != commit.ExpectedRevision {
		return false, nil
	}
	current := c.entries[commit.Key]
	if !commit.ExpiredAt.IsZero() && !current.expiresAt.Equal(commit.ExpiredAt) {
		return false, nil
	}
	revision := current.revision
	now := time.Now()
	if revision == "" || current.authID != commit.AuthID || !now.Before(current.expiresAt) {
		revision = uuid.NewString()
	}
	c.pruneLocked()
	c.entries[commit.Key] = sessionEntry{
		authID: commit.AuthID, revision: revision, expiresAt: now.Add(commit.TTL),
	}
	return true, nil
}

// Invalidate removes a specific session binding.
func (c *SessionCache) Invalidate(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, sessionID)
	c.mu.Unlock()
}

// InvalidateAuth removes all sessions bound to a specific auth ID.
// Used when an auth becomes unavailable.
func (c *SessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	c.mu.Lock()
	for sid, entry := range c.entries {
		if entry.authID == authID {
			delete(c.entries, sid)
		}
	}
	c.mu.Unlock()
}

// Stop terminates the background cleanup goroutine safely and idempotently.
func (c *SessionCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// cleanupLoop removes expired or stale entries.
func (c *SessionCache) cleanupLoop() {
	interval := c.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

// cleanup removes expired or stale entries.
func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	for sid, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sid)
		}
	}
	c.mu.Unlock()
}
