package push

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// logFilesMu keeps cleanup from removing files while local log writers use them.
// Writers hold a read lock so independent request logs can still be written concurrently.
var logFilesMu sync.RWMutex

// CleanupLogDirectory removes the oldest local .log and .log.gz files until their
// combined size is within maxTotalSizeMB. A non-positive limit disables cleanup.
// Only regular files directly inside logDir are considered.
func CleanupLogDirectory(ctx context.Context, logDir string, maxTotalSizeMB int) (int, error) {
	if maxTotalSizeMB <= 0 {
		return 0, nil
	}
	const bytesPerMB = 1024 * 1024
	if int64(maxTotalSizeMB) > math.MaxInt64/bytesPerMB {
		return 0, fmt.Errorf("log directory size limit is too large")
	}
	maxBytes := int64(maxTotalSizeMB) * bytesPerMB

	if errContext := ctx.Err(); errContext != nil {
		return 0, errContext
	}

	entries, errRead := os.ReadDir(logDir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			// Windows may also report a missing path when logDir is a regular file.
			if _, errStat := os.Stat(logDir); os.IsNotExist(errStat) {
				return 0, nil
			}
		}
		return 0, fmt.Errorf("read log directory: %w", errRead)
	}

	type logFile struct {
		name string
		info os.FileInfo
	}
	files := make([]logFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if errContext := ctx.Err(); errContext != nil {
			return 0, errContext
		}
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if !strings.HasSuffix(lowerName, ".log") && !strings.HasSuffix(lowerName, ".log.gz") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			if os.IsNotExist(errInfo) {
				continue
			}
			return 0, fmt.Errorf("stat log file: %w", errInfo)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, logFile{name: name, info: info})
		total += info.Size()
	}
	if total <= maxBytes {
		return 0, nil
	}

	// Scanning and sorting must not block request or usage log writers.
	sort.Slice(files, func(i, j int) bool {
		if files[i].info.ModTime().Equal(files[j].info.ModTime()) {
			return files[i].name < files[j].name
		}
		return files[i].info.ModTime().Before(files[j].info.ModTime())
	})

	deleted := 0
	for _, file := range files {
		if total <= maxBytes {
			break
		}
		if errContext := ctx.Err(); errContext != nil {
			return deleted, errContext
		}
		remainingSize, removed, errRemove := removeUnchangedLogFile(ctx, filepath.Join(logDir, file.name), file.info)
		if errRemove != nil {
			return deleted, errRemove
		}
		total += remainingSize - file.info.Size()
		if removed {
			deleted++
		}
	}
	return deleted, nil
}

// removeUnchangedLogFile revalidates snapshot size and modification time while
// excluding writers. It returns the current regular-file size (zero if absent or removed).
func removeUnchangedLogFile(ctx context.Context, path string, snapshot os.FileInfo) (int64, bool, error) {
	logFilesMu.Lock()
	defer logFilesMu.Unlock()
	if errContext := ctx.Err(); errContext != nil {
		return 0, false, errContext
	}

	current, errStat := os.Lstat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("stat log file before cleanup: %w", errStat)
	}
	if !current.Mode().IsRegular() {
		return 0, false, nil
	}
	// A writer may have updated or replaced this file after the unlocked scan.
	// Leave it for a later pass rather than deleting it using stale ordering.
	if snapshot.Size() != current.Size() || !snapshot.ModTime().Equal(current.ModTime()) {
		return current.Size(), false, nil
	}
	if errRemove := os.Remove(path); errRemove != nil {
		if os.IsNotExist(errRemove) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("remove log file: %w", errRemove)
	}
	return 0, true, nil
}
