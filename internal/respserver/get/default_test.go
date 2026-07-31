package get

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func newGetTestRuntime(t *testing.T) *home.Runtime {
	t.Helper()

	ctx := context.Background()
	db, errOpenSQLite := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
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

	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(cluster.NewRepository(db), ""))
	return rt
}

func TestHandleDefaultGETPlainKeyUsesKV(t *testing.T) {
	rt := newGetTestRuntime(t)
	if written, errSet := rt.KVSet(context.Background(), "plain:key", []byte("value"), 0, ""); errSet != nil || !written {
		t.Fatalf("KVSet() = %v, %v, want true, nil", written, errSet)
	}

	reply := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", "plain:key"})
	if reply.Kind != dispatch.ReplyKindBulkString || string(reply.BulkString) != "value" {
		t.Fatalf("handleDefault(GET plain:key) = %#v, want bulk value", reply)
	}
}

func TestHandleDefaultGETPlainKeyMissingReturnsNullBulk(t *testing.T) {
	rt := newGetTestRuntime(t)
	reply := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", "missing"})
	if reply.Kind != dispatch.ReplyKindBulkString || reply.BulkString != nil {
		t.Fatalf("handleDefault(GET missing) = %#v, want null bulk", reply)
	}
}

func TestHandleDefaultGETRefreshJSONCompatibility(t *testing.T) {
	rt := newGetTestRuntime(t)
	rt.SetClusterRefreshHandler(func(ctx context.Context, authIndex, observedAccessTokenSHA256 string) ([]byte, error) {
		if authIndex != "auth-1" {
			t.Fatalf("authIndex = %q, want auth-1", authIndex)
		}
		if observedAccessTokenSHA256 != "token-hash" {
			t.Fatalf("access token hash = %q, want token-hash", observedAccessTokenSHA256)
		}
		return []byte(`{"ok":true}`), nil
	})

	reply := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", `{"type":"refresh","auth_index":"auth-1","access_token_sha256":"token-hash"}`})
	if reply.Kind != dispatch.ReplyKindBulkString || string(reply.BulkString) != `{"ok":true}` {
		t.Fatalf("handleDefault(refresh) = %#v, want refresh payload", reply)
	}
}

func TestHandleDefaultGETRefreshPreservesAuthenticationError(t *testing.T) {
	rt := newGetTestRuntime(t)
	rt.SetClusterRefreshHandler(func(context.Context, string, string) ([]byte, error) {
		return nil, &coreauth.Error{
			Code:       "authentication_error",
			Message:    "credential unauthorized",
			HTTPStatus: 401,
		}
	})

	reply := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", `{"type":"refresh","auth_index":"auth-1"}`})
	if reply.Kind != dispatch.ReplyKindBulkString {
		t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
	}
	if got := gjson.GetBytes(reply.BulkString, "error.type").String(); got != "authentication_error" {
		t.Fatalf("error.type = %q, want authentication_error; body=%s", got, string(reply.BulkString))
	}
	if got := gjson.GetBytes(reply.BulkString, "error.message").String(); got != "credential unauthorized" {
		t.Fatalf("error.message = %q, want redacted authentication message; body=%s", got, string(reply.BulkString))
	}
}

func TestHandleDefaultGETRefreshRedactsTransientInfrastructureError(t *testing.T) {
	rt := newGetTestRuntime(t)
	rt.SetClusterRefreshHandler(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("database unavailable: provider-secret")
	})

	reply := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", `{"type":"refresh","auth_index":"auth-1"}`})
	if got := gjson.GetBytes(reply.BulkString, "error.type").String(); got != "refresh_temporarily_unavailable" {
		t.Fatalf("error.type = %q, want refresh_temporarily_unavailable; body=%s", got, string(reply.BulkString))
	}
	if strings.Contains(string(reply.BulkString), "provider-secret") {
		t.Fatalf("refresh error leaked infrastructure detail: %s", string(reply.BulkString))
	}
}

func TestHandleDefaultGETJSONErrorsRemainJSON(t *testing.T) {
	rt := newGetTestRuntime(t)

	unsupported := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", `{"type":"unknown"}`})
	if unsupported.Kind != dispatch.ReplyKindBulkString || !strings.Contains(strings.ToLower(gjson.GetBytes(unsupported.BulkString, "error.message").String()), "unsupported") {
		t.Fatalf("handleDefault(unsupported) = %#v, want unsupported type JSON error", unsupported)
	}

	invalid := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", `{bad}`})
	if invalid.Kind != dispatch.ReplyKindBulkString || !strings.Contains(strings.ToLower(gjson.GetBytes(invalid.BulkString, "error.message").String()), "invalid") {
		t.Fatalf("handleDefault(invalid) = %#v, want invalid request JSON error", invalid)
	}
}

func TestHandleDefaultModelsRejectsBadCredentials(t *testing.T) {
	// The models branch must route through AuthenticateHTTPRequest, so requests
	// without a valid credential surface an auth error instead of model data.
	rt := newGetTestRuntime(t)

	cases := []struct {
		name      string
		payload   string
		wantError string
	}{
		{"missing", `{"type":"models"}`, "no_credentials"},
		{"invalid", `{"type":"models","headers":{"x-api-key":"missing-client-key"}}`, "invalid_credential"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := handleDefault(context.Background(), dispatch.Env{Runtime: rt}, []string{"GET", tc.payload})
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
