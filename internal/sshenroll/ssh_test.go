package sshenroll

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// A disposable copy of this test executable supplies the SSH process boundary.
// No actual ssh, provider, login hooks, real config or user unit is invoked.
func init() {
	mode := os.Getenv("TETHER_SSH_FIXTURE_HELPER")
	if mode == "" {
		return
	}
	if mode == "capture" {
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	}
	if mode == "refuse" {
		_, _ = fmt.Fprint(os.Stdout, "synthetic private output")
		_, _ = fmt.Fprint(os.Stderr, "synthetic private diagnostic")
		os.Exit(17)
	}
	if mode == "forward" {
		for i, arg := range os.Args {
			if arg == "-L" && i+1 < len(os.Args) {
				parts := strings.Split(os.Args[i+1], ":")
				if len(parts) != 4 {
					os.Exit(2)
				}
				listener, err := net.Listen("tcp4", net.JoinHostPort(parts[0], parts[1]))
				if err != nil {
					os.Exit(3)
				}
				for {
					conn, err := listener.Accept()
					if err != nil {
						os.Exit(4)
					}
					_ = conn.Close()
				}
			}
		}
		os.Exit(5)
	}
	os.Exit(6)
}
func TestSSHPrivateCaptureAndSanitizedRefusal(t *testing.T) {
	t.Setenv("TETHER_SSH_FIXTURE_HELPER", "capture")
	s := &SSH{Target: "worker", Command: os.Args[0]}
	data, err := s.run(context.Background(), "authored harmless script", strings.NewReader("synthetic private output"), 5*time.Second)
	if err != nil || string(data) != "synthetic private output" {
		t.Fatal("private pipe capture failed")
	}
	t.Setenv("TETHER_SSH_FIXTURE_HELPER", "refuse")
	_, err = s.run(context.Background(), "authored harmless script", nil, 5*time.Second)
	if err == nil || strings.Contains(err.Error(), "synthetic private") {
		t.Fatal("private SSH output escaped error")
	}
}
func TestSSHForwardOwnedCancellationAndJoin(t *testing.T) {
	t.Setenv("TETHER_SSH_FIXTURE_HELPER", "forward")
	s := &SSH{Target: "worker", Command: os.Args[0]}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tunnel, err := s.Forward(ctx, 7181)
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(tunnel.BaseURL(), "http://")
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("owned forward remained alive after joined close")
	}
}
func TestPreflightParsesLoginInventoryAndRefusesMissingCapability(t *testing.T) {
	p, err := parsePreflight([]byte("os\tLinux\narch\tarm64\nhome\t/home/worker\npath\t/usr/bin\ngit\tyes\nbubblewrap\tyes\nmanager\tyes\nlinger\tyes\nwritable\tyes\nexisting\tno\nprovider:codex\t/usr/bin/codex\n"))
	if err != nil || p.Validate([]string{"codex"}) != nil {
		t.Fatal("valid login inventory refused", err)
	}
	p.Bubblewrap = false
	if err := p.Validate([]string{"codex"}); err == nil {
		t.Fatal("unusable bubblewrap accepted")
	}
	if _, err := parsePreflight([]byte("login banner with synthetic private material\n")); err == nil || strings.Contains(err.Error(), "synthetic private") {
		t.Fatal("login banner accepted or reflected")
	}
}
