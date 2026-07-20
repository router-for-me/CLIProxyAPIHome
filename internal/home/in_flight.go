package home

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultInFlightLeaseTTL        = 30 * time.Minute
	minimumInFlightLeaseTTL        = time.Minute
	inFlightLeaseRetention         = 10 * time.Minute
	inFlightLeaseCleanupInterval   = time.Minute
	inFlightLeaseCleanupBatchSize  = 10000
	inFlightLeaseCleanupMaxBatches = 10
)

type InFlightReserveInput struct {
	DispatchID     string
	RequestID      string
	CredentialID   string
	Provider       string
	RequestedModel string
	Model          string
	CPANodeID      string
	CPAIP          string
	CPALabel       string
	ForceMapping   bool
	OriginalAlias  string
	TTL            time.Duration
}

type InFlightLease struct {
	ID             uint
	LeaseID        string
	DispatchID     string
	RequestID      string
	CredentialID   string
	Provider       string
	RequestedModel string
	Model          string
	CPANodeID      string
	CPAIP          string
	CPALabel       string
	ForceMapping   bool
	OriginalAlias  string
	StartedAt      time.Time
	LastRenewedAt  time.Time
	ExpiresAt      time.Time
	Reused         bool
}

type InFlightModelSummary struct {
	Model       string
	InFlight    int64
	MaxInFlight *int
	Remaining   *int64
	Saturated   bool
}

type InFlightCredentialSummary struct {
	CredentialID        string
	InFlight            int64
	MaxInFlight         *int
	Remaining           *int64
	TotalSaturated      bool
	SaturatedModelCount int
	Models              []InFlightModelSummary
}

type InFlightCredentialDetail struct {
	Summary    InFlightCredentialSummary
	Requests   []InFlightLease
	NextCursor string
	ObservedAt time.Time
}

type InFlightLeasePage struct {
	Requests   []InFlightLease
	NextCursor string
	ObservedAt time.Time
}

type ConcurrencyExceededError struct {
	Scope        string
	CredentialID string
	Model        string
	Current      int64
	Limit        int
}

func (e *ConcurrencyExceededError) Error() string {
	if e == nil {
		return "credential concurrency exceeded"
	}
	if strings.TrimSpace(e.Scope) == "mixed" {
		return "credential concurrency exceeded across available credentials"
	}
	if strings.TrimSpace(e.Scope) == "model" {
		return fmt.Sprintf("credential model concurrency exceeded: %s (%d/%d)", strings.TrimSpace(e.Model), e.Current, e.Limit)
	}
	return fmt.Sprintf("credential concurrency exceeded (%d/%d)", e.Current, e.Limit)
}

type DispatchReplayError struct {
	DispatchID string
}

func (e *DispatchReplayError) Error() string {
	return "dispatch id cannot be replayed"
}

type inFlightStore interface {
	ReserveInFlightLease(ctx context.Context, input InFlightReserveInput) (*InFlightLease, error)
	RenewInFlightLease(ctx context.Context, leaseID string, nodeID string, ttl time.Duration) (bool, error)
	ReleaseInFlightLease(ctx context.Context, leaseID string, nodeID string, reason string) (bool, error)
	PurgeInFlightLeases(ctx context.Context, now time.Time, retention time.Duration, limit int) (int64, error)
}

func (r *Runtime) inFlightStore() inFlightStore {
	if r == nil || r.clusterAdapter == nil {
		return nil
	}
	store, _ := r.clusterAdapter.(inFlightStore)
	return store
}

func (r *Runtime) inFlightLeaseTTL() time.Duration {
	if r == nil {
		return defaultInFlightLeaseTTL
	}
	cfg := r.Config()
	if cfg == nil {
		return defaultInFlightLeaseTTL
	}
	raw := strings.TrimSpace(cfg.InFlightLeaseTTL)
	if raw == "" {
		return defaultInFlightLeaseTTL
	}
	ttl, errParse := time.ParseDuration(raw)
	if errParse != nil || ttl < minimumInFlightLeaseTTL {
		return defaultInFlightLeaseTTL
	}
	return ttl
}

func (r *Runtime) ReserveInFlightLease(ctx context.Context, input InFlightReserveInput) (*InFlightLease, error) {
	store := r.inFlightStore()
	if store == nil {
		return nil, fmt.Errorf("home runtime: in-flight store unavailable")
	}
	input.TTL = r.inFlightLeaseTTL()
	return store.ReserveInFlightLease(ctx, input)
}

func (r *Runtime) RenewInFlightLease(ctx context.Context, leaseID string, nodeID string) (bool, error) {
	store := r.inFlightStore()
	if store == nil {
		return false, fmt.Errorf("home runtime: in-flight store unavailable")
	}
	return store.RenewInFlightLease(ctx, strings.TrimSpace(leaseID), strings.TrimSpace(nodeID), r.inFlightLeaseTTL())
}

func (r *Runtime) ReleaseInFlightLease(ctx context.Context, leaseID string, nodeID string, reason string) (bool, error) {
	store := r.inFlightStore()
	if store == nil {
		return false, fmt.Errorf("home runtime: in-flight store unavailable")
	}
	return store.ReleaseInFlightLease(ctx, strings.TrimSpace(leaseID), strings.TrimSpace(nodeID), strings.TrimSpace(reason))
}

func (r *Runtime) startInFlightCleanupLoop(ctx context.Context) {
	store := r.inFlightStore()
	if store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		ticker := time.NewTicker(inFlightLeaseCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				for batch := 0; batch < inFlightLeaseCleanupMaxBatches; batch++ {
					deleted, errPurge := store.PurgeInFlightLeases(ctx, now.UTC(), inFlightLeaseRetention, inFlightLeaseCleanupBatchSize)
					if errPurge != nil {
						log.WithError(errPurge).Warn("home in-flight: failed to purge expired leases")
						break
					}
					if deleted < inFlightLeaseCleanupBatchSize {
						break
					}
				}
			}
		}
	}()
}
