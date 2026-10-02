package client

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/identity"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialUnixRequestsAndStreams(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "")
	dir, err := os.MkdirTemp("/var/tmp", "tth-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "token")
	if err := identity.WriteTokenFile(file, token); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("Unix request missing bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/events/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: daemon.ready\ndata: {\"scope\":\"daemon\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	for _, opt := range []Option{WithToken(token), WithTokenFile(file)} {
		c := New("unix:"+socket, opt)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.Ping(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		events, errs, err := c.StreamEvents(ctx, EventsStreamQuery{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		event, ok := <-events
		if !ok || event.Kind != "daemon.ready" {
			cancel()
			t.Fatal("no stream event")
		}
		for err := range errs {
			if err != nil {
				cancel()
				t.Fatal(err)
			}
		}
		cancel()
		c.http.CloseIdleConnections()
	}
}
func TestCredentialLookupAndFailClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".tether", "run", "operator.token")
	if err := identity.WriteTokenFile(file, token); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong credential")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := New("tcp:" + strings.TrimPrefix(server.URL, "http://")).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_TOKEN", token)
	if err := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithTokenFile(file)).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "missing")
	if err := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithTokenFile(missing)).Ping(context.Background()); err == nil || errors.Is(err, ErrDaemonUnreachable) {
		t.Fatal("explicit missing file did not fail closed", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_TOKEN", "")
	if err := New("tcp:" + strings.TrimPrefix(server.URL, "http://")).Ping(context.Background()); err == nil {
		t.Fatal("insecure default accepted")
	}
	if err := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithToken("secret\ninjected")).Ping(context.Background()); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("credential validation/error disclosure")
	}
}
func TestCredentialRedirectRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "")
	reached := false
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusOK) }))
	defer foreign.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, http.StatusFound) }))
	defer source.Close()
	if err := New("tcp:"+strings.TrimPrefix(source.URL, "http://"), WithToken("test-token")).Ping(context.Background()); err == nil {
		t.Fatal("followed foreign origin")
	}
	if reached {
		t.Fatal("foreign origin contacted")
	}
}
