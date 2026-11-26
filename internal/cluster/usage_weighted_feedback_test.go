package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gorm.io/gorm"
)

type usagePermissionFailureAdapter struct {
	*RuntimeAdapter
	permissionErr error
}

func (a *usagePermissionFailureAdapter) AllowedUsageIDsForAPIKeyModel(ctx context.Context, apiKey, modelID string) ([]string, []string, error) {
	if a.permissionErr != nil {
		return nil, nil, a.permissionErr
	}
	return a.RuntimeAdapter.AllowedUsageIDsForAPIKeyModel(ctx, apiKey, modelID)
}

func TestReusedUsageFeedbackAfterBillingAdmissionExhausted(t *testing.T) {
	for _, limit := range []string{"credits", "period"} {
		for _, feedback := range []string{"record", "replay-first", "permission-retry"} {
			t.Run(limit+"/"+feedback, func(t *testing.T) {
				ctx := t.Context()
				repo := newCredentialFoundationTestRepository(t)
				credits := 1.0
				if limit == "period" {
					credits = 10
				}
				user := createPeriodLimitUser(t, repo, ctx, "usage-user", credits)
				if limit == "period" {
					if _, errUpdate := repo.UpdateUser(ctx, user.ID, UserUpdate{Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 1}}); errUpdate != nil {
						t.Fatal(errUpdate)
					}
				}
				key := "usage-client-key"
				if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
					t.Fatal(errKey)
				}
				const model = "weighted-billed-model"
				if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: model, RequestPrice: 1, Enabled: true}); errPrice != nil {
					t.Fatal(errPrice)
				}
				runtime, errRuntime := home.NewRuntime(&config.Config{AuthDir: t.TempDir(), Routing: config.RoutingConfig{Strategy: "weighted-round-robin"}})
				if errRuntime != nil {
					t.Fatal(errRuntime)
				}
				t.Cleanup(runtime.Stop)
				adapter := &usagePermissionFailureAdapter{RuntimeAdapter: NewRuntimeAdapter(repo, "")}
				runtime.SetClusterAdapter(adapter)
				for _, auth := range []*coreauth.Auth{
					{ID: "billed-usage-a", Index: "billed-usage-a", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "3"}},
					{ID: "billed-usage-b", Index: "billed-usage-b", Provider: "codex", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{
						model: {Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)},
					}},
				} {
					if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
						t.Fatal(errUpsert)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				payload := fmt.Sprintf(`{"timestamp":%q,"execution_id":"billed-response","auth_index":"billed-usage-b","provider":"codex","api_key":%q,"model":%q,"tokens":{"total_tokens":1},"home_selection":{"reused":true,"model":%q}}`, time.Now().UTC().Format(time.RFC3339Nano), key, model, model)
				if inserted, errStore := adapter.StoreUsagePayload(ctx, payload, time.Now()); errStore != nil || !inserted {
					t.Fatalf("initial persistence: inserted=%t err=%v", inserted, errStore)
				}
				billedUser, errUser := repo.GetUser(ctx, user.ID)
				if errUser != nil || billedUser.Credits != credits-1 {
					t.Fatalf("billing: user=%+v err=%v", billedUser, errUser)
				}
				// Only the next request is blocked. Completed usage must still be
				// acknowledged, accounted, and allowed to recover model availability.
				_, _, errAdmission := repo.AllowedDispatchIDsForAPIKeyModel(ctx, key, model)
				if errAdmission == nil {
					t.Fatal("next request should be denied after exhausting the limit")
				}
				code, _ := homeerrors.SplitRedisErrorMessage(errAdmission.Error())
				wantCode := homeerrors.TypeUserCreditsInsufficient
				if limit == "period" {
					wantCode = homeerrors.TypeUserPeriodLimitExceeded
				}
				if code != wantCode {
					t.Fatalf("admission code=%s, want %s", code, wantCode)
				}
				if feedback == "permission-retry" {
					failure := errors.New("usage permissions temporarily unavailable")
					adapter.permissionErr = failure
					if errFeedback := runtime.RecordUsagePayload(ctx, payload); !errors.Is(errFeedback, failure) {
						t.Fatalf("permission failure after persistence=%v", errFeedback)
					}
					adapter.permissionErr = nil
				} else if feedback == "record" {
					if errFeedback := runtime.RecordUsagePayload(ctx, payload); errFeedback != nil {
						t.Fatal(errFeedback)
					}
				}
				for range 3 {
					if inserted, errStore := adapter.StoreUsagePayload(ctx, payload, time.Now()); errStore != nil || inserted {
						t.Fatalf("replay persistence: inserted=%t err=%v", inserted, errStore)
					}
					if errFeedback := runtime.ApplyUsagePayloadFeedback(ctx, payload); errFeedback != nil {
						t.Fatal(errFeedback)
					}
				}
				auth, ok := runtime.CoreManager().GetByID("billed-usage-b")
				if !ok || (auth.ModelStates[model] != nil && auth.ModelStates[model].Unavailable) {
					t.Fatalf("completed usage did not apply recovery feedback: %+v", auth)
				}
				var recentSuccess int64
				for _, bucket := range auth.RecentRequestsSnapshot(time.Now()) {
					recentSuccess += bucket.Success
				}
				if auth.Success != 1 || recentSuccess != 1 {
					t.Fatalf("feedback retry statistics: success=%d recent=%d, want 1", auth.Success, recentSuccess)
				}
				// One reused B call at 3:1 leaves exactly three A calls of debt.
				for i, want := range []string{"billed-usage-a", "billed-usage-a", "billed-usage-a", "billed-usage-a", "billed-usage-a", "billed-usage-b"} {
					decision, errDispatch := runtime.CoreManager().Dispatch(ctx, []string{"codex"}, model, coreauth.Options{})
					if errDispatch != nil || decision == nil || decision.Auth.ID != want {
						t.Fatalf("pick %d: decision=%+v err=%v, want %s", i, decision, errDispatch, want)
					}
				}
				replayedUser, errReplayUser := repo.GetUser(ctx, user.ID)
				if errReplayUser != nil || replayedUser.Credits != credits-1 {
					t.Fatalf("replay balance: user=%+v err=%v", replayedUser, errReplayUser)
				}
				charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
				if errCharges != nil || len(charges.Records) != 1 {
					t.Fatalf("replay billing: charges=%+v err=%v", charges, errCharges)
				}
			})
		}
	}
}

func TestReusedUsageAcknowledgesDeletedAndRotatedAPIKeys(t *testing.T) {
	for _, action := range []string{"delete", "rotate"} {
		for _, queued := range []bool{false, true} {
			for _, replayFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/queued=%t/replay-first=%t", action, queued, replayFirst), func(t *testing.T) {
					ctx := t.Context()
					repo := newCredentialFoundationTestRepository(t)
					key, errKey := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: "old-client-key"})
					if errKey != nil {
						t.Fatal(errKey)
					}
					runtime, errRuntime := home.NewRuntime(&config.Config{AuthDir: t.TempDir(), Routing: config.RoutingConfig{Strategy: "weighted-round-robin"}})
					if errRuntime != nil {
						t.Fatal(errRuntime)
					}
					t.Cleanup(runtime.Stop)
					adapter := NewRuntimeAdapter(repo, "")
					runtime.SetClusterAdapter(adapter)
					const authID, model = "deleted-key-usage-auth", "deleted-key-usage-model"
					for _, id := range []string{"deleted-key-usage-peer", authID} {
						auth := &coreauth.Auth{ID: id, Index: id, Provider: "codex", Status: coreauth.StatusActive}
						if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
							t.Fatal(errUpsert)
						}
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
						if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					payload := fmt.Sprintf(`{"timestamp":%q,"execution_id":"deleted-key-response","api_key":"old-client-key","auth_index":%q,"provider":"codex","model":%q,"failed":true,"fail":{"status_code":429},"home_selection":{"reused":true,"model":%q}}`, time.Now().UTC().Format(time.RFC3339Nano), authID, model, model)
					if queued {
						if inserted, errStore := adapter.StoreUsagePayload(ctx, payload, time.Now()); errStore != nil || !inserted {
							t.Fatalf("queued persistence: inserted=%t err=%v", inserted, errStore)
						}
					}
					if action == "delete" {
						if errDelete := repo.DeleteAPIKey(ctx, APIKeySelector{ID: key.ID}); errDelete != nil {
							t.Fatal(errDelete)
						}
					} else {
						nextKey := "rotated-client-key"
						if _, errRotate := repo.UpdateAPIKey(ctx, APIKeySelector{ID: key.ID}, APIKeyAdminUpdate{APIKey: &nextKey}); errRotate != nil {
							t.Fatal(errRotate)
						}
					}
					if _, _, errPolicy := adapter.AllowedUsageIDsForAPIKeyModel(ctx, "old-client-key", model); !errors.Is(errPolicy, homeerrors.ErrUsageAPIKeyNotFound) {
						t.Fatalf("old key must be terminally absent: %v", errPolicy)
					}
					if inserted, errStore := adapter.StoreUsagePayload(ctx, payload, time.Now()); errStore != nil || inserted == queued {
						t.Fatalf("persistence: inserted=%t err=%v", inserted, errStore)
					}
					if !replayFirst {
						if errFeedback := runtime.RecordUsagePayload(ctx, payload); errFeedback != nil {
							t.Fatal(errFeedback)
						}
					}
					for range 3 {
						if errFeedback := runtime.ApplyUsagePayloadFeedback(ctx, payload); errFeedback != nil {
							t.Fatal(errFeedback)
						}
					}
					got, ok := runtime.CoreManager().GetByID(authID)
					if !ok || got.ModelStates[model] == nil || !got.ModelStates[model].Quota.Exceeded || !got.ModelStates[model].Unavailable {
						t.Fatalf("missing key prevented cooldown feedback: %+v", got)
					}
					var recentFailed int64
					for _, bucket := range got.RecentRequestsSnapshot(time.Now()) {
						recentFailed += bucket.Failed
					}
					if got.Failed != 1 || recentFailed != 1 {
						t.Fatalf("request counted %d times, recent=%d, want 1", got.Failed, recentFailed)
					}
				})
			}
		}
	}
}

func TestUsagePermissionsPreserveModelChannelRestrictions(t *testing.T) {
	ctx := t.Context()
	repo := newCredentialFoundationTestRepository(t)
	if _, errUpsert := repo.UpsertAuth(ctx, &coreauth.Auth{ID: "allowed-auth", Index: "allowed-auth", Provider: "codex", Status: coreauth.StatusActive}, "test"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	channel, errChannel := repo.CreateChannelGroup(ctx, "usage-channel", false)
	if errChannel != nil {
		t.Fatal(errChannel)
	}
	if _, errDetail := repo.CreateChannelGroupDetail(ctx, channel.ID, "allowed-auth"); errDetail != nil {
		t.Fatal(errDetail)
	}
	models, errModels := repo.CreateModelGroup(ctx, "usage-models", false)
	if errModels != nil {
		t.Fatal(errModels)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, models.ID, "route", []uint{channel.ID}); errDetail != nil {
		t.Fatal(errDetail)
	}
	groups := []uint{models.ID}
	if _, errKey := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: "usage-key", ModelGroups: &groups}); errKey != nil {
		t.Fatal(errKey)
	}
	authIDs, modelIDs, errAllowed := repo.AllowedUsageIDsForAPIKeyModel(ctx, "usage-key", "route(high)")
	if errAllowed != nil || !reflect.DeepEqual(authIDs, []string{"allowed-auth"}) || !reflect.DeepEqual(modelIDs, []string{"route"}) {
		t.Fatalf("usage permissions=(%v,%v,%v)", authIDs, modelIDs, errAllowed)
	}
	if _, _, errMissing := repo.AllowedUsageIDsForAPIKeyModel(ctx, "missing", "route"); !errors.Is(errMissing, homeerrors.ErrUsageAPIKeyNotFound) || !errors.Is(errMissing, gorm.ErrRecordNotFound) {
		t.Fatalf("missing API key error=%v", errMissing)
	}
	// Query failures are not translated into unrestricted or empty permissions.
	if errDrop := repo.db.Migrator().DropTable(&ModelGroupDetailRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if _, _, errQuery := repo.AllowedUsageIDsForAPIKeyModel(ctx, "usage-key", "route"); errQuery == nil {
		t.Fatal("permission query failure must propagate")
	}
}
