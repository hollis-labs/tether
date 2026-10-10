package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/sshenroll"
)

type enrollmentTrapRemote struct{ t *testing.T }

func (r enrollmentTrapRemote) Preflight(context.Context, sshenroll.Options) (sshenroll.Preflight, error) {
	r.t.Fatal("invalid CLI input reached SSH")
	return sshenroll.Preflight{}, nil
}
func (r enrollmentTrapRemote) Install(context.Context, sshenroll.WorkerRequest, *sshenroll.Artifact) (sshenroll.WorkerState, error) {
	r.t.Fatal("unexpected worker change")
	return sshenroll.WorkerState{}, nil
}
func (r enrollmentTrapRemote) Inspect(context.Context, sshenroll.WorkerRequest) (sshenroll.WorkerState, error) {
	r.t.Fatal("unexpected worker inspection")
	return sshenroll.WorkerState{}, nil
}
func (r enrollmentTrapRemote) Grant(context.Context, sshenroll.WorkerRequest) (identity.IssuedGrant, error) {
	r.t.Fatal("unexpected grant")
	return identity.IssuedGrant{}, nil
}
func (r enrollmentTrapRemote) Forward(context.Context, int) (sshenroll.Tunnel, error) {
	r.t.Fatal("unexpected forward")
	return nil, nil
}
func (r enrollmentTrapRemote) Rollback(context.Context, sshenroll.WorkerRequest) error {
	r.t.Fatal("unexpected rollback")
	return nil
}

func TestEnvironmentCLIRefusesInjectionBeforeSSH(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "receipt")
	cmd := newEnvironmentCommand(func(string) *sshenroll.Manager { return &sshenroll.Manager{Remote: enrollmentTrapRemote{t}} })
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"add", "worker;injected", "--authority", "worker", "--version", "0.8.0", "--archive", "/synthetic/archive", "--checksums", "/synthetic/checksums", "--provider", "codex", "--receipt-dir", directory})
	if err := cmd.Execute(); err == nil {
		t.Fatal("CLI target injection accepted")
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatal("invalid input created receipt")
	}
}
func TestEnvironmentPrivateHelperMalformedInputIsSanitized(t *testing.T) {
	cmd := newWorkerEnrollmentCommand()
	cmd.SetIn(strings.NewReader("synthetic private malformed input"))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"install"})
	err := cmd.Execute()
	if err == nil || strings.Contains(err.Error()+output.String(), "synthetic private") {
		t.Fatal("private malformed input echoed")
	}
}

func TestEnvironmentPrivateHelperRejectsActionBeforeStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cmd := newWorkerEnrollmentCommand()
	cmd.SetIn(strings.NewReader("synthetic private input"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"unsupported"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, err := os.Lstat(filepath.Join(home, ".tether")); !os.IsNotExist(err) {
		t.Fatal("unknown action wrote worker state")
	}
}
