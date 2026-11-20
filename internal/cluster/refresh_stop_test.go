package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// stoppableRefreshRoundTripper answers once released and loses its answer when the request's
// context ended first, as a real provider call does after the provider already rotated the token.
type stoppableRefreshRoundTripper struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (rt *stoppableRefreshRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.once.Do(func() { close(rt.started) })
	select {
	case <-rt.release:
	case <-req.Context().Done():
	}
	if errCtx := req.Context().Err(); errCtx != nil {
		return nil, errCtx
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"rotated-access-token","refresh_token":"rotated-refresh-token","expires_in":3600,"token_type":"Bearer"}`)),
	}, nil
}

func TestAClaimedRefreshWritesBackAfterItsCallerIsCancelled(t *testing.T) {
	const authID = "antigravity-stopping"
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	transport := &stoppableRefreshRoundTripper{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9311, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, errRefresh := controller.RefreshNow(ctx, authID)
		result <- errRefresh
	}()
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider refresh did not start")
	}
	// Home stops while the provider call is out: the caller's context ends before the answer.
	cancel()
	close(transport.release)
	select {
	case errRefresh := <-result:
		if errRefresh != nil {
			t.Fatalf("RefreshNow() error = %v, want the claimed refresh to finish", errRefresh)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claimed refresh did not finish")
	}
	persisted, _, errAuth := repo.GetAuth(context.Background(), authID)
	if errAuth != nil {
		t.Fatalf("GetAuth() error = %v", errAuth)
	}
	if got := persisted.Metadata["refresh_token"]; got != "rotated-refresh-token" {
		t.Fatalf("persisted refresh token = %v, want the provider's rotated one", got)
	}
}

func TestARejectedRefreshNamesTheCredentialOnlyByAnOpaqueReference(t *testing.T) {
	const authID = "antigravity-person@example.com"
	var logs bytes.Buffer
	previous := log.StandardLogger().Out
	previousFormatter := log.StandardLogger().Formatter
	log.SetOutput(&logs)
	log.SetFormatter(&log.JSONFormatter{})
	t.Cleanup(func() { log.SetOutput(previous); log.SetFormatter(previousFormatter) })

	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	auth := newInvalidGrantRefreshAuth(authID)
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	transport := &refreshTestRoundTripper{body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`}
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 9312, Secret: "master-secret"}, CoordinatorOptions{})
	markRefreshTestMaster(t, repo, coordinator)
	controller := NewRefreshController(coordinator, runtime, repo, nil)
	_, errRefresh := controller.RefreshNow(ctx, authID)
	requireTerminalRefreshError(t, errRefresh)

	digest := sha256.Sum256([]byte(authID))
	want := "credential refresh rejected credential=sha256:" + hex.EncodeToString(digest[:]) + " reason=invalid_grant"
	found := false
	for _, line := range bytes.Split(logs.Bytes(), []byte("\n")) {
		var entry struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(line, &entry) != nil || !strings.HasPrefix(entry.Msg, "credential refresh rejected") {
			continue
		}
		if entry.Msg != want {
			t.Fatalf("rejection line = %q, want %q", entry.Msg, want)
		}
		found = true
	}
	if !found {
		t.Fatalf("no rejection line in the refresh log, want %q", want)
	}
}
