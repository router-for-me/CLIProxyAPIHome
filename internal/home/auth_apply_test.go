package home

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestApplyCoreAuthUpdateClearsCompletedRefreshLeaseDeadline(t *testing.T) {
	runtime, errRuntime := NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	t.Cleanup(runtime.Stop)

	oldRefreshAt := time.Now().UTC().Add(-time.Hour)
	existing := &coreauth.Auth{
		ID:               "refresh-event-auth",
		Index:            "refresh-event-auth",
		Provider:         "codex",
		Status:           coreauth.StatusError,
		Unavailable:      true,
		LastRefreshedAt:  oldRefreshAt,
		NextRefreshAfter: time.Now().UTC().Add(5 * time.Minute),
		NextRetryAfter:   time.Now().UTC().Add(5 * time.Minute),
		UpdatedAt:        time.Now().UTC().Add(-time.Minute),
	}
	if _, errRegister := runtime.CoreManager().Register(coreauth.WithSkipPersist(context.Background()), existing); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	completedAt := time.Now().UTC()
	incoming := existing.Clone()
	incoming.Status = coreauth.StatusActive
	incoming.Unavailable = false
	incoming.LastRefreshedAt = completedAt
	incoming.NextRefreshAfter = time.Time{}
	incoming.NextRetryAfter = time.Time{}
	incoming.UpdatedAt = completedAt
	runtime.applyCoreAuthAddOrUpdate(context.Background(), incoming)

	updated, ok := runtime.CoreManager().GetByID(existing.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.NextRefreshAfter.IsZero() || !updated.NextRetryAfter.IsZero() || updated.Unavailable {
		t.Fatalf("completed refresh retained lease state: %#v", updated)
	}
}
