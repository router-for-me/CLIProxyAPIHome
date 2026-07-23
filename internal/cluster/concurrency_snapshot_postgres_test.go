package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestPostgresConcurrencyReadsUseOneRepeatableReadSnapshot(t *testing.T) {
	t.Run("state includes old policy models and counters", func(t *testing.T) {
		repo := newPostgresQuiescenceRepository(t)
		ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCtx()
		seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
		seedConcurrencyAdmissionAuth(t, repo, "cred-1")
		seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(3), map[string]int64{"old": 3})
		preparePostgresConcurrencyPolicyPatch(t, ctx, repo, "home-a")
		lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
		if _, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "old", Lifetime: lifetime, ProtocolVersion: 1}); errAdmit != nil {
			t.Fatal(errAdmit)
		}

		assertRead := blockPostgresPolicyModelReadForMutation(t, ctx, repo, func() error {
			admission, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "new", Lifetime: lifetime, ProtocolVersion: 1})
			if errAdmit != nil {
				return errAdmit
			}
			if !admission.Accounted {
				return gorm.ErrRecordNotFound
			}
			if errRelease := repo.ApplyConcurrencyRelease(ctx, ConcurrencyReleaseRequest{CredentialID: "cred-1", Model: "old", ReleaseSeq: 1, Lifetime: lifetime}); errRelease != nil {
				return errRelease
			}
			if _, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", ConcurrencyPolicyPatch{
				MaxInFlight:        OptionalLimit{Set: true, Value: 4},
				MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{"new": int64Pointer(4)}},
			}, nil); errPatch != nil {
				return errPatch
			}
			return nil
		})
		states, errRead := repo.ReadConcurrencyState(ctx)
		if errRead != nil {
			t.Fatal(errRead)
		}
		assertRead()
		if len(states) != 1 || states[0].MaxInFlight == nil || *states[0].MaxInFlight != 3 || states[0].AdmittedInFlight != 1 {
			t.Fatalf("states = %#v, want old snapshot", states)
		}
		if len(states[0].Models) != 1 || states[0].Models[0].Model != "old" || states[0].Models[0].MaxInFlight != 3 || states[0].Models[0].AdmittedInFlight != 1 {
			t.Fatalf("models = %#v, want old snapshot", states[0].Models)
		}
	})

	for _, test := range []struct {
		name string
		read func(*Repository, context.Context) (CredentialConcurrencyPolicy, error)
	}{
		{
			name: "list includes one policy version",
			read: func(repo *Repository, ctx context.Context) (CredentialConcurrencyPolicy, error) {
				policies, errPolicies := repo.ListCredentialConcurrencyPolicies(ctx)
				if errPolicies != nil {
					return CredentialConcurrencyPolicy{}, errPolicies
				}
				if len(policies) != 1 {
					return CredentialConcurrencyPolicy{}, gorm.ErrRecordNotFound
				}
				return policies[0], nil
			},
		},
		{
			name: "export includes one policy version",
			read: func(repo *Repository, ctx context.Context) (CredentialConcurrencyPolicy, error) {
				root := map[string]any{}
				if errExport := repo.ExportCredentialConcurrencyPolicies(ctx, root); errExport != nil {
					return CredentialConcurrencyPolicy{}, errExport
				}
				policies, okPolicies := root[credentialConcurrencyPoliciesRootKey].(map[string]CredentialConcurrencyExchangePolicy)
				if !okPolicies {
					return CredentialConcurrencyPolicy{}, gorm.ErrRecordNotFound
				}
				policy, okPolicy := policies["cred-1"]
				if !okPolicy {
					return CredentialConcurrencyPolicy{}, gorm.ErrRecordNotFound
				}
				return CredentialConcurrencyPolicy{CredentialID: "cred-1", MaxInFlight: policy.MaxInFlight, MaxInFlightByModel: policy.MaxInFlightByModel}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCtx()
			seedConcurrencyAdmissionAuth(t, repo, "cred-1")
			seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(2), map[string]int64{"old": 2})
			assertRead := blockPostgresPolicyModelReadForMutation(t, ctx, repo, func() error {
				_, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", ConcurrencyPolicyPatch{
					MaxInFlight:        OptionalLimit{Set: true, Value: 4},
					MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{"new": int64Pointer(4)}},
				}, nil)
				return errPatch
			})
			policy, errRead := test.read(repo, ctx)
			if errRead != nil {
				t.Fatal(errRead)
			}
			assertRead()
			if policy.MaxInFlight == nil || *policy.MaxInFlight != 2 || len(policy.MaxInFlightByModel) != 1 || policy.MaxInFlightByModel["old"] != 2 {
				t.Fatalf("policy = %#v, want old snapshot", policy)
			}
		})
	}
}

func blockPostgresPolicyModelReadForMutation(t *testing.T, ctx context.Context, repo *Repository, mutate func() error) func() {
	t.Helper()
	started := make(chan struct{})
	done := make(chan error, 1)
	var fired atomic.Bool
	const callbackName = "test:concurrency-policy-model-snapshot"
	if errRegister := repo.db.Callback().Query().Before("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if db.Statement.Schema == nil || db.Statement.Schema.Table != (CredentialConcurrencyModelPolicyRecord{}).TableName() || !fired.CompareAndSwap(false, true) {
			return
		}
		close(started)
		select {
		case errMutation := <-done:
			if errMutation != nil {
				db.AddError(errMutation)
			}
		case <-ctx.Done():
			db.AddError(ctx.Err())
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	t.Cleanup(func() {
		if errRemove := repo.db.Callback().Query().Remove(callbackName); errRemove != nil {
			t.Errorf("remove policy model read barrier: %v", errRemove)
		}
	})
	go func() {
		select {
		case <-started:
			done <- mutate()
		case <-ctx.Done():
			done <- ctx.Err()
		}
	}()
	return func() {
		t.Helper()
		if !fired.Load() {
			t.Fatal("policy model read barrier did not run")
		}
	}
}
