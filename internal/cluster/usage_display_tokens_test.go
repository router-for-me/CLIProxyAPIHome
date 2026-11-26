package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestUsageObservabilityDisplayTokensAcrossManagementReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}

	cases := []struct {
		name, provider, tokens, state string
		input, output                 int64
	}{
		{"claude", "claude", `"input_tokens":2,"output_tokens":2070,"cache_read_tokens":10000,"cache_creation_tokens":926,"total_tokens":12998`, "", 10928, 2070},
		{"gemini", "gemini", `"input_tokens":61070,"output_tokens":50,"reasoning_tokens":139,"total_tokens":61259`, "", 61070, 189},
		{"openai", "openai", `"input_tokens":87384,"output_tokens":204,"reasoning_tokens":100,"cache_read_tokens":84992,"total_tokens":87588`, "", 87384, 204},
		{"inconsistent", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":999`, "", 100, 20},
		{"unclassified", "unknown", `"input_tokens":100,"output_tokens":20,"total_tokens":120`, "", 100, 20},
		{"pending", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":130`, "pending", 100, 20},
		{"canonical-zero", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":130`, "zero", 0, 0},
	}
	type tokenPair struct{ input, output int64 }
	want := make(map[string]tokenPair, len(cases))
	wantByProvider := map[string]tokenPair{}
	var wantTotal tokenPair
	usageIDs := make(map[string]uint, len(cases))
	for index, tc := range cases {
		payload := fmt.Sprintf(`{"timestamp":"2026-06-10T12:00:%02dZ","provider":%q,"model":"display-model","request_id":%q,"session_id":"display-session","auth_index":%q,"endpoint":"/v1/responses","latency_ms":1000,"ttft_ms":300,"tokens":{%s}}`,
			index, tc.provider, tc.name, "display-"+tc.provider, tc.tokens)
		usage, errAppend := repo.AppendUsage(ctx, payload, "192.0.2.10")
		if errAppend != nil {
			t.Fatalf("%s: %v", tc.name, errAppend)
		}
		switch tc.state {
		case "pending":
			if errUpdate := db.Model(&UsageRecord{}).Where("id = ?", usage.ID).Update("token_accounting_version", 0).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
		case "zero":
			if errUpdate := db.Model(&UsageRecord{}).Where("id = ?", usage.ID).Updates(usageTokenBreakdownUpdates(newUnclassifiedUsageTokenBreakdown(0))).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
		}
		usageIDs[tc.name] = usage.ID
		want[tc.name] = tokenPair{tc.input, tc.output}
		providerWant := wantByProvider[tc.provider]
		wantByProvider[tc.provider] = tokenPair{providerWant.input + tc.input, providerWant.output + tc.output}
		wantTotal = tokenPair{wantTotal.input + tc.input, wantTotal.output + tc.output}
	}

	records, errRecords := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{Limit: 100})
	if errRecords != nil {
		t.Fatal(errRecords)
	}
	if len(records.Records) != len(cases) {
		t.Fatalf("records = %d, want %d", len(records.Records), len(cases))
	}
	for _, record := range records.Records {
		if got := (tokenPair{record.Tokens.InputTokens, record.Tokens.OutputTokens}); got != want[record.RequestID] {
			t.Fatalf("record %s tokens = %+v, want %+v", record.RequestID, got, want[record.RequestID])
		}
		if wantTPS := float64(want[record.RequestID].output); wantTPS == 0 {
			if record.Performance.TPS != nil {
				t.Errorf("record %s TPS = %v, want nil", record.RequestID, *record.Performance.TPS)
			}
		} else if record.Performance.TPS == nil || *record.Performance.TPS != wantTPS {
			t.Errorf("record %s performance = %+v, want TPS %v", record.RequestID, record.Performance, wantTPS)
		}
	}
	detail, errDetail := repo.GetUsageObservabilityRecord(ctx, fmt.Sprint(usageIDs["claude"]))
	if errDetail != nil {
		t.Fatal(errDetail)
	}
	if got := (tokenPair{detail.Tokens.InputTokens, detail.Tokens.OutputTokens}); got != want["claude"] || detail.Tokens.CacheReadTokens != 10000 || detail.Tokens.TotalTokens != 12998 {
		t.Fatalf("claude detail tokens = %+v", detail.Tokens)
	}

	from := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{From: &from, To: &to, Interval: "hour"})
	if errOverview != nil {
		t.Fatal(errOverview)
	}
	if got := (tokenPair{overview.Totals.InputTokens, overview.Totals.OutputTokens}); got != wantTotal {
		t.Fatalf("overview totals = %+v, want %+v", got, wantTotal)
	}
	var trendTotal tokenPair
	for _, point := range overview.Trend {
		trendTotal = tokenPair{trendTotal.input + point.InputTokens, trendTotal.output + point.OutputTokens}
	}
	if trendTotal != wantTotal {
		t.Fatalf("overview trend = %+v, want %+v", trendTotal, wantTotal)
	}
	if len(overview.ModelEfficiency) != 1 || (tokenPair{overview.ModelEfficiency[0].InputTokens, overview.ModelEfficiency[0].OutputTokens}) != wantTotal {
		t.Fatalf("model efficiency = %+v, want %+v", overview.ModelEfficiency, wantTotal)
	}
	assertProviderItems := func(source string, items []UsageObservabilityAggregateItem, idPrefix string) {
		t.Helper()
		if len(items) != len(wantByProvider) {
			t.Fatalf("%s items = %+v", source, items)
		}
		for _, item := range items {
			provider := item.ID[len(idPrefix):]
			if got := (tokenPair{item.InputTokens, item.OutputTokens}); got != wantByProvider[provider] {
				t.Fatalf("%s %s tokens = %+v, want %+v", source, item.ID, got, wantByProvider[provider])
			}
		}
	}
	assertProviderItems("overview top providers", overview.Top.Providers, "")

	for _, groupBy := range []string{"provider", "credential"} {
		aggregates, errAggregates := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{From: &from, To: &to, GroupBy: groupBy, Limit: 100})
		if errAggregates != nil {
			t.Fatalf("%s aggregates: %v", groupBy, errAggregates)
		}
		prefix := ""
		if groupBy == "credential" {
			prefix = "display-"
		}
		assertProviderItems(groupBy+" aggregates", aggregates.Items, prefix)
	}

	realtime, errRealtime := repo.UsageObservabilityRealtime(ctx, UsageObservabilityRealtimeQuery{From: &from, To: &to, GroupBy: "provider", BucketSeconds: 3600})
	if errRealtime != nil {
		t.Fatal(errRealtime)
	}
	assertProviderItems("realtime current usage", realtime.CurrentUsage, "")

	tree, errTree := repo.GetSessionTree(ctx, "display-session")
	if errTree != nil {
		t.Fatal(errTree)
	}
	if len(tree.Tree) != 1 || len(tree.Tree[0].Timeline) != len(cases) {
		t.Fatalf("session tree = %+v", tree)
	}
	if got := (tokenPair{tree.Tree[0].InputTokens, tree.Tree[0].OutputTokens}); got != wantTotal {
		t.Fatalf("session node tokens = %+v, want %+v", got, wantTotal)
	}
	for _, turn := range tree.Tree[0].Timeline {
		if got := (tokenPair{turn.InputTokens, turn.OutputTokens}); got != want[turn.RequestID] {
			t.Fatalf("session turn %s tokens = %+v, want %+v", turn.RequestID, got, want[turn.RequestID])
		}
	}
}
