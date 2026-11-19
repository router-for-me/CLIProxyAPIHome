package userapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type apiKeyLoginResponse struct {
	Token string `json:"token"`
	User  struct {
		ID               uint    `json:"id"`
		Username         string  `json:"username"`
		Credits          float64 `json:"credits"`
		CreditsUnlimited bool    `json:"credits_unlimited"`
	} `json:"user"`
}

func TestAPIKeyLoginProvisionsUserBindsKeyAndReusesAccount(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()

	const (
		authAlphaID    = "api-key-login-auth-alpha"
		authBetaID     = "api-key-login-auth-beta"
		authDisabledID = "api-key-login-auth-disabled"
	)
	for _, auth := range []*coreauth.Auth{
		{ID: authAlphaID, Index: authAlphaID, Provider: "openai", Status: coreauth.StatusActive},
		{ID: authBetaID, Index: authBetaID, Provider: "openai", Status: coreauth.StatusActive},
		{ID: authDisabledID, Index: authDisabledID, Provider: "openai", Status: coreauth.StatusDisabled, Disabled: true},
	} {
		if _, errUpsert := handler.repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
		}
	}
	registry.GetGlobalRegistry().RegisterClient(authAlphaID, "openai", []*registry.ModelInfo{
		{ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 mini"},
	})
	registry.GetGlobalRegistry().RegisterClient(authBetaID, "openai", []*registry.ModelInfo{
		{ID: "text-only-model", DisplayName: "Text Only"},
	})
	registry.GetGlobalRegistry().RegisterClient(authDisabledID, "openai", []*registry.ModelInfo{
		{ID: "disabled-channel-model", DisplayName: "Disabled Model"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authAlphaID)
		registry.GetGlobalRegistry().UnregisterClient(authBetaID)
		registry.GetGlobalRegistry().UnregisterClient(authDisabledID)
	})

	alphaChannel, errAlpha := handler.repo.CreateChannelGroup(ctx, "alpha-channel", false)
	if errAlpha != nil {
		t.Fatalf("CreateChannelGroup(alpha) error = %v", errAlpha)
	}
	if _, errDetail := handler.repo.CreateChannelGroupDetail(ctx, alphaChannel.ID, authAlphaID); errDetail != nil {
		t.Fatalf("CreateChannelGroupDetail(alpha) error = %v", errDetail)
	}
	if _, errDetail := handler.repo.CreateChannelGroupDetail(ctx, alphaChannel.ID, authDisabledID); errDetail != nil {
		t.Fatalf("CreateChannelGroupDetail(disabled) error = %v", errDetail)
	}
	betaChannel, errBeta := handler.repo.CreateChannelGroup(ctx, "beta-channel", false)
	if errBeta != nil {
		t.Fatalf("CreateChannelGroup(beta) error = %v", errBeta)
	}
	if _, errDetail := handler.repo.CreateChannelGroupDetail(ctx, betaChannel.ID, authBetaID); errDetail != nil {
		t.Fatalf("CreateChannelGroupDetail(beta) error = %v", errDetail)
	}

	const clientKey = "aig-0123456789abcdef0123456789abcdef0123456789a"
	displayName := "my-team-alias"
	createdKey, errCreateKey := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{
		APIKey:         clientKey,
		DisplayName:    &displayName,
		DisplayNameSet: true,
		Channels:       &[]uint{alphaChannel.ID},
	})
	if errCreateKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreateKey)
	}
	if createdKey.UserID != nil {
		t.Fatalf("initial key user_id = %v, want nil", *createdKey.UserID)
	}

	// First login with arbitrary username and API key in password auto-provisions user.
	firstResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", map[string]any{
		"username": "my-alias",
		"password": clientKey,
	}, "")
	if firstResp.Code != http.StatusOK {
		t.Fatalf("first login status = %d body=%s, want 200", firstResp.Code, firstResp.Body.String())
	}
	var firstLogin apiKeyLoginResponse
	if errDecode := json.Unmarshal(firstResp.Body.Bytes(), &firstLogin); errDecode != nil {
		t.Fatalf("decode first login: %v", errDecode)
	}
	if firstLogin.Token == "" {
		t.Fatal("first login returned empty bearer token")
	}
	if firstLogin.User.ID == 0 {
		t.Fatal("first login returned user id 0")
	}
	if firstLogin.User.Username != displayName {
		t.Fatalf("provisioned username = %q, want %q", firstLogin.User.Username, displayName)
	}
	if !firstLogin.User.CreditsUnlimited {
		t.Fatal("provisioned user credits_unlimited = false, want true")
	}

	storedUser, errUser := handler.repo.GetUser(ctx, firstLogin.User.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if !storedUser.CreditsUnlimited || storedUser.Timezone != "UTC" {
		t.Fatalf("stored user = unlimited:%v tz:%q, want true / UTC", storedUser.CreditsUnlimited, storedUser.Timezone)
	}

	keysForUser, errKeys := handler.repo.ListAPIKeyRecordsForUser(ctx, firstLogin.User.ID)
	if errKeys != nil {
		t.Fatalf("ListAPIKeyRecordsForUser() error = %v", errKeys)
	}
	if len(keysForUser) != 1 || keysForUser[0].APIKey != clientKey {
		t.Fatalf("bound keys for user = %#v, want [%s]", keysForUser, clientKey)
	}

	// Subsequent GET /user/me returns credits_unlimited: true.
	meResp := performUserJSONRequest(t, router, http.MethodGet, "/user/me", nil, firstLogin.Token)
	if meResp.Code != http.StatusOK {
		t.Fatalf("GET /user/me status = %d body=%s", meResp.Code, meResp.Body.String())
	}
	var mePayload struct {
		User struct {
			ID               uint `json:"id"`
			CreditsUnlimited bool `json:"credits_unlimited"`
		} `json:"user"`
	}
	if errDecode := json.Unmarshal(meResp.Body.Bytes(), &mePayload); errDecode != nil {
		t.Fatalf("decode /user/me: %v", errDecode)
	}
	if mePayload.User.ID != firstLogin.User.ID || !mePayload.User.CreditsUnlimited {
		t.Fatalf("/user/me user = %+v, want id=%d credits_unlimited=true", mePayload.User, firstLogin.User.ID)
	}

	// Subsequent GET /user/api-keys returns the bound aig-... key.
	apiKeysResp := performUserJSONRequest(t, router, http.MethodGet, "/user/api-keys", nil, firstLogin.Token)
	if apiKeysResp.Code != http.StatusOK {
		t.Fatalf("GET /user/api-keys status = %d body=%s", apiKeysResp.Code, apiKeysResp.Body.String())
	}
	var apiKeysPayload struct {
		APIKeys []struct {
			ID       uint   `json:"id"`
			APIKey   string `json:"api_key"`
			Channels []uint `json:"channels"`
		} `json:"api_keys"`
	}
	if errDecode := json.Unmarshal(apiKeysResp.Body.Bytes(), &apiKeysPayload); errDecode != nil {
		t.Fatalf("decode /user/api-keys: %v", errDecode)
	}
	if len(apiKeysPayload.APIKeys) != 1 || apiKeysPayload.APIKeys[0].APIKey != clientKey {
		t.Fatalf("/user/api-keys = %+v, want [%s]", apiKeysPayload.APIKeys, clientKey)
	}
	if len(apiKeysPayload.APIKeys[0].Channels) != 1 || apiKeysPayload.APIKeys[0].Channels[0] != alphaChannel.ID {
		t.Fatalf("/user/api-keys channels = %v, want [%d]", apiKeysPayload.APIKeys[0].Channels, alphaChannel.ID)
	}

	// Subsequent GET /user/models/accessible returns only the channel-scoped model.
	modelsResp := performUserJSONRequest(t, router, http.MethodGet, "/user/models/accessible", nil, firstLogin.Token)
	if modelsResp.Code != http.StatusOK {
		t.Fatalf("GET /user/models/accessible status = %d body=%s", modelsResp.Code, modelsResp.Body.String())
	}
	var modelsPayload userModelListPayload
	if errDecode := json.Unmarshal(modelsResp.Body.Bytes(), &modelsPayload); errDecode != nil {
		t.Fatalf("decode /user/models/accessible: %v", errDecode)
	}
	gotModels := userModelIDs(modelsPayload.Models)
	if len(gotModels) != 1 || gotModels[0] != "gpt-4.1-mini" {
		t.Fatalf("/user/models/accessible models = %v, want [gpt-4.1-mini]", gotModels)
	}
	if !modelsPayload.Access.Restricted || modelsPayload.Access.APIKeyCount != 1 {
		t.Fatalf("/user/models/accessible access = %+v, want restricted=true api_key_count=1", modelsPayload.Access)
	}
	if modelsPayload.Access.Reason != modelAccessReasonChannelGroups {
		t.Fatalf("/user/models/accessible reason = %q, want %q", modelsPayload.Access.Reason, modelAccessReasonChannelGroups)
	}

	// Second login variations reuse the exact same user.
	for _, payload := range []map[string]any{
		{"username": clientKey, "password": clientKey},
		{"username": "", "password": clientKey},
		{"username": clientKey, "password": ""},
		{"username": displayName, "password": clientKey},
	} {
		againResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", payload, "")
		if againResp.Code != http.StatusOK {
			t.Fatalf("repeat login %v status = %d body=%s, want 200", payload, againResp.Code, againResp.Body.String())
		}
		var againLogin apiKeyLoginResponse
		if errDecode := json.Unmarshal(againResp.Body.Bytes(), &againLogin); errDecode != nil {
			t.Fatalf("decode repeat login: %v", errDecode)
		}
		if againLogin.User.ID != firstLogin.User.ID {
			t.Fatalf("repeat login %v user id = %d, want %d", payload, againLogin.User.ID, firstLogin.User.ID)
		}
	}

	users, errUsers := handler.repo.ListUsers(ctx)
	if errUsers != nil {
		t.Fatalf("ListUsers() error = %v", errUsers)
	}
	if len(users) != 1 {
		t.Fatalf("total users = %d, want 1", len(users))
	}
}

func TestAPIKeyLoginRejectsUnknownDeletedAndMismatchedKeys(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()

	const activeKey = "aig-active-key-11111111111111111111111111111111"
	const deletedKey = "aig-deleted-key-2222222222222222222222222222222"
	if _, errCreate := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: activeKey}); errCreate != nil {
		t.Fatalf("CreateAPIKey(active) error = %v", errCreate)
	}
	if _, errCreate := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: deletedKey}); errCreate != nil {
		t.Fatalf("CreateAPIKey(deleted) error = %v", errCreate)
	}
	if errDelete := handler.repo.DeleteAPIKey(ctx, cluster.APIKeySelector{APIKey: deletedKey}); errDelete != nil {
		t.Fatalf("DeleteAPIKey(deleted) error = %v", errDelete)
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "unknown key in password", body: map[string]any{"username": "x", "password": "aig-unknown-key-999999999999999999999999999999"}},
		{name: "unknown key in username", body: map[string]any{"username": "aig-unknown-key-999999999999999999999999999999", "password": ""}},
		{name: "deleted key", body: map[string]any{"username": "x", "password": deletedKey}},
		{name: "key in username with non-matching password", body: map[string]any{"username": activeKey, "password": "wrong-password"}},
		{name: "two different aig keys", body: map[string]any{"username": activeKey, "password": "aig-other-key-333333333333333333333333333333333"}},
		{name: "bare prefix", body: map[string]any{"username": "x", "password": "aig-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", tc.body, "")
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d body=%s, want 401", resp.Code, resp.Body.String())
			}
			var errPayload struct {
				Error string `json:"error"`
			}
			if errDecode := json.Unmarshal(resp.Body.Bytes(), &errPayload); errDecode != nil {
				t.Fatalf("decode error payload: %v", errDecode)
			}
			if errPayload.Error != "invalid_credentials" {
				t.Fatalf("error code = %q, want invalid_credentials", errPayload.Error)
			}
		})
	}
}

func TestAPIKeyLoginFallbackUsernameCollisionLongKeyAndNormalLogin(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()

	// Normal username/password login (including a password starting with aig-) continues to work first.
	normalUser := "alice"
	normalPass := "aig-not-an-api-key-password"
	hashedNormal, errHash := hashPassword(normalPass)
	if errHash != nil {
		t.Fatalf("hashPassword() error = %v", errHash)
	}
	alice, errAlice := handler.repo.CreateUser(ctx, cluster.UserUpdate{
		Username: &normalUser,
		Password: &hashedNormal,
	})
	if errAlice != nil {
		t.Fatalf("CreateUser(alice) error = %v", errAlice)
	}
	aliceResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", map[string]any{
		"username": normalUser,
		"password": normalPass,
	}, "")
	if aliceResp.Code != http.StatusOK {
		t.Fatalf("normal user login status = %d body=%s, want 200", aliceResp.Code, aliceResp.Body.String())
	}
	var aliceLogin apiKeyLoginResponse
	if errDecode := json.Unmarshal(aliceResp.Body.Bytes(), &aliceLogin); errDecode != nil {
		t.Fatalf("decode normal login: %v", errDecode)
	}
	if aliceLogin.User.ID != alice.ID || aliceLogin.User.CreditsUnlimited {
		t.Fatalf("normal login user = %+v, want id=%d credits_unlimited=false", aliceLogin.User, alice.ID)
	}

	// Colliding display_name gets -<6hex> suffix instead of hijacking existing user.
	collidingKey := "aig-colliding-key-abcdef0123456789abcdef0123456"
	collidingSum := sha256.Sum256([]byte(collidingKey))
	collidingHex := hex.EncodeToString(collidingSum[:])
	if _, errKey := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{
		APIKey:         collidingKey,
		DisplayName:    &normalUser,
		DisplayNameSet: true,
	}); errKey != nil {
		t.Fatalf("CreateAPIKey(colliding) error = %v", errKey)
	}
	collideResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", map[string]any{
		"username": "ignored",
		"password": collidingKey,
	}, "")
	if collideResp.Code != http.StatusOK {
		t.Fatalf("colliding key login status = %d body=%s, want 200", collideResp.Code, collideResp.Body.String())
	}
	var collideLogin apiKeyLoginResponse
	if errDecode := json.Unmarshal(collideResp.Body.Bytes(), &collideLogin); errDecode != nil {
		t.Fatalf("decode colliding login: %v", errDecode)
	}
	wantCollidingUsername := normalUser + "-" + collidingHex[:6]
	if collideLogin.User.ID == alice.ID || collideLogin.User.Username != wantCollidingUsername {
		t.Fatalf("colliding provisioned user = %+v, want id!=%d username=%q", collideLogin.User, alice.ID, wantCollidingUsername)
	}

	// Key without display_name and longer than 72 bytes uses key-<12hex> and hashes SHA-256 hex.
	longKey := "aig-" + strings.Repeat("z", 80)
	longSum := sha256.Sum256([]byte(longKey))
	longHex := hex.EncodeToString(longSum[:])
	if _, errKey := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{
		APIKey: longKey,
	}); errKey != nil {
		t.Fatalf("CreateAPIKey(long) error = %v", errKey)
	}
	longResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", map[string]any{
		"password": longKey,
	}, "")
	if longResp.Code != http.StatusOK {
		t.Fatalf("long key login status = %d body=%s, want 200", longResp.Code, longResp.Body.String())
	}
	var longLogin apiKeyLoginResponse
	if errDecode := json.Unmarshal(longResp.Body.Bytes(), &longLogin); errDecode != nil {
		t.Fatalf("decode long login: %v", errDecode)
	}
	wantLongUsername := "key-" + longHex[:12]
	if longLogin.User.Username != wantLongUsername || !longLogin.User.CreditsUnlimited {
		t.Fatalf("long key user = %+v, want username=%q credits_unlimited=true", longLogin.User, wantLongUsername)
	}

	// Soft-deleted owning user triggers re-provisioning and rebinding on next API-key login.
	if errDelUser := handler.repo.DeleteUser(ctx, longLogin.User.ID); errDelUser != nil {
		t.Fatalf("DeleteUser(long) error = %v", errDelUser)
	}
	reprovisionResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", map[string]any{
		"username": longKey,
		"password": longKey,
	}, "")
	if reprovisionResp.Code != http.StatusOK {
		t.Fatalf("reprovision login status = %d body=%s, want 200", reprovisionResp.Code, reprovisionResp.Body.String())
	}
	var reprovisionLogin apiKeyLoginResponse
	if errDecode := json.Unmarshal(reprovisionResp.Body.Bytes(), &reprovisionLogin); errDecode != nil {
		t.Fatalf("decode reprovision login: %v", errDecode)
	}
	if reprovisionLogin.User.ID == longLogin.User.ID || reprovisionLogin.User.Username != wantLongUsername {
		t.Fatalf("reprovisioned user = %+v, want new id != %d username=%q", reprovisionLogin.User, longLogin.User.ID, wantLongUsername)
	}
}

func TestAPIKeyLoginConcurrentProvisioningCreatesSingleUser(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()

	const clientKey = "aig-concurrent-key-0123456789abcdef0123456789ab"
	alias := "concurrent-alias"
	if _, errCreate := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{
		APIKey:         clientKey,
		DisplayName:    &alias,
		DisplayNameSet: true,
	}); errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}

	const workers = 6
	payloadBytes, errMarshal := json.Marshal(map[string]any{
		"username": alias,
		"password": clientKey,
	})
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	results := make([]uint, workers)
	errs := make([]string, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/user/login", bytes.NewReader(payloadBytes))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				errs[idx] = resp.Body.String()
				return
			}
			var decoded apiKeyLoginResponse
			if errDecode := json.Unmarshal(resp.Body.Bytes(), &decoded); errDecode != nil {
				errs[idx] = errDecode.Error()
				return
			}
			results[idx] = decoded.User.ID
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != "" {
			t.Fatalf("worker %d failed: %s", i, errs[i])
		}
		if results[i] == 0 || results[i] != results[0] {
			t.Fatalf("worker %d got user_id=%d, want consistent %d", i, results[i], results[0])
		}
	}

	users, errUsers := handler.repo.ListUsers(ctx)
	if errUsers != nil {
		t.Fatalf("ListUsers() error = %v", errUsers)
	}
	if len(users) != 1 {
		t.Fatalf("users = %d, want 1", len(users))
	}
}

func TestAPIKeyLoginEmptyChannelGroupHidesModels(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	ctx := context.Background()

	const authID = "empty-channel-other-auth"
	registry.GetGlobalRegistry().RegisterClient(authID, "openai", []*registry.ModelInfo{
		{ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 mini"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})

	emptyChannel, errEmpty := handler.repo.CreateChannelGroup(ctx, "empty-channel", false)
	if errEmpty != nil {
		t.Fatalf("CreateChannelGroup(empty) error = %v", errEmpty)
	}
	const clientKey = "aig-empty-channel-key-1234567890abcdef123456789"
	if _, errCreate := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{
		APIKey:   clientKey,
		Channels: &[]uint{emptyChannel.ID},
	}); errCreate != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreate)
	}

	loginResp := performUserJSONRequest(t, router, http.MethodPost, "/user/login", map[string]any{
		"username": clientKey,
		"password": clientKey,
	}, "")
	if loginResp.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s, want 200", loginResp.Code, loginResp.Body.String())
	}
	var login apiKeyLoginResponse
	if errDecode := json.Unmarshal(loginResp.Body.Bytes(), &login); errDecode != nil {
		t.Fatalf("decode login: %v", errDecode)
	}

	modelsResp := performUserJSONRequest(t, router, http.MethodGet, "/user/models/accessible", nil, login.Token)
	if modelsResp.Code != http.StatusOK {
		t.Fatalf("GET /user/models/accessible status = %d body=%s", modelsResp.Code, modelsResp.Body.String())
	}
	var modelsPayload userModelListPayload
	if errDecode := json.Unmarshal(modelsResp.Body.Bytes(), &modelsPayload); errDecode != nil {
		t.Fatalf("decode models: %v", errDecode)
	}
	if len(modelsPayload.Models) != 0 || !modelsPayload.Access.Restricted {
		t.Fatalf("models = %v restricted=%v, want empty and restricted=true", userModelIDs(modelsPayload.Models), modelsPayload.Access.Restricted)
	}
	if modelsPayload.Access.Reason != modelAccessReasonChannelGroups {
		t.Fatalf("empty channel reason = %q, want %q", modelsPayload.Access.Reason, modelAccessReasonChannelGroups)
	}
}
