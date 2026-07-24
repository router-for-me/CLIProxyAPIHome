package cluster

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func credentialConcurrencyConfigFixture(cpaHeartbeatTimeout time.Duration) config.CredentialConcurrencyConfig {
	return config.CredentialConcurrencyConfig{
		CPAHeartbeatTimeout:  cpaHeartbeatTimeout,
		CPACancelBound:       5 * time.Second,
		ReclaimGrace:         5 * time.Second,
		CleanupInterval:      5 * time.Second,
		ReleaseFlushInterval: "250ms",
		ReleaseMaxBackoff:    "2s",
		BusyRetryMin:         "250ms",
		BusyRetryMax:         "1s",
		MaxLimit:             1_000_000,
	}
}

func TestCredentialConcurrencyLifecycleFixture(t *testing.T) {
	fixture := struct {
		Defaults config.CredentialConcurrencyConfig
		Invalid  []struct {
			NodeHeartbeatTimeout time.Duration
			Config               config.CredentialConcurrencyConfig
		}
	}{
		Defaults: credentialConcurrencyConfigFixture(3 * time.Second),
		Invalid: []struct {
			NodeHeartbeatTimeout time.Duration
			Config               config.CredentialConcurrencyConfig
		}{
			{NodeHeartbeatTimeout: 3 * time.Second, Config: credentialConcurrencyConfigFixture(3 * time.Second)},
			{NodeHeartbeatTimeout: 20 * time.Second, Config: credentialConcurrencyConfigFixture(0)},
		},
	}
	fixture.Defaults.LifecycleConfigRevision = 1

	expectedDefaults := config.CredentialConcurrencyConfig{
		LifecycleConfigRevision:    1,
		ObservationBarrierRevision: 0,
		CPAHeartbeatTimeout:        3 * time.Second,
		CPACancelBound:             5 * time.Second,
		ReclaimGrace:               5 * time.Second,
		CleanupInterval:            5 * time.Second,
		ReleaseFlushInterval:       "250ms",
		ReleaseMaxBackoff:          "2s",
		BusyRetryMin:               "250ms",
		BusyRetryMax:               "1s",
		MaxLimit:                   1_000_000,
	}
	if fixture.Defaults != expectedDefaults {
		t.Fatalf("defaults = %#v, want %#v", fixture.Defaults, expectedDefaults)
	}

	expectedInvalid := []struct {
		nodeHeartbeatTimeout time.Duration
		config               config.CredentialConcurrencyConfig
	}{
		{
			nodeHeartbeatTimeout: 3 * time.Second,
			config: config.CredentialConcurrencyConfig{
				CPAHeartbeatTimeout:  3 * time.Second,
				CPACancelBound:       5 * time.Second,
				ReclaimGrace:         5 * time.Second,
				CleanupInterval:      5 * time.Second,
				ReleaseFlushInterval: "250ms",
				ReleaseMaxBackoff:    "2s",
				BusyRetryMin:         "250ms",
				BusyRetryMax:         "1s",
				MaxLimit:             1_000_000,
			},
		},
		{
			nodeHeartbeatTimeout: 20 * time.Second,
			config: config.CredentialConcurrencyConfig{
				CPAHeartbeatTimeout:  0,
				CPACancelBound:       5 * time.Second,
				ReclaimGrace:         5 * time.Second,
				CleanupInterval:      5 * time.Second,
				ReleaseFlushInterval: "250ms",
				ReleaseMaxBackoff:    "2s",
				BusyRetryMin:         "250ms",
				BusyRetryMax:         "1s",
				MaxLimit:             1_000_000,
			},
		},
	}
	if len(fixture.Invalid) != len(expectedInvalid) {
		t.Fatalf("invalid fixture count = %d, want %d", len(fixture.Invalid), len(expectedInvalid))
	}
	for index, expected := range expectedInvalid {
		item := fixture.Invalid[index]
		if item.NodeHeartbeatTimeout != expected.nodeHeartbeatTimeout || item.Config != expected.config {
			t.Fatalf("invalid fixture %d = %#v, want node heartbeat timeout %s and config %#v", index, item, expected.nodeHeartbeatTimeout, expected.config)
		}
		if errValidate := ValidateCredentialConcurrencyLifecycle(item.NodeHeartbeatTimeout, item.Config); errValidate == nil {
			t.Fatalf("invalid fixture %d passed", index)
		}
	}
}
