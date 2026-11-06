package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestMarkResultRequestScopedDoesNotPersistOrAdoptSharedState(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const authID = "request-scoped-cluster-auth"
			const model = "gpt-6-sol"
			local := &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive, StateVersion: 1}
			persisted := local.Clone()
			persisted.StateVersion = 2
			persisted.Status = StatusError
			persisted.Unavailable = true
			persisted.NextRetryAfter = time.Now().Add(time.Hour)
			persisted.ModelStates = map[string]*ModelState{
				model: {Status: StatusError, Unavailable: true, NextRetryAfter: persisted.NextRetryAfter},
			}
			store := &fakeMutatorStore{persisted: persisted.Clone()}
			manager := NewManager(store, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), local); errRegister != nil {
				t.Fatal(errRegister)
			}
			before, _ := manager.GetByID(authID)
			// Keep queued writes observable without racing a background worker.
			manager.resultPersistActive = map[string]struct{}{authID: {}}
			manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: "codex", Model: model, Error: &Error{Code: ErrorCodeRequestScoped, HTTPStatus: status, Message: "request-scoped failure"}})
			after, _ := manager.GetByID(authID)
			if !reflect.DeepEqual(availabilityFingerprint(before, model), availabilityFingerprint(after, model)) || before.StateVersion != after.StateVersion {
				t.Errorf("request fault adopted shared availability: before=%#v after=%#v", before, after)
			}
			if after.Failed != before.Failed+1 {
				t.Error("request failure was not counted")
			}
			if store.mutationAttemptCount() != 0 || store.saveCount() != 0 || len(manager.resultPersistPending) != 0 {
				t.Errorf("request fault reached persistence: mutate=%d save=%d queued=%d", store.mutationAttemptCount(), store.saveCount(), len(manager.resultPersistPending))
			}
			if !reflect.DeepEqual(store.persistedSnapshot(), persisted) {
				t.Error("request fault changed shared credential state")
			}
		})
	}
}
