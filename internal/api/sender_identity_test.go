package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

var senderPrincipals = map[string]identity.Principal{
	"operator":     {ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}, Addresses: []string{identity.OperatorID}},
	"session":      {ID: "msg://session/local/s1", Kind: "session", SessionID: "s1", Scopes: []string{"message.write"}, Addresses: []string{"msg://session/local/s1"}},
	"device":       {ID: "msg://device/d1", Kind: "device", Scopes: []string{"operate"}, Addresses: []string{}},
	"agentsession": {ID: "msg://session/local/s3", Kind: "session", SessionID: "s3", Scopes: []string{"message.write"}, Addresses: []string{"msg://session/local/s3", "msg://agent/agent-mux/builder"}},
	"sendas":       {ID: "msg://session/local/s2", Kind: "session", SessionID: "s2", Scopes: []string{"message.write", identity.ScopeSendAs}, Addresses: []string{"msg://session/local/s2"}},
}

type senderHarness struct {
	srv    *httptest.Server
	db     *store.Store
	mu     sync.Mutex
	events []identity.Observation
}

func (h *senderHarness) eventNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events {
		out = append(out, e.Event)
	}
	return out
}

func newSenderHarness(t *testing.T, mode identity.Mode) *senderHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "sender.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := &senderHarness{db: db}
	svc := &fakeLaunchService{
		getRes:          map[string]*store.SessionRow{"w": {ID: "w", State: string(session.StateRunning), LogicalAgentID: "worker"}},
		runtimeHealthOK: map[string]bool{"w": true},
	}
	api := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore(), IdentityMode: mode,
		IdentityAudit: func(_ context.Context, o identity.Observation) error {
			h.mu.Lock()
			h.events = append(h.events, o)
			h.mu.Unlock()
			return nil
		}})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := senderPrincipals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *senderHarness) do(t *testing.T, method, path, principal, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader([]byte(body)))
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func envelopeFrom(t *testing.T, raw []byte, notify bool) messaging.Envelope {
	t.Helper()
	var env messaging.Envelope
	if notify {
		var out messageNotifyResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode notify: %v (%s)", err, raw)
		}
		return out.Message
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	return env
}

func TestSenderIdentity_MissingFromIsStamped(t *testing.T) {
	for _, tc := range []struct{ principal, want string }{
		{"operator", identity.OperatorID},
		{"session", "msg://session/local/s1"},
		{"device", "msg://service/device/d1"},
	} {
		for _, route := range []string{"/messages", "/messages/notify"} {
			t.Run(tc.principal+route, func(t *testing.T) {
				h := newSenderHarness(t, identity.Observe)
				code, raw := h.do(t, http.MethodPost, route, tc.principal,
					`{"to":"msg://agent/local/worker","kind":"notice","payload":{"text":"hi"},"wake":false}`)
				if code != http.StatusCreated {
					t.Fatalf("status = %d: %s", code, raw)
				}
				env := envelopeFrom(t, raw, route == "/messages/notify")
				if env.From.URN() != tc.want {
					t.Fatalf("from = %q, want %q", env.From.URN(), tc.want)
				}
				if env.Metadata[MetaFromVerification] != "stamped" || env.Metadata[MetaFromPrincipal] != senderPrincipals[tc.principal].ID || env.Metadata[MetaFromUnverified] != "" {
					t.Fatalf("metadata = %v", env.Metadata)
				}
				if len(h.eventNames()) != 0 {
					t.Fatalf("unexpected audit events %v", h.eventNames())
				}
			})
		}
	}
}

func TestSenderIdentity_OwnFromIsVerifiedAndCallerMetadataIsStripped(t *testing.T) {
	h := newSenderHarness(t, identity.Enforce)
	code, raw := h.do(t, http.MethodPost, "/messages", "session",
		`{"from":"msg://session/local/s1","to":"msg://agent/local/worker","kind":"notice","metadata":{"from_unverified":"false","from_verification":"stamped","from_principal":"msg://user/local/operator"}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	env := envelopeFrom(t, raw, false)
	if env.Metadata[MetaFromVerification] != "verified" || env.Metadata[MetaFromPrincipal] != "msg://session/local/s1" || env.Metadata[MetaFromUnverified] != "" {
		t.Fatalf("metadata = %v", env.Metadata)
	}
}

func TestSenderIdentity_SpoofedFrom(t *testing.T) {
	const spoof = `{"from":"msg://agent/local/impersonated","to":"msg://agent/local/worker","kind":"notice","metadata":{"from_verification":"verified"},"wake":false}`
	for _, route := range []string{"/messages", "/messages/notify"} {
		t.Run("observe"+route, func(t *testing.T) {
			h := newSenderHarness(t, identity.Observe)
			code, raw := h.do(t, http.MethodPost, route, "session", spoof)
			if code != http.StatusCreated {
				t.Fatalf("observe must keep the message: status = %d: %s", code, raw)
			}
			env := envelopeFrom(t, raw, route == "/messages/notify")
			if env.From.URN() != "msg://agent/local/impersonated" {
				t.Fatalf("from = %q", env.From.URN())
			}
			if env.Metadata[MetaFromUnverified] != "true" || env.Metadata[MetaFromVerification] != "unverified" || env.Metadata[MetaFromPrincipal] != "msg://session/local/s1" {
				t.Fatalf("metadata = %v", env.Metadata)
			}
			if got := h.eventNames(); len(got) != 1 || got[0] != "from_unverified" {
				t.Fatalf("audit events = %v", got)
			}
			if h.events[0].PrincipalID != "msg://session/local/s1" || h.events[0].Route != "/messages" {
				t.Fatalf("audit row = %+v", h.events[0])
			}
		})
		t.Run("enforce"+route, func(t *testing.T) {
			h := newSenderHarness(t, identity.Enforce)
			code, raw := h.do(t, http.MethodPost, route, "session", spoof)
			if code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", code, raw)
			}
			if got := h.eventNames(); len(got) != 1 || got[0] != "from_refused" {
				t.Fatalf("audit events = %v", got)
			}
			page, err := h.db.MessagingStore().List(context.Background(), messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "worker"}, store.ListFilter{})
			if err != nil || page.Total != 0 {
				t.Fatalf("refused message was stored: total=%d err=%v", page.Total, err)
			}
		})
	}
}

func TestSenderIdentity_SendAsGrantAndOperator(t *testing.T) {
	h := newSenderHarness(t, identity.Enforce)
	for _, principal := range []string{"sendas", "operator"} {
		code, raw := h.do(t, http.MethodPost, "/messages", principal,
			`{"from":"msg://agent/local/delegate","to":"msg://agent/local/worker","kind":"notice"}`)
		if code != http.StatusCreated {
			t.Fatalf("%s: status = %d: %s", principal, code, raw)
		}
		if env := envelopeFrom(t, raw, false); env.Metadata[MetaFromVerification] != "verified" {
			t.Fatalf("%s: metadata = %v", principal, env.Metadata)
		}
	}
}

func TestSenderIdentity_AnonymousIsUntouched(t *testing.T) {
	h := newSenderHarness(t, identity.Observe)
	code, raw := h.do(t, http.MethodPost, "/messages", "",
		`{"from":"msg://agent/local/anyone","to":"msg://agent/local/worker","kind":"notice","metadata":{"from_unverified":"x"}}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, raw)
	}
	env := envelopeFrom(t, raw, false)
	if env.From.URN() != "msg://agent/local/anyone" || env.Metadata[MetaFromVerification] != "" || env.Metadata[MetaFromUnverified] != "" {
		t.Fatalf("anonymous send altered: from=%q metadata=%v", env.From.URN(), env.Metadata)
	}
}

func TestSenderIdentity_AsVerification(t *testing.T) {
	owner := messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "owner"}
	send := func(t *testing.T, h *senderHarness) string {
		env, err := h.db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice,
			From: messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "sender"}, To: owner})
		if err != nil {
			t.Fatal(err)
		}
		return env.ID
	}
	t.Run("observe flags and serves", func(t *testing.T) {
		h := newSenderHarness(t, identity.Observe)
		id := send(t, h)
		code, raw := h.do(t, http.MethodGet, "/messages/"+id+"?as="+owner.URN(), "session", "")
		if code != http.StatusOK {
			t.Fatalf("status = %d: %s", code, raw)
		}
		if got := h.eventNames(); len(got) != 1 || got[0] != "as_unverified" {
			t.Fatalf("audit events = %v", got)
		}
	})
	t.Run("enforce refuses", func(t *testing.T) {
		h := newSenderHarness(t, identity.Enforce)
		id := send(t, h)
		for _, path := range []string{
			"/messages/" + id + "?as=" + owner.URN(),
			"/messages/inbox?to=" + owner.URN() + "&as=" + owner.URN(),
			"/messages/list?to=" + owner.URN() + "&as=" + owner.URN(),
			"/messages/subscribe?to=" + owner.URN() + "&as=" + owner.URN(),
		} {
			if code, raw := h.do(t, http.MethodGet, path, "session", ""); code != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403: %s", path, code, raw)
			}
		}
		if code, _ := h.do(t, http.MethodPost, "/messages/"+id+"/read?as="+owner.URN(), "session", ""); code != http.StatusForbidden {
			t.Fatalf("read: status = %d, want 403", code)
		}
	})
	t.Run("own address, send_as and operator pass in enforce", func(t *testing.T) {
		h := newSenderHarness(t, identity.Enforce)
		id := send(t, h)
		if code, raw := h.do(t, http.MethodGet, "/messages/"+id+"?as="+owner.URN(), "operator", ""); code != http.StatusOK {
			t.Fatalf("operator: status = %d: %s", code, raw)
		}
		if code, raw := h.do(t, http.MethodGet, "/messages/"+id+"?as="+owner.URN(), "sendas", ""); code != http.StatusOK {
			t.Fatalf("send_as: status = %d: %s", code, raw)
		}
		own, err := h.db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice,
			From: owner, To: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}})
		if err != nil {
			t.Fatal(err)
		}
		if code, raw := h.do(t, http.MethodGet, "/messages/"+own.ID+"?as=msg://session/local/s1", "session", ""); code != http.StatusOK {
			t.Fatalf("own address: status = %d: %s", code, raw)
		}
		if len(h.eventNames()) != 0 {
			t.Fatalf("unexpected audit events %v", h.eventNames())
		}
	})
}

// A routed reply sent through POST /messages or notify keeps the stamped
// sender verification on the stored and returned reply (acceptance 5).
func TestSenderIdentity_RoutedReplyCarriesVerification(t *testing.T) {
	for _, route := range []string{"/messages", "/messages/notify"} {
		for _, tc := range []struct {
			name, from, state string
			unverified        string
		}{
			{"stamped", "", "stamped", ""},
			{"verified", "msg://session/local/s9", "verified", ""},
			{"unverified", "msg://agent/local/impersonated", "unverified", "true"},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				svc := &fakeReplies{}
				db, err := store.Open(filepath.Join(t.TempDir(), "reply.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				svc.db = db
				api := NewHandler(Deps{MessageStore: db.MessagingStore(), Channels: channels.New(db, nil), RoutingReplies: svc, Service: &fakeLaunchService{}, IdentityMode: identity.Observe})
				parent := routedMessage(t, db)
				p := identity.Principal{ID: "msg://session/local/s9", Kind: "session", SessionID: "s9", Addresses: []string{"msg://session/local/s9"}}
				body := map[string]any{"kind": "response", "in_reply_to": parent.ID, "payload": map[string]string{"body": "go"}, "metadata": map[string]string{"from_unverified": "false"}}
				if tc.from != "" {
					body["from"] = tc.from
				}
				if route == "/messages/notify" {
					body["to"] = parent.From.URN()
				}
				w := postAs(identity.WithPrincipal(context.Background(), p), api, route, body, nil)
				if w.Code != http.StatusCreated {
					t.Fatalf("status %d %s", w.Code, w.Body)
				}
				var out struct {
					messaging.Envelope
					Message messaging.Envelope `json:"message"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				for _, md := range []map[string]string{out.Metadata, out.Message.Metadata} {
					if md == nil {
						continue
					}
					if md[MetaFromVerification] != tc.state || md[MetaFromPrincipal] != p.ID || md[MetaFromUnverified] != tc.unverified {
						t.Fatalf("response metadata = %v", md)
					}
				}
				if out.Metadata == nil && out.Message.Metadata == nil {
					t.Fatalf("no envelope metadata in %s", w.Body)
				}
				stored, err := db.MessagingStore().Get(context.Background(), svc.lastReplyID(t, db, parent.ID))
				if err != nil {
					t.Fatal(err)
				}
				if stored.Metadata[MetaFromVerification] != tc.state || stored.Metadata[MetaFromUnverified] != tc.unverified {
					t.Fatalf("stored metadata = %v", stored.Metadata)
				}
			})
		}
	}
}

func (f *fakeReplies) lastReplyID(t *testing.T, db *store.Store, parent string) string {
	t.Helper()
	var id string
	if err := db.DB().QueryRow(`SELECT id FROM messages WHERE in_reply_to=? ORDER BY created_at DESC LIMIT 1`, parent).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A session credential's address set includes its logical agent (see
// app.mintSessionCredential), so the common agent case works in enforce.
func TestSenderIdentity_SessionSendsAndReadsAsItsOwnAgent(t *testing.T) {
	const own, other = "msg://agent/agent-mux/builder", "msg://agent/agent-mux/reviewer"
	send := func(from string) string {
		return `{"from":"` + from + `","to":"msg://agent/local/worker","kind":"notice"}`
	}
	t.Run("enforce", func(t *testing.T) {
		h := newSenderHarness(t, identity.Enforce)
		code, raw := h.do(t, http.MethodPost, "/messages", "agentsession", send(own))
		if code != http.StatusCreated {
			t.Fatalf("own agent: status = %d: %s", code, raw)
		}
		if env := envelopeFrom(t, raw, false); env.Metadata[MetaFromVerification] != "verified" {
			t.Fatalf("metadata = %v", env.Metadata)
		}
		if code, raw := h.do(t, http.MethodPost, "/messages", "agentsession", send(other)); code != http.StatusForbidden {
			t.Fatalf("other agent: status = %d, want 403: %s", code, raw)
		}
		for as, want := range map[string]int{own: http.StatusOK, other: http.StatusForbidden} {
			if code, raw := h.do(t, http.MethodGet, "/messages/list?to="+as+"&as="+as, "agentsession", ""); code != want {
				t.Fatalf("list as %s: status = %d, want %d: %s", as, code, want, raw)
			}
		}
	})
	t.Run("observe", func(t *testing.T) {
		h := newSenderHarness(t, identity.Observe)
		code, raw := h.do(t, http.MethodPost, "/messages", "agentsession", send(other))
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, raw)
		}
		if env := envelopeFrom(t, raw, false); env.Metadata[MetaFromUnverified] != "true" {
			t.Fatalf("metadata = %v", env.Metadata)
		}
		if code, _ := h.do(t, http.MethodGet, "/messages/list?to="+other+"&as="+other, "agentsession", ""); code != http.StatusOK {
			t.Fatalf("observe list as other: status = %d", code)
		}
		if got := h.eventNames(); len(got) != 2 || got[0] != "from_unverified" || got[1] != "as_unverified" {
			t.Fatalf("audit events = %v", got)
		}
	})
}
