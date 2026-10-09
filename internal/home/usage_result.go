package home

import (
	"context"
	"net/http"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// RecordUsagePayload applies downstream usage status to the scheduler auth state.
func (r *Runtime) RecordUsagePayload(ctx context.Context, payload string) {
	// Validate input data before converting it into runtime state.
	if r == nil || r.coreManager == nil {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == "" || !gjson.Valid(payload) {
		return
	}

	authIndex := strings.TrimSpace(gjson.Get(payload, "auth_index").String())
	if authIndex == "" {
		return
	}

	provider := strings.TrimSpace(gjson.Get(payload, "provider").String())
	model := strings.TrimSpace(gjson.Get(payload, "model").String())
	if coreauth.CanonicalModelID(model) == "" {
		return
	}

	statusCode := int(gjson.Get(payload, "fail.status_code").Int())
	if statusCode <= 0 {
		if gjson.Get(payload, "failed").Bool() {
			statusCode = 500
		} else {
			statusCode = 200
		}
	}
	body := gjson.Get(payload, "fail.body").String()
	headers := parseResponseHeaders(payload)

	result := coreauth.NewUsageResultWithHeaders(authIndex, provider, model, statusCode, body, headers)
	result.AccessTokenSHA256 = strings.TrimSpace(gjson.Get(payload, "access_token_sha256").String())
	r.coreManager.MarkResult(ctx, result)
}

// parseResponseHeaders reads a usage payload's header fields into a single
// merged http.Header, so callers see Claude/codex rate-limit hints
// regardless of which of the two wire shapes actually survived sanitizing.
//
// "response_headers" is the node's raw http.Header marshal -- keys are
// header names and values are JSON arrays (Go's encoding/json shape for
// map[string][]string), e.g.
// {"Anthropic-Ratelimit-Unified-Status":["rejected"]} -- never a bare
// string, even for a single value. It is correct to read if it is ever
// present, but internal/cluster/quota_ingestion.go's
// sanitizeUsageQuotaHeaders unconditionally deletes this field from every
// payload before RecordUsagePayload ever sees it (it strips credential
// material) -- so in production this branch is normally empty and the real
// data arrives via quota_headers below.
//
// "quota_headers" is what sanitizeUsageQuotaHeaders re-attaches for a
// provider it recognizes (today: codex, claude): a FLAT map[string]string
// of that provider's allowlisted headers (see the `filtered` map in
// sanitizeUsageQuotaHeaders) -- a different shape from response_headers, so
// it is parsed separately below rather than through the array-unwrapping
// path.
//
// The two are merged; response_headers wins on a key collision (it is a
// stronger signal -- an unsanitized full header set -- on the rare path
// where both are present). Returns nil when neither field is present or
// usable, so callers fall back to body-only parsing.
func parseResponseHeaders(payload string) http.Header {
	headers := make(http.Header)
	parseResponseHeadersArrayShape(payload, headers)
	parseResponseHeadersFlatShape(payload, headers)
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// parseResponseHeadersArrayShape reads "response_headers" (JSON-array
// values) into headers, in place.
func parseResponseHeadersArrayShape(payload string, headers http.Header) {
	node := gjson.Get(payload, "response_headers")
	if !node.Exists() || !node.IsObject() {
		return
	}
	node.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			return true
		}
		if value.IsArray() {
			for _, item := range value.Array() {
				v := item.String()
				if v != "" {
					headers.Add(name, v)
				}
			}
			return true
		}
		// Defensive fallback: accept a bare string too, in case a
		// non-standard producer ever sends a single unwrapped value.
		v := strings.TrimSpace(value.String())
		if v != "" {
			headers.Add(name, v)
		}
		return true
	})
}

// parseResponseHeadersFlatShape reads "quota_headers" (flat string values
// -- see sanitizeUsageQuotaHeaders' `filtered` map) into headers, in place.
// Skips any key response_headers already populated, so that field keeps
// precedence on a collision.
func parseResponseHeadersFlatShape(payload string, headers http.Header) {
	node := gjson.Get(payload, "quota_headers")
	if !node.Exists() || !node.IsObject() {
		return
	}
	node.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			return true
		}
		if len(headers.Values(name)) > 0 {
			return true
		}
		v := strings.TrimSpace(value.String())
		if v != "" {
			headers.Add(name, v)
		}
		return true
	})
}
