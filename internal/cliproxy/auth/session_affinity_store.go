package auth

import (
	"context"
	"errors"
	"time"
)

// ErrSessionAffinityConflict asks dispatch to re-read a concurrently changed binding.
var ErrSessionAffinityConflict = errors.New("session affinity binding changed")

// SessionBinding is an authoritative snapshot, including expired bindings.
// Revisions are opaque and must never be reused after deletion or expiration.
type SessionBinding struct {
	AuthID    string
	Revision  string
	ExpiresAt time.Time
}

// SessionAffinityCommit conditionally renews or replaces a binding.
type SessionAffinityCommit struct {
	Key              string
	ExpectedRevision string
	// ExpiredAt guards a cold selection based on an expired snapshot against
	// a concurrent renewal that keeps the same credential revision.
	ExpiredAt time.Time
	AuthID    string
	TTL       time.Duration
	// SessionKey and Provider record a successful selection for cross-model
	// Guardian inheritance. SessionKey hashes the session without its model or provider.
	// Empty metadata keeps ordinary affinity without recording Guardian history.
	SessionKey string
	Provider   string
}

// SessionAffinityStore provides authoritative reads and atomic conditional writes.
// A missing binding has an empty revision. Expired bindings retain their revision
// until deleted so stale writers cannot overwrite a newer binding.
type SessionAffinityStore interface {
	LoadSessionBinding(context.Context, string) (SessionBinding, error)
	// LoadLatestSessionBinding returns the latest live selection across models
	// on the supplied real credential providers, without renewing its lease.
	LoadLatestSessionBinding(context.Context, string, []string) (SessionBinding, error)
	CompareAndSwapSessionBinding(context.Context, SessionAffinityCommit) (bool, error)
}

// SessionAffinitySelection defers binding changes until dispatch is prepared and
// admitted. Each dispatch attempt owns its selection; selectors must not share it.
type SessionAffinitySelection struct {
	Store       SessionAffinityStore
	Commit      *SessionAffinityCommit
	reservation *weightedReservation
}

// Reset releases any pending reservation before clearing selection metadata.
// It is safe to call repeatedly, including with a nil selection.
func (s *SessionAffinitySelection) Reset() {
	if s == nil {
		return
	}
	s.Finish(false)
	*s = SessionAffinitySelection{}
}

// Finish confirms weighted credit after successful binding/admission, or releases
// it on failure/retry. Dispatch must finish each selection before discarding it.
// It is safe to call repeatedly, including with a nil selection.
func (s *SessionAffinitySelection) Finish(committed bool) {
	if s != nil {
		s.reservation.finish(committed)
		s.reservation = nil
	}
}
