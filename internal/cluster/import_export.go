package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

type ImportOptions struct {
	ConfigPath           string
	AuthDir              string
	Repository           *Repository
	NodeHeartbeatTimeout time.Duration
	Now                  time.Time
}

type ImportStats struct {
	Created     int
	Updated     int
	Unchanged   int
	Restored    int
	Overwritten int
	Skipped     int
}

type ExportOptions struct {
	OutputDir            string
	Repository           *Repository
	ConfigName           string
	AuthDirName          string
	NodeHeartbeatTimeout time.Duration
}

type ExportStats struct {
	ConfigBytes int
	AuthFiles   int
}

const defaultExportAuthDirName = "~/.cli-proxy-api"

var defaultImportTime = time.Unix(1, 0).UTC()

type exportSnapshotConfigReadContextKey struct{}

func waitForExportSnapshotConfigRead(ctx context.Context) {
	wait, okWait := contextOrBackground(ctx).Value(exportSnapshotConfigReadContextKey{}).(func())
	if okWait && wait != nil {
		wait()
	}
}

// ImportLocalState imports config.yaml and auth-dir credentials into the repository.
func ImportLocalState(ctx context.Context, opts ImportOptions) (ImportStats, error) {
	// Normalize source data before building the derived payload.
	stats := ImportStats{}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Repository == nil {
		return stats, fmt.Errorf("import repository is required")
	}
	now := opts.Now
	if now.IsZero() {
		// Keep default imports stable so identical source files remain idempotent.
		now = defaultImportTime
	}

	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath == "" {
		configPath = "config.yaml"
	}
	info, errStat := os.Stat(configPath)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return stats, fmt.Errorf("config path %q does not exist", configPath)
		}
		return stats, fmt.Errorf("stat config path %q: %w", configPath, errStat)
	}
	if info.IsDir() {
		return stats, fmt.Errorf("config path %q is a directory", configPath)
	}

	rawConfig, errReadConfig := os.ReadFile(configPath)
	if errReadConfig != nil {
		return stats, fmt.Errorf("read config path %q: %w", configPath, errReadConfig)
	}
	root := map[string]any{}
	if len(rawConfig) != 0 {
		if errUnmarshalConfig := yaml.Unmarshal(rawConfig, &root); errUnmarshalConfig != nil {
			return stats, fmt.Errorf("parse config path %q: %w", configPath, errUnmarshalConfig)
		}
		if root == nil {
			root = map[string]any{}
		}
	}
	cfg, _, errRuntimeConfig := RuntimeConfigFromRoot(root)
	if errRuntimeConfig != nil {
		return stats, errRuntimeConfig
	}
	authDir, errResolveAuthDir := resolveImportAuthDir(configPath, cfg, opts.AuthDir)
	if errResolveAuthDir != nil {
		return stats, errResolveAuthDir
	}
	lifecycleNodeHeartbeatTimeout := opts.NodeHeartbeatTimeout
	if lifecycleNodeHeartbeatTimeout <= 0 {
		lifecycleNodeHeartbeatTimeout = DefaultHeartbeatTimeout()
	}

	pendingAuths := make(map[string]*coreauth.Auth)
	authOrder := make([]string, 0, 16)
	if errConfigAuths := collectImportConfigAuths(ctx, opts.Repository, cfg, authDir, now, pendingAuths, &authOrder, &stats); errConfigAuths != nil {
		return stats, errConfigAuths
	}
	if errFileAuths := collectImportAuthFiles(cfg, authDir, now, pendingAuths, &authOrder, &stats); errFileAuths != nil {
		return stats, errFileAuths
	}
	if errValidate := validateImportCredentialCollisions(ctx, opts.Repository, pendingAuths, authOrder); errValidate != nil {
		return stats, errValidate
	}
	db, errDB := opts.Repository.database()
	if errDB != nil {
		return stats, errDB
	}
	mutationStats := ImportStats{}
	errTransaction := withConcurrencyTransaction(ctx, db, func(tx *gorm.DB) error {
		gate, errGate := lockConcurrencyActivationGate(tx)
		if errGate != nil {
			return errGate
		}
		lifecycleRevision, lifecycleChanged, errUpdateLifecycle := updateLifecycleConfigTx(ctx, tx, lifecycleNodeHeartbeatTimeout, cfg.CredentialConcurrency)
		if errUpdateLifecycle != nil {
			return errUpdateLifecycle
		}
		if lifecycleChanged {
			if errEvent := appendEvent(tx, "config", "update", "credential-concurrency", lifecycleRevision); errEvent != nil {
				return errEvent
			}
		}
		txRepo := NewRepository(tx)
		if errLock := txRepo.lockAllProviderAuthsForReconciliationTx(ctx, tx); errLock != nil {
			return errLock
		}
		for _, key := range providerCredentialConfigKeys {
			next := providerAuthsForConfigKey(importAuthsInOrder(pendingAuths, authOrder), key)
			results, errResults := providerAuthImportResults(ctx, txRepo, next)
			if errResults != nil {
				return errResults
			}
			if errReconcile := txRepo.reconcileProviderAuthsWithLockedActivationGateTx(ctx, tx, key, next, ConcurrencyCredentialReferenceChecker{}); errReconcile != nil {
				return mapCredentialConcurrencyOrphan(errReconcile)
			}
			for _, result := range results {
				addImportResult(&mutationStats, result)
			}
		}
		for _, credentialID := range authOrder {
			auth := pendingAuths[credentialID]
			if auth == nil || isAnyProviderConfigAuth(auth) {
				continue
			}
			delete(auth.Attributes, "provider_credential_id_generated")
			_, result, errUpsertAuth := txRepo.UpsertAuthWithResult(ctx, auth, "upsert")
			if errUpsertAuth != nil {
				return errUpsertAuth
			}
			addImportResult(&mutationStats, result)
		}
		if errImportPolicies := txRepo.importCredentialConcurrencyPoliciesWithLockedActivationGateTx(ctx, tx, gate, root); errImportPolicies != nil {
			return errImportPolicies
		}
		return importConfigRoot(ctx, txRepo, root, &mutationStats)
	})
	if errTransaction != nil {
		return stats, errTransaction
	}
	addImportStats(&stats, mutationStats)
	return stats, nil
}

// ExportLocalState exports repository config and auth files into a local directory.
func ExportLocalState(ctx context.Context, opts ExportOptions) (ExportStats, error) {
	stats := ExportStats{}
	ctx = contextOrBackground(ctx)
	if opts.Repository == nil {
		return stats, fmt.Errorf("export repository is required")
	}
	lifecycleNodeHeartbeatTimeout := opts.NodeHeartbeatTimeout
	if lifecycleNodeHeartbeatTimeout <= 0 {
		lifecycleNodeHeartbeatTimeout = DefaultHeartbeatTimeout()
	}
	if _, errEnsureLifecycle := opts.Repository.EnsureLifecycleConfig(ctx, lifecycleNodeHeartbeatTimeout); errEnsureLifecycle != nil {
		return stats, errEnsureLifecycle
	}

	outputDir := strings.TrimSpace(opts.OutputDir)
	if outputDir == "" {
		outputDir = "."
	}
	configName := strings.TrimSpace(opts.ConfigName)
	if configName == "" {
		configName = "config.yaml"
	}
	authDirName := strings.TrimSpace(opts.AuthDirName)
	if authDirName == "" {
		authDirName = defaultExportAuthDirName
	}
	var errValidatePath error
	configName, errValidatePath = validateExportRelativePath("ConfigName", configName)
	if errValidatePath != nil {
		return stats, errValidatePath
	}
	var errResolveExportAuthDir error
	authDirName, authDir, errResolveExportAuthDir := resolveExportAuthDir(outputDir, authDirName)
	if errResolveExportAuthDir != nil {
		return stats, errResolveExportAuthDir
	}

	configPath := filepath.Join(outputDir, configName)
	if errEnsureTargets := ensureExportTargetsAvailable(configPath, configName, authDir); errEnsureTargets != nil {
		return stats, errEnsureTargets
	}

	var root map[string]any
	var auths []*coreauth.Auth
	db, errDB := opts.Repository.database()
	if errDB != nil {
		return stats, errDB
	}
	tx := readOnlyRepeatableReadTransaction(ctx, db)
	if tx.Error != nil {
		return stats, tx.Error
	}
	errSnapshot := func() error {
		var errLoadSnapshot error
		root, errLoadSnapshot = loadConfigRootForExportTx(ctx, tx)
		if errLoadSnapshot != nil {
			return errLoadSnapshot
		}
		waitForExportSnapshotConfigRead(ctx)
		lifecycleRecord := LifecycleConfigRecord{}
		if errLifecycle := tx.WithContext(ctx).First(&lifecycleRecord, "id = ?", 1).Error; errLifecycle != nil {
			return errLifecycle
		}
		lifecycleConfig, errLifecycle := lifecycleConfigFromRecord(lifecycleRecord)
		if errLifecycle != nil {
			return errLifecycle
		}
		var errListAuths error
		auths, errListAuths = listAuthsForExportTx(ctx, tx)
		if errListAuths != nil {
			return errListAuths
		}
		sort.Slice(auths, func(i, j int) bool {
			return auths[i].ID < auths[j].ID
		})
		root["auth-dir"] = authDirName
		root["credential-concurrency"] = lifecycleConfig
		ApplyCredentialConfigToRoot(root, auths)
		return exportCredentialConcurrencyPoliciesForAuthsTx(ctx, tx, root, auths)
	}()
	if errSnapshot != nil {
		if errRollback := tx.Rollback().Error; errRollback != nil {
			return stats, fmt.Errorf("export snapshot: %w; rollback: %v", errSnapshot, errRollback)
		}
		return stats, errSnapshot
	}
	if errCommit := tx.Commit().Error; errCommit != nil {
		return stats, errCommit
	}
	if _, errNormalizeSecret := normalizeConfigRootSecrets(root); errNormalizeSecret != nil {
		return stats, errNormalizeSecret
	}

	data, errMarshal := yaml.Marshal(root)
	if errMarshal != nil {
		return stats, errMarshal
	}
	if len(data) == 0 {
		return stats, fmt.Errorf("exported config is empty")
	}
	if errWriteConfig := writeExportFileExclusive(configPath, configName, data, 0o600); errWriteConfig != nil {
		return stats, errWriteConfig
	}
	stats.ConfigBytes = len(data)

	authFiles, errWriteAuthFiles := writeExportAuthFilesExclusive(auths, authDir)
	if errWriteAuthFiles != nil {
		return stats, errWriteAuthFiles
	}
	stats.AuthFiles = authFiles
	return stats, nil
}

// readOnlyRepeatableReadTransaction starts one consistent read snapshot for PostgreSQL and SQLite.
func readOnlyRepeatableReadTransaction(ctx context.Context, db *gorm.DB) *gorm.DB {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return db.WithContext(contextOrBackground(ctx)).Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	}
	return db.WithContext(contextOrBackground(ctx)).Begin()
}

func loadConfigRootForExportTx(ctx context.Context, tx *gorm.DB) (map[string]any, error) {
	var records []ConfigRecord
	if errFind := tx.WithContext(ctx).Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	root := make(map[string]any, len(records)+1)
	for _, record := range records {
		if isClusterCredentialConfigKey(record.Key) || strings.TrimSpace(record.Key) == configAPIKeysRootKey {
			continue
		}
		var value any
		if len(record.Value) > 0 {
			if errUnmarshal := json.Unmarshal(record.Value, &value); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		root[record.Key] = value
	}
	var apiKeyRecords []APIKeyRecord
	if errFind := tx.WithContext(ctx).Order("id").Find(&apiKeyRecords).Error; errFind != nil {
		return nil, errFind
	}
	keys := make([]string, 0, len(apiKeyRecords))
	for _, record := range apiKeyRecords {
		if key := strings.TrimSpace(record.APIKey); key != "" {
			keys = append(keys, key)
		}
	}
	if keys = normalizeAPIKeys(keys); len(keys) > 0 {
		root[configAPIKeysRootKey] = keys
	}
	return root, nil
}

func listAuthsForExportTx(ctx context.Context, tx *gorm.DB) ([]*coreauth.Auth, error) {
	var records []AuthRecord
	if errFind := tx.WithContext(ctx).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	auths := make([]*coreauth.Auth, 0, len(records))
	for index := range records {
		auth, errAuth := RecordToAuth(&records[index])
		if errAuth != nil {
			return nil, errAuth
		}
		auth.ID = records[index].UUID
		auth.Index = records[index].UUID
		auths = append(auths, auth)
	}
	return auths, nil
}

func exportCredentialConcurrencyPoliciesForAuthsTx(ctx context.Context, tx *gorm.DB, root map[string]any, auths []*coreauth.Auth) error {
	if root == nil {
		return fmt.Errorf("config root is nil")
	}
	policies, errPolicies := listCredentialConcurrencyPoliciesTx(ctx, tx)
	if errPolicies != nil {
		return errPolicies
	}
	serialized := serializedCredentialIDs(auths)
	var credentialRecords []AuthRecord
	if errFind := tx.WithContext(ctx).Unscoped().Select("uuid").Find(&credentialRecords).Error; errFind != nil {
		return errFind
	}
	knownCredentials := make(map[string]struct{}, len(credentialRecords))
	for _, record := range credentialRecords {
		knownCredentials[record.UUID] = struct{}{}
	}
	out := make(map[string]CredentialConcurrencyExchangePolicy)
	for _, policy := range policies {
		if _, known := knownCredentials[policy.CredentialID]; !known {
			return fmt.Errorf("credential concurrency policy references unresolved credential %s", policy.CredentialID)
		}
		if _, okSerialized := serialized[policy.CredentialID]; !okSerialized || (policy.MaxInFlight == nil && len(policy.MaxInFlightByModel) == 0) {
			continue
		}
		item := CredentialConcurrencyExchangePolicy{MaxInFlightByModel: cloneConcurrencyModelLimits(policy.MaxInFlightByModel)}
		if policy.MaxInFlight != nil && *policy.MaxInFlight > 0 {
			value := *policy.MaxInFlight
			item.MaxInFlight = &value
		}
		out[policy.CredentialID] = item
	}
	root[credentialConcurrencyPoliciesRootKey] = out
	return nil
}

func serializedCredentialIDs(auths []*coreauth.Auth) map[string]struct{} {
	ids := make(map[string]struct{}, len(auths))
	seenFiles := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if credentialConfigAuthKind(auth) != "" {
			ids[auth.ID] = struct{}{}
		}
		if !isExportOAuthAuth(auth) {
			continue
		}
		fileName := exportAuthFileName(auth)
		if fileName == "" {
			continue
		}
		if _, exists := seenFiles[fileName]; exists {
			continue
		}
		seenFiles[fileName] = struct{}{}
		ids[auth.ID] = struct{}{}
	}
	return ids
}

func validateExportRelativePath(field string, value string) (string, error) {
	value = strings.TrimSpace(value)
	cleanValue := filepath.Clean(value)
	if value == "" || value == "." || filepath.IsAbs(value) || cleanValue != value || cleanValue == ".." || strings.HasPrefix(cleanValue, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid export %s %q: path must be a clean relative path within OutputDir", field, value)
	}
	return cleanValue, nil
}

func resolveExportAuthDir(outputDir string, authDirName string) (string, string, error) {
	authDirName = strings.TrimSpace(authDirName)
	if authDirName == "" {
		authDirName = defaultExportAuthDirName
	}
	if filepath.IsAbs(authDirName) || strings.HasPrefix(authDirName, "~") {
		resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(authDirName)
		if errResolveAuthDir != nil {
			return "", "", errResolveAuthDir
		}
		return authDirName, resolvedAuthDir, nil
	}
	cleanAuthDirName, errValidatePath := validateExportRelativePath("AuthDirName", authDirName)
	if errValidatePath != nil {
		return "", "", errValidatePath
	}
	return cleanAuthDirName, filepath.Join(outputDir, cleanAuthDirName), nil
}

func ensureExportTargetsAvailable(configPath string, configName string, authDir string) error {
	if _, errStat := os.Stat(configPath); errStat == nil {
		return fmt.Errorf("%s already exists", exportTargetName(configName, configPath))
	} else if !os.IsNotExist(errStat) {
		return fmt.Errorf("stat export config path %q: %w", configPath, errStat)
	}

	info, errStatAuth := os.Stat(authDir)
	if errStatAuth != nil {
		if os.IsNotExist(errStatAuth) {
			return nil
		}
		return fmt.Errorf("stat export auth dir %q: %w", authDir, errStatAuth)
	}
	if !info.IsDir() {
		return fmt.Errorf("auth dir %q already exists and is not a directory", authDir)
	}
	entries, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		return fmt.Errorf("read export auth dir %q: %w", authDir, errReadDir)
	}
	if len(entries) > 0 {
		return fmt.Errorf("auth dir %q already exists and is not empty", authDir)
	}
	return nil
}

func writeExportFileExclusive(path string, name string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
			return errMkdir
		}
	}
	file, errOpenFile := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errOpenFile != nil {
		if os.IsExist(errOpenFile) {
			return fmt.Errorf("%s already exists", exportTargetName(name, path))
		}
		return errOpenFile
	}
	if _, errWriteFile := file.Write(data); errWriteFile != nil {
		if errCloseFile := file.Close(); errCloseFile != nil {
			return fmt.Errorf("write export file %q: %w; close error: %v", path, errWriteFile, errCloseFile)
		}
		return errWriteFile
	}
	if errCloseFile := file.Close(); errCloseFile != nil {
		return errCloseFile
	}
	return nil
}

func exportTargetName(name string, path string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	return filepath.Base(path)
}

func importConfigRoot(ctx context.Context, repo *Repository, root map[string]any, stats *ImportStats) error {
	for key, value := range root {
		normalizedKey := strings.TrimSpace(key)
		if normalizedKey == "" {
			stats.Skipped++
			continue
		}
		if normalizedKey == configAPIKeysRootKey {
			apiKeyStats, errAPIKeys := repo.UpsertAPIKeys(ctx, normalizeAPIKeysFromAny(value))
			if errAPIKeys != nil {
				return errAPIKeys
			}
			addImportAPIKeyStats(stats, apiKeyStats)
			continue
		}
		if isClusterCredentialConfigKey(normalizedKey) || normalizedKey == "credential-concurrency" || normalizedKey == credentialConcurrencyPoliciesRootKey {
			stats.Skipped++
			continue
		}
		result, errUpsertConfigValue := repo.UpsertConfigValueWithResult(ctx, normalizedKey, value)
		if errUpsertConfigValue != nil {
			return errUpsertConfigValue
		}
		addImportResult(stats, result)
	}
	return nil
}

func collectImportConfigAuths(ctx context.Context, repo *Repository, cfg *appconfig.Config, authDir string, now time.Time, pending map[string]*coreauth.Auth, order *[]string, stats *ImportStats) error {
	existingBySource := make(map[string]string)
	if repo != nil {
		existing, errList := repo.ListAuths(ctx)
		if errList != nil {
			return errList
		}
		for _, auth := range existing {
			if auth == nil || auth.Attributes == nil {
				continue
			}
			source := strings.TrimSpace(auth.Attributes["source"])
			if source != "" {
				existingBySource[auth.Provider+"\\x00"+source] = auth.ID
			}
		}
	}
	sctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     authDir,
		Now:         now,
		IDGenerator: synthesizer.NewStableIDGenerator(),
		UUIDForAuth: func(auth *coreauth.Auth) string {
			if auth == nil || auth.Attributes == nil {
				return ""
			}
			return existingBySource[auth.Provider+"\\x00"+strings.TrimSpace(auth.Attributes["source"])]
		},
	}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(sctx)
	if errSynthesize != nil {
		return errSynthesize
	}
	for _, auth := range auths {
		if errAdd := addImportAuth(pending, order, stats, auth); errAdd != nil {
			return errAdd
		}
	}
	return nil
}

func collectImportAuthFiles(cfg *appconfig.Config, authDir string, now time.Time, pending map[string]*coreauth.Auth, order *[]string, stats *ImportStats) error {
	if strings.TrimSpace(authDir) == "" {
		return nil
	}
	entries, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		if os.IsNotExist(errReadDir) {
			return nil
		}
		return errReadDir
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		fullPath := filepath.Join(authDir, entry.Name())
		rawPayload, errReadFile := os.ReadFile(fullPath)
		if errReadFile != nil {
			return errReadFile
		}
		updatedPayload, fileUUID, changed, errUUID := EnsureOAuthPayloadUUID(rawPayload)
		if errUUID != nil {
			return errUUID
		}
		if changed {
			if errWriteFile := os.WriteFile(fullPath, updatedPayload, 0o600); errWriteFile != nil {
				return errWriteFile
			}
		}

		currentFileUUID := fileUUID
		sctx := &synthesizer.SynthesisContext{
			Config:      cfg,
			AuthDir:     authDir,
			Now:         now,
			IDGenerator: synthesizer.NewStableIDGenerator(),
			ClusterMode: true,
		}
		sctx.UUIDForAuth = func(auth *coreauth.Auth) string {
			_ = auth
			return currentFileUUID
		}

		auths := synthesizer.SynthesizeAuthFile(sctx, fullPath, updatedPayload)
		if len(auths) == 0 {
			stats.Skipped++
			continue
		}
		ApplyOriginalAuthFileName(auths, entry.Name())
		for _, auth := range auths {
			if errAdd := addImportAuth(pending, order, stats, auth); errAdd != nil {
				return errAdd
			}
		}
	}
	return nil
}

func addImportAuth(pending map[string]*coreauth.Auth, order *[]string, stats *ImportStats, auth *coreauth.Auth) error {
	if auth == nil {
		stats.Skipped++
		return nil
	}
	credentialID := strings.TrimSpace(auth.ID)
	if credentialID == "" {
		stats.Skipped++
		return nil
	}
	if _, exists := pending[credentialID]; exists {
		return fmt.Errorf("%w: duplicate imported credential id: %s", ErrCredentialIdentityConflict, credentialID)
	}
	*order = append(*order, credentialID)
	pending[credentialID] = auth
	return nil
}

var providerCredentialConfigKeys = []string{
	"gemini-api-key",
	"vertex-api-key",
	"codex-api-key",
	"xai-api-key",
	"claude-api-key",
	"openai-compatibility",
}

func importAuthsInOrder(pending map[string]*coreauth.Auth, order []string) []*coreauth.Auth {
	auths := make([]*coreauth.Auth, 0, len(order))
	for _, credentialID := range order {
		if auth := pending[credentialID]; auth != nil {
			auths = append(auths, auth)
		}
	}
	return auths
}

func validateImportCredentialCollisions(ctx context.Context, repo *Repository, pending map[string]*coreauth.Auth, order []string) error {
	existing, errList := repo.ListAuthsUnscoped(ctx, nil)
	if errList != nil {
		return errList
	}
	existingByID := make(map[string]*coreauth.Auth, len(existing))
	for _, auth := range existing {
		if auth != nil {
			existingByID[auth.ID] = auth
		}
	}
	for _, credentialID := range order {
		incoming := pending[credentialID]
		previous := existingByID[credentialID]
		if incoming == nil || previous == nil || sameImportedCredentialLineage(previous, incoming) {
			continue
		}
		return fmt.Errorf("%w: imported credential id belongs to another auth: %s", ErrCredentialIdentityConflict, credentialID)
	}
	return nil
}

func sameImportedCredentialLineage(previous *coreauth.Auth, incoming *coreauth.Auth) bool {
	previousKey := providerConfigKeyForAuth(previous)
	incomingKey := providerConfigKeyForAuth(incoming)
	if previousKey != "" || incomingKey != "" {
		return previousKey != "" && previousKey == incomingKey && previous.Provider == incoming.Provider
	}
	if previous == nil || incoming == nil || previous.Provider != incoming.Provider || previous.Attributes == nil || incoming.Attributes == nil {
		return false
	}
	return strings.TrimSpace(previous.Attributes["source"]) == strings.TrimSpace(incoming.Attributes["source"])
}

func isAnyProviderConfigAuth(auth *coreauth.Auth) bool {
	return providerConfigKeyForAuth(auth) != ""
}

func providerConfigKeyForAuth(auth *coreauth.Auth) string {
	for _, key := range providerCredentialConfigKeys {
		if isProviderAuthForConfigKey(auth, key) {
			return key
		}
	}
	return ""
}

func providerAuthImportResults(ctx context.Context, repo *Repository, next []*coreauth.Auth) ([]UpsertResult, error) {
	if len(next) == 0 {
		return nil, nil
	}
	db, errDB := repo.database()
	if errDB != nil {
		return nil, errDB
	}
	ids := make([]string, 0, len(next))
	for _, auth := range next {
		if auth != nil {
			ids = append(ids, auth.ID)
		}
	}
	var records []AuthRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Unscoped().Where("uuid IN ?", ids).Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	existingByID := make(map[string]*AuthRecord, len(records))
	for i := range records {
		existingByID[records[i].UUID] = &records[i]
	}
	results := make([]UpsertResult, 0, len(next))
	for _, auth := range next {
		if auth == nil {
			continue
		}
		existing := existingByID[auth.ID]
		if existing == nil {
			results = append(results, UpsertResultCreated)
			continue
		}
		if existing.DeletedAt.Valid {
			results = append(results, UpsertResultRestored)
			continue
		}
		current, errCurrent := RecordToAuth(existing)
		if errCurrent != nil {
			return nil, errCurrent
		}
		candidate := auth.Clone()
		candidate.Disabled = current.Disabled
		candidate.Status = current.Status
		candidate.StatusMessage = current.StatusMessage
		candidateRecord, errRecord := AuthToRecord(candidate)
		if errRecord != nil {
			return nil, errRecord
		}
		sameJSON, errJSONEqual := semanticJSONEqual([]byte(existing.AuthJSON), []byte(candidateRecord.AuthJSON))
		if errJSONEqual != nil {
			return nil, errJSONEqual
		}
		if sameJSON {
			results = append(results, UpsertResultUnchanged)
		} else {
			results = append(results, UpsertResultUpdated)
		}
	}
	return results, nil
}

func addImportStats(stats *ImportStats, next ImportStats) {
	stats.Created += next.Created
	stats.Updated += next.Updated
	stats.Unchanged += next.Unchanged
	stats.Restored += next.Restored
	stats.Overwritten += next.Overwritten
	stats.Skipped += next.Skipped
}

func addImportResult(stats *ImportStats, result UpsertResult) {
	switch result {
	case UpsertResultCreated:
		stats.Created++
	case UpsertResultUpdated:
		stats.Updated++
	case UpsertResultUnchanged:
		stats.Unchanged++
	case UpsertResultRestored:
		stats.Restored++
	default:
		stats.Skipped++
	}
}

func addImportAPIKeyStats(stats *ImportStats, apiKeyStats APIKeyUpsertStats) {
	stats.Created += apiKeyStats.Created
	stats.Updated += apiKeyStats.Updated
	stats.Unchanged += apiKeyStats.Unchanged
	stats.Restored += apiKeyStats.Restored
	stats.Updated += apiKeyStats.Removed
}

func resolveImportAuthDir(configPath string, cfg *appconfig.Config, explicitAuthDir string) (string, error) {
	if authDir := strings.TrimSpace(explicitAuthDir); authDir != "" {
		return util.ResolveAuthDir(authDir)
	}
	authDir := ""
	if cfg != nil {
		authDir = strings.TrimSpace(cfg.AuthDir)
	}
	if authDir == "" {
		return "", nil
	}
	if filepath.IsAbs(authDir) || strings.HasPrefix(authDir, "~") {
		return util.ResolveAuthDir(authDir)
	}
	configDir := filepath.Dir(configPath)
	if configDir == "" || configDir == "." {
		return util.ResolveAuthDir(authDir)
	}
	return util.ResolveAuthDir(filepath.Join(configDir, authDir))
}
