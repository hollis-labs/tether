package main

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"gopkg.in/yaml.v3"
)

func TestDaemonTerminalAttachmentsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		enabled    bool
	}{
		{name: "default off"},
		{name: "explicit off", yaml: "daemon:\n  terminal_attachments:\n    enabled: false\n"},
		{name: "enabled", yaml: "daemon:\n  terminal_attachments:\n    enabled: true\n", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var global config.Global
			if err := yaml.Unmarshal([]byte(tc.yaml), &global); err != nil {
				t.Fatal(err)
			}
			global.Daemon.ListenAddr = "unix:/unused"
			global.Daemon.ShutdownTimeout = "1s"
			cfg, err := daemonConfigFromCatalog(&config.Catalog{Global: global})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TerminalAttachments != tc.enabled {
				t.Fatalf("TerminalAttachments = %t, want %t", cfg.TerminalAttachments, tc.enabled)
			}
			if cfg.ChatWire {
				t.Fatal("terminal flag also enabled chat wire")
			}
		})
	}
}
