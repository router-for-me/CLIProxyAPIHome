package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/gorm"
)

func TestValidateCredentialConcurrencyLifecycle(t *testing.T) {
	valid := config.DefaultCredentialConcurrencyConfig()
	if errValidate := ValidateCredentialConcurrencyLifecycle(20*time.Second, valid); errValidate != nil {
		t.Fatalf("valid lifecycle config: %v", errValidate)
	}
	valid.CPAHeartbeatTimeout = 20 * time.Second
	valid.CPACancelBound = time.Second
	valid.ReclaimGrace = time.Second
	if errValidate := ValidateCredentialConcurrencyLifecycle(20*time.Second, valid); errValidate == nil {
		t.Fatal("unsafe lifecycle timing was accepted")
	}
}

func TestValidateCredentialConcurrencyLifecycleRejectsDurationOverflow(t *testing.T) {
	tests := []struct {
		name   string
		node   time.Duration
		mutate func(*config.CredentialConcurrencyConfig)
	}{
		{
			name: "node heartbeat timeout plus reclaim grace",
			node: time.Duration(math.MaxInt64),
			mutate: func(cfg *config.CredentialConcurrencyConfig) {
				cfg.ReclaimGrace = time.Nanosecond
			},
		},
		{
			name: "CPA heartbeat timeout plus cancel bound",
			node: time.Second,
			mutate: func(cfg *config.CredentialConcurrencyConfig) {
				cfg.CPAHeartbeatTimeout = time.Duration(math.MaxInt64)
				cfg.CPACancelBound = time.Nanosecond
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.DefaultCredentialConcurrencyConfig()
			test.mutate(&cfg)
			if errValidate := ValidateCredentialConcurrencyLifecycle(test.node, cfg); errValidate == nil {
				t.Fatal("lifecycle duration overflow was accepted")
			}
		})
	}
}

func TestEnsureLifecycleConfigCreatesRevisionOneForUpgradedDatabase(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)

	record, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second)
	if errEnsure != nil {
		t.Fatal(errEnsure)
	}
	if record.ID != 1 || record.Revision != 1 || record.NodeHeartbeatTimeout != 20*time.Second {
		t.Fatalf("lifecycle record = %#v", record)
	}
	var payload config.CredentialConcurrencyConfig
	if errUnmarshal := json.Unmarshal(record.Payload, &payload); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if payload.LifecycleConfigRevision != 1 || payload.ObservationBarrierRevision != 0 {
		t.Fatalf("lifecycle payload revisions = %d, %d, want 1, 0", payload.LifecycleConfigRevision, payload.ObservationBarrierRevision)
	}
}

func TestEnsureLifecycleConfigRejectsNodeTimeoutMismatch(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, 21*time.Second); errEnsure == nil {
		t.Fatal("EnsureLifecycleConfig accepted a node heartbeat timeout mismatch")
	}
}

func TestUpdateLifecycleConfigPersistsHotLimiterFieldsWithoutRevisionChange(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	current := config.DefaultCredentialConcurrencyConfig()
	if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current); errUpdate != nil || revision != 1 {
		t.Fatalf("initial update = %d, %v", revision, errUpdate)
	}

	next := current
	next.MaxLimit = 99
	next.BusyRetryMax = "2s"
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, next)
	if errUpdate != nil || revision != 1 {
		t.Fatalf("limiter update = %d, %v, want revision 1", revision, errUpdate)
	}

	got, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	if got.MaxLimit != next.MaxLimit || got.BusyRetryMax != next.BusyRetryMax {
		t.Fatalf("lifecycle limiter config = %#v, want %#v", got, next)
	}
}

func TestUpdateLifecycleConfigRejectsActiveMembership(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	current := config.DefaultCredentialConcurrencyConfig()
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current)
	if errUpdate != nil || revision != 1 {
		t.Fatalf("initial update = %d, %v", revision, errUpdate)
	}
	if errSeed := repo.db.Create(&CPANodeMembershipRecord{CertificateFingerprint: "fp-a", NodeID: "cpa-a", State: MembershipStateActive}).Error; errSeed != nil {
		t.Fatal(errSeed)
	}
	next := current
	next.CPAHeartbeatTimeout = 4 * time.Second
	if _, errUpdate = repo.UpdateLifecycleConfig(ctx, 20*time.Second, next); !errors.Is(errUpdate, ErrLifecycleConfigInUse) {
		t.Fatalf("error = %v, want ErrLifecycleConfigInUse", errUpdate)
	}
}

func TestUpdateLifecycleConfigAllowsNoOpWithMembership(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{MembershipStateActive, MembershipStateCanceling} {
		t.Run(state, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			current := config.DefaultCredentialConcurrencyConfig()
			if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current); errUpdate != nil || revision != 1 {
				t.Fatalf("initial update = %d, %v", revision, errUpdate)
			}
			if errSeed := repo.db.Create(&CPANodeMembershipRecord{CertificateFingerprint: "fp-" + state, NodeID: "cpa-" + state, State: state}).Error; errSeed != nil {
				t.Fatal(errSeed)
			}
			if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current); errUpdate != nil || revision != 1 {
				t.Fatalf("unchanged update = %d, %v, want revision 1", revision, errUpdate)
			}
			record, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second)
			if errEnsure != nil {
				t.Fatal(errEnsure)
			}
			if record.Revision != 1 {
				t.Fatalf("revision = %d, want 1", record.Revision)
			}
		})
	}
}

func TestUpdateLifecycleConfigAllowsHotOnlyChangesWithMembership(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(*config.CredentialConcurrencyConfig)
	}{
		{name: "release flush interval", mutate: func(cfg *config.CredentialConcurrencyConfig) { cfg.ReleaseFlushInterval = "500ms" }},
		{name: "release max backoff", mutate: func(cfg *config.CredentialConcurrencyConfig) { cfg.ReleaseMaxBackoff = "3s" }},
		{name: "busy retry min", mutate: func(cfg *config.CredentialConcurrencyConfig) { cfg.BusyRetryMin = "300ms" }},
		{name: "busy retry max", mutate: func(cfg *config.CredentialConcurrencyConfig) { cfg.BusyRetryMax = "2s" }},
		{name: "max limit", mutate: func(cfg *config.CredentialConcurrencyConfig) { cfg.MaxLimit-- }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			current := config.DefaultCredentialConcurrencyConfig()
			if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current); errUpdate != nil || revision != 1 {
				t.Fatalf("initial update = %d, %v", revision, errUpdate)
			}
			member := CPANodeMembershipRecord{CertificateFingerprint: "fp-hot", NodeID: "cpa-hot", State: MembershipStateActive, LifecycleConfigRevision: 1}
			if errSeed := repo.db.Create(&member).Error; errSeed != nil {
				t.Fatal(errSeed)
			}
			next := current
			test.mutate(&next)
			if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, next); errUpdate != nil || revision != 1 {
				t.Fatalf("hot update = %d, %v, want revision 1", revision, errUpdate)
			}
			got, errLifecycle := repo.LifecycleConfig(ctx)
			if errLifecycle != nil {
				t.Fatal(errLifecycle)
			}
			if !lifecycleConfigEqual(got, next) || got.LifecycleConfigRevision != 1 {
				t.Fatalf("lifecycle config = %#v, want %#v with revision 1", got, next)
			}
			var stored CPANodeMembershipRecord
			if errFind := repo.db.First(&stored, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if stored.LifecycleConfigRevision != 1 {
				t.Fatalf("membership lifecycle revision = %d, want 1", stored.LifecycleConfigRevision)
			}
		})
	}
}

func TestUpdateLifecycleConfigRejectsMixedSafetyAndHotChangesWithMembership(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{MembershipStateActive, MembershipStateCanceling} {
		t.Run(state, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			current := config.DefaultCredentialConcurrencyConfig()
			if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current); errUpdate != nil || revision != 1 {
				t.Fatalf("initial update = %d, %v", revision, errUpdate)
			}
			if errSeed := repo.db.Create(&CPANodeMembershipRecord{CertificateFingerprint: "fp-mixed-" + state, NodeID: "cpa-mixed-" + state, State: state}).Error; errSeed != nil {
				t.Fatal(errSeed)
			}
			next := current
			next.CPAHeartbeatTimeout = 4 * time.Second
			next.MaxLimit--
			if revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, next); !errors.Is(errUpdate, ErrLifecycleConfigInUse) || revision != 0 {
				t.Fatalf("mixed update = %d, %v, want ErrLifecycleConfigInUse", revision, errUpdate)
			}
		})
	}
}

func TestUpdateLifecycleConfigKeepsBarrierAndRevisionWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	current := config.DefaultCredentialConcurrencyConfig()
	current.ObservationBarrierRevision = 99
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, current)
	if errUpdate != nil || revision != 1 {
		t.Fatalf("initial update = %d, %v", revision, errUpdate)
	}
	current.ObservationBarrierRevision = 100
	revision, errUpdate = repo.UpdateLifecycleConfig(ctx, 20*time.Second, current)
	if errUpdate != nil || revision != 1 {
		t.Fatalf("unchanged update = %d, %v, want revision 1", revision, errUpdate)
	}
	record, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second)
	if errEnsure != nil {
		t.Fatal(errEnsure)
	}
	var payload config.CredentialConcurrencyConfig
	if errUnmarshal := json.Unmarshal(record.Payload, &payload); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if payload.ObservationBarrierRevision != 0 {
		t.Fatalf("observation barrier = %d, want 0", payload.ObservationBarrierRevision)
	}
}

func TestReplaceConfigSnapshotUsesLifecycleConfig(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	next := config.DefaultCredentialConcurrencyConfig()
	next.CPAHeartbeatTimeout = 4 * time.Second
	next.ObservationBarrierRevision = 99
	if errReplace := repo.ReplaceConfigSnapshotWithLifecycleConfig(ctx, 20*time.Second, map[string]any{
		"credential-concurrency": next,
		"port":                   8327,
	}); errReplace != nil {
		t.Fatal(errReplace)
	}

	snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	if _, exists := snapshot["credential-concurrency"]; exists {
		t.Fatal("lifecycle configuration was stored in the config snapshot")
	}
	cfg, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	if cfg.LifecycleConfigRevision != 2 || cfg.ObservationBarrierRevision != 0 || cfg.CPAHeartbeatTimeout != 4*time.Second {
		t.Fatalf("lifecycle config = %#v", cfg)
	}
}

func TestConcurrencyActivationGateSerializesLegacyAndActivation(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	errs := make(chan error, 2)

	go func() {
		errs <- repo.WithConcurrencyActivationGate(context.Background(), func(_ *gorm.DB, _ *ConcurrencyActivationGateRecord) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first callback did not enter the activation gate")
	}

	go func() {
		errs <- repo.WithConcurrencyActivationGate(context.Background(), func(_ *gorm.DB, _ *ConcurrencyActivationGateRecord) error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second callback entered before the first released the activation gate")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		select {
		case errGate := <-errs:
			if errGate != nil {
				t.Fatal(errGate)
			}
		case <-time.After(time.Second):
			t.Fatal("activation gate callback did not complete")
		}
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second callback did not enter after the first released the activation gate")
	}
}
