package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConcurrencyPolicyPatchOptionalsPreservePresence(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")

	cases := []struct {
		name         string
		payload      string
		wantGlobal   *int64
		wantSet      bool
		wantNull     bool
		wantRevision int64
	}{
		{name: "omitted", payload: `{}`, wantGlobal: nil, wantRevision: 0},
		{name: "null", payload: `{"max_in_flight":null}`, wantGlobal: nil, wantSet: true, wantNull: true, wantRevision: 1},
		{name: "zero", payload: `{"max_in_flight":0}`, wantGlobal: nil, wantSet: true, wantRevision: 2},
		{name: "positive", payload: `{"max_in_flight":2}`, wantGlobal: int64Pointer(2), wantSet: true, wantRevision: 3},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var patch ConcurrencyPolicyPatch
			if errUnmarshal := json.Unmarshal([]byte(testCase.payload), &patch); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if patch.MaxInFlight.Set != testCase.wantSet || patch.MaxInFlight.Null != testCase.wantNull {
				t.Fatalf("MaxInFlight optional = %#v", patch.MaxInFlight)
			}
			policy, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", patch, nil)
			if errPatch != nil {
				t.Fatal(errPatch)
			}
			if !equalOptionalInt64(policy.MaxInFlight, testCase.wantGlobal) {
				t.Fatalf("MaxInFlight = %v, want %v", policy.MaxInFlight, testCase.wantGlobal)
			}
			if policy.ObservationBarrierRevision != testCase.wantRevision {
				t.Fatalf("ObservationBarrierRevision = %d, want %d", policy.ObservationBarrierRevision, testCase.wantRevision)
			}
			stored, errGet := repo.GetCredentialConcurrencyPolicy(context.Background(), "cred-1")
			if errGet != nil {
				t.Fatal(errGet)
			}
			if !equalOptionalInt64(stored.MaxInFlight, testCase.wantGlobal) || stored.ObservationBarrierRevision != testCase.wantRevision {
				t.Fatalf("stored policy = %#v", stored)
			}
		})
	}
}

func TestConcurrencyPolicyPatchModelReplacementDeletesNullAndZero(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")

	var initial ConcurrencyPolicyPatch
	if errUnmarshal := json.Unmarshal([]byte(`{"max_in_flight_by_model":{"gpt":2,"claude":3}}`), &initial); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", initial, nil); errPatch != nil {
		t.Fatal(errPatch)
	}

	var replacement ConcurrencyPolicyPatch
	if errUnmarshal := json.Unmarshal([]byte(`{"max_in_flight_by_model":{"gpt":null,"claude":0,"gemini":4}}`), &replacement); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	policy, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", replacement, nil)
	if errPatch != nil {
		t.Fatal(errPatch)
	}
	if len(policy.MaxInFlightByModel) != 1 || policy.MaxInFlightByModel["gemini"] != 4 {
		t.Fatalf("MaxInFlightByModel = %#v", policy.MaxInFlightByModel)
	}
}

func TestConcurrencyPolicyPatchNullModelMapClearsPolicies(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	limit := int64(2)
	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{"gpt": &limit}},
	}, nil); errPatch != nil {
		t.Fatal(errPatch)
	}
	var patch ConcurrencyPolicyPatch
	if errUnmarshal := json.Unmarshal([]byte(`{"max_in_flight_by_model":null}`), &patch); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	policy, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", patch, nil)
	if errPatch != nil {
		t.Fatal(errPatch)
	}
	if len(policy.MaxInFlightByModel) != 0 {
		t.Fatalf("MaxInFlightByModel = %#v, want empty", policy.MaxInFlightByModel)
	}
}

func TestPatchCredentialConcurrencyPolicyBlocksLegacyMembership(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	seedActiveCPAMembership(t, repo, "fp-1", 0)

	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 2},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencyLegacyMembershipActive) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
}

func TestPatchCredentialConcurrencyPolicyRejectsCanonicalDuplicate(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")

	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{
			"gpt(high)": int64Pointer(2),
			"gpt":       int64Pointer(3),
		}},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencyDuplicateModel) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
}

func TestPatchCredentialConcurrencyPolicyEnforcesCanonicalModelKeyByteLimitBeforeSave(t *testing.T) {
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
			seedConcurrencyPolicyAuth(t, repo, "cred-1")
			_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
				MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{testCase.model: int64Pointer(1)}},
			}, nil)
			if testCase.valid {
				if errPatch != nil {
					t.Fatal(errPatch)
				}
				return
			}
			if !errors.Is(errPatch, ErrConcurrencyInvalidModel) {
				t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
			}
			var policies, models int64
			if errCount := repo.db.Model(&CredentialConcurrencyPolicyRecord{}).Count(&policies).Error; errCount != nil {
				t.Fatal(errCount)
			}
			if errCount := repo.db.Model(&CredentialConcurrencyModelPolicyRecord{}).Count(&models).Error; errCount != nil {
				t.Fatal(errCount)
			}
			if policies != 0 || models != 0 {
				t.Fatalf("oversized model mutated policy state: policies=%d models=%d", policies, models)
			}
		})
	}
}

func TestPatchCredentialConcurrencyPolicyRejectsMalformedUTF8BeforeMutation(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")

	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{"GPT\xff(HIGH)": int64Pointer(1)}},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencyInvalidModel) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}

	var policies, models, barriers, gates, events int64
	for _, check := range []struct {
		name  string
		model any
		count *int64
	}{
		{name: "policies", model: &CredentialConcurrencyPolicyRecord{}, count: &policies},
		{name: "models", model: &CredentialConcurrencyModelPolicyRecord{}, count: &models},
		{name: "barriers", model: &ConcurrencyObservationBarrierRecord{}, count: &barriers},
		{name: "gates", model: &ConcurrencyActivationGateRecord{}, count: &gates},
		{name: "events", model: &ClusterEventRecord{}, count: &events},
	} {
		if errCount := repo.db.Model(check.model).Count(check.count).Error; errCount != nil {
			t.Fatalf("count %s: %v", check.name, errCount)
		}
	}
	if policies != 0 || models != 0 || barriers != 0 || gates != 0 || events != 0 {
		t.Fatalf("malformed model mutated policy state: policies=%d models=%d barriers=%d gates=%d events=%d", policies, models, barriers, gates, events)
	}
}

func TestPatchCredentialConcurrencyPolicyRejectsUnknownCredential(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "missing", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 1},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencyCredentialNotFound) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
}

func TestPatchCredentialConcurrencyPolicyRespectsConfiguredLimit(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	lifecycle := config.DefaultCredentialConcurrencyConfig()
	lifecycle.MaxLimit = 1
	if _, errUpdate := repo.UpdateLifecycleConfig(context.Background(), DefaultHeartbeatTimeout(), lifecycle); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 2},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencyInvalidLimit) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
}

func TestPatchCredentialConcurrencyPolicyRequiresCompatibleLiveHomes(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	if _, errRegister := repo.RegisterHomeIncarnation(context.Background(), "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"}); errRegister != nil {
		t.Fatal(errRegister)
	}

	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 1},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencyHomeCapabilityMissing) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
}

func TestPatchCredentialConcurrencyPolicyRejectsMultipleSQLiteHomes(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		if _, errRegister := repo.RegisterHomeIncarnation(context.Background(), ip, 8317, []string{"credential_concurrency_limits_v2"}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 1},
	}, nil)
	if !errors.Is(errPatch, ErrConcurrencySQLiteMultiHome) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
}

func TestConcurrencyActivationGateAllowsOnlyOneLegacySubscriptionOrPolicyActivation(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	home, errRegister := repo.RegisterHomeIncarnation(context.Background(), "127.0.0.1", 8317, []string{"credential_concurrency_limits_v2"})
	if errRegister != nil {
		t.Fatal(errRegister)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	go func() {
		<-start
		_, errSubscribe := repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{
			Fingerprint: "fp-legacy", NodeID: "cpa-legacy", Home: home, ProtocolVersion: 0,
		})
		errorsCh <- errSubscribe
	}()
	go func() {
		<-start
		_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
			MaxInFlight: OptionalLimit{Set: true, Value: 1},
		}, nil)
		errorsCh <- errPatch
	}()
	close(start)

	first := <-errorsCh
	second := <-errorsCh
	if (first == nil) == (second == nil) {
		t.Fatalf("results = %v, %v; want exactly one success", first, second)
	}
	if first != nil && !errors.Is(first, ErrConcurrencyLegacyMembershipActive) && !errors.Is(first, ErrConcurrencyProtocolRequired) {
		t.Fatalf("first error = %v", first)
	}
	if second != nil && !errors.Is(second, ErrConcurrencyLegacyMembershipActive) && !errors.Is(second, ErrConcurrencyProtocolRequired) {
		t.Fatalf("second error = %v", second)
	}
}

func TestRuntimeConfigIncludesObservationBarrier(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	policy, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 2}}, nil)
	if errPatch != nil {
		t.Fatal(errPatch)
	}
	cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	if policy.ObservationBarrierRevision <= 0 || cfg.CredentialConcurrency.ObservationBarrierRevision != policy.ObservationBarrierRevision {
		t.Fatalf("policy=%#v config=%#v", policy, cfg.CredentialConcurrency)
	}
}

func TestPatchCredentialConcurrencyPolicyUsesExpectedVersionAndRetainsEmptyPolicy(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	policy, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 2}}, nil)
	if errPatch != nil {
		t.Fatal(errPatch)
	}
	wrongVersion := policy.Version + 1
	if _, errPatch = repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 0}}, &wrongVersion); !errors.Is(errPatch, ErrConcurrencyPolicyVersionConflict) {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
	policy, errPatch = repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 0}}, &policy.Version)
	if errPatch != nil {
		t.Fatal(errPatch)
	}
	if policy.MaxInFlight != nil {
		t.Fatalf("MaxInFlight = %v, want nil", policy.MaxInFlight)
	}
	var record CredentialConcurrencyPolicyRecord
	if errFind := repo.db.First(&record, "credential_id = ?", "cred-1").Error; errFind != nil {
		t.Fatalf("empty policy record was not retained: %v", errFind)
	}
}

func seedConcurrencyPolicyAuth(t *testing.T, repo *Repository, id string) {
	t.Helper()
	if errCreate := repo.db.Create(&AuthRecord{UUID: id, ID: id, Index: id, AuthJSON: JSONB(`{}`), Version: 1}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
}

func seedActiveCPAMembership(t *testing.T, repo *Repository, fingerprint string, protocolVersion int) {
	t.Helper()
	if errCreate := repo.db.Create(&CPANodeMembershipRecord{
		CertificateFingerprint: fingerprint,
		NodeID:                 fingerprint,
		HomeIP:                 "127.0.0.1",
		HomePort:               8317,
		HomeStartedAt:          databaseTestTime,
		ProtocolVersion:        protocolVersion,
		State:                  MembershipStateActive,
		ConnectedAt:            databaseTestTime,
		LastSeenAt:             databaseTestTime,
	}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
}

var databaseTestTime = time.Unix(1, 0).UTC()

func int64Pointer(value int64) *int64 {
	return &value
}

func equalOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func TestPatchCredentialConcurrencyPolicyLocksAuthHomesThenPolicy(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")

	var order []string
	ctx := context.WithValue(context.Background(), concurrencyPolicyLockOrderContextKey{}, func(step string) {
		order = append(order, step)
	})
	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 1},
	}, nil); errPatch != nil {
		t.Fatal(errPatch)
	}
	if len(order) != 3 || order[0] != "auth" || order[1] != "homes" || order[2] != "policy" {
		t.Fatalf("lock order = %v, want auth then Homes then policy", order)
	}
}

func TestPatchCredentialConcurrencyPolicyExpiresStaleHomesBeforeActivation(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	now, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	stale := HomeProcessIncarnationRecord{
		HomeIP:       "127.0.0.1",
		HomePort:     8317,
		StartedAt:    now.Add(-21 * time.Second),
		LastSeenAt:   now.Add(-21 * time.Second),
		State:        HomeIncarnationActive,
		Capabilities: JSONB(`[]`),
	}
	if errCreate := repo.db.Create(&stale).Error; errCreate != nil {
		t.Fatal(errCreate)
	}

	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 1},
	}, nil); errPatch != nil {
		t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
	}
	var stored HomeProcessIncarnationRecord
	if errFind := repo.db.First(&stored, "home_ip = ? AND home_port = ? AND started_at = ?", stale.HomeIP, stale.HomePort, stale.StartedAt).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if stored.State != HomeIncarnationExpired {
		t.Fatalf("stale Home state = %q, want %q", stored.State, HomeIncarnationExpired)
	}
}

func TestPatchCredentialConcurrencyPolicyRejectsNonProtocolOneMembership(t *testing.T) {
	for _, protocolVersion := range []int{-1, 0, 2} {
		t.Run(fmt.Sprintf("protocol-%d", protocolVersion), func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			seedConcurrencyPolicyAuth(t, repo, "cred-1")
			seedActiveCPAMembership(t, repo, "fp-1", protocolVersion)

			_, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), "cred-1", ConcurrencyPolicyPatch{
				MaxInFlight: OptionalLimit{Set: true, Value: 1},
			}, nil)
			if !errors.Is(errPatch, ErrConcurrencyLegacyMembershipActive) {
				t.Fatalf("PatchCredentialConcurrencyPolicy() error = %v", errPatch)
			}
		})
	}
}

func TestGetCredentialConcurrencyPolicyBlocksUntilPatchCommits(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	initial := ConcurrencyPolicyPatch{MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{"gpt": int64Pointer(1)}}}
	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", initial, nil); errPatch != nil {
		t.Fatal(errPatch)
	}

	authLocked := make(chan struct{})
	continuePatch := make(chan struct{})
	patchCtx := context.WithValue(ctx, concurrencyPolicyPatchAuthLockAcquiredContextKey{}, func() {
		close(authLocked)
		<-continuePatch
	})
	patchDone := make(chan error, 1)
	go func() {
		_, errPatch := repo.PatchCredentialConcurrencyPolicy(patchCtx, "cred-1", ConcurrencyPolicyPatch{
			MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{"claude": int64Pointer(2)}},
		}, nil)
		patchDone <- errPatch
	}()
	select {
	case <-authLocked:
	case <-ctx.Done():
		t.Fatal("PatchCredentialConcurrencyPolicy() did not lock the auth record")
	}

	readResult := make(chan CredentialConcurrencyPolicy, 1)
	readError := make(chan error, 1)
	go func() {
		policy, errGet := repo.GetCredentialConcurrencyPolicy(ctx, "cred-1")
		if errGet != nil {
			readError <- errGet
			return
		}
		readResult <- policy
	}()
	select {
	case errGet := <-readError:
		t.Fatalf("GetCredentialConcurrencyPolicy() returned before patch commit: %v", errGet)
	case policy := <-readResult:
		t.Fatalf("GetCredentialConcurrencyPolicy() returned before patch commit: %#v", policy)
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("GetCredentialConcurrencyPolicy() did not remain blocked")
	}

	close(continuePatch)
	select {
	case errPatch := <-patchDone:
		if errPatch != nil {
			t.Fatal(errPatch)
		}
	case <-ctx.Done():
		t.Fatal("PatchCredentialConcurrencyPolicy() did not commit")
	}
	select {
	case errGet := <-readError:
		t.Fatal(errGet)
	case policy := <-readResult:
		if policy.Version != 2 || len(policy.MaxInFlightByModel) != 1 || policy.MaxInFlightByModel["claude"] != 2 {
			t.Fatalf("GetCredentialConcurrencyPolicy() = %#v, want replacement policy", policy)
		}
	case <-ctx.Done():
		t.Fatal("GetCredentialConcurrencyPolicy() did not finish")
	}
}

func TestPatchCredentialConcurrencyPolicyAndLegacySubscriptionDoNotDeadlockPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	home, errHome := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{credentialConcurrencyLimitsCapability})
	if errHome != nil {
		t.Fatal(errHome)
	}
	lifecycle, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	go func() {
		<-start
		_, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, "cred-1", ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 1}}, nil)
		errorsCh <- errPatch
	}()
	go func() {
		<-start
		_, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
			Fingerprint: "fp-legacy", NodeID: "cpa-legacy", Home: home, ProtocolVersion: 0, LifecycleConfigRevision: lifecycle.LifecycleConfigRevision,
		})
		errorsCh <- errSubscribe
	}()
	close(start)

	first := <-errorsCh
	second := <-errorsCh
	if (first == nil) == (second == nil) {
		t.Fatalf("results = %v, %v; want exactly one success", first, second)
	}
}
