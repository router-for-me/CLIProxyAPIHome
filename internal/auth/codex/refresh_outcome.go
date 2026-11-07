package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
)

// RefreshOutcomeMetadataKey stores unresolved exchanges without persisting tokens or error bodies.
const RefreshOutcomeMetadataKey = "codex_refresh_outcomes"

// RefreshOutcomeUnknownError means the provider may have consumed the refresh grant.
// It is neither a rejected grant nor permission to submit the same grant again.
type RefreshOutcomeUnknownError struct {
	grant string
	stage string
	cause error
}

func (e *RefreshOutcomeUnknownError) Error() string {
	return "codex token refresh outcome unknown at " + e.stage + "; replace the refresh credentials before retrying"
}

func (e *RefreshOutcomeUnknownError) Unwrap() error { return e.cause }

// Stage identifies where the result became uncertain, without upstream error text.
func (e *RefreshOutcomeUnknownError) Stage() string { return e.stage }

// NewRefreshOutcomeUnknownError records which grant must not be replayed.
func NewRefreshOutcomeUnknownError(clientID, refreshToken, stage string, cause error) *RefreshOutcomeUnknownError {
	switch stage {
	case "transport", "response_read", "response_status", "response_decode", "response_validate",
		"pending", "claim", "claim_release", "receipt_read", "receipt_write", "receipt_encode",
		"receipt_decode", "receipt_expired", "receipt_cycle", "receipt_chain_limit":
	default:
		stage = "transport"
	}
	return &RefreshOutcomeUnknownError{grant: refreshGrantFingerprint(clientID, refreshToken), stage: stage, cause: cause}
}

// IsRefreshOutcomeUnknown distinguishes unresolved exchanges from terminal rejection.
func IsRefreshOutcomeUnknown(err error) bool {
	var outcome *RefreshOutcomeUnknownError
	return errors.As(err, &outcome)
}

func refreshGrantFingerprint(clientID, refreshToken string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(clientID) + "\x00" + strings.TrimSpace(refreshToken)))
	return hex.EncodeToString(digest[:])
}

// Journal observations do not establish that this caller submitted a request.
// A retained claim still blocks; an absent claim may reflect a qualified release.
func isJournalObservationStage(stage string) bool {
	switch stage {
	case "claim", "claim_release", "pending", "receipt_read":
		return true
	default:
		return false
	}
}

// Unresolved grants must not expire from this cache: elapsed time does not prove
// that an earlier exchange had no effect. Credential owners also persist this
// state in metadata for subsequent processes.
var unresolvedRefreshOutcomes sync.Map

func unresolvedRefreshOutcome(clientID, refreshToken string) error {
	if value, ok := unresolvedRefreshOutcomes.Load(refreshGrantFingerprint(clientID, refreshToken)); ok {
		return value.(*RefreshOutcomeUnknownError)
	}
	return nil
}

func rememberRefreshOutcome(err error) {
	var outcome *RefreshOutcomeUnknownError
	if errors.As(err, &outcome) {
		// Do not retain transport objects, response bodies, or arbitrary error text.
		unresolvedRefreshOutcomes.Store(outcome.grant, &RefreshOutcomeUnknownError{grant: outcome.grant, stage: outcome.stage})
	}
}

// doRefreshRequest disallows redirects: a later dial failure must not hide an
// earlier submitted POST, and a redirect must not replay rotating credentials.
func doRefreshRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	refreshClient := *client
	refreshClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return refreshClient.Do(req)
}

func refreshRequestNotSent(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func refreshPartition(metadata map[string]any, siwc bool) (string, string, string) {
	partition, clientID := "outer", ClientID
	if siwc {
		partition = "siwc"
		metadata, _ = metadata["siwc"].(map[string]any)
		clientID, _ = metadata["client_id"].(string)
	}
	token, _ := metadata["refresh_token"].(string)
	if strings.TrimSpace(token) == "" || strings.TrimSpace(clientID) == "" {
		return partition, "", ""
	}
	return partition, clientID, token
}

// RefreshOutcomeUnknown returns a persisted fence only for the current grant.
// Replacing one partition does not release the other partition's fence.
// Loading the fence also prevents other in-process callers from replaying it.
func RefreshOutcomeUnknown(metadata map[string]any, siwc bool) error {
	partition, clientID, token := refreshPartition(metadata, siwc)
	if token == "" {
		return nil
	}
	outcomes, _ := metadata[RefreshOutcomeMetadataKey].(map[string]any)
	state, _ := outcomes[partition].(map[string]any)
	grant, _ := state["grant_sha256"].(string)
	stage, _ := state["stage"].(string)
	if grant != refreshGrantFingerprint(clientID, token) {
		return nil
	}
	outcome := NewRefreshOutcomeUnknownError(clientID, token, stage, nil)
	rememberRefreshOutcome(outcome)
	return outcome
}

func outcomeForGrant(err error, grant string) *RefreshOutcomeUnknownError {
	if err == nil {
		return nil
	}
	if outcome, ok := err.(*RefreshOutcomeUnknownError); ok && outcome.grant == grant {
		return outcome
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range multi.Unwrap() {
			if outcome := outcomeForGrant(nested, grant); outcome != nil {
				return outcome
			}
		}
	}
	return outcomeForGrant(errors.Unwrap(err), grant)
}

// RecordRefreshOutcomeUnknown applies only failures of the tokens still present
// in metadata, so a stale refresh cannot quarantine replacement credentials.
// It returns whether the error describes a current grant, even if already recorded.
func RecordRefreshOutcomeUnknown(metadata map[string]any, err error) bool {
	if metadata == nil || err == nil {
		return false
	}
	previous, _ := metadata[RefreshOutcomeMetadataKey].(map[string]any)
	outcomes := make(map[string]any, len(previous)+2)
	for partition, state := range previous {
		outcomes[partition] = state
	}
	matched := false
	for _, siwc := range []bool{false, true} {
		partition, clientID, token := refreshPartition(metadata, siwc)
		if token == "" {
			continue
		}
		outcome := outcomeForGrant(err, refreshGrantFingerprint(clientID, token))
		if outcome == nil {
			continue
		}
		previousState, _ := previous[partition].(map[string]any)
		previousGrant, _ := previousState["grant_sha256"].(string)
		previousStage, _ := previousState["stage"].(string)
		// A later journal failure must not erase evidence of an earlier
		// submitted exchange, even if no journal entry could be persisted.
		if previousGrant != outcome.grant || !isJournalObservationStage(outcome.stage) || isJournalObservationStage(previousStage) {
			outcomes[partition] = map[string]any{"grant_sha256": outcome.grant, "stage": outcome.stage}
		}
		matched = true
	}
	if matched {
		metadata[RefreshOutcomeMetadataKey] = outcomes
	}
	return matched
}

// MergeRefreshOutcomes adopts persisted fences without overwriting newer grants.
func MergeRefreshOutcomes(metadata, source map[string]any) {
	for _, siwc := range []bool{false, true} {
		RecordRefreshOutcomeUnknown(metadata, RefreshOutcomeUnknown(source, siwc))
	}
}
