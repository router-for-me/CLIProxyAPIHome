package home

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
)

func TestUsageFeedbackRequestStatsAfterPermissionRetry(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			runtime := newWeightedUsageFeedbackRuntime(t)
			failure := errors.New("permission database unavailable")
			adapter := &weightedUsagePolicyErrorAdapter{err: failure}
			runtime.clusterAdapter = adapter
			doc := ojson.NewDocString(reusedFeedbackUsage)
			if errSet := doc.Set("timestamp", time.Now().UTC().Format(time.RFC3339Nano)); errSet != nil {
				t.Fatal(errSet)
			}
			if errSet := doc.Set("failed", failed); errSet != nil {
				t.Fatal(errSet)
			}
			if failed {
				if errSet := doc.Set("fail.status_code", 429); errSet != nil {
					t.Fatal(errSet)
				}
			}
			payload := doc.String()
			for range 2 {
				if errUsage := runtime.RecordUsagePayload(t.Context(), payload); !errors.Is(errUsage, failure) {
					t.Fatalf("permission failure=%v", errUsage)
				}
			}
			assertUsageFeedbackStats(t, runtime, 0, 0)
			adapter.err = nil
			for range 3 {
				if errFeedback := runtime.ApplyUsagePayloadFeedback(t.Context(), payload); errFeedback != nil {
					t.Fatal(errFeedback)
				}
			}
			// Even a duplicate delivered through the initial-record path must not
			// count twice. Statistics dedupe is independent of database insertion.
			if errUsage := runtime.RecordUsagePayload(t.Context(), payload); errUsage != nil {
				t.Fatal(errUsage)
			}
			wantSuccess, wantFailed := int64(1), int64(0)
			if failed {
				wantSuccess, wantFailed = 0, 1
			}
			assertUsageFeedbackStats(t, runtime, wantSuccess, wantFailed)
		})
	}
}

func TestUsageFeedbackMissingKeyIsTerminalButDatabaseErrorsRetry(t *testing.T) {
	failure := errors.New("database unavailable")
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "deleted key", err: fmt.Errorf("permissions: %w", homeerrors.ErrUsageAPIKeyNotFound)},
		{name: "database outage", err: failure, want: failure},
		{name: "unclassified missing record", err: gorm.ErrRecordNotFound, want: gorm.ErrRecordNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := newWeightedUsageFeedbackRuntime(t)
			runtime.clusterAdapter = &weightedUsagePolicyErrorAdapter{err: test.err}
			if errUsage := runtime.RecordUsagePayload(t.Context(), reusedFeedbackUsage); !errors.Is(errUsage, test.want) {
				t.Fatalf("feedback=%v, want %v", errUsage, test.want)
			}
			if errReplay := runtime.ApplyUsagePayloadFeedback(t.Context(), reusedFeedbackUsage); !errors.Is(errReplay, test.want) {
				t.Fatalf("replay=%v, want %v", errReplay, test.want)
			}
			wantSuccess := int64(0)
			if test.want == nil {
				wantSuccess = 1
			}
			assertUsageFeedbackStats(t, runtime, wantSuccess, 0)
		})
	}
}

func assertUsageFeedbackStats(t *testing.T, runtime *Runtime, success, failed int64) {
	t.Helper()
	auth, ok := runtime.coreManager.GetByID("feedback-usage-b")
	if !ok {
		t.Fatal("feedback auth not loaded")
	}
	var recentSuccess, recentFailed int64
	for _, bucket := range auth.RecentRequestsSnapshot(time.Now()) {
		recentSuccess += bucket.Success
		recentFailed += bucket.Failed
	}
	if auth.Success != success || auth.Failed != failed || recentSuccess != success || recentFailed != failed {
		t.Fatalf("stats=%d/%d recent=%d/%d, want %d/%d", auth.Success, auth.Failed, recentSuccess, recentFailed, success, failed)
	}
}
