package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RefreshJournal claims a grant in the authoritative credential database before
// submission. Claims never expire. Only their owner may complete or release them;
// release is allowed only when the request is known not to have been submitted.
// Receipts contain credentials and must be backed up and protected with auth data.
type RefreshJournal interface {
	RefreshClaim(context.Context, string) (string, error)
	RefreshRead(context.Context, string) ([]byte, error)
	RefreshComplete(context.Context, string, string, []byte) error
	RefreshRelease(context.Context, string, string) error
}

type refreshJournalContextKey struct{}

// WithRefreshJournal binds refreshes to the authoritative credential database.
func WithRefreshJournal(ctx context.Context, journal RefreshJournal) context.Context {
	return context.WithValue(ctx, refreshJournalContextKey{}, journal)
}

func refreshJournalFrom(ctx context.Context) RefreshJournal {
	journal, _ := ctx.Value(refreshJournalContextKey{}).(RefreshJournal)
	return journal
}

type refreshReceipt struct {
	Stage    string          `json:"stage,omitempty"`
	Rejected string          `json:"rejected,omitempty"`
	Status   int             `json:"status,omitempty"`
	Tokens   *CodexTokenData `json:"tokens,omitempty"`
}

func journalOutcome(token, stage string, cause error) error {
	return NewRefreshOutcomeUnknownError(ClientID, token, stage, cause)
}

// PreserveRefreshOutcome migrates an existing metadata fence without overwriting
// a newer journal result. A completed owner receipt can recover stale metadata.
func PreserveRefreshOutcome(ctx context.Context, token string, outcome error) error {
	journal := refreshJournalFrom(ctx)
	if journal == nil {
		return outcome
	}
	var unknown *RefreshOutcomeUnknownError
	if errors.As(outcome, &unknown) && isJournalObservationStage(unknown.Stage()) {
		// Defer observations to the journal. In particular, a pending owner
		// may subsequently prove non-submission and release its claim.
		return nil
	}
	key := refreshGrantFingerprint(ClientID, token)
	owner, errClaim := journal.RefreshClaim(ctx, key)
	if errClaim != nil {
		return journalOutcome(token, "claim", errClaim)
	}
	if owner == "" {
		return nil
	}
	stage := "pending"
	if errors.As(outcome, &unknown) {
		stage = unknown.Stage()
	}
	receipt, errEncode := json.Marshal(refreshReceipt{Stage: stage})
	if errEncode != nil {
		return journalOutcome(token, "receipt_encode", errEncode)
	}
	if errComplete := completeRefresh(ctx, journal, key, owner, receipt); errComplete != nil {
		return journalOutcome(token, "receipt_write", errComplete)
	}
	return nil
}

func journalRefresh(ctx context.Context, journal RefreshJournal, token string, run func() (*CodexTokenData, error)) (*CodexTokenData, error) {
	key := refreshGrantFingerprint(ClientID, token)
	owner, errClaim := journal.RefreshClaim(ctx, key)
	if errClaim != nil {
		return nil, journalOutcome(token, "claim", errClaim)
	}
	if owner == "" {
		return readRefreshReceipt(ctx, journal, key, token)
	}
	value, errRun := run()
	if errRun != nil && !IsRefreshOutcomeUnknown(errRun) && (refreshRequestNotSent(errRun) ||
		errors.Is(errRun, context.Canceled) || errors.Is(errRun, context.DeadlineExceeded)) {
		persistCtx, cancelPersist := refreshPersistenceContext(ctx)
		errRelease := journal.RefreshRelease(persistCtx, key, owner)
		cancelPersist()
		if errRelease != nil {
			return nil, journalOutcome(token, "claim_release", errRelease)
		}
		return nil, errRun
	}
	receipt := refreshReceipt{Tokens: value}
	if errRun == nil && value != nil && value.RefreshToken == "" {
		// Omitted refresh tokens retain the submitted grant. Save it explicitly
		// so late callers do not retain an earlier grant from their own auth.
		tokens := *value
		tokens.RefreshToken = token
		receipt.Tokens = &tokens
	}
	if errRun != nil {
		receipt.Tokens = nil
		var rejected *codexRefreshResponseError
		var unknown *RefreshOutcomeUnknownError
		switch {
		case errors.As(errRun, &unknown):
			receipt.Stage = unknown.Stage()
		case errors.As(errRun, &rejected) && rejected.terminalCode != "":
			receipt.Rejected, receipt.Status = rejected.terminalCode, rejected.statusCode
		default:
			receipt.Stage = "response_validate"
		}
	}
	encoded, errEncode := json.Marshal(receipt)
	if errEncode != nil {
		return nil, journalOutcome(token, "receipt_encode", errEncode)
	}
	if errComplete := completeRefresh(ctx, journal, key, owner, encoded); errComplete != nil {
		return nil, journalOutcome(token, "receipt_write", errComplete)
	}
	return value, errRun
}

// Keep acquisition-result persistence independent of caller cancellation, while
// retaining the existing credential-acquisition bound for an unavailable store.
func refreshPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func completeRefresh(ctx context.Context, journal RefreshJournal, key, owner string, receipt []byte) error {
	persistCtx, cancelPersist := refreshPersistenceContext(ctx)
	defer cancelPersist()
	return journal.RefreshComplete(persistCtx, key, owner, receipt)
}

func readRefreshReceipt(ctx context.Context, journal RefreshJournal, key, token string) (*CodexTokenData, error) {
	seen := make(map[string]bool)
	var value *CodexTokenData
	grant := token
	for len(seen) < 1024 {
		if seen[key] {
			return nil, journalOutcome(token, "receipt_cycle", nil)
		}
		seen[key] = true
		encoded, errRead := journal.RefreshRead(ctx, key)
		if errRead != nil {
			return nil, journalOutcome(token, "receipt_read", errRead)
		}
		if len(encoded) == 0 && value != nil {
			return usableRefreshReceipt(token, value)
		}
		var receipt refreshReceipt
		if errDecode := json.Unmarshal(encoded, &receipt); errDecode != nil {
			return nil, journalOutcome(token, "receipt_decode", errDecode)
		}
		if receipt.Stage != "" {
			return nil, journalOutcome(token, receipt.Stage, nil)
		}
		if receipt.Rejected != "" {
			body, _ := json.Marshal(map[string]string{"error": receipt.Rejected})
			status := receipt.Status
			if status == 0 {
				status = http.StatusUnauthorized
			}
			return nil, newCodexRefreshResponseError(status, body)
		}
		value = receipt.Tokens
		if value == nil || strings.TrimSpace(value.AccessToken) == "" {
			return nil, journalOutcome(token, "receipt_decode", fmt.Errorf("invalid refresh receipt"))
		}
		if value.RefreshToken == "" {
			// Older receipts may omit it; the chain identifies the grant used
			// for this exchange, which can differ from the caller's old grant.
			value.RefreshToken = grant
		}
		next := refreshGrantFingerprint(ClientID, value.RefreshToken)
		if strings.TrimSpace(value.RefreshToken) == "" || next == key {
			return usableRefreshReceipt(token, value)
		}
		key = next
		grant = value.RefreshToken
	}
	return nil, journalOutcome(token, "receipt_chain_limit", nil)
}

func usableRefreshReceipt(token string, value *CodexTokenData) (*CodexTokenData, error) {
	expiry, errParse := time.Parse(time.RFC3339, value.Expire)
	if errParse != nil || !expiry.After(time.Now()) {
		return nil, journalOutcome(token, "receipt_expired", errParse)
	}
	return value, nil
}
