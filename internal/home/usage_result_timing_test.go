package home

import (
	"fmt"
	"testing"
	"time"
)

func TestUsagePayloadObservedAtValidatesTiming(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		timestamp string
		latency   string
		want      time.Time
		valid     bool
	}{
		{name: "legacy untimed", latency: "0", valid: true},
		{name: "invalid timestamp", timestamp: "invalid", latency: "0"},
		{name: "missing latency", timestamp: now.Format(time.RFC3339Nano), want: now, valid: true},
		{name: "completion", timestamp: now.Add(-time.Second).Format(time.RFC3339Nano), latency: "500", want: now.Add(-500 * time.Millisecond), valid: true},
		{name: "skew boundary", timestamp: now.Add(usageFeedbackMaxClockSkew).Format(time.RFC3339Nano), latency: "0", want: now.Add(usageFeedbackMaxClockSkew), valid: true},
		{name: "future clock", timestamp: now.Add(24 * time.Hour).Format(time.RFC3339Nano), latency: "0"},
		{name: "future completion", timestamp: now.Format(time.RFC3339Nano), latency: "300001"},
		{name: "negative latency", timestamp: now.Format(time.RFC3339Nano), latency: "-1"},
		{name: "duration overflow", timestamp: now.Format(time.RFC3339Nano), latency: "9223372036854775807"},
		{name: "integer overflow", timestamp: now.Format(time.RFC3339Nano), latency: "18446744073709551616"},
		{name: "fractional latency", timestamp: now.Format(time.RFC3339Nano), latency: "0.5"},
		{name: "old redelivery", timestamp: now.Add(-24 * time.Hour).Format(time.RFC3339Nano), latency: "500", want: now.Add(-24*time.Hour + 500*time.Millisecond), valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := "{}"
			if test.timestamp != "" {
				payload = fmt.Sprintf(`{"timestamp":%q}`, test.timestamp)
			}
			if test.latency != "" {
				payload = payload[:len(payload)-1]
				if len(payload) > 1 {
					payload += ","
				}
				payload += `"latency_ms":` + test.latency + "}"
			}
			got, valid := usagePayloadObservedAt(payload, now)
			if valid != test.valid || !got.Equal(test.want) {
				t.Fatalf("observedAt = %v, valid = %v; want %v, %v", got, valid, test.want, test.valid)
			}
			if valid {
				redelivered, validRedelivery := usagePayloadObservedAt(payload, now.Add(time.Hour))
				if !validRedelivery || !redelivered.Equal(got) {
					t.Fatalf("redelivery changed accepted event time: %v, %v", redelivered, validRedelivery)
				}
			}
		})
	}
}
