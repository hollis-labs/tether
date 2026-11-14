package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil/migrateddb"
)

const meLogical = "msg://agent/agent-mux/photon-tether"

// seatServer runs the API in enforce mode with the caller authenticated as a
// session whose credential lists only addrs.
func seatServer(t *testing.T, db *store.Store, addrs ...string) *httptest.Server {
	t.Helper()
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"archie":        {ID: "archie", URN: archieURN},
	}}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}, IdentityMode: identity.Enforce})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := identity.Principal{Verified: true, Kind: "session", ID: "msg://session/local/s1", Addresses: append([]string{"msg://session/local/s1"}, addrs...), Scopes: []string{"message.write", "message.read", "read"}}
		h.ServeHTTP(w, r.WithContext(identity.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seatSend(t *testing.T, db *store.Store, from, to string) string {
	t.Helper()
	env, err := db.MessagingStore().Send(t.Context(), messaging.Envelope{Kind: messaging.MsgKindRequest, From: mustURN(t, from), To: mustURN(t, to), Payload: json.RawMessage(`{"body":"q"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return env.ID
}

func mustURN(t *testing.T, u string) messaging.Address {
	t.Helper()
	a, err := messaging.ParseURN(u)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func seatCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.Bytes()
}

// A seat reads mail sent to its catalog urn using its logical form, and mail
// sent to its logical form using its catalog urn (CW-20261114-0013).
func TestSeatReadsItsMailUnderEitherAddress(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "seat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Credential knows only the logical form; mail went to the catalog urn.
	logicalSeat := seatServer(t, db, meLogical)
	id := seatSend(t, db, archieURN, meURN)
	if code, raw := seatCall(t, logicalSeat, http.MethodGet, "/messages/"+id+"?as="+meLogical, ""); code != http.StatusOK {
		t.Fatalf("get as logical: %d %s", code, raw)
	}
	code, raw := seatCall(t, logicalSeat, http.MethodGet, "/messages/list?to="+meLogical+"&as="+meLogical, "")
	var list struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &list) != nil || len(list.Messages) != 1 || list.Messages[0].ID != id {
		t.Fatalf("list as logical: %d %s", code, raw)
	}
	if code, raw := seatCall(t, logicalSeat, http.MethodPost, "/messages/"+id+"/read?as="+meLogical, ""); code/100 != 2 {
		t.Fatalf("mark read as logical: %d %s", code, raw)
	}

	// Credential knows only the catalog urn; mail was addressed to the logical form.
	urnSeat := seatServer(t, db, meURN)
	body, _ := json.Marshal(map[string]any{"from": archieURN, "to": meLogical, "kind": "request", "payload": map[string]string{"body": "q2"}})
	archie := seatServer(t, db, archieURN)
	code, raw = seatCall(t, archie, http.MethodPost, "/messages", string(body))
	var sent messaging.Envelope
	if code != http.StatusCreated || json.Unmarshal(raw, &sent) != nil {
		t.Fatalf("send to logical: %d %s", code, raw)
	}
	if sent.To.URN() != meURN {
		t.Fatalf("mail to the logical form stored under %s, want the catalog urn %s", sent.To.URN(), meURN)
	}
	if code, raw := seatCall(t, urnSeat, http.MethodGet, "/messages/"+sent.ID+"?as="+meURN, ""); code != http.StatusOK {
		t.Fatalf("get as catalog urn: %d %s", code, raw)
	}
	// Another seat's address is still refused.
	if code, _ := seatCall(t, urnSeat, http.MethodGet, "/messages/"+sent.ID+"?as="+archieURN, ""); code != http.StatusForbidden {
		t.Fatalf("as another seat: %d", code)
	}
}

// With a registry, the logical form maps through the tether id, and a
// deprecated duplicate's merged_into is followed.
func TestCanonicalSeatFollowsRegistry(t *testing.T) {
	reg := registry.NewService(registry.NewStorage(migrateddb.Open(t)))
	ctx := t.Context()
	seat, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Builder"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AttachExternalID(ctx, seat.URN, "tether", "cmd-builder-2"); err != nil {
		t.Fatal(err)
	}
	s := &Server{Registry: reg}
	if got := s.canonicalSeat(ctx, logicalAgentPrefix+"cmd-builder-2"); got != seat.URN {
		t.Fatalf("canonical = %s, want %s", got, seat.URN)
	}
	if got := s.canonicalSeat(ctx, seat.URN); got != seat.URN {
		t.Fatalf("catalog urn not stable: %s", got)
	}
	if forms := s.seatForms(ctx, seat.URN); len(forms) != 2 || forms[1] != logicalAgentPrefix+"cmd-builder-2" {
		t.Fatalf("forms = %v", forms)
	}
	if !s.sameSeat(ctx, logicalAgentPrefix+"cmd-builder-2", seat.URN) || s.sameSeat(ctx, logicalAgentPrefix+"other", seat.URN) {
		t.Fatal("sameSeat")
	}
	// A deprecated duplicate holding the tether id points at its merge target.
	dup, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Dup"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AttachExternalID(ctx, dup.URN, "tether", "cmd-builder-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Merge(ctx, dup.URN, seat.URN); err == nil {
		// Merge moves the tether id onto seat when seat lacks it; either way the
		// logical form resolves to seat.
		if got := s.canonicalSeat(ctx, logicalAgentPrefix+"cmd-builder-3"); got != seat.URN {
			t.Fatalf("after merge canonical = %s, want %s", got, seat.URN)
		}
	}
}
