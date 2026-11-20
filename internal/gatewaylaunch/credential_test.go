package gatewaylaunch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

func TestCredentialRotatesBeforeExpiryAndCleansUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires tmpfs")
	}
	var count atomic.Int32
	minted := make(chan time.Time, 32)
	path, stop, err := Start(func(context.Context) (Credential, error) {
		n := count.Add(1)
		minted <- time.Now()
		return Credential{Token: string('a' + n), ExpiresAt: time.Now().Add(500 * time.Millisecond)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	first := <-minted
	select {
	case second := <-minted:
		if second.Sub(first) >= 500*time.Millisecond {
			t.Fatal("refresh missed expiry")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no refresh")
	}
	if filepath.Dir(filepath.Dir(path)) != "/dev/shm" {
		t.Fatal("credential not in tmpfs")
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatal("credential directory not private", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential not private", err)
	}
	if _, err := tokenfile.Read(path); err != nil {
		t.Fatal(err)
	}
	stop()
	stop()
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credential directory retained")
	}
}

func TestExpiredCredentialRemovedWhenRefreshFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires tmpfs")
	}
	var count atomic.Int32
	path, stop, err := Start(func(context.Context) (Credential, error) {
		if count.Add(1) > 1 {
			return Credential{}, errors.New("synthetic issuer failure")
		}
		return Credential{Token: "synthetic", ExpiresAt: time.Now().Add(150 * time.Millisecond)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired credential remained readable")
}

func TestGatewayEndpointRejectsCredentialLeakRoutes(t *testing.T) {
	for _, url := range []string{"http://public.example/mcp", "https://user:pass@example/mcp", "file:///tmp/mcp", "https://example/mcp#fragment", ""} {
		if ValidateURL(url) == nil {
			t.Error("accepted unsafe endpoint")
		}
	}
	for _, url := range []string{"https://mcp.example/mcp", "http://127.0.0.1:18213/mcp", "http://[::1]:18213/mcp"} {
		if err := ValidateURL(url); err != nil {
			t.Error(err)
		}
	}
}

func TestGatewayRunHandoffAndWorkerRefreshResume(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires tmpfs")
	}
	dir, err := memoryDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gateway.token")
	alias := dir + "/../" + filepath.Base(dir) + "/gateway.token"
	if _, err := Resume(alias, func(context.Context) (Credential, error) {
		t.Fatal("noncanonical path reached mint")
		return Credential{}, nil
	}); err == nil {
		t.Fatal("noncanonical tmpfs alias accepted")
	}
	if err := tokenfile.Write(path, "previous-worker-lease"); err != nil {
		t.Fatal(err)
	}
	if err := BindRunToken(path, "synthetic-run"); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRunToken(RunTokenPath(path), ""); err != nil || got != "synthetic-run" {
		t.Fatal("run token handoff failed")
	}
	stop, err := Resume(path, func(context.Context) (Credential, error) {
		return Credential{Token: "renewed-worker-lease", ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if got, err := tokenfile.Read(path); err != nil || got != "renewed-worker-lease" {
		t.Fatal("worker lease not renewed")
	}
	if got, err := ReadRunToken(RunTokenPath(path), ""); err != nil || got != "synthetic-run" {
		t.Fatal("refresh changed run context")
	}
	stop()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("handoff retained at stop")
	}
	if _, err := Resume(filepath.Join(t.TempDir(), "gateway.token"), func(context.Context) (Credential, error) {
		t.Fatal("minted into disk-backed directory")
		return Credential{}, nil
	}); err == nil {
		t.Fatal("resume accepted arbitrary directory")
	}
	if _, err := ReadRunToken(RunTokenPath(path), "ambient-fallback"); err == nil {
		t.Fatal("missing explicit run handoff used ambient token")
	}
}
