package config

import (
	"gopkg.in/yaml.v3"
	"path/filepath"
	"testing"
)

func TestMCPUpstreamOwnership(t *testing.T) {
	for _, value := range []string{"", "legacy_proxy", "daemon", "invalid"} {
		got, err := (DaemonConfig{MCPUpstreams: value}).MCPUpstreamOwnership()
		if value == "invalid" {
			if err == nil {
				t.Fatal("unknown ownership accepted")
			}
			continue
		}
		want := value
		if want == "" {
			want = MCPUpstreamsLegacy
		}
		if err != nil || got != want {
			t.Fatalf("%q = %q, %v", value, got, err)
		}
	}
}

func TestOwnershipChangesOnlyAtNewLaunchBoundary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	write(t, filepath.Join(root, "global.yaml"), "daemon:\n  mcp_upstream_ownership: daemon\n")
	got, err := ReadMCPUpstreamOwnership(root, DaemonConfig{})
	if err != nil || got != MCPUpstreamsDaemon {
		t.Fatal(got, err)
	}
	write(t, filepath.Join(root, "global.yaml"), "daemon:\n  mcp_upstream_ownership: unknown\n")
	if _, err := ReadMCPUpstreamOwnership(root, DaemonConfig{}); err == nil {
		t.Fatal("unknown ownership silently fell back")
	}
	// Shared loading remains available to stop/status and other clients.
	if _, err := Load(root); err != nil {
		t.Fatal("shared loader rejected launch-only ownership", err)
	}
}

func TestOwnershipExplicitMalformedValuesDoNotBecomeLegacy(t *testing.T) {
	for _, value := range []string{"null", "", "\"\"", "\"   \"", "[]", "{}", "true", "123"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			body := "daemon:\n  mcp_upstream_ownership: " + value + "\n  listen_addr: unix:/kept.sock\n"
			write(t, filepath.Join(root, "global.yaml"), body)
			var cfg struct {
				Daemon DaemonConfig `yaml:"daemon"`
			}
			if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
				t.Fatal("shared loading must remain available", err)
			}
			if cfg.Daemon.ListenAddr != "unix:/kept.sock" {
				t.Fatal("other daemon settings lost")
			}
			if _, err := cfg.Daemon.MCPUpstreamOwnership(); err == nil {
				t.Fatal("malformed ownership accepted")
			}
			if _, err := ReadMCPUpstreamOwnership(root, DaemonConfig{}); err == nil {
				t.Fatal("launch silently became legacy")
			}
			if _, err := Load(root); err != nil {
				t.Fatal("shared catalog loading rejected ownership value", err)
			}
		})
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "global.yaml"), "daemon:\n  listen_addr: unix:/kept.sock\n")
	if got, err := ReadMCPUpstreamOwnership(root, DaemonConfig{}); err != nil || got != MCPUpstreamsLegacy {
		t.Fatal(got, err)
	}
}

func TestOwnershipUsesYamlMergedAndAliasedSelectors(t *testing.T) {
	for _, body := range []string{
		"defaults: &defaults\n  mcp_upstream_ownership: daemon\ndaemon:\n  <<: *defaults\n",
		"selected: &selected daemon\ndaemon:\n  mcp_upstream_ownership: *selected\n",
	} {
		root := t.TempDir()
		write(t, filepath.Join(root, "global.yaml"), body)
		got, err := ReadMCPUpstreamOwnership(root, DaemonConfig{})
		if err != nil || got != MCPUpstreamsDaemon {
			t.Fatalf("merged/aliased selector: %q %v", got, err)
		}
	}
}
