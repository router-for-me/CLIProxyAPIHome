package cluster

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"gorm.io/gorm"
)

// Run the same behavioral contract against both supported database engines.
func TestConcurrencyCountsContract(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) *Repository
	}{
		{"sqlite", newCredentialFoundationTestRepository},
		{"postgres", newPostgresQuiescenceRepository},
	} {
		t.Run(backend.name, func(t *testing.T) {
			for _, test := range []struct {
				name    string
				counts  []CredentialConcurrencyCounterRecord
				model   string
				total   int64
				matched int64
				invalid bool
			}{
				{name: "no rows", model: "gpt"},
				{name: "multiple nodes models and credentials", model: "gpt", total: 10, matched: 5, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: 2},
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-b", ActiveCount: 3},
					{CredentialID: "cred-1", Model: "claude", CertificateFingerprint: "fp-a", ActiveCount: 5},
					{CredentialID: "other", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: math.MaxInt64},
					{CredentialID: "other", Model: "claude", CertificateFingerprint: "fp-b", ActiveCount: -1},
				}},
				{name: "absent model", model: "absent", total: 2, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: 2},
				}},
				{name: "empty model used by state reader", total: 2, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: 2},
				}},
				{name: "negative masked by positive sum", model: "gpt", invalid: true, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: 5},
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-b", ActiveCount: -1},
				}},
				{name: "negative in another model", model: "gpt", invalid: true, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: 5},
					{CredentialID: "cred-1", Model: "claude", CertificateFingerprint: "fp-b", ActiveCount: -1},
				}},
				{name: "sum overflow", model: "gpt", invalid: true, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: math.MaxInt64},
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-b", ActiveCount: 1},
				}},
				{name: "total overflow across models", model: "gpt", invalid: true, counts: []CredentialConcurrencyCounterRecord{
					{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: math.MaxInt64},
					{CredentialID: "cred-1", Model: "claude", CertificateFingerprint: "fp-b", ActiveCount: 1},
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					repo := backend.open(t)
					seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
					seedConcurrencyAdmissionAuth(t, repo, "cred-1")
					seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(math.MaxInt64), map[string]int64{"gpt": math.MaxInt64, "absent": 2})
					for index := range test.counts {
						test.counts[index].UpdatedAt = databaseTestTime
						if errCreate := repo.db.Create(&test.counts[index]).Error; errCreate != nil {
							t.Fatal(errCreate)
						}
					}
					before := readConcurrencyTestCounters(t, repo)
					var total, matched int64
					errSum := repo.db.Transaction(func(tx *gorm.DB) error {
						var errCounts error
						total, matched, errCounts = sumConcurrencyCountsTx(t.Context(), tx, "cred-1", test.model)
						return errCounts
					})
					states, errState := repo.ReadConcurrencyState(t.Context())
					if test.invalid {
						if errSum == nil || total != 0 || matched != 0 || errState == nil || states != nil {
							t.Fatalf("sum=(%d,%d,%v), state=(%#v,%v), want fail closed", total, matched, errSum, states, errState)
						}
						for _, counter := range test.counts {
							if counter.ActiveCount < 0 && (!errors.Is(errSum, ErrConcurrencyCounterInvalid) || !errors.Is(errState, ErrConcurrencyCounterInvalid)) {
								t.Fatalf("negative counter errors=(%v,%v), want ErrConcurrencyCounterInvalid", errSum, errState)
							}
						}
					} else {
						if errSum != nil || total != test.total || matched != test.matched {
							t.Fatalf("sum=(%d,%d,%v), want (%d,%d,nil)", total, matched, errSum, test.total, test.matched)
						}
						if errState != nil || len(states) != 1 || states[0].AdmittedInFlight != test.total || len(states[0].Models) != 2 {
							t.Fatalf("state=(%#v,%v)", states, errState)
						}
						for _, modelState := range states[0].Models {
							want := int64(0)
							for _, counter := range test.counts {
								if counter.CredentialID == "cred-1" && counter.Model == modelState.Model {
									want += counter.ActiveCount
								}
							}
							if modelState.AdmittedInFlight != want {
								t.Fatalf("model state=%#v, want %d", modelState, want)
							}
						}
					}
					request := ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 1, Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")}
					result, errAdmit := repo.AdmitCredentialConcurrency(t.Context(), request)
					if test.invalid {
						if errAdmit == nil || result.Accounted || IsConcurrencySaturated(errAdmit) {
							t.Fatalf("admission=(%#v,%v), want tracker failure", result, errAdmit)
						}
						if after := readConcurrencyTestCounters(t, repo); !reflect.DeepEqual(before, after) {
							t.Fatalf("counter mutation: before=%#v after=%#v", before, after)
						}
					} else if errAdmit != nil || !result.Accounted {
						t.Fatalf("admission=(%#v,%v)", result, errAdmit)
					}
				})
			}

			t.Run("query failure and cancellation roll back", func(t *testing.T) {
				for _, cancelQuery := range []bool{false, true} {
					t.Run(map[bool]string{false: "database error", true: "canceled"}[cancelQuery], func(t *testing.T) {
						repo := backend.open(t)
						seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
						seedConcurrencyAdmissionAuth(t, repo, "cred-1")
						seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(3), nil)
						seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 1, 7)
						before := readConcurrencyTestCounters(t, repo)
						request := ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 1, Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")}
						ctx, cancel := context.WithCancel(t.Context())
						defer cancel()
						fired := false
						const callback = "test:fail-concurrency-count-query"
						if errRegister := repo.db.Callback().Row().Before("gorm:row").Register(callback, func(db *gorm.DB) {
							if db.Statement.Table != (CredentialConcurrencyCounterRecord{}).TableName() {
								return
							}
							fired = true
							if cancelQuery {
								cancel()
							} else {
								// Force a real SQL execution error only when the aggregate is reached.
								db.Statement.Table = "missing_concurrency_counter_test_table"
							}
						}); errRegister != nil {
							t.Fatal(errRegister)
						}
						t.Cleanup(func() {
							if errRemove := repo.db.Callback().Row().Remove(callback); errRemove != nil {
								t.Error(errRemove)
							}
						})
						result, errAdmit := repo.AdmitCredentialConcurrency(ctx, request)
						if !fired || errAdmit == nil || result.Accounted || IsConcurrencySaturated(errAdmit) {
							t.Fatalf("fired=%v admission=(%#v,%v)", fired, result, errAdmit)
						}
						if cancelQuery && !errors.Is(errAdmit, context.Canceled) {
							t.Fatalf("error=%v, want canceled", errAdmit)
						}
						if after := readConcurrencyTestCounters(t, repo); !reflect.DeepEqual(before, after) {
							t.Fatalf("counter mutation: before=%#v after=%#v", before, after)
						}
						if states, errState := repo.ReadConcurrencyState(ctx); errState == nil || states != nil {
							t.Fatalf("state=(%#v,%v), want query failure", states, errState)
						}
					})
				}
			})
		})
	}
}

func readConcurrencyTestCounters(t *testing.T, repo *Repository) []CredentialConcurrencyCounterRecord {
	t.Helper()
	var counters []CredentialConcurrencyCounterRecord
	if errFind := repo.db.Order("credential_id, model, certificate_fingerprint").Find(&counters).Error; errFind != nil {
		t.Fatal(errFind)
	}
	return counters
}

func TestSumConcurrencyCountsRejectsNilTransaction(t *testing.T) {
	if total, model, errCounts := sumConcurrencyCountsTx(t.Context(), nil, "cred-1", "gpt"); errCounts == nil || total != 0 || model != 0 {
		t.Fatalf("sum=(%d,%d,%v)", total, model, errCounts)
	}
}
