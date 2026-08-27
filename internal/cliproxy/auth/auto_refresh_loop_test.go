package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

type autoRefreshTestResolver struct {
	auth  *Auth
	err   error
	calls int
}

func (r *autoRefreshTestResolver) GetFullAuth(_ context.Context, uuid string) (*Auth, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	if r.auth == nil || r.auth.ID != uuid {
		return nil, ErrFullAuthNotFound
	}
	return r.auth.Clone(), nil
}

func TestNextRefreshCheckAtExcludesIneligibleAuths(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		auth *Auth
	}{
		{
			name: "API key",
			auth: &Auth{
				Provider:   "codex",
				Status:     StatusActive,
				Attributes: map[string]string{"auth_kind": "api_key", "api_key": "test-key"},
			},
		},
		{
			name: "disabled OAuth flag",
			auth: &Auth{
				Provider:   "codex",
				Status:     StatusActive,
				Disabled:   true,
				Attributes: map[string]string{"auth_kind": "oauth"},
			},
		},
		{
			name: "disabled OAuth status",
			auth: &Auth{
				Provider:   "codex",
				Status:     StatusDisabled,
				Attributes: map[string]string{"auth_kind": "oauth"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if next, ok := nextRefreshCheckAt(now, test.auth, 15*time.Minute); ok || !next.IsZero() {
				t.Fatalf("nextRefreshCheckAt() = (%v, %t), want zero, false", next, ok)
			}
		})
	}
}

func TestNextRefreshCheckAtUsesProviderExpiryLead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		provider string
		lead     time.Duration
		expiry   time.Time
		want     time.Time
		wantDue  bool
	}{
		{name: "Codex", provider: "codex", lead: 5 * 24 * time.Hour, expiry: now.Add(10 * 24 * time.Hour), want: now.Add(5 * 24 * time.Hour)},
		{name: "Claude", provider: "claude", lead: 4 * time.Hour, expiry: now.Add(10 * 24 * time.Hour), want: now.Add(10*24*time.Hour - 4*time.Hour)},
		{name: "Antigravity", provider: "antigravity", lead: 50 * time.Minute, expiry: now.Add(10 * 24 * time.Hour), want: now.Add(10*24*time.Hour - 50*time.Minute)},
		{name: "Kimi", provider: "kimi", lead: 5 * time.Minute, expiry: now.Add(10 * 24 * time.Hour), want: now.Add(10*24*time.Hour - 5*time.Minute)},
		{name: "xAI", provider: "xai", lead: 5 * time.Minute, expiry: now.Add(10 * 24 * time.Hour), want: now.Add(10*24*time.Hour - 5*time.Minute)},
		{name: "boundary is due", provider: "xai", lead: 5 * time.Minute, expiry: now.Add(5 * time.Minute), want: now, wantDue: true},
	}
	manager := NewManager(nil, nil, nil)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{
				Provider: test.provider,
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token":  "test-access",
					"refresh_token": "test-refresh",
					"expires_at":    test.expiry.Format(time.RFC3339Nano),
				},
			}
			next, ok := nextRefreshCheckAt(now, auth, 15*time.Minute)
			if !ok || !next.Equal(test.want) {
				t.Fatalf("nextRefreshCheckAt() = (%v, %t), want %v, true", next, ok, test.want)
			}
			if gotDue := manager.ShouldRefreshCredential(auth, now); gotDue != test.wantDue {
				t.Fatalf("ShouldRefreshCredential() = %t, want %t", gotDue, test.wantDue)
			}
			if gotLead := ProviderRefreshLead(test.provider, nil); gotLead == nil || *gotLead != test.lead {
				t.Fatalf("ProviderRefreshLead() = %v, want %v", gotLead, test.lead)
			}
		})
	}
}

func TestBuiltInRefreshWithoutExpiryPolicy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	interval := 15 * time.Minute
	tests := []struct {
		name    string
		auth    *Auth
		want    time.Time
		wantDue bool
	}{
		{
			name: "missing access token refreshes immediately",
			auth: &Auth{
				Provider: "kimi",
				Status:   StatusActive,
				Metadata: map[string]any{"refresh_token": "test-refresh"},
			},
			want:    now,
			wantDue: true,
		},
		{
			name: "unknown Kimi expiry is rechecked without using lead as lifetime",
			auth: &Auth{
				Provider:        "kimi",
				Status:          StatusActive,
				LastRefreshedAt: now.Add(-24 * time.Hour),
				Metadata: map[string]any{
					"access_token":  "test-access",
					"refresh_token": "test-refresh",
				},
			},
			want: now.Add(interval),
		},
		{
			name: "explicit interval remains authoritative",
			auth: &Auth{
				Provider:        "kimi",
				Status:          StatusActive,
				LastRefreshedAt: now.Add(-30 * time.Minute),
				Attributes:      map[string]string{"refresh_interval_seconds": "1h"},
				Metadata: map[string]any{
					"access_token":  "test-access",
					"refresh_token": "test-refresh",
				},
			},
			want: now.Add(30 * time.Minute),
		},
		{
			name: "minimal OAuth remains initially due",
			auth: &Auth{
				Provider:   "kimi",
				Status:     StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth"},
			},
			want:    now,
			wantDue: true,
		},
	}
	manager := NewManager(nil, nil, nil)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next, ok := nextRefreshCheckAt(now, test.auth, interval)
			if !ok || !next.Equal(test.want) {
				t.Fatalf("nextRefreshCheckAt() = (%v, %t), want %v, true", next, ok, test.want)
			}
			if gotDue := manager.ShouldRefreshCredential(test.auth, now); gotDue != test.wantDue {
				t.Fatalf("ShouldRefreshCredential() = %t, want %t", gotDue, test.wantDue)
			}
		})
	}
}

func TestRefreshScheduleHonorsRefreshBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		backoff time.Duration
	}{
		{name: "pending", backoff: refreshPendingBackoff},
		{name: "ineffective refresh", backoff: refreshIneffectiveBackoff},
		{name: "transient failure", backoff: refreshFailureBackoff},
		{name: "cluster lease", backoff: 5 * time.Minute},
	}
	manager := NewManager(nil, nil, nil)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{
				Provider:         "codex",
				Status:           StatusActive,
				NextRefreshAfter: now.Add(test.backoff),
				Metadata: map[string]any{
					"access_token":  "test-access",
					"refresh_token": "test-refresh",
					"expires_at":    now.Format(time.RFC3339Nano),
				},
			}
			next, ok := nextRefreshCheckAt(now, auth, 15*time.Minute)
			if !ok || !next.Equal(auth.NextRefreshAfter) {
				t.Fatalf("nextRefreshCheckAt() = (%v, %t), want %v, true", next, ok, auth.NextRefreshAfter)
			}
			if manager.ShouldRefreshCredential(auth, now) {
				t.Fatal("ShouldRefreshCredential() = true during refresh backoff")
			}
		})
	}
}

func TestAutoRefreshReschedulesResolvedFullAuth(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Millisecond)
	expiry := start.Add(7 * 24 * time.Hour)
	minimal := &Auth{
		ID:           "resolved-not-due",
		Index:        "resolved-not-due",
		StateVersion: 7,
		Provider:     "codex",
		Status:       StatusActive,
		Attributes:   map[string]string{"auth_kind": "oauth"},
	}
	full := minimal.Clone()
	full.Metadata = map[string]any{
		"access_token":  "test-access",
		"refresh_token": "test-refresh",
		"expires_at":    expiry.Format(time.RFC3339Nano),
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), minimal); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	resolver := &autoRefreshTestResolver{auth: full}
	manager.SetFullAuthResolver(resolver)
	providerCalls := 0
	manager.SetAutoRefreshHandler(func(context.Context, *Auth) error {
		providerCalls++
		return nil
	})
	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)

	loop.handleDueAuth(context.Background(), start, minimal.ID)
	authID := <-loop.jobs
	resolved := manager.refreshAuth(context.Background(), authID)
	loop.rescheduleAfterRefresh(start.Add(time.Second), authID, resolved)

	if resolver.calls != 1 {
		t.Fatalf("full auth resolver calls = %d, want 1", resolver.calls)
	}
	if providerCalls != 0 {
		t.Fatalf("provider refresh calls = %d, want 0", providerCalls)
	}
	want := expiry.Add(-codexRefreshLead)
	item := loop.index[minimal.ID]
	if item == nil || !item.next.Equal(want) {
		t.Fatalf("scheduled refresh = %#v, want %v", item, want)
	}
	select {
	case <-loop.wakeCh:
	default:
		t.Fatal("resolved schedule did not wake the timer loop")
	}
	loop.queueReschedule(minimal.ID)
	loop.applyDirty(start.Add(time.Second))
	item = loop.index[minimal.ID]
	if item == nil || !item.next.Equal(want) {
		t.Fatalf("scheduled refresh after matching dirty state = %#v, want %v", item, want)
	}
	if due := loop.popDue(start.Add(refreshPendingBackoff)); len(due) != 0 {
		t.Fatalf("due auths after pending backoff = %v, want none", due)
	}
}

func TestAutoRefreshRefreshesTrulyDueFullAuthOnce(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Millisecond)
	minimal := &Auth{
		ID:           "resolved-due",
		Index:        "resolved-due",
		StateVersion: 11,
		Provider:     "antigravity",
		Status:       StatusActive,
		Attributes:   map[string]string{"auth_kind": "oauth"},
	}
	full := minimal.Clone()
	full.Metadata = map[string]any{
		"access_token":  "test-access",
		"refresh_token": "test-refresh",
		"expires_at":    start.Add(antigravityRefreshLead).Format(time.RFC3339Nano),
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), minimal); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	resolver := &autoRefreshTestResolver{auth: full}
	manager.SetFullAuthResolver(resolver)
	providerCalls := 0
	refreshedExpiry := start.Add(time.Hour)
	manager.SetAutoRefreshHandler(func(ctx context.Context, selected *Auth) error {
		providerCalls++
		updated := selected.Clone()
		updated.Metadata["access_token"] = "refreshed-access"
		updated.Metadata["expires_at"] = refreshedExpiry.Format(time.RFC3339Nano)
		_, errUpdate := manager.Update(ctx, updated)
		return errUpdate
	})
	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)

	loop.handleDueAuth(context.Background(), start, minimal.ID)
	authID := <-loop.jobs
	resolved := manager.refreshAuth(context.Background(), authID)
	loop.rescheduleAfterRefresh(start.Add(time.Second), authID, resolved)

	if providerCalls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1", providerCalls)
	}
	want := refreshedExpiry.Add(-antigravityRefreshLead)
	item := loop.index[minimal.ID]
	if item == nil || !item.next.Equal(want) {
		t.Fatalf("scheduled refresh = %#v, want %v", item, want)
	}
	if due := loop.popDue(start.Add(refreshPendingBackoff)); len(due) != 0 {
		t.Fatalf("due auths after one minute = %v, want none", due)
	}
}

func TestResolvedRefreshScheduleRejectsStaleStateVersion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	currentNext := now.Add(2 * time.Minute)
	current := &Auth{
		ID:               "stale-resolved",
		Index:            "stale-resolved",
		StateVersion:     2,
		Provider:         "codex",
		Status:           StatusActive,
		NextRefreshAfter: currentNext,
		Attributes:       map[string]string{"auth_kind": "oauth"},
	}
	resolved := current.Clone()
	resolved.StateVersion = 1
	resolved.NextRefreshAfter = time.Time{}
	resolved.Metadata = map[string]any{
		"access_token":  "stale-access",
		"refresh_token": "stale-refresh",
		"expires_at":    now.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)
	loop.rescheduleAfterRefresh(now, current.ID, resolved)

	item := loop.index[current.ID]
	if item == nil || !item.next.Equal(currentNext) {
		t.Fatalf("scheduled refresh = %#v, want current index deadline %v", item, currentNext)
	}
}

func TestDirtyRescheduleVersionOrdering(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		committedVersion int64
		dirtyVersion     int64
		wantDirty        bool
	}{
		{name: "older dirty is rejected", committedVersion: 2, dirtyVersion: 1},
		{name: "matching dirty is rejected", committedVersion: 2, dirtyVersion: 2},
		{name: "newer dirty supersedes", committedVersion: 1, dirtyVersion: 2, wantDirty: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)
			committedNext := now.Add(24 * time.Hour)
			dirtyNext := now.Add(time.Minute)

			loop.upsertAfterRefresh("version-ordering", committedNext, test.committedVersion)
			loop.upsertDirty("version-ordering", dirtyNext, test.dirtyVersion)

			want := committedNext
			if test.wantDirty {
				want = dirtyNext
			}
			item := loop.index["version-ordering"]
			if item == nil || !item.next.Equal(want) {
				t.Fatalf("scheduled refresh = %#v, want %v", item, want)
			}
		})
	}
}

func TestDirtyRescheduleSupersedesOlderResolvedStateVersion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	current := &Auth{
		ID:           "newer-dirty-state",
		Index:        "newer-dirty-state",
		StateVersion: 1,
		Provider:     "codex",
		Status:       StatusActive,
		Attributes:   map[string]string{"auth_kind": "oauth"},
	}
	resolved := current.Clone()
	resolved.Metadata = map[string]any{
		"access_token":  "resolved-access",
		"refresh_token": "resolved-refresh",
		"expires_at":    now.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), current); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)
	loop.rescheduleAfterRefresh(now, current.ID, resolved)

	currentNext := now.Add(2 * time.Minute)
	newer := current.Clone()
	newer.StateVersion = 2
	newer.NextRefreshAfter = currentNext
	if _, errUpdate := manager.Update(context.Background(), newer); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	loop.queueReschedule(current.ID)
	loop.applyDirty(now)

	item := loop.index[current.ID]
	if item == nil || !item.next.Equal(currentNext) {
		t.Fatalf("scheduled refresh = %#v, want newer index deadline %v", item, currentNext)
	}
}

func TestAutoRefreshResolverErrorKeepsPendingBackoff(t *testing.T) {
	t.Parallel()

	start := time.Now().UTC().Truncate(time.Millisecond)
	minimal := &Auth{
		ID:           "resolver-error",
		Index:        "resolver-error",
		StateVersion: 3,
		Provider:     "codex",
		Status:       StatusActive,
		Attributes:   map[string]string{"auth_kind": "oauth"},
	}
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), minimal); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	resolver := &autoRefreshTestResolver{err: errors.New("transient lookup failure")}
	manager.SetFullAuthResolver(resolver)
	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)

	loop.handleDueAuth(context.Background(), start, minimal.ID)
	authID := <-loop.jobs
	resolved := manager.refreshAuth(context.Background(), authID)
	loop.rescheduleAfterRefresh(start.Add(time.Second), authID, resolved)

	if resolver.calls != 1 {
		t.Fatalf("full auth resolver calls = %d, want 1", resolver.calls)
	}
	want := start.Add(refreshPendingBackoff)
	item := loop.index[minimal.ID]
	if item == nil || !item.next.Equal(want) {
		t.Fatalf("scheduled refresh = %#v, want pending backoff %v", item, want)
	}
	if due := loop.popDue(start.Add(time.Second)); len(due) != 0 {
		t.Fatalf("due auths during pending backoff = %v, want none", due)
	}
}
