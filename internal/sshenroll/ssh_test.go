package sshenroll

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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
	if mode == "stall" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "forward" || mode == "substitute-tcp" {
		for i, arg := range os.Args {
			if arg == "-L" && i+1 < len(os.Args) {
				parts := strings.Split(os.Args[i+1], ":")
				network, address := "unix", parts[0]
				if len(parts) == 4 {
					network, address = "tcp4", net.JoinHostPort(parts[0], parts[1])
				} else if len(parts) != 3 {
					os.Exit(2)
				}
				if mode == "substitute-tcp" && network == "unix" {
					for {
						time.Sleep(time.Hour)
					}
				}
				listener, err := net.Listen(network, address)
				if err != nil {
					os.Exit(3)
				}
				for {
					conn, err := listener.Accept()
					if err != nil {
						os.Exit(4)
					}
					if mode == "forward" {
						if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
							_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 13\r\nConnection: close\r\n\r\nowned-forward")
						}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tunnel, err := s.Forward(ctx, 7181)
	if err != nil {
		t.Fatal(err)
	}
	owned := tunnel.(*sshTunnel)
	address := owned.socket
	info, err := os.Stat(owned.directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("forward directory is not private", err)
	}
	conn, err := net.DialTimeout("unix", address, time.Second)
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
	if _, err := os.Lstat(owned.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned forward directory remained after joined close", err)
	}
	if conn, err := net.DialTimeout("unix", address, 100*time.Millisecond); err == nil {
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

func TestSSHForwardRefusesTCPReadinessWithoutOwnedSocket(t *testing.T) {
	t.Setenv("TETHER_SSH_FIXTURE_HELPER", "substitute-tcp")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	s := &SSH{Target: "worker", Command: os.Args[0]}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if tunnel, err := s.Forward(ctx, port); !errors.Is(err, context.DeadlineExceeded) {
		if tunnel != nil {
			_ = tunnel.Close()
		}
		t.Fatal("TCP substitute did not reach owned-forward cancellation", err)
	}
}

func TestSSHTunnelHTTPClientUsesOnlyPrivateSocket(t *testing.T) {
	t.Setenv("TETHER_SSH_FIXTURE_HELPER", "forward")
	s := &SSH{Target: "worker", Command: os.Args[0]}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tunnel, err := s.Forward(ctx, 7181)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tunnel.Close() }()
	var tcpCalled atomic.Bool
	input := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		tcpCalled.Store(true)
		return nil, errors.New("unexpected TCP transport")
	}}}
	client, err := enrollmentTunnelHTTPClient(input, tunnel, 7181)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(tunnel.BaseURL() + "/auth/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "owned-forward" || tcpCalled.Load() {
		t.Fatal("HTTP verification escaped the owned private SSH socket", err)
	}
}
