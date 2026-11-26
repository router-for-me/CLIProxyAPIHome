package home

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestAntigravityModelEnumProbeCompatibility(t *testing.T) {
	if _, ok := parseAntigravityModelCapabilityHints([]byte(`{"models":{},"webSearchModelIds":[null]}`)); ok {
		t.Error("null web search model ID was accepted")
	}
	hints, ok := parseAntigravityModelCapabilityHints([]byte(`{"models":{"id":{"MODEL":"WRONG"},"gemini-2.5-pro":{"model":"MODEL_GOOGLE_GEMINI_2_5_PRO"}}}`))
	if !ok || hints.modelEnums["id"] != "" || hints.modelEnums["gemini-2.5-pro"] != "MODEL_GOOGLE_GEMINI_2_5_PRO" {
		t.Error("model enum parsing did not respect the exact field name")
	}
	request := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request <- string(body)
		_, _ = fmt.Fprint(w, `{"models":{}}`)
	}))
	t.Cleanup(srv.Close)
	rt := &Runtime{}
	_, status := rt.probeAntigravityModelCapabilityHints(t.Context(), &coreauth.Auth{}, []string{srv.URL}, "", "access", "test-client")
	if status != antigravityProbeStatusSuccess || <-request != `{"project":""}` {
		t.Fatal("probe must send an explicit empty project to match the pinned SDK")
	}
}

func TestAntigravityModelEnumScopeDoesNotMutateEmptyAttributes(t *testing.T) {
	auth := &coreauth.Auth{ID: "scope", Provider: "antigravity", Attributes: make(map[string]string), Metadata: map[string]any{"refresh_token": "grant"}}
	dispatch, errSanitize := SanitizeAuthForDownstream(auth, nil)
	if errSanitize != nil || dispatch.Attributes[antigravityModelEnumScopeAttribute] == "" || len(auth.Attributes) != 0 {
		t.Fatalf("scope projection changed the source attributes: %v", errSanitize)
	}
}

func TestAntigravityModelEnumsRejectOlderCrossNodeProbe(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := "MODEL_NEW"
		if r.Header.Get("Authorization") == "Bearer old" {
			close(oldStarted)
			<-releaseOld
			value = "MODEL_OLD"
		}
		_, _ = fmt.Fprintf(w, `{"models":{"gemini-2.5-pro":{"model":%q}}}`, value)
	}))
	t.Cleanup(srv.Close)
	old := &coreauth.Auth{ID: "cluster-probe", Provider: "antigravity", Metadata: map[string]any{"access_token": "old", "refresh_token": "same-grant", "base_url": srv.URL}}
	newer := old.Clone()
	newer.Metadata["access_token"] = "new"
	adapter := &antigravityEnumTestAdapter{fakeAntigravityClusterAdapter: &fakeAntigravityClusterAdapter{}, kvStore: &runtimeKVTestAdapter{}}
	// The two route cache keys simulate distinct in-process probe caches on Home nodes;
	// the credential scope and shared storage are the same.
	nodeA := &Runtime{cfg: &config.Config{}, clusterAdapter: adapter}
	nodeB := &Runtime{cfg: &config.Config{}, clusterAdapter: adapter}
	newer.ProxyURL = "direct"
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		nodeA.fetchAntigravityModelCapabilityHints(t.Context(), old)
	}()
	<-oldStarted
	nodeB.fetchAntigravityModelCapabilityHints(t.Context(), newer)
	close(releaseOld)
	<-oldDone
	if !strings.Contains(string(adapter.raw), "MODEL_NEW") || adapter.writes != 1 {
		t.Fatalf("older probe overwrote the newer catalog: %s (%d writes)", adapter.raw, adapter.writes)
	}
}

func TestAntigravityModelEnumCPADepProbeKeepsHTTP11(t *testing.T) {
	if !antigravityProbeHTTP11 {
		t.Skip("the default build does not force HTTP/1.1")
	}
	protocol := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol <- r.Proto + "/" + r.TLS.NegotiatedProtocol
		_, _ = fmt.Fprint(w, `{"models":{}}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	rt := &Runtime{}
	_, status := rt.probeAntigravityModelCapabilityHints(t.Context(), &coreauth.Auth{}, []string{srv.URL}, "", "access", "test-client")
	if status != antigravityProbeStatusSuccess {
		t.Fatalf("HTTP/1.1 probe with an HTTP/2-capable base transport failed: %v", status)
	}
	if got := <-protocol; got != "HTTP/1.1/" {
		t.Fatalf("probe transport = %q, want HTTP/1.1 without ALPN", got)
	}
}
