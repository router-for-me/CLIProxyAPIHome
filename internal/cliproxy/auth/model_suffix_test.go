package auth

import "testing"

func TestCanonicalModelID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "plain", model: " gpt-5.4 ", want: "gpt-5.4"},
		{name: "suffix", model: " gpt-5.4(high) ", want: "gpt-5.4"},
		{name: "unterminated suffix", model: "gpt-5.4(high", want: "gpt-5.4(high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := CanonicalModelID(test.model); got != test.want {
				t.Fatalf("CanonicalModelID(%q) = %q, want %q", test.model, got, test.want)
			}
		})
	}
}
