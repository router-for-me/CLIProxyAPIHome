package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func quotaCooldownModelState(now time.Time, delay time.Duration) *ModelState {
	next := now.Add(delay)
	return &ModelState{
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: next,
			BackoffLevel:  3,
		},
		UpdatedAt: now,
	}
}

func TestModelScopedAggregateDoesNotBlockUnrelatedModel(t *testing.T) {
	now := time.Now().UTC()
	early := now.Add(10 * time.Minute)
	late := now.Add(20 * time.Minute)
	auth := &Auth{
		ID:       "auth-model-aggregate",
		Provider: "antigravity",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: early,
				Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: early, BackoffLevel: 1},
			},
			"model-b": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: late,
				Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: late, BackoffLevel: 3},
			},
		},
	}

	updateAggregatedAvailability(auth, now)
	if !auth.Quota.Exceeded || auth.Quota.Scope != quotaScopeModel || !auth.Quota.NextRecoverAt.Equal(early) || auth.Quota.BackoffLevel != 3 {
		t.Fatalf("model aggregate = %#v, want earliest recovery and maximum backoff", auth.Quota)
	}
	if blocked, reason, next := isAuthBlockedForModel(auth, "model-c", now); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("unrelated model blocked/reason/next = %v/%v/%v, want available", blocked, reason, next)
	}
}

func TestLegacyMixedModelAggregateDoesNotBlockUnrelatedModel(t *testing.T) {
	now := time.Now().UTC()
	early := now.Add(10 * time.Minute)
	late := now.Add(20 * time.Minute)
	auth := &Auth{
		ID:       "auth-legacy-model-aggregate",
		Provider: "antigravity",
		Status:   StatusError,
		Quota:    QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: early, BackoffLevel: 3},
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: early,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: early, BackoffLevel: 1},
			},
			"model-b": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: late,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: late, BackoffLevel: 3},
			},
		},
	}

	if blocked, reason, next := isAuthBlockedForModel(auth, "model-c", now); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("unrelated model blocked/reason/next = %v/%v/%v, want available", blocked, reason, next)
	}
}

func TestLegacyCredentialWideStateDoesNotBlockDispatch(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(15 * time.Minute)
	auth := &Auth{
		ID:             "auth-legacy-wide",
		Provider:       "codex",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          QuotaState{Exceeded: true, Scope: "credential", Reason: "quota", NextRecoverAt: next, BackoffLevel: 4},
	}

	for _, model := range []string{"", "gpt-5"} {
		if blocked, reason, retryAt := isAuthBlockedForModel(auth, model, now); blocked || reason != blockReasonNone || !retryAt.IsZero() {
			t.Fatalf("model %q blocked/reason/next = %v/%v/%v, want available", model, blocked, reason, retryAt)
		}
	}
}
func registryHasAvailableModel(modelRegistry *registry.ModelRegistry, model string) bool {
	for _, item := range modelRegistry.GetAvailableModelDefinitions() {
		if item != nil && item.ID == model {
			return true
		}
	}
	return false
}

func TestReconcileRegistryModelStatesKeepsTransientErrorsVisible(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "request-timeout", status: http.StatusRequestTimeout},
		{name: "internal-server-error", status: http.StatusInternalServerError},
		{name: "bad-gateway", status: http.StatusBadGateway},
		{name: "service-unavailable", status: http.StatusServiceUnavailable},
		{name: "gateway-timeout", status: http.StatusGatewayTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authID := "auth-registry-transient-" + tc.name
			model := "registry-transient-" + tc.name
			modelRegistry := registry.GetGlobalRegistry()
			modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			manager := NewManager(nil, nil, nil)
			if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive}); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			manager.MarkResult(context.Background(), Result{
				AuthID: authID, Provider: "codex", Model: model,
				Error: &Error{Message: http.StatusText(tc.status), HTTPStatus: tc.status},
			})
			manager.ReconcileRegistryModelStates(context.Background(), authID)
			if !registryHasAvailableModel(modelRegistry, model) {
				t.Fatalf("registry hid model for transient HTTP %d", tc.status)
			}
		})
	}
}

func TestShouldSuspendRegistryModelMatchesResultTransitions(t *testing.T) {
	const model = "registry-transition-model"
	modelUnsupported := &Error{Message: "requested model is not supported", HTTPStatus: http.StatusBadRequest}
	cases := []struct {
		name   string
		reason blockReason
		err    *Error
		want   bool
	}{
		{name: "quota", reason: blockReasonCooldown, want: true},
		{name: "disabled", reason: blockReasonDisabled, want: true},
		{name: "payment-required", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusPaymentRequired}, want: true},
		{name: "forbidden", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusForbidden}, want: true},
		{name: "not-found", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusNotFound}, want: true},
		{name: "model-not-supported", reason: blockReasonOther, err: modelUnsupported, want: true},
		{name: "unauthorized", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusUnauthorized}},
		{name: "request-timeout", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusRequestTimeout}},
		{name: "service-unavailable", reason: blockReasonOther, err: &Error{HTTPStatus: http.StatusServiceUnavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{ModelStates: map[string]*ModelState{model: {LastError: tc.err}}}
			if got := shouldSuspendRegistryModel(auth, model, tc.reason); got != tc.want {
				t.Fatalf("shouldSuspendRegistryModel() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileRegistryModelStatesResumesPeerAfterStateReset(t *testing.T) {
	const authID = "auth-registry-peer"
	const model = "registry-peer-model"
	now := time.Now().UTC()
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	modelRegistry.SuspendClientModel(authID, model, "forbidden")
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("test setup did not suspend model")
	}

	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Index: authID, Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: {Status: StatusActive, UpdatedAt: now, QuotaResetAt: now}},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.ReconcileRegistryModelStates(context.Background(), authID)
	if !registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("registry remained suspended after peer reconciliation")
	}
}
func TestMergePersistedModelStatePreservesActiveQuotaWithNewerNonQuotaError(t *testing.T) {
	now := time.Now().UTC()
	quotaRecover := now.Add(10 * time.Minute)
	persisted := quotaCooldownModelState(now, 10*time.Minute)
	persisted.Quota.Scope = quotaScopeModel
	localRetry := now.Add(time.Minute)
	localUpdated := now.Add(2 * time.Minute)
	local := &ModelState{
		Status:         StatusError,
		StatusMessage:  "transient upstream error",
		Unavailable:    true,
		NextRetryAfter: localRetry,
		LastError:      &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable},
		UpdatedAt:      localUpdated,
	}

	merged := MergePersistedModelState(persisted, local)
	if merged == nil || merged.LastError == nil || merged.LastError.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("merged state = %#v, want newer local 5xx", merged)
	}
	if !merged.Quota.Exceeded || merged.Quota.Scope != quotaScopeModel || !merged.Quota.NextRecoverAt.Equal(quotaRecover) {
		t.Fatalf("merged quota = %#v, want persisted quota", merged.Quota)
	}
	if !merged.Unavailable || !merged.NextRetryAfter.Equal(quotaRecover) {
		t.Fatalf("merged availability = %v/%v, want quota deadline %v", merged.Unavailable, merged.NextRetryAfter, quotaRecover)
	}
	if !merged.UpdatedAt.Equal(localUpdated) {
		t.Fatalf("merged UpdatedAt = %v, want local timestamp %v", merged.UpdatedAt, localUpdated)
	}
	auth := &Auth{ModelStates: map[string]*ModelState{"gpt-a": merged}}
	if blocked, reason, next := isAuthBlockedForModel(auth, "gpt-a", now.Add(2*time.Minute)); !blocked || reason != blockReasonCooldown || !next.Equal(quotaRecover) {
		t.Fatalf("blocked/reason/next after local retry = %v/%v/%v, want shared quota until %v", blocked, reason, next, quotaRecover)
	}
}
