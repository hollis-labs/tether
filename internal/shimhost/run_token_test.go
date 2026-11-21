//go:build linux

package shimhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/runtoken"
)

func TestRuntimeRunTokenLocalDescriptorAndRecovery(t *testing.T) {
	cfg, spec := hostSpec(t)
	spec.Env[0] = "TETHER_TEST_SHIM=run-token-child"
	spec.Env = append(spec.Env, runtoken.TokenEnv+"="+testRuntimeRunToken, runtoken.IDEnv+"=local-execution")
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, err := p.Place(ctx, "private-run", spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopTestPlacement(t, p, r) })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(spec.Cwd, "run-token-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider did not receive token from runtime-only handoff")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := filepath.WalkDir(spec.Cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Type().IsRegular() {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			if strings.Contains(string(b), testRuntimeRunToken) {
				// File delivery is the sole permitted on-disk proof. Descriptors,
				// journals and other placement state must remain proof-free.
				st, e := os.Stat(path)
				parent, pe := os.Stat(filepath.Dir(path))
				if e != nil || pe != nil || filepath.Base(path) != "run.token" || !strings.HasPrefix(filepath.Base(filepath.Dir(path)), "tether-run-token-") || st.Mode().Perm() != 0600 || parent.Mode().Perm() != 0700 {
					t.Fatal("run token persisted outside protected provider file")
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := Descriptor(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtoken.RestoreEnvironment(descriptor.Env, spec.Session); !errors.Is(err, runtoken.ErrHandoff) {
		t.Fatal("inactive durable descriptor replayed a run token")
	}
	recovered, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := recovered.Inspect(ctx, r)
	if err != nil || !inspection.Running {
		t.Fatal("controller recovery could not reattach live runtime")
	}
	again, err := recovered.Place(ctx, "private-run", spec)
	if err != nil || again.HostPID != r.HostPID {
		t.Fatal("recovery resent runtime launch")
	}
}

func TestRuntimeRunTokenSurvivesCallerCancellation(t *testing.T) {
	cfg, spec := hostSpec(t)
	gate := filepath.Join(spec.Cwd, "release-start")
	spec.Env[0] = "TETHER_TEST_SHIM=run-token-child"
	spec.Env = append(spec.Env, runtoken.TokenEnv+"="+testRuntimeRunToken, "TETHER_TEST_RUN_GATE="+gate)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	r, err := p.Place(ctx, "delayed-private-run", spec)
	cancel()
	if err == nil {
		t.Fatal("delayed launch unexpectedly ready")
	}
	t.Cleanup(func() { stopTestPlacement(t, p, r) })
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(spec.Cwd, "run-token-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled caller revoked submitted runtime token")
		}
		time.Sleep(10 * time.Millisecond)
	}
	descriptor, err := Descriptor(r)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := runtoken.RestoreEnvironment(descriptor.Env, spec.Session)
		if errors.Is(err, runtoken.ErrHandoff) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ready runtime retained launcher handoff")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRuntimeRunTokenReleasedAfterVerifiedExit(t *testing.T) {
	cfg, spec := hostSpec(t)
	spec.Env[0] = "TETHER_TEST_SHIM=run-token-child"
	spec.Env = append(spec.Env, runtoken.TokenEnv+"="+testRuntimeRunToken, "TETHER_TEST_RUN_GATE="+filepath.Join(spec.Cwd, "never-start"))
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	r, err := p.Place(ctx, "terminated-private-run", spec)
	cancel()
	if err == nil || r.HostPID == 0 {
		t.Fatal("expected uncertain submitted process")
	}
	process, err := os.FindProcess(r.HostPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	descriptor, err := Descriptor(r)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := runtoken.RestoreEnvironment(descriptor.Env, spec.Session)
		if errors.Is(err, runtoken.ErrHandoff) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("verified exited local process retained handoff")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
