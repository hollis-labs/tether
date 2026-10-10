package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func remoteTargetFixture(t *testing.T, target string) {
	t.Helper()
	oldTarget, oldCatalog, oldToken := remoteTarget, catalogPath, tokenFilePath
	flag := rootCmd.PersistentFlags().Lookup("target")
	oldChanged := flag.Changed
	t.Cleanup(func() {
		remoteTarget, catalogPath, tokenFilePath = oldTarget, oldCatalog, oldToken
		flag.Changed = oldChanged
	})
	remoteTarget, tokenFilePath, flag.Changed = target, "", false
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("TETHER_TOKEN", "device-test")
	catalogPath = filepath.Join(root, "catalog")
	if err := os.Mkdir(catalogPath, 0700); err != nil {
		t.Fatal(err)
	}
	// Any catalog fallback would fail before returning the intended API result.
	if err := os.WriteFile(filepath.Join(catalogPath, "global.yaml"), []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteTargetGuardRunsBeforeHostHandlers(t *testing.T) {
	remoteTargetFixture(t, "tcp:127.0.0.1:8099")
	if rootCmd.PersistentPreRunE == nil {
		t.Fatal("remote guard not installed")
	}
	ran := false
	root := &cobra.Command{Use: "tether", PersistentPreRunE: rootCmd.PersistentPreRunE}
	root.AddCommand(&cobra.Command{Use: "doctor", RunE: func(*cobra.Command, []string) error { ran = true; return nil }})
	root.SetArgs([]string{"doctor"})
	root.SilenceErrors, root.SilenceUsage = true, true
	if err := root.Execute(); err == nil || ran {
		t.Fatalf("host handler executed: ran=%v err=%v", ran, err)
	}
	for _, path := range [][]string{{"workspaces", "prune"}, {"projects", "list"}, {"agents", "edit"}, {"resolve"}, {"path"}, {"settings", "retention"}, {"doctor"}, {"mcp"}, {"acp"}, {"daemon", "start"}, {"serve"}, {"service", "install"}, {"boot"}} {
		cmd, _, err := rootCmd.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := guardRemoteCommand(cmd, nil); err == nil {
			t.Fatalf("host command admitted: %s", cmd.CommandPath())
		}
	}
	cmd, _, _ := rootCmd.Find([]string{"sessions", "get"})
	if err := guardRemoteCommand(cmd, nil); err != nil {
		t.Fatal(err)
	}
	remoteTarget = ""
	if err := guardRemoteCommand(cmd, nil); err != nil {
		t.Fatalf("local CLI changed: %v", err)
	}
	rootCmd.PersistentFlags().Lookup("target").Changed = true
	if err := guardRemoteCommand(cmd, nil); err == nil {
		t.Fatal("explicit empty target silently became local")
	}
}

func TestRemoteTargetRejectsInvalidTransportAndImplicitCredentials(t *testing.T) {
	remoteTargetFixture(t, "tcp:127.0.0.1:8099")
	for _, target := range []string{"", "unix:/tmp/worker.sock", "http://localhost:123", "tcp:192.0.2.1:123", "tcp:localhost:0", "tcp:localhost:65536", "tcp:localhost:http"} {
		if err := validateRemoteTarget(target); err == nil {
			t.Fatalf("unsafe target admitted: %q", target)
		}
	}
	t.Setenv("TETHER_TOKEN", "")
	if _, err := newDaemonClient(catalogPath); err == nil || !strings.Contains(err.Error(), "explicit device credential") {
		t.Fatalf("implicit credential/catalog fallback: %v", err)
	}
	if _, err := openStoreReadOnly(catalogPath); err == nil || !strings.Contains(err.Error(), "unavailable with --target") {
		t.Fatalf("remote store opened: %v", err)
	}
	cmd, _, _ := rootCmd.Find([]string{"launch"})
	flag := cmd.Flags().Lookup("agent-file")
	oldChanged := flag.Changed
	flag.Changed = true
	defer func() { flag.Changed = oldChanged }()
	if err := guardRemoteCommand(cmd, nil); err == nil {
		t.Fatal("implicit daemon host launch input admitted")
	}
}
