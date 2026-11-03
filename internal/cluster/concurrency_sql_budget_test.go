package cluster

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestReadConcurrencyStateSQLBudgets(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *Repository
	}{
		{"sqlite", newCredentialFoundationTestRepository},
		{"postgres", newPostgresQuiescenceRepository},
	} {
		t.Run(backend.name, func(t *testing.T) {
			for _, credentialCount := range []int{0, 1, 100} {
				t.Run(fmt.Sprintf("%d credentials", credentialCount), func(t *testing.T) {
					repo := backend.open(t)
					for index := credentialCount - 1; index >= 0; index-- {
						credentialID := fmt.Sprintf("cred-%03d", index)
						seedConcurrencyAdmissionPolicy(t, repo, credentialID, int64Pointer(10), map[string]int64{
							"zeta": 4, "gpt": 3, "claude": 2, "absent": 1,
						})
						for _, model := range []string{"zeta", "gpt", "claude", "unconfigured"} {
							for _, fingerprint := range []string{"fp-a", "fp-b"} {
								seedReleaseCounter(t, repo, credentialID, model, fingerprint, 1, 0)
							}
						}
					}
					counter := &lifecycleSQLCounter{Interface: logger.Default.LogMode(logger.Silent)}
					repo.db = repo.db.Session(&gorm.Session{Logger: counter})
					states, errRead := repo.ReadConcurrencyState(t.Context())
					if errRead != nil || len(states) != credentialCount {
						t.Fatalf("states=(%#v,%v)", states, errRead)
					}
					wantSQL := int64(3)
					if credentialCount == 0 {
						wantSQL = 1
					}
					if count := counter.statements.Load(); count != wantSQL {
						t.Fatalf("state SQL statements=%d, want %d", count, wantSQL)
					}
					for index, state := range states {
						if state.CredentialID != fmt.Sprintf("cred-%03d", index) || state.AdmittedInFlight != 8 || len(state.Models) != 4 {
							t.Fatalf("state=%#v", state)
						}
						for modelIndex, model := range []string{"absent", "claude", "gpt", "zeta"} {
							wantCount := int64(2)
							if model == "absent" {
								wantCount = 0
							}
							want := CredentialConcurrencyModelState{Model: model, MaxInFlight: int64(modelIndex + 1), AdmittedInFlight: wantCount}
							if state.Models[modelIndex] != want {
								t.Fatalf("model=%#v, want %#v", state.Models[modelIndex], want)
							}
						}
					}
					t.Logf("%d credentials with 4 model policies each: %d business SQL statements", credentialCount, wantSQL)
				})
			}
		})
	}
}

func TestConcurrencySQLBudgets(t *testing.T) {
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
			seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(10), map[string]int64{"gpt": 10})
			request := ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 1, Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")}
			counter := &lifecycleSQLCounter{Interface: logger.Default.LogMode(logger.Silent)}
			repo.db = repo.db.Session(&gorm.Session{Logger: counter})
			if total, model, errCounts := sumConcurrencyCountsTx(t.Context(), repo.db, "cred-1", "gpt"); errCounts != nil || total != 0 || model != 0 {
				t.Fatalf("sum=(%d,%d,%v)", total, model, errCounts)
			}
			if count := counter.statements.Load(); count != 1 {
				t.Fatalf("aggregate SQL statements=%d, want 1", count)
			}
			for _, phase := range []string{"create counter", "update counter"} {
				counter.statements.Store(0)
				result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request)
				if errAdmit != nil || !result.Accounted {
					t.Fatalf("%s admission=(%#v,%v)", phase, result, errAdmit)
				}
				// GORM traces business SQL, not database/sql Begin/Commit calls.
				if count := counter.statements.Load(); count != 6 {
					t.Fatalf("%s SQL statements=%d, want 6", phase, count)
				}
			}
			release := ConcurrencyReleaseRequest{CredentialID: request.CredentialID, Model: request.Model, ReleaseSeq: 1, Lifetime: request.Lifetime}
			for _, phase := range []struct {
				name string
				want int64
			}{
				{"apply release", 5},
				{"duplicate release", 4},
			} {
				counter.statements.Store(0)
				if errRelease := repo.ApplyConcurrencyRelease(t.Context(), release); errRelease != nil {
					t.Fatalf("%s: %v", phase.name, errRelease)
				}
				if count := counter.statements.Load(); count != phase.want {
					t.Fatalf("%s SQL statements=%d, want %d", phase.name, count, phase.want)
				}
			}
			t.Log("aggregate: 1 SQL statement; limited admission: 6; release: 5; duplicate release: 4")
		})
	}
}
