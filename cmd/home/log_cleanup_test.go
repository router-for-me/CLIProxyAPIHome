package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

func TestLogCleanupMaintenanceStartsImmediatelyAndUsesCurrentConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	if errMkdir := os.Mkdir("logs", 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	writeLog := func(name string, modified time.Time) {
		t.Helper()
		path := filepath.Join("logs", name)
		if errWrite := os.WriteFile(path, make([]byte, 600*1024), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		if errTimes := os.Chtimes(path, modified, modified); errTimes != nil {
			t.Fatal(errTimes)
		}
	}
	base := time.Unix(100, 0)
	writeLog("old.log", base)
	writeLog("new.log", base.Add(time.Second))

	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{LogsMaxTotalSizeMB: 1}
		runtime, errRuntime := home.NewRuntime(cfg)
		if errRuntime != nil {
			t.Fatal(errRuntime)
		}
		defer runtime.Stop()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			runLogCleanupMaintenance(ctx, runtime)
			close(done)
		}()
		synctest.Wait()
		if _, errStat := os.Stat(filepath.Join("logs", "old.log")); !os.IsNotExist(errStat) {
			t.Fatalf("startup cleanup did not remove the oldest log: %v", errStat)
		}
		if _, errStat := os.Stat(filepath.Join("logs", "new.log")); errStat != nil {
			t.Fatalf("startup cleanup removed the newest log: %v", errStat)
		}

		disabled := *cfg
		disabled.LogsMaxTotalSizeMB = 0
		if errApply := runtime.ApplyConfigFromCluster(ctx, &disabled); errApply != nil {
			t.Fatal(errApply)
		}
		writeLog("latest.log", base.Add(2*time.Second))
		// Time advances virtually inside synctest; no wall-clock sleep is used.
		time.Sleep(time.Minute)
		synctest.Wait()
		if _, errStat := os.Stat(filepath.Join("logs", "new.log")); errStat != nil {
			t.Fatalf("cleanup ignored the disabled cluster config: %v", errStat)
		}

		if errApply := runtime.ApplyConfigFromCluster(ctx, cfg); errApply != nil {
			t.Fatal(errApply)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if _, errStat := os.Stat(filepath.Join("logs", "new.log")); !os.IsNotExist(errStat) {
			t.Fatalf("cleanup did not use the re-enabled cluster config: %v", errStat)
		}
		if _, errStat := os.Stat(filepath.Join("logs", "latest.log")); errStat != nil {
			t.Fatalf("periodic cleanup removed the newest log: %v", errStat)
		}
		cancel()
		<-done
	})
}
