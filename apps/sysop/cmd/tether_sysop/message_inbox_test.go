package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func readInbox(t *testing.T, s *appServer, query string) messagesResponse {
	t.Helper()
	r := httptest.NewRecorder()
	s.handleMessagesList(r, httptest.NewRequest(http.MethodGet, "/api/messages?"+query, nil))
	if r.Code != http.StatusOK {
		t.Fatalf("inbox status %d: %s", r.Code, r.Body.String())
	}
	var body messagesResponse
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestScopedInboxPagesSurviveAgentTrafficAndRefresh(t *testing.T) {
	s, db := inboxFixture(t)
	// This traffic previously displaced every older user message from the
	// global 500-row slice. Equal timestamps exercise the page tie-breaker.
	for i := 0; i < 510; i++ {
		_, err := db.DB().Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,created_at) VALUES(?,'notice','msg://agent/local/test','msg://agent/local/recipient','2026-10-03T00:00:00Z')`, fmt.Sprintf("agent-%04d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,created_at) VALUES('variant','notice','msg://agent/local/test','msg://user/agent-mux/chris','2026-10-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for offset := 0; offset < 2; offset++ {
		q := fmt.Sprintf("scope=user&limit=1&offset=%d&archive=all", offset)
		for refresh := 0; refresh < 2; refresh++ {
			p := readInbox(t, s, q)
			if p.Total != 2 || len(p.Messages) != 1 {
				t.Fatalf("page: %+v", p)
			}
			if refresh == 0 {
				seen[p.Messages[0].ID] = true
			}
		}
	}
	if !seen["fixture-message"] || !seen["variant"] {
		t.Fatal(seen)
	}
}

func TestReadArchiveStatePersistsAcrossReaderRestart(t *testing.T) {
	s, _ := inboxFixture(t)
	for _, a := range []struct {
		path string
		fn   http.HandlerFunc
	}{{"read", s.handleMessageMarkRead}, {"archive", s.handleMessageArchive}} {
		r := httptest.NewRecorder()
		a.fn(r, httptest.NewRequest(http.MethodPost, "/api/messages/"+a.path, strings.NewReader(`{"id":"fixture-message","as":"msg://user/local/chris"}`)))
		if r.Code != 200 {
			t.Fatalf("action %s: %d %s", a.path, r.Code, r.Body.String())
		}
	}
	s.closeStateReader()
	if p := readInbox(t, s, "scope=user&read=unread&archive=all"); p.Total != 0 {
		t.Fatal("read message returned as unread")
	}
	if p := readInbox(t, s, "scope=user&read=read&archive=archived"); p.Total != 1 || p.Messages[0].ReadAt == "" || p.Messages[0].ArchivedAt == "" {
		t.Fatalf("persisted state: %+v", p)
	}
	if p := readInbox(t, s, "scope=user&archive=active"); p.Total != 0 {
		t.Fatal("archive filter ignored")
	}
}

func TestAliasesRenameWithoutChangingEnvelopeIdentity(t *testing.T) {
	s, db := inboxFixture(t)
	for _, alias := range []string{"chris-old", "chris-new"} {
		if err := db.SetMessageAlias("msg://user/local/chris", alias); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ResolveMessageAddress("chris-old"); err == nil {
		t.Fatal("old alias still resolves")
	}
	if err := db.SetMessageAlias("msg://user/local/other", "CHRIS-NEW"); err == nil {
		t.Fatal("ambiguous alias accepted")
	}
	r := httptest.NewRecorder()
	s.handleMessageReply(r, httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(`{"from":"msg://agent/local/test","to":"@CHRIS-NEW","body":"test"}`)))
	if r.Code != 201 {
		t.Fatalf("alias send: %d %s", r.Code, r.Body.String())
	}
	p := readInbox(t, s, "scope=user&to=chris-new&archive=all")
	if p.Total != 2 {
		t.Fatal(p)
	}
	for _, m := range p.Messages {
		if m.To != "msg://user/local/chris" {
			t.Fatal("alias changed stored recipient", m.To)
		}
	}
}
