//go:build linux

package shimhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLogOpenFailureIsPrechildPlacementFailure(t *testing.T) {
	cfg, spec := hostSpec(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := p.SessionDir(spec.Session)
	if err := PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "host.log"), 0700); err != nil {
		t.Fatal(err)
	}
	receipt, err := p.Place(context.Background(), "log-failure", spec)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "placement_failed" || !receipt.Retired || receipt.HostPID != 0 || receipt.ProviderPID != 0 {
		t.Fatalf("pre-Start failure became uncertain: %+v %v", receipt, err)
	}
	if _, err := os.Stat(receipt.DescriptorPath); !os.IsNotExist(err) {
		t.Fatalf("prechild failure retained descriptor: %v", err)
	}
	// Retirement is complete and needs no unrecorded PID to stop.
	if err := p.Stop(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
}

func TestUnitPrefixRequiresTrailingDash(t *testing.T) {
	_, err := New(Config{StateDir: t.TempDir(), ShimCommand: []string{"fake"}, UnitPrefix: "host"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "invalid_config" {
		t.Fatalf("accepted ambiguous unit prefix: %v", err)
	}
}
