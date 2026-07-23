package auth

import "testing"

func TestConcurrencyCandidateExcludedUsesLimiterModelKeys(t *testing.T) {
	opts := Options{
		Metadata: map[string]any{
			ExcludedConcurrencyCandidatesMetadataKey: []ExcludedConcurrencyCandidate{{
				CredentialID: "credential-1",
				Model:        "gpt(high)",
			}},
		},
	}
	if !concurrencyCandidateExcluded(opts, "credential-1", "GPT") {
		t.Fatal("recognized reasoning suffix did not exclude the canonical model")
	}

	opts.Metadata[ExcludedConcurrencyCandidatesMetadataKey] = []ExcludedConcurrencyCandidate{{
		CredentialID: "credential-1",
		Model:        "gpt(custom)",
	}}
	if concurrencyCandidateExcluded(opts, "credential-1", "gpt") {
		t.Fatal("unknown suffix incorrectly excluded a distinct limiter model")
	}
	if !concurrencyCandidateExcluded(opts, "credential-1", "gpt(custom)") {
		t.Fatal("unknown suffix did not exclude its exact limiter model")
	}
}
