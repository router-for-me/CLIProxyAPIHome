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

// ExcludedAuthIDsMetadataKey stores credential IDs already attempted for the
// current request retry round in Options.Metadata.
const ExcludedAuthIDsMetadataKey = "excluded_auth_ids"

// RequestRetryRoundMetadataKey stores the zero-based request retry round in
// Options.Metadata. Round zero is the initial credential round.
const RequestRetryRoundMetadataKey = "request_retry_round"

const requestRetryDefaultMetadataKey = "request_retry_default"

// ExcludedConcurrencyCandidatesMetadataKey stores candidates rejected by atomic
// concurrency admission during this dispatch attempt.
const ExcludedConcurrencyCandidatesMetadataKey = "excluded_concurrency_candidates"

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

func requestRetryRoundFromOptions(opts Options) int {
	if opts.Metadata == nil {
		return 0
	}
	switch value := opts.Metadata[RequestRetryRoundMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 && value == float64(int(value)) {
			return int(value)
		}
	}
	return 0
}

func requestRetryRoundMetadataPresent(opts Options) bool {
	if opts.Metadata == nil {
		return false
	}
	_, ok := opts.Metadata[RequestRetryRoundMetadataKey]
	return ok
}

func requestRetryDefaultFromOptions(opts Options) int {
	if opts.Metadata == nil {
		return 0
	}
	switch value := opts.Metadata[requestRetryDefaultMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 && value == float64(int(value)) {
			return int(value)
		}
	}
	return 0
}

func withRequestRetryDispatchMetadata(opts Options, round, defaultRetry int) Options {
	metadata := make(map[string]any, len(opts.Metadata)+2)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	if round > 0 {
		metadata[RequestRetryRoundMetadataKey] = round
	} else if requestRetryRoundMetadataPresent(opts) {
		metadata[RequestRetryRoundMetadataKey] = 0
	} else {
		delete(metadata, RequestRetryRoundMetadataKey)
	}
	if defaultRetry > 0 {
		metadata[requestRetryDefaultMetadataKey] = defaultRetry
	} else {
		delete(metadata, requestRetryDefaultMetadataKey)
	}
	opts.Metadata = metadata
	return opts
}
