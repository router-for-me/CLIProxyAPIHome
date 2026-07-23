package respserver

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestServerAppliesHotInFlightLimits(t *testing.T) {
	server := New("", nil)
	initial := server.currentInFlightLimits()

	updatedConfig := config.DefaultCredentialInFlightConfig()
	updatedConfig.MaxDetails = 7
	updatedConfig.MaxStringBytes = 128
	if errApply := server.ApplyInFlightConfig(updatedConfig); errApply != nil {
		t.Fatalf("ApplyInFlightConfig() error = %v", errApply)
	}
	updated := server.currentInFlightLimits()
	if updated.MaxDetails != 7 || updated.MaxStringBytes != 128 {
		t.Fatalf("updated limits = %#v", updated)
	}

	invalidConfig := updatedConfig
	invalidConfig.MaxPartCount = config.DefaultInFlightMaxPartCount + 1
	if errApply := server.ApplyInFlightConfig(invalidConfig); errApply == nil {
		t.Fatal("ApplyInFlightConfig() error = nil, want validation error")
	}
	if retained := server.currentInFlightLimits(); retained != updated {
		t.Fatalf("limits after invalid config = %#v, want %#v", retained, updated)
	}
	if initial == updated {
		t.Fatal("initial limits unexpectedly match updated limits")
	}
}

func TestServerNewUsesDefaultInFlightLimits(t *testing.T) {
	if got, want := New("", nil).currentInFlightLimits(), cluster.DefaultInFlightLimits(); got != want {
		t.Fatalf("default limits = %#v, want %#v", got, want)
	}
}
