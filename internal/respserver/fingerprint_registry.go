package respserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	log "github.com/sirupsen/logrus"
)

var (
	ErrFingerprintFenced           = errors.New("fingerprint is fenced")
	ErrInvalidSubscriptionLifetime = errors.New("invalid subscription lifetime")
)

type fingerprintLifetimeKey struct {
	fingerprint string
	connectedAt time.Time
}

type fingerprintEntry struct {
	mu            sync.Mutex
	lifetime      cluster.ConnectionLifetime
	fenced        bool
	fenceRevision int64
	connections   map[*TrackedConnection]struct{}
	handlers      int
	changed       chan struct{}
}

// FingerprintRegistry tracks exactly one CPA membership lifetime per key.
type FingerprintRegistry struct {
	mu      sync.Mutex
	entries map[fingerprintLifetimeKey]*fingerprintEntry
}

// TrackedConnection owns a cancellable connection lifetime.
type TrackedConnection struct {
	registry        *FingerprintRegistry
	entry           *fingerprintEntry
	lifetime        cluster.ConnectionLifetime
	conn            net.Conn
	ctx             context.Context
	cancel          context.CancelFunc
	handlers        int
	handlersChanged chan struct{}
	once            sync.Once
}

// NewFingerprintRegistry creates an empty fingerprint lifetime registry.
func NewFingerprintRegistry() *FingerprintRegistry {
	return &FingerprintRegistry{entries: make(map[fingerprintLifetimeKey]*fingerprintEntry)}
}

// Accept registers a newly accepted connection with its immutable accept lifetime.
func (r *FingerprintRegistry) Accept(ctx context.Context, conn net.Conn, lifetime cluster.ConnectionLifetime) (*TrackedConnection, error) {
	if r == nil {
		return nil, fmt.Errorf("fingerprint registry is nil")
	}
	if conn == nil {
		return nil, fmt.Errorf("connection is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	tracked := &TrackedConnection{registry: r, lifetime: lifetime, conn: conn, ctx: connectionCtx, cancel: cancel, handlersChanged: make(chan struct{})}

	r.mu.Lock()
	entry := r.entryLocked(lifetimeKey(lifetime))
	entry.mu.Lock()
	if entry.fenced {
		entry.mu.Unlock()
		r.mu.Unlock()
		cancel()
		return nil, ErrFingerprintFenced
	}
	entry.connections[tracked] = struct{}{}
	tracked.entry = entry
	entry.mu.Unlock()
	r.mu.Unlock()
	return tracked, nil
}

// Context returns the connection lifetime context.
func (c *TrackedConnection) Context() context.Context {
	if c == nil || c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// Lifetime returns the lifetime currently associated with this connection.
func (c *TrackedConnection) Lifetime() cluster.ConnectionLifetime {
	if c == nil || c.registry == nil {
		return cluster.ConnectionLifetime{}
	}
	c.registry.mu.Lock()
	defer c.registry.mu.Unlock()
	return c.lifetime
}

// AttachSubscriptionLifetime moves bootstrap tracking to a non-controlled subscription lifetime.
func (c *TrackedConnection) AttachSubscriptionLifetime(lifetime cluster.ConnectionLifetime) error {
	if c == nil || c.registry == nil || lifetime.Controlled || !lifetime.Subscription || lifetime.Fingerprint == "" || lifetime.ConnectedAt.IsZero() {
		return ErrInvalidSubscriptionLifetime
	}
	for {
		c.registry.mu.Lock()
		if c.lifetime.Fingerprint != "" && c.lifetime.Fingerprint != lifetime.Fingerprint {
			c.registry.mu.Unlock()
			return ErrInvalidSubscriptionLifetime
		}
		oldEntry := c.entry
		if oldEntry == nil {
			c.registry.mu.Unlock()
			return net.ErrClosed
		}
		oldEntry.mu.Lock()
		if c.handlers != 0 {
			changed := c.handlersChanged
			oldEntry.mu.Unlock()
			c.registry.mu.Unlock()
			<-changed
			continue
		}
		newEntry := c.registry.entryLocked(lifetimeKey(lifetime))
		if oldEntry == newEntry {
			oldEntry.mu.Unlock()
			c.registry.mu.Unlock()
			return nil
		}
		newEntry.mu.Lock()
		if newEntry.fenced {
			newEntry.mu.Unlock()
			oldEntry.mu.Unlock()
			c.registry.mu.Unlock()
			return ErrFingerprintFenced
		}
		if oldEntry != newEntry {
			delete(oldEntry.connections, c)
			oldEntry.signalChanged()
		}
		newEntry.connections[c] = struct{}{}
		c.entry = newEntry
		c.lifetime = lifetime
		newEntry.mu.Unlock()
		oldEntry.mu.Unlock()
		c.registry.mu.Unlock()
		return nil
	}
}

// BeginHandler prevents a fence acknowledgement until the returned callback runs.
func (c *TrackedConnection) BeginHandler() (func(), error) {
	if c == nil || c.registry == nil {
		return nil, fmt.Errorf("tracked connection is nil")
	}
	c.registry.mu.Lock()
	entry := c.entry
	if entry == nil {
		c.registry.mu.Unlock()
		return nil, net.ErrClosed
	}
	entry.mu.Lock()
	c.registry.mu.Unlock()
	if entry.fenced {
		entry.mu.Unlock()
		return nil, ErrFingerprintFenced
	}
	entry.handlers++
	c.handlers++
	entry.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.registry.mu.Lock()
			entry.mu.Lock()
			entry.handlers--
			c.handlers--
			entry.signalChanged()
			close(c.handlersChanged)
			c.handlersChanged = make(chan struct{})
			entry.mu.Unlock()
			c.registry.mu.Unlock()
		})
	}, nil
}

// Close stops the tracked connection and removes it from its lifetime entry.
func (c *TrackedConnection) Close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	c.once.Do(func() {
		c.cancel()
		if c.conn != nil {
			closeErr = c.conn.Close()
		}
		if c.registry == nil {
			return
		}
		c.registry.mu.Lock()
		entry := c.entry
		if entry != nil {
			entry.mu.Lock()
			delete(entry.connections, c)
			entry.signalChanged()
			entry.mu.Unlock()
		}
		c.registry.mu.Unlock()
	})
	return closeErr
}

// Fence cancels all connections for an exact membership lifetime and waits for all work to drain.
func (r *FingerprintRegistry) Fence(ctx context.Context, lifetime cluster.ConnectionLifetime, revision int64) error {
	return r.fence(ctx, lifetime, revision, nil)
}

// FenceAndAcknowledge keeps the exact lifetime entry fenced while acknowledge persists its drain acknowledgement.
func (r *FingerprintRegistry) FenceAndAcknowledge(ctx context.Context, lifetime cluster.ConnectionLifetime, revision int64, acknowledge func() error) error {
	return r.fence(ctx, lifetime, revision, acknowledge)
}

func (r *FingerprintRegistry) fence(ctx context.Context, lifetime cluster.ConnectionLifetime, revision int64, acknowledge func() error) error {
	if r == nil {
		return fmt.Errorf("fingerprint registry is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	entry := r.entryLocked(lifetimeKey(lifetime))
	entry.mu.Lock()
	entry.fenced = true
	if revision > entry.fenceRevision {
		entry.fenceRevision = revision
	}
	connections := make([]*TrackedConnection, 0, len(entry.connections))
	for connection := range entry.connections {
		connections = append(connections, connection)
	}
	entry.mu.Unlock()
	r.mu.Unlock()

	for _, connection := range connections {
		if errClose := connection.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			log.WithError(errClose).Warn("fingerprint connection close failed")
		}
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	for len(entry.connections) != 0 || entry.handlers != 0 {
		changed := entry.changed
		entry.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		entry.mu.Lock()
	}
	if acknowledge != nil {
		return acknowledge()
	}
	return nil
}

// LatestFenceRevision returns the latest fence revision for an exact lifetime.
func (r *FingerprintRegistry) LatestFenceRevision(lifetime cluster.ConnectionLifetime) int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	entry := r.entries[lifetimeKey(lifetime)]
	r.mu.Unlock()
	if entry == nil {
		return 0
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.fenceRevision
}

func (r *FingerprintRegistry) entryLocked(key fingerprintLifetimeKey) *fingerprintEntry {
	if r.entries == nil {
		r.entries = make(map[fingerprintLifetimeKey]*fingerprintEntry)
	}
	entry := r.entries[key]
	if entry == nil {
		entry = &fingerprintEntry{
			lifetime:    cluster.ConnectionLifetime{Fingerprint: key.fingerprint, ConnectedAt: key.connectedAt},
			connections: make(map[*TrackedConnection]struct{}),
			changed:     make(chan struct{}),
		}
		r.entries[key] = entry
	}
	return entry
}

func (entry *fingerprintEntry) signalChanged() {
	close(entry.changed)
	entry.changed = make(chan struct{})
}

func lifetimeKey(lifetime cluster.ConnectionLifetime) fingerprintLifetimeKey {
	return fingerprintLifetimeKey{fingerprint: lifetime.Fingerprint, connectedAt: lifetime.ConnectedAt.UTC()}
}
