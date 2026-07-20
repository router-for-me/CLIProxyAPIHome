package dynamic

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestHandleAuthValidate(t *testing.T) {
	ctx := context.Background()
	rt := newAuthValidateRuntime(t, ctx, "valid-client-key", "deleted-client-key")

	cases := []struct {
		name              string
		payload           string
		wantAuthenticated bool
		wantPrincipal     string
		wantSource        string
		wantErrorType     string
	}{
		{
			name:              "valid bearer",
			payload:           `{"type":"auth-validate","headers":{"authorization":"Bearer valid-client-key"}}`,
			wantAuthenticated: true,
			wantPrincipal:     "valid-client-key",
			wantSource:        "authorization",
		},
		{
			name:              "valid query key",
			payload:           `{"type":"auth-validate","query":{"key":"valid-client-key"}}`,
			wantAuthenticated: true,
			wantPrincipal:     "valid-client-key",
			wantSource:        "query-key",
		},
		{
			name:          "missing",
			payload:       `{"type":"auth-validate"}`,
			wantErrorType: "no_credentials",
		},
		{
			name:          "invalid nonexistent",
			payload:       `{"type":"auth-validate","headers":{"x-api-key":"missing-client-key"}}`,
			wantErrorType: "invalid_credential",
		},
		{
			name:          "invalid soft deleted",
			payload:       `{"type":"auth-validate","headers":{"x-goog-api-key":"deleted-client-key"}}`,
			wantErrorType: "invalid_credential",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := handleAuthValidate(ctx, dispatch.Env{Runtime: rt}, []string{"RPOP", tc.payload})
			if reply.Kind != dispatch.ReplyKindBulkString {
				t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
			}

			var got struct {
				Authenticated bool              `json:"authenticated"`
				Principal     string            `json:"principal"`
				Metadata      map[string]string `json:"metadata"`
				Error         *struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if errUnmarshal := json.Unmarshal(reply.BulkString, &got); errUnmarshal != nil {
				t.Fatalf("unmarshal response: %v; body=%s", errUnmarshal, string(reply.BulkString))
			}

			if got.Authenticated != tc.wantAuthenticated {
				t.Fatalf("authenticated = %t, want %t; body=%s", got.Authenticated, tc.wantAuthenticated, string(reply.BulkString))
			}
			if tc.wantAuthenticated {
				if got.Principal != tc.wantPrincipal {
					t.Fatalf("principal = %q, want %q", got.Principal, tc.wantPrincipal)
				}
				if got.Metadata["source"] != tc.wantSource {
					t.Fatalf("metadata.source = %q, want %q", got.Metadata["source"], tc.wantSource)
				}
				return
			}
			if got.Error == nil {
				t.Fatalf("error = nil, want %q; body=%s", tc.wantErrorType, string(reply.BulkString))
			}
			if got.Error.Type != tc.wantErrorType {
				t.Fatalf("error.type = %q, want %q; body=%s", got.Error.Type, tc.wantErrorType, string(reply.BulkString))
			}
		})
	}
}

func TestHandleAuthValidateRejectsWhenNoAPIKeysConfigured(t *testing.T) {
	ctx := context.Background()
	rt := newAuthValidateRuntimeWithoutKeys(t, ctx)

	cases := []struct {
		name          string
		payload       string
		wantErrorType string
	}{
		{
			name:          "missing",
			payload:       `{"type":"auth-validate"}`,
			wantErrorType: "no_credentials",
		},
		{
			name:          "invalid",
			payload:       `{"type":"auth-validate","headers":{"authorization":"Bearer any-client-key"}}`,
			wantErrorType: "invalid_credential",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := handleAuthValidate(ctx, dispatch.Env{Runtime: rt}, []string{"RPOP", tc.payload})
			if reply.Kind != dispatch.ReplyKindBulkString {
				t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
			}

			var got struct {
				Authenticated bool              `json:"authenticated"`
				Principal     string            `json:"principal"`
				Metadata      map[string]string `json:"metadata"`
				Error         *struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if errUnmarshal := json.Unmarshal(reply.BulkString, &got); errUnmarshal != nil {
				t.Fatalf("unmarshal response: %v; body=%s", errUnmarshal, string(reply.BulkString))
			}
			if got.Authenticated {
				t.Fatalf("authenticated = true, want false; body=%s", string(reply.BulkString))
			}
			if got.Error == nil {
				t.Fatalf("error = nil, want %q; body=%s", tc.wantErrorType, string(reply.BulkString))
			}
			if got.Error.Type != tc.wantErrorType {
				t.Fatalf("error.type = %q, want %q; body=%s", got.Error.Type, tc.wantErrorType, string(reply.BulkString))
			}
		})
	}
}

func newAuthValidateRuntime(t *testing.T, ctx context.Context, validKey string, deletedKey string) *home.Runtime {
	t.Helper()
	runtime, _ := newAuthValidateRuntimeWithRepository(t, ctx, validKey, deletedKey)
	return runtime
}

func newAuthValidateRuntimeWithRepository(t *testing.T, ctx context.Context, validKey string, deletedKey string) (*home.Runtime, *cluster.Repository) {
	t.Helper()

	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	repo := cluster.NewRepository(db)
	username := "auth-validate-user"
	user, errCreateUser := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, cluster.APIKeyUserUpdate{APIKey: &validKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser(valid) error = %v", errCreateKey)
	}
	deleted, errCreateDeleted := repo.CreateAPIKeyForUser(ctx, user.ID, cluster.APIKeyUserUpdate{APIKey: &deletedKey})
	if errCreateDeleted != nil {
		t.Fatalf("CreateAPIKeyForUser(deleted) error = %v", errCreateDeleted)
	}
	if errDelete := repo.DeleteAPIKeyForUser(ctx, user.ID, deleted.ID, ""); errDelete != nil {
		t.Fatalf("DeleteAPIKeyForUser() error = %v", errDelete)
	}

	rt, errRuntime := home.NewRuntime(&config.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.10"))
	return rt, repo
}

func TestHandleAuthValidateDoesNotReserveInFlightLease(t *testing.T) {
	ctx := context.Background()
	rt, repo := newAuthValidateRuntimeWithRepository(t, ctx, "valid-client-key", "deleted-client-key")
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:          "auth-validate-upstream",
		Index:       "auth-validate-upstream",
		Provider:    "codex",
		Status:      coreauth.StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
		MaxInFlight: 1,
		Metadata:    map[string]any{"type": "codex"},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	if errReload := rt.ReloadAuths(ctx); errReload != nil {
		t.Fatalf("ReloadAuths() error = %v", errReload)
	}

	reply := handleAuthValidate(ctx, dispatch.Env{Runtime: rt}, []string{"RPOP", `{"type":"auth-validate","headers":{"authorization":"Bearer valid-client-key"}}`})
	if reply.Kind != dispatch.ReplyKindBulkString {
		t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
	}
	summaries, _, errSummaries := repo.ListInFlightCredentialSummaries(ctx)
	if errSummaries != nil {
		t.Fatalf("ListInFlightCredentialSummaries() error = %v", errSummaries)
	}
	found := false
	for _, summary := range summaries {
		if summary.CredentialID != auth.ID {
			continue
		}
		found = true
		if summary.InFlight != 0 {
			t.Fatalf("auth-validate reserved %d leases, want 0", summary.InFlight)
		}
	}
	if !found {
		t.Fatalf("credential %q missing from in-flight summaries", auth.ID)
	}
}

func newAuthValidateRuntimeWithoutKeys(t *testing.T, ctx context.Context) *home.Runtime {
	t.Helper()

	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sqlite db: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	rt, errRuntime := home.NewRuntime(&config.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(cluster.NewRepository(db), "192.0.2.10"))
	return rt
}

func TestHandleAuthRejectsBadCredentials(t *testing.T) {
	// dispatchRequest must preserve the structured access error code so the downstream
	// proxy can map no_credentials / invalid_credential to 401 instead of a generic 502.
	ctx := context.Background()
	rt := newAuthValidateRuntime(t, ctx, "valid-client-key", "deleted-client-key")

	cases := []struct {
		name      string
		payload   string
		wantError string
	}{
		{
			name:      "missing credential",
			payload:   `{"type":"auth","model":"glm-5.2"}`,
			wantError: "no_credentials",
		},
		{
			name:      "invalid credential",
			payload:   `{"type":"auth","model":"glm-5.2","headers":{"x-api-key":"missing-client-key"}}`,
			wantError: "invalid_credential",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := handleAuth(ctx, dispatch.Env{Runtime: rt}, []string{"RPOP", tc.payload})
			if reply.Kind != dispatch.ReplyKindBulkString {
				t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
			}

			var got struct {
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if errUnmarshal := json.Unmarshal(reply.BulkString, &got); errUnmarshal != nil {
				t.Fatalf("unmarshal response: %v; body=%s", errUnmarshal, string(reply.BulkString))
			}
			if got.Error == nil {
				t.Fatalf("error = nil, want %q; body=%s", tc.wantError, string(reply.BulkString))
			}
			if got.Error.Type != tc.wantError {
				t.Fatalf("error.type = %q, want %q; body=%s", got.Error.Type, tc.wantError, string(reply.BulkString))
			}
		})
	}
}

func TestBuildDispatchErrorJSONUsesModelConcurrencyType(t *testing.T) {
	payload := buildDispatchErrorJSON(&home.ConcurrencyExceededError{
		Scope:        "model",
		CredentialID: "auth-model-limit",
		Model:        "gpt-5",
		Current:      2,
		Limit:        2,
	})
	var got struct {
		Error struct {
			Type  string `json:"type"`
			Scope string `json:"scope"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &got); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if got.Error.Type != "credential_model_concurrency_exceeded" || got.Error.Scope != "model" {
		t.Fatalf("error = %#v", got.Error)
	}
}
