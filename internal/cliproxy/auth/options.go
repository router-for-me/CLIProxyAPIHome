package auth

import (
	"net/http"
	"net/url"
)

// RequestedModelMetadataKey stores the client-requested model name in Options.Metadata.
// It is used to preserve the original model string across prefix rewriting / alias resolution.
const RequestedModelMetadataKey = "requested_model"

// AllowedAuthIDsMetadataKey stores the dispatch allowlist in Options.Metadata.
const AllowedAuthIDsMetadataKey = "allowed_auth_ids"

// AllowedModelIDsMetadataKey stores the model allowlist in Options.Metadata.
const AllowedModelIDsMetadataKey = "allowed_model_ids"

// ExcludedConcurrencyCandidatesMetadataKey stores candidates rejected by atomic
// concurrency admission during this dispatch attempt.
const ExcludedConcurrencyCandidatesMetadataKey = "excluded_concurrency_candidates"

// DownstreamWebsocketMetadataKey records that the CPA node is serving this request over
// a downstream websocket. CPA knows this from its own request context, so it can only
// reach Home by travelling with the dispatch request.
const DownstreamWebsocketMetadataKey = "downstream_websocket"

// downstreamWebsocketFromOptions reports whether the requesting node is serving a
// downstream websocket connection.
func downstreamWebsocketFromOptions(opts Options) bool {
	if opts.Metadata == nil {
		return false
	}
	enabled, _ := opts.Metadata[DownstreamWebsocketMetadataKey].(bool)
	return enabled
}

// ExcludedConcurrencyCandidate identifies a credential-wide or model-scoped exclusion.
type ExcludedConcurrencyCandidate struct {
	CredentialID string
	Model        string
}

// Options carries optional request hints used during dispatch selection.
//
// This is a deliberately small subset of CPA's execution options: CLIProxyAPIHome only needs
// headers / query / raw request bytes for selector decisions, and a generic metadata bag.
type Options struct {
	Headers         http.Header
	Query           url.Values
	OriginalRequest []byte
	Metadata        map[string]any
}
