package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/next/internal/datadir"
	"github.com/hollis-labs/tether/next/internal/legacyguard"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// configure points SUBSTRATE_CONFIG at an override that moves the HTTP host to addr.
func configure(t *testing.T, addr string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("httphost:\n  addr: %q\n", addr)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBSTRATE_CONFIG", p)
}

func env(dir string) func(string) string {
	return func(k string) string {
		if k == "TETHER_DATA_DIR" {
			return dir
		}
		return ""
	}
}

func TestServeAnswersHealthzAndStopsOnCancel(t *testing.T) {
	addr := freeAddr(t)
	configure(t, addr)
	data := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, env(data), []string{"serve"}) }()

	var body string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				body = string(b)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		cancel()
		t.Fatalf("/healthz never answered ok: %q", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after cancel", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
	for _, p := range []string{data, filepath.Join(data, databaseFile)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}

func TestBootRefusesOldLayout(t *testing.T) {
	configure(t, freeAddr(t))
	data := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(data, databaseFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	err = run(context.Background(), env(data), []string{"serve"})
	if !errors.Is(err, legacyguard.ErrLegacyLayout) {
		t.Fatalf("run on an old-layout data dir = %v, want ErrLegacyLayout", err)
	}
}

func TestBootRequiresDataDir(t *testing.T) {
	configure(t, freeAddr(t))
	err := run(context.Background(), env(""), []string{"serve"})
	if err == nil || !strings.Contains(err.Error(), "TETHER_DATA_DIR is not set") {
		t.Fatalf("run without TETHER_DATA_DIR = %v", err)
	}
}

// oldRoot builds the old tree's data root layout: state/tether.db with the old migrator table.
func oldRoot(t *testing.T) (root, db string) {
	t.Helper()
	root = t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	db = filepath.Join(root, "state", "tether.db")
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	return root, db
}

func TestBootRefusesTheOldDataRootUntouched(t *testing.T) {
	configure(t, freeAddr(t))
	root, _ := oldRoot(t)
	err := run(context.Background(), env(root), []string{"serve"})
	if !errors.Is(err, datadir.ErrOldDataRoot) {
		t.Fatalf("run with TETHER_DATA_DIR at the old root = %v, want ErrOldDataRoot", err)
	}
	if _, err := os.Stat(filepath.Join(root, databaseFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a new database was created in the old root: %v", err)
	}
	if fi, _ := os.Stat(root); fi.Mode().Perm() != 0o755 {
		t.Fatalf("the old root's mode changed to %v", fi.Mode().Perm())
	}
}

func TestBootRefusesAnAbsolutePathToTheOldDatabase(t *testing.T) {
	addr := freeAddr(t)
	_, oldDB := oldRoot(t)
	p := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("httphost:\n  addr: %q\ndatabase:\n  sqlite:\n    path: %q\n", addr, oldDB)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBSTRATE_CONFIG", p)
	err := run(context.Background(), env(filepath.Join(t.TempDir(), "data")), []string{"serve"})
	if !errors.Is(err, legacyguard.ErrLegacyLayout) {
		t.Fatalf("run with database.sqlite.path at the old database = %v, want ErrLegacyLayout", err)
	}
}
