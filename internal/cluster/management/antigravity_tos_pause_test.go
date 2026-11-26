package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

func TestGetAntigravityTOSPause(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour)
	active := strconv.FormatInt(future.UnixNano(), 10)
	expired := strconv.FormatInt(future.Add(-2*time.Hour).UnixNano(), 10)
	for _, tc := range []struct {
		name, window, stored string
		noRuntime            bool
		wantHTTP             int
		wantResume           time.Time
	}{
		{name: "no deadline", window: "6h", wantHTTP: 200},
		{name: "active", window: "6h", stored: active, wantHTTP: 200, wantResume: future},
		{name: "expired value still in KV", window: "6h", stored: expired, wantHTTP: 200},
		{name: "disabled with future deadline", window: "0", stored: active, wantHTTP: 200},
		{name: "unreadable deadline", window: "6h", stored: "invalid-private-data", wantHTTP: 503},
		{name: "missing runtime", noRuntime: true, wantHTTP: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, closeDB := openManagementLogTestDB(t)
			t.Cleanup(closeDB)
			repo := cluster.NewRepository(db)
			var runtime *home.Runtime
			if !tc.noRuntime {
				cfg := &config.Config{AuthDir: t.TempDir(), Antigravity: config.AntigravityConfig{TOSPauseWindow: tc.window}}
				rt, errRuntime := home.NewRuntime(cfg)
				if errRuntime != nil {
					t.Fatal(errRuntime)
				}
				t.Cleanup(rt.Stop)
				rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "pause-status-test"))
				runtime = rt
			}
			const key = "cpa:antigravity:tos-pause:until"
			if tc.stored != "" {
				if _, errSet := repo.KVSet(t.Context(), key, []byte(tc.stored), time.Hour, cluster.KVSetModeAlways); errSet != nil {
					t.Fatal(errSet)
				}
			}
			engine := gin.New()
			const path = "/v8/management/routing/antigravity/tos-pause"
			engine.GET(path, NewHandler(repo, runtime, "", 0).GetAntigravityTOSPause)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != tc.wantHTTP || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response = %d %s %s", rec.Code, rec.Header(), rec.Body)
			}
			if tc.wantHTTP != http.StatusOK {
				if !strings.Contains(rec.Body.String(), "antigravity_tos_pause_unavailable") || strings.Contains(rec.Body.String(), "invalid-private-data") {
					t.Fatalf("unexpected error response: %s", rec.Body)
				}
				return
			}
			var got struct {
				Paused     *bool           `json:"paused"`
				ResumeAt   json.RawMessage `json:"resume_at"`
				ObservedAt time.Time       `json:"observed_at"`
			}
			if errDecode := json.Unmarshal(rec.Body.Bytes(), &got); errDecode != nil {
				t.Fatal(errDecode)
			}
			wantResume := "null"
			wantPaused := !tc.wantResume.IsZero()
			if wantPaused {
				wantResume = strconv.Quote(tc.wantResume.Format(time.RFC3339Nano))
			}
			if got.Paused == nil || *got.Paused != wantPaused || string(got.ResumeAt) != wantResume || got.ObservedAt.IsZero() {
				t.Fatalf("unexpected observation: %s", rec.Body)
			}
			raw, found, errRead := repo.KVGet(t.Context(), key)
			if errRead != nil || found != (tc.stored != "") || string(raw) != tc.stored {
				t.Fatalf("stored deadline changed on read: found=%v, error=%v", found, errRead)
			}
		})
	}
}
