package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func mailboxFrame(seq int, id string) string {
	return fmt.Sprintf("id: %d\nevent: message\ndata: {\"seq\":%d,\"id\":%q,\"kind\":\"notice\",\"from\":\"msg://agent/t/bob\",\"to\":\"msg://agent/t/alice\",\"subject\":\"s-%s\",\"body\":\"b-%s\",\"payload\":null,\"created_at\":\"2026-10-12T00:00:00Z\"}\n\n", seq, seq, id, id, id)
}

func TestMailboxWatchResumesWithoutDuplicates(t *testing.T) {
	var mu sync.Mutex
	var afters []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after_seq"))
		n := len(afters)
		mu.Unlock()
		if r.URL.Path != "/messages/watch" || r.URL.Query().Get("as") != "msg://agent/t/alice" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1: // from now: sync at 10, two messages, then the connection drops
			fmt.Fprint(w, "id: 10\nevent: synchronized\ndata: {\"seq\":10}\n\n")
			fmt.Fprint(w, mailboxFrame(11, "m1")+mailboxFrame(12, "m2"))
		case 2: // server replays m2 (overlap) and adds m3, then a gap
			fmt.Fprint(w, mailboxFrame(12, "m2")+mailboxFrame(13, "m3"))
			fmt.Fprint(w, "id: 13\nevent: gap\ndata: {\"reason\":\"restart\",\"high_water_seq\":13}\n\n")
		default:
			cancel()
		}
	}))
	defer srv.Close()

	catalogDir := writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
	oldCatalog, oldJSON, oldBackoff := catalogPath, mailboxJSONFlag, mailboxWatchBackoff
	catalogPath, mailboxJSONFlag, mailboxWatchBackoff = catalogDir, true, 5*time.Millisecond
	t.Cleanup(func() { catalogPath, mailboxJSONFlag, mailboxWatchBackoff = oldCatalog, oldJSON, oldBackoff })

	mailboxWatchCmd.SetContext(ctx)
	out := captureStdout(t, func() {
		if err := mailboxWatchCmd.RunE(mailboxWatchCmd, []string{"msg://agent/t/alice"}); err != nil {
			t.Fatalf("mailbox watch: %v", err)
		}
	})

	var ids []string
	var gaps int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var o map[string]any
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			t.Fatalf("line is not one JSON object: %q", line)
		}
		if o["event"] == "gap" {
			gaps++
			continue
		}
		for _, k := range []string{"id", "from", "to", "kind", "subject", "body", "seq"} {
			if _, ok := o[k]; !ok {
				t.Fatalf("line missing %q: %s", k, line)
			}
		}
		ids = append(ids, o["id"].(string))
	}
	if got := strings.Join(ids, ","); got != "m1,m2,m3" {
		t.Fatalf("delivered %s, want m1,m2,m3 exactly once", got)
	}
	if gaps != 1 {
		t.Fatalf("gap lines = %d, want 1", gaps)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(afters) < 3 || afters[0] != "" || afters[1] != "12" || afters[2] != "13" {
		t.Fatalf("after_seq per connection = %q, want [\"\" 12 13]", afters)
	}
}

func TestMailboxWatchFailsFastWhenRouteMissing(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	catalogDir := writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
	oldCatalog := catalogPath
	catalogPath = catalogDir
	t.Cleanup(func() { catalogPath = oldCatalog })

	mailboxWatchCmd.SetContext(context.Background())
	if err := mailboxWatchCmd.RunE(mailboxWatchCmd, []string{"msg://agent/t/alice"}); err == nil {
		t.Fatal("want an error so callers can fall back to polling")
	}
}
