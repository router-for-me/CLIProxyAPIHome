package proxyutil

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SameOriginRedirectPolicy prevents credentials in headers or bodies from leaving
// the original origin, while preserving an existing client's redirect policy.
func SameOriginRedirectPolicy(previous func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if len(via) == 0 || request.URL.User != nil || !sameOrigin(request.URL, via[0].URL) {
			return errors.New("redirect to a different origin is not allowed")
		}
		if previous != nil {
			return previous(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) && originPort(left) == originPort(right)
}

func originPort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	switch strings.ToLower(target.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}
