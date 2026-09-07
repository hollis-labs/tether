package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shortTempDir returns a short path under /tmp so we stay under the
// ~104-char sun_path limit for unix domain sockets on macOS/BSD. t.TempDir()
// on Darwin lives under /var/folders/.../T which routinely exceeds the
// limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "mux-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestListener_UnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	addr := "unix:" + filepath.Join(dir, "s.sock")
	lis, err := Listener(addr)
	if err != nil {
		t.Fatalf("Listener: %v", err)
	}
	defer lis.Close()
	if got := lis.Addr().Network(); got != "unix" {
		t.Errorf("network = %q, want unix", got)
	}
}

func TestListener_UnixStaleSocketRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")
	addr := "unix:" + path

	lis1, err := Listener(addr)
	if err != nil {
		t.Fatalf("first Listener: %v", err)
	}
	_ = lis1.Close()
	// The closed listener may leave the file behind; the second Listener
	// must overwrite it.
	lis2, err := Listener(addr)
	if err != nil {
		t.Fatalf("second Listener (stale socket): %v", err)
	}
	defer lis2.Close()
}

// TestListener_UnixRefusesToStealALiveSocket is T11's durability review
// evidence (CW-20260906-0042): a socket-typed file existing at the
// target path is NOT sufficient proof of staleness if something is
// actually still listening on it -- the live-reproduced split-brain this
// closes doesn't even require a stale/missing PID file, just a second
// Listener() call at the same path while the first is still live.
func TestListener_UnixRefusesToStealALiveSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	path := filepath.Join(dir, "s.sock")
	addr := "unix:" + path

	lis1, err := Listener(addr)
	if err != nil {
		t.Fatalf("first Listener: %v", err)
	}
	defer lis1.Close()

	if _, err := Listener(addr); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Listener while the first is still live: err = %v, want a wrapped ErrAlreadyRunning", err)
	}

	// The live socket file must still be there and still working --
	// refusing the second bind must not have deleted or damaged it.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("live socket file missing after a refused steal attempt: %v", statErr)
	}
	conn, dialErr := net.Dial("unix", path)
	if dialErr != nil {
		t.Fatalf("original listener no longer reachable after a refused steal attempt: %v", dialErr)
	}
	_ = conn.Close()
}

func TestListener_UnixRefusesRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	path := filepath.Join(dir, "notasock")
	if err := os.WriteFile(path, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Listener("unix:" + path); err == nil {
		t.Error("expected error for non-socket file at listen path")
	}
}

func TestListener_TCPLoopback(t *testing.T) {
	lis, err := Listener("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listener: %v", err)
	}
	defer lis.Close()
	if got := lis.Addr().Network(); got != "tcp" {
		t.Errorf("network = %q, want tcp", got)
	}
}

func TestListener_UnknownSchemeRejected(t *testing.T) {
	if _, err := Listener("udp:127.0.0.1:1"); err == nil {
		t.Error("expected error for unsupported scheme")
	}
	if _, err := Listener(""); err == nil {
		t.Error("expected error for empty addr")
	}
}

func TestPIDFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "muxd.pid")
	if err := WritePIDFile(path, 12345); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	pid, err := ReadPIDFile(path)
	if err != nil {
		t.Fatalf("ReadPIDFile: %v", err)
	}
	if pid != 12345 {
		t.Errorf("round-trip pid = %d, want 12345", pid)
	}
	if err := RemovePIDFile(path); err != nil {
		t.Fatalf("RemovePIDFile: %v", err)
	}
	if _, err := ReadPIDFile(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected ErrNotExist after remove; got %v", err)
	}
}

func TestPIDFile_RejectsLiveDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "muxd.pid")
	if err := WritePIDFile(path, os.Getpid()); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	err := WritePIDFile(path, os.Getpid()+9999) // try to claim with different pid
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("expected ErrAlreadyRunning; got %v", err)
	}
}

func TestPIDFile_OverwritesStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "muxd.pid")
	// Pick a PID very unlikely to exist.
	stale := 1
	for i := 99999; i < 100100; i++ {
		if !IsAlive(i) {
			stale = i
			break
		}
	}
	if err := os.WriteFile(path, []byte(" 99999 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = stale // silence if loop finds nothing
	if err := WritePIDFile(path, os.Getpid()); err != nil {
		t.Errorf("expected stale pidfile to be overwritten; got %v", err)
	}
}

func TestPIDFile_RemoveMissingIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.pid")
	if err := RemovePIDFile(path); err != nil {
		t.Errorf("RemovePIDFile on missing path returned %v", err)
	}
}

func TestIsAlive_SelfTrue(t *testing.T) {
	if !IsAlive(os.Getpid()) {
		t.Error("IsAlive(self) = false")
	}
}

func TestIsAlive_BogusFalse(t *testing.T) {
	if IsAlive(0) {
		t.Error("IsAlive(0) = true")
	}
}

func TestServer_RunServesHealthAndShutsDown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	pidfile := filepath.Join(dir, "d.pid")
	cfg := Config{
		ListenAddr:      "unix:" + sock,
		PIDFile:         pidfile,
		ShutdownTimeout: 2 * time.Second,
	}

	closed := false
	srv := &Server{
		Config:  cfg,
		Manager: nil,
		Close:   func() error { closed = true; return nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	// Wait for the listener + pid file to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			if _, err := os.Stat(pidfile); err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket not created: %v", err)
	}

	client := DialHTTPClient(cfg.ListenAddr)
	resp, err := client.Get(BaseURL(cfg.ListenAddr) + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("Status = %q, want ok", h.Status)
	}
	if h.PID != os.Getpid() {
		t.Errorf("Health.PID = %d, want %d", h.PID, os.Getpid())
	}
	if h.Listener != cfg.ListenAddr {
		t.Errorf("Health.Listener = %q, want %q", h.Listener, cfg.ListenAddr)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if !closed {
		t.Error("Close callback was not invoked")
	}
	if _, err := os.Stat(pidfile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pidfile still exists after shutdown: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket still exists after shutdown: %v", err)
	}
}

func TestServer_RunCloseErrorStillCleansArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	pidfile := filepath.Join(dir, "d.pid")
	cfg := Config{
		ListenAddr:      "unix:" + sock,
		PIDFile:         pidfile,
		ShutdownTimeout: 2 * time.Second,
	}

	srv := &Server{
		Config: cfg,
		Close: func() error {
			return errors.New("close failed")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			if _, err := os.Stat(pidfile); err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "close failed") {
			t.Fatalf("Run error = %v, want close failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if _, err := os.Stat(pidfile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pidfile still exists after shutdown: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket still exists after shutdown: %v", err)
	}
}

func TestServer_RunRefusesSecondInstance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	cfg := Config{
		ListenAddr:      "unix:" + filepath.Join(dir, "s.sock"),
		PIDFile:         filepath.Join(dir, "d.pid"),
		ShutdownTimeout: time.Second,
	}

	if err := WritePIDFile(cfg.PIDFile, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	// os.Getpid() is alive (we're in it), so a second instance must bail.
	srv := &Server{Config: cfg}
	err := srv.Run(context.Background())
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("expected ErrAlreadyRunning; got %v", err)
	}
}
