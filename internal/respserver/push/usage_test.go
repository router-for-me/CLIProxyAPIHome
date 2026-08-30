package push

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func TestUsagePayloadWithCPAIdentityFillsMissingRuntimeFields(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","request_id":"req-1"}`
	enriched, errEnrich := usagePayloadWithCPAIdentity(payload, dispatch.Env{NodeID: "node-1", ClientIP: "10.0.0.5"})
	if errEnrich != nil {
		t.Fatalf("usagePayloadWithCPAIdentity() error = %v", errEnrich)
	}
	if got := gjson.Get(enriched, "cpa_node_id").String(); got != "node-1" {
		t.Fatalf("cpa_node_id = %q, want node-1", got)
	}
	if got := gjson.Get(enriched, "cpa_ip").String(); got != "10.0.0.5" {
		t.Fatalf("cpa_ip = %q, want 10.0.0.5", got)
	}
	if got := gjson.Get(enriched, "cpa_label").String(); got != "node-1" {
		t.Fatalf("cpa_label = %q, want node-1", got)
	}
}

func TestSanitizeExistingUsageLogRemovesHistoricalProviderAPIKeySources(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "usage.log")
	content := strings.Join([]string{
		`{"provider":"xai","auth_type":"api_key","auth_index":"xai-auth","source":"historical-upstream-secret"}`,
		`{"provider":"xai","auth_type":"oauth","auth_index":"xai-oauth","source":"user@example.com"}`,
		`{"provider":"xai","auth_type":"api_key","source":"truncated-secret`,
	}, "\n") + "\n"
	if errWrite := os.WriteFile(filePath, []byte(content), 0o644); errWrite != nil {
		t.Fatalf("write historical usage log: %v", errWrite)
	}

	usageLogMu.Lock()
	delete(usageLogSanitizedStates, usageLogCleanPath(filePath))
	errSanitize := sanitizeUsageLogPathLocked(filePath)
	usageLogMu.Unlock()
	if errSanitize != nil {
		t.Fatalf("sanitizeUsageLogPathLocked() error = %v", errSanitize)
	}
	data, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("read sanitized usage log: %v", errRead)
	}
	result := string(data)
	if strings.Contains(result, "historical-upstream-secret") || !strings.Contains(result, `"source":"xai-auth"`) {
		t.Fatalf("provider API key source was not sanitized: %s", result)
	}
	if !strings.Contains(result, `"source":"user@example.com"`) {
		t.Fatalf("OAuth source was unexpectedly changed: %s", result)
	}
	if strings.Contains(result, "truncated-secret") || !strings.Contains(result, "discarded_invalid_historical_usage") {
		t.Fatalf("invalid historical usage was not safely discarded: %s", result)
	}

	oldWriter, errOpen := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		t.Fatalf("open usage log as old process: %v", errOpen)
	}
	if _, errWrite := oldWriter.WriteString(`{"provider":"xai","auth_type":"apikey","auth_index":"late-auth","source":"late-upstream-secret"}` + "\n"); errWrite != nil {
		t.Fatalf("append usage as old process: %v", errWrite)
	}
	if errClose := oldWriter.Close(); errClose != nil {
		t.Fatalf("close old usage writer: %v", errClose)
	}
	usageLogMu.Lock()
	errSanitize = sanitizeUsageLogPathLocked(filePath)
	usageLogMu.Unlock()
	if errSanitize != nil {
		t.Fatalf("repeat sanitizeUsageLogPathLocked() error = %v", errSanitize)
	}
	data, errRead = os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("read repeatedly sanitized usage log: %v", errRead)
	}
	if strings.Contains(string(data), "late-upstream-secret") || !strings.Contains(string(data), `"source":"late-auth"`) {
		t.Fatalf("late historical usage was not sanitized: %s", string(data))
	}

	if runtime.GOOS != "windows" {
		info, errStat := os.Stat(filePath)
		if errStat != nil {
			t.Fatalf("stat sanitized usage log: %v", errStat)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("sanitized usage log mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestUsagePayloadWithCPAIdentityPreservesReportedRuntimeFields(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","request_id":"req-1","cpa_node_id":"reported-node","cpa_ip":"192.0.2.10","cpa_label":"reported-label"}`
	enriched, errEnrich := usagePayloadWithCPAIdentity(payload, dispatch.Env{NodeID: "node-1", ClientIP: "10.0.0.5"})
	if errEnrich != nil {
		t.Fatalf("usagePayloadWithCPAIdentity() error = %v", errEnrich)
	}
	if got := gjson.Get(enriched, "cpa_node_id").String(); got != "reported-node" {
		t.Fatalf("cpa_node_id = %q, want reported-node", got)
	}
	if got := gjson.Get(enriched, "cpa_ip").String(); got != "192.0.2.10" {
		t.Fatalf("cpa_ip = %q, want 192.0.2.10", got)
	}
	if got := gjson.Get(enriched, "cpa_label").String(); got != "reported-label" {
		t.Fatalf("cpa_label = %q, want reported-label", got)
	}
}
