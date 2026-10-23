package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261023-0018, receive-only: under enforce, a session may use ?as= for its own agent
// instance mailbox and no other; it still cannot send as it, and a grant on a logical
// agent's address does not reach another agent's instances.
func TestInstanceMailboxIdentityUnderEnforce(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := &fakeLaunchService{
		getRes: map[string]*store.SessionRow{
			"w1": {ID: "w1", State: string(session.StateRunning), LogicalAgentID: "worker"},
			"w2": {ID: "w2", State: string(session.StateRunning), LogicalAgentID: "worker"},
			"g1": {ID: "g1", State: string(session.StateRunning), LogicalAgentID: "general"},
		},
		runtimeHealthOK: map[string]bool{"w1": true, "w2": true, "g1": true},
	}
	api := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore(), IdentityMode: identity.Enforce})
	principals := map[string]identity.Principal{
		"w1": {ID: "msg://session/local/w1", Kind: "session", Verified: true, SessionID: "w1", Scopes: []string{"message.write"}, Addresses: []string{"msg://session/local/w1"}},
		// A device that holds the worker agent's address and nothing else.
		"agent-worker": {ID: "msg://device/d1", Kind: "device", Verified: true, Scopes: []string{"operate"}, Addresses: []string{"msg://agent/agent-mux/worker"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	inbox := func(principal, mailbox string) int {
		t.Helper()
		q := url.Values{"to": {mailbox}, "as": {mailbox}}
		req, _ := http.NewRequest("GET", srv.URL+"/messages/inbox?"+q.Encode(), nil)
		req.Header.Set("X-Test-Principal", principal)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	for name, tc := range map[string]struct {
		principal, mailbox string
		want               int
	}{
		"own instance mailbox":                                                 {"w1", "msg://agent/agent-mux/worker/w1", http.StatusOK},
		"a sibling's instance mailbox":                                         {"w1", "msg://agent/agent-mux/worker/w2", http.StatusForbidden},
		"own session id under another agent":                                   {"w1", "msg://agent/agent-mux/general/w1", http.StatusForbidden},
		"another agent's instance mailbox":                                     {"w1", "msg://agent/agent-mux/general/g1", http.StatusForbidden},
		"agent-name prefix collision":                                          {"w1", "msg://agent/agent-mux/worker-2/w1", http.StatusForbidden},
		"the agent's bare mailbox is not covered":                              {"w1", "msg://agent/agent-mux/worker", http.StatusForbidden},
		"a grant on the agent address (not a session) cannot read an instance": {"agent-worker", "msg://agent/agent-mux/worker/w1", http.StatusForbidden},
		"a grant on the agent address still reads the bare mailbox":            {"agent-worker", "msg://agent/agent-mux/worker", http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			if got := inbox(tc.principal, tc.mailbox); got != tc.want {
				t.Fatalf("GET inbox as %s by %s = %d, want %d", tc.mailbox, tc.principal, got, tc.want)
			}
		})
	}

	// Receive-only: the same session cannot send as its instance address.
	body := `{"kind":"notice","from":"msg://agent/agent-mux/worker/w1","to":"msg://agent/test/target","payload":{"text":"x"}}`
	req, _ := http.NewRequest("POST", srv.URL+"/messages", bytes.NewBufferString(body))
	req.Header.Set("X-Test-Principal", "w1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("send as the instance address = %d, want 403 (receive-only)", resp.StatusCode)
	}
}
