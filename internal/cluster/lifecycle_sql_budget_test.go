package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type lifecycleSQLCounter struct {
	gormlogger.Interface
	statements atomic.Int64
}

func (l *lifecycleSQLCounter) Trace(ctx context.Context, begin time.Time, query func() (string, int64), err error) {
	l.statements.Add(1)
	l.Interface.Trace(ctx, begin, query, err)
}

func TestLifecycleHotPathSQLBudgets(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	counter := &lifecycleSQLCounter{Interface: repo.db.Logger}
	repo.db = repo.db.Session(&gorm.Session{Logger: counter})
	identity := InFlightIngestIdentity{
		CertificateFingerprint: lifetime.Fingerprint, NodeID: "node-a", MembershipConnectedAt: lifetime.ConnectedAt,
	}
	for _, revision := range []int64{1, 2} {
		counter.statements.Store(0)
		result, errIngest := repo.IngestInFlightFrame(ctx, identity, mustInFlightJSON(t, inFlightPart(revision, 0, 1, nil, nil)), DefaultInFlightLimits())
		if errIngest != nil || !result.Published {
			t.Fatalf("revision %d result = %#v, error = %v", revision, result, errIngest)
		}
		if count := counter.statements.Load(); count > 8 {
			t.Fatalf("revision %d SQL statements = %d, want at most 8", revision, count)
		} else {
			t.Logf("single-part revision %d: %d business SQL statements", revision, count)
		}
	}
	counter.statements.Store(0)
	if errHeartbeat := repo.RefreshCPALiveness(ctx, lifetime); errHeartbeat != nil {
		t.Fatal(errHeartbeat)
	}
	if count := counter.statements.Load(); count != 3 {
		t.Fatalf("CPA heartbeat SQL statements = %d, want 3", count)
	}
	counter.statements.Store(0)
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, lifetime.Home); errHeartbeat != nil {
		t.Fatal(errHeartbeat)
	}
	if count := counter.statements.Load(); count != 2 {
		t.Fatalf("Home heartbeat SQL statements = %d, want 2", count)
	}
}
