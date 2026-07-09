package cluster

import "testing"

func TestUsageRecordFromPayloadStoresRequestIDAndHomeIP(t *testing.T) {
	payload := `{"timestamp":"2026-05-29T01:02:03Z","request_id":"req-usage-1","executor_type":"CodexWebsocketsExecutor","tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`

	record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload: %v", errRecord)
	}

	if record.RequestID != "req-usage-1" {
		t.Fatalf("request id = %q, want req-usage-1", record.RequestID)
	}
	if record.HomeIP != "192.0.2.10" {
		t.Fatalf("home ip = %q, want 192.0.2.10", record.HomeIP)
	}
	if record.ExecutorType != "CodexWebsocketsExecutor" {
		t.Fatalf("executor type = %q, want CodexWebsocketsExecutor", record.ExecutorType)
	}
	if record.TotalTokens != 15 {
		t.Fatalf("total tokens = %d, want 15", record.TotalTokens)
	}
}

func TestUsageRecordFromPayloadWithRuntimeStoresOwnershipColumns(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","request_id":"req-runtime-1","endpoint":"/v1/responses","upstream_status_code":"202","tokens":{"total_tokens":3}}`

	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{
		HomeIP:    "192.0.2.10",
		HomePort:  8327,
		CPANodeID: "node-1",
		CPAIP:     "10.0.0.5",
		CPAPort:   8317,
	})
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayloadWithRuntime: %v", errRecord)
	}

	if record.HomeIP != "192.0.2.10" || record.HomePort != 8327 {
		t.Fatalf("home ownership = %s:%d, want 192.0.2.10:8327", record.HomeIP, record.HomePort)
	}
	if record.CPANodeID != "node-1" || record.CPAIP != "10.0.0.5" || record.CPAPort != 8317 || record.CPALabel != "node-1" {
		t.Fatalf("CPA ownership = node=%q ip=%q port=%d label=%q, want node-1 10.0.0.5 8317 node-1", record.CPANodeID, record.CPAIP, record.CPAPort, record.CPALabel)
	}
	if record.EventType != "response" {
		t.Fatalf("event type = %q, want response", record.EventType)
	}
	if record.UpstreamStatusCode != 202 {
		t.Fatalf("upstream status code = %d, want 202", record.UpstreamStatusCode)
	}
}

func TestUsageRecordFromPayloadDoesNotTreatClientIPAsCPAIP(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","request_id":"req-client-ip","client_ip":"203.0.113.8","endpoint":"/v1/chat/completions"}`

	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayloadWithRuntime: %v", errRecord)
	}

	if record.CPAIP != "" {
		t.Fatalf("CPA IP = %q, want empty when only client_ip exists", record.CPAIP)
	}
}

func TestUsageRecordFromPayloadDerivesCPALabelFromPayloadOwnership(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","request_id":"req-cpa-label","cpa_node_id":"node-from-payload","cpa_ip":"10.0.0.5","cpa_port":8317}`

	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayloadWithRuntime: %v", errRecord)
	}

	if record.CPALabel != "node-from-payload" {
		t.Fatalf("CPA label = %q, want node-from-payload", record.CPALabel)
	}
}
