package management

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

func TestConfigV8GETDoesNotInitializeOrLockLifecycle(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		name := "missing lifecycle"
		if initialized {
			name = "initialized lifecycle"
		}
		t.Run(name, func(t *testing.T) {
			db, cleanup := openManagementLogTestDB(t)
			t.Cleanup(cleanup)
			repo := cluster.NewRepository(db)
			if initialized {
				if _, errEnsure := repo.EnsureLifecycleConfig(t.Context(), cluster.DefaultHeartbeatTimeout()); errEnsure != nil {
					t.Fatal(errEnsure)
				}
			}
			writes := 0
			denyWrite := func(tx *gorm.DB) {
				writes++
				tx.AddError(errors.New("GET must not write"))
			}
			if errRegister := db.Callback().Create().Before("gorm:create").Register("test:deny_create", denyWrite); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errRegister := db.Callback().Update().Before("gorm:update").Register("test:deny_update", denyWrite); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errRegister := db.Callback().Delete().Before("gorm:delete").Register("test:deny_delete", denyWrite); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errRegister := db.Callback().Query().Before("gorm:query").Register("test:deny_lock", func(tx *gorm.DB) {
				if _, locking := tx.Statement.Clauses["FOR"]; locking {
					t.Error("GET requested a row lock")
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			handler := NewHandler(repo, nil, "127.0.0.1", 8327)
			router := gin.New()
			router.GET("/v8/management/config", handler.ConfigV8)
			router.GET("/v8/management/config/*path", handler.ConfigV8)
			router.GET("/v8/management/config.yaml", handler.ConfigV8)
			for _, path := range []string{"/v8/management/config", "/v8/management/config/credentials/concurrency", "/v8/management/config.yaml"} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				want := http.StatusOK
				if !initialized {
					want = http.StatusInternalServerError
					if !strings.Contains(response.Body.String(), "config_load_failed") {
						t.Fatalf("missing lifecycle response: %s", response.Body.String())
					}
				}
				if response.Code != want {
					t.Fatalf("GET %s: status=%d, want %d: %s", path, response.Code, want, response.Body.String())
				}
			}
			if writes != 0 {
				t.Fatalf("GET attempted %d writes", writes)
			}
		})
	}
}
