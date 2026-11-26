package cluster

import (
	"errors"
	"testing"
)

func TestUsagePluginDeliveryCallbackFailureDoesNotComplete(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var repo *Repository
			if backend == "postgres" {
				repo = newPostgresQuiescenceRepository(t)
			} else {
				var closeRepo func()
				repo, closeRepo = newBillingTestRepository(t, t.Context())
				defer closeRepo()
			}
			payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"unconfirmed-callback"}`
			if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend != nil {
				t.Fatal(errAppend)
			}
			pending := errors.New("plugin callback is pending")
			if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { return pending }); !errors.Is(errDelivery, pending) {
				t.Fatalf("callback error was not propagated: %v", errDelivery)
			}
			var record UsageRecord
			if errFirst := repo.db.First(&record).Error; errFirst != nil || record.PluginUsageDelivered {
				t.Fatalf("unconfirmed delivery was completed: record=%+v error=%v", record, errFirst)
			}
			// A new repository has no old local in-flight state. The durable marker
			// must still allow the unconfirmed event to reach its consumer.
			retry := NewRepository(repo.db)
			calls := 0
			for attempt := 0; attempt < 2; attempt++ {
				if errDelivery := retry.DeliverPluginUsage(t.Context(), payload, func() error { calls++; return nil }); errDelivery != nil {
					t.Fatal(errDelivery)
				}
			}
			if calls != 1 {
				t.Fatalf("confirmed callback count=%d, want one", calls)
			}
			if errFirst := repo.db.First(&record).Error; errFirst != nil || !record.PluginUsageDelivered {
				t.Fatalf("confirmed delivery was not completed: record=%+v error=%v", record, errFirst)
			}
		})
	}
}
