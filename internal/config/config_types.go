package config

type RequestScopedErrorRule struct {
	// Status matches the HTTP status code of the upstream response (e.g. 400).
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// Match matches substrings in the upstream error body.
	Match []string `yaml:"match,omitempty" json:"match,omitempty"`
	// MatchRegexr matches regular expressions in the upstream error body.
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	// Action specifies the handling behavior: "stop", "stop-and-cooldown", "continue", "continue-and-cooldown".
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}
