package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestHomeIncarnationIsAppendOnlyAndFencedCannotHeartbeat(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	first, errFirst := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	if errFence := repo.FenceHomeIncarnation(ctx, first, "quiescence timeout"); errFence != nil {
		t.Fatal(errFence)
	}
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, first); !errors.Is(errHeartbeat, ErrHomeIncarnationFenced) {
		t.Fatalf("heartbeat error = %v", errHeartbeat)
	}
	second, errSecond := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if !second.StartedAt.After(first.StartedAt) {
		t.Fatalf("started_at did not increase: first=%s second=%s", first.StartedAt, second.StartedAt)
	}
}

func TestHomeIncarnationRegistrationRequiresLimiterCapabilityWhenPoliciesActive(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	gate := ConcurrencyActivationGateRecord{ID: 1, ActivePolicyCount: 1}
	if errGate := repo.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.Assignments(map[string]any{"active_policy_count": 1})}).Create(&gate).Error; errGate != nil {
		t.Fatal(errGate)
	}
	_, errRegister := repo.RegisterHomeIncarnation(context.Background(), "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if !errors.Is(errRegister, ErrConcurrencyHomeCapabilityRequired) {
		t.Fatalf("registration error = %v", errRegister)
	}
}

func TestRetiredHomeIncarnationCannotHeartbeat(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id, errRegister := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, nil)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRetire := repo.RetireHomeIncarnation(ctx, id); errRetire != nil {
		t.Fatal(errRetire)
	}
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, id); !errors.Is(errHeartbeat, ErrHomeIncarnationInactive) {
		t.Fatalf("heartbeat error = %v", errHeartbeat)
	}
}

func TestSQLiteRegistrationExpiresCrashedHomeBeforeSingleHomeCheck(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	now, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	stale := HomeProcessIncarnationRecord{
		HomeIP:     "127.0.0.1",
		HomePort:   8317,
		StartedAt:  now.Add(-21 * time.Second),
		LastSeenAt: now.Add(-21 * time.Second),
		State:      HomeIncarnationActive,
		Capabilities: JSONB(`[
            "credential_concurrency_limits_v2"
        ]`),
	}
	if errCreate := repo.db.Create(&stale).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errGate := repo.db.Model(&ConcurrencyActivationGateRecord{}).Where("id = ?", 1).Update("active_policy_count", 1).Error; errGate != nil {
		t.Fatal(errGate)
	}
	if _, errRegister := repo.RegisterHomeIncarnation(ctx, "127.0.0.2", 8317, []string{"credential_concurrency_limits_v2"}); errRegister != nil {
		t.Fatalf("registration error = %v", errRegister)
	}
	var expired HomeProcessIncarnationRecord
	if errFind := repo.db.First(&expired, "home_ip = ? AND home_port = ? AND started_at = ?", stale.HomeIP, stale.HomePort, stale.StartedAt).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if expired.State != HomeIncarnationExpired {
		t.Fatalf("state = %q, want %q", expired.State, HomeIncarnationExpired)
	}
}

func TestRegistrationExpiresStaleIncarnationBeforeCapabilityAdmission(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	now, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	stale := HomeProcessIncarnationRecord{
		HomeIP:       "127.0.0.1",
		HomePort:     8317,
		StartedAt:    now.Add(-DefaultHeartbeatTimeout() - time.Second),
		LastSeenAt:   now.Add(-DefaultHeartbeatTimeout() - time.Second),
		State:        HomeIncarnationActive,
		Capabilities: JSONB(`["credential_concurrency_limits_v2"]`),
	}
	if errCreate := repo.db.Create(&stale).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	gate := ConcurrencyActivationGateRecord{ID: 1, ActivePolicyCount: 1}
	if errGate := repo.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.Assignments(map[string]any{"active_policy_count": 1})}).Create(&gate).Error; errGate != nil {
		t.Fatal(errGate)
	}

	expiredBeforeAdmission := false
	if errCallback := repo.db.Callback().Update().Before("gorm:update").Register("test:observe_stale_expiry_before_admission", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if tx.Statement.Table == (HomeProcessIncarnationRecord{}).TableName() && ok && values["state"] == HomeIncarnationExpired {
			expiredBeforeAdmission = true
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	_, errRegister := repo.RegisterHomeIncarnation(ctx, "127.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if !errors.Is(errRegister, ErrConcurrencyHomeCapabilityRequired) {
		t.Fatalf("registration error = %v, want %v", errRegister, ErrConcurrencyHomeCapabilityRequired)
	}
	if !expiredBeforeAdmission {
		t.Fatal("stale incarnation expiry did not run before capability admission")
	}
}

func TestSQLiteRegistrationRejectsSecondLiveHomeWhenPoliciesActive(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	first, errFirst := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_limits_v2"})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	if errGate := repo.db.Model(&ConcurrencyActivationGateRecord{}).Where("id = ?", 1).Update("active_policy_count", 1).Error; errGate != nil {
		t.Fatal(errGate)
	}
	_, errRegister := repo.RegisterHomeIncarnation(ctx, "127.0.0.2", 8317, []string{"credential_concurrency_limits_v2"})
	if !errors.Is(errRegister, ErrConcurrencySQLiteMultiHome) {
		t.Fatalf("registration error = %v, want %v", errRegister, ErrConcurrencySQLiteMultiHome)
	}
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, first); errHeartbeat != nil {
		t.Fatalf("first incarnation heartbeat error = %v", errHeartbeat)
	}
}

func TestCoordinatorInitializeBlocksBeforeStartupRecoveryCompletes(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	recoveryStarted := make(chan struct{})
	releaseRecovery := make(chan struct{})
	defer func() {
		select {
		case <-releaseRecovery:
		default:
			close(releaseRecovery)
		}
	}()
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8317}, CoordinatorOptions{
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			close(recoveryStarted)
			<-releaseRecovery
			return nil
		},
	})

	initialized := make(chan error, 1)
	go func() {
		initialized <- coordinator.Initialize(context.Background())
	}()

	select {
	case <-recoveryStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("startup recovery did not begin")
	}
	select {
	case errInitialize := <-initialized:
		t.Fatalf("Initialize() completed while recovery was blocked: %v", errInitialize)
	default:
	}

	close(releaseRecovery)
	if errInitialize := <-initialized; errInitialize != nil {
		t.Fatalf("Initialize() error = %v", errInitialize)
	}
}

func TestCoordinatorInitializeRetiresIncarnationWhenStartupRecoveryFails(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	existing, errRegister := repo.RegisterHomeIncarnation(context.Background(), "127.0.0.2", 8317, nil)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	wantErr := errors.New("startup recovery failed")
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8317}, CoordinatorOptions{
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			return wantErr
		},
	})

	errInitialize := coordinator.Initialize(context.Background())
	if !errors.Is(errInitialize, wantErr) {
		t.Fatalf("Initialize() error = %v, want %v", errInitialize, wantErr)
	}
	if state := homeIncarnationState(t, repo, existing); state != HomeIncarnationActive {
		t.Fatalf("existing incarnation state = %q, want %q", state, HomeIncarnationActive)
	}
	var failed HomeProcessIncarnationRecord
	if errFind := repo.db.Where("home_ip = ? AND home_port = ?", "127.0.0.1", 8317).First(&failed).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if failed.State != HomeIncarnationRetired {
		t.Fatalf("failed incarnation state = %q, want %q", failed.State, HomeIncarnationRetired)
	}
}

func TestCoordinatorInitializeRetiresExactIncarnationWhenStartupRecoveryCancelsContext(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	existing, errRegister := repo.RegisterHomeIncarnation(context.Background(), "127.0.0.1", 8317, nil)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantErr := errors.New("startup recovery failed")
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8317}, CoordinatorOptions{
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			cancel()
			return wantErr
		},
	})

	errInitialize := coordinator.Initialize(ctx)
	if !errors.Is(errInitialize, wantErr) {
		t.Fatalf("Initialize() error = %v, want %v", errInitialize, wantErr)
	}
	if state := homeIncarnationState(t, repo, existing); state != HomeIncarnationActive {
		t.Fatalf("existing incarnation state = %q, want %q", state, HomeIncarnationActive)
	}
	var failed HomeProcessIncarnationRecord
	if errFind := repo.db.Where("home_ip = ? AND home_port = ?", "127.0.0.1", 8317).Order("started_at DESC").First(&failed).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if failed.State != HomeIncarnationRetired {
		t.Fatalf("failed incarnation state = %q, want %q", failed.State, HomeIncarnationRetired)
	}
}

func TestCoordinatorInitializeRetiresIncarnationWhenInitialHeartbeatContextCanceled(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if errCallback := repo.db.Callback().Update().Before("gorm:update").Register("test:cancel_initial_home_heartbeat", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if tx.Statement.Table == (HomeProcessIncarnationRecord{}).TableName() && ok && values["last_seen_at"] != nil {
			cancel()
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8317}, CoordinatorOptions{})
	errInitialize := coordinator.Initialize(ctx)
	if !errors.Is(errInitialize, context.Canceled) {
		t.Fatalf("Initialize() error = %v, want %v", errInitialize, context.Canceled)
	}
	assertNoActiveHomeIncarnations(t, repo)
}

func TestCoordinatorInitializeRetiresIncarnationWhenInitialHeartbeatFails(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	wantErr := errors.New("initial Home heartbeat failed")
	if errCallback := repo.db.Callback().Update().Before("gorm:update").Register("test:fail_initial_home_heartbeat", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if tx.Statement.Table == (HomeProcessIncarnationRecord{}).TableName() && ok && values["last_seen_at"] != nil {
			tx.AddError(wantErr)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8317}, CoordinatorOptions{})
	errInitialize := coordinator.Initialize(context.Background())
	if !errors.Is(errInitialize, wantErr) {
		t.Fatalf("Initialize() error = %v, want %v", errInitialize, wantErr)
	}
	assertNoActiveHomeIncarnations(t, repo)
}

func homeIncarnationState(t *testing.T, repo *Repository, id HomeIncarnationID) string {
	t.Helper()
	var record HomeProcessIncarnationRecord
	if errFind := repo.db.First(&record, "home_ip = ? AND home_port = ? AND started_at = ?", id.IP, id.Port, id.StartedAt).Error; errFind != nil {
		t.Fatal(errFind)
	}
	return record.State
}

func assertNoActiveHomeIncarnations(t *testing.T, repo *Repository) {
	t.Helper()
	var count int64
	if errCount := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("state = ?", HomeIncarnationActive).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if count != 0 {
		t.Fatalf("active Home incarnations = %d, want 0", count)
	}
}

func TestDatabaseNowIgnoresProcessClock(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	now, errNow := DatabaseNow(context.Background(), repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	var raw string
	if errScan := repo.db.Raw("SELECT CURRENT_TIMESTAMP").Scan(&raw).Error; errScan != nil {
		t.Fatal(errScan)
	}
	expected, errParse := time.Parse("2006-01-02 15:04:05", raw)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !now.Equal(expected.UTC()) {
		t.Fatalf("database now = %s, want %s", now, expected.UTC())
	}
}
