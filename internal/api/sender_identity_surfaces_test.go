package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/testutil/migrateddb"

	"github.com/hollis-labs/tether/internal/broker"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// principalServer serves h with the named senderPrincipals injected as the
// verified principal, the way the identity middleware would.
func principalServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := senderPrincipals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type auditLog struct {
	mu     sync.Mutex
	events []string
}

func (a *auditLog) record(_ context.Context, o identity.Observation) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, o.Event)
	return nil
}

func (a *auditLog) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

func doJSON(t *testing.T, srv *httptest.Server, method, path, principal string, body any) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func groupFixture(t *testing.T, mode identity.Mode) (srv *httptest.Server, audit *auditLog, svc *registry.Service) {
	t.Helper()
	db := migrateddb.Open(t)
	svc = registry.NewService(registry.NewStorage(db))
	audit = &auditLog{}
	srv = principalServer(t, NewHandler(Deps{Registry: svc, Groups: svc, IdentityMode: mode, IdentityAudit: audit.record}))
	return srv, audit, svc
}

func seedAgentProfile(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	code, raw := doJSON(t, srv, http.MethodPost, "/registry/agents", "", registry.Profile{DisplayName: name, LastUpdatedBy: "tester"})
	var p registry.Profile
	if code != http.StatusCreated || json.Unmarshal(raw, &p) != nil {
		t.Fatalf("seed agent: %d %s", code, raw)
	}
	return p.URN
}

// The caller authenticates as one registered agent and claims another that IS
// a member of the target group (the case the old t05 test missed, which only
// used a member of a different group).
func TestGroupRoutes_ImpersonatingAMemberOfTheTargetGroup(t *testing.T) {
	setup := func(t *testing.T, mode identity.Mode) (*httptest.Server, *auditLog, string, string, string) {
		srv, audit, _ := groupFixture(t, mode)
		owner := seedAgentProfile(t, srv, "Owner")
		attacker := seedAgentProfile(t, srv, "Attacker")
		code, raw := doJSON(t, srv, http.MethodPost, "/groups", "", registry.Profile{DisplayName: "G", LastUpdatedBy: owner})
		var g registry.Profile
		if code != http.StatusCreated || json.Unmarshal(raw, &g) != nil {
			t.Fatalf("create group: %d %s", code, raw)
		}
		// The attacker is a member of its own address only: its principal's
		// address set is its own URN.
		senderPrincipals["groupattacker"] = identity.Principal{ID: attacker, Kind: "session", Verified: true, Addresses: []string{attacker}}
		t.Cleanup(func() { delete(senderPrincipals, "groupattacker") })
		return srv, audit, owner, attacker, g.URN
	}
	send := func(srv *httptest.Server, group, from string) (int, []byte) {
		body := map[string]any{"kind": "message", "payload": map[string]string{"text": "x"}}
		if from != "" {
			body["from"] = from
		}
		return doJSON(t, srv, http.MethodPost, itemURLGroup(group)+"/messages", "groupattacker", body)
	}

	t.Run("enforce refuses a send claiming the owner", func(t *testing.T) {
		srv, audit, owner, _, group := setup(t, identity.Enforce)
		if code, raw := send(srv, group, owner); code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", code, raw)
		}
		if got := audit.names(); len(got) != 1 || got[0] != "from_refused" {
			t.Fatalf("audit = %v", got)
		}
	})
	t.Run("enforce refuses the other claim-carrying group routes", func(t *testing.T) {
		srv, audit, owner, attacker, group := setup(t, identity.Enforce)
		for _, tc := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, itemURLGroup(group) + "/messages?as=" + owner, nil},
			{http.MethodGet, "/mentions?as=" + owner, nil},
			{http.MethodGet, "/groups?member=" + owner, nil},
			{http.MethodPost, itemURLGroup(group) + "/read", map[string]any{"as": owner, "up_to_seq": 1}},
			{http.MethodPost, itemURLGroup(group) + "/members", map[string]any{"member": attacker, "by": owner}},
			{http.MethodPost, itemURLGroup(group) + "/leave", map[string]any{"member": owner}},
			{http.MethodPatch, itemURLGroup(group) + "/members/" + url.PathEscape(attacker), map[string]any{"role": "admin", "by": owner}},
			{http.MethodDelete, itemURLGroup(group) + "/members/" + url.PathEscape(owner), map[string]any{"by": owner}},
			{http.MethodDelete, itemURLGroup(group) + "?as=" + owner, nil},
			{http.MethodPost, "/groups", registry.Profile{DisplayName: "H", LastUpdatedBy: owner}},
		} {
			if code, raw := doJSON(t, srv, tc.method, tc.path, "groupattacker", tc.body); code != http.StatusForbidden {
				t.Errorf("%s %s: status = %d, want 403: %s", tc.method, tc.path, code, raw)
			}
		}
		// Each refusal came from the identity guard, not from the service's own
		// membership rules.
		if got := audit.names(); len(got) != 10 {
			t.Errorf("audit = %v, want one as_refused per request", got)
		} else {
			for _, e := range got {
				if e != "as_refused" {
					t.Errorf("audit event %q", e)
				}
			}
		}
	})
	t.Run("observe records the claim and lets the service decide", func(t *testing.T) {
		srv, audit, owner, _, group := setup(t, identity.Observe)
		if code, raw := send(srv, group, owner); code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, raw)
		}
		if got := audit.names(); len(got) != 1 || got[0] != "from_unverified" {
			t.Fatalf("audit = %v", got)
		}
	})
	t.Run("a missing from is stamped from the principal", func(t *testing.T) {
		srv, audit, _, attacker, group := setup(t, identity.Enforce)
		// The attacker is not a member: the service answers 403 for the
		// stamped address itself, proving the stamp (not a 400 for a missing
		// from) reached the membership check.
		if code, raw := send(srv, group, ""); code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 from membership: %s", code, raw)
		}
		code, raw := doJSON(t, srv, http.MethodPost, itemURLGroup(group)+"/members", "groupattacker", map[string]any{"member": attacker})
		if code == http.StatusBadRequest {
			t.Fatalf("by was not stamped: %d %s", code, raw)
		}
		if len(audit.names()) != 0 {
			t.Fatalf("unexpected audit %v", audit.names())
		}
	})
}

func brokerFixture(t *testing.T, mode identity.Mode) (*httptest.Server, *fakeBroker, *auditLog) {
	t.Helper()
	fb := &fakeBroker{}
	audit := &auditLog{}
	srv := principalServer(t, NewHandler(Deps{Broker: fb, IdentityMode: mode, IdentityAudit: audit.record}))
	return srv, fb, audit
}

func TestBrokerRoutes_SenderIsStampedAndVerified(t *testing.T) {
	body := func(sender string) map[string]any {
		m := map[string]any{"recipient": "msg://agent/local/worker", "message_type": "notice", "payload": "hi"}
		if sender != "" {
			m["sender"] = sender
		}
		return m
	}
	for _, route := range []string{"/broker/envelopes", "/broker/requests?wait=false"} {
		t.Run(route, func(t *testing.T) {
			srv, fb, audit := brokerFixture(t, identity.Enforce)
			code, raw := doJSON(t, srv, http.MethodPost, route, "session", body(""))
			if code >= 300 || len(fb.created) != 1 || fb.created[0].Sender != "msg://session/local/s1" {
				t.Fatalf("stamp: %d %s created=%+v", code, raw, fb.created)
			}
			if code, raw = doJSON(t, srv, http.MethodPost, route, "session", body("msg://session/local/s1")); code >= 300 {
				t.Fatalf("own sender: %d %s", code, raw)
			}
			if code, raw = doJSON(t, srv, http.MethodPost, route, "session", body("msg://agent/local/boss")); code != http.StatusForbidden {
				t.Fatalf("spoof: %d, want 403: %s", code, raw)
			}
			if len(fb.created) != 2 {
				t.Fatalf("refused envelope was created: %d", len(fb.created))
			}
			if got := audit.names(); len(got) != 1 || got[0] != "from_refused" {
				t.Fatalf("audit = %v", got)
			}
		})
		t.Run(route+"/observe", func(t *testing.T) {
			srv, fb, audit := brokerFixture(t, identity.Observe)
			if code, raw := doJSON(t, srv, http.MethodPost, route, "session", body("msg://agent/local/boss")); code >= 300 {
				t.Fatalf("observe must keep it: %d %s", code, raw)
			}
			if len(fb.created) != 1 || fb.created[0].Sender != "msg://agent/local/boss" {
				t.Fatalf("created = %+v", fb.created)
			}
			if got := audit.names(); len(got) != 1 || got[0] != "from_unverified" {
				t.Fatalf("audit = %v", got)
			}
		})
	}
}

func TestBrokerRoutes_MailboxAsAndReplyAreBoundToThePrincipal(t *testing.T) {
	srv, fb, audit := brokerFixture(t, identity.Enforce)
	fb.rows = map[string]*broker.Envelope{"e1": {ID: "e1", Sender: "msg://agent/local/boss", Recipient: "msg://agent/local/victim", MessageType: "notice"}}
	fb.byRecip = map[string][]broker.Envelope{"msg://agent/local/victim": {*fb.rows["e1"]}}

	for _, path := range []string{
		"/broker/envelopes?recipient=msg://agent/local/victim&as=msg://agent/local/victim",
		"/broker/envelopes/e1?as=msg://agent/local/victim",
	} {
		if code, raw := doJSON(t, srv, http.MethodGet, path, "session", nil); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403: %s", path, code, raw)
		}
	}
	if code, raw := doJSON(t, srv, http.MethodPost, "/broker/envelopes/e1/reply", "session", map[string]any{"payload": "x"}); code != http.StatusForbidden {
		t.Errorf("reply as the original recipient: %d, want 403: %s", code, raw)
	}
	if len(fb.replied) != 0 {
		t.Fatalf("refused reply was stored")
	}
	if code, raw := doJSON(t, srv, http.MethodGet, "/broker/envelopes?recipient=msg://session/local/s1&as=msg://session/local/s1", "session", nil); code != http.StatusOK {
		t.Errorf("own mailbox: %d %s", code, raw)
	}
	if len(audit.names()) < 3 {
		t.Fatalf("audit = %v", audit.names())
	}
}

// A device principal's ID (msg://device/<id>) is not a messaging address. Its
// routed replies are attributed to the stamped sender, in observe and enforce.
func TestDeviceRoutedReplies(t *testing.T) {
	for _, mode := range []identity.Mode{identity.Observe, identity.Enforce} {
		for _, route := range []string{"/messages", "/messages/notify"} {
			t.Run(string(mode)+route, func(t *testing.T) {
				svc := &fakeReplies{}
				db, err := store.Open(filepath.Join(t.TempDir(), "device.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				svc.db = db
				srv := principalServer(t, NewHandler(Deps{MessageStore: db.MessagingStore(), Channels: channels.New(db, nil),
					RoutingReplies: svc, Service: &fakeLaunchService{}, IdentityMode: mode}))
				parent := routedMessage(t, db)
				body := map[string]any{"kind": "response", "in_reply_to": parent.ID, "payload": map[string]string{"body": "go"}}
				if route == "/messages/notify" {
					body["to"] = parent.From.URN()
				}
				code, raw := doJSON(t, srv, http.MethodPost, route, "device", body)
				if code != http.StatusCreated {
					t.Fatalf("status %d %s", code, raw)
				}
				if got := svc.requests[0]; got.Caller.ID != "msg://device/d1" || got.Sender != "msg://service/device/d1" || !got.Verified {
					t.Fatalf("request %+v", got)
				}
				var msg struct {
					From     string            `json:"from"`
					Metadata map[string]string `json:"metadata"`
					Message  struct {
						From     string            `json:"from"`
						Metadata map[string]string `json:"metadata"`
					} `json:"message"`
				}
				if err := json.Unmarshal(raw, &msg); err != nil {
					t.Fatal(err)
				}
				from, md := msg.From, msg.Metadata
				if route == "/messages/notify" {
					from, md = msg.Message.From, msg.Message.Metadata
				}
				if from != "msg://service/device/d1" || md[MetaFromVerification] != "stamped" || md[MetaFromPrincipal] != "msg://device/d1" {
					t.Fatalf("reply from=%q metadata=%v", from, md)
				}
			})
		}
	}
}
