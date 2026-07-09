package push

import (
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
