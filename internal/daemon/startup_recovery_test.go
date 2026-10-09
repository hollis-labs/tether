package daemon

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartupRecoveryRunsAfterListenerAndJoinsBeforeClose(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "daemon.sock")
	started := make(chan error, 1)
	var joined atomic.Bool
	server := &Server{Config: Config{ListenAddr: "unix:" + socket, PIDFile: filepath.Join(dir, "daemon.pid"), ShutdownTimeout: time.Second}}
	server.Startup = func(ctx context.Context) {
		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
		response, err := client.Get("http://localhost/health")
		if err == nil {
			if response.StatusCode != 200 {
				t.Errorf("health=%d", response.StatusCode)
			}
			_ = response.Body.Close()
		}
		client.CloseIdleConnections()
		started <- err
		<-ctx.Done()
		joined.Store(true)
	}
	server.Close = func() error {
		if !joined.Load() {
			t.Error("store closed before startup recovery joined")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup not called")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not join recovery")
	}
}
