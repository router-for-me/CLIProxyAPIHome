package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCodexRefreshIgnoresLegacyPendingJournal(t *testing.T) {
	ctx := context.Background()
	repo := newRefreshTestRepository(t)
	grant := t.Name()
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(codexauth.ClientID+"\x00"+grant)))
	auth := &coreauth.Auth{ID: grant, Index: grant, Provider: "codex", Status: coreauth.StatusError,
		Unavailable: true, RuntimeRefreshBlocked: true,
		LastError:        &coreauth.Error{Code: coreauth.ErrorCodeRefreshOutcomeUnknown, Retryable: false},
		LastRefreshError: &coreauth.Error{Code: coreauth.ErrorCodeRefreshOutcomeUnknown, Retryable: false},
		NextRefreshAfter: time.Now().Add(-time.Minute),
		Metadata: map[string]any{
			"access_token": "previous", "refresh_token": grant,
			codexauth.RefreshOutcomeMetadataKey: map[string]any{"outer": map[string]any{"grant_sha256": key, "stage": "pending"}},
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "register"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if errCreate := repo.db.Create(&CodexRefreshRecord{Grant: key, Owner: "old-owner", Receipt: JSONB(`{"stage":"pending"}`)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	transport := refreshRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if errParse := req.ParseForm(); errParse != nil || req.Form.Get("refresh_token") != grant {
			t.Fatal("refresh did not reuse the current grant")
		}
		return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"recovered","expires_in":3600}`))}, nil
	})
	http.DefaultTransport = transport
	runtime := newRefreshTestRuntime(t, repo, auth, transport)
	controller := NewRefreshController(nil, runtime, repo, nil)
	if _, errRefresh := controller.refreshLocalWithLock(ctx, auth.ID, ""); errRefresh != nil {
		t.Fatalf("legacy journal blocked recovery: %v", errRefresh)
	}
	stored, _, errGet := repo.GetAuth(ctx, auth.ID)
	if errGet != nil || calls != 1 || stored.Disabled || stored.Unavailable || stored.RuntimeRefreshBlocked || stored.LastRefreshError != nil || stored.Metadata["access_token"] != "recovered" || stored.Metadata["refresh_token"] != grant {
		t.Fatalf("refresh did not recover: error=%v calls=%d", errGet, calls)
	}
}

func TestCodexLegacyJournalSnapshotCompatibility(t *testing.T) {
	ctx := context.Background()
	for _, version := range []int{16, currentDatabaseVersion} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			repo := newRefreshTestRepository(t)
			const key = "legacy-grant"
			receipt := JSONB(`{"stage":"pending"}`)
			if errCreate := repo.db.Create(&CodexRefreshRecord{Grant: key, Owner: "old-owner", Receipt: receipt}).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			path := filepath.Join(t.TempDir(), "snapshot.zip")
			if _, errExport := exportDatabaseSnapshotVersion(ctx, repo.db, path, version); errExport != nil {
				t.Fatal(errExport)
			}
			snapshot, errOpen := OpenDatabaseSnapshot(ctx, path)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			t.Cleanup(func() {
				if errClose := snapshot.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			target := newRefreshTestRepository(t)
			if _, errImport := ImportDatabaseSnapshot(ctx, target.db, snapshot, nil); errImport != nil {
				t.Fatal(errImport)
			}
			var records []CodexRefreshRecord
			if errFind := target.db.Where("grant_sha256 = ?", key).Find(&records).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if version == 16 {
				if len(records) != 0 {
					t.Fatal("v16 snapshot unexpectedly included journal history")
				}
				return
			}
			var before, after any
			if len(records) != 1 || json.Unmarshal([]byte(receipt), &before) != nil || json.Unmarshal([]byte(records[0].Receipt), &after) != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("v17 snapshot lost legacy journal data")
			}
		})
	}
}
