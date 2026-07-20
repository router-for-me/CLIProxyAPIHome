package config

import (
	"fmt"
	"net"
	"strings"
)

// NormalizeTrustedProxies trims, removes empty values, and de-duplicates the
// explicit reverse-proxy allowlist while preserving its configured order.
func (cfg *Config) NormalizeTrustedProxies() {
	if cfg == nil || len(cfg.TrustedProxies) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(cfg.TrustedProxies))
	normalized := make([]string, 0, len(cfg.TrustedProxies))
	for _, raw := range cfg.TrustedProxies {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	cfg.TrustedProxies = normalized
}

// ValidateTrustedProxies accepts only explicit IP addresses and CIDRs, and
// rejects trust-all networks that would let direct clients spoof their source.
func ValidateTrustedProxies(values []string) error {
	for _, value := range values {
		if strings.Contains(value, "/") {
			_, network, errCIDR := net.ParseCIDR(value)
			if errCIDR != nil {
				return fmt.Errorf("trusted proxy %q is invalid: %w", value, errCIDR)
			}
			if trustsEveryAddressInIPFamily(network) {
				return fmt.Errorf("trusted proxy %q must not trust every address", value)
			}
			continue
		}
		if net.ParseIP(value) == nil {
			return fmt.Errorf("trusted proxy %q is not an IP address", value)
		}
	}
	return nil
}

func trustsEveryAddressInIPFamily(network *net.IPNet) bool {
	if network == nil {
		return false
	}
	trustsEveryIPv4 := network.Contains(net.IPv4zero) && network.Contains(net.IPv4bcast)
	lastIPv6 := net.IP{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	trustsEveryIPv6 := network.Contains(net.IPv6zero) && network.Contains(lastIPv6)
	return trustsEveryIPv4 || trustsEveryIPv6
}
