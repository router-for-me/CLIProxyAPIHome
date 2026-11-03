package cluster

import (
	"errors"
	"math"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestIncrementConcurrencyCounterPreservesBaselineAndRejectsOverflow(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *Repository
	}{
		{"sqlite", newCredentialFoundationTestRepository},
		{"postgres", newPostgresQuiescenceRepository},
	} {
		t.Run(backend.name, func(t *testing.T) {
			repo := backend.open(t)
			for _, test := range []struct {
				name    string
				active  int64
				invalid bool
			}{
				{"negative", -1, true},
				{"overflow", math.MaxInt64, true},
				{"zero", 0, false},
				{"last valid increment", math.MaxInt64 - 1, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					seedReleaseCounter(t, repo, test.name, "gpt", "fp-a", test.active, 7)
					before := loadReleaseCounter(t, repo, test.name, "gpt", "fp-a")
					errIncrement := withConcurrencyTransaction(t.Context(), repo.db, func(tx *gorm.DB) error {
						return incrementConcurrencyCounterTx(t.Context(), tx, test.name, "gpt", "fp-a")
					})
					after := loadReleaseCounter(t, repo, test.name, "gpt", "fp-a")
					if test.invalid {
						if !errors.Is(errIncrement, ErrConcurrencyCounterInvalid) || after != before {
							t.Fatalf("increment error=%v counter=%#v, want unchanged %#v", errIncrement, after, before)
						}
					} else if errIncrement != nil || after.ActiveCount != test.active+1 || after.LastReleaseSeq != 7 {
						t.Fatalf("increment error=%v counter=%#v", errIncrement, after)
					}
				})
			}
		})
	}
}

func TestConcurrencyCounterWritesUseDatabaseTime(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *Repository
	}{
		{"sqlite", newCredentialFoundationTestRepository},
		{"postgres", newPostgresQuiescenceRepository},
	} {
		t.Run(backend.name, func(t *testing.T) {
			repo := backend.open(t)
			seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			seedConcurrencyAdmissionAuth(t, repo, "cred-1")
			seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(10), nil)
			lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			repo.db = repo.db.Session(&gorm.Session{NowFunc: func() time.Time { return databaseTestTime.Add(100 * 365 * 24 * time.Hour) }})
			for _, phase := range []string{"insert", "increment", "release"} {
				t.Run(phase, func(t *testing.T) {
					before, errBefore := DatabaseNow(t.Context(), repo.db)
					if errBefore != nil {
						t.Fatal(errBefore)
					}
					if phase == "release" {
						if errRelease := repo.ApplyConcurrencyRelease(t.Context(), ConcurrencyReleaseRequest{CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 1, Lifetime: lifetime}); errRelease != nil {
							t.Fatal(errRelease)
						}
					} else {
						result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 1, Lifetime: lifetime})
						if errAdmit != nil || !result.Accounted {
							t.Fatalf("admission=(%#v,%v)", result, errAdmit)
						}
					}
					after, errAfter := DatabaseNow(t.Context(), repo.db)
					if errAfter != nil {
						t.Fatal(errAfter)
					}
					counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
					if counter.UpdatedAt.Before(before) || counter.UpdatedAt.After(after) {
						t.Fatalf("updated_at=%s, want database time in [%s,%s]", counter.UpdatedAt, before, after)
					}
				})
			}
		})
	}
}
