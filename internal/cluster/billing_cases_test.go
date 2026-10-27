package cluster

import (
	"context"
	"encoding/json"
	"testing"
)

func newBillingCaseUser(t *testing.T, repo *Repository, key string) *UserRecord {
	t.Helper()
	ctx := context.Background()
	username := key + "@example.com"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	return user
}

func billingCaseCharges(t *testing.T, repo *Repository, userID uint) map[string]BillingChargeRecord {
	t.Helper()
	result, errList := repo.ListBillingCharges(context.Background(), BillingChargeQuery{UserID: &userID, Limit: 100})
	if errList != nil {
		t.Fatalf("ListBillingCharges() error = %v", errList)
	}
	out := make(map[string]BillingChargeRecord, len(result.Records))
	for _, record := range result.Records {
		out[record.RequestID] = record
	}
	return out
}

func TestBillingFailedRequestsBillConsumedTokensOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "failed-key")
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "antigravity", Model: "gemini-3.8-flash-high", InputPricePerMillion: 1, OutputPricePerMillion: 4, CacheReadPricePerMillion: 0.1, RequestPrice: 0.5, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	// Shapes observed in production: a client-cancelled stream that consumed a
	// prompt, and an upstream 400 that consumed nothing.
	payloads := []string{
		`{"timestamp":"2026-10-27T01:00:00Z","provider":"antigravity","executor_type":"AntigravityExecutor","model":"gemini-3.8-flash-high","api_key":"failed-key","request_id":"req-499","failed":true,"stream":true,"fail":{"status_code":499,"body":"context canceled"},"accounting_version":2,"tokens":{"input_tokens":128594,"output_tokens":0,"total_tokens":128594},"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":128594,"input":{"total_tokens":128594,"uncached_tokens":28594,"cache_read_tokens":100000,"cache_write_tokens":0},"output":{"total_tokens":0,"non_reasoning_tokens":0,"reasoning_tokens":0},"unclassified_tokens":0}}`,
		`{"timestamp":"2026-10-27T01:00:01Z","provider":"antigravity","executor_type":"AntigravityExecutor","model":"gemini-3.8-flash-high","api_key":"failed-key","request_id":"req-400","failed":true,"fail":{"status_code":400,"body":"bad request"},"accounting_version":2,"tokens":{"total_tokens":0},"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":0,"input":{"total_tokens":0,"uncached_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0},"output":{"total_tokens":0,"non_reasoning_tokens":0,"reasoning_tokens":0},"unclassified_tokens":0}}`,
	}
	for _, payload := range payloads {
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage() error = %v", errUsage)
		}
	}
	charges := billingCaseCharges(t, repo, user.ID)
	if _, ok := charges["req-400"]; ok || len(charges) != 1 {
		t.Fatalf("charges = %v, want only the 499 request billed", charges)
	}
	charge := charges["req-499"]
	// The request price is not billed for a failed request.
	if want := billingRoundAmount((28594*1 + 100000*0.1) / 1e6); charge.Amount != want {
		t.Fatalf("499 amount = %.10f, want %.10f", charge.Amount, want)
	}
	var snapshot BillingPriceSnapshot
	if errUnmarshal := json.Unmarshal(charge.PriceSnapshot, &snapshot); errUnmarshal != nil || !snapshot.FailedRequest || snapshot.FailStatusCode != 499 || snapshot.RequestPrice != 0 {
		t.Fatalf("snapshot = %s err = %v", charge.PriceSnapshot, errUnmarshal)
	}
}

func TestBillingUsesOutboundServiceTier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "tier-key")
	for tier, price := range map[string]float64{"standard": 1, "priority": 2} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "gpt-5.5", ServiceTier: tier, InputPricePerMillion: price, Enabled: true}); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%s) error = %v", tier, errCreate)
		}
	}
	for request, tiers := range map[string][2]string{
		"req-forced":     {"auto", "priority"}, // CPA config forced priority on an auto request
		"req-downgraded": {"priority", "default"},
		"req-client":     {"priority", ""}, // no outbound tier reported: client tier applies
	} {
		payload := `{"timestamp":"2026-10-27T01:00:00Z","provider":"codex","executor_type":"CodexExecutor","model":"gpt-5.5","api_key":"tier-key","request_id":"` + request + `","service_tier":"` + tiers[0] + `","effective_service_tier":"` + tiers[1] + `","response_service_tier":"default","tokens":{"input_tokens":1000000,"total_tokens":1000000}}`
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(%s) error = %v", request, errUsage)
		}
	}
	want := map[string]float64{"req-forced": 2, "req-downgraded": 1, "req-client": 2}
	for request, charge := range billingCaseCharges(t, repo, user.ID) {
		if charge.Amount != want[request] {
			t.Fatalf("%s amount = %v, want %v (snapshot %s)", request, charge.Amount, want[request], charge.PriceSnapshot)
		}
	}
}

func TestBillingImagesEndpointBillsAtLeastOneImage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "image-key")
	perImage := 0.04
	update := BillingModelPriceUpdate{Provider: "codex", Model: "gpt-image-2", InputPricePerMillion: 5, OutputPricePerMillion: 40, Enabled: true}
	if errSet := update.Extra.Set(BillingPriceImage, &perImage); errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, update); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	// Real gpt-image-2 edit usage: tokens are reported, the image count is not.
	payload := `{"timestamp":"2026-10-27T01:00:00Z","provider":"codex","executor_type":"CodexExecutor","model":"gpt-image-2","endpoint":"POST /v1/images/edits","api_key":"image-key","request_id":"req-image","accounting_version":2,"tokens":{"input_tokens":1014,"output_tokens":1544,"reasoning_tokens":0,"cached_tokens":0,"cache_read_tokens":0,"cache_read_tokens_present":true,"cache_creation_tokens":0,"total_tokens":2558},"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":2558,"input":{"total_tokens":1014,"uncached_tokens":1014,"cache_read_tokens":0,"cache_write_tokens":0},"output":{"total_tokens":1544,"non_reasoning_tokens":1544,"reasoning_tokens":0},"unclassified_tokens":0}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	charge := billingCaseCharges(t, repo, user.ID)["req-image"]
	if want := billingRoundAmount((1014*5+1544*40)/1e6 + 0.04); charge.Amount != want {
		t.Fatalf("image amount = %.10f, want %.10f", charge.Amount, want)
	}
}
