package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

func newPostgresLifecycleFixture(t *testing.T) (*Repository, context.Context, ConnectionLifetime) {
	t.Helper()
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cfg, errConfig := repo.EnsureLifecycleConfig(ctx, DefaultHeartbeatTimeout())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	home, errHome := repo.RegisterHomeIncarnation(ctx, "home-a", 8317, []string{credentialConcurrencyLimitsCapability})
	if errHome != nil {
		t.Fatal(errHome)
	}
	member, errMember := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
		Fingerprint: "fp-a", NodeID: "node-a", Home: home, ProtocolVersion: 1,
		LifecycleConfigRevision: cfg.Revision,
	})
	if errMember != nil {
		t.Fatal(errMember)
	}
	for _, credentialID := range []string{"cred-a", "cred-b"} {
		seedConcurrencyAdmissionAuth(t, repo, credentialID)
		seedConcurrencyAdmissionPolicy(t, repo, credentialID, int64Pointer(5), nil)
	}
	return repo, ctx, ConnectionLifetime{
		Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home, Controlled: true,
	}
}

// holdPostgresAdmission pauses a real admission after all its lifecycle and policy locks are held.
func holdPostgresAdmission(t *testing.T, ctx context.Context, repo *Repository, lifetime ConnectionLifetime) (<-chan postgresBackendPIDResult, func()) {
	t.Helper()
	pid := make(chan postgresBackendPIDResult, 1)
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	holderCtx := context.WithValue(ctx, concurrencyContenderBackendPIDContextKey{}, postgresContenderBackendPIDHook(pid))
	holderCtx = context.WithValue(holderCtx, concurrencyAdmissionPolicyLockAcquiredContextKey{}, func() {
		close(locked)
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	go func() {
		_, errAdmit := repo.AdmitCredentialConcurrency(holderCtx, ConcurrencyAdmissionRequest{
			CredentialID: "cred-a", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1,
		})
		done <- errAdmit
	}()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
			if errAdmit := <-done; errAdmit != nil {
				t.Errorf("held admission: %v", errAdmit)
			}
		})
	}
	t.Cleanup(finish)
	select {
	case <-locked:
	case <-ctx.Done():
		t.Fatal("admission did not acquire locks:", ctx.Err())
	}
	return pid, finish
}

func TestPostgresAdmissionAllowsIndependentCommandsAndHeartbeats(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	var otherMember CPANodeMembershipRecord
	if errFind := repo.db.First(&otherMember, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	otherMember.CertificateFingerprint = "fp-b"
	otherMember.NodeID = "node-b"
	if errCreate := repo.db.Create(&otherMember).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	otherLifetime := lifetime
	otherLifetime.Fingerprint = otherMember.CertificateFingerprint

	_, finish := holdPostgresAdmission(t, ctx, repo, lifetime)
	for _, candidate := range []ConnectionLifetime{lifetime, otherLifetime} {
		if _, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
			CredentialID: "cred-b", Model: "gpt", Lifetime: candidate, ProtocolVersion: 1,
		}); errAdmit != nil {
			t.Fatal("independent admission blocked:", errAdmit)
		}
		if errRelease := repo.ApplyConcurrencyRelease(ctx, ConcurrencyReleaseRequest{
			CredentialID: "cred-b", Model: "gpt", Lifetime: candidate, ReleaseSeq: 1,
		}); errRelease != nil {
			t.Fatal("independent release blocked:", errRelease)
		}
	}
	if errHeartbeat := repo.RefreshCPALiveness(ctx, lifetime); errHeartbeat != nil {
		t.Fatal("CPA heartbeat blocked:", errHeartbeat)
	}
	if errHeartbeat := repo.HeartbeatHomeIncarnation(ctx, lifetime.Home); errHeartbeat != nil {
		t.Fatal("Home heartbeat blocked:", errHeartbeat)
	}
	if errParticipation := repo.RecordParticipation(ctx, lifetime); errParticipation != nil {
		t.Fatal("connection classification blocked:", errParticipation)
	}
	identity := InFlightIngestIdentity{
		CertificateFingerprint: lifetime.Fingerprint, NodeID: "node-a", MembershipConnectedAt: lifetime.ConnectedAt,
	}
	result, errIngest := repo.IngestInFlightFrame(ctx, identity, mustInFlightJSON(t, inFlightPart(1, 0, 1, nil, nil)), DefaultInFlightLimits())
	if errIngest != nil || !result.Published {
		t.Fatalf("independent snapshot = %#v, error = %v", result, errIngest)
	}
	finish()
}

type lifecycleTransitionPIDContextKey struct{}

type lifecycleTransitionPIDObserver struct {
	once    sync.Once
	results chan postgresBackendPIDResult
}

// observeLifecycleTransitionPID records the same backend before a lifecycle statement starts waiting.
func observeLifecycleTransitionPID(t *testing.T, repo *Repository) {
	t.Helper()
	observe := func(tx *gorm.DB) {
		observer, okObserver := tx.Statement.Context.Value(lifecycleTransitionPIDContextKey{}).(*lifecycleTransitionPIDObserver)
		if !okObserver || tx.DryRun {
			return
		}
		observer.once.Do(func() {
			var pid int
			errPID := tx.Statement.ConnPool.QueryRowContext(tx.Statement.Context, "SELECT pg_backend_pid()").Scan(&pid)
			observer.results <- postgresBackendPIDResult{pid: pid, err: errPID}
		})
	}
	if errRegister := repo.db.Callback().Query().Before("gorm:query").Register("test:lifecycle-transition-pid", observe); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := repo.db.Callback().Update().Before("gorm:update").Register("test:lifecycle-transition-pid", observe); errRegister != nil {
		t.Fatal(errRegister)
	}
}

func TestPostgresExpirationRechecksCommittedHeartbeat(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	if errStale := repo.db.Model(&HomeProcessIncarnationRecord{}).
		Where("home_ip = ?", lifetime.Home.IP).Update("last_seen_at", databaseTestTime).Error; errStale != nil {
		t.Fatal(errStale)
	}
	holderPID := make(chan postgresBackendPIDResult, 1)
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if errHeartbeat := tx.Model(&HomeProcessIncarnationRecord{}).
				Where("home_ip = ?", lifetime.Home.IP).Update("last_seen_at", gorm.Expr("clock_timestamp()")).Error; errHeartbeat != nil {
				return errHeartbeat
			}
			if errPID := postgresContenderBackendPIDHook(holderPID)(tx); errPID != nil {
				return errPID
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			close(release)
			if errHeartbeat := <-done; errHeartbeat != nil {
				t.Errorf("held heartbeat: %v", errHeartbeat)
			}
		})
	}
	t.Cleanup(finish)
	select {
	case <-locked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	observeLifecycleTransitionPID(t, repo)
	observer := &lifecycleTransitionPIDObserver{results: make(chan postgresBackendPIDResult, 1)}
	transitionCtx := context.WithValue(ctx, lifecycleTransitionPIDContextKey{}, observer)
	expired := make(chan error, 1)
	go func() {
		expired <- repo.db.WithContext(transitionCtx).Transaction(func(tx *gorm.DB) error {
			return expireStaleHomeIncarnations(transitionCtx, tx, DefaultHeartbeatTimeout())
		})
	}()
	waitForPostgresContendersBlockedByHolder(t, ctx, repo, holderPID, observer.results)
	finish()
	select {
	case errExpire := <-expired:
		if errExpire != nil {
			t.Fatal(errExpire)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var home HomeProcessIncarnationRecord
	if errFind := repo.db.First(&home, "home_ip = ?", lifetime.Home.IP).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if home.State != HomeIncarnationActive {
		t.Fatalf("fresh heartbeat was ignored: Home state = %q", home.State)
	}
}

func TestPostgresLifecycleTransitionsWaitForAdmission(t *testing.T) {
	for _, action := range []string{"fence", "retire", "expire", "cancel", "takeover", "quiescence"} {
		t.Run(action, func(t *testing.T) {
			repo, ctx, lifetime := newPostgresLifecycleFixture(t)
			if action == "expire" || action == "quiescence" {
				if errStale := repo.db.Model(&HomeProcessIncarnationRecord{}).
					Where("home_ip = ?", lifetime.Home.IP).Update("last_seen_at", databaseTestTime).Error; errStale != nil {
					t.Fatal(errStale)
				}
			}
			cfg, errConfig := repo.LifecycleConfig(ctx)
			if errConfig != nil {
				t.Fatal(errConfig)
			}
			holderPID, finish := holdPostgresAdmission(t, ctx, repo, lifetime)
			observeLifecycleTransitionPID(t, repo)
			observer := &lifecycleTransitionPIDObserver{results: make(chan postgresBackendPIDResult, 1)}
			transitionCtx := context.WithValue(ctx, lifecycleTransitionPIDContextKey{}, observer)
			done := make(chan error, 1)
			go func() {
				var errTransition error
				switch action {
				case "fence":
					errTransition = repo.FenceHomeIncarnation(transitionCtx, lifetime.Home, "test")
				case "retire":
					errTransition = repo.RetireHomeIncarnation(transitionCtx, lifetime.Home)
				case "expire":
					errTransition = repo.db.WithContext(transitionCtx).Transaction(func(tx *gorm.DB) error {
						return expireStaleHomeIncarnations(transitionCtx, tx, time.Second)
					})
				case "cancel":
					_, errTransition = repo.BeginFingerprintCancellationForLifetime(transitionCtx, lifetime)
				case "takeover":
					_, errTransition = repo.SubscribeMembership(transitionCtx, SubscribeMembershipRequest{
						Fingerprint: lifetime.Fingerprint, NodeID: "node-a", Home: lifetime.Home,
						ProtocolVersion: 1, LifecycleConfigRevision: cfg.LifecycleConfigRevision, Takeover: true,
					})
				case "quiescence":
					lifecycle, errLifecycle := lifecycleConfigForQuiescence(repo.db.WithContext(ctx))
					if errLifecycle != nil {
						errTransition = errLifecycle
						break
					}
					errTransition = repo.db.WithContext(transitionCtx).Transaction(func(tx *gorm.DB) error {
						now, errNow := DatabaseNow(transitionCtx, tx)
						if errNow != nil {
							return errNow
						}
						return resolveQuiescenceHomeRowTx(transitionCtx, tx, CPANodeQuiescenceRecord{
							HomeIP: lifetime.Home.IP, HomePort: lifetime.Home.Port, HomeStartedAt: lifetime.Home.StartedAt,
						}, lifecycle, now, "test")
					})
				}
				done <- errTransition
			}()
			waitForPostgresContendersBlockedByHolder(t, ctx, repo, holderPID, observer.results)
			finish()
			select {
			case errTransition := <-done:
				if errTransition != nil {
					t.Fatal(errTransition)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, errAdmit := repo.AdmitCredentialConcurrency(ctx, ConcurrencyAdmissionRequest{
				CredentialID: "cred-b", Model: "gpt", Lifetime: lifetime, ProtocolVersion: 1,
			})
			if !errors.Is(errAdmit, ErrConcurrencyNodeUnavailable) {
				t.Fatalf("old lifetime admission = %v, want unavailable", errAdmit)
			}
		})
	}
}
