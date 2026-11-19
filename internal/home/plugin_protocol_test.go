package home

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSchedulerProtocolAdvertisementIsHostOwned(t *testing.T) {
	for _, available := range []bool{false, true} {
		payload, errSanitize := sanitizeConfigYAMLForDownstream([]byte("home-scheduler-protocol: 99\n"), available)
		if errSanitize != nil {
			t.Fatal(errSanitize)
		}
		var decoded struct {
			Protocol int `yaml:"home-scheduler-protocol"`
		}
		if errDecode := yaml.Unmarshal(payload, &decoded); errDecode != nil {
			t.Fatal(errDecode)
		}
		want := 0
		if available {
			want = 1
		}
		if decoded.Protocol != want {
			t.Fatalf("available=%t protocol=%d, want %d", available, decoded.Protocol, want)
		}
	}
}
