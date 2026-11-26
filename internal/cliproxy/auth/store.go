package auth

import (
	"context"
	"errors"
)

// ErrRuntimeProjectionPersist prevents scheduling projections from replacing
// complete credentials in any full-record persistence path.
var ErrRuntimeProjectionPersist = errors.New("auth: cannot persist an incomplete runtime projection")

// ErrAuthNotFound reports an authoritative credential lookup or mutation whose
// target no longer exists. It is distinct from an unloaded local credential.
var ErrAuthNotFound = errors.New("auth: credential not found")

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

// StateMutation distinguishes shared state changes from observation-only writes.
type StateMutation uint8

const (
	StateMutationUnchanged StateMutation = iota
	// StateMutationChanged persists the auth and notifies peers to reload it.
	StateMutationChanged
	// StateMutationObservation persists a replay fence without notifying peers.
	// The storage revision must still advance to reject older snapshot saves.
	StateMutationObservation
)

// StateMutator applies mutate to the authoritative auth under a row lock.
// Changed and observation-only mutations are persisted atomically; only changed
// mutations publish an auth event. The returned auth includes the storage revision.
type StateMutator interface {
	MutateAuthState(ctx context.Context, id string, mutate func(auth *Auth) StateMutation) (*Auth, error)
}
