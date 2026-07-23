package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestConcurrencyPolicyExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, source, "uuid-provider-1")
	if _, errPatch := source.PatchCredentialConcurrencyPolicy(ctx, "uuid-provider-1", ConcurrencyPolicyPatch{
		MaxInFlight: OptionalLimit{Set: true, Value: 4},
		MaxInFlightByModel: OptionalModelLimitMap{Set: true, Value: map[string]*int64{
			"gpt(high)": int64Pointer(2),
		}},
	}, nil); errPatch != nil {
		t.Fatal(errPatch)
	}

	root := map[string]any{}
	if errExport := source.ExportCredentialConcurrencyPolicies(ctx, root); errExport != nil {
		t.Fatalf("ExportCredentialConcurrencyPolicies() error = %v", errExport)
	}

	target := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, target, "uuid-provider-1")
	if errImport := target.ImportCredentialConcurrencyPolicies(ctx, root); errImport != nil {
		t.Fatalf("ImportCredentialConcurrencyPolicies() error = %v", errImport)
	}
	policy, errPolicy := target.GetCredentialConcurrencyPolicy(ctx, "uuid-provider-1")
	if errPolicy != nil {
		t.Fatal(errPolicy)
	}
	if policy.MaxInFlight == nil || *policy.MaxInFlight != 4 || policy.MaxInFlightByModel["gpt"] != 2 {
		t.Fatalf("policy = %#v", policy)
	}
}

func TestExportLocalStateWritesEmptyConcurrencyPolicySection(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	outputDir := t.TempDir()
	if _, errExport := ExportLocalState(context.Background(), ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	data, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	root := map[string]any{}
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	policies, exists := root[credentialConcurrencyPoliciesRootKey].(map[string]any)
	if !exists || len(policies) != 0 {
		t.Fatalf("%s = %#v, want empty map", credentialConcurrencyPoliciesRootKey, root[credentialConcurrencyPoliciesRootKey])
	}
}

func TestExportLocalStateExportsPoliciesOnlyForSerializedCredentials(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	configID := "26262626-2626-4262-8262-262626262626"
	runtimeID := "27272727-2727-4272-8272-272727272727"
	for _, auth := range []*coreauth.Auth{
		{ID: configID, Index: configID, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[export]", "api_key": "key"}},
		{ID: runtimeID, Index: runtimeID, Provider: "codex", Metadata: map[string]any{"type": "codex"}, Attributes: map[string]string{"runtime_only": "true"}},
	} {
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
	}
	setExchangePolicy(t, repo, configID, 2, nil)
	setExchangePolicy(t, repo, runtimeID, 3, nil)

	outputDir := t.TempDir()
	if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	data, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	root := map[string]any{}
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	policies, okPolicies := root[credentialConcurrencyPoliciesRootKey].(map[string]any)
	if !okPolicies || len(policies) != 1 || policies[configID] == nil || policies[runtimeID] != nil {
		t.Fatalf("%s = %#v, want only serialized credential %s", credentialConcurrencyPoliciesRootKey, root[credentialConcurrencyPoliciesRootKey], configID)
	}
}

func TestExportLocalStateRejectsUnresolvedConcurrencyPolicy(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	if errCreate := repo.db.Create(&CredentialConcurrencyPolicyRecord{CredentialID: "missing-credential", MaxInFlight: int64Pointer(2)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	_, errExport := ExportLocalState(context.Background(), ExportOptions{Repository: repo, OutputDir: t.TempDir(), AuthDirName: "auth"})
	if errExport == nil {
		t.Fatal("ExportLocalState() succeeded with an unresolved concurrency policy")
	}
}

func TestFullExportImportExcludesRetiredConcurrencyPolicies(t *testing.T) {
	ctx := context.Background()
	source := newCredentialFoundationTestRepository(t)
	activeID := "31313131-3131-4131-8131-313131313131"
	retiredID := "32323232-3232-4232-8232-323232323232"
	active := &coreauth.Auth{ID: activeID, Index: activeID, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[active]", "api_key": "active-key"}}
	if _, errUpsert := source.UpsertAuth(ctx, active, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	setExchangePolicy(t, source, activeID, 2, nil)
	seedRetiredProviderConcurrencyPolicy(t, source, retiredID)

	outputDir := t.TempDir()
	if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: source, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	data, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	root := map[string]any{}
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	policies, okPolicies := root[credentialConcurrencyPoliciesRootKey].(map[string]any)
	if !okPolicies || len(policies) != 1 || policies[activeID] == nil || policies[retiredID] != nil {
		t.Fatalf("%s = %#v, want only active serialized credential %s", credentialConcurrencyPoliciesRootKey, root[credentialConcurrencyPoliciesRootKey], activeID)
	}

	target := newCredentialFoundationTestRepository(t)
	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: filepath.Join(outputDir, "config.yaml"), Repository: target}); errImport != nil {
		t.Fatal(errImport)
	}
	assertExchangePolicy(t, target, activeID, 2, nil)
	var retiredPolicyCount int64
	if errCount := target.db.Model(&CredentialConcurrencyPolicyRecord{}).Where("credential_id = ?", retiredID).Count(&retiredPolicyCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if retiredPolicyCount != 0 {
		t.Fatalf("retired policy count = %d, want 0", retiredPolicyCount)
	}
}

func TestExportLocalStateConcurrentPolicyMutationKeepsSerializedPoliciesResolvable(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "28282828-2828-4282-8282-282828282828"
	auth := &coreauth.Auth{ID: id, Index: id, Provider: "gemini", Attributes: map[string]string{"source": "config:gemini[export]", "api_key": "key"}}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	setExchangePolicy(t, repo, id, 2, nil)

	done := make(chan error, 1)
	go func() {
		for index := 0; index < 20; index++ {
			limit := int64(2 + index%2)
			_, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, id, ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: limit}}, nil)
			if errPatch != nil {
				done <- errPatch
				return
			}
		}
		done <- nil
	}()
	for index := 0; index < 10; index++ {
		outputDir := t.TempDir()
		if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
			t.Fatal(errExport)
		}
		data, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
		if errRead != nil {
			t.Fatal(errRead)
		}
		root := map[string]any{}
		if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		policies, okPolicies := root[credentialConcurrencyPoliciesRootKey].(map[string]any)
		if !okPolicies || len(policies) != 1 || policies[id] == nil {
			t.Fatalf("%s = %#v, want policy for exported credential %s", credentialConcurrencyPoliciesRootKey, root[credentialConcurrencyPoliciesRootKey], id)
		}
	}
	if errMutation := <-done; errMutation != nil {
		t.Fatal(errMutation)
	}
}

func TestConcurrencyCredentialReferenceCheckerIncludesCounters(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	if errCreate := repo.db.Create(&CredentialConcurrencyCounterRecord{CredentialID: "cred-1", Model: "gpt", CertificateFingerprint: "fingerprint", UpdatedAt: databaseTestTime}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	referenced, errReferences := (ConcurrencyCredentialReferenceChecker{}).HasCredentialReferences(ctx, repo.db, "cred-1")
	if errReferences != nil {
		t.Fatal(errReferences)
	}
	if !referenced {
		t.Fatal("counter-only credential was not treated as referenced")
	}
}

func TestImportPreservesPolicyWhenProviderUUIDIsStable(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "22222222-2222-4222-8222-222222222222"
	auth := &coreauth.Auth{
		ID:       id,
		Index:    id,
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[managed]",
			"api_key": "original",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	setExchangePolicy(t, repo, id, 2, nil)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, configPath, "gemini-api-key:\n  - id: "+id+"\n    api-key: rotated\n")
	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo}); errImport != nil {
		t.Fatal(errImport)
	}
	assertExchangePolicy(t, repo, id, 2, nil)
}

func TestImportRejectsMissingManagedCredentialUUID(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "11111111-1111-4111-8111-111111111111"
	auth := &coreauth.Auth{
		ID:       id,
		Index:    id,
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[managed]",
			"api_key": "original",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(ctx, id, ConcurrencyPolicyPatch{MaxInFlight: OptionalLimit{Set: true, Value: 1}}, nil); errPatch != nil {
		t.Fatal(errPatch)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, configPath, "gemini-api-key:\n  - api-key: rotated\n")
	_, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo})
	if !errors.Is(errImport, ErrCredentialConcurrencyOrphan) {
		t.Fatalf("ImportLocalState() error = %v, want ErrCredentialConcurrencyOrphan", errImport)
	}
}

func TestImportPolicySectionMissingPreservesPolicies(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	setExchangePolicy(t, repo, "cred-1", 2, nil)

	if errImport := repo.ImportCredentialConcurrencyPolicies(ctx, map[string]any{}); errImport != nil {
		t.Fatal(errImport)
	}
	assertExchangePolicy(t, repo, "cred-1", 2, nil)
}

func TestImportEmptyPolicySectionClearsLimits(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	setExchangePolicy(t, repo, "cred-1", 2, map[string]int64{"gpt": 1})

	if errImport := repo.ImportCredentialConcurrencyPolicies(ctx, map[string]any{"credential-concurrency-policies": map[string]any{}}); errImport != nil {
		t.Fatal(errImport)
	}
	assertExchangePolicy(t, repo, "cred-1", 0, nil)
}

func TestImportLocalStateRetiredConcurrencyPolicies(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		config    string
		wantClear bool
		wantError bool
	}{
		{name: "missing preserves", config: "port: 8327\n"},
		{name: "empty clears", config: "credential-concurrency-policies: {}\n", wantClear: true},
		{name: "nonempty rejects", config: "credential-concurrency-policies:\n  30303030-3030-4030-8030-303030303030:\n    max-in-flight: 4\n", wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newCredentialFoundationTestRepository(t)
			id := "30303030-3030-4030-8030-303030303030"
			before := seedRetiredProviderConcurrencyPolicy(t, repo, id)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, configPath, testCase.config)

			_, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo})
			if testCase.wantError {
				if !errors.Is(errImport, ErrConcurrencyCredentialNotFound) {
					t.Fatalf("ImportLocalState() error = %v, want ErrConcurrencyCredentialNotFound", errImport)
				}
				assertRetiredProviderConcurrencyPolicy(t, repo, id, before, false)
				return
			}
			if errImport != nil {
				t.Fatal(errImport)
			}
			assertRetiredProviderConcurrencyPolicy(t, repo, id, before, testCase.wantClear)
		})
	}
}

func TestImportPolicyMaxInFlightOptionalsClearOrSetLimits(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value any
		want  int64
	}{
		{name: "omitted", value: map[string]any{}, want: 0},
		{name: "null", value: map[string]any{"max-in-flight": nil}, want: 0},
		{name: "zero", value: map[string]any{"max-in-flight": 0}, want: 0},
		{name: "positive", value: map[string]any{"max-in-flight": 4}, want: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newCredentialFoundationTestRepository(t)
			seedConcurrencyPolicyAuth(t, repo, "cred-1")
			setExchangePolicy(t, repo, "cred-1", 2, nil)
			root := map[string]any{"credential-concurrency-policies": map[string]any{"cred-1": testCase.value}}
			if errImport := repo.ImportCredentialConcurrencyPolicies(ctx, root); errImport != nil {
				t.Fatal(errImport)
			}
			assertExchangePolicy(t, repo, "cred-1", testCase.want, nil)
		})
	}
}

func TestImportPolicyReplacementIsAtomic(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	seedConcurrencyPolicyAuth(t, repo, "cred-2")
	setExchangePolicy(t, repo, "cred-1", 2, nil)
	setExchangePolicy(t, repo, "cred-2", 3, nil)

	root := map[string]any{"credential-concurrency-policies": map[string]any{
		"cred-1": map[string]any{"max-in-flight": 4},
		"cred-2": map[string]any{"max-in-flight": -1},
	}}
	if errImport := repo.ImportCredentialConcurrencyPolicies(ctx, root); !errors.Is(errImport, ErrConcurrencyInvalidLimit) {
		t.Fatalf("ImportCredentialConcurrencyPolicies() error = %v, want ErrConcurrencyInvalidLimit", errImport)
	}
	assertExchangePolicy(t, repo, "cred-1", 2, nil)
	assertExchangePolicy(t, repo, "cred-2", 3, nil)
}

func TestImportPartialPolicySectionReplacesSet(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	seedConcurrencyPolicyAuth(t, repo, "cred-1")
	seedConcurrencyPolicyAuth(t, repo, "cred-2")
	setExchangePolicy(t, repo, "cred-1", 2, nil)
	setExchangePolicy(t, repo, "cred-2", 3, nil)

	root := map[string]any{"credential-concurrency-policies": map[string]any{
		"cred-1": map[string]any{"max-in-flight": 4, "max-in-flight-by-model": map[string]any{"gpt(high)": 2}},
	}}
	if errImport := repo.ImportCredentialConcurrencyPolicies(ctx, root); errImport != nil {
		t.Fatal(errImport)
	}
	assertExchangePolicy(t, repo, "cred-1", 4, map[string]int64{"gpt": 2})
	assertExchangePolicy(t, repo, "cred-2", 0, nil)
}

func seedRetiredProviderConcurrencyPolicy(t *testing.T, repo *Repository, credentialID string) CredentialConcurrencyPolicyRecord {
	t.Helper()
	auth := &coreauth.Auth{
		ID:       credentialID,
		Index:    credentialID,
		Provider: "gemini",
		Attributes: map[string]string{
			"source":  "config:gemini[retired]",
			"api_key": "key",
		},
	}
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	setExchangePolicy(t, repo, credentialID, 2, map[string]int64{"gpt": 1})
	if errCreate := repo.db.Create(&CredentialConcurrencyCounterRecord{CredentialID: credentialID, Model: "gpt", CertificateFingerprint: "fingerprint", UpdatedAt: databaseTestTime}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	policy := CredentialConcurrencyPolicyRecord{}
	if errFind := repo.db.First(&policy, "credential_id = ?", credentialID).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if errRetire := repo.RetireProviderAuth(context.Background(), credentialID); errRetire != nil {
		t.Fatal(errRetire)
	}
	return policy
}

func assertRetiredProviderConcurrencyPolicy(t *testing.T, repo *Repository, credentialID string, before CredentialConcurrencyPolicyRecord, cleared bool) {
	t.Helper()
	auth := AuthRecord{}
	if errFind := repo.db.Unscoped().First(&auth, "uuid = ?", credentialID).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if !auth.DeletedAt.Valid {
		t.Fatal("retired credential was reactivated")
	}
	policy := CredentialConcurrencyPolicyRecord{}
	if errFind := repo.db.First(&policy, "credential_id = ?", credentialID).Error; errFind != nil {
		t.Fatalf("policy main row was removed: %v", errFind)
	}
	var modelCount int64
	if errCount := repo.db.Model(&CredentialConcurrencyModelPolicyRecord{}).Where("credential_id = ?", credentialID).Count(&modelCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	var counterCount int64
	if errCount := repo.db.Model(&CredentialConcurrencyCounterRecord{}).Where("credential_id = ?", credentialID).Count(&counterCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if counterCount != 1 {
		t.Fatalf("counter count = %d, want 1", counterCount)
	}
	if cleared {
		if policy.MaxInFlight != nil || modelCount != 0 || policy.Version != before.Version+1 {
			t.Fatalf("cleared retired policy = %#v, model count = %d, want cleared version %d", policy, modelCount, before.Version+1)
		}
		return
	}
	if policy.MaxInFlight == nil || *policy.MaxInFlight != 2 || modelCount != 1 || policy.Version != before.Version {
		t.Fatalf("preserved retired policy = %#v, model count = %d, want unchanged", policy, modelCount)
	}
}

func setExchangePolicy(t *testing.T, repo *Repository, credentialID string, maxInFlight int64, models map[string]int64) {
	t.Helper()
	modelPatch := OptionalModelLimitMap{Set: true, Value: make(map[string]*int64, len(models))}
	for model, limit := range models {
		modelPatch.Value[model] = int64Pointer(limit)
	}
	if _, errPatch := repo.PatchCredentialConcurrencyPolicy(context.Background(), credentialID, ConcurrencyPolicyPatch{
		MaxInFlight:        OptionalLimit{Set: true, Value: maxInFlight},
		MaxInFlightByModel: modelPatch,
	}, nil); errPatch != nil {
		t.Fatal(errPatch)
	}
}

func assertExchangePolicy(t *testing.T, repo *Repository, credentialID string, maxInFlight int64, models map[string]int64) {
	t.Helper()
	policy, errPolicy := repo.GetCredentialConcurrencyPolicy(context.Background(), credentialID)
	if errPolicy != nil {
		t.Fatal(errPolicy)
	}
	if maxInFlight == 0 {
		if policy.MaxInFlight != nil {
			t.Fatalf("MaxInFlight = %d, want nil", *policy.MaxInFlight)
		}
	} else if policy.MaxInFlight == nil || *policy.MaxInFlight != maxInFlight {
		t.Fatalf("MaxInFlight = %v, want %d", policy.MaxInFlight, maxInFlight)
	}
	if len(policy.MaxInFlightByModel) != len(models) {
		t.Fatalf("MaxInFlightByModel = %#v, want %#v", policy.MaxInFlightByModel, models)
	}
	for model, limit := range models {
		if policy.MaxInFlightByModel[model] != limit {
			t.Fatalf("MaxInFlightByModel = %#v, want %#v", policy.MaxInFlightByModel, models)
		}
	}
}
