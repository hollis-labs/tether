package main

import (
	"github.com/hollis-labs/tether/internal/config"
	"testing"
)

func TestDoctorDaemonOwnershipRequiresEndpoint(t *testing.T) {
	cat := &config.Catalog{}
	cat.Global.Daemon.MCPUpstreams = config.MCPUpstreamsDaemon
	if got := checkMCPUpstreamOwnership(cat); got.Status != statusFail {
		t.Fatalf("disabled endpoint: %+v", got)
	}
}

func TestDoctorMCPEndpointExpandsUnixPathOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cat := &config.Catalog{}
	cat.Global.Identity.Mode = "observe"
	cat.Global.Daemon.MCPEndpoint.Enabled = true
	for _, addr := range []string{"", "unix:~/.tether/run/tetherd.sock", "unix:/absolute/tetherd.sock", "tcp:127.0.0.1:8994"} {
		cat.Global.Daemon.ListenAddr = addr
		if got := checkMCPEndpoint(cat); got.Status != statusOK {
			t.Fatalf("%s: %+v", addr, got)
		}
	}
	cat.Global.Daemon.ListenAddr = "tcp:0.0.0.0:8994"
	if got := checkMCPEndpoint(cat); got.Status != statusFail {
		t.Fatalf("unsafe listener: %+v", got)
	}
}
