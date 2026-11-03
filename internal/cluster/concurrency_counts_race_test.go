package cluster

import (
	"context"
	"testing"
	"time"
)

func TestConcurrencyCountsAdmissionReleasePolicyRace(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *Repository
	}{
		{"sqlite", newCredentialFoundationTestRepository},
		{"postgres", newPostgresQuiescenceRepository},
	} {
		t.Run(backend.name, func(t *testing.T) {
			for _, limits := range []struct {
				name    string
				initial int64
				patched int64
			}{
				{"lower", 2, 1},
				{"raise", 1, 2},
			} {
				t.Run(limits.name, func(t *testing.T) {
					repo := backend.open(t)
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
					seedConcurrencyAdmissionAuth(t, repo, "cred-1")
					seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(limits.initial), nil)
					preparePostgresConcurrencyPolicyPatch(t, ctx, repo, "home-a")
					seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 1, 0)
					lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
					start := make(chan struct{})
					admitted := make(chan bool, 1)
					done := make(chan error, 3)
					go func() {
						<-start
						result, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 1, Lifetime: lifetime})
						admitted <- result.Accounted
						if IsConcurrencySaturated(errAdmit) {
							errAdmit = nil
						}
						done <- errAdmit
					}()
					go func() {
						<-start
						done <- repo.ApplyConcurrencyRelease(ctx, ConcurrencyReleaseRequest{CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 1, Lifetime: lifetime})
					}()
					go func() {
						<-start
						_, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: limits.patched}}, nil)
						done <- errPatch
					}()
					close(start)
					for range 3 {
						select {
						case errOperation := <-done:
							if errOperation != nil {
								t.Fatal(errOperation)
							}
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
					}
					want := int64(0)
					if <-admitted {
						want = 1
					}
					counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
					if counter.ActiveCount != want || counter.LastReleaseSeq != 1 {
						t.Fatalf("counter=%#v, want active=%d release_seq=1", counter, want)
					}
					policy, errPolicy := repo.GetCredentialConcurrencyPolicy(ctx, "cred-1")
					if errPolicy != nil || policy.MaxInFlight == nil || *policy.MaxInFlight != limits.patched {
						t.Fatalf("policy=(%#v,%v)", policy, errPolicy)
					}
					states, errState := repo.ReadConcurrencyState(ctx)
					if errState != nil || len(states) != 1 || states[0].AdmittedInFlight != want {
						t.Fatalf("state=(%#v,%v), want admitted=%d", states, errState, want)
					}
				})
			}
		})
	}
}

func TestConcurrencyUnlimitedSkipsCounts(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *Repository
	}{
		{"sqlite", newCredentialFoundationTestRepository},
		{"postgres", newPostgresQuiescenceRepository},
	} {
		t.Run(backend.name, func(t *testing.T) {
			for _, policy := range []string{"absent", "disabled", "other model limited"} {
				t.Run(policy, func(t *testing.T) {
					repo := backend.open(t)
					seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
					seedConcurrencyAdmissionAuth(t, repo, "cred-1")
					if policy != "absent" {
						models := map[string]int64{}
						if policy == "other model limited" {
							models["claude"] = 1
						}
						seedConcurrencyAdmissionPolicy(t, repo, "cred-1", nil, models)
					}
					// Unlimited requests must not start inspecting or mutating existing counters.
					seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", -1, 7)
					request := ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 0, Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")}
					result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request)
					if errAdmit != nil || result.Accounted {
						t.Fatalf("admission=(%#v,%v)", result, errAdmit)
					}
					counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
					if counter.ActiveCount != -1 || counter.LastReleaseSeq != 7 {
						t.Fatalf("counter mutated: %#v", counter)
					}
				})
			}
		})
	}
}
