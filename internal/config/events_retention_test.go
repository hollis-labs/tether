package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Events retention is off unless enabled; enabled without days is D-50's 90
// days; 0 or negative days disables it (CW-20260930-0008).
func TestEventsRetentionWindow(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"absent", `shutdown_timeout: 10s`, 0},
		{"days without enabled", "events_retention:\n  days: 30", 0},
		{"enabled, default window", "events_retention:\n  enabled: true", 90 * day},
		{"enabled, 30 days", "events_retention:\n  enabled: true\n  days: 30", 30 * day},
		{"enabled, 0 days", "events_retention:\n  enabled: true\n  days: 0", 0},
		{"enabled, negative days", "events_retention:\n  enabled: true\n  days: -1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d DaemonConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &d); err != nil {
				t.Fatal(err)
			}
			if got := d.EventsRetention.Window(); got != tc.want {
				t.Fatalf("window = %s; want %s", got, tc.want)
			}
		})
	}
}
