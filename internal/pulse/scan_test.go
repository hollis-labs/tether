package pulse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanIdentityLivenessAndDataMinimization(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	write := func(name string, d map[string]any) {
		t.Helper()
		b, _ := json.Marshal(d)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{"thread-a", "thread-b", "reused"} {
		start := "Thu Oct 15 22:05:27 2026"
		if sid == "reused" {
			start = "old birth time"
		}
		write(sid+".json", map[string]any{"v": 1, "tool": "codex", "session_id": sid, "pid": 42, "proc_start": start, "state": "working", "updated_at": now.UnixMilli(), "prompt": "private prompt", "cwd": "/private/path", "last_action": map[string]any{"tool": "exec_command", "target": "secret command", "at": now.UnixMilli(), "done": false}})
	}
	s := &Scanner{Directory: dir, Starts: func(context.Context) (map[int]string, error) {
		return map[int]string{42: "Thu Oct 15 22:05:27 2026"}, nil
	}}
	b, err := s.Scan(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range b.Events {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		got[e.Process.ProviderSessionID] = true
	}
	if !got["thread-a"] || !got["thread-b"] || got["reused"] {
		t.Fatalf("shared PID must preserve threads and reject reused PID: %v", got)
	}
	raw, _ := json.Marshal(b)
	for _, s := range []string{"private prompt", "/private/path", "secret command", "target"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("transport leaked %q", s)
		}
	}
	stale, err := s.Scan(context.Background(), now.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stale.Events {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["state"] != "unknown" || p["stale"] != true {
			t.Fatalf("quiet shared process cannot prove live thread state: %s", e.Payload)
		}
	}
}

func TestMissingDirectoryIsSuccessfulEmptyScan(t *testing.T) {
	s := &Scanner{Directory: filepath.Join(t.TempDir(), "missing"), Starts: func(context.Context) (map[int]string, error) {
		t.Fatal("no need to scan processes without pulse files")
		return nil, nil
	}}
	b, err := s.Scan(context.Background(), time.Now())
	if err != nil || b == nil || b.Events == nil || len(b.Events) != 0 {
		t.Fatalf("%+v %v", b, err)
	}
}
