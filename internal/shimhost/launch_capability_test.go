//go:build linux

package shimhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostingCapabilityProbeRefusesUnsupportedKernel(t *testing.T) {
	old := getPeerPIDFD
	t.Cleanup(func() { getPeerPIDFD = old })
	getPeerPIDFD = func(int) (int, error) { return -1, unix.ENOPROTOOPT }
	err := Supported()
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "unsupported" {
		t.Fatalf("capability probe: %v", err)
	}
}

func TestTransientUnitPrefixAndEnvironmentValuesStayOutOfArgv(t *testing.T) {
	cfg, spec := hostSpec(t)
	cfg.Backend = SystemdUser
	cfg.AllowSystemd = true
	cfg.UnitPrefix = "tether-dev-shim-"
	cfg.HostEnv = []string{"HOME=" + cfg.StateDir, "TEST_SECRET=must-not-be-in-argv"}
	called := false
	cfg.Command = func(_ context.Context, args []string) ([]byte, error) {
		called = true
		if args[0] != "systemd-run" {
			t.Fatalf("unexpected command: %s", args[0])
		}
		all := strings.Join(args, " ")
		if !strings.Contains(all, "--unit=tether-dev-shim-") || !strings.Contains(all, "--setenv=HOME") || !strings.Contains(all, "--setenv=TEST_SECRET") || strings.Contains(all, "must-not-be-in-argv") {
			t.Fatalf("unsafe unit/environment argv: %s", all)
		}
		return nil, errors.New("injected submit failure")
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Place(context.Background(), "dev-unit", spec)
	var fault *Failure
	if !called || !errors.As(err, &fault) || fault.Code != "outcome_unknown" {
		t.Fatalf("submit: %v", err)
	}
}
