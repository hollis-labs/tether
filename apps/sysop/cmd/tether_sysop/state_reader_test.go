package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/store"
)

func inboxFixture(t *testing.T) (*appServer, *store.Store) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.DB().Exec(`INSERT INTO messages (id, kind, from_urn, to_urn, payload, created_at) VALUES ('fixture-message', 'response', 'msg://agent/local/test', 'msg://user/local/chris', '"hello"', '2026-10-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("catalog:\n  defaults:\n    state_db: "+dbPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &appServer{catalogRoot: root}
	t.Cleanup(s.closeStateReader)
	return s, db
}

func TestMessagesConcurrentReadsWithWriter(t *testing.T) {
	s, writer := inboxFixture(t)
	ts := httptest.NewServer(http.HandlerFunc(s.handleMessages))
	defer ts.Close()
	client := ts.Client()
	client.Timeout = 3 * time.Second
	// Hold the daemon's writer reservation across every refresh. WAL readers
	// should see the committed inbox, without attempting schema writes.
	if _, err := writer.DB().Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = writer.DB().Exec("ROLLBACK") }()
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		failures := make(chan error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := client.Get(ts.URL + "/api/messages")
				if err != nil {
					failures <- err
					return
				}
				defer resp.Body.Close()
				var body messagesResponse
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					failures <- err
					return
				}
				if resp.StatusCode != http.StatusOK {
					failures <- fmt.Errorf("status %d: %s", resp.StatusCode, body.Error)
					return
				}
				if len(body.Messages) != 1 || body.Messages[0].ID != "fixture-message" || body.Totals.User.Unread != 1 {
					failures <- fmt.Errorf("unexpected committed inbox: %+v", body)
				}
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if t.Failed() {
			break
		}
	}
}

func TestMessagesUnavailableErrorAndRetry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("catalog:\n  defaults:\n    state_db: "+path+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &appServer{catalogRoot: root}
	defer s.closeStateReader()
	rr := httptest.NewRecorder()
	s.handleMessagesList(rr, httptest.NewRequest(http.MethodGet, "/api/messages", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing DB: status %d", rr.Code)
	}
	var failure struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error == "" || failure.Message != failure.Error {
		t.Fatalf("missing UI error detail: %+v", failure)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("GET created missing DB: %v", err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rr = httptest.NewRecorder()
	s.handleMessagesList(rr, httptest.NewRequest(http.MethodGet, "/api/messages", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("startup retry: status %d, %s", rr.Code, rr.Body.String())
	}
	// A daemon restart can leave this reader open. Newly committed messages
	// must still be visible without reopening it or depending on schema writes.
	if _, err := db.DB().Exec(`INSERT INTO messages (id,kind,from_urn,to_urn,created_at) VALUES ('after-start','response','msg://agent/local/test','msg://user/local/chris','2026-10-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	resp, err := s.loadMessages()
	if err != nil || len(resp.Messages) != 1 || resp.Messages[0].ID != "after-start" {
		t.Fatalf("subsequent daemon write: %+v, %v", resp, err)
	}
}

func TestMessagesReaderFollowsCatalogPath(t *testing.T) {
	s, _ := inboxFixture(t)
	first, err := s.loadMessages()
	if err != nil || len(first.Messages) != 1 {
		t.Fatalf("first inbox: %+v, %v", first, err)
	}
	path := filepath.Join(t.TempDir(), "replacement.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.WriteFile(filepath.Join(s.catalogRoot, "global.yaml"), []byte("catalog:\n  defaults:\n    state_db: "+path+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := s.loadMessages()
	if err != nil || len(next.Messages) != 0 {
		t.Fatalf("replacement inbox: %+v, %v", next, err)
	}
}
