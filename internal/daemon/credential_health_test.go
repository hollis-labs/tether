package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDenyListWatchHealthFreshStaleAndUnavailable(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "status.json")
	if denyListWatchHealth("", now) != nil {
		t.Fatal("unconfigured watcher advertised")
	}
	if !denyListWatchHealth(path, now).Unavailable {
		t.Fatal("missing state healthy")
	}
	for _, tc := range []struct {
		age   time.Duration
		stale bool
	}{{0, false}, {6 * time.Minute, true}, {-2 * time.Minute, true}} {
		raw, _ := json.Marshal(map[string]any{"checked_at": now.Add(-tc.age), "warning_count": 3, "acknowledged_count": 1, "cursor": 123, "private_log_line": "DO NOT EXPOSE"})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		h := denyListWatchHealth(path, now)
		if h.Unavailable || h.Stale != tc.stale || h.WarningCount != 3 {
			t.Fatalf("health=%+v", h)
		}
		public, _ := json.Marshal(h)
		if strings.Contains(string(public), "DO NOT EXPOSE") || strings.Contains(string(public), "private_log_line") {
			t.Fatal("raw detector state exposed")
		}
	}
	for _, raw := range []string{`{`, `{}`, `{"checked_at":"2026-10-16T00:00:00Z","warning_count":1,"acknowledged_count":2}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if !denyListWatchHealth(path, now).Unavailable {
			t.Fatal("invalid state healthy")
		}
	}
}
