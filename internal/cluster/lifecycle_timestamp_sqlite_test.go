package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func newSQLiteHeartbeatTestLifetime(t *testing.T) (*Repository, ConnectionLifetime, time.Duration) {
	t.Helper()
	repo := newCredentialFoundationTestRepository(t)
	cfg, errConfig := repo.EnsureLifecycleConfig(t.Context(), DefaultHeartbeatTimeout())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	home, errHome := repo.RegisterHomeIncarnation(t.Context(), "sqlite-heartbeat-home", 8327, nil)
	if errHome != nil {
		t.Fatal(errHome)
	}
	member, errMember := repo.SubscribeMembership(t.Context(), SubscribeMembershipRequest{
		Fingerprint: "sqlite-heartbeat-fingerprint", NodeID: "sqlite-heartbeat-cpa", Home: home,
		ProtocolVersion: 1, LifecycleConfigRevision: cfg.Revision,
	})
	if errMember != nil {
		t.Fatal(errMember)
	}
	return repo, ConnectionLifetime{
		Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home, Controlled: true,
	}, member.CPAHeartbeatTimeout
}

func setSQLiteLifecycleTestClock(t *testing.T, repo *Repository, now *time.Time) {
	t.Helper()
	const callback = "test:sqlite-lifecycle-clock"
	if errRegister := repo.db.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			tx.Statement.SQL.Reset()
			tx.Statement.SQL.WriteString("SELECT ?")
			tx.Statement.Vars = []any{now.UTC().Format(time.RFC3339Nano)}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	t.Cleanup(func() {
		if errRemove := repo.db.Callback().Row().Remove(callback); errRemove != nil {
			t.Errorf("remove SQLite lifecycle test clock: %v", errRemove)
		}
	})
}

func TestSQLiteHomeHeartbeatExactTimeoutBoundary(t *testing.T) {
	repo, lifetime, _ := newSQLiteHeartbeatTestLifetime(t)
	ctx := t.Context()
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, lifetime.Home); errHeartbeat != nil {
		t.Fatal(errHeartbeat)
	}
	var home HomeProcessIncarnationRecord
	if errFind := repo.db.First(&home, "home_ip = ?", lifetime.Home.IP).Error; errFind != nil {
		t.Fatal(errFind)
	}
	lastSeen := home.LastSeenAt.UTC()
	now := lastSeen.Add(DefaultHeartbeatTimeout())
	setSQLiteLifecycleTestClock(t, repo, &now)
	expire := func() {
		t.Helper()
		if errExpire := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return expireStaleHomeIncarnations(ctx, tx, DefaultHeartbeatTimeout())
		}); errExpire != nil {
			t.Fatal(errExpire)
		}
		if errFind := repo.db.First(&home, "home_ip = ?", lifetime.Home.IP).Error; errFind != nil {
			t.Fatal(errFind)
		}
	}
	expire()
	if home.State != HomeIncarnationActive {
		t.Fatalf("Home expired at the exact timeout boundary: state = %q", home.State)
	}
	var matching int64
	if errCount := repo.db.Model(&HomeProcessIncarnationRecord{}).
		Where("home_ip = ? AND last_seen_at = ?", lifetime.Home.IP, lastSeen).Count(&matching).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if matching != 1 {
		t.Fatal("Home heartbeat storage does not match a bound UTC time.Time")
	}
	now = now.Add(databaseTimestampStep(repo.db))
	expire()
	if home.State != HomeIncarnationExpired {
		t.Fatalf("Home state beyond timeout = %q, want expired", home.State)
	}
}

func TestSQLiteCPAHeartbeatExactTimeoutBoundary(t *testing.T) {
	repo, lifetime, heartbeatTimeout := newSQLiteHeartbeatTestLifetime(t)
	ctx := t.Context()
	if errHeartbeat := repo.RefreshCPALiveness(ctx, lifetime); errHeartbeat != nil {
		t.Fatal(errHeartbeat)
	}
	var member CPANodeMembershipRecord
	if errFind := repo.db.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	lastSeen := member.LastSeenAt.UTC()
	var matching, expired int64
	if errCount := repo.db.Model(&CPANodeMembershipRecord{}).
		Where("certificate_fingerprint = ? AND last_seen_at = ? AND updated_at = ?", lifetime.Fingerprint, lastSeen, member.UpdatedAt.UTC()).Count(&matching).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if matching != 1 {
		t.Error("CPA heartbeat storage does not match bound UTC time.Time values")
	}
	if errCount := repo.db.Model(&CPANodeMembershipRecord{}).
		Where("certificate_fingerprint = ? AND last_seen_at < ?", lifetime.Fingerprint, lastSeen).Count(&expired).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if expired != 0 {
		t.Error("CPA heartbeat incorrectly matches a strict expiry predicate at the exact timeout boundary")
	}
	now := lastSeen.Add(heartbeatTimeout)
	setSQLiteLifecycleTestClock(t, repo, &now)
	if errCleanup := repo.cleanupStaleMembership(ctx, lifetime.Fingerprint, heartbeatTimeout); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	if errFind := repo.db.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if member.State != MembershipStateActive {
		t.Fatalf("CPA state at the exact timeout boundary = %q, want active", member.State)
	}
	now = now.Add(databaseTimestampStep(repo.db))
	if errCleanup := repo.cleanupStaleMembership(ctx, lifetime.Fingerprint, heartbeatTimeout); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	if errFind := repo.db.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if member.State != MembershipStateCanceling {
		t.Fatalf("CPA state beyond timeout = %q, want canceling", member.State)
	}
}

func TestSQLiteHeartbeatDatabaseClockFailureRollsBack(t *testing.T) {
	for _, kind := range []string{"home", "cpa"} {
		t.Run(kind, func(t *testing.T) {
			repo, lifetime, _ := newSQLiteHeartbeatTestLifetime(t)
			var beforeHome HomeProcessIncarnationRecord
			var beforeCPA CPANodeMembershipRecord
			if errFind := repo.db.First(&beforeHome, "home_ip = ?", lifetime.Home.IP).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if errFind := repo.db.First(&beforeCPA, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
				t.Fatal(errFind)
			}
			wantErr := errors.New("database clock unavailable")
			if errRegister := repo.db.Callback().Row().Before("gorm:row").Register("test:fail-heartbeat-clock", func(tx *gorm.DB) {
				if tx.Statement.SQL.String() == databaseNowQuery(tx) {
					tx.AddError(wantErr)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			var errHeartbeat error
			if kind == "home" {
				errHeartbeat = repo.HeartbeatHomeIncarnation(context.Background(), lifetime.Home)
			} else {
				errHeartbeat = repo.RefreshCPALiveness(context.Background(), lifetime)
			}
			if !errors.Is(errHeartbeat, wantErr) {
				t.Fatalf("heartbeat error = %v, want database clock failure", errHeartbeat)
			}
			var afterHome HomeProcessIncarnationRecord
			var afterCPA CPANodeMembershipRecord
			if errFind := repo.db.First(&afterHome, "home_ip = ?", lifetime.Home.IP).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if errFind := repo.db.First(&afterCPA, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if !afterHome.LastSeenAt.Equal(beforeHome.LastSeenAt) || !afterCPA.LastSeenAt.Equal(beforeCPA.LastSeenAt) || !afterCPA.UpdatedAt.Equal(beforeCPA.UpdatedAt) {
				t.Fatal("failed heartbeat changed persisted liveness")
			}
		})
	}
}
