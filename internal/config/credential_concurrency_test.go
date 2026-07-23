package config

import (
	"testing"
	"time"
)

func TestCredentialConcurrencyDefaults(t *testing.T) {
	got := DefaultCredentialConcurrencyConfig()
	if got.LifecycleConfigRevision != 0 || got.ObservationBarrierRevision != 0 {
		t.Fatalf("default revisions = %d, %d, want 0, 0", got.LifecycleConfigRevision, got.ObservationBarrierRevision)
	}
	if got.CPAHeartbeatTimeout != 3*time.Second || got.CPACancelBound != 5*time.Second || got.ReclaimGrace != 5*time.Second || got.CleanupInterval != 5*time.Second {
		t.Fatalf("default lifecycle config = %#v", got)
	}
	if got.ReleaseFlushInterval != "250ms" || got.ReleaseMaxBackoff != "2s" || got.BusyRetryMin != "250ms" || got.BusyRetryMax != "1s" || got.MaxLimit != 1_000_000 {
		t.Fatalf("default limiter config = %#v", got)
	}
}

func TestValidateCredentialConcurrencyConfig(t *testing.T) {
	valid := DefaultCredentialConcurrencyConfig()
	if errValidate := ValidateCredentialConcurrencyConfig(valid); errValidate != nil {
		t.Fatalf("ValidateCredentialConcurrencyConfig() error = %v", errValidate)
	}

	tests := []struct {
		name   string
		mutate func(*CredentialConcurrencyConfig)
	}{
		{
			name: "non-positive release flush interval",
			mutate: func(cfg *CredentialConcurrencyConfig) {
				cfg.ReleaseFlushInterval = "0s"
			},
		},
		{
			name: "release max backoff below flush interval",
			mutate: func(cfg *CredentialConcurrencyConfig) {
				cfg.ReleaseMaxBackoff = "100ms"
			},
		},
		{
			name: "busy max retry below minimum",
			mutate: func(cfg *CredentialConcurrencyConfig) {
				cfg.BusyRetryMax = "100ms"
			},
		},
		{
			name: "fractional busy retry bounds",
			mutate: func(cfg *CredentialConcurrencyConfig) {
				cfg.BusyRetryMin = "1.5ms"
				cfg.BusyRetryMax = "2.5ms"
			},
		},
		{
			name: "sub-millisecond busy retry bound",
			mutate: func(cfg *CredentialConcurrencyConfig) {
				cfg.BusyRetryMin = "500us"
				cfg.BusyRetryMax = "1ms"
			},
		},
		{
			name: "limit exceeds maximum",
			mutate: func(cfg *CredentialConcurrencyConfig) {
				cfg.MaxLimit = 1_000_001
			},
		},
	}
	exactMilliseconds := valid
	exactMilliseconds.BusyRetryMin = "1ms"
	exactMilliseconds.BusyRetryMax = "1ms"
	if errValidate := ValidateCredentialConcurrencyConfig(exactMilliseconds); errValidate != nil {
		t.Fatalf("ValidateCredentialConcurrencyConfig() exact millisecond error = %v", errValidate)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if errValidate := ValidateCredentialConcurrencyConfig(cfg); errValidate == nil {
				t.Fatal("ValidateCredentialConcurrencyConfig() error = nil, want validation error")
			}
		})
	}
}
