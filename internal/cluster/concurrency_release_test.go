package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
)

func TestApplyConcurrencyReleaseIsCumulativeAndPreservesBaseline(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 3, 0)
	req := ConcurrencyReleaseRequest{
		CredentialID: "cred-1", Model: "gpt(high)", ReleaseSeq: 2,
		Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"),
	}

	if errRelease := repo.ApplyConcurrencyRelease(context.Background(), req); errRelease != nil {
		t.Fatalf("first release error = %v", errRelease)
	}
	if errRelease := repo.ApplyConcurrencyRelease(context.Background(), req); errRelease != nil {
		t.Fatalf("duplicate release error = %v", errRelease)
	}

	counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
	if counter.ActiveCount != 1 || counter.LastReleaseSeq != 2 {
		t.Fatalf("counter = %#v", counter)
	}
}

func TestAdmissionAndReleaseUseCanonicalModelKeyByteLimit(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		model string
		valid bool
	}{
		{name: "ascii boundary", model: strings.Repeat("a", concurrency.MaxCanonicalModelKeyBytes), valid: true},
		{name: "multibyte boundary", model: strings.Repeat("界", 85) + "a", valid: true},
		{name: "ascii over", model: strings.Repeat("a", concurrency.MaxCanonicalModelKeyBytes+1)},
		{name: "multibyte over", model: strings.Repeat("界", 86)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
			seedConcurrencyAdmissionAuth(t, repo, "cred-1")
			seedConcurrencyAdmissionPolicy(t, repo, "cred-1", int64Pointer(1), nil)
			lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")

			admission, errAdmit := repo.AdmitCredentialConcurrency(context.Background(), ConcurrencyAdmissionRequest{
				CredentialID: "cred-1", Model: testCase.model + "(high)", Lifetime: lifetime, ProtocolVersion: 1,
			})
			if !testCase.valid {
				if !errors.Is(errAdmit, ErrConcurrencyInvalidModel) {
					t.Fatalf("AdmitCredentialConcurrency() error = %v", errAdmit)
				}
				if got := countConcurrencyAdmissionCounters(t, repo); got != 0 {
					t.Fatalf("oversized admission counter total = %d, want 0", got)
				}
				return
			}
			if errAdmit != nil || !admission.Accounted || admission.Model != testCase.model {
				t.Fatalf("admission = %#v, error = %v", admission, errAdmit)
			}
			if errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{
				CredentialID: "cred-1", Model: testCase.model + "(medium)", ReleaseSeq: 1, Lifetime: lifetime,
			}); errRelease != nil {
				t.Fatalf("ApplyConcurrencyRelease() error = %v", errRelease)
			}
			counter := loadReleaseCounter(t, repo, "cred-1", testCase.model, "fp-a")
			if counter.ActiveCount != 0 || counter.LastReleaseSeq != 1 {
				t.Fatalf("counter = %#v", counter)
			}
		})
	}
}

func TestApplyConcurrencyReleaseRejectsMalformedUTF8BeforeCounterMutation(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt�", "fp-a", 1, 0)

	errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{
		CredentialID: "cred-1", Model: "GPT\xff(HIGH)", ReleaseSeq: 1,
		Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"),
	})
	if !errors.Is(errRelease, ErrConcurrencyInvalidRelease) {
		t.Fatalf("ApplyConcurrencyRelease() error = %v", errRelease)
	}
	counter := loadReleaseCounter(t, repo, "cred-1", "gpt�", "fp-a")
	if counter.ActiveCount != 1 || counter.LastReleaseSeq != 0 {
		t.Fatalf("legal model counter mutated: %#v", counter)
	}
}

func TestApplyConcurrencyReleaseIgnoresDuplicateAndOutOfOrderSequences(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 3, 0)
	lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")

	for _, seq := range []int64{2, 1, 2, 3} {
		if errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{CredentialID: "cred-1", Model: "gpt", ReleaseSeq: seq, Lifetime: lifetime}); errRelease != nil {
			t.Fatalf("release sequence %d error = %v", seq, errRelease)
		}
	}
	counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
	if counter.ActiveCount != 0 || counter.LastReleaseSeq != 3 {
		t.Fatalf("counter = %#v", counter)
	}
}

func TestApplyConcurrencyReleaseIsCumulativePostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 2, 0)
	lifetime := concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")

	if errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{CredentialID: "cred-1", Model: "gpt(high)", ReleaseSeq: 1, Lifetime: lifetime}); errRelease != nil {
		t.Fatal(errRelease)
	}
	if errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 2, Lifetime: lifetime}); errRelease != nil {
		t.Fatal(errRelease)
	}
	counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
	if counter.ActiveCount != 0 || counter.LastReleaseSeq != 2 {
		t.Fatalf("counter = %#v", counter)
	}
}

func TestApplyConcurrencyReleaseRejectsFutureSequenceWithoutMutation(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 2, 4)

	errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{
		CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 7,
		Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"),
	})
	if !errors.Is(errRelease, ErrConcurrencyReleaseExceedsActive) {
		t.Fatalf("release error = %v, want %v", errRelease, ErrConcurrencyReleaseExceedsActive)
	}
	counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
	if counter.ActiveCount != 2 || counter.LastReleaseSeq != 4 {
		t.Fatalf("counter mutated after rejected release = %#v", counter)
	}
}

func TestApplyConcurrencyReleasePreservesCounterAfterPolicyRemoval(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyAdmissionLifetime(t, repo, "fp-a", "home-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 1, 5)

	if errRelease := repo.ApplyConcurrencyRelease(context.Background(), ConcurrencyReleaseRequest{
		CredentialID: "cred-1", Model: "gpt", ReleaseSeq: 6,
		Lifetime: concurrencyAdmissionLifetime(t, repo, "fp-a", "home-a"),
	}); errRelease != nil {
		t.Fatalf("release after policy removal error = %v", errRelease)
	}
	counter := loadReleaseCounter(t, repo, "cred-1", "gpt", "fp-a")
	if counter.ActiveCount != 0 || counter.LastReleaseSeq != 6 {
		t.Fatalf("counter = %#v", counter)
	}
}

func TestCompleteFingerprintCancellationDeletesCountersBeforeClosed(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-a")
	seedReleaseCounter(t, repo, "cred-1", "gpt", "fp-a", 1, 7)
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, home); errAck != nil {
		t.Fatal(errAck)
	}

	if errComplete := repo.CompleteFingerprintCancellation(ctx, "fp-a", member.ConnectedAt, revision); errComplete != nil {
		t.Fatalf("CompleteFingerprintCancellation() error = %v", errComplete)
	}
	if got := countReleaseFingerprintCounters(t, repo, "fp-a"); got != 0 {
		t.Fatalf("counter rows = %d", got)
	}
	assertMembershipState(t, repo, "fp-a", MembershipStateClosed)
}

func seedReleaseCounter(t *testing.T, repo *Repository, credentialID string, model string, fingerprint string, activeCount int64, lastReleaseSeq int64) {
	t.Helper()
	if errCreate := repo.db.Create(&CredentialConcurrencyCounterRecord{
		CredentialID: credentialID, Model: model, CertificateFingerprint: fingerprint,
		ActiveCount: activeCount, LastReleaseSeq: lastReleaseSeq, UpdatedAt: databaseTestTime,
	}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
}

func loadReleaseCounter(t *testing.T, repo *Repository, credentialID string, model string, fingerprint string) CredentialConcurrencyCounterRecord {
	t.Helper()
	var counter CredentialConcurrencyCounterRecord
	if errFind := repo.db.First(&counter, "credential_id = ? AND model = ? AND certificate_fingerprint = ?", credentialID, model, fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	return counter
}

func countReleaseFingerprintCounters(t *testing.T, repo *Repository, fingerprint string) int64 {
	t.Helper()
	var count int64
	if errCount := repo.db.Model(&CredentialConcurrencyCounterRecord{}).Where("certificate_fingerprint = ?", fingerprint).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	return count
}
