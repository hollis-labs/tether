package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestLocalTailStillReadsCompleteHostLog(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("local\xff"), 15000)
	if err := os.WriteFile(filepath.Join(ws, "logs", "session.log"), data, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.SessionDTO{ID: "s", Workspace: ws})
	}))
	defer server.Close()
	remoteTargetFixture(t, "")
	isolateTetherPaths(t, t.TempDir())
	config := fmt.Sprintf("version: 0.1.0\nmodules:\n  remote_listener: true\ndaemon:\n  listen_addr: tcp:%s\n  shutdown_timeout: 10s\n", strings.TrimPrefix(server.URL, "http://"))
	writeCatalog(t, catalogPath, config)
	got := captureRemoteStdout(t, func() error { return runTailSnapshotContext(context.Background(), "s") })
	if !bytes.Equal(got, data) {
		t.Fatal("local tail was bounded, redirected, or damaged")
	}
}

func TestRemoteSessionsUseAPIWithoutLocalCatalogOrLog(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "logs", "session.log"), []byte("client-local log must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer device-test" {
			t.Error("device credential missing")
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/sessions":
			_ = json.NewEncoder(w).Encode(api.ListSessionsResponse{Sessions: []api.SessionDTO{{ID: "s", Workspace: ws}}})
		case "/sessions/s":
			_ = json.NewEncoder(w).Encode(api.SessionDTO{ID: "s", Workspace: ws})
		case "/sessions/s/log":
			_ = json.NewEncoder(w).Encode(api.SessionLogResponse{Data: []byte{0xff, 'r', 'e', 'm', 'o', 't', 'e'}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	remoteTargetFixture(t, "tcp:"+strings.TrimPrefix(server.URL, "http://"))
	if _, err := listSessionsDaemonOrStore(context.Background(), catalogPath); err != nil {
		t.Fatal(err)
	}
	if _, err := getSessionDaemonOrStore(context.Background(), catalogPath, "s"); err != nil {
		t.Fatal(err)
	}
	for _, tail := range []func() error{
		func() error { return runTailSnapshotContext(context.Background(), "s") },
		func() error {
			return attachFallbackContext(context.Background(), errors.New("daemon 404: session not running"), "s", true)
		},
	} {
		out := captureRemoteStdout(t, tail)
		if string(out) != string([]byte{0xff, 'r', 'e', 'm', 'o', 't', 'e'}) {
			t.Fatalf("tail read client disk or damaged bytes: %q", out)
		}
	}
}

func captureRemoteStdout(t *testing.T, fn func() error) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old; _ = f.Close() }()
	if err := fn(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRemoteSessionErrorsNeverFallBackToLocalState(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"API refused"}}`))
			}))
			defer server.Close()
			remoteTargetFixture(t, "tcp:"+strings.TrimPrefix(server.URL, "http://"))
			if _, err := listSessionsDaemonOrStore(context.Background(), catalogPath); err == nil || !strings.Contains(err.Error(), "API refused") {
				t.Fatalf("list fallback: %v", err)
			}
			if _, err := getSessionDaemonOrStore(context.Background(), catalogPath, "s"); err == nil || !strings.Contains(err.Error(), "API refused") {
				t.Fatalf("get fallback: %v", err)
			}
			if err := runTailSnapshotContext(context.Background(), "s"); err == nil || !strings.Contains(err.Error(), "API refused") {
				t.Fatalf("tail fallback: %v", err)
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	target := "tcp:" + strings.TrimPrefix(server.URL, "http://")
	server.Close()
	remoteTargetFixture(t, target)
	if _, err := listSessionsDaemonOrStore(context.Background(), catalogPath); !errors.Is(err, client.ErrDaemonUnreachable) {
		t.Fatalf("unreachable list fell back: %v", err)
	}
	if _, err := getSessionDaemonOrStore(context.Background(), catalogPath, "s"); !errors.Is(err, client.ErrDaemonUnreachable) {
		t.Fatalf("unreachable get fell back: %v", err)
	}
	if err := attachFallbackContext(context.Background(), client.ErrDaemonUnreachable, "s", true); !errors.Is(err, client.ErrDaemonUnreachable) {
		t.Fatalf("unreachable attach fell back: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runTailSnapshotContext(ctx, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled tail fell back: %v", err)
	}
}
