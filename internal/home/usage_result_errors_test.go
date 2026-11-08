package home

import (
	"context"
	"errors"
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

type usageFeedbackErrorStore struct {
	coreauth.Store
	err error
}

func (s *usageFeedbackErrorStore) MutateAuthState(context.Context, string, func(*coreauth.Auth) coreauth.StateMutation) (*coreauth.Auth, error) {
	return nil, s.err
}

func TestRecordUsagePayloadOnlyAcknowledgesTerminalNotFound(t *testing.T) {
	transient := errors.New("database unavailable")
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "deleted credential", err: fmt.Errorf("mutation: %w", coreauth.ErrAuthNotFound)},
		{name: "deleted during cache reconciliation", err: fmt.Errorf("reload: %w", coreauth.ErrFullAuthNotFound)},
		{name: "transient mutation failure", err: transient, want: transient},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			manager := coreauth.NewManager(&usageFeedbackErrorStore{err: test.err}, nil, nil)
			if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{ID: "feedback-auth", Index: "feedback-auth", Provider: "codex", Status: coreauth.StatusActive}); errRegister != nil {
				t.Fatal(errRegister)
			}
			rt := &Runtime{coreManager: manager}
			payload := `{"timestamp":"2026-01-01T00:00:00Z","auth_index":"feedback-auth","provider":"codex","model":"gpt-5","failed":true,"fail":{"status_code":429}}`
			if errRecord := rt.RecordUsagePayload(ctx, payload); !errors.Is(errRecord, test.want) {
				t.Fatalf("RecordUsagePayload error = %v, want %v", errRecord, test.want)
			}
			if errFeedback := rt.ApplyUsagePayloadFeedback(ctx, payload); !errors.Is(errFeedback, test.want) {
				t.Fatalf("ApplyUsagePayloadFeedback error = %v, want %v", errFeedback, test.want)
			}
		})
	}
}
