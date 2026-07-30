package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/buildinfo"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	"github.com/router-for-me/CLIProxyAPIHome/internal/managementhttp"
	"github.com/router-for-me/CLIProxyAPIHome/internal/pluginauth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/protocolmux"
	quotacollector "github.com/router-for-me/CLIProxyAPIHome/internal/quota"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver"
	"github.com/router-for-me/CLIProxyAPIHome/internal/usermail"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"gorm.io/gorm"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// init copies main package build metadata into shared build info.
func init() {
	buildinfo.Version = Version
	buildinfo.Commit = Commit
	buildinfo.BuildDate = BuildDate
}

// main starts the home command entrypoint.
func main() {
	os.Exit(run())
}

type inFlightConfigApplier interface {
	ApplyInFlightConfig(config.CredentialInFlightConfig) error
}

func applyRESPInFlightConfig(applier inFlightConfigApplier, cfg *config.Config) error {
	if applier == nil || cfg == nil {
		return nil
	}
	return applier.ApplyInFlightConfig(cfg.CredentialInFlight)
}

func applyConfigEvent(ctx context.Context, runtime *home.Runtime, respApplier inFlightConfigApplier, cfg *config.Config, payload []byte) error {
	if reflect.DeepEqual(runtime.Config(), cfg) {
		return applyRESPInFlightConfig(respApplier, cfg)
	}
	if errApply := runtime.ApplyConfigFromCluster(ctx, cfg); errApply != nil {
		return errApply
	}
	if errApplyInFlight := applyRESPInFlightConfig(respApplier, cfg); errApplyInFlight != nil {
		return errApplyInFlight
	}
	runtime.PublishConfigYAML(payload)
	return nil
}

type concurrencyAdmissionRepository interface {
	AdmitCredentialConcurrency(context.Context, cluster.ConcurrencyAdmissionRequest) (cluster.ConcurrencyAdmissionResult, error)
}

func newRuntimeConcurrencyAdmitter(repo concurrencyAdmissionRepository, currentHome func() (cluster.HomeIncarnationID, bool)) home.ConcurrencyAdmitter {
	return home.ConcurrencyAdmitterFunc(func(admitCtx context.Context, req home.ConcurrencyAdmissionRequest) (home.ConcurrencyAdmissionResult, error) {
		if repo == nil || currentHome == nil {
			return home.ConcurrencyAdmissionResult{}, cluster.ErrConcurrencyNodeUnavailable
		}
		homeID, initialized := currentHome()
		if !initialized {
			return home.ConcurrencyAdmissionResult{}, cluster.ErrConcurrencyNodeUnavailable
		}
		result, errAdmit := repo.AdmitCredentialConcurrency(admitCtx, cluster.ConcurrencyAdmissionRequest{
			CredentialID: req.CredentialID,
			Model:        req.Model,
			Lifetime: cluster.ConnectionLifetime{
				Fingerprint: req.Fingerprint,
				ConnectedAt: req.ConnectedAt,
				Controlled:  req.Controlled,
				Home:        homeID,
			},
			ProtocolVersion: req.ProtocolVersion,
		})
		return home.ConcurrencyAdmissionResult{Accounted: result.Accounted, CredentialID: result.CredentialID, Model: result.Model}, errAdmit
	})
}

// run executes the home command and returns the process exit code.
func run() int {
	// Keep validation before state changes so failures leave existing data intact.
	logging.SetupBaseLogger()

	var configPath string
	var addr string
	var sqlitePath string
	var importState bool
	var exportState bool
	var databaseImportPath string
	var databaseExportPath string
	var exportDir string
	var authDir string
	flag.StringVar(&configPath, "config", "config.yaml", "Config file path")
	flag.StringVar(&addr, "addr", "", "Override RESP listen address (host:port)")
	flag.StringVar(&sqlitePath, "sqlite-path", "", "SQLite database path")
	flag.BoolVar(&importState, "import", false, "Import config and auth files into database, then exit")
	flag.BoolVar(&exportState, "export", false, "Export config and auth files from database, then exit")
	flag.StringVar(&databaseImportPath, "db-import", "", "Import a portable Home database snapshot, then exit")
	flag.StringVar(&databaseExportPath, "db-export", "", "Export a portable Home database snapshot, then exit")
	flag.StringVar(&exportDir, "export-dir", "", "Override output directory used by export")
	flag.StringVar(&authDir, "auth-dir", "", "Override auth directory used by import")
	flag.Parse()
	databaseImportPath = strings.TrimSpace(databaseImportPath)
	databaseExportPath = strings.TrimSpace(databaseExportPath)
	if oneTimeModeCount(importState, exportState, databaseImportPath, databaseExportPath) > 1 {
		log.Errorf("only one of -import, -export, -db-import, or -db-export can be used")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var databaseSnapshot *cluster.ValidatedDatabaseSnapshot
	if databaseImportPath != "" {
		var errSnapshot error
		databaseSnapshot, errSnapshot = cluster.OpenDatabaseSnapshot(runCtx, databaseImportPath)
		if errSnapshot != nil {
			log.Errorf("failed to validate database snapshot: %v", errSnapshot)
			return 1
		}
		defer func() {
			if errCloseSnapshot := databaseSnapshot.Close(); errCloseSnapshot != nil {
				log.Warnf("failed to close database snapshot: %v", errCloseSnapshot)
			}
		}()
		manifest := databaseSnapshot.Manifest()
		log.Infof("database snapshot validation completed source_backend=%s tables=%d", manifest.SourceBackend, len(manifest.Tables))
	}

	clusterCfg, clusterExists, errClusterCfg := cluster.LoadConfigOptional("")
	if errClusterCfg != nil {
		log.Errorf("failed to load cluster config: %v", errClusterCfg)
		return 1
	}
	if databaseSnapshot != nil {
		if errPreflight := databaseSnapshot.ValidateForBackend(runCtx, resolveRuntimeDatabaseBackend(clusterCfg, clusterExists)); errPreflight != nil {
			log.Errorf("failed to validate database snapshot for target backend: %v", errPreflight)
			return 1
		}
	}

	var cfg *config.Config
	var rt *home.Runtime
	var coordinator *cluster.Coordinator
	var clusterRepo *cluster.Repository
	var clusterClientAddr string
	var clusterAdapter *cluster.RuntimeAdapter
	var coordinatorErrCh <-chan error
	var eventWatcher *cluster.EventWatcher
	var eventWatcherErrCh <-chan error
	var clusterRESPHandler *cluster.RESPHandler
	var clusterTLSConfig *tls.Config
	var respSrv *respserver.Server
	clusterDB, dbBackend, errClusterOpen := openRuntimeDatabase(runCtx, clusterCfg, clusterExists, sqlitePath)
	if errClusterOpen != nil {
		log.Errorf("failed to open database: %v", errClusterOpen)
		return 1
	}
	sqlDB, errSQLDB := clusterDB.DB()
	if errSQLDB != nil {
		log.Errorf("failed to get sql db: %v", errSQLDB)
		return 1
	}
	defer func() {
		if errCloseSQL := sqlDB.Close(); errCloseSQL != nil {
			log.Warnf("failed to close sql db: %v", errCloseSQL)
		}
	}()
	if databaseSnapshot != nil {
		result, errImport := cluster.ImportDatabaseSnapshot(runCtx, clusterDB, databaseSnapshot, nil)
		if errImport != nil {
			log.Errorf("failed to import database snapshot: %v", errImport)
			return 1
		}
		for _, table := range result.Tables {
			log.Infof("database snapshot table import completed table=%s imported=%d skipped=%d", table.Name, table.Imported, table.Skipped)
		}
		log.Infof("database snapshot import completed target_backend=%s tables=%d", dbBackend, len(result.Tables))
		return 0
	}
	if errMigrate := cluster.AutoMigrate(clusterDB); errMigrate != nil {
		log.Errorf("failed to migrate database: %v", errMigrate)
		return 1
	}
	if databaseExportPath != "" {
		manifest, errExport := cluster.ExportDatabaseSnapshot(runCtx, clusterDB, cluster.DatabaseSnapshotExportOptions{
			Path:        databaseExportPath,
			HomeVersion: Version,
			HomeCommit:  Commit,
			Progress: func(progress cluster.DatabaseSnapshotProgress) {
				log.Infof("database snapshot table export completed table=%s rows=%d", progress.Table, progress.Rows)
			},
		})
		if errExport != nil {
			log.Errorf("failed to export database snapshot: %v", errExport)
			return 1
		}
		log.Infof("database snapshot export completed source_backend=%s tables=%d path=%s", manifest.SourceBackend, len(manifest.Tables), databaseExportPath)
		return 0
	}
	repo := cluster.NewRepository(clusterDB)
	clusterRepo = repo
	nodeCfg := resolveDatabaseNodeConfig(clusterCfg, clusterExists)
	if _, errEnsureLifecycle := repo.EnsureLifecycleConfig(runCtx, nodeCfg.HeartbeatTimeout); errEnsureLifecycle != nil {
		log.Errorf("failed to initialize lifecycle configuration: %v", errEnsureLifecycle)
		return 1
	}
	if importState {
		stats, errImport := cluster.ImportLocalState(runCtx, cluster.ImportOptions{
			ConfigPath:           configPath,
			AuthDir:              authDir,
			Repository:           repo,
			NodeHeartbeatTimeout: nodeCfg.HeartbeatTimeout,
		})
		if errImport != nil {
			log.Errorf("failed to import local state: %v", errImport)
			return 1
		}
		log.Infof(
			"database import completed created=%d updated=%d unchanged=%d restored=%d overwritten=%d skipped=%d",
			stats.Created,
			stats.Updated,
			stats.Unchanged,
			stats.Restored,
			stats.Overwritten,
			stats.Skipped,
		)
		return 0
	}
	if exportState {
		stats, errExport := cluster.ExportLocalState(runCtx, exportOptionsForDir(exportDir, repo, nodeCfg.HeartbeatTimeout))
		if errExport != nil {
			log.Errorf("failed to export local state: %v", errExport)
			return 1
		}
		log.Infof("database export completed config_bytes=%d auth_files=%d", stats.ConfigBytes, stats.AuthFiles)
		return 0
	}

	snapshot, errSnapshot := repo.LoadConfigSnapshot(runCtx)
	if errSnapshot != nil {
		log.Errorf("failed to load database config snapshot: %v", errSnapshot)
		return 1
	}
	if len(snapshot) == 0 {
		log.Errorf("database config is empty; run with -import first")
		return 1
	}
	lastSeenID, runtimeCfg, errRuntimeConfig := loadInitialRuntimeConfig(runCtx, repo)
	if errRuntimeConfig != nil {
		log.Errorf("failed to load database runtime config: %v", errRuntimeConfig)
		return 1
	}
	cfg = runtimeCfg
	applyLogLevel(cfg)

	var errRuntime error
	rt, errRuntime = home.NewRuntime(cfg)
	if errRuntime != nil {
		log.Errorf("failed to init runtime: %v", errRuntime)
		return 1
	}
	if rt == nil {
		log.Errorf("failed to init runtime")
		return 1
	}
	pluginAuthService := pluginauth.NewService(repo)
	rt.SetPluginStoreAuthResolver(pluginAuthService.Resolved)
	rt.SetPluginSyncConfigLoader(func(ctx context.Context) (*config.Config, error) {
		current, _, errConfig := repo.LoadConfigAsRuntimeConfig(ctx)
		return current, errConfig
	})
	rt.SetPluginSyncNodeActive(repo.ClientCertificateFingerprintActive)

	clusterClientAddr, errClusterClientAddr := resolveDatabaseNodeIP(runCtx, clusterDB, dbBackend, clusterCfg, clusterExists)
	if errClusterClientAddr != nil {
		log.Errorf("failed to resolve database node ip: %v", errClusterClientAddr)
		return 1
	}
	tlsConfig, errCertificates := repo.EnsureClusterCertificates(runCtx, clusterClientAddr)
	if errCertificates != nil {
		log.Errorf("failed to init runtime certificates: %v", errCertificates)
		return 1
	}
	clusterTLSConfig = tlsConfig
	adapter := cluster.NewRuntimeAdapter(repo, clusterClientAddr)
	clusterAdapter = adapter
	rt.SetClusterAdapter(adapter)
	rt.SetConcurrencyAdmitter(newRuntimeConcurrencyAdmitter(repo, func() (cluster.HomeIncarnationID, bool) {
		if coordinator == nil {
			return cluster.HomeIncarnationID{}, false
		}
		return coordinator.HomeIncarnation()
	}))
	eventWatcher = cluster.NewEventWatcherFrom(repo, nodeCfg.EventPollInterval, lastSeenID, func(eventCtx context.Context, event cluster.ClusterEventRecord) error {
		scope := strings.TrimSpace(event.Scope)
		if strings.EqualFold(scope, "plugin-store-auth") {
			_, payload, errConfig := repo.LoadConfigAsRuntimeConfig(eventCtx)
			if errConfig != nil {
				return errConfig
			}
			rt.PublishConfigYAML(payload)
			return nil
		}
		if strings.EqualFold(scope, "config") {
			nextConfig, payload, errConfig := repo.LoadConfigAsRuntimeConfig(eventCtx)
			if errConfig != nil {
				return errConfig
			}
			return applyConfigEvent(eventCtx, rt, respSrv, nextConfig, payload)
		}
		if strings.EqualFold(scope, "cpa-fence") {
			return handleCPAFenceEvent(eventCtx, event, clusterRepo, coordinator, respSrv)
		}
		if strings.EqualFold(scope, "plugin-task") {
			_, payload, errConfig := repo.LoadConfigAsRuntimeConfig(eventCtx)
			if errConfig != nil {
				return errConfig
			}
			rt.PublishConfigYAML(payload)
			return nil
		}
		if errApplyEvent := adapter.ApplyEvent(eventCtx, event); errApplyEvent != nil {
			return errApplyEvent
		}
		return rt.ReloadAuths(eventCtx)
	})
	defer rt.Stop()

	listenAddr, listenPort, errListenAddr := resolveListenAddress(addr, cfg, clusterCfg, clusterExists)
	if errListenAddr != nil {
		log.Errorf("failed to resolve listen address: %v", errListenAddr)
		return 1
	}
	addr = listenAddr
	clusterAdvertisedPort := listenPort
	if clusterExists {
		var errAdvertisedPort error
		clusterAdvertisedPort, errAdvertisedPort = resolveClusterAdvertisedPort(clusterCfg, listenPort)
		if errAdvertisedPort != nil {
			log.Errorf("failed to resolve cluster advertised port: %v", errAdvertisedPort)
			return 1
		}
	}
	if clusterAdvertisedPort <= 0 {
		log.Errorf("failed to resolve database node port: listen port must be greater than 0")
		return 1
	}
	if clusterAdapter != nil {
		clusterAdapter.SetHomePort(clusterAdvertisedPort)
	}

	if clusterRepo != nil {
		coordinator = cluster.NewCoordinator(clusterRepo, cluster.NodeIdentity{
			IP:   clusterClientAddr,
			Port: clusterAdvertisedPort,
		}, cluster.CoordinatorOptions{
			HeartbeatInterval: nodeCfg.HeartbeatInterval,
			HeartbeatTimeout:  nodeCfg.HeartbeatTimeout,
			StartupRecovery: func(recoveryCtx context.Context, incarnation cluster.HomeIncarnationID) error {
				return recoverCPAFences(recoveryCtx, clusterRepo, incarnation, respSrv)
			},
		})
		refreshController := cluster.NewRefreshController(coordinator, rt, clusterRepo, clusterTLSConfig)
		coordinator.SetOnMasterChanged(refreshController.OnMasterChanged)
		rt.SetClusterRefreshHandler(refreshController.RefreshNowObserved)
		clusterRESPHandler = cluster.NewRESPHandler(coordinator, refreshController, clusterRepo)
		if clusterAdapter == nil {
			log.Errorf("failed to init cluster runtime adapter")
			return 1
		}
	}

	if errStart := rt.Start(runCtx, configPath); errStart != nil {
		log.Errorf("failed to start runtime: %v", errStart)
		return 1
	}
	userMailService := usermail.NewService(repo, func() *config.Config { return rt.Config() })
	userMailService.Start(runCtx)
	quotaHomeID := net.JoinHostPort(clusterClientAddr, strconv.Itoa(clusterAdvertisedPort))
	quotaCollector := quotacollector.NewCollector(repo, quotacollector.Options{
		HomeID: quotaHomeID,
		GlobalProxyURLProvider: func() string {
			currentConfig := rt.Config()
			if currentConfig == nil {
				return ""
			}
			return currentConfig.ProxyURL
		},
		ResolveAuth: func(ctx context.Context, candidate *coreauth.Auth) (*coreauth.Auth, error) {
			if candidate == nil {
				return nil, fmt.Errorf("quota collector credential is nil")
			}
			current, _, errAuth := repo.GetAuth(ctx, candidate.ID)
			if errAuth != nil {
				return nil, errAuth
			}
			manager := rt.CoreManager()
			if manager != nil && manager.ShouldRefreshCredential(current, time.Now().UTC()) {
				if _, errRefresh := rt.RefreshNow(ctx, current.ID); errRefresh != nil {
					return nil, errRefresh
				}
				current, _, errAuth = repo.GetAuth(ctx, candidate.ID)
				if errAuth != nil {
					return nil, errAuth
				}
			}
			return current, nil
		},
	})
	quotaCollector.Start(runCtx)

	respSrv = respserver.New(addr, rt)
	respSrv.SetClusterHandler(clusterRESPHandler)
	respSrv.SetInFlightSnapshotStore(repo)
	respSrv.SetConcurrencyReleaseStore(repo)
	if errApplyInFlight := applyRESPInFlightConfig(respSrv, cfg); errApplyInFlight != nil {
		log.Errorf("failed to apply in-flight observation config: %v", errApplyInFlight)
		return 1
	}

	cfgPath := strings.TrimSpace(rt.ConfigPath())
	if cfgPath == "" {
		cfgPath = strings.TrimSpace(configPath)
	}
	mgmtOpts := make([]managementhttp.RouteOption, 0, 1)
	if clusterRepo != nil {
		mgmtOpts = append(mgmtOpts, managementhttp.WithDatabaseManagement(managementhttp.DatabaseManagementOption{
			Enabled:          true,
			Repository:       clusterRepo,
			Runtime:          rt,
			NodeIP:           clusterClientAddr,
			NodePort:         clusterAdvertisedPort,
			HeartbeatTimeout: nodeCfg.HeartbeatTimeout,
			ForwardTLSConfig: clusterTLSConfig,
			QuotaRecollect:   quotaCollector,
		}))
	}
	mgmtBuild, errMgmt := managementhttp.Build(cfgPath, mgmtOpts...)
	if errMgmt != nil {
		log.Errorf("failed to init management http: %v", errMgmt)
		return 1
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mgmtBuild.Engine,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if errInitialize := initializeCoordinatorBeforeListen(runCtx, coordinator); errInitialize != nil {
		log.Errorf("failed to initialize cluster coordinator: %v", errInitialize)
		return 1
	}
	if coordinator != nil {
		defer func() {
			if errRetire := coordinator.RetireHomeIncarnation(context.Background()); errRetire != nil && !errors.Is(errRetire, cluster.ErrHomeIncarnationFenced) {
				log.Warnf("failed to retire Home incarnation: %v", errRetire)
			}
		}()
	}

	baseListener, errListen := net.Listen("tcp", addr)
	if errListen != nil {
		log.Errorf("failed to listen on %s: %v", addr, errListen)
		return 1
	}

	if clusterTLSConfig != nil {
		httpSrv.TLSConfig = clusterTLSConfig
		if errHTTP2 := http2.ConfigureServer(httpSrv, &http2.Server{}); errHTTP2 != nil {
			log.Warnf("failed to configure HTTP/2: %v", errHTTP2)
		}
	} else if cfg.TLS.Enable {
		certPath := strings.TrimSpace(cfg.TLS.Cert)
		keyPath := strings.TrimSpace(cfg.TLS.Key)
		if certPath == "" || keyPath == "" {
			log.Errorf("failed to start HTTPS server: tls.cert or tls.key is empty")
			_ = baseListener.Close()
			return 1
		}
		certPair, errLoad := tls.LoadX509KeyPair(certPath, keyPath)
		if errLoad != nil {
			log.Errorf("failed to start HTTPS server: %v", errLoad)
			_ = baseListener.Close()
			return 1
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{certPair},
			NextProtos:   []string{"h2", "http/1.1"},
		}
		httpSrv.TLSConfig = tlsConfig
		if errHTTP2 := http2.ConfigureServer(httpSrv, &http2.Server{}); errHTTP2 != nil {
			log.Warnf("failed to configure HTTP/2: %v", errHTTP2)
		}
		baseListener = tls.NewListener(baseListener, tlsConfig)
	}

	httpListener := protocolmux.NewListener(baseListener.Addr(), 1024)
	var shutdownOnce sync.Once
	shutdownServers := func() {
		shutdownOnce.Do(func() {
			cancelRun()
			_ = httpListener.Close()
			_ = baseListener.Close()
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			_ = httpSrv.Shutdown(shutdownCtx)
		})
	}

	httpErrCh := make(chan error, 1)
	acceptErrCh := make(chan error, 1)

	if coordinator != nil {
		errCh := make(chan error, 1)
		coordinatorErrCh = errCh
		go func() {
			if errCoordinator := coordinator.Start(runCtx); errCoordinator != nil {
				errCh <- errCoordinator
			}
		}()
	}
	if eventWatcher != nil {
		errCh := make(chan error, 1)
		eventWatcherErrCh = errCh
		go func() {
			if errWatcher := eventWatcher.Start(runCtx); errWatcher != nil {
				errCh <- errWatcher
			}
		}()
	}

	go func() {
		httpErrCh <- httpSrv.Serve(httpListener)
	}()
	go func() {
		acceptErrCh <- protocolmux.Serve(runCtx, baseListener, httpListener, func(conn net.Conn) {
			go respSrv.HandleConn(runCtx, conn)
		}, nil, clusterTLSConfig)
	}()
	go runUsageDerivedColumnBackfillMaintenance(runCtx, repo)
	go runUsageCacheReadBackfillMaintenance(runCtx, repo)
	go runUsageTokenAccountingBackfillMaintenance(runCtx, repo)
	go runInFlightStagingCleanupMaintenance(runCtx, repo, rt)

	go func() {
		<-ctx.Done()
		shutdownServers()
	}()

	select {
	case errServe := <-httpErrCh:
		errServe = protocolmux.NormalizeServeError(errServe)
		shutdownServers()
		errAccept := collectServeError(acceptErrCh, 2*time.Second)
		if errServe != nil {
			log.Errorf("http server error: %v", errServe)
			return 1
		}
		if errAccept != nil {
			log.Errorf("listener accept error: %v", errAccept)
			return 1
		}
		return 0
	case errAccept := <-acceptErrCh:
		errAccept = protocolmux.NormalizeServeError(errAccept)
		shutdownServers()
		errServe := collectServeError(httpErrCh, 2*time.Second)
		if errAccept != nil {
			log.Errorf("listener accept error: %v", errAccept)
			return 1
		}
		if errServe != nil {
			log.Errorf("http server error: %v", errServe)
			return 1
		}
		return 0
	case errCoordinator := <-coordinatorErrCh:
		shutdownServers()
		errServe := collectServeError(httpErrCh, 2*time.Second)
		errAccept := collectServeError(acceptErrCh, 2*time.Second)
		if errCoordinator != nil {
			log.Errorf("cluster coordinator error: %v", errCoordinator)
			return 1
		}
		if errServe != nil {
			log.Errorf("http server error: %v", errServe)
			return 1
		}
		if errAccept != nil {
			log.Errorf("listener accept error: %v", errAccept)
			return 1
		}
		return 0
	case errWatcher := <-eventWatcherErrCh:
		shutdownServers()
		errServe := collectServeError(httpErrCh, 2*time.Second)
		errAccept := collectServeError(acceptErrCh, 2*time.Second)
		if errWatcher != nil {
			log.Errorf("cluster event watcher error: %v", errWatcher)
			return 1
		}
		if errServe != nil {
			log.Errorf("http server error: %v", errServe)
			return 1
		}
		if errAccept != nil {
			log.Errorf("listener accept error: %v", errAccept)
			return 1
		}
		return 0
	}
}

// resolveSQLitePath resolves the SQLite database path from flag and config values.
func initializeCoordinatorBeforeListen(ctx context.Context, coordinator *cluster.Coordinator) error {
	if coordinator == nil {
		return nil
	}
	return coordinator.Initialize(ctx)
}

func resolveSQLitePath(flagPath string, configPath string) string {
	flagPath = strings.TrimSpace(flagPath)
	if flagPath != "" {
		return flagPath
	}
	configPath = strings.TrimSpace(configPath)
	if configPath != "" {
		return configPath
	}
	return "home.db"
}

func oneTimeModeCount(importState bool, exportState bool, databaseImportPath string, databaseExportPath string) int {
	count := 0
	if importState {
		count++
	}
	if exportState {
		count++
	}
	if strings.TrimSpace(databaseImportPath) != "" {
		count++
	}
	if strings.TrimSpace(databaseExportPath) != "" {
		count++
	}
	return count
}

func resolveRuntimeDatabaseBackend(clusterCfg *cluster.Config, clusterExists bool) cluster.DatabaseBackend {
	if clusterExists && clusterCfg != nil {
		return clusterCfg.DatabaseBackend()
	}
	return cluster.DatabaseBackendSQLite
}

// openRuntimeDatabase opens the database used by the DB-backed runtime.
func openRuntimeDatabase(ctx context.Context, clusterCfg *cluster.Config, clusterExists bool, sqlitePath string) (*gorm.DB, cluster.DatabaseBackend, error) {
	if clusterExists {
		if clusterCfg == nil {
			return nil, "", fmt.Errorf("cluster config is nil")
		}
		switch clusterCfg.DatabaseBackend() {
		case cluster.DatabaseBackendSQLite:
			db, errOpenSQLite := cluster.OpenSQLite(ctx, resolveSQLitePath(sqlitePath, clusterCfg.SQLite.Path))
			return db, cluster.DatabaseBackendSQLite, errOpenSQLite
		case cluster.DatabaseBackendPostgres:
			db, errOpenPostgres := cluster.Open(ctx, clusterCfg.PGSQL)
			return db, cluster.DatabaseBackendPostgres, errOpenPostgres
		default:
			return nil, "", fmt.Errorf("unsupported database backend %q", clusterCfg.DatabaseBackend())
		}
	}
	db, errOpenSQLite := cluster.OpenSQLite(ctx, resolveSQLitePath(sqlitePath, ""))
	return db, cluster.DatabaseBackendSQLite, errOpenSQLite
}

// resolveDatabaseNodeConfig resolves coordinator and watcher timing settings.
func resolveDatabaseNodeConfig(clusterCfg *cluster.Config, clusterExists bool) cluster.NodeConfig {
	nodeCfg := cluster.NodeConfig{}
	if clusterExists && clusterCfg != nil {
		nodeCfg = clusterCfg.Node
	}
	if nodeCfg.HeartbeatInterval <= 0 {
		nodeCfg.HeartbeatInterval = 5 * time.Second
	}
	if nodeCfg.HeartbeatTimeout <= 0 {
		nodeCfg.HeartbeatTimeout = cluster.DefaultHeartbeatTimeout()
	}
	if nodeCfg.EventPollInterval <= 0 {
		nodeCfg.EventPollInterval = 3 * time.Second
	}
	return nodeCfg
}

// resolveDatabaseNodeIP resolves the node identity IP for the selected database backend.
func resolveDatabaseNodeIP(ctx context.Context, db *gorm.DB, backend cluster.DatabaseBackend, clusterCfg *cluster.Config, clusterExists bool) (string, error) {
	if clusterExists && clusterCfg != nil {
		externalIP := strings.TrimSpace(clusterCfg.Node.ExternalIP)
		if externalIP != "" {
			return externalIP, nil
		}
	}
	if clusterExists && backend == cluster.DatabaseBackendSQLite {
		return "", fmt.Errorf("node.external-ip is required when cluster uses sqlite backend")
	}
	if backend == cluster.DatabaseBackendPostgres {
		return cluster.ClientAddr(ctx, db)
	}
	return "127.0.0.1", nil
}

// resolveListenAddress resolves a listen address.
func resolveListenAddress(addr string, cfg *config.Config, clusterCfg *cluster.Config, clusterEnabled bool) (string, int, error) {
	// Keep validation before state changes so failures leave existing data intact.
	addr = strings.TrimSpace(addr)
	if addr == "" {
		host := ""
		port := 0
		if cfg != nil {
			host = strings.TrimSpace(cfg.Host)
			port = cfg.Port
		}
		if clusterEnabled {
			if clusterCfg == nil {
				return "", 0, fmt.Errorf("cluster config is nil")
			}
			port = clusterCfg.Node.Port
		}
		if host == "" {
			addr = ":" + strconv.Itoa(port)
		} else {
			addr = net.JoinHostPort(host, strconv.Itoa(port))
		}
	}

	port, errPort := listenPortFromAddress(addr)
	if errPort != nil {
		return "", 0, errPort
	}
	if clusterEnabled {
		if port <= 0 {
			return "", 0, fmt.Errorf("cluster listen port must be greater than 0")
		}
	}
	return addr, port, nil
}

// resolveClusterAdvertisedPort resolves the externally reachable cluster port.
func resolveClusterAdvertisedPort(clusterCfg *cluster.Config, listenPort int) (int, error) {
	if clusterCfg == nil {
		return 0, fmt.Errorf("cluster config is nil")
	}
	port := listenPort
	if clusterCfg.Node.ExternalPort > 0 {
		port = clusterCfg.Node.ExternalPort
	}
	if port <= 0 {
		return 0, fmt.Errorf("cluster advertised port must be greater than 0")
	}
	return port, nil
}

// listenPortFromAddress derives listen port from address.
func listenPortFromAddress(addr string) (int, error) {
	_, portValue, errSplitHostPort := net.SplitHostPort(addr)
	if errSplitHostPort != nil {
		return 0, errSplitHostPort
	}
	port, errPort := strconv.Atoi(strings.TrimSpace(portValue))
	if errPort != nil {
		return 0, errPort
	}
	return port, nil
}

func runUsageDerivedColumnBackfillMaintenance(ctx context.Context, repo *cluster.Repository) {
	if repo == nil {
		return
	}
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, errBackfill := repo.RunUsageDerivedColumnBackfillBatch(batchCtx)
		cancel()
		delay := time.Minute
		if errBackfill != nil {
			if ctx.Err() == nil {
				log.WithError(errBackfill).Warn("usage derived-column backfill batch failed")
			}
		} else if !result.Done && !result.Skipped {
			delay = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func runInFlightStagingCleanupMaintenance(ctx context.Context, repo *cluster.Repository, runtime *home.Runtime) {
	if repo == nil || runtime == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		cfg := runtime.Config()
		if cfg == nil {
			log.Warn("in-flight staging cleanup skipped: runtime config is unavailable")
		} else {
			_, _, retention, errDurations := cfg.CredentialInFlight.Durations()
			if errDurations != nil {
				log.WithError(errDurations).Warn("in-flight staging cleanup skipped: invalid retention")
			} else {
				cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				errCleanup := repo.CleanupExpiredInFlightStaging(cleanupCtx, retention)
				cancel()
				if errCleanup != nil && ctx.Err() == nil {
					log.WithError(errCleanup).Warn("in-flight staging cleanup failed")
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runUsageCacheReadBackfillMaintenance(ctx context.Context, repo *cluster.Repository) {
	if repo == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		result, errBackfill := repo.RunUsageCacheReadBackfillBatch(batchCtx)
		cancel()
		if errBackfill != nil {
			if ctx.Err() == nil {
				log.WithError(errBackfill).Warn("usage cache-read backfill batch failed")
			}
		} else if result.Done {
			return
		} else if !result.Skipped {
			// Keep draining successful batches without waiting on the idle ticker.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runUsageTokenAccountingBackfillMaintenance(ctx context.Context, repo *cluster.Repository) {
	if repo == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, errBackfill := repo.RunUsageTokenAccountingBackfillBatch(batchCtx)
		cancel()
		if errBackfill != nil {
			if ctx.Err() == nil {
				log.WithError(errBackfill).Warn("usage token accounting backfill batch failed")
			}
		} else if !result.Done && !result.Skipped {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// collectServeError handles a collect serve error.
func collectServeError(ch <-chan error, timeout time.Duration) error {
	if ch == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case errServe := <-ch:
		return protocolmux.NormalizeServeError(errServe)
	case <-timer.C:
		return nil
	}
}

func exportOptionsForDir(exportDir string, repo *cluster.Repository, nodeHeartbeatTimeout time.Duration) cluster.ExportOptions {
	options := cluster.ExportOptions{
		OutputDir:            exportDir,
		Repository:           repo,
		NodeHeartbeatTimeout: nodeHeartbeatTimeout,
	}
	if strings.TrimSpace(exportDir) != "" {
		options.AuthDirName = "auths"
	}
	return options
}

// applyLogLevel applies a log level.
func applyLogLevel(cfg *config.Config) {
	currentLevel := log.GetLevel()
	debugEnabled := cfg != nil && cfg.Debug
	if debugEnabled {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	if nextLevel := log.GetLevel(); nextLevel != currentLevel {
		log.Infof("log level changed from %s to %s (debug=%t)", currentLevel, nextLevel, debugEnabled)
	}
}
