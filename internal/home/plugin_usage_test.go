package home

import (
	"testing"
	"time"
)

func TestDecodePluginUsageLegacyDefaults(t *testing.T) {
	record, errDecode := DecodePluginUsagePayload(`{"request_id":"inbound","auth_index":"auth"}`)
	if errDecode != nil || record.RequestID != "" || record.TraceID != "inbound" || record.AuthID != "auth" || !record.Generate || record.SelectionID != "" {
		t.Fatalf("record=%+v error=%v", record, errDecode)
	}
}

func TestDecodePluginUsageRetainedSelectionHasDistinctExecutions(t *testing.T) {
	first, errFirst := DecodePluginUsagePayload(`{"execution_id":"execution-1","selection_id":"retained-selection"}`)
	second, errSecond := DecodePluginUsagePayload(`{"execution_id":"execution-2","selection_id":"retained-selection"}`)
	if errFirst != nil || errSecond != nil || first.SelectionID != second.SelectionID || first.RequestID == second.RequestID {
		t.Fatalf("first=%+v second=%+v errors=%v/%v", first, second, errFirst, errSecond)
	}
}

func TestDecodePluginUsagePreservesExecutionAndSelection(t *testing.T) {
	record, errDecode := DecodePluginUsagePayload(
		`{"request_id":"inbound","execution_id":"execution","selection_id":"selection","trace_id":"trace","provider":"codex","executor_type":"codex","model":"upstream","alias":"public","auth_index":"auth","auth_id":"auth-id","base_url":"https://example.invalid","generate":false,"stream":true,"failed":true,"fail":{"status_code":429,"body":"quota"},"timestamp":"2026-01-02T03:04:05Z","latency_ms":123,"ttft_ms":12,"tokens":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"cache_read_tokens":3,"image_input_tokens":0},"response_headers":{"X-Test":["value"]}}`)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if record.RequestID != "execution" || record.TraceID != "trace" || record.SelectionID != "selection" || record.AuthID != "auth-id" || record.AuthIndex != "auth" {
		t.Fatalf("identity=%+v", record)
	}
	if record.Generate || !record.Stream || !record.Failed || record.Failure.StatusCode != 429 || record.Failure.Body != "quota" {
		t.Fatalf("result=%+v", record)
	}
	if record.Detail.InputTokens != 10 || record.Detail.TotalTokens != 14 || record.Detail.CacheReadTokens != 3 || record.Detail.ImageInputTokens == nil || *record.Detail.ImageInputTokens != 0 {
		t.Fatalf("tokens=%+v", record.Detail)
	}
	if record.Latency != 123*time.Millisecond || record.TTFT != 12*time.Millisecond || record.ResponseHeaders.Get("X-Test") != "value" {
		t.Fatalf("metadata=%+v", record)
	}
}

func TestDecodePluginUsageRejectsMalformedValues(t *testing.T) {
	for _, payload := range []string{
		`null`,
		`[]`,
		`{"stream":"yes"}`,
		`{`,
		`{"selection_id":123}`,
		`{"timestamp":"bad"}`,
		`{"tokens":{"input_tokens":"bad"}}`,
		`{"latency_ms":-1}`,
		`{"ttft_ms":9223372036854775807}`,
	} {
		if _, errDecode := DecodePluginUsagePayload(payload); errDecode == nil {
			t.Fatalf("accepted invalid usage: %s", payload)
		}
	}
}
