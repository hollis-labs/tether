package jsonltail

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) add(l []byte) { c.mu.Lock(); c.lines = append(c.lines, string(l)); c.mu.Unlock() }
func (c *collector) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func waitLines(t *testing.T, c *collector, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.get()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("got %q, want %d lines", c.get(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.get()
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestTailWaitsSplitsAndResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.jsonl")
	c := &collector{}
	resets := 0
	var mu sync.Mutex
	tl := &Tail{Path: path, Poll: 5 * time.Millisecond, OnReset: func() { mu.Lock(); resets++; mu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx, c.add) }()

	time.Sleep(20 * time.Millisecond) // file absent: waits
	appendFile(t, path, "{\"a\":1}\n{\"b\":")
	waitLines(t, c, 1)
	time.Sleep(20 * time.Millisecond)
	if got := c.get(); len(got) != 1 {
		t.Fatalf("partial line yielded: %q", got)
	}
	appendFile(t, path, "2}\r\n\n")
	got := waitLines(t, c, 2)
	if got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("lines = %q", got)
	}
	// The file is rewritten shorter (the spool trimming itself): start over.
	if err := os.WriteFile(path, []byte("{\"c\":3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = waitLines(t, c, 3)
	if got[2] != `{"c":3}` {
		t.Fatalf("after reset: %q", got)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if resets != 1 {
		t.Errorf("resets = %d", resets)
	}
}

func TestTailFromEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.jsonl")
	appendFile(t, path, "old\n")
	c := &collector{}
	tl := &Tail{Path: path, Poll: 5 * time.Millisecond, FromEnd: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tl.Run(ctx, c.add) }()
	time.Sleep(20 * time.Millisecond)
	appendFile(t, path, "new\n")
	if got := waitLines(t, c, 1); got[0] != "new" {
		t.Fatalf("lines = %q", got)
	}
}
