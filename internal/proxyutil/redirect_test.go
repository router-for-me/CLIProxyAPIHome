package proxyutil

import (
	"errors"
	"net/http"
	"testing"
)

func TestSameOriginRedirectPolicy(t *testing.T) {
	for _, test := range []struct {
		name, origin, target string
		allowed              bool
	}{
		{"same origin", "https://example.com/a", "https://example.com/b", true},
		{"default https port", "https://example.com/a", "https://EXAMPLE.com:443/b", true},
		{"default http port", "http://example.com/a", "http://example.com:80/b", true},
		{"different host", "https://example.com/a", "https://other.example/b", false},
		{"subdomain", "https://example.com/a", "https://login.example.com/b", false},
		{"different port", "https://example.com/a", "https://example.com:8443/b", false},
		{"https downgrade", "https://example.com/a", "http://example.com/b", false},
		{"different scheme", "http://example.com/a", "https://example.com/b", false},
		{"userinfo", "https://example.com/a", "https://other:password@example.com/b", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			origin, errOrigin := http.NewRequest(http.MethodPost, test.origin, nil)
			target, errTarget := http.NewRequest(http.MethodPost, test.target, nil)
			if errOrigin != nil || errTarget != nil {
				t.Fatal("invalid test URL")
			}
			errRedirect := SameOriginRedirectPolicy(nil)(target, []*http.Request{origin})
			if (errRedirect == nil) != test.allowed {
				t.Fatalf("redirect allowed=%v, want %v", errRedirect == nil, test.allowed)
			}
		})
	}
}

func TestSameOriginRedirectPolicyPreservesLimitsAndCustomPolicy(t *testing.T) {
	origin, _ := http.NewRequest(http.MethodGet, "https://example.com/a", nil)
	target, _ := http.NewRequest(http.MethodGet, "https://example.com/b", nil)
	if errRedirect := SameOriginRedirectPolicy(nil)(target, make([]*http.Request, 0)); errRedirect == nil {
		t.Fatal("redirect without an original request was accepted")
	}
	via := make([]*http.Request, 10)
	for index := range via {
		via[index] = origin
	}
	if errRedirect := SameOriginRedirectPolicy(nil)(target, via); errRedirect == nil {
		t.Fatal("default redirect limit was bypassed")
	}
	previous := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if errRedirect := SameOriginRedirectPolicy(previous)(target, via[:1]); !errors.Is(errRedirect, http.ErrUseLastResponse) {
		t.Fatal("existing redirect policy was discarded")
	}
}
