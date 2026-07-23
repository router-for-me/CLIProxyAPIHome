package config

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/concurrency"
)

const (
	DefaultCPAHeartbeatTimeout = 3 * time.Second
	DefaultCPACancelBound      = 5 * time.Second
	DefaultReclaimGrace        = 5 * time.Second
	DefaultCleanupInterval     = 5 * time.Second
)

// CredentialConcurrencyConfig controls Home credential concurrency lifecycle behavior.
type CredentialConcurrencyConfig struct {
	LifecycleConfigRevision    int64         `yaml:"lifecycle-config-revision" json:"lifecycle-config-revision"`
	ObservationBarrierRevision int64         `yaml:"observation-barrier-revision" json:"observation-barrier-revision"`
	CPAHeartbeatTimeout        time.Duration `yaml:"cpa-heartbeat-timeout" json:"cpa-heartbeat-timeout"`
	CPACancelBound             time.Duration `yaml:"cpa-cancel-bound" json:"cpa-cancel-bound"`
	ReclaimGrace               time.Duration `yaml:"reclaim-grace" json:"reclaim-grace"`
	CleanupInterval            time.Duration `yaml:"cleanup-interval" json:"cleanup-interval"`
	ReleaseFlushInterval       string        `yaml:"release-flush-interval" json:"release-flush-interval"`
	ReleaseMaxBackoff          string        `yaml:"release-max-backoff" json:"release-max-backoff"`
	BusyRetryMin               string        `yaml:"busy-retry-min" json:"busy-retry-min"`
	BusyRetryMax               string        `yaml:"busy-retry-max" json:"busy-retry-max"`
	MaxLimit                   int64         `yaml:"max-limit" json:"max-limit"`
}

// DefaultCredentialConcurrencyConfig returns the lifecycle and limiter defaults.
func DefaultCredentialConcurrencyConfig() CredentialConcurrencyConfig {
	return CredentialConcurrencyConfig{
		CPAHeartbeatTimeout:  DefaultCPAHeartbeatTimeout,
		CPACancelBound:       DefaultCPACancelBound,
		ReclaimGrace:         DefaultReclaimGrace,
		CleanupInterval:      DefaultCleanupInterval,
		ReleaseFlushInterval: "250ms",
		ReleaseMaxBackoff:    "2s",
		BusyRetryMin:         "250ms",
		BusyRetryMax:         "1s",
		MaxLimit:             concurrency.MaxConfiguredLimit,
	}
}

// ValidateCredentialConcurrencyConfig validates limiter timing and configured bounds.
func ValidateCredentialConcurrencyConfig(cfg CredentialConcurrencyConfig) error {
	releaseFlushInterval, errReleaseFlushInterval := time.ParseDuration(cfg.ReleaseFlushInterval)
	if errReleaseFlushInterval != nil || releaseFlushInterval <= 0 {
		return fmt.Errorf("credential-concurrency.release-flush-interval must be positive")
	}
	releaseMaxBackoff, errReleaseMaxBackoff := time.ParseDuration(cfg.ReleaseMaxBackoff)
	if errReleaseMaxBackoff != nil || releaseMaxBackoff <= 0 {
		return fmt.Errorf("credential-concurrency.release-max-backoff must be positive")
	}
	if releaseMaxBackoff < releaseFlushInterval {
		return fmt.Errorf("credential-concurrency.release-max-backoff must not be less than release-flush-interval")
	}
	busyRetryMin, errBusyRetryMin := time.ParseDuration(cfg.BusyRetryMin)
	if errBusyRetryMin != nil || busyRetryMin <= 0 || busyRetryMin%time.Millisecond != 0 {
		return fmt.Errorf("credential-concurrency.busy-retry-min must be a positive whole millisecond duration")
	}
	busyRetryMax, errBusyRetryMax := time.ParseDuration(cfg.BusyRetryMax)
	if errBusyRetryMax != nil || busyRetryMax <= 0 || busyRetryMax%time.Millisecond != 0 {
		return fmt.Errorf("credential-concurrency.busy-retry-max must be a positive whole millisecond duration")
	}
	if busyRetryMax < busyRetryMin {
		return fmt.Errorf("credential-concurrency.busy-retry-max must not be less than busy-retry-min")
	}
	if cfg.MaxLimit < 1 || cfg.MaxLimit > concurrency.MaxConfiguredLimit {
		return fmt.Errorf("credential-concurrency.max-limit must be between 1 and %d", concurrency.MaxConfiguredLimit)
	}
	return nil
}
