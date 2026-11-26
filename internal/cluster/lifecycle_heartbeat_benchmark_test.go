package cluster

import (
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Run only against a disposable local PostgreSQL database, via HOME_TEST_POSTGRES_DSN.
// Measure the complete persisted heartbeat on one pooled connection, excluding setup.
func BenchmarkRefreshCPALiveness(b *testing.B) {
	repo := newPostgresQuiescenceTestRepository(b)
	pool, errDB := repo.db.DB()
	if errDB != nil {
		b.Fatal(errDB)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	repo.db = repo.db.Session(&gorm.Session{Logger: logger.Discard})
	ctx := b.Context()
	cfg, errConfig := repo.EnsureLifecycleConfig(ctx, DefaultHeartbeatTimeout())
	if errConfig != nil {
		b.Fatal(errConfig)
	}
	home, errHome := repo.RegisterHomeIncarnation(ctx, "heartbeat-benchmark", 8317, []string{credentialConcurrencyLimitsCapability})
	if errHome != nil {
		b.Fatal(errHome)
	}
	member, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
		Fingerprint: "heartbeat-benchmark", NodeID: "heartbeat-benchmark", Home: home,
		ProtocolVersion: 1, LifecycleConfigRevision: cfg.Revision,
	})
	if errSubscribe != nil {
		b.Fatal(errSubscribe)
	}
	lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if errRefresh := repo.RefreshCPALiveness(ctx, lifetime); errRefresh != nil {
			b.Fatal(errRefresh)
		}
	}
	b.StopTimer()
}
