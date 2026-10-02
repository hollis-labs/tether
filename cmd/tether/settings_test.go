package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSettingsEffectiveRetention(t *testing.T) {
	previous := catalogPath
	t.Cleanup(func() { catalogPath = previous })
	for _, tc := range []struct {
		name, body string
		enabled    bool
		days       int
	}{
		{"default", "", true, 90},
		{"custom", "    days: 7\n", true, 7},
		{"disabled", "    enabled: false\n", false, 0},
		{"zero", "    days: 0\n", false, 0},
		{"negative", "    days: -1\n", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTetherPaths(t, t.TempDir())
			body := "version: 0.1.0\n"
			if tc.body != "" {
				body += "daemon:\n  events_retention:\n" + tc.body
			}
			catalogPath = writeCatalog(t, t.TempDir(), body)
			var out bytes.Buffer
			cmd := settingsCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"--json"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var got struct {
				Retention struct {
					Enabled bool     `json:"enabled"`
					Days    int      `json:"days"`
					Tables  []string `json:"tables"`
				} `json:"daemon.events_retention"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Retention.Enabled != tc.enabled || got.Retention.Days != tc.days {
				t.Fatalf("effective retention = %+v, want enabled=%v days=%d", got.Retention, tc.enabled, tc.days)
			}
			if !slices.Contains(got.Retention.Tables, "a2a_tasks") {
				t.Fatalf("retention tables omit A2A: %+v", got.Retention.Tables)
			}
			catCheck, cat := checkCatalog(catalogPath)
			if cat == nil {
				t.Fatalf("catalog: %+v", catCheck)
			}
			message := retentionMessage(cat.Global.Daemon.EventsRetention)
			if !strings.Contains(message, "terminal a2a_tasks") || !strings.Contains(message, "restart applies changes") || (!tc.enabled && !strings.Contains(message, "disabled")) {
				t.Fatalf("doctor retention message: %s", message)
			}
		})
	}
}
