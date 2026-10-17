package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mcp/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tether/internal/workerruntime"
)

func TestPluginOwnsMailSocketAndDrainsOnUnload(t *testing.T) {
	root := t.TempDir()
	if err := workerruntime.SaveJSON(filepath.Join(root, "config.json"), workerruntime.Config{Hub: "http://127.0.0.1:1", Label: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "device.token"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &workerPlugin{}
	if _, err := p.Init(context.Background(), subprocess.InitParams{DataDir: root}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Load(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", filepath.Join(root, "run", "mail.sock"))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// A second plugin must refuse the live socket, not take it away.
	second := &workerPlugin{}
	if _, err := second.Init(ctx, subprocess.InitParams{DataDir: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Load(ctx); err == nil {
		t.Fatal("second worker took the live socket")
	}
	if err := p.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if h, err := p.Health(ctx); err != nil || h.OK {
		t.Fatalf("stopped plugin reported healthy: %+v %v", h, err)
	}
	if c, err := net.Dial("unix", filepath.Join(root, "run", "mail.sock")); err == nil {
		c.Close()
		t.Fatal("socket survived unload")
	}
}
