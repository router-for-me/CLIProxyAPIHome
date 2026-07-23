package push

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

const concurrencyReleaseMaximumCredentialIDBytes = 256

type concurrencyReleaseFrame struct {
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
	ReleaseSeq   int64  `json:"release_seq"`
}

// handleConcurrencyRelease applies one cumulative concurrency release from a controlled CPA connection.
func handleConcurrencyRelease(ctx context.Context, env dispatch.Env, args []string) dispatch.Reply {
	frame, okFrame := parseConcurrencyReleaseFrame(args)
	if !okFrame {
		return dispatch.Err("invalid concurrency release")
	}
	if env.ConcurrencyReleaseStore == nil || !env.ConnectionLifetime.Controlled || strings.TrimSpace(env.ConnectionLifetime.Fingerprint) == "" || env.ConnectionLifetime.ConnectedAt.IsZero() {
		return dispatch.Err("concurrency release connection is not active")
	}

	credentialID := strings.TrimSpace(frame.CredentialID)
	model, validModel := concurrency.ValidCanonicalConcurrencyModelKey(frame.Model)
	if credentialID == "" || !validModel || len(credentialID) > concurrencyReleaseMaximumCredentialIDBytes || frame.ReleaseSeq <= 0 {
		return dispatch.Err("invalid concurrency release")
	}

	if errRelease := env.ConcurrencyReleaseStore.ApplyConcurrencyRelease(ctx, cluster.ConcurrencyReleaseRequest{
		CredentialID: credentialID,
		Model:        model,
		ReleaseSeq:   frame.ReleaseSeq,
		Lifetime:     env.ConnectionLifetime,
	}); errRelease != nil {
		return dispatch.Err("concurrency release rejected")
	}
	return dispatch.Integer(1)
}

func parseConcurrencyReleaseFrame(args []string) (concurrencyReleaseFrame, bool) {
	if len(args) < 2 || !strings.EqualFold(strings.TrimSpace(args[0]), "LPUSH") || !strings.EqualFold(strings.TrimSpace(args[1]), "concurrency-release") {
		return concurrencyReleaseFrame{}, false
	}
	switch len(args) {
	case 3:
		return decodeConcurrencyReleaseFrame([]byte(args[2]))
	case 5:
		releaseSeq, errSequence := strconv.ParseInt(strings.TrimSpace(args[4]), 10, 64)
		if errSequence != nil {
			return concurrencyReleaseFrame{}, false
		}
		return concurrencyReleaseFrame{
			CredentialID: args[2],
			Model:        args[3],
			ReleaseSeq:   releaseSeq,
		}, true
	default:
		return concurrencyReleaseFrame{}, false
	}
}

func decodeConcurrencyReleaseFrame(raw []byte) (concurrencyReleaseFrame, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	frame := concurrencyReleaseFrame{}
	if errDecode := decoder.Decode(&frame); errDecode != nil {
		return concurrencyReleaseFrame{}, false
	}
	if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
		return concurrencyReleaseFrame{}, false
	}
	return frame, true
}
