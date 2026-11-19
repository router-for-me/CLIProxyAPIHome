//go:build cgo && (amd64 || arm64) && (windows || (linux && !android) || (darwin && !ios) || (freebsd && amd64))

package push

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestUsageIngestionSDKHost(t *testing.T) {
	mode := os.Getenv("CPA_HOME_TEST_PLUGIN_MODE")
	if mode == "" {
		root := t.TempDir()
		ext := ".so"
		switch runtime.GOOS {
		case "windows":
			ext = ".dll"
		case "darwin":
			ext = ".dylib"
		}
		platform := filepath.Join(root, "plugins", runtime.GOOS, runtime.GOARCH)
		if errMkdir := os.MkdirAll(platform, 0o755); errMkdir != nil {
			t.Fatal(errMkdir)
		}
		build := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-buildmode=c-shared", "-o", filepath.Join(platform, "probe-v1.0.0"+ext), "./testdata/usageplugin")
		if output, errBuild := build.CombinedOutput(); errBuild != nil {
			t.Fatalf("build SDK fixture: %v\n%s", errBuild, output)
		}
		// The SDK keeps native images mapped. Run each host in a child so the
		// parent can remove its shadow copies on Windows after both tests exit.
		for _, capability := range []string{"scheduler", "usage"} {
			t.Run(capability, func(t *testing.T) {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUsageIngestionSDKHost$")
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "CPA_HOME_TEST_PLUGIN_MODE="+capability)
				if output, errRun := cmd.CombinedOutput(); errRun != nil {
					t.Fatalf("run SDK host: %v\n%s", errRun, output)
				}
			})
		}
		return
	}

	db, rt := newUsageIngestionTestRuntime(t, true)
	enabled := true
	rt.PluginHost().ApplyConfig(t.Context(), pluginhost.RuntimeConfig{
		Enabled: true, Dir: "plugins", Configs: map[string]pluginhost.PluginInstanceConfig{"probe": {Enabled: &enabled}},
	})
	if plugins := rt.RegisteredPlugins(); len(plugins) != 1 || plugins[0].ID != "probe" {
		t.Fatalf("expected the native plugin to be registered, got %+v", plugins)
	}
	if rt.HasScheduler() != (mode == "scheduler") || rt.PluginHost().HasUsagePlugins() != (mode == "usage") {
		t.Fatalf("unexpected SDK capabilities for %s", mode)
	}
	args := []string{"LPUSH", "usage", `{"timestamp":"2026-11-19T13:10:33Z","provider":"codex","model":"gpt-5","auth_index":"usage-ingestion-auth","execution_id":"sdk-execution","selection_id":"sdk-selection"}`}
	for range 2 {
		if reply := handleUsage(t.Context(), dispatch.Env{Runtime: rt}, args); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
			t.Fatalf("LPUSH usage: %+v", reply)
		}
	}
	var rows []cluster.UsageRecord
	if errFind := db.Find(&rows).Error; errFind != nil || len(rows) != 1 || !rows[0].PluginUsageDelivered {
		t.Fatalf("usage not durably completed: rows=%+v error=%v", rows, errFind)
	}
	raw, errRead := os.ReadFile("observed.jsonl")
	if mode == "scheduler" {
		if !errors.Is(errRead, os.ErrNotExist) {
			t.Fatalf("scheduler-only host unexpectedly recorded usage: %v", errRead)
		}
		return
	}
	if errRead != nil {
		t.Fatal(errRead)
	}
	// A second callback would append a second JSON object, failing Unmarshal.
	var record pluginapi.UsageRecord
	if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
		t.Fatal(errDecode)
	}
	if record.RequestID != "sdk-execution" || record.SelectionID != "sdk-selection" {
		t.Fatalf("unexpected callback identity: execution=%q selection=%q", record.RequestID, record.SelectionID)
	}
}
