package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestSessionAffinityGuardianParentCredential(t *testing.T) {
	for _, strategy := range []string{"fill-first", "round-robin", "weighted-round-robin"} {
		for _, tc := range []struct {
			name              string
			provider          string
			parentProvider    string
			subagent          string
			disabled          bool
			existingChild     bool
			olderReview       bool
			parentUnavailable bool
			parentExcluded    bool
			parentLowPriority bool
			fork              bool
			noParent          bool
			want              string
		}{
			{name: "cross model", subagent: "guardian", want: "b"},
			{name: "custom model case insensitive header", subagent: "  GuArDiAn ", want: "b"},
			{name: "latest parent beats older review model", subagent: "guardian", olderReview: true, want: "b"},
			{name: "existing child wins", subagent: "guardian", existingChild: true, want: "a"},
			{name: "disabled", subagent: "guardian", disabled: true, want: "a"},
			{name: "ordinary subagent stays model scoped", subagent: "collab_spawn", want: "a"},
			{name: "review model alone is not guardian", want: "a"},
			{name: "no parent", subagent: "guardian", noParent: true, want: "a"},
			{name: "disabled parent", subagent: "guardian", parentUnavailable: true, want: "a"},
			{name: "parent outside candidates", subagent: "guardian", parentExcluded: true, want: "a"},
			{name: "parent lower priority remains eligible", subagent: "guardian", parentLowPriority: true, want: "b"},
			{name: "fork does not use cross model guardian", subagent: "guardian", fork: true, want: "a"},
			{name: "mixed parent direct child", provider: "codex", parentProvider: "mixed", subagent: "guardian", want: "b"},
			{name: "direct parent mixed child", provider: "mixed", parentProvider: "codex", subagent: "guardian", want: "b"},
		} {
			t.Run(strategy+"/"+tc.name, func(t *testing.T) {
				store := NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				enabled := !tc.disabled
				var fallback Selector = &FillFirstSelector{}
				switch strategy {
				case "round-robin":
					fallback = &RoundRobinSelector{}
				case "weighted-round-robin":
					fallback = &WeightedRoundRobinSelector{}
				}
				parentSelector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: &FillFirstSelector{}})
				childSelector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, SubagentAffinity: &enabled})
				a := &Auth{ID: "a", Provider: "codex", Status: StatusActive}
				b := &Auth{ID: "b", Provider: "codex", Status: StatusActive}
				provider, parentProvider := tc.provider, tc.parentProvider
				if provider == "" {
					provider = "codex"
				}
				if parentProvider == "" {
					parentProvider = "codex"
				}
				parentOpts := Options{Headers: http.Header{"Session_id": {"parent"}}}
				pick := func(selector *SessionAffinitySelector, provider, model string, opts Options, candidates []*Auth, want string) {
					t.Helper()
					picked, errPick := selector.Pick(t.Context(), provider, model, opts, candidates)
					if errPick != nil || picked == nil || picked.ID != want {
						t.Fatalf("provider=%s model=%s pick=%v err=%v, want %s", provider, model, picked, errPick, want)
					}
				}
				if tc.olderReview {
					pick(parentSelector, parentProvider, "custom-review", parentOpts, []*Auth{a}, "a")
				}
				childOpts := Options{Headers: http.Header{"Session_id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}, "x-openai-subagent": {tc.subagent}}}
				if tc.existingChild {
					pick(childSelector, provider, "custom-review", childOpts, []*Auth{a}, "a")
				}
				pick(parentSelector, parentProvider, "main-model", parentOpts, []*Auth{b}, "b")
				b.Disabled = tc.parentUnavailable
				if tc.parentLowPriority {
					a.Attributes = map[string]string{"priority": "10"}
				}
				if tc.fork {
					childOpts.Headers.Set("X-Node-Kind", "fork")
				}
				if tc.noParent {
					childOpts.Headers.Del("X-Codex-Parent-Thread-Id")
				}
				candidates := []*Auth{a, b}
				if tc.parentExcluded {
					candidates = []*Auth{a}
				}
				pick(childSelector, provider, "custom-review", childOpts, candidates, tc.want)
				pick(childSelector, provider, "custom-review", childOpts, candidates, tc.want)
				if tc.parentLowPriority {
					childOpts.Headers.Del("X-Codex-Parent-Thread-Id")
					pick(childSelector, provider, "custom-review", childOpts, candidates, tc.want)
				}
				parent, errLoad := store.LoadSessionBinding(t.Context(), sessionBindingKey(parentProvider, "codex:parent", "main-model"))
				if errLoad != nil || parent.AuthID != "b" {
					t.Fatalf("child modified parent: %+v err=%v", parent, errLoad)
				}
			})
		}
	}
}

func TestSessionAffinityGuardianProviderIsolationAndExpiry(t *testing.T) {
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: &FillFirstSelector{}})
	parent := Options{Headers: http.Header{"Session_id": {"parent"}}}
	a := &Auth{ID: "a", Provider: "codex", Status: StatusActive}
	b := &Auth{ID: "b", Provider: "codex", Status: StatusActive}
	c := &Auth{ID: "c", Provider: "openai", Status: StatusActive}
	for _, step := range []struct {
		model string
		auth  *Auth
	}{{"old-model", a}, {"main-model", b}, {"openai-model", c}} {
		if _, errPick := selector.Pick(t.Context(), "mixed", step.model, parent, []*Auth{step.auth}); errPick != nil {
			t.Fatal(errPick)
		}
	}
	child := Options{Headers: http.Header{"Session_id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}, "X-Openai-Subagent": {"guardian"}}}
	picked, errPick := selector.Pick(t.Context(), "codex", "review", child, []*Auth{a, b})
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("crossed real providers: %v err=%v", picked, errPick)
	}
	store.mu.Lock()
	key := sessionBindingKey("mixed", "codex:parent", "main-model")
	entry := store.entries[key]
	entry.expiresAt = time.Now().Add(-time.Hour)
	store.entries[key] = entry
	store.mu.Unlock()
	latest, errLatest := store.LoadLatestSessionBinding(t.Context(), sessionBindingKey("", "codex:parent", ""), []string{"codex"})
	if errLatest != nil || latest.AuthID != "a" {
		t.Fatalf("expired parent was inherited: %+v err=%v", latest, errLatest)
	}
}

type guardianFailingStore struct {
	SessionAffinityStore
	failure error
}

func (s *guardianFailingStore) LoadLatestSessionBinding(context.Context, string, []string) (SessionBinding, error) {
	return SessionBinding{}, s.failure
}

func TestSessionAffinityGuardianStoreFailure(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	t.Cleanup(cache.Stop)
	failure := errors.New("shared parent lookup unavailable")
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: &guardianFailingStore{SessionAffinityStore: cache, failure: failure}})
	opts := Options{Headers: http.Header{"Session_id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}, "X-Openai-Subagent": {"guardian"}}}
	picked, errPick := selector.Pick(t.Context(), "codex", "review", opts, []*Auth{{ID: "a", Provider: "codex"}})
	if picked != nil || !errors.Is(errPick, failure) {
		t.Fatalf("lookup failure fell back locally: %v err=%v", picked, errPick)
	}
}

func TestSessionAffinityGuardianDeferredSelectionOrder(t *testing.T) {
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: &FillFirstSelector{}})
	opts := Options{Headers: http.Header{"Session_id": {"parent"}}, SessionAffinity: &SessionAffinitySelection{}}
	if _, errPick := selector.Pick(t.Context(), "codex", "main-model", opts, []*Auth{{ID: "b", Provider: "codex"}}); errPick != nil {
		t.Fatal(errPick)
	}
	key := sessionBindingKey("", "codex:parent", "")
	if latest, errLatest := store.LoadLatestSessionBinding(t.Context(), key, []string{"codex"}); errLatest != nil || latest.AuthID != "" {
		t.Fatalf("unadmitted selection visible: %+v err=%v", latest, errLatest)
	}
	if swapped, errSwap := store.CompareAndSwapSessionBinding(t.Context(), *opts.SessionAffinity.Commit); errSwap != nil || !swapped {
		t.Fatalf("commit=%t err=%v", swapped, errSwap)
	}
	if latest, errLatest := store.LoadLatestSessionBinding(t.Context(), key, []string{"codex"}); errLatest != nil || latest.AuthID != "b" {
		t.Fatalf("admitted selection missing: %+v err=%v", latest, errLatest)
	}
}
