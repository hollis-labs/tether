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

const testRuntimeRunToken = "synthetic-local-run-token-never-persist"

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
				t.Fatal("run token persisted in local placement state")
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
