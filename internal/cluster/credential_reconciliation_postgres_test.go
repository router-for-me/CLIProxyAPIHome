package cluster

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/gorm"
)

type blockingCredentialReferenceChecker struct {
	checked chan<- struct{}
	release <-chan struct{}
}

func (c blockingCredentialReferenceChecker) HasCredentialReferences(ctx context.Context, _ *gorm.DB, _ string) (bool, error) {
	select {
	case c.checked <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-c.release:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestConfigReplacementAndPolicyPatchDoNotDeadlockPostgres(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		replace func(context.Context, *Repository, map[string]any) error
	}{
		{
			name: "config replace",
			replace: func(ctx context.Context, repo *Repository, root map[string]any) error {
				return repo.ReplaceConfigSnapshot(ctx, root)
			},
		},
		{
			name: "plugin task config sync",
			replace: func(ctx context.Context, repo *Repository, root map[string]any) error {
				_, errReplace := repo.ReplaceConfigSnapshotAndCreatePluginTask(ctx, root, node.PluginTask{Operation: node.PluginTaskOperationDelete, PluginID: "plugin-id"})
				return errReplace
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCtx()
			credentialID := "73737373-7373-4737-8737-737373737373"
			root := map[string]any{
				"gemini-api-key": []any{map[string]any{"id": credentialID, "api-key": "key"}},
				credentialConcurrencyPoliciesRootKey: map[string]any{
					credentialID: map[string]any{"max-in-flight": 2},
				},
			}
			if errReplace := repo.ReplaceConfigSnapshot(ctx, root); errReplace != nil {
				t.Fatal(errReplace)
			}

			start := make(chan struct{})
			patchDone := make(chan error, 1)
			replaceDone := make(chan error, 1)
			go func() {
				<-start
				_, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, credentialID, ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 3}}, nil)
				patchDone <- errPatch
			}()
			go func() {
				<-start
				replaceDone <- testCase.replace(ctx, repo, root)
			}()
			close(start)
			for _, done := range []<-chan error{patchDone, replaceDone} {
				select {
				case errDone := <-done:
					if errDone != nil {
						t.Fatal(errDone)
					}
				case <-ctx.Done():
					t.Fatal("concurrent policy patch and config replacement did not complete")
				}
			}
		})
	}
}

func TestReconcileProviderAuthsSerializesPolicyCreationPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()

	credentialID := "73737373-7373-4737-8737-737373737373"
	auth := &coreauth.Auth{
		ID:       credentialID,
		Index:    credentialID,
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[retire]",
			"api_key": "key",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}

	checked := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
	})
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- repo.ReconcileProviderAuths(ctx, "gemini-api-key", nil, blockingCredentialReferenceChecker{checked: checked, release: release})
	}()
	select {
	case <-checked:
	case <-ctx.Done():
		t.Fatal("reconciliation did not reach the reference checker")
	}

	patchDone := make(chan error, 1)
	go func() {
		_, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, credentialID, ConcurrencyPolicyPatch{
			MaxInFlight: OptionalLimit{Set: true, Value: 1},
		}, nil)
		patchDone <- errPatch
	}()
	select {
	case errPatch := <-patchDone:
		t.Fatalf("policy creation completed before reconciliation retired the credential: %v", errPatch)
	case <-time.After(100 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case errReconcile := <-reconcileDone:
		if errReconcile != nil {
			t.Fatalf("reconciliation error = %v", errReconcile)
		}
	case <-ctx.Done():
		t.Fatal("reconciliation did not complete")
	}
	select {
	case errPatch := <-patchDone:
		if !errors.Is(errPatch, ErrConcurrencyCredentialNotFound) {
			t.Fatalf("policy creation error = %v, want ErrConcurrencyCredentialNotFound", errPatch)
		}
	case <-ctx.Done():
		t.Fatal("policy creation did not complete")
	}

	var active int64
	if errCount := repo.db.Model(&AuthRecord{}).Where("uuid = ?", credentialID).Count(&active).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if active != 0 {
		t.Fatalf("active credential count = %d, want 0", active)
	}
	var policies int64
	if errCount := repo.db.Model(&CredentialConcurrencyPolicyRecord{}).Where("credential_id = ?", credentialID).Count(&policies).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if policies != 0 {
		t.Fatalf("policy count = %d, want 0", policies)
	}
}
