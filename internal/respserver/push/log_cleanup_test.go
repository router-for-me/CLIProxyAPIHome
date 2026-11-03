package push

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeCleanupTestLog(t *testing.T, dir, name string, size int, modified time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, make([]byte, size), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errTimes := os.Chtimes(path, modified, modified); errTimes != nil {
		t.Fatal(errTimes)
	}
	return path
}

func TestCleanupLogDirectoryRemovesOldestLogsOnly(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(100, 0)
	oldest := writeCleanupTestLog(t, dir, "z-old.LOG", 400*1024, base)
	compressed := writeCleanupTestLog(t, dir, "middle.log.gz", 400*1024, base.Add(time.Second))
	newest := writeCleanupTestLog(t, dir, "a-new.log", 400*1024, base.Add(2*time.Second))
	unrelated := writeCleanupTestLog(t, dir, "notes.txt", 2*1024*1024, base)
	nestedDir := filepath.Join(dir, "nested.log")
	if errMkdir := os.Mkdir(nestedDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	nested := writeCleanupTestLog(t, nestedDir, "nested.log", 2*1024*1024, base)

	deleted, errCleanup := CleanupLogDirectory(t.Context(), dir, 1)
	if errCleanup != nil || deleted != 1 {
		t.Fatalf("cleanup = (%d, %v), want (1, nil)", deleted, errCleanup)
	}
	if _, errStat := os.Stat(oldest); !os.IsNotExist(errStat) {
		t.Fatalf("oldest log still exists: %v", errStat)
	}
	for _, path := range []string{compressed, newest, unrelated, nested} {
		if _, errStat := os.Stat(path); errStat != nil {
			t.Errorf("expected %s to remain: %v", path, errStat)
		}
	}
	deleted, errCleanup = CleanupLogDirectory(t.Context(), dir, 1)
	if errCleanup != nil || deleted != 0 {
		t.Fatalf("repeated cleanup = (%d, %v), want (0, nil)", deleted, errCleanup)
	}
}

func TestCleanupLogDirectoryLimitsAndErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		limitMB int
		size    int
		want    int
	}{
		{name: "disabled", limitMB: 0, size: 2 * 1024 * 1024},
		{name: "negative disabled", limitMB: -1, size: 2 * 1024 * 1024},
		{name: "exact limit", limitMB: 1, size: 1024 * 1024},
		{name: "one byte over", limitMB: 1, size: 1024*1024 + 1, want: 1},
		{name: "oversized single log", limitMB: 1, size: 2 * 1024 * 1024, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeCleanupTestLog(t, dir, "request.log", test.size, time.Unix(100, 0))
			deleted, errCleanup := CleanupLogDirectory(t.Context(), dir, test.limitMB)
			if errCleanup != nil || deleted != test.want {
				t.Fatalf("cleanup = (%d, %v), want (%d, nil)", deleted, errCleanup, test.want)
			}
			_, errStat := os.Stat(path)
			if test.want == 0 && errStat != nil || test.want == 1 && !os.IsNotExist(errStat) {
				t.Fatalf("unexpected remaining file state: %v", errStat)
			}
		})
	}

	dir := t.TempDir()
	if deleted, errCleanup := CleanupLogDirectory(t.Context(), filepath.Join(dir, "missing"), 1); errCleanup != nil || deleted != 0 {
		t.Fatalf("missing directory cleanup = (%d, %v), want (0, nil)", deleted, errCleanup)
	}
	path := writeCleanupTestLog(t, dir, "request.log", 2*1024*1024, time.Unix(100, 0))
	if _, errCleanup := CleanupLogDirectory(t.Context(), path, 1); errCleanup == nil {
		t.Fatal("cleanup of a non-directory must return an error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if deleted, errCleanup := CleanupLogDirectory(ctx, dir, 1); !errors.Is(errCleanup, context.Canceled) || deleted != 0 {
		t.Fatalf("canceled cleanup = (%d, %v), want (0, context.Canceled)", deleted, errCleanup)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("canceled cleanup removed a log: %v", errStat)
	}
	maxInt := int(^uint(0) >> 1)
	if int64(maxInt) > (1<<63-1)/(1024*1024) {
		if _, errCleanup := CleanupLogDirectory(t.Context(), dir, maxInt); errCleanup == nil {
			t.Fatal("overflowing size limit must return an error")
		}
	}
}

func TestCleanupLogDirectoryLimitsHomesIndependently(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	for _, dir := range dirs {
		writeCleanupTestLog(t, dir, "old.log.gz", 600*1024, time.Unix(100, 0))
		writeCleanupTestLog(t, dir, "new.log", 600*1024, time.Unix(200, 0))
	}
	for i, dir := range dirs {
		deleted, errCleanup := CleanupLogDirectory(t.Context(), dir, 1)
		if errCleanup != nil || deleted != 1 {
			t.Fatalf("Home %d cleanup = (%d, %v), want (1, nil)", i, deleted, errCleanup)
		}
		if _, errStat := os.Stat(filepath.Join(dir, "new.log")); errStat != nil {
			t.Fatalf("Home %d newest log missing: %v", i, errStat)
		}
		if i == 0 {
			if _, errStat := os.Stat(filepath.Join(dirs[1], "old.log.gz")); errStat != nil {
				t.Fatalf("Home 0 cleanup affected Home 1: %v", errStat)
			}
		}
	}
}

// cleanupCheckpointContext coordinates work at cancellation checks without
// filesystem timing, sleeps, or test hooks in the cleanup implementation.
type cleanupCheckpointContext struct {
	context.Context
	checks  atomic.Int32
	onCheck func(int32)
}

func (c *cleanupCheckpointContext) Err() error {
	c.onCheck(c.checks.Add(1))
	return c.Context.Err()
}

func TestCleanupLogDirectoryScanAndSortAllowLogWrites(t *testing.T) {
	t.Chdir(t.TempDir())
	if errMkdir := os.Mkdir("logs", 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	writeCleanupTestLog(t, "logs", "old.log", 600*1024, time.Unix(100, 0))
	writeCleanupTestLog(t, "logs", "new.log", 600*1024, time.Unix(200, 0))
	var phases []string
	var deletionLocked bool
	ctx := &cleanupCheckpointContext{Context: t.Context(), onCheck: func(check int32) {
		var phase string
		switch check {
		case 2:
			phase = "scan"
		case 4:
			// Two entries have been scanned and sorted, before any deletion.
			phase = "sorted"
		case 5:
			if logFilesMu.TryRLock() {
				logFilesMu.RUnlock()
				t.Fatal("file revalidation and deletion must exclude writers")
			}
			deletionLocked = true
			return
		default:
			return
		}
		if !logFilesMu.TryRLock() {
			t.Fatalf("%s holds the exclusive log writer lock", phase)
		}
		logFilesMu.RUnlock()
		if errWrite := writeRequestLogFile("request-"+phase+".log", phase); errWrite != nil {
			t.Fatal(errWrite)
		}
		if errAppend := appendUsageLog(`{"event_type":"cleanup-` + phase + `"}`); errAppend != nil {
			t.Fatal(errAppend)
		}
		phases = append(phases, phase)
	}}
	deleted, errCleanup := CleanupLogDirectory(ctx, "logs", 1)
	if errCleanup != nil || deleted != 1 {
		t.Fatalf("cleanup = (%d, %v), want (1, nil)", deleted, errCleanup)
	}
	if strings.Join(phases, ",") != "scan,sorted" || !deletionLocked {
		t.Fatalf("cleanup checkpoints: phases=%v deletionLocked=%v", phases, deletionLocked)
	}
	for _, phase := range phases {
		data, errRead := os.ReadFile(filepath.Join("logs", "request-"+phase+".log"))
		if errRead != nil || string(data) != phase+"\n" {
			t.Fatalf("%s request log = %q, error=%v", phase, data, errRead)
		}
	}
	usage, errRead := os.ReadFile(filepath.Join("logs", "usage.log"))
	wantUsage := "{\"event_type\":\"cleanup-scan\"}\n{\"event_type\":\"cleanup-sorted\"}\n"
	if errRead != nil || string(usage) != wantUsage {
		t.Fatalf("usage log = %q, error=%v", usage, errRead)
	}
}

func TestCleanupLogDirectoryRevalidatesModifiedLogs(t *testing.T) {
	for _, test := range []struct {
		name        string
		updatedSize int
		wantDeleted int
	}{
		{name: "shrunk", updatedSize: 100 * 1024},
		{name: "grown", updatedSize: 900 * 1024, wantDeleted: 1},
		{name: "same size rewritten", updatedSize: 600 * 1024, wantDeleted: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if errMkdir := os.Mkdir("logs", 0o700); errMkdir != nil {
				t.Fatal(errMkdir)
			}
			oldest := writeCleanupTestLog(t, "logs", "old.log", 600*1024, time.Unix(100, 0))
			newest := writeCleanupTestLog(t, "logs", "new.log", 600*1024, time.Unix(200, 0))
			updated := false
			ctx := &cleanupCheckpointContext{Context: t.Context(), onCheck: func(check int32) {
				if check != 4 {
					return
				}
				if !logFilesMu.TryRLock() {
					t.Fatal("writers must be able to update files after the scan")
				}
				logFilesMu.RUnlock()
				if errWrite := writeRequestLogFile("old.log", strings.Repeat("x", test.updatedSize-1)); errWrite != nil {
					t.Fatal(errWrite)
				}
				updated = true
			}}
			deleted, errCleanup := CleanupLogDirectory(ctx, "logs", 1)
			if errCleanup != nil || deleted != test.wantDeleted || !updated {
				t.Fatalf("cleanup = (%d, %v), updated=%v; want (%d, nil), updated=true", deleted, errCleanup, updated, test.wantDeleted)
			}
			if info, errStat := os.Stat(oldest); errStat != nil || info.Size() != int64(test.updatedSize) {
				t.Fatalf("updated log was removed or changed: %v", errStat)
			}
			_, errStat := os.Stat(newest)
			if test.wantDeleted == 0 && errStat != nil || test.wantDeleted == 1 && !os.IsNotExist(errStat) {
				t.Fatalf("cleanup did not account for the updated file size: %v", errStat)
			}
		})
	}
}

func TestCleanupLogDirectoryRevalidatesAppendedUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	if errAppend := appendUsageLog(`{"event_type":"before-scan"}`); errAppend != nil {
		t.Fatal(errAppend)
	}
	path := filepath.Join("logs", "usage.log")
	snapshot, errStat := os.Lstat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if errAppend := appendUsageLog(`{"event_type":"after-scan"}`); errAppend != nil {
		t.Fatal(errAppend)
	}
	remainingSize, removed, errRemove := removeUnchangedLogFile(t.Context(), path, snapshot)
	if errRemove != nil || removed || remainingSize <= snapshot.Size() {
		t.Fatalf("appended usage cleanup = (%d, %v, %v), snapshot size=%d", remainingSize, removed, errRemove, snapshot.Size())
	}
	data, errRead := os.ReadFile(path)
	want := "{\"event_type\":\"before-scan\"}\n{\"event_type\":\"after-scan\"}\n"
	if errRead != nil || string(data) != want {
		t.Fatalf("usage log = %q, error=%v", data, errRead)
	}
}

func TestCleanupLogDirectorySkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := writeCleanupTestLog(t, t.TempDir(), "target.log", 2*1024*1024, time.Unix(100, 0))
	link := filepath.Join(dir, "link.log")
	if errSymlink := os.Symlink(target, link); errSymlink != nil {
		t.Skipf("symlinks are unavailable: %v", errSymlink)
	}
	deleted, errCleanup := CleanupLogDirectory(t.Context(), dir, 1)
	if errCleanup != nil || deleted != 0 {
		t.Fatalf("symlink cleanup = (%d, %v), want (0, nil)", deleted, errCleanup)
	}
	if _, errStat := os.Lstat(link); errStat != nil {
		t.Fatalf("symlink was removed: %v", errStat)
	}
}
