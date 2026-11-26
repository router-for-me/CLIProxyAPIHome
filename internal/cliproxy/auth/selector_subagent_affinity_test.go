package auth

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSessionAffinitySubagents(t *testing.T) {
	enabled, disabled := true, false
	cases := []struct {
		name          string
		opts          Options
		alwaysInherit bool
	}{
		{name: "generic child", opts: Options{Headers: http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}}}},
		{name: "codex child", opts: Options{Headers: http.Header{"Session_id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}}}},
		{name: "guardian child", opts: Options{Headers: http.Header{"Session_id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}, "X-Openai-Subagent": {"guardian"}}}},
		{name: "claude agent", opts: Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"parent"}, "X-Claude-Code-Agent-Id": {"worker"}}}},
		{name: "pi child", opts: Options{Headers: http.Header{"X-Session-Id": {"slot:child"}, "X-Parent-Session-Id": {"slot:parent"}}}},
		{name: "nested child", opts: Options{OriginalRequest: []byte(`{"request":{"session_id":"child","parent_session_id":"parent"}}`)}},
		{name: "fork marker", alwaysInherit: true, opts: Options{Headers: http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}, "x-node-kind": {" FoRk "}}}},
		{name: "compaction marker", alwaysInherit: true, opts: Options{Headers: http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}, "X-Node-Kind": {"compaction"}}}},
		{name: "body fork", alwaysInherit: true, opts: Options{OriginalRequest: []byte(`{"session_id":"child","forked_from_thread_id":"parent"}`)}},
		{name: "nested body fork", alwaysInherit: true, opts: Options{OriginalRequest: []byte(`{"request":{"session_id":"child","parent_session_id":"parent","metadata":{"forked_from_thread_id":"parent"}}}`)}},
		{name: "codex turn fork", alwaysInherit: true, opts: Options{Headers: http.Header{"Session-Id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}, "X-Codex-Turn-Metadata": {`{"forked_from_thread_id":"parent"}`}}}},
		{name: "large codex turn fork", alwaysInherit: true, opts: Options{Headers: http.Header{"Session-Id": {"child"}, "X-Codex-Parent-Thread-Id": {"parent"}, "x-codex-turn-metadata": {`{"forked_from_thread_id":"parent","extra":"` + strings.Repeat("x", 300) + `"}`}}}},
		{name: "body compaction", alwaysInherit: true, opts: Options{OriginalRequest: []byte(`{"session_id":"child","parent_session_id":"parent","node_kind":"compaction"}`)}},
		{name: "body fork flag", alwaysInherit: true, opts: Options{OriginalRequest: []byte(`{"session_id":"child","parent_session_id":"parent","is_fork":true}`)}},
		{name: "false fork flag", opts: Options{OriginalRequest: []byte(`{"session_id":"child","parent_session_id":"parent","is_fork":false,"forked_from_thread_id":""}`)}},
		{name: "fork metadata", alwaysInherit: true, opts: Options{Headers: http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}}, Metadata: map[string]any{"is_fork": true}}},
		{name: "lcp lineage", alwaysInherit: true, opts: Options{Headers: http.Header{"X-Session-Id": {"lcp:v1:child"}, "X-Parent-Session-Id": {"lcp:v1:parent"}}}},
		{name: "message hash alias", alwaysInherit: true, opts: Options{OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"}]}`)}},
		{name: "CPA message hash alias", alwaysInherit: true, opts: Options{Headers: http.Header{"X-Session-Id": {"msg:full-hash"}, "X-Parent-Session-Id": {"msg:short-hash"}}}},
	}
	for _, tc := range cases {
		for _, setting := range []struct {
			name  string
			value *bool
		}{{"default", nil}, {"enabled", &enabled}, {"disabled", &disabled}} {
			for _, strategy := range []string{"fill-first", "round-robin", "weighted-round-robin"} {
				t.Run(tc.name+"/"+setting.name+"/"+strategy, func(t *testing.T) {
					var fallback Selector = &FillFirstSelector{}
					switch strategy {
					case "round-robin":
						fallback = &RoundRobinSelector{}
					case "weighted-round-robin":
						fallback = &WeightedRoundRobinSelector{}
					}
					store := NewSessionCache(time.Hour)
					t.Cleanup(store.Stop)
					selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, SubagentAffinity: setting.value})
					primary, parent := extractSessionIDs(tc.opts.Headers, tc.opts.OriginalRequest, tc.opts.Metadata)
					if primary == "" || parent == "" || primary == parent {
						t.Fatalf("invalid fixture identities: %q %q", primary, parent)
					}
					parentKey := sessionBindingKey("openai", parent, "model")
					store.Set(parentKey, "b")
					auths := []*Auth{{ID: "a", Status: StatusActive}, {ID: "b", Status: StatusActive}}
					want := "b"
					if setting.value != nil && !*setting.value && !tc.alwaysInherit {
						want = "a"
					}
					for range 2 {
						picked, errPick := selector.Pick(t.Context(), "openai", "model", tc.opts, auths)
						if errPick != nil || picked == nil || picked.ID != want {
							t.Fatalf("Pick()=%v err=%v, want %s", picked, errPick, want)
						}
					}
					parentBinding, errLoad := store.LoadSessionBinding(t.Context(), parentKey)
					if errLoad != nil || parentBinding.AuthID != "b" {
						t.Fatalf("child changed parent: %+v err=%v", parentBinding, errLoad)
					}
				})
			}
		}
	}
}

func TestSessionAffinitySubagentsExistingChildAndExpiry(t *testing.T) {
	disabled := false
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: &FillFirstSelector{}, SubagentAffinity: &disabled})
	childKey := sessionBindingKey("openai", "header:child", "model")
	store.Set(childKey, "b")
	store.Set(sessionBindingKey("openai", "header:parent", "model"), "b")
	opts := Options{Headers: http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}}}
	auths := []*Auth{{ID: "a", Status: StatusActive}, {ID: "b", Status: StatusActive}}
	picked, errPick := selector.Pick(t.Context(), "openai", "model", opts, auths)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("existing child lost binding: %v err=%v", picked, errPick)
	}
	store.mu.Lock()
	entry := store.entries[childKey]
	entry.expiresAt = time.Now().Add(-time.Hour)
	store.entries[childKey] = entry
	store.mu.Unlock()
	picked, errPick = selector.Pick(t.Context(), "openai", "model", opts, auths)
	if errPick != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("expired child inherited parent: %v err=%v", picked, errPick)
	}
}
