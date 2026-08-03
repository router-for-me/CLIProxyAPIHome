package auth

import "context"

// Store abstracts persistence of Auth state across restarts.
type Store interface {
	// List returns all auth records stored in the backend.
	List(ctx context.Context) ([]*Auth, error)
	// Save persists the provided auth record, replacing any existing one with same ID.
	Save(ctx context.Context, auth *Auth) (string, error)
	// Delete removes the auth record identified by id.
	Delete(ctx context.Context, id string) error
}

// StateVersionSaver is an optional Store capability that returns the revision
// accepted for a save. A zero revision means the snapshot was ignored as stale.
type StateVersionSaver interface {
	SaveWithStateVersion(ctx context.Context, auth *Auth) (id string, stateVersion int64, err error)
}

// StateMutator is an optional Store capability. Implementations load the
// authoritative persisted copy of the auth (typically under a database row
// lock), apply mutate to it, and persist the result atomically when mutate
// reports a change. The returned auth reflects the post-mutation persisted
// state whether or not anything changed. Backends that implement this
// interface let availability transitions such as quota backoff escalation
// stay atomic across multiple Home nodes sharing one database.
type StateMutator interface {
	MutateAuthState(ctx context.Context, id string, mutate func(auth *Auth) bool) (*Auth, error)
}
