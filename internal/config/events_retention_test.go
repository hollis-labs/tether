package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults apply even when the config section is absent; explicit false or
// nonpositive days disables retention.
func TestEventsRetentionWindow(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"absent", `shutdown_timeout: 10s`, 90 * day},
		{"days without enabled", "events_retention:\n  days: 30", 30 * day},
		{"explicit disabled", "events_retention:\n  enabled: false", 0},
		{"days only, zero", "events_retention:\n  days: 0", 0},
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
