package dynamic

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

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
	creditsUnlimited := true
	user, errCreateUser := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, CreditsUnlimited: &creditsUnlimited})
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
	return rt
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
