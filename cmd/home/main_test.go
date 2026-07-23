package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver"
)

type recordingInFlightConfigApplier struct {
	current             config.CredentialInFlightConfig
	calls               int
	runtime             *home.Runtime
	runtimeConfigAtCall *config.Config
}

func (a *recordingInFlightConfigApplier) ApplyInFlightConfig(cfg config.CredentialInFlightConfig) error {
	if a.runtime != nil {
		a.runtimeConfigAtCall = a.runtime.Config()
	}
	a.current = cfg
	a.calls++
	return nil
}

func TestApplyConfigEventSynchronizesRESPInFlightLimitsWhenRuntimeConfigMatches(t *testing.T) {
	tests := []struct {
		name      string
		oldConfig config.CredentialInFlightConfig
		newConfig config.CredentialInFlightConfig
	}{
		{
			name:      "tighten",
			oldConfig: config.DefaultCredentialInFlightConfig(),
			newConfig: func() config.CredentialInFlightConfig {
				cfg := config.DefaultCredentialInFlightConfig()
				cfg.MaxDetails = 7
				cfg.MaxStringBytes = 128
				return cfg
			}(),
		},
		{
			name: "relax",
			oldConfig: func() config.CredentialInFlightConfig {
				cfg := config.DefaultCredentialInFlightConfig()
				cfg.MaxDetails = 7
				cfg.MaxStringBytes = 128
				return cfg
			}(),
			newConfig: config.DefaultCredentialInFlightConfig(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimeConfig := config.Config{CredentialInFlight: test.newConfig}
			runtime, errNewRuntime := home.NewRuntime(&runtimeConfig)
			if errNewRuntime != nil {
				t.Fatalf("NewRuntime() error = %v", errNewRuntime)
			}
			nextConfig := config.Config{CredentialInFlight: test.newConfig}
			if !reflect.DeepEqual(runtime.Config(), &nextConfig) {
				t.Fatal("Runtime config must already equal the event config")
			}

			resp := &recordingInFlightConfigApplier{current: test.oldConfig, runtime: runtime}
			if reflect.DeepEqual(resp.current, nextConfig.CredentialInFlight) {
				t.Fatal("RESP limits must begin stale")
			}
			if errApply := applyConfigEvent(context.Background(), runtime, resp, &nextConfig, nil); errApply != nil {
				t.Fatalf("applyConfigEvent() error = %v", errApply)
			}
			if resp.calls != 1 || !reflect.DeepEqual(resp.current, nextConfig.CredentialInFlight) {
				t.Fatalf("RESP in-flight config = %#v after %d calls, want %#v after one call", resp.current, resp.calls, nextConfig.CredentialInFlight)
			}
			if !reflect.DeepEqual(runtime.Config(), &nextConfig) {
				t.Fatal("Runtime config changed despite matching the event config")
			}
		})
	}
}

func TestApplyConfigEventAppliesRuntimeBeforeSynchronizingRESPInFlightLimits(t *testing.T) {
	oldConfig := config.Config{CredentialInFlight: config.DefaultCredentialInFlightConfig()}
	runtime, errNewRuntime := home.NewRuntime(&oldConfig)
	if errNewRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errNewRuntime)
	}

	nextConfig := oldConfig
	nextConfig.CredentialInFlight.MaxDetails = 7
	resp := &recordingInFlightConfigApplier{current: oldConfig.CredentialInFlight, runtime: runtime}
	var published bool
	unsubscribe := runtime.SubscribeConfigYAML(func(payload []byte) error {
		if resp.calls != 1 {
			t.Errorf("RESP synchronization calls = %d when config was published, want 1", resp.calls)
		}
		published = true
		return nil
	})
	defer unsubscribe()

	payload := []byte("credential-in-flight:\n  max-details: 7\n")
	if errApply := applyConfigEvent(context.Background(), runtime, resp, &nextConfig, payload); errApply != nil {
		t.Fatalf("applyConfigEvent() error = %v", errApply)
	}
	if !reflect.DeepEqual(resp.runtimeConfigAtCall, &nextConfig) {
		t.Fatalf("Runtime config during RESP synchronization = %#v, want %#v", resp.runtimeConfigAtCall, &nextConfig)
	}
	if !reflect.DeepEqual(resp.current, nextConfig.CredentialInFlight) {
		t.Fatalf("RESP in-flight config = %#v, want %#v", resp.current, nextConfig.CredentialInFlight)
	}
	if !published {
		t.Fatal("config event was not published")
	}
}

func TestApplyConfigEventDoesNotSynchronizeRESPInFlightLimitsWhenRuntimeApplyFails(t *testing.T) {
	oldConfig := config.Config{CredentialInFlight: config.DefaultCredentialInFlightConfig()}
	runtime, errNewRuntime := home.NewRuntime(&oldConfig)
	if errNewRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errNewRuntime)
	}

	invalidAuthDir := filepath.Join(t.TempDir(), "auth-file")
	if errWrite := os.WriteFile(invalidAuthDir, nil, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	nextConfig := oldConfig
	nextConfig.AuthDir = invalidAuthDir
	nextConfig.CredentialInFlight.MaxDetails = 7
	resp := &recordingInFlightConfigApplier{current: oldConfig.CredentialInFlight, runtime: runtime}
	var published bool
	unsubscribe := runtime.SubscribeConfigYAML(func([]byte) error {
		published = true
		return nil
	})
	defer unsubscribe()

	if errApply := applyConfigEvent(context.Background(), runtime, resp, &nextConfig, []byte("credential-in-flight:\n  max-details: 7\n")); errApply == nil {
		t.Fatal("applyConfigEvent() error = nil, want runtime apply error")
	}
	if resp.calls != 0 {
		t.Fatalf("RESP synchronization calls = %d, want 0", resp.calls)
	}
	if !reflect.DeepEqual(resp.current, oldConfig.CredentialInFlight) {
		t.Fatalf("RESP in-flight config = %#v, want %#v", resp.current, oldConfig.CredentialInFlight)
	}
	if !reflect.DeepEqual(runtime.Config(), &oldConfig) {
		t.Fatalf("Runtime config = %#v, want %#v", runtime.Config(), &oldConfig)
	}
	if published {
		t.Fatal("config event was published after runtime apply failure")
	}
}

func TestRunExportUsesConfiguredHeartbeatTimeout(t *testing.T) {
	const heartbeatTimeout = 30 * time.Second

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "home.db")
	exportDir := filepath.Join(dir, "export")
	if errWrite := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte("sqlite:\n  path: home.db\nnode:\n  external-ip: 127.0.0.1\n  port: 8327\n  heartbeat-timeout: 30s\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	db, errOpen := cluster.OpenSQLite(context.Background(), dbPath)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	if _, errEnsure := repo.EnsureLifecycleConfig(context.Background(), heartbeatTimeout); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	if errUpsert := repo.UpsertConfigValue(context.Background(), "port", 8327); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if errClose := sqlDB.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	t.Chdir(dir)
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	os.Args = []string{"home", "-export", "-sqlite-path", dbPath, "-export-dir", exportDir}
	flag.CommandLine = flag.NewFlagSet("home", flag.ContinueOnError)
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	}()

	if exitCode := run(); exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}
	if _, errStat := os.Stat(filepath.Join(exportDir, "config.yaml")); errStat != nil {
		t.Fatalf("exported config: %v", errStat)
	}
}

func TestRunInitializesCoordinatorBeforeListen(t *testing.T) {
	db, errOpen := cluster.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	coordinator := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.1", Port: 8317}, cluster.CoordinatorOptions{
		HeartbeatTimeout: 20 * time.Second,
	})

	if errInitialize := initializeCoordinatorBeforeListen(context.Background(), coordinator); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	var count int64
	if errCount := db.Model(&cluster.HomeProcessIncarnationRecord{}).Where("state = ?", cluster.HomeIncarnationActive).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if count != 1 {
		t.Fatalf("active Home incarnations = %d, want 1", count)
	}
}

func TestRecoverCPAFencesUsesStartupIncarnationBeforeCoordinatorInitialization(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() { _ = sqlDB.Close() }()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errLifecycle := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	member, errMember := repo.SubscribeMembership(ctx, cluster.SubscribeMembershipRequest{Fingerprint: "fp-recovery", NodeID: "cpa-recovery", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errMember != nil {
		t.Fatal(errMember)
	}
	if _, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint); errBegin != nil {
		t.Fatal(errBegin)
	}
	if errRecover := recoverCPAFences(ctx, repo, home, respserver.New("", nil)); errRecover != nil {
		t.Fatal(errRecover)
	}
	var got cluster.CPANodeMembershipRecord
	if errFind := db.First(&got, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if got.State != cluster.MembershipStateClosed {
		t.Fatalf("membership state = %q, want %q", got.State, cluster.MembershipStateClosed)
	}
}

func TestCPAFenceReplayDoesNotAcknowledgeReusedCancellationRevision(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	lifecycleRevision, errLifecycle := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	request := cluster.SubscribeMembershipRequest{Fingerprint: "fp-reused-cancel-revision", NodeID: "cpa-reuse", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: lifecycleRevision}
	first, errSubscribe := repo.SubscribeMembership(ctx, request)
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	firstRevision, errBegin := repo.BeginFingerprintCancellation(ctx, first.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, first.CertificateFingerprint, first.ConnectedAt, firstRevision, home); errAck != nil {
		t.Fatal(errAck)
	}
	if errComplete := repo.CompleteFingerprintCancellation(ctx, first.CertificateFingerprint, first.ConnectedAt, firstRevision); errComplete != nil {
		t.Fatal(errComplete)
	}

	second, errSubscribe := repo.SubscribeMembership(ctx, request)
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	if second.ConnectedAt.Equal(first.ConnectedAt) {
		t.Fatalf("reused membership connected_at = %v", second.ConnectedAt)
	}
	secondRevision, errBegin := repo.BeginFingerprintCancellation(ctx, second.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if secondRevision != firstRevision {
		t.Fatalf("reused cancellation revision = %d, want %d", secondRevision, firstRevision)
	}

	oldEvent := cluster.ClusterEventRecord{
		Scope:      "cpa-fence",
		Op:         first.ConnectedAt.UTC().Format(time.RFC3339Nano),
		EntityUUID: first.CertificateFingerprint,
		Version:    firstRevision,
	}
	if errFence := handleCPAFence(ctx, oldEvent, repo, home, respserver.New("", nil)); errFence != nil {
		t.Fatal(errFence)
	}

	var got cluster.CPANodeMembershipRecord
	if errFind := db.First(&got, "certificate_fingerprint = ?", second.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if got.State != cluster.MembershipStateCanceling || !got.ConnectedAt.Equal(second.ConnectedAt) {
		t.Fatalf("membership after old event = state %q connected_at %v, want canceling lifetime %v", got.State, got.ConnectedAt, second.ConnectedAt)
	}
	var row cluster.CPANodeQuiescenceRecord
	if errFind := db.Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ?", second.CertificateFingerprint, second.ConnectedAt, secondRevision).First(&row).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if row.Status != cluster.QuiescenceStatusPending {
		t.Fatalf("new lifetime quiescence status = %q, want %q", row.Status, cluster.QuiescenceStatusPending)
	}
}

type concurrencyAdmitterSpy struct {
	request cluster.ConcurrencyAdmissionRequest
	calls   int
}

func (s *concurrencyAdmitterSpy) AdmitCredentialConcurrency(_ context.Context, request cluster.ConcurrencyAdmissionRequest) (cluster.ConcurrencyAdmissionResult, error) {
	s.request = request
	s.calls++
	return cluster.ConcurrencyAdmissionResult{}, nil
}

func TestRuntimeConcurrencyAdmitterUsesCurrentHomeIdentity(t *testing.T) {
	spy := &concurrencyAdmitterSpy{}
	homeID := cluster.HomeIncarnationID{IP: "127.0.0.1", Port: 8317, StartedAt: time.Now().UTC()}
	admitter := newRuntimeConcurrencyAdmitter(spy, func() (cluster.HomeIncarnationID, bool) { return homeID, true })
	if _, errAdmit := admitter.AdmitCredentialConcurrency(context.Background(), home.ConcurrencyAdmissionRequest{CredentialID: "cred", Model: "gpt", Fingerprint: "fp", ConnectedAt: time.Now().UTC(), Controlled: true, ProtocolVersion: 1}); errAdmit != nil {
		t.Fatal(errAdmit)
	}
	if spy.calls != 1 || spy.request.Lifetime.Home != homeID {
		t.Fatalf("request = %#v", spy.request)
	}
}

func TestRuntimeConcurrencyAdmitterRejectsUnavailableHomeIdentity(t *testing.T) {
	spy := &concurrencyAdmitterSpy{}
	admitter := newRuntimeConcurrencyAdmitter(spy, func() (cluster.HomeIncarnationID, bool) { return cluster.HomeIncarnationID{}, false })
	_, errAdmit := admitter.AdmitCredentialConcurrency(context.Background(), home.ConcurrencyAdmissionRequest{})
	if !errors.Is(errAdmit, cluster.ErrConcurrencyNodeUnavailable) || spy.calls != 0 {
		t.Fatalf("error = %v, calls = %d", errAdmit, spy.calls)
	}
}

func TestRecoverCPAFencesFencesStalePriorHomeIncarnation(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	oldHome, errOldHome := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errOldHome != nil {
		t.Fatal(errOldHome)
	}
	revision, errLifecycle := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	member, errMember := repo.SubscribeMembership(ctx, cluster.SubscribeMembershipRequest{Fingerprint: "fp-recovery-crash", NodeID: "cpa-recovery", Home: oldHome, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errMember != nil {
		t.Fatal(errMember)
	}
	cancelRevision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errStale := db.Model(&cluster.HomeProcessIncarnationRecord{}).
		Where("home_ip = ? AND home_port = ? AND started_at = ?", oldHome.IP, oldHome.Port, oldHome.StartedAt).
		Update("last_seen_at", time.Now().UTC().Add(-2*cluster.DefaultHeartbeatTimeout())).Error; errStale != nil {
		t.Fatal(errStale)
	}
	newHome, errNewHome := repo.RegisterHomeIncarnation(ctx, oldHome.IP, oldHome.Port, []string{"credential_concurrency_foundation_v1"})
	if errNewHome != nil {
		t.Fatal(errNewHome)
	}
	if newHome.StartedAt.Equal(oldHome.StartedAt) {
		t.Fatalf("restart reused started_at %v", newHome.StartedAt)
	}

	if errRecover := recoverCPAFences(ctx, repo, newHome, respserver.New("", nil)); errRecover != nil {
		t.Fatal(errRecover)
	}

	var got cluster.CPANodeMembershipRecord
	if errFind := db.First(&got, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if got.State != cluster.MembershipStateClosed {
		t.Fatalf("membership state = %q, want %q", got.State, cluster.MembershipStateClosed)
	}
	var row cluster.CPANodeQuiescenceRecord
	if errFind := db.Where("certificate_fingerprint = ? AND membership_connected_at = ? AND cancel_revision = ? AND home_started_at = ?", member.CertificateFingerprint, member.ConnectedAt, cancelRevision, oldHome.StartedAt).First(&row).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if row.Status != cluster.QuiescenceStatusFenced {
		t.Fatalf("stale quiescence status = %q, want %q", row.Status, cluster.QuiescenceStatusFenced)
	}
}

func TestLoadInitialRuntimeConfigReadsEventHighWaterBeforeSnapshot(t *testing.T) {
	wantConfig := &config.Config{Debug: true}
	repo := &startupConfigRepositoryStub{cfg: wantConfig, maxEventID: 41}

	highWater, gotConfig, errLoad := loadInitialRuntimeConfig(context.Background(), repo)
	if errLoad != nil {
		t.Fatalf("loadInitialRuntimeConfig() error = %v", errLoad)
	}
	if highWater != 41 {
		t.Fatalf("high-water = %d, want 41", highWater)
	}
	if gotConfig != wantConfig {
		t.Fatalf("config = %p, want %p", gotConfig, wantConfig)
	}
	wantActions := []string{"max-event-id", "load-config"}
	if strings.Join(repo.actions, ",") != strings.Join(wantActions, ",") {
		t.Fatalf("repository actions = %#v, want %#v", repo.actions, wantActions)
	}
}

type startupConfigRepositoryStub struct {
	cfg        *config.Config
	payload    []byte
	maxEventID int64
	actions    []string
}

func (s *startupConfigRepositoryStub) LoadConfigAsRuntimeConfig(context.Context) (*config.Config, []byte, error) {
	s.actions = append(s.actions, "load-config")
	return s.cfg, append([]byte(nil), s.payload...), nil
}

func (s *startupConfigRepositoryStub) MaxEventID(context.Context) (int64, error) {
	s.actions = append(s.actions, "max-event-id")
	return s.maxEventID, nil
}

func TestResolveClusterAdvertisedPortUsesExternalPort(t *testing.T) {
	t.Parallel()

	cfg := &cluster.Config{
		Node: cluster.NodeConfig{
			ExternalPort: 443,
			Port:         8327,
		},
	}

	port, errPort := resolveClusterAdvertisedPort(cfg, 8327)
	if errPort != nil {
		t.Fatalf("resolveClusterAdvertisedPort failed: %v", errPort)
	}
	if port != 443 {
		t.Fatalf("advertised port = %d, want 443", port)
	}
}

func TestResolveClusterAdvertisedPortFallsBackToListenPort(t *testing.T) {
	t.Parallel()

	cfg := &cluster.Config{
		Node: cluster.NodeConfig{
			Port: 8327,
		},
	}

	port, errPort := resolveClusterAdvertisedPort(cfg, 18327)
	if errPort != nil {
		t.Fatalf("resolveClusterAdvertisedPort failed: %v", errPort)
	}
	if port != 18327 {
		t.Fatalf("advertised port = %d, want 18327", port)
	}
}

func TestResolveSQLitePath_UsesFlagOverride(t *testing.T) {
	t.Parallel()

	got := resolveSQLitePath("flag.db", "config.db")
	if got != "flag.db" {
		t.Fatalf("resolveSQLitePath() = %q, want flag.db", got)
	}
}

func TestResolveSQLitePath_UsesConfigFallback(t *testing.T) {
	t.Parallel()

	got := resolveSQLitePath("", "config.db")
	if got != "config.db" {
		t.Fatalf("resolveSQLitePath() = %q, want config.db", got)
	}
}

func TestResolveSQLitePath_UsesDefault(t *testing.T) {
	t.Parallel()

	got := resolveSQLitePath("", "")
	if got != "home.db" {
		t.Fatalf("resolveSQLitePath() = %q, want home.db", got)
	}
}

func TestDatabaseSnapshotOneTimeModeCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		importState        bool
		exportState        bool
		databaseImportPath string
		databaseExportPath string
		want               int
	}{
		{name: "none", want: 0},
		{name: "local import", importState: true, want: 1},
		{name: "local export", exportState: true, want: 1},
		{name: "database import", databaseImportPath: "snapshot.zip", want: 1},
		{name: "database export", databaseExportPath: "snapshot.zip", want: 1},
		{name: "all", importState: true, exportState: true, databaseImportPath: "in.zip", databaseExportPath: "out.zip", want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := oneTimeModeCount(test.importState, test.exportState, test.databaseImportPath, test.databaseExportPath)
			if got != test.want {
				t.Fatalf("oneTimeModeCount() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDatabaseSnapshotResolveRuntimeDatabaseBackend(t *testing.T) {
	t.Parallel()

	if got := resolveRuntimeDatabaseBackend(nil, false); got != cluster.DatabaseBackendSQLite {
		t.Fatalf("non-cluster backend = %q, want sqlite", got)
	}
	postgresConfig := &cluster.Config{PGSQL: cluster.PGSQLConfig{Host: "postgres"}}
	if got := resolveRuntimeDatabaseBackend(postgresConfig, true); got != cluster.DatabaseBackendPostgres {
		t.Fatalf("cluster backend = %q, want postgres", got)
	}
}

func TestExportOptionsForDir_UsesConfiguredHeartbeatTimeout(t *testing.T) {
	t.Parallel()

	const heartbeatTimeout = 30 * time.Second
	opts := exportOptionsForDir("out", nil, heartbeatTimeout)
	if opts.NodeHeartbeatTimeout != heartbeatTimeout {
		t.Fatalf("NodeHeartbeatTimeout = %s, want %s", opts.NodeHeartbeatTimeout, heartbeatTimeout)
	}
}

func TestExportOptionsForDir_UsesDefaultAuthDirWithoutOutputDir(t *testing.T) {
	t.Parallel()

	opts := exportOptionsForDir("", nil, cluster.DefaultHeartbeatTimeout())
	if opts.OutputDir != "" {
		t.Fatalf("OutputDir = %q, want empty", opts.OutputDir)
	}
	if opts.AuthDirName != "" {
		t.Fatalf("AuthDirName = %q, want empty for backend default", opts.AuthDirName)
	}
}

func TestExportOptionsForDir_UsesAuthsForExplicitOutputDir(t *testing.T) {
	t.Parallel()

	opts := exportOptionsForDir("out", nil, cluster.DefaultHeartbeatTimeout())
	if opts.OutputDir != "out" {
		t.Fatalf("OutputDir = %q, want out", opts.OutputDir)
	}
	if opts.AuthDirName != "auths" {
		t.Fatalf("AuthDirName = %q, want auths", opts.AuthDirName)
	}
}

func TestResolveDatabaseNodeIP_RejectsClusterSQLiteWithoutExternalIP(t *testing.T) {
	t.Parallel()

	cfg := &cluster.Config{
		SQLite: cluster.SQLiteConfig{Path: "home.db"},
	}
	got, errNodeIP := resolveDatabaseNodeIP(context.Background(), nil, cluster.DatabaseBackendSQLite, cfg, true)
	if errNodeIP == nil {
		t.Fatalf("resolveDatabaseNodeIP() error = nil, want node.external-ip error")
	}
	if got != "" {
		t.Fatalf("resolveDatabaseNodeIP() = %q, want empty ip on error", got)
	}
	if !strings.Contains(errNodeIP.Error(), "node.external-ip is required when cluster uses sqlite backend") {
		t.Fatalf("resolveDatabaseNodeIP() error = %v, want node.external-ip sqlite error", errNodeIP)
	}
}

func TestResolveDatabaseNodeIP_UsesLoopbackForNonClusterSQLite(t *testing.T) {
	t.Parallel()

	got, errNodeIP := resolveDatabaseNodeIP(context.Background(), nil, cluster.DatabaseBackendSQLite, nil, false)
	if errNodeIP != nil {
		t.Fatalf("resolveDatabaseNodeIP() error = %v", errNodeIP)
	}
	if got != "127.0.0.1" {
		t.Fatalf("resolveDatabaseNodeIP() = %q, want 127.0.0.1", got)
	}
}
