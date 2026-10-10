package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/service"
)

func replaceServiceFactory(t *testing.T, factory func() (*service.Manager, error)) {
	t.Helper()
	old := workerServiceFactory
	workerServiceFactory = factory
	t.Cleanup(func() { workerServiceFactory = old })
}

func TestServiceInvalidInputBeforeConfiguration(t *testing.T) {
	called := false
	replaceServiceFactory(t, func() (*service.Manager, error) {
		called = true
		return nil, errors.New("must not open selected config")
	})
	for _, args := range [][]string{
		{"install", "latest", "--archive", "a", "--checksums", "c"},
		{"update", "../0.8.0", "--archive", "a", "--checksums", "c"},
		{"switch-back", "v0.8.0"},
		{"install", "0.8.0"},
		{"restart", "extra"},
	} {
		cmd := newWorkerServiceCommand()
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Fatal("invalid service input accepted", args)
		}
		if called {
			t.Fatal("invalid input touched configuration", args)
		}
	}
}

func TestServiceLingerDiagnosisAndReadOnlyStatus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux service prerequisites")
	}
	dir := t.TempDir()
	mutations := false
	m := &service.Manager{Runtime: service.Runtime{Root: filepath.Join(dir, "runtime")}, UnitDir: filepath.Join(dir, "units"), Catalog: filepath.Join(dir, "catalog"), PathEnv: "/usr/bin", UID: "1001", User: "fixture"}
	m.Run = func(ctx context.Context, command string, args ...string) (string, error) {
		if command == "loginctl" {
			return "no\n", nil
		}
		if command == "systemctl" && len(args) > 1 && args[1] == "show" {
			return "LoadState=not-found\n", nil
		}
		mutations = true
		return "", errors.New("refusing live mutation")
	}
	replaceServiceFactory(t, func() (*service.Manager, error) { return m, nil })
	cmd := newWorkerServiceCommand()
	cmd.SetArgs([]string{"install", "0.8.0", "--archive", "not-read", "--checksums", "not-read"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "linger-disabled") || !strings.Contains(err.Error(), "sudo loginctl enable-linger 'fixture'") {
		t.Fatal("missing privileged diagnostic", err)
	}
	var output bytes.Buffer
	cmd = newWorkerServiceCommand()
	cmd.SetArgs([]string{"status", "--json"})
	cmd.SetOut(&output)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("uninstalled/linger-disabled status reported healthy")
	}
	if !strings.Contains(output.String(), `"linger-disabled"`) || !strings.Contains(output.String(), `"protocol_hint"`) || !strings.Contains(output.String(), "protocol_mismatch") {
		t.Fatal("status failed to explain compatibility/prerequisite state", output.String())
	}
	if mutations {
		t.Fatal("status or prerequisite check mutated a service/account")
	}
}

func TestServiceCommandRegistered(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"service", "switch-back"})
	if err != nil || command.Name() != "switch-back" {
		t.Fatal("service command not reachable", err)
	}
}
