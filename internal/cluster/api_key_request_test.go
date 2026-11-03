package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAPIKeyRequestCacheReusesRecord(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	if _, errCreate := repo.CreateAPIKey(t.Context(), APIKeyEntryUpdate{APIKey: "client-key"}); errCreate != nil {
		t.Fatal(errCreate)
	}
	counter := &lifecycleSQLCounter{Interface: logger.Default.LogMode(logger.Silent)}
	repo.db = repo.db.Session(&gorm.Session{Logger: counter})
	ctx := WithAPIKeyRequestCache(t.Context())
	if valid, errValidate := repo.ValidateAPIKey(ctx, " client-key "); errValidate != nil || !valid {
		t.Fatalf("validation=(%t,%v)", valid, errValidate)
	}
	if _, _, errAllowed := repo.AllowedDispatchIDsForAPIKey(ctx, "client-key"); errAllowed != nil {
		t.Fatal(errAllowed)
	}
	if _, _, errAllowed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, "client-key", "gpt"); errAllowed != nil {
		t.Fatal(errAllowed)
	}
	if _, errAllowed := repo.AllowedAuthIDsForAPIKey(ctx, "client-key"); errAllowed != nil {
		t.Fatal(errAllowed)
	}
	if _, errAllowed := repo.AllowedModelIDsForAPIKey(ctx, "client-key"); errAllowed != nil {
		t.Fatal(errAllowed)
	}
	if _, errAccess := repo.ModelAccessForAPIKey(ctx, "client-key"); errAccess != nil {
		t.Fatal(errAccess)
	}
	if count := counter.statements.Load(); count != 1 {
		t.Fatalf("authentication and access SQL statements=%d, want 1", count)
	}
}

func TestAPIKeyRequestCacheIsScopedToRequestRepositoryAndKey(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	record, errCreate := repo.CreateAPIKey(t.Context(), APIKeyEntryUpdate{APIKey: "client-key"})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	ctx := WithAPIKeyRequestCache(t.Context())
	if valid, errValidate := repo.ValidateAPIKey(ctx, record.APIKey); errValidate != nil || !valid {
		t.Fatalf("validation=(%t,%v)", valid, errValidate)
	}
	otherRepo := newCredentialFoundationTestRepository(t)
	if valid, errValidate := otherRepo.ValidateAPIKey(ctx, record.APIKey); errValidate != nil || valid {
		t.Fatalf("other repository validation=(%t,%v)", valid, errValidate)
	}
	if valid, errValidate := repo.ValidateAPIKey(ctx, "missing"); errValidate != nil || valid {
		t.Fatalf("other key validation=(%t,%v)", valid, errValidate)
	}
	if _, _, errAllowed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, "missing", "gpt"); !errors.Is(errAllowed, gorm.ErrRecordNotFound) {
		t.Fatalf("missing key access error=%v", errAllowed)
	}
	if errDelete := repo.db.Delete(record).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	for _, nextCtx := range []context.Context{WithAPIKeyRequestCache(ctx), t.Context()} {
		if valid, errValidate := repo.ValidateAPIKey(nextCtx, record.APIKey); errValidate != nil || valid {
			t.Fatalf("next request validation=(%t,%v)", valid, errValidate)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, errAllowed := repo.AllowedDispatchIDsForAPIKey(canceled, record.APIKey); !errors.Is(errAllowed, context.Canceled) {
		t.Fatalf("canceled access error=%v", errAllowed)
	}
}

func TestAPIKeyRequestCacheDoesNotCacheBilling(t *testing.T) {
	for _, resolver := range []string{"dispatch", "model dispatch"} {
		for _, limit := range []string{"credits", "period"} {
			t.Run(resolver+"/"+limit, func(t *testing.T) {
				repo := newCredentialFoundationTestRepository(t)
				user := createPeriodLimitUser(t, repo, t.Context(), "user", 100)
				key := "client-key"
				if _, errCreate := repo.CreateAPIKeyForUser(t.Context(), user.ID, APIKeyUserUpdate{APIKey: &key}); errCreate != nil {
					t.Fatal(errCreate)
				}
				ctx := WithAPIKeyRequestCache(t.Context())
				if valid, errValidate := repo.ValidateAPIKey(ctx, key); errValidate != nil || !valid {
					t.Fatalf("validation=(%t,%v)", valid, errValidate)
				}
				if _, _, errAllowed := repo.AllowedDispatchIDsForAPIKey(ctx, key); errAllowed != nil {
					t.Fatal(errAllowed)
				}
				column, want := "credits", homeerrors.TypeUserCreditsInsufficient
				if limit == "period" {
					column, want = "limit_5h_credits", homeerrors.TypeUserPeriodLimitExceeded
				}
				if errUpdate := repo.db.Model(user).Update(column, 0).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				var errAllowed error
				if resolver == "dispatch" {
					_, _, errAllowed = repo.AllowedDispatchIDsForAPIKey(ctx, key)
				} else {
					_, _, errAllowed = repo.AllowedDispatchIDsForAPIKeyModel(ctx, key, "gpt")
				}
				if errAllowed == nil || !strings.Contains(errAllowed.Error(), want) {
					t.Fatalf("access error=%v, want %s", errAllowed, want)
				}
			})
		}
	}
}

func TestAPIKeyRequestCacheDoesNotCacheModelPermissions(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	group, errGroup := repo.CreateModelGroup(t.Context(), "models", false)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	if _, errDetail := repo.CreateModelGroupDetail(t.Context(), group.ID, "gpt", nil); errDetail != nil {
		t.Fatal(errDetail)
	}
	groups := []uint{group.ID}
	if _, errKey := repo.CreateAPIKey(t.Context(), APIKeyEntryUpdate{APIKey: "client-key", ModelGroups: &groups}); errKey != nil {
		t.Fatal(errKey)
	}
	ctx := WithAPIKeyRequestCache(t.Context())
	if valid, errValidate := repo.ValidateAPIKey(ctx, "client-key"); errValidate != nil || !valid {
		t.Fatalf("validation=(%t,%v)", valid, errValidate)
	}
	if _, models, errAllowed := repo.AllowedDispatchIDsForAPIKey(ctx, "client-key"); errAllowed != nil || len(models) != 1 || models[0] != "gpt" {
		t.Fatalf("initial models=(%v,%v)", models, errAllowed)
	}
	disabled := true
	if _, errDisable := repo.UpdateModelGroup(t.Context(), group.ID, ModelGroupUpdate{Disabled: &disabled}); errDisable != nil {
		t.Fatal(errDisable)
	}
	if _, models, errAllowed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, "client-key", "gpt"); errAllowed != nil || models == nil || len(models) != 0 {
		t.Fatalf("disabled models=(%v,%v)", models, errAllowed)
	}
}
