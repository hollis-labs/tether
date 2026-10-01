package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveMCPProxyConfigOnlyRequiresProxy(t *testing.T) {
	_, _, err := resolveMCPProxyConfig(false, false, "", "clockwork", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only requires --proxy") {
		t.Fatalf("expected --only requires --proxy error, got %v", err)
	}
}

func TestResolveMCPProxyConfigOnlyRequiresServerList(t *testing.T) {
	_, _, err := resolveMCPProxyConfig(true, false, "", " , ", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only requires a non-empty") {
		t.Fatalf("expected non-empty --only list error, got %v", err)
	}

	_, _, err = resolveMCPProxyConfig(true, false, "", "", "vanta", true)
	if err == nil || !strings.Contains(err.Error(), "--only requires a non-empty") {
		t.Fatalf("expected --only to ignore env fallback, got %v", err)
	}
}

func TestResolveMCPProxyConfigOnlyRejectsConflicts(t *testing.T) {
	_, _, err := resolveMCPProxyConfig(true, true, "", "clockwork", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only cannot be combined with --broker") {
		t.Fatalf("expected --only/--broker conflict, got %v", err)
	}

	_, _, err = resolveMCPProxyConfig(true, false, "vanta", "clockwork", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only cannot be combined with --servers") {
		t.Fatalf("expected --only/--servers conflict, got %v", err)
	}
}

func TestResolveMCPProxyConfigServersFallbackAndOnly(t *testing.T) {
	filter, only, err := resolveMCPProxyConfig(true, false, "", "", "vanta, clockwork", false)
	if err != nil {
		t.Fatalf("resolve servers env: %v", err)
	}
	if only {
		t.Fatal("expected --servers/env mode, got only mode")
	}
	if want := []string{"vanta", "clockwork"}; !reflect.DeepEqual(filter, want) {
		t.Fatalf("env filter = %v, want %v", filter, want)
	}

	filter, only, err = resolveMCPProxyConfig(true, false, "", "cerberus,clockwork", "vanta", true)
	if err != nil {
		t.Fatalf("resolve only: %v", err)
	}
	if !only {
		t.Fatal("expected only mode")
	}
	if want := []string{"cerberus", "clockwork"}; !reflect.DeepEqual(filter, want) {
		t.Fatalf("only filter = %v, want %v", filter, want)
	}
}

// A planted `tether mcp --daemon-only` refuses to start without a reachable
// daemon, with an error the agent's MCP status shows, and never opens the
// state database on the way (CW-20261001-0173).
func TestRunMCPDaemonOnly_FailsClosedWithoutADaemon(t *testing.T) {
	state := t.TempDir()
	catalog := t.TempDir()
	global := "version: 1.0.0\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(state, "tether.db") + "\n" +
		"daemon:\n  listen_addr: unix:" + filepath.Join(state, "tetherd.sock") + "\n  shutdown_timeout: 1s\n"
	if err := os.WriteFile(filepath.Join(catalog, "global.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	old := catalogPath
	catalogPath = catalog
	t.Cleanup(func() { catalogPath = old })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	listenAddr, _ := resolveDaemonAddr()
	if listenAddr == "" {
		t.Fatal("the fixture catalog's daemon address did not resolve")
	}
	for name, addr := range map[string]string{"no socket": listenAddr, "no address": ""} {
		cmd.SilenceUsage = false
		err := runMCPDaemonOnly(cmd, addr, "tok", nil, nil, false)
		if !cmd.SilenceUsage {
			t.Errorf("%s: usage is not silenced, so the one-line reason is followed by cobra's usage text", name)
		}
		if err == nil || !strings.Contains(err.Error(), "tether daemon unreachable; tether tools unavailable") {
			t.Errorf("%s: err = %v, want the daemon-unreachable message", name, err)
		}
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the state directory gained %v: a daemon-only server must not create the database", names)
	}
}
