package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/userprofile"
)

func profileRequest(s *appServer, method, body string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	s.handleMessageProfile(r, httptest.NewRequest(method, "/api/messages/profile", strings.NewReader(body)))
	return r
}

func TestLocalProfilePreferenceCanonicalPersistenceAndClientBoundary(t *testing.T) {
	s, db := inboxFixture(t)
	r := profileRequest(s, http.MethodGet, "")
	var p userprofile.Profile
	if err := json.Unmarshal(r.Body.Bytes(), &p); err != nil || r.Code != 200 || p.URN != userprofile.OperatorURN || p.Messaging.FromDefault != "" {
		t.Fatalf("absent: %d %s", r.Code, r.Body.String())
	}
	p.URN = "msg://user/local/chris"
	p.Aliases = []userprofile.Alias{{URN: p.URN, Alias: "chris"}, {URN: "msg://user/agent-mux/chris", Alias: "chris-mux"}}
	if err := userprofile.Save(s.userProfilePath, p); err != nil {
		t.Fatal(err)
	}
	r = profileRequest(s, http.MethodPost, `{"from_default":"@CHRIS-MUX"}`)
	if r.Code != 200 {
		t.Fatalf("save alias: %d %s", r.Code, r.Body.String())
	}
	// A fresh server instance loads the canonical preference from disk.
	restarted := &appServer{catalogRoot: s.catalogRoot, userProfilePath: s.userProfilePath}
	t.Cleanup(restarted.closeStateReader)
	p, err := restarted.loadUserProfile()
	if err != nil || p.DefaultSender() != "msg://user/agent-mux/chris" {
		t.Fatalf("restart: %+v %v", p, err)
	}
	for _, body := range []string{
		`{"from_default":"unknown"}`,
		`{"from_default":"msg://user/local/other","key":"other.messaging.from_default"}`,
		`{"from_default":"msg://user/local/other","urn":"msg://user/local/other"}`,
		`{"from_default":"msg://user/local/other"}` + strings.Repeat(" ", 1<<20) + `{"key":"other.messaging.from_default"}`,
	} {
		r = profileRequest(s, http.MethodPost, body)
		if r.Code != 400 {
			t.Fatalf("invalid request accepted: %d %s", r.Code, r.Body.String())
		}
	}
	p, err = s.loadUserProfile()
	if err != nil || p.DefaultSender() != "msg://user/agent-mux/chris" {
		t.Fatal("invalid request changed preference")
	}
	var storedTo string
	if err := db.DB().QueryRow(`SELECT to_urn FROM messages WHERE id='fixture-message'`).Scan(&storedTo); err != nil || storedTo != "msg://user/local/chris" {
		t.Fatalf("preference rewrote message: %s %v", storedTo, err)
	}
	r = profileRequest(s, http.MethodPost, `{"from_default":""}`)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	p, _ = s.loadUserProfile()
	if p.DefaultSender() != p.URN || p.Messaging.FromDefault != "" {
		t.Fatal("cleared preference did not use configured local user")
	}
}

func TestProfileAliasesSendFilterAndAmbiguity(t *testing.T) {
	s, db := inboxFixture(t)
	p := userprofile.Profile{URN: "msg://user/local/chris", Aliases: []userprofile.Alias{{URN: "msg://user/agent-mux/chris", Alias: "chris-mux"}}}
	if err := userprofile.Save(s.userProfilePath, p); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	s.handleMessageReply(r, httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(`{"from":"@chris-mux","to":"@chris-mux","body":"profile alias"}`)))
	if r.Code != 201 {
		t.Fatalf("send: %d %s", r.Code, r.Body.String())
	}
	all := readInbox(t, s, "scope=user&archive=all")
	filtered := readInbox(t, s, "scope=user&to=chris-mux&archive=all")
	if all.Total != 2 || filtered.Total != 1 || filtered.Messages[0].From != "msg://user/agent-mux/chris" || filtered.Messages[0].To != "msg://user/agent-mux/chris" {
		t.Fatalf("aggregate/filter: %+v %+v", all, filtered)
	}
	if err := db.SetMessageAlias("msg://user/local/other", "chris-mux"); err != nil {
		t.Fatal(err)
	}
	r = profileRequest(s, http.MethodPost, `{"from_default":"@chris-mux"}`)
	if r.Code != 400 {
		t.Fatal("ambiguous alias accepted")
	}
}

func TestInvalidSavedProfileSurfacesError(t *testing.T) {
	s, _ := inboxFixture(t)
	if err := os.WriteFile(s.userProfilePath, []byte(`{"urn":"msg://user/local/chris","aliases":[],"messaging":{"from_default":"bad"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := profileRequest(s, http.MethodGet, "")
	if r.Code != 503 || !strings.Contains(r.Body.String(), "messaging.from_default") {
		t.Fatalf("silent remap: %d %s", r.Code, r.Body.String())
	}
}
