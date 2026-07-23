package cluster

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

const (
	databaseSnapshotFormat        = "cliproxyapihome-database-snapshot"
	databaseSnapshotFormatVersion = 2
	databaseSnapshotManifestName  = "manifest.json"
	databaseSnapshotBatchSize     = 500
	databaseSnapshotManifestLimit = 4 << 20
)

// DatabaseSnapshotManifest describes one portable Home database snapshot.
type DatabaseSnapshotManifest struct {
	Format        string                          `json:"format"`
	FormatVersion int                             `json:"format_version"`
	CreatedAt     time.Time                       `json:"created_at"`
	HomeVersion   string                          `json:"home_version"`
	HomeCommit    string                          `json:"home_commit"`
	SourceBackend DatabaseBackend                 `json:"source_backend"`
	Tables        []DatabaseSnapshotManifestTable `json:"tables"`
}

// DatabaseSnapshotManifestTable describes one JSONL table entry.
type DatabaseSnapshotManifestTable struct {
	Name    string `json:"name"`
	Rows    int64  `json:"rows"`
	SHA256  string `json:"sha256"`
	Restore bool   `json:"restore"`
}

// DatabaseSnapshotProgress describes a completed snapshot table stage.
type DatabaseSnapshotProgress struct {
	Operation string
	Table     string
	Rows      int64
	Skipped   int64
}

// DatabaseSnapshotExportOptions configures a logical snapshot export.
type DatabaseSnapshotExportOptions struct {
	Path        string
	HomeVersion string
	HomeCommit  string
	Progress    func(DatabaseSnapshotProgress)
}

// DatabaseSnapshotImportTableResult reports one imported or skipped table.
type DatabaseSnapshotImportTableResult struct {
	Name     string
	Imported int64
	Skipped  int64
}

// DatabaseSnapshotImportResult reports all table-level import counts.
type DatabaseSnapshotImportResult struct {
	Tables []DatabaseSnapshotImportTableResult
}

// ValidatedDatabaseSnapshot owns an open, structurally validated snapshot archive.
// Keeping the file open prevents a path replacement race between validation
// and import on Windows.
type ValidatedDatabaseSnapshot struct {
	file             *os.File
	reader           *zip.Reader
	entries          map[string]*zip.File
	manifest         DatabaseSnapshotManifest
	models           []databaseModel
	validationMu     sync.Mutex
	validatedTargets map[DatabaseBackend]bool
}

// Manifest returns a detached copy of the validated snapshot manifest.
func (s *ValidatedDatabaseSnapshot) Manifest() DatabaseSnapshotManifest {
	if s == nil {
		return DatabaseSnapshotManifest{}
	}
	manifest := s.manifest
	manifest.Tables = append([]DatabaseSnapshotManifestTable(nil), s.manifest.Tables...)
	return manifest
}

// Close releases the snapshot archive.
func (s *ValidatedDatabaseSnapshot) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	errClose := s.file.Close()
	s.file = nil
	s.reader = nil
	s.entries = nil
	return errClose
}

// ValidateForBackend runs target-specific checks before the database is opened.
func (s *ValidatedDatabaseSnapshot) ValidateForBackend(ctx context.Context, backend DatabaseBackend) error {
	if s == nil || s.file == nil {
		return fmt.Errorf("database snapshot is not open")
	}
	if errContext := contextOrBackground(ctx).Err(); errContext != nil {
		return errContext
	}
	switch backend {
	case DatabaseBackendSQLite, DatabaseBackendPostgres:
	default:
		return fmt.Errorf("unsupported database snapshot target backend %q", backend)
	}
	s.validationMu.Lock()
	defer s.validationMu.Unlock()
	if s.validatedTargets[backend] {
		return nil
	}
	if backend == DatabaseBackendPostgres {
		if errFields := s.validatePostgresFields(ctx); errFields != nil {
			return errFields
		}
	}
	s.validatedTargets[backend] = true
	return nil
}

// ExportDatabaseSnapshot writes a versioned portable database snapshot.
func ExportDatabaseSnapshot(ctx context.Context, db *gorm.DB, opts DatabaseSnapshotExportOptions) (DatabaseSnapshotManifest, error) {
	if db == nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("database connection is nil")
	}
	ctx = contextOrBackground(ctx)
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return DatabaseSnapshotManifest{}, fmt.Errorf("database snapshot export path is required")
	}
	backend, errBackend := databaseBackendFromDB(db)
	if errBackend != nil {
		return DatabaseSnapshotManifest{}, errBackend
	}
	if _, errStat := os.Stat(path); errStat == nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("database snapshot export target already exists: %s", path)
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return DatabaseSnapshotManifest{}, fmt.Errorf("inspect database snapshot export target: %w", errStat)
	}

	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("create database snapshot export directory: %w", errMkdir)
	}
	tempFile, errTemp := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if errTemp != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("create database snapshot temporary file: %w", errTemp)
	}
	tempPath := tempFile.Name()
	published := false
	defer func() {
		if !published {
			closeDatabaseSnapshotResource(tempFile, "temporary file")
			if errRemove := os.Remove(tempPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.WithError(errRemove).Warn("failed to remove database snapshot temporary file")
			}
		}
	}()
	if errChmod := tempFile.Chmod(0o600); errChmod != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("secure database snapshot temporary file: %w", errChmod)
	}

	zipWriter := zip.NewWriter(tempFile)
	manifest := DatabaseSnapshotManifest{
		Format:        databaseSnapshotFormat,
		FormatVersion: databaseSnapshotFormatVersion,
		CreatedAt:     time.Now().UTC(),
		HomeVersion:   strings.TrimSpace(opts.HomeVersion),
		HomeCommit:    strings.TrimSpace(opts.HomeCommit),
		SourceBackend: backend,
		Tables:        make([]DatabaseSnapshotManifestTable, 0, len(homeDatabaseModels)),
	}
	errExport := exportDatabaseSnapshotTransaction(ctx, db, backend, zipWriter, &manifest, opts.Progress)
	if errExport != nil {
		closeDatabaseSnapshotResource(zipWriter, "zip writer")
		return DatabaseSnapshotManifest{}, errExport
	}
	manifestWriter, errManifestEntry := zipWriter.CreateHeader(&zip.FileHeader{Name: databaseSnapshotManifestName, Method: zip.Deflate})
	if errManifestEntry != nil {
		closeDatabaseSnapshotResource(zipWriter, "zip writer")
		return DatabaseSnapshotManifest{}, fmt.Errorf("create database snapshot manifest entry: %w", errManifestEntry)
	}
	manifestJSON, errManifestJSON := json.MarshalIndent(manifest, "", "  ")
	if errManifestJSON != nil {
		closeDatabaseSnapshotResource(zipWriter, "zip writer")
		return DatabaseSnapshotManifest{}, fmt.Errorf("encode database snapshot manifest: %w", errManifestJSON)
	}
	manifestJSON = append(manifestJSON, '\n')
	if _, errWriteManifest := manifestWriter.Write(manifestJSON); errWriteManifest != nil {
		closeDatabaseSnapshotResource(zipWriter, "zip writer")
		return DatabaseSnapshotManifest{}, fmt.Errorf("write database snapshot manifest: %w", errWriteManifest)
	}
	if errCloseZIP := zipWriter.Close(); errCloseZIP != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("close database snapshot archive: %w", errCloseZIP)
	}
	if errSync := tempFile.Sync(); errSync != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("sync database snapshot temporary file: %w", errSync)
	}
	if errClose := tempFile.Close(); errClose != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("close database snapshot temporary file: %w", errClose)
	}
	if errPublish := publishDatabaseSnapshot(tempPath, path); errPublish != nil {
		return DatabaseSnapshotManifest{}, errPublish
	}
	published = true
	if errRemove := os.Remove(tempPath); errRemove != nil {
		log.WithError(errRemove).Warn("failed to remove published database snapshot temporary file")
	}
	return manifest, nil
}

// publishDatabaseSnapshot creates the destination as a hard link so an export
// never replaces a file another process created after the initial path check.
// Filesystems without hard link support fall back to an exclusive copy.
func publishDatabaseSnapshot(tempPath string, path string) error {
	if errLink := os.Link(tempPath, path); errLink == nil {
		return nil
	} else if errors.Is(errLink, os.ErrExist) {
		return fmt.Errorf("database snapshot export target already exists: %s", path)
	} else if errCopy := copyDatabaseSnapshotToNewFile(tempPath, path); errCopy != nil {
		if errors.Is(errCopy, os.ErrExist) {
			return fmt.Errorf("database snapshot export target already exists: %s", path)
		}
		return fmt.Errorf("publish database snapshot after hard link failure: %w", errCopy)
	}
	return nil
}

func copyDatabaseSnapshotToNewFile(tempPath string, path string) (err error) {
	source, errOpen := os.Open(tempPath)
	if errOpen != nil {
		return fmt.Errorf("open database snapshot temporary file: %w", errOpen)
	}
	defer closeDatabaseSnapshotResource(source, "published database snapshot temporary file")

	destination, errCreate := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errCreate != nil {
		return errCreate
	}
	removeDestination := true
	defer func() {
		closeDatabaseSnapshotResource(destination, "published database snapshot destination file")
		if removeDestination {
			if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.WithError(errRemove).Warn("failed to remove incomplete database snapshot destination file")
			}
		}
	}()

	if _, errCopy := io.Copy(destination, source); errCopy != nil {
		return fmt.Errorf("copy database snapshot to destination: %w", errCopy)
	}
	if errSync := destination.Sync(); errSync != nil {
		return fmt.Errorf("sync database snapshot destination file: %w", errSync)
	}
	if errClose := destination.Close(); errClose != nil {
		return fmt.Errorf("close database snapshot destination file: %w", errClose)
	}
	removeDestination = false
	return nil
}

func exportDatabaseSnapshotTransaction(ctx context.Context, db *gorm.DB, backend DatabaseBackend, zipWriter *zip.Writer, manifest *DatabaseSnapshotManifest, progress func(DatabaseSnapshotProgress)) error {
	transaction := func(tx *gorm.DB) error {
		for _, model := range homeDatabaseModels {
			if errContext := ctx.Err(); errContext != nil {
				return errContext
			}
			entry, errEntry := zipWriter.CreateHeader(&zip.FileHeader{Name: databaseSnapshotTableEntryName(model.name), Method: zip.Deflate})
			if errEntry != nil {
				return fmt.Errorf("create database snapshot table entry %s: %w", model.name, errEntry)
			}
			hasher := sha256.New()
			rows, errRows := exportDatabaseSnapshotTable(ctx, tx, model, io.MultiWriter(entry, hasher))
			if errRows != nil {
				return errRows
			}
			manifest.Tables = append(manifest.Tables, DatabaseSnapshotManifestTable{
				Name:    model.name,
				Rows:    rows,
				SHA256:  hex.EncodeToString(hasher.Sum(nil)),
				Restore: model.restore,
			})
			if progress != nil {
				progress(DatabaseSnapshotProgress{Operation: "export", Table: model.name, Rows: rows})
			}
		}
		return nil
	}
	if backend == DatabaseBackendPostgres {
		return db.WithContext(ctx).Transaction(transaction, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	}
	return db.WithContext(ctx).Transaction(transaction)
}

func exportDatabaseSnapshotTable(ctx context.Context, tx *gorm.DB, model databaseModel, writer io.Writer) (int64, error) {
	modelSchema, errSchema := schema.Parse(model.newRecord(), &sync.Map{}, schema.NamingStrategy{})
	if errSchema != nil {
		return 0, fmt.Errorf("parse database snapshot export model %s: %w", model.name, errSchema)
	}
	query := tx.WithContext(ctx).Unscoped().Model(model.newRecord())
	for _, column := range model.orderBy {
		query = query.Order(clause.OrderByColumn{Column: clause.Column{Name: column}})
	}
	rows, errRows := query.Rows()
	if errRows != nil {
		return 0, fmt.Errorf("query database snapshot table %s: %w", model.name, errRows)
	}
	defer closeDatabaseSnapshotResource(rows, "database rows")
	var count int64
	for rows.Next() {
		if errContext := ctx.Err(); errContext != nil {
			return count, errContext
		}
		record := model.newRecord()
		if errScan := tx.ScanRows(rows, record); errScan != nil {
			return count, fmt.Errorf("scan database snapshot table %s: %w", model.name, errScan)
		}
		normalizeSnapshotTimes(record)
		if errEncoding := validateDatabaseSnapshotExportRecordEncoding(ctx, model, modelSchema, record); errEncoding != nil {
			return count, errEncoding
		}
		line, errMarshal := json.Marshal(record)
		if errMarshal != nil {
			return count, fmt.Errorf("encode database snapshot table %s row %d: %w", model.name, count+1, errMarshal)
		}
		if _, errEncoding := inspectSnapshotJSON(line); errEncoding != nil {
			return count, fmt.Errorf("encode database snapshot table %s row %d with valid text: %w", model.name, count+1, errEncoding)
		}
		line = append(line, '\n')
		if _, errWrite := writer.Write(line); errWrite != nil {
			return count, fmt.Errorf("write database snapshot table %s row %d: %w", model.name, count+1, errWrite)
		}
		count++
	}
	if errRows := rows.Err(); errRows != nil {
		return count, fmt.Errorf("iterate database snapshot table %s: %w", model.name, errRows)
	}
	return count, nil
}

// OpenDatabaseSnapshot validates the archive and individual records before any
// target database connection is required. Cross-record constraints are checked
// later inside the target import transaction.
func OpenDatabaseSnapshot(ctx context.Context, path string) (*ValidatedDatabaseSnapshot, error) {
	ctx = contextOrBackground(ctx)
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("database snapshot import path is required")
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, fmt.Errorf("open database snapshot: %w", errOpen)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			closeDatabaseSnapshotResource(file, "snapshot file")
		}
	}()
	info, errStat := file.Stat()
	if errStat != nil {
		return nil, fmt.Errorf("stat database snapshot: %w", errStat)
	}
	reader, errZIP := zip.NewReader(file, info.Size())
	if errZIP != nil {
		return nil, fmt.Errorf("open database snapshot archive: %w", errZIP)
	}
	entries, errEntries := validateDatabaseSnapshotEntries(reader)
	if errEntries != nil {
		return nil, errEntries
	}
	manifest, errManifest := readDatabaseSnapshotManifest(entries[databaseSnapshotManifestName])
	if errManifest != nil {
		return nil, errManifest
	}
	if manifest.FormatVersion > databaseSnapshotFormatVersion {
		return nil, fmt.Errorf("database snapshot format version %d is newer than supported version %d", manifest.FormatVersion, databaseSnapshotFormatVersion)
	}
	models, okModels := databaseSnapshotModels(manifest.FormatVersion)
	if !okModels {
		return nil, fmt.Errorf("unsupported database snapshot format version %d", manifest.FormatVersion)
	}
	if errEntries := validateDatabaseSnapshotEntrySet(entries, models); errEntries != nil {
		return nil, errEntries
	}
	if errManifest := validateDatabaseSnapshotManifest(manifest, entries, models); errManifest != nil {
		return nil, errManifest
	}
	for index, table := range manifest.Tables {
		model := models[index]
		if errTable := validateDatabaseSnapshotTable(ctx, entries[databaseSnapshotTableEntryName(table.Name)], model, table); errTable != nil {
			return nil, errTable
		}
	}
	closeOnError = false
	return &ValidatedDatabaseSnapshot{
		file:             file,
		reader:           reader,
		entries:          entries,
		manifest:         manifest,
		models:           models,
		validatedTargets: make(map[DatabaseBackend]bool, 2),
	}, nil
}

func validateDatabaseSnapshotEntries(reader *zip.Reader) (map[string]*zip.File, error) {
	entries := make(map[string]*zip.File, len(reader.File))
	for _, entry := range reader.File {
		if entry == nil {
			return nil, fmt.Errorf("database snapshot contains an invalid zip entry")
		}
		if _, duplicate := entries[entry.Name]; duplicate {
			return nil, fmt.Errorf("database snapshot contains duplicate zip entry %q", entry.Name)
		}
		if entry.FileInfo().IsDir() || entry.FileInfo().Mode()&os.ModeType != 0 {
			return nil, fmt.Errorf("database snapshot zip entry %q is not a regular file", entry.Name)
		}
		entries[entry.Name] = entry
	}
	if entries[databaseSnapshotManifestName] == nil {
		return nil, fmt.Errorf("database snapshot is missing zip entry %q", databaseSnapshotManifestName)
	}
	return entries, nil
}

func validateDatabaseSnapshotEntrySet(entries map[string]*zip.File, models []databaseModel) error {
	allowed := map[string]struct{}{databaseSnapshotManifestName: {}}
	for _, model := range models {
		allowed[databaseSnapshotTableEntryName(model.name)] = struct{}{}
	}
	if len(entries) != len(allowed) {
		return fmt.Errorf("database snapshot zip entry set is incomplete")
	}
	for name := range entries {
		if _, okAllowed := allowed[name]; !okAllowed {
			return fmt.Errorf("database snapshot contains disallowed zip entry %q", name)
		}
	}
	for name := range allowed {
		if entries[name] == nil {
			return fmt.Errorf("database snapshot is missing zip entry %q", name)
		}
	}
	return nil
}

func readDatabaseSnapshotManifest(entry *zip.File) (DatabaseSnapshotManifest, error) {
	if entry == nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("database snapshot manifest entry is missing")
	}
	if entry.UncompressedSize64 > databaseSnapshotManifestLimit {
		return DatabaseSnapshotManifest{}, fmt.Errorf("database snapshot manifest exceeds %d bytes", databaseSnapshotManifestLimit)
	}
	reader, errOpen := entry.Open()
	if errOpen != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("open database snapshot manifest: %w", errOpen)
	}
	defer closeDatabaseSnapshotResource(reader, "manifest entry")
	raw, errRead := io.ReadAll(io.LimitReader(reader, databaseSnapshotManifestLimit+1))
	if errRead != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("read database snapshot manifest: %w", errRead)
	}
	if len(raw) > databaseSnapshotManifestLimit {
		return DatabaseSnapshotManifest{}, fmt.Errorf("database snapshot manifest exceeds %d bytes", databaseSnapshotManifestLimit)
	}
	manifest := DatabaseSnapshotManifest{}
	if errDecode := decodeStrictJSON(raw, &manifest); errDecode != nil {
		return DatabaseSnapshotManifest{}, fmt.Errorf("decode database snapshot manifest: %w", errDecode)
	}
	return manifest, nil
}

func validateDatabaseSnapshotManifest(manifest DatabaseSnapshotManifest, entries map[string]*zip.File, models []databaseModel) error {
	if manifest.Format != databaseSnapshotFormat {
		return fmt.Errorf("unsupported database snapshot format %q", manifest.Format)
	}
	if manifest.FormatVersion > databaseSnapshotFormatVersion {
		return fmt.Errorf("database snapshot format version %d is newer than supported version %d", manifest.FormatVersion, databaseSnapshotFormatVersion)
	}
	if manifest.FormatVersion < 1 {
		return fmt.Errorf("unsupported database snapshot format version %d", manifest.FormatVersion)
	}
	if manifest.CreatedAt.IsZero() {
		return fmt.Errorf("database snapshot created_at is required")
	}
	_, createdAtOffset := manifest.CreatedAt.Zone()
	if createdAtOffset != 0 {
		return fmt.Errorf("database snapshot created_at must use UTC")
	}
	switch manifest.SourceBackend {
	case DatabaseBackendSQLite, DatabaseBackendPostgres:
	default:
		return fmt.Errorf("unsupported database snapshot source backend %q", manifest.SourceBackend)
	}
	if len(manifest.Tables) != len(models) {
		return fmt.Errorf("database snapshot manifest contains %d tables, want %d", len(manifest.Tables), len(models))
	}
	for index, model := range models {
		table := manifest.Tables[index]
		if table.Name != model.name {
			return fmt.Errorf("database snapshot table %d is %q, want %q", index, table.Name, model.name)
		}
		if table.Rows < 0 {
			return fmt.Errorf("database snapshot table %s has a negative row count", table.Name)
		}
		if table.Restore != model.restore {
			return fmt.Errorf("database snapshot table %s restore policy does not match format version %d", table.Name, manifest.FormatVersion)
		}
		checksum, errChecksum := hex.DecodeString(table.SHA256)
		if errChecksum != nil || len(checksum) != sha256.Size || table.SHA256 != strings.ToLower(table.SHA256) {
			return fmt.Errorf("database snapshot table %s has an invalid sha256", table.Name)
		}
		if entries[databaseSnapshotTableEntryName(table.Name)] == nil {
			return fmt.Errorf("database snapshot table %s entry is missing", table.Name)
		}
	}
	return nil
}

func validateDatabaseSnapshotTable(ctx context.Context, entry *zip.File, model databaseModel, manifest DatabaseSnapshotManifestTable) error {
	reader, errOpen := entry.Open()
	if errOpen != nil {
		return fmt.Errorf("open database snapshot table %s: %w", model.name, errOpen)
	}
	defer closeDatabaseSnapshotResource(reader, "table entry")
	hasher := sha256.New()
	lineReader := bufio.NewReader(io.TeeReader(reader, hasher))
	modelSchema, errSchema := schema.Parse(model.newRecord(), &sync.Map{}, schema.NamingStrategy{})
	if errSchema != nil {
		return fmt.Errorf("parse database snapshot model %s: %w", model.name, errSchema)
	}
	var rows int64
	for {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		line, errLine := lineReader.ReadBytes('\n')
		if len(line) > 0 {
			rows++
			if line[len(line)-1] != '\n' {
				return fmt.Errorf("database snapshot table %s row %d is not newline terminated", model.name, rows)
			}
			line = bytes.TrimSuffix(line, []byte{'\n'})
			if len(line) == 0 {
				return fmt.Errorf("database snapshot table %s row %d is empty", model.name, rows)
			}
			if _, errEncoding := inspectSnapshotJSON(line); errEncoding != nil {
				return fmt.Errorf("database snapshot table %s row %d has invalid text encoding: %w", model.name, rows, errEncoding)
			}
			record := model.newRecord()
			if errDecode := decodeStrictJSON(line, record); errDecode != nil {
				return fmt.Errorf("database snapshot table %s row %d is invalid: %w", model.name, rows, errDecode)
			}
			normalizeSnapshotTimes(record)
			if errRecord := validateDatabaseSnapshotRecord(ctx, model, modelSchema, line, record); errRecord != nil {
				return errRecord
			}
		}
		if errors.Is(errLine, io.EOF) {
			break
		}
		if errLine != nil {
			return fmt.Errorf("read database snapshot table %s row %d: %w", model.name, rows+1, errLine)
		}
	}
	if rows != manifest.Rows {
		return fmt.Errorf("database snapshot table %s row count is %d, manifest declares %d", model.name, rows, manifest.Rows)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))
	if checksum != manifest.SHA256 {
		return fmt.Errorf("database snapshot table %s checksum mismatch", model.name)
	}
	return nil
}

func validateDatabaseSnapshotRecord(ctx context.Context, model databaseModel, modelSchema *schema.Schema, raw []byte, record any) error {
	value := reflect.ValueOf(record)
	if value.Kind() != reflect.Ptr || value.IsNil() {
		return fmt.Errorf("database snapshot table %s record factory returned an invalid value", model.name)
	}
	value = value.Elem()
	primaryLabel := databaseSnapshotPrimaryLabel(ctx, modelSchema, value)
	var fields map[string]json.RawMessage
	if errRaw := json.Unmarshal(raw, &fields); errRaw != nil {
		return fmt.Errorf("database snapshot table %s record %s is not an object: %w", model.name, primaryLabel, errRaw)
	}
	for _, field := range modelSchema.Fields {
		fieldValue, _ := field.ValueOf(ctx, value)
		if field.NotNull {
			rawField, present := fields[field.Name]
			if !present || bytes.Equal(bytes.TrimSpace(rawField), []byte("null")) {
				return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, "must not be null")
			}
		}
		if !snapshotFiniteValue(reflect.ValueOf(fieldValue)) {
			return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, "contains NaN or Inf")
		}
	}
	if model.autoIncrement {
		for _, field := range modelSchema.Fields {
			if !field.AutoIncrement {
				continue
			}
			_, zero := field.ValueOf(ctx, value)
			if zero {
				return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, "must be non-zero for an auto-increment record")
			}
		}
	}
	switch typed := record.(type) {
	case *ChannelGroupDetailRecord:
		if strings.TrimSpace(typed.AuthID) == "" {
			return databaseSnapshotFieldError(model.name, primaryLabel, "auth_id", "must not be blank")
		}
	case *QuotaSnapshotRecord:
		if strings.TrimSpace(typed.CredentialID) == "" {
			return databaseSnapshotFieldError(model.name, primaryLabel, "credential_id", "must not be blank")
		}
	}
	return nil
}

func (s *ValidatedDatabaseSnapshot) validatePostgresFields(ctx context.Context) error {
	for _, model := range s.models {
		if !model.restore {
			continue
		}
		entry := s.entries[databaseSnapshotTableEntryName(model.name)]
		reader, errOpen := entry.Open()
		if errOpen != nil {
			return fmt.Errorf("open database snapshot table %s for postgres preflight: %w", model.name, errOpen)
		}
		modelSchema, errSchema := schema.Parse(model.newRecord(), &sync.Map{}, schema.NamingStrategy{})
		if errSchema != nil {
			closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
			return fmt.Errorf("parse database snapshot model %s for postgres preflight: %w", model.name, errSchema)
		}
		lineReader := bufio.NewReader(reader)
		var row int64
		for {
			if errContext := ctx.Err(); errContext != nil {
				closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
				return errContext
			}
			line, errLine := lineReader.ReadBytes('\n')
			if len(line) > 0 {
				row++
				line = bytes.TrimSuffix(line, []byte{'\n'})
				record := model.newRecord()
				if errDecode := decodeStrictJSON(line, record); errDecode != nil {
					closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
					return fmt.Errorf("decode validated database snapshot table %s row %d for postgres preflight: %w", model.name, row, errDecode)
				}
				value := reflect.ValueOf(record).Elem()
				primaryLabel := databaseSnapshotPrimaryLabel(ctx, modelSchema, value)
				for _, field := range modelSchema.Fields {
					fieldValue, _ := field.ValueOf(ctx, value)
					if snapshotUnsignedValueExceedsPostgres(fieldValue) {
						closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
						return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, "exceeds the postgres signed integer range")
					}
					containsNUL, errNUL := snapshotPostgresValueContainsNUL(fieldValue)
					if errNUL != nil {
						closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
						return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, fmt.Sprintf("cannot be checked for postgres NUL compatibility: %v", errNUL))
					}
					if containsNUL {
						closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
						return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, "contains a NUL character unsupported by postgres")
					}
					if field.TagSettings["SIZE"] == "" || field.Size <= 0 {
						continue
					}
					switch typed := fieldValue.(type) {
					case string:
						if utf8.RuneCountInString(typed) > field.Size {
							closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
							return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, fmt.Sprintf("exceeds postgres declared size %d", field.Size))
						}
					case []byte:
						if len(typed) > field.Size {
							closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
							return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, fmt.Sprintf("exceeds postgres declared size %d", field.Size))
						}
					}
				}
			}
			if errors.Is(errLine, io.EOF) {
				break
			}
			if errLine != nil {
				closeDatabaseSnapshotResource(reader, "postgres preflight table entry")
				return fmt.Errorf("read database snapshot table %s for postgres preflight: %w", model.name, errLine)
			}
		}
		if errClose := reader.Close(); errClose != nil {
			return fmt.Errorf("close database snapshot table %s after postgres preflight: %w", model.name, errClose)
		}
	}
	return nil
}

var snapshotJSONBType = reflect.TypeOf(JSONB(nil))

func validateDatabaseSnapshotExportRecordEncoding(ctx context.Context, model databaseModel, modelSchema *schema.Schema, record any) error {
	value := reflect.ValueOf(record)
	if modelSchema == nil || value.Kind() != reflect.Ptr || value.IsNil() {
		return fmt.Errorf("database snapshot table %s export record is invalid", model.name)
	}
	value = value.Elem()
	primaryLabel := databaseSnapshotPrimaryLabel(ctx, modelSchema, value)
	for _, field := range modelSchema.Fields {
		fieldValue, _ := field.ValueOf(ctx, value)
		if errEncoding := validateSnapshotValueEncoding(reflect.ValueOf(fieldValue)); errEncoding != nil {
			return databaseSnapshotFieldError(model.name, primaryLabel, field.DBName, errEncoding.Error())
		}
	}
	return nil
}

func validateSnapshotValueEncoding(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if value.Type() == snapshotJSONBType {
		raw := value.Interface().(JSONB)
		if len(raw) == 0 {
			return nil
		}
		if _, errJSON := inspectSnapshotJSON(raw); errJSON != nil {
			return fmt.Errorf("contains invalid JSON encoding: %w", errJSON)
		}
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("contains invalid UTF-8")
		}
	case reflect.Array, reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for index := 0; index < value.Len(); index++ {
			if errEncoding := validateSnapshotValueEncoding(value.Index(index)); errEncoding != nil {
				return errEncoding
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if errEncoding := validateSnapshotValueEncoding(iterator.Key()); errEncoding != nil {
				return errEncoding
			}
			if errEncoding := validateSnapshotValueEncoding(iterator.Value()); errEncoding != nil {
				return errEncoding
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).PkgPath == "" {
				if errEncoding := validateSnapshotValueEncoding(value.Field(index)); errEncoding != nil {
					return errEncoding
				}
			}
		}
	}
	return nil
}

func snapshotPostgresValueContainsNUL(value any) (bool, error) {
	reflected := reflect.ValueOf(value)
	for reflected.IsValid() && (reflected.Kind() == reflect.Interface || reflected.Kind() == reflect.Ptr) {
		if reflected.IsNil() {
			return false, nil
		}
		reflected = reflected.Elem()
	}
	if !reflected.IsValid() {
		return false, nil
	}
	if reflected.Type() == snapshotJSONBType {
		return snapshotJSONContainsNUL(reflected.Interface().(JSONB))
	}
	if reflected.Kind() == reflect.String {
		return strings.IndexByte(reflected.String(), 0) >= 0, nil
	}
	return false, nil
}

func snapshotJSONContainsNUL(raw JSONB) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	return inspectSnapshotJSON(raw)
}

func inspectSnapshotJSON(raw []byte) (bool, error) {
	if !utf8.Valid(raw) {
		return false, fmt.Errorf("JSON contains invalid UTF-8")
	}
	if !json.Valid(raw) {
		return false, fmt.Errorf("invalid JSON")
	}
	containsNUL := false
	for index := 0; index < len(raw); {
		if raw[index] != '"' {
			index++
			continue
		}
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] != '\\' {
				_, size := utf8.DecodeRune(raw[index:])
				index += size
				continue
			}
			escapeStart := index
			if escapeStart+2 > len(raw) {
				return false, fmt.Errorf("JSON contains an incomplete escape at byte %d", escapeStart)
			}
			if raw[escapeStart+1] != 'u' {
				index += 2
				continue
			}
			codePoint, okCodePoint := decodeSnapshotJSONUnicodeEscape(raw, escapeStart)
			if !okCodePoint {
				return false, fmt.Errorf("JSON contains an invalid Unicode escape at byte %d", escapeStart)
			}
			index += 6
			if codePoint == 0 {
				containsNUL = true
			}
			switch {
			case codePoint >= 0xd800 && codePoint <= 0xdbff:
				lowSurrogate, okLowSurrogate := decodeSnapshotJSONUnicodeEscape(raw, index)
				if !okLowSurrogate || lowSurrogate < 0xdc00 || lowSurrogate > 0xdfff {
					return false, fmt.Errorf("JSON contains an unpaired high surrogate escape at byte %d", escapeStart)
				}
				index += 6
			case codePoint >= 0xdc00 && codePoint <= 0xdfff:
				return false, fmt.Errorf("JSON contains an unpaired low surrogate escape at byte %d", escapeStart)
			}
		}
		index++
	}
	return containsNUL, nil
}

func decodeSnapshotJSONUnicodeEscape(raw []byte, offset int) (uint16, bool) {
	if offset < 0 || offset+6 > len(raw) || raw[offset] != '\\' || raw[offset+1] != 'u' {
		return 0, false
	}
	var value uint16
	for _, digit := range raw[offset+2 : offset+6] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func snapshotUnsignedValueExceedsPostgres(value any) bool {
	reflected := reflect.ValueOf(value)
	for reflected.IsValid() && (reflected.Kind() == reflect.Interface || reflected.Kind() == reflect.Ptr) {
		if reflected.IsNil() {
			return false
		}
		reflected = reflected.Elem()
	}
	if !reflected.IsValid() {
		return false
	}
	switch reflected.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflected.Uint() > math.MaxInt64
	default:
		return false
	}
}

func databaseSnapshotFieldError(table string, primary string, field string, reason string) error {
	return fmt.Errorf("database snapshot table %s record %s field %s %s", table, primary, field, reason)
}

func databaseSnapshotPrimaryLabel(ctx context.Context, modelSchema *schema.Schema, value reflect.Value) string {
	key := databaseSnapshotKey(ctx, modelSchema.PrimaryFields, value)
	if key == "" || key == "[]" {
		return "<unknown>"
	}
	return key
}

func databaseSnapshotKey(ctx context.Context, fields []*schema.Field, value reflect.Value) string {
	values := make([]any, 0, len(fields))
	for _, field := range fields {
		fieldValue, _ := field.ValueOf(ctx, value)
		values = append(values, fieldValue)
	}
	raw, errMarshal := json.Marshal(values)
	if errMarshal != nil {
		return fmt.Sprintf("%v", values)
	}
	return string(raw)
}

func snapshotFiniteValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return true
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Float32, reflect.Float64:
		floatValue := value.Float()
		return !math.IsNaN(floatValue) && !math.IsInf(floatValue, 0)
	case reflect.Array, reflect.Slice:
		for index := 0; index < value.Len(); index++ {
			if !snapshotFiniteValue(value.Index(index)) {
				return false
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if !snapshotFiniteValue(iterator.Value()) {
				return false
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).PkgPath == "" && !snapshotFiniteValue(value.Field(index)) {
				return false
			}
		}
	}
	return true
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(target); errDecode != nil {
		return errDecode
	}
	var extra any
	if errExtra := decoder.Decode(&extra); !errors.Is(errExtra, io.EOF) {
		if errExtra == nil {
			return fmt.Errorf("multiple json values are not allowed")
		}
		return errExtra
	}
	return nil
}

func normalizeSnapshotTimes(record any) {
	normalizeSnapshotTimeValue(reflect.ValueOf(record))
}

var snapshotTimeType = reflect.TypeOf(time.Time{})

func normalizeSnapshotTimeValue(value reflect.Value) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return
		}
		normalizeSnapshotTimeValue(value.Elem())
		return
	}
	if value.Type() == snapshotTimeType {
		if value.CanSet() {
			value.Set(reflect.ValueOf(value.Interface().(time.Time).UTC()))
		}
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).PkgPath == "" {
				normalizeSnapshotTimeValue(value.Field(index))
			}
		}
	case reflect.Array, reflect.Slice:
		for index := 0; index < value.Len(); index++ {
			normalizeSnapshotTimeValue(value.Index(index))
		}
	}
}

// ImportDatabaseSnapshot restores persistent business tables into an empty
// target database. Runtime-only tables remain untouched and are reported as
// skipped.
func ImportDatabaseSnapshot(ctx context.Context, db *gorm.DB, snapshot *ValidatedDatabaseSnapshot, progress func(DatabaseSnapshotProgress)) (DatabaseSnapshotImportResult, error) {
	if db == nil {
		return DatabaseSnapshotImportResult{}, fmt.Errorf("database connection is nil")
	}
	if snapshot == nil || snapshot.file == nil {
		return DatabaseSnapshotImportResult{}, fmt.Errorf("database snapshot is not open")
	}
	ctx = contextOrBackground(ctx)
	backend, errBackend := databaseBackendFromDB(db)
	if errBackend != nil {
		return DatabaseSnapshotImportResult{}, errBackend
	}
	if errValidate := snapshot.ValidateForBackend(ctx, backend); errValidate != nil {
		return DatabaseSnapshotImportResult{}, errValidate
	}
	if errEmpty := ensureExistingDatabaseSnapshotTargetEmpty(ctx, db); errEmpty != nil {
		return DatabaseSnapshotImportResult{}, errEmpty
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		return DatabaseSnapshotImportResult{}, fmt.Errorf("migrate database before snapshot import: %w", errMigrate)
	}

	result := DatabaseSnapshotImportResult{Tables: make([]DatabaseSnapshotImportTableResult, 0, len(snapshot.models))}
	now := time.Now().UTC()
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errEmpty := ensureDatabaseSnapshotTargetEmpty(ctx, tx); errEmpty != nil {
			return errEmpty
		}
		for index, model := range snapshot.models {
			manifestTable := snapshot.manifest.Tables[index]
			tableResult := DatabaseSnapshotImportTableResult{Name: model.name}
			if !model.restore {
				tableResult.Skipped = manifestTable.Rows
				result.Tables = append(result.Tables, tableResult)
				if progress != nil {
					progress(DatabaseSnapshotProgress{Operation: "import", Table: model.name, Skipped: tableResult.Skipped})
				}
				continue
			}
			imported, skipped, errImport := importDatabaseSnapshotTable(ctx, tx, snapshot.entries[databaseSnapshotTableEntryName(model.name)], model, now)
			if errImport != nil {
				return errImport
			}
			tableResult.Imported = imported
			tableResult.Skipped = skipped
			result.Tables = append(result.Tables, tableResult)
			if progress != nil {
				progress(DatabaseSnapshotProgress{Operation: "import", Table: model.name, Rows: imported, Skipped: skipped})
			}
		}
		if errRelationships := validateImportedDatabaseSnapshotRelationships(tx); errRelationships != nil {
			return errRelationships
		}
		if errCounts := validateDatabaseSnapshotImportCounts(tx, result, snapshot.models); errCounts != nil {
			return errCounts
		}
		if errSequences := resetDatabaseSnapshotSequences(tx, backend, snapshot.models); errSequences != nil {
			return errSequences
		}
		return nil
	})
	if errTransaction != nil {
		return DatabaseSnapshotImportResult{}, fmt.Errorf("import database snapshot: %w", errTransaction)
	}
	return result, nil
}

func ensureExistingDatabaseSnapshotTargetEmpty(ctx context.Context, db *gorm.DB) error {
	for _, model := range homeDatabaseModels {
		if !model.restore {
			continue
		}
		scoped := db.WithContext(ctx)
		if !scoped.Migrator().HasTable(model.newRecord()) {
			continue
		}
		if errEmpty := ensureDatabaseSnapshotTableEmpty(ctx, scoped, model); errEmpty != nil {
			return errEmpty
		}
	}
	return nil
}

func ensureDatabaseSnapshotTargetEmpty(ctx context.Context, db *gorm.DB) error {
	for _, model := range homeDatabaseModels {
		if !model.restore {
			continue
		}
		if errEmpty := ensureDatabaseSnapshotTableEmpty(ctx, db, model); errEmpty != nil {
			return errEmpty
		}
	}
	return nil
}

func ensureDatabaseSnapshotTableEmpty(ctx context.Context, db *gorm.DB, model databaseModel) error {
	var count int64
	if errCount := db.WithContext(ctx).Unscoped().Model(model.newRecord()).Count(&count).Error; errCount != nil {
		return fmt.Errorf("count target database table %s: %w", model.name, errCount)
	}
	if count != 0 {
		return fmt.Errorf("target database business table %s is not empty", model.name)
	}
	return nil
}

func importDatabaseSnapshotTable(ctx context.Context, tx *gorm.DB, entry *zip.File, model databaseModel, now time.Time) (int64, int64, error) {
	reader, errOpen := entry.Open()
	if errOpen != nil {
		return 0, 0, fmt.Errorf("open database snapshot table %s for import: %w", model.name, errOpen)
	}
	defer closeDatabaseSnapshotResource(reader, "import table entry")
	lineReader := bufio.NewReader(reader)
	batch := model.newBatch()
	batchValue := reflect.ValueOf(batch)
	if batchValue.Kind() != reflect.Ptr || batchValue.Elem().Kind() != reflect.Slice {
		return 0, 0, fmt.Errorf("database snapshot table %s batch factory returned an invalid value", model.name)
	}
	modelSchema, errSchema := schema.Parse(model.newRecord(), &sync.Map{}, schema.NamingStrategy{})
	if errSchema != nil {
		return 0, 0, fmt.Errorf("parse database snapshot import model %s: %w", model.name, errSchema)
	}
	var imported int64
	var skipped int64
	flush := func() error {
		if batchValue.Elem().Len() == 0 {
			return nil
		}
		rows, errRows := databaseSnapshotBatchRows(ctx, modelSchema, batchValue.Elem())
		if errRows != nil {
			return errRows
		}
		if errCreate := tx.WithContext(ctx).Table(model.name).CreateInBatches(rows, databaseSnapshotBatchSize).Error; errCreate != nil {
			return errCreate
		}
		batchValue.Elem().SetLen(0)
		return nil
	}
	for {
		if errContext := ctx.Err(); errContext != nil {
			return imported, skipped, errContext
		}
		line, errLine := lineReader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSuffix(line, []byte{'\n'})
			record := model.newRecord()
			if errDecode := decodeStrictJSON(line, record); errDecode != nil {
				return imported, skipped, fmt.Errorf("decode validated database snapshot table %s: %w", model.name, errDecode)
			}
			normalizeSnapshotTimes(record)
			if !restoreDatabaseSnapshotRecord(record, now) {
				skipped++
			} else {
				recordValue := reflect.ValueOf(record)
				batchValue.Elem().Set(reflect.Append(batchValue.Elem(), recordValue.Elem()))
				imported++
				if batchValue.Elem().Len() >= databaseSnapshotBatchSize {
					if errFlush := flush(); errFlush != nil {
						return imported, skipped, fmt.Errorf("insert database snapshot table %s: %w", model.name, errFlush)
					}
				}
			}
		}
		if errors.Is(errLine, io.EOF) {
			break
		}
		if errLine != nil {
			return imported, skipped, fmt.Errorf("read database snapshot table %s for import: %w", model.name, errLine)
		}
	}
	if errFlush := flush(); errFlush != nil {
		return imported, skipped, fmt.Errorf("insert database snapshot table %s: %w", model.name, errFlush)
	}
	return imported, skipped, nil
}

func databaseSnapshotBatchRows(ctx context.Context, modelSchema *schema.Schema, batch reflect.Value) ([]map[string]any, error) {
	if modelSchema == nil || batch.Kind() != reflect.Slice {
		return nil, fmt.Errorf("database snapshot import batch is invalid")
	}
	rows := make([]map[string]any, batch.Len())
	for index := 0; index < batch.Len(); index++ {
		row := make(map[string]any, len(modelSchema.DBNames))
		for _, column := range modelSchema.DBNames {
			field := modelSchema.FieldsByDBName[column]
			if field == nil {
				return nil, fmt.Errorf("database snapshot import model %s is missing field for column %s", modelSchema.Table, column)
			}
			value, _ := field.ValueOf(ctx, batch.Index(index))
			row[column] = value
		}
		rows[index] = row
	}
	return rows, nil
}

func restoreDatabaseSnapshotRecord(record any, now time.Time) bool {
	kvRecord, okKV := record.(*KVRecord)
	if !okKV {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(kvRecord.Key)), "internal:migration:") {
		return false
	}
	return kvRecord.ExpiresAt == nil || kvRecord.ExpiresAt.After(now)
}

func validateDatabaseSnapshotImportCounts(tx *gorm.DB, result DatabaseSnapshotImportResult, models []databaseModel) error {
	results := make(map[string]DatabaseSnapshotImportTableResult, len(result.Tables))
	for _, table := range result.Tables {
		results[table.Name] = table
	}
	for _, model := range models {
		if !model.restore {
			continue
		}
		var count int64
		if errCount := tx.Unscoped().Model(model.newRecord()).Count(&count).Error; errCount != nil {
			return fmt.Errorf("verify imported database snapshot table %s: %w", model.name, errCount)
		}
		if count != results[model.name].Imported {
			return fmt.Errorf("verify imported database snapshot table %s: found %d rows, want %d", model.name, count, results[model.name].Imported)
		}
	}
	return nil
}

func validateImportedDatabaseSnapshotRelationships(tx *gorm.DB) error {
	checks := []struct {
		name  string
		query string
	}{
		{name: "plugin_store_auth.key_version", query: `SELECT COUNT(*) FROM "plugin_store_auth" AS child WHERE NOT EXISTS (SELECT 1 FROM "plugin_store_auth_key" AS parent WHERE parent."key_version" = child."key_version")`},
		{name: "api_key.user_id", query: `SELECT COUNT(*) FROM "api_key" AS child WHERE child."user_id" IS NOT NULL AND NOT EXISTS (SELECT 1 FROM "user" AS parent WHERE parent."id" = child."user_id")`},
		{name: "channel_group_detail.channel_group_id", query: `SELECT COUNT(*) FROM "channel_group_detail" AS child WHERE NOT EXISTS (SELECT 1 FROM "channel_group" AS parent WHERE parent."id" = child."channel_group_id")`},
		{name: "channel_group_detail.auth_id", query: `SELECT COUNT(*) FROM "channel_group_detail" AS child WHERE NOT EXISTS (SELECT 1 FROM "auth" AS parent WHERE parent."uuid" = child."auth_id" OR parent."id" = child."auth_id" OR parent."index" = child."auth_id")`},
		{name: "model_group_detail.model_group_id", query: `SELECT COUNT(*) FROM "model_group_detail" AS child WHERE NOT EXISTS (SELECT 1 FROM "model_group" AS parent WHERE parent."id" = child."model_group_id")`},
		{name: "quota_snapshot.credential_id", query: `SELECT COUNT(*) FROM "quota_snapshot" AS child WHERE NOT EXISTS (SELECT 1 FROM "auth" AS parent WHERE parent."uuid" = child."credential_id" OR parent."id" = child."credential_id" OR parent."index" = child."credential_id")`},
		{name: "quota_window.credential_id", query: `SELECT COUNT(*) FROM "quota_window" AS child WHERE NOT EXISTS (SELECT 1 FROM "quota_snapshot" AS parent WHERE parent."credential_id" = child."credential_id")`},
		{name: "billing_balance_record.user_id", query: `SELECT COUNT(*) FROM "billing_balance_record" AS child WHERE NOT EXISTS (SELECT 1 FROM "user" AS parent WHERE parent."id" = child."user_id")`},
		{name: "billing_charge.usage_id", query: `SELECT COUNT(*) FROM "billing_charge" AS child WHERE NOT EXISTS (SELECT 1 FROM "usage" AS parent WHERE parent."id" = child."usage_id")`},
		{name: "billing_charge.user_id", query: `SELECT COUNT(*) FROM "billing_charge" AS child WHERE child."user_id" IS NOT NULL AND NOT EXISTS (SELECT 1 FROM "user" AS parent WHERE parent."id" = child."user_id")`},
		{name: "billing_charge.api_key_id", query: `SELECT COUNT(*) FROM "billing_charge" AS child WHERE child."api_key_id" IS NOT NULL AND NOT EXISTS (SELECT 1 FROM "api_key" AS parent WHERE parent."id" = child."api_key_id")`},
		{name: "credential_concurrency_policies.credential_id", query: `SELECT COUNT(*) FROM "credential_concurrency_policies" AS child WHERE NOT EXISTS (SELECT 1 FROM "auth" AS parent WHERE parent."uuid" = child."credential_id")`},
		{name: "credential_concurrency_model_policies.credential_id", query: `SELECT COUNT(*) FROM "credential_concurrency_model_policies" AS child WHERE NOT EXISTS (SELECT 1 FROM "credential_concurrency_policies" AS parent WHERE parent."credential_id" = child."credential_id")`},
	}
	for _, check := range checks {
		var count int64
		if errCheck := tx.Raw(check.query).Scan(&count).Error; errCheck != nil {
			return fmt.Errorf("verify imported database snapshot relationship %s: %w", check.name, errCheck)
		}
		if count != 0 {
			return fmt.Errorf("verify imported database snapshot relationship %s: found %d orphaned records", check.name, count)
		}
	}
	if errGroups := validateImportedDatabaseSnapshotAPIKeyGroups(tx); errGroups != nil {
		return errGroups
	}
	return validateImportedDatabaseSnapshotConcurrencyState(tx)
}

func validateImportedDatabaseSnapshotConcurrencyState(tx *gorm.DB) error {
	var invalidGateIDs int64
	if errCount := tx.Model(&ConcurrencyActivationGateRecord{}).Where("id <> ?", 1).Count(&invalidGateIDs).Error; errCount != nil {
		return fmt.Errorf("verify imported database snapshot concurrency activation gate ids: %w", errCount)
	}
	if invalidGateIDs != 0 {
		return fmt.Errorf("verify imported database snapshot concurrency activation gate: found %d records outside singleton id 1", invalidGateIDs)
	}

	var activePolicyCount int64
	if errCount := tx.Raw(`SELECT COUNT(*) FROM "credential_concurrency_policies" AS policy WHERE policy."max_in_flight" IS NOT NULL OR EXISTS (SELECT 1 FROM "credential_concurrency_model_policies" AS model_policy WHERE model_policy."credential_id" = policy."credential_id")`).Scan(&activePolicyCount).Error; errCount != nil {
		return fmt.Errorf("verify imported database snapshot active concurrency policy count: %w", errCount)
	}
	activationGate := ConcurrencyActivationGateRecord{}
	errGate := tx.First(&activationGate, "id = ?", 1).Error
	if errors.Is(errGate, gorm.ErrRecordNotFound) {
		if activePolicyCount != 0 {
			return fmt.Errorf("verify imported database snapshot concurrency activation gate: missing singleton for %d active policies", activePolicyCount)
		}
	} else if errGate != nil {
		return fmt.Errorf("verify imported database snapshot concurrency activation gate: %w", errGate)
	} else if activationGate.ActivePolicyCount != activePolicyCount {
		return fmt.Errorf("verify imported database snapshot concurrency activation gate: active policy count is %d, want %d", activationGate.ActivePolicyCount, activePolicyCount)
	}

	var invalidBarrierIDs int64
	if errCount := tx.Model(&ConcurrencyObservationBarrierRecord{}).Where("id <> ?", 1).Count(&invalidBarrierIDs).Error; errCount != nil {
		return fmt.Errorf("verify imported database snapshot concurrency observation barrier ids: %w", errCount)
	}
	if invalidBarrierIDs != 0 {
		return fmt.Errorf("verify imported database snapshot concurrency observation barrier: found %d records outside singleton id 1", invalidBarrierIDs)
	}

	var maximumPolicyRevision sql.NullInt64
	if errRevision := tx.Model(&CredentialConcurrencyPolicyRecord{}).Select("MAX(observation_barrier_revision)").Scan(&maximumPolicyRevision).Error; errRevision != nil {
		return fmt.Errorf("verify imported database snapshot concurrency policy barrier revisions: %w", errRevision)
	}
	if !maximumPolicyRevision.Valid {
		return nil
	}
	barrier := ConcurrencyObservationBarrierRecord{}
	if errBarrier := tx.First(&barrier, "id = ?", 1).Error; errors.Is(errBarrier, gorm.ErrRecordNotFound) {
		return fmt.Errorf("verify imported database snapshot concurrency observation barrier: missing singleton for policy revision %d", maximumPolicyRevision.Int64)
	} else if errBarrier != nil {
		return fmt.Errorf("verify imported database snapshot concurrency observation barrier: %w", errBarrier)
	}
	if barrier.Revision < maximumPolicyRevision.Int64 {
		return fmt.Errorf("verify imported database snapshot concurrency observation barrier: revision is %d, want at least %d", barrier.Revision, maximumPolicyRevision.Int64)
	}
	return nil
}

func validateImportedDatabaseSnapshotAPIKeyGroups(tx *gorm.DB) error {
	channelGroupIDs, errChannels := loadImportedDatabaseSnapshotUintIDs(tx, &ChannelGroupRecord{}, "channel_group")
	if errChannels != nil {
		return errChannels
	}
	modelGroupIDs, errModels := loadImportedDatabaseSnapshotUintIDs(tx, &ModelGroupRecord{}, "model_group")
	if errModels != nil {
		return errModels
	}
	rows, errRows := tx.Raw(`SELECT "id", "channels", "model_groups" FROM "api_key"`).Rows()
	if errRows != nil {
		return fmt.Errorf("query imported database snapshot api key groups: %w", errRows)
	}
	defer closeDatabaseSnapshotResource(rows, "api key group validation rows")
	for rows.Next() {
		var id uint
		var channels JSONB
		var modelGroups JSONB
		if errScan := rows.Scan(&id, &channels, &modelGroups); errScan != nil {
			return fmt.Errorf("scan imported database snapshot api key groups: %w", errScan)
		}
		channelIDs, errParseChannels := apiKeyChannelsFromJSON(channels)
		if errParseChannels != nil {
			return databaseSnapshotFieldError("api_key", fmt.Sprintf("[%d]", id), "channels", "is not a valid channel group id list")
		}
		for _, channelID := range channelIDs {
			if _, okChannel := channelGroupIDs[channelID]; !okChannel {
				return databaseSnapshotFieldError("api_key", fmt.Sprintf("[%d]", id), "channels", fmt.Sprintf("references missing channel group %d", channelID))
			}
		}
		modelGroupIDList, errParseModels := apiKeyModelGroupsFromJSON(modelGroups)
		if errParseModels != nil {
			return databaseSnapshotFieldError("api_key", fmt.Sprintf("[%d]", id), "model_groups", "is not a valid model group id list")
		}
		for _, modelGroupID := range modelGroupIDList {
			if _, okModel := modelGroupIDs[modelGroupID]; !okModel {
				return databaseSnapshotFieldError("api_key", fmt.Sprintf("[%d]", id), "model_groups", fmt.Sprintf("references missing model group %d", modelGroupID))
			}
		}
	}
	if errRows := rows.Err(); errRows != nil {
		return fmt.Errorf("iterate imported database snapshot api key groups: %w", errRows)
	}
	if errClose := rows.Close(); errClose != nil {
		return fmt.Errorf("close imported database snapshot api key groups: %w", errClose)
	}
	return nil
}

func loadImportedDatabaseSnapshotUintIDs(tx *gorm.DB, model any, table string) (map[uint]struct{}, error) {
	var values []uint
	if errLoad := tx.Unscoped().Model(model).Pluck("id", &values).Error; errLoad != nil {
		return nil, fmt.Errorf("load imported database snapshot %s ids: %w", table, errLoad)
	}
	ids := make(map[uint]struct{}, len(values))
	for _, value := range values {
		ids[value] = struct{}{}
	}
	return ids, nil
}

func resetDatabaseSnapshotSequences(tx *gorm.DB, backend DatabaseBackend, models []databaseModel) error {
	for _, model := range models {
		if !model.restore || !model.autoIncrement {
			continue
		}
		modelSchema, errSchema := schema.Parse(model.newRecord(), &sync.Map{}, schema.NamingStrategy{})
		if errSchema != nil {
			return fmt.Errorf("parse database snapshot sequence model %s: %w", model.name, errSchema)
		}
		var autoField *schema.Field
		for _, field := range modelSchema.Fields {
			if field.AutoIncrement {
				autoField = field
				break
			}
		}
		if autoField == nil {
			return fmt.Errorf("database snapshot table %s declares auto increment without an auto increment field", model.name)
		}
		switch backend {
		case DatabaseBackendPostgres:
			if errReset := resetPostgresDatabaseSnapshotSequence(tx, model.name, autoField.DBName); errReset != nil {
				return errReset
			}
		case DatabaseBackendSQLite:
			if errReset := resetSQLiteDatabaseSnapshotSequence(tx, model.name, autoField.DBName); errReset != nil {
				return errReset
			}
		}
	}
	return nil
}

func resetPostgresDatabaseSnapshotSequence(tx *gorm.DB, table string, column string) error {
	var sequence sql.NullString
	if errSequence := tx.Raw("SELECT pg_get_serial_sequence(?, ?)", table, column).Scan(&sequence).Error; errSequence != nil {
		return fmt.Errorf("find postgres sequence for database snapshot table %s: %w", table, errSequence)
	}
	if !sequence.Valid || strings.TrimSpace(sequence.String) == "" {
		return fmt.Errorf("postgres sequence for database snapshot table %s is missing", table)
	}
	var maximum sql.NullInt64
	query := fmt.Sprintf("SELECT MAX(%s) FROM %s", quoteDatabaseSnapshotIdentifier(column), quoteDatabaseSnapshotIdentifier(table))
	if errMaximum := tx.Raw(query).Scan(&maximum).Error; errMaximum != nil {
		return fmt.Errorf("find postgres maximum id for database snapshot table %s: %w", table, errMaximum)
	}
	value := int64(1)
	called := false
	if maximum.Valid {
		value = maximum.Int64
		called = true
	}
	if errSet := tx.Exec("SELECT setval(CAST(? AS regclass), ?, ?)", sequence.String, value, called).Error; errSet != nil {
		return fmt.Errorf("reset postgres sequence for database snapshot table %s: %w", table, errSet)
	}
	return nil
}

func resetSQLiteDatabaseSnapshotSequence(tx *gorm.DB, table string, column string) error {
	var maximum sql.NullInt64
	query := fmt.Sprintf("SELECT MAX(%s) FROM %s", quoteDatabaseSnapshotIdentifier(column), quoteDatabaseSnapshotIdentifier(table))
	if errMaximum := tx.Raw(query).Scan(&maximum).Error; errMaximum != nil {
		return fmt.Errorf("find sqlite maximum id for database snapshot table %s: %w", table, errMaximum)
	}
	if !maximum.Valid {
		if errDelete := tx.Exec("DELETE FROM sqlite_sequence WHERE name = ?", table).Error; errDelete != nil {
			return fmt.Errorf("clear sqlite sequence for database snapshot table %s: %w", table, errDelete)
		}
		return nil
	}
	update := tx.Exec("UPDATE sqlite_sequence SET seq = ? WHERE name = ?", maximum.Int64, table)
	if update.Error != nil {
		return fmt.Errorf("update sqlite sequence for database snapshot table %s: %w", table, update.Error)
	}
	if update.RowsAffected == 0 {
		if errInsert := tx.Exec("INSERT INTO sqlite_sequence(name, seq) VALUES(?, ?)", table, maximum.Int64).Error; errInsert != nil {
			return fmt.Errorf("insert sqlite sequence for database snapshot table %s: %w", table, errInsert)
		}
	}
	var sequence int64
	if errVerify := tx.Raw("SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&sequence).Error; errVerify != nil {
		return fmt.Errorf("verify sqlite sequence for database snapshot table %s: %w", table, errVerify)
	}
	if sequence != maximum.Int64 {
		return fmt.Errorf("verify sqlite sequence for database snapshot table %s: got %d, want %d", table, sequence, maximum.Int64)
	}
	return nil
}

func databaseBackendFromDB(db *gorm.DB) (DatabaseBackend, error) {
	if db == nil || db.Dialector == nil {
		return "", fmt.Errorf("database dialect is unavailable")
	}
	switch db.Dialector.Name() {
	case "sqlite":
		return DatabaseBackendSQLite, nil
	case "postgres":
		return DatabaseBackendPostgres, nil
	default:
		return "", fmt.Errorf("unsupported database snapshot backend %q", db.Dialector.Name())
	}
}

func databaseSnapshotTableEntryName(table string) string {
	return "tables/" + table + ".jsonl"
}

func quoteDatabaseSnapshotIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func closeDatabaseSnapshotResource(resource io.Closer, description string) {
	if resource == nil {
		return
	}
	if errClose := resource.Close(); errClose != nil && !errors.Is(errClose, os.ErrClosed) {
		log.WithError(errClose).WithField("resource", description).Warn("failed to close database snapshot resource")
	}
}
