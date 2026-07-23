package cluster

import (
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestBeginFingerprintCancellationLocksMembershipQuiescenceBeforeHomes(t *testing.T) {
	ctx := context.Background()
	repo, _, member := newQuiescenceMembership(t, ctx, "fp-lock-order")
	participant, errParticipant := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errParticipant != nil {
		t.Fatal(errParticipant)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
		t.Fatal(errParticipation)
	}

	var order []string
	lockCtx := context.WithValue(ctx, quiescenceLockOrderContextKey{}, func(step string) {
		order = append(order, step)
	})
	if _, errBegin := repo.BeginFingerprintCancellation(lockCtx, member.CertificateFingerprint); errBegin != nil {
		t.Fatal(errBegin)
	}
	assertQuiescenceLockOrder(t, order)
}

func TestBeginFingerprintCancellationLockOrderPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx := context.Background()
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	member, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
		Fingerprint: "fp-lock-order-postgres", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision,
	})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	participant, errParticipant := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errParticipant != nil {
		t.Fatal(errParticipant)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
		t.Fatal(errParticipation)
	}

	var order []string
	lockCtx := context.WithValue(ctx, quiescenceLockOrderContextKey{}, func(step string) {
		order = append(order, step)
	})
	if _, errBegin := repo.BeginFingerprintCancellation(lockCtx, member.CertificateFingerprint); errBegin != nil {
		t.Fatal(errBegin)
	}
	assertQuiescenceLockOrder(t, order)
}

func assertQuiescenceLockOrder(t *testing.T, order []string) {
	t.Helper()
	if len(order) < 3 || !slices.Equal(order[:2], []string{"membership", "quiescence"}) {
		t.Fatalf("lock order = %v, want membership then quiescence then Homes", order)
	}
	for _, step := range order[2:] {
		if step != "homes" {
			t.Fatalf("lock order = %v, want Home access only after quiescence", order)
		}
	}
}

func newPostgresQuiescenceRepository(t *testing.T) *Repository {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	}
	if dsn == "" {
		t.Skip("HOME_TEST_POSTGRES_DSN is not configured")
	}
	adminDB, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig())
	if errOpen != nil {
		t.Fatalf("open postgres admin database: %v", errOpen)
	}
	adminSQLDB, errAdminDB := adminDB.DB()
	if errAdminDB != nil {
		t.Fatalf("get postgres admin database: %v", errAdminDB)
	}
	schema := "quiescence_lock_order_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := adminDB.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		t.Fatalf("create postgres schema: %v", errCreate)
	}

	db, errSchemaOpen := gorm.Open(postgres.Open(postgresDSNWithSearchPath(dsn, schema)), databaseGORMConfig())
	if errSchemaOpen != nil {
		if errDrop := adminDB.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres schema after open failure: %v", errDrop)
		}
		if errClose := adminSQLDB.Close(); errClose != nil {
			t.Errorf("close postgres admin database: %v", errClose)
		}
		t.Fatalf("open postgres schema database: %v", errSchemaOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres database: %v", errClose)
		}
		if errDrop := adminDB.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres schema: %v", errDrop)
		}
		if errClose := adminSQLDB.Close(); errClose != nil {
			t.Errorf("close postgres admin database: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	return NewRepository(db)
}

func postgresDSNWithSearchPath(dsn string, schema string) string {
	parsed, errParse := url.Parse(dsn)
	if errParse == nil && parsed.Scheme != "" {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return dsn + " search_path=" + schema
}

func TestQuiescencePostgresConcurrentHeartbeatCancellationAndRecovery(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()

	if _, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig()); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	homeA, errHomeA := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHomeA != nil {
		t.Fatal(errHomeA)
	}
	homeB, errHomeB := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHomeB != nil {
		t.Fatal(errHomeB)
	}
	lifecycle, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}

	members := make([]CPANodeMembershipRecord, 4)
	for index := range members {
		owner := homeA
		participant := homeB
		if index%2 != 0 {
			owner, participant = homeB, homeA
		}
		member, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
			Fingerprint:             "fp-postgres-concurrent-" + strconv.Itoa(index),
			NodeID:                  "cpa-postgres-concurrent-" + strconv.Itoa(index),
			Home:                    owner,
			ProtocolVersion:         1,
			LifecycleConfigRevision: lifecycle.LifecycleConfigRevision,
		})
		if errSubscribe != nil {
			t.Fatal(errSubscribe)
		}
		if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
			t.Fatal(errParticipation)
		}
		members[index] = member
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	var runCanceled atomic.Bool
	cancelWorkers := func() {
		runCanceled.Store(true)
		cancelRun()
	}
	defer cancelWorkers()
	errs := make(chan error, 32)
	var workers sync.WaitGroup
	workers.Add(2)
	for _, home := range []HomeIncarnationID{homeA, homeB} {
		home := home
		go func() {
			defer workers.Done()
			for runCtx.Err() == nil {
				if errHeartbeat := repo.HeartbeatHomeIncarnation(runCtx, home); errHeartbeat != nil {
					if isExpectedRunCancellationError(runCtx, runCanceled.Load(), errHeartbeat) {
						return
					}
					errs <- errHeartbeat
					return
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for runCtx.Err() == nil {
			if errRecover := repo.RecoverStaleQuiescence(runCtx); errRecover != nil {
				if isExpectedRunCancellationError(runCtx, runCanceled.Load(), errRecover) {
					return
				}
				errs <- errRecover
				return
			}
		}
	}()

	type cancellation struct {
		member   CPANodeMembershipRecord
		revision int64
	}
	cancellations := make(chan cancellation, len(members))
	for _, member := range members {
		member := member
		workers.Add(1)
		go func() {
			defer workers.Done()
			revision, errBegin := repo.BeginFingerprintCancellation(runCtx, member.CertificateFingerprint)
			if errBegin != nil {
				if isExpectedRunCancellationError(runCtx, runCanceled.Load(), errBegin) {
					return
				}
				errs <- errBegin
				return
			}
			cancellations <- cancellation{member: member, revision: revision}
		}()
	}
	for range members {
		select {
		case cancellation := <-cancellations:
			for _, home := range []HomeIncarnationID{homeA, homeB} {
				if errAcknowledge := repo.AcknowledgeQuiescence(ctx, cancellation.member.CertificateFingerprint, cancellation.member.ConnectedAt, cancellation.revision, home); errAcknowledge != nil {
					cancelWorkers()
					workers.Wait()
					t.Fatal(errAcknowledge)
				}
			}
		case errWorker := <-errs:
			cancelWorkers()
			workers.Wait()
			t.Fatal(errWorker)
		case <-ctx.Done():
			cancelWorkers()
			workers.Wait()
			t.Fatal(ctx.Err())
		}
	}
	cancelWorkers()
	workers.Wait()
	select {
	case errWorker := <-errs:
		t.Fatal(errWorker)
	default:
	}

	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	for _, member := range members {
		var got CPANodeMembershipRecord
		if errFind := repo.db.First(&got, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
			t.Fatal(errFind)
		}
		if got.State != MembershipStateClosed {
			t.Fatalf("membership %s state = %q, want %q", member.CertificateFingerprint, got.State, MembershipStateClosed)
		}
		var rows []CPANodeQuiescenceRecord
		if errRows := repo.db.Where("certificate_fingerprint = ?", member.CertificateFingerprint).Order("home_ip").Find(&rows).Error; errRows != nil {
			t.Fatal(errRows)
		}
		if len(rows) != 2 {
			t.Fatalf("membership %s quiescence rows = %d, want 2", member.CertificateFingerprint, len(rows))
		}
		for _, row := range rows {
			if row.Status != QuiescenceStatusAcknowledged {
				t.Fatalf("membership %s Home %s:%d status = %q, want %q", member.CertificateFingerprint, row.HomeIP, row.HomePort, row.Status, QuiescenceStatusAcknowledged)
			}
		}
	}
	for _, home := range []HomeIncarnationID{homeA, homeB} {
		var got HomeProcessIncarnationRecord
		if errFind := repo.db.First(&got, "home_ip = ? AND home_port = ? AND started_at = ?", home.IP, home.Port, home.StartedAt).Error; errFind != nil {
			t.Fatal(errFind)
		}
		if got.State != HomeIncarnationActive {
			t.Fatalf("Home %s:%d state = %q, want %q", home.IP, home.Port, got.State, HomeIncarnationActive)
		}
	}
}

func TestQuiescenceWorkerIgnoresOnlyExpectedRunCancellationErrors(t *testing.T) {
	canceledCtx, cancelCtx := context.WithCancel(context.Background())
	cancelCtx()

	tests := []struct {
		name        string
		ctx         context.Context
		canceledRun bool
		err         error
		ignore      bool
	}{
		{name: "canceled run context", ctx: canceledCtx, canceledRun: true, err: context.Canceled, ignore: true},
		{name: "deadline error after canceled run context", ctx: canceledCtx, canceledRun: true, err: context.DeadlineExceeded, ignore: true},
		{name: "canceled error before run cancellation", ctx: canceledCtx, err: context.Canceled, ignore: false},
		{name: "unexpected error after run cancellation", ctx: canceledCtx, canceledRun: true, err: errors.New("deadlock detected"), ignore: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isExpectedRunCancellationError(test.ctx, test.canceledRun, test.err); got != test.ignore {
				t.Fatalf("isExpectedRunCancellationError() = %t, want %t", got, test.ignore)
			}
		})
	}
}

func isExpectedRunCancellationError(runCtx context.Context, canceledRun bool, err error) bool {
	return canceledRun && runCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

func TestQuiescenceRejectsZeroExpectedLifetime(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-zero-lifetime")
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, time.Time{}, revision, home); !errors.Is(errAck, ErrQuiescenceRevisionMismatch) {
		t.Fatalf("acknowledge error = %v, want %v", errAck, ErrQuiescenceRevisionMismatch)
	}
	if errComplete := repo.CompleteFingerprintCancellation(ctx, member.CertificateFingerprint, time.Time{}, revision); !errors.Is(errComplete, ErrQuiescenceRevisionMismatch) {
		t.Fatalf("complete error = %v, want %v", errComplete, ErrQuiescenceRevisionMismatch)
	}
}

func TestQuiescenceRejectsPriorRevisionAcknowledgment(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-prior-revision")
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision-1, home); !errors.Is(errAck, ErrQuiescenceRevisionMismatch) {
		t.Fatalf("acknowledge error = %v, want %v", errAck, ErrQuiescenceRevisionMismatch)
	}
}

func TestQuiescenceRejectsPriorLifetimeAcknowledgment(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-prior-lifetime")
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", member.CertificateFingerprint).Update("connected_at", member.ConnectedAt.Add(time.Second)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, home); !errors.Is(errAck, ErrQuiescenceRevisionMismatch) {
		t.Fatalf("acknowledge error = %v, want %v", errAck, ErrQuiescenceRevisionMismatch)
	}
}

func TestQuiescenceCompletionRejectsPriorLifetime(t *testing.T) {
	ctx := context.Background()
	repo, _, member := newQuiescenceMembership(t, ctx, "fp-complete-prior-lifetime")
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	errComplete := repo.CompleteFingerprintCancellation(ctx, member.CertificateFingerprint, member.ConnectedAt.Add(time.Second), revision)
	if !errors.Is(errComplete, ErrQuiescenceRevisionMismatch) {
		t.Fatalf("complete error = %v, want %v", errComplete, ErrQuiescenceRevisionMismatch)
	}
}

func TestQuiescenceRejectsMissingExpectedRows(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-missing-rows")
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errDelete := repo.db.Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ?", member.CertificateFingerprint, member.ConnectedAt, revision).Delete(&CPANodeQuiescenceRecord{}).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	if errComplete := repo.CompleteFingerprintCancellation(ctx, member.CertificateFingerprint, member.ConnectedAt, revision); !errors.Is(errComplete, ErrFingerprintQuiescenceSetIncomplete) {
		t.Fatalf("complete error = %v, want %v", errComplete, ErrFingerprintQuiescenceSetIncomplete)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, home); !errors.Is(errAck, ErrQuiescenceRevisionMismatch) {
		t.Fatalf("acknowledge deleted row error = %v, want %v", errAck, ErrQuiescenceRevisionMismatch)
	}
}

func TestRecoverStaleQuiescenceCompletesFinalAcknowledgedCancellation(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-recovery-final-ack")
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, home); errAck != nil {
		t.Fatal(errAck)
	}
	pending, errPending := repo.ListPendingQuiescence(ctx, home)
	if errPending != nil {
		t.Fatal(errPending)
	}
	if len(pending) != 0 {
		t.Fatalf("pending quiescence rows = %d, want 0 after final acknowledgment", len(pending))
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	var got CPANodeMembershipRecord
	if errFind := repo.db.First(&got, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if got.State != MembershipStateClosed {
		t.Fatalf("membership state = %q, want %q", got.State, MembershipStateClosed)
	}
}

func TestQuiescenceFencesExpiredHomeIncarnation(t *testing.T) {
	ctx := context.Background()
	repo, owner, member := newQuiescenceMembership(t, ctx, "fp-expired-home")
	expired, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: expired}); errParticipation != nil {
		t.Fatal(errParticipation)
	}
	if errExpire := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", expired.IP, expired.Port, expired.StartedAt).Updates(map[string]any{"state": HomeIncarnationExpired, "last_seen_at": time.Unix(0, 0).UTC()}).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	var row CPANodeQuiescenceRecord
	if errFind := repo.db.Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", member.CertificateFingerprint, member.ConnectedAt, revision, expired.IP, expired.Port, expired.StartedAt).First(&row).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if row.Status != QuiescenceStatusFenced {
		t.Fatalf("expired home quiescence status = %q, want %q", row.Status, QuiescenceStatusFenced)
	}
	var record HomeProcessIncarnationRecord
	if errFind := repo.db.First(&record, "home_ip = ? AND home_port = ? AND started_at = ?", expired.IP, expired.Port, expired.StartedAt).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if record.State != HomeIncarnationFenced {
		t.Fatalf("expired Home state = %q, want %q (owner=%#v)", record.State, HomeIncarnationFenced, owner)
	}
}

func TestQuiescenceFencedHomeHonorsReclaimGraceBeforeCompletion(t *testing.T) {
	ctx := context.Background()
	repo, owner, member := newQuiescenceMembership(t, ctx, "fp-fenced-home-grace")
	participant, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
		t.Fatal(errParticipation)
	}
	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", participant.IP, participant.Port, participant.StartedAt).Updates(map[string]any{
		"state": HomeIncarnationFenced, "last_seen_at": time.Now().UTC(),
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}

	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	assertQuiescenceStatus(t, repo, member, revision, participant, QuiescenceStatusPending)
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, owner); errAck != nil {
		t.Fatal(errAck)
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	assertMembershipState(t, repo, member.CertificateFingerprint, MembershipStateCanceling)
	assertQuiescenceStatus(t, repo, member, revision, participant, QuiescenceStatusPending)

	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", participant.IP, participant.Port, participant.StartedAt).Update("last_seen_at", time.Unix(0, 0).UTC()).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	assertMembershipState(t, repo, member.CertificateFingerprint, MembershipStateClosed)
	assertQuiescenceStatus(t, repo, member, revision, participant, QuiescenceStatusFenced)
}

func TestQuiescenceEligibilityDoesNotLockRecentHomeAndFencesPostGrace(t *testing.T) {
	ctx := context.Background()
	repo, owner, member := newQuiescenceMembership(t, ctx, "fp-eligibility-nonlocking")
	participant, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
		t.Fatal(errParticipation)
	}
	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", participant.IP, participant.Port, participant.StartedAt).Updates(map[string]any{
		"state": HomeIncarnationFenced, "last_seen_at": time.Now().UTC(),
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}

	var homeLockQueries atomic.Int64
	if errCallback := repo.db.Callback().Query().After("gorm:query").Register("quiescence:record-home-eligibility-lock", func(tx *gorm.DB) {
		if tx.Statement.Table != (HomeProcessIncarnationRecord{}).TableName() || !statementContainsString(tx.Statement.Vars, participant.IP) {
			return
		}
		if _, locked := tx.Statement.Clauses["FOR"]; locked {
			homeLockQueries.Add(1)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, owner); errAck != nil {
		t.Fatal(errAck)
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	if got := homeLockQueries.Load(); got != 0 {
		t.Fatalf("recent Home eligibility FOR UPDATE queries = %d, want 0", got)
	}
	assertQuiescenceStatus(t, repo, member, revision, participant, QuiescenceStatusPending)

	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", participant.IP, participant.Port, participant.StartedAt).Update("last_seen_at", time.Unix(0, 0).UTC()).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	if got := homeLockQueries.Load(); got != 0 {
		t.Fatalf("post-grace Home eligibility FOR UPDATE queries = %d, want 0", got)
	}
	assertMembershipState(t, repo, member.CertificateFingerprint, MembershipStateClosed)
	assertQuiescenceStatus(t, repo, member, revision, participant, QuiescenceStatusFenced)
}

func statementContainsString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertQuiescenceStatus(t *testing.T, repo *Repository, member CPANodeMembershipRecord, revision int64, home HomeIncarnationID, want string) {
	t.Helper()
	var row CPANodeQuiescenceRecord
	if errFind := repo.db.Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", member.CertificateFingerprint, member.ConnectedAt, revision, home.IP, home.Port, home.StartedAt).First(&row).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if row.Status != want {
		t.Fatalf("Home quiescence status = %q, want %q", row.Status, want)
	}
}

func assertMembershipState(t *testing.T, repo *Repository, fingerprint string, want string) {
	t.Helper()
	var membership CPANodeMembershipRecord
	if errFind := repo.db.First(&membership, "certificate_fingerprint = ?", fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if membership.State != want {
		t.Fatalf("membership state = %q, want %q", membership.State, want)
	}
}

func TestRefreshCPALivenessRejectsFencedHomeIncarnation(t *testing.T) {
	ctx := context.Background()
	repo, home, member := newQuiescenceMembership(t, ctx, "fp-liveness-fenced")
	coordinator := NewCoordinator(repo, NodeIdentity{IP: home.IP, Port: home.Port}, CoordinatorOptions{})
	coordinator.mu.Lock()
	coordinator.homeIncarnation = home
	coordinator.initialized = true
	coordinator.mu.Unlock()
	handler := NewRESPHandler(coordinator, nil, repo)
	if errFence := repo.FenceHomeIncarnation(ctx, home, "test"); errFence != nil {
		t.Fatal(errFence)
	}
	lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home, Subscription: true}
	if errRefresh := handler.RefreshCPALiveness(ctx, lifetime); !errors.Is(errRefresh, ErrHomeIncarnationFenced) {
		t.Fatalf("refresh error = %v, want %v", errRefresh, ErrHomeIncarnationFenced)
	}
}

func newQuiescenceMembership(t *testing.T, ctx context.Context, fingerprint string) (*Repository, HomeIncarnationID, CPANodeMembershipRecord) {
	t.Helper()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errConfig := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	member, errMember := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
		Fingerprint: fingerprint, NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision,
	})
	if errMember != nil {
		t.Fatal(errMember)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home}); errParticipation != nil {
		t.Fatal(errParticipation)
	}
	return repo, home, member
}
