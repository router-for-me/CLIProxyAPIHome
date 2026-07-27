package cluster

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestAdmitCredentialConcurrencySerializesLastSlot(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionLifetime(t, repo, "fp-b", "home-b")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(1), map[string]int64{"gpt": 1})

	results := make(chan error, 2)
	for _, request := range []ConcurrencyAdmissionRequest{
		{CredentialID: "cred-1", Model: "gpt(high)", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1},
		{CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-b", "home-b"), ProtocolVersion: 1},
	} {
		request := request
		go func() {
			_, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), request)
			results <- errAdmit
		}()
	}

	passed, saturated := 0, 0
	for range 2 {
		errResult := <-results
		if errResult == nil {
			passed++
		} else if IsConcurrencySaturated(errResult) {
			saturated++
		}
	}
	if passed != 1 || saturated != 1 {
		t.Fatalf("passed=%d saturated=%d", passed, saturated)
	}
	if got := countConcurrencyAdmissionCounters(t, repo); got != 1 {
		t.Fatalf("counter total = %d, want 1", got)
	}
}

func TestAdmitCredentialConcurrencyUnlimitedDoesNotWriteCounter(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-free")

	result, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
		CredentialID: "cred-free", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
	})
	if errAdmit != nil || result.Accounted {
		t.Fatalf("result=%#v error=%v", result, errAdmit)
	}
	if got := countConcurrencyAdmissionCounters(t, repo); got != 0 {
		t.Fatalf("counter rows = %d, want 0", got)
	}
}

func TestAdmitCredentialConcurrencyEnforcesGlobalAndModelLimits(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(2), map[string]int64{"gpt": 1})
	lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")

	if _, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1}); errAdmit != nil {
		t.Fatal(errAdmit)
	}
	if _, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1}); !IsConcurrencySaturated(errAdmit) {
		t.Fatalf("model limit error = %v", errAdmit)
	}
	if _, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "claude", Lifetime: lifetime, ProtocolVersion: 1}); errAdmit != nil {
		t.Fatal(errAdmit)
	}
	if _, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gemini", Lifetime: lifetime, ProtocolVersion: 1}); !IsConcurrencySaturated(errAdmit) {
		t.Fatalf("global limit error = %v", errAdmit)
	}
}

func TestAdmitCredentialConcurrencyDisabledPolicyDoesNotCount(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", nil, nil)

	result, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
		CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 0,
	})
	if errAdmit != nil || result.Accounted {
		t.Fatalf("result=%#v error=%v", result, errAdmit)
	}
	if got := countConcurrencyAdmissionCounters(t, repo); got != 0 {
		t.Fatalf("counter rows = %d, want 0", got)
	}
}

func TestAdmitCredentialConcurrencyRejectsMalformedUTF8BeforeCounterMutation(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(2), nil)
	seedReleaseCounter(t, repo, "cred-1", "gpt�", "fp-a", 1, 0)

	result, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
		CredentialID: "cred-1", Model: "GPT\xff(HIGH)", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
	})
	if !errors.Is(errAdmit, ErrConcurrencyInvalidModel) {
		t.Fatalf("AdmitCredentialConcurrency() error = %v", errAdmit)
	}
	if result.Accounted || result.Model != "" {
		t.Fatalf("admission result = %#v", result)
	}
	counter := loadReleaseCounter(t, repo, "cred-1", "gpt�", "fp-a")
	if counter.ActiveCount != 1 || counter.LastReleaseSeq != 0 {
		t.Fatalf("legal model counter mutated: %#v", counter)
	}
}

func TestAdmitCredentialConcurrencyRejectsUnavailableLifetime(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(1), nil)

	_, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: "gpt", ProtocolVersion: 1})
	if !errors.Is(errAdmit, ErrConcurrencyNodeUnavailable) {
		t.Fatalf("error = %v", errAdmit)
	}
}

func TestAdmitCredentialConcurrencyRejectsInvalidCounters(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(math.MaxInt64), nil)
	if errCreate := repo.db.Create(&CredentialConcurrencyCounterRecord{
		CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fp-a", ActiveCount: -1, UpdatedAt: databaseTestTime,
	}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}

	_, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
		CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
	})
	if errAdmit == nil || IsConcurrencySaturated(errAdmit) {
		t.Fatalf("error = %v, want tracker failure", errAdmit)
	}
}

func TestAdmitCredentialConcurrencyRejectsCounterSumOverflow(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(math.MaxInt64), nil)
	for _, fingerprint := range []string{"fp-a", "fp-b"} {
		if errCreate := repo.db.Create(&CredentialConcurrencyCounterRecord{
			CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: fingerprint, ActiveCount: math.MaxInt64, UpdatedAt: databaseTestTime,
		}).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}

	_, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
		CredentialID: "cred-1", Model: "gpt", Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"), ProtocolVersion: 1,
	})
	if errAdmit == nil || IsConcurrencySaturated(errAdmit) {
		t.Fatalf("error = %v, want tracker failure", errAdmit)
	}
}

func seedConcurrencyAdmissionLifetime(t *testing.T, repo *Repository, fingerprint string, homeIP string) {
	t.Helper()
	home := HomeProcessIncarnationRecord{
		HomeIP: homeIP, HomePort: 8317, StartedAt: databaseTestTime, LastSeenAt: databaseTestTime,
		State: HomeIncarnationActive, Capabilities: JSONB(`[]`),
	}
	if errCreate := repo.db.Create(&home).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	membership := CPANodeMembershipRecord{
		CertificateFingerprint: fingerprint, NodeID: fingerprint, HomeIP: homeIP, HomePort: 8317, HomeStartedAt: databaseTestTime,
		ProtocolVersion: 1, State: MembershipStateActive, ConnectedAt: databaseTestTime, LastSeenAt: databaseTestTime,
	}
	if errCreate := repo.db.Create(&membership).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
}

func concurrencyAdmissionLifetime(t *testing.T, repo *Repository, fingerprint string, homeIP string) ConnectionLifetime {
	t.Helper()
	var membership CPANodeMembershipRecord
	if errFind := repo.db.First(&membership, "certificate_fingerprint = ?", fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	return ConnectionLifetime{
		Fingerprint: fingerprint, ConnectedAt: membership.ConnectedAt, Controlled: true,
		Home: HomeIncarnationID{IP: homeIP, Port: 8317, StartedAt: databaseTestTime},
	}
}

func seedConcurrencyAdmissionAuth(t *testing.T, repo *Repository, credentialID string) {
	t.Helper()
	if errCreate := repo.db.Create(&AuthRecord{UUID: credentialID, ID: credentialID, Index: credentialID, AuthJSON: JSONB(`{}`), Version: 1}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
}

func seedConcurrencyAdmissionPolicy(t *testing.T, repo *Repository, credentialID string, global *int64, models map[string]int64) {
	t.Helper()
	if errCreate := repo.db.Create(&CredentialConcurrencyPolicyRecord{CredentialID: credentialID, MaxInFlight: global, Version: 1, EffectiveAt: databaseTestTime}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for model, limit := range models {
		if errCreate := repo.db.Create(&CredentialConcurrencyModelPolicyRecord{CredentialID: credentialID, Model: model, MaxInFlight: limit}).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
}

func countConcurrencyAdmissionCounters(t *testing.T, repo *Repository) int64 {
	t.Helper()
	var count int64
	if errCount := repo.db.Model(&CredentialConcurrencyCounterRecord{}).Select("COALESCE(SUM(active_count), 0)").Scan(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	return count
}

func TestReadConcurrencyStateReturnsAuthoritativeCounters(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedConcurrencyAdmissionAuth(t, repo, "cred-1")
	seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(3), map[string]int64{"gpt": 2})
	lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	for _, model := range []string{"gpt", "gpt", "claude"} {
		if _, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{CredentialID: "cred-1", Model: model, Lifetime: lifetime, ProtocolVersion: 1}); errAdmit != nil {
			t.Fatal(errAdmit)
		}
	}

	states, errRead := repo.ReadConcurrencyState(context.Background())
	if errRead != nil {
		t.Fatal(errRead)
	}
	if len(states) != 1 || states[0].CredentialID != "cred-1" || states[0].AdmittedInFlight != 3 || !equalOptionalInt64(states[0].MaxInFlight, int64Pointer(3)) {
		t.Fatalf("states = %#v", states)
	}
	if len(states[0].Models) != 1 || states[0].Models[0].Model != "gpt" || states[0].Models[0].AdmittedInFlight != 2 || states[0].Models[0].MaxInFlight != 2 {
		t.Fatalf("models = %#v", states[0].Models)
	}
}

func TestLockActiveConcurrencyLifetimeTxRejectsNonOwnerHome(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "subscription-home")
	currentHome := HomeProcessIncarnationRecord{
		HomeIP: "current-home", HomePort: 8317, StartedAt: databaseTestTime.Add(time.Second), LastSeenAt: databaseTestTime,
		State: HomeIncarnationActive, Capabilities: JSONB(`[]`),
	}
	if errCreate := repo.db.Create(&currentHome).Error; errCreate != nil {
		t.Fatal(errCreate)
	}

	errTransaction := repo.db.Transaction(func(tx *gorm.DB) error {
		return repo.LockActiveConcurrencyLifetimeTx(context.Background(), tx, ConnectionLifetime{
			Fingerprint: "fp-a", ConnectedAt: databaseTestTime, Controlled: true,
			Home: HomeIncarnationID{IP: currentHome.HomeIP, Port: currentHome.HomePort, StartedAt: currentHome.StartedAt},
		})
	})
	if !errors.Is(errTransaction, ErrConcurrencyNodeUnavailable) {
		t.Fatalf("LockActiveConcurrencyLifetimeTx() error = %v, want %v", errTransaction, ErrConcurrencyNodeUnavailable)
	}
}
