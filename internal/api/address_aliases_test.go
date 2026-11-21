package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	gomsg "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// Address aliases over HTTP (CW-20261114-0067): the migration the task is
// for, on a synthetic mailbox. The operator principal's address and a registry
// agent row become aliases of one chosen user address.

const aliasCanonicalURN = "msg://user/local/chrispian"

type aliasHarness struct {
	srv *httptest.Server
	// principals are this harness's own test principals, consulted before senderPrincipals.
	principals map[string]identity.Principal
	db         *store.Store
	reg        *registry.Service
	agentURN   string // the registry agent row with display_name chrispian
}

func newAliasHarness(t *testing.T, mode identity.Mode) *aliasHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "alias.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	reg := registry.NewService(registry.NewStorage(db.DB()))
	// The daemon's wiring: group mentions are emitted through the local store.
	reg.SetMentionParser(messaging.NewParser(reg, db.MessagingStore()))
	row, err := reg.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "chrispian", Role: "team-operator", LastUpdatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{Agents: map[string]config.Agent{"archie": {ID: "archie", URN: archieURN}}}
	api := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Aliases: db,
		Registry: reg, Groups: reg, Catalog: fixedCatalog{cat}, IdentityMode: mode})
	h := &aliasHarness{db: db, reg: reg, agentURN: row.URN, principals: map[string]identity.Principal{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := h.principals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		} else if p, ok := senderPrincipals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *aliasHarness) do(t *testing.T, method, path, principal, body string) (int, []byte) {
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

func (h *aliasHarness) alias(t *testing.T, alias string) {
	t.Helper()
	body, _ := json.Marshal(AddressAliasRequest{Alias: alias, Canonical: aliasCanonicalURN})
	if code, raw := h.do(t, http.MethodPost, "/messages/aliases", "operator", string(body)); code != http.StatusOK {
		t.Fatalf("alias %s: %d %s", alias, code, raw)
	}
}

func (h *aliasHarness) migrate(t *testing.T) {
	t.Helper()
	h.alias(t, identity.OperatorID)
	h.alias(t, h.agentURN)
}

func (h *aliasHarness) send(t *testing.T, principal, from, to string) gomsg.Envelope {
	t.Helper()
	msg := map[string]any{"kind": "notice", "to": to, "payload": map[string]string{"body": "to " + to}}
	if from != "" {
		msg["from"] = from
	}
	body, _ := json.Marshal(msg)
	code, raw := h.do(t, http.MethodPost, "/messages", principal, string(body))
	if code != http.StatusCreated {
		t.Fatalf("send to %s: %d %s", to, code, raw)
	}
	var env gomsg.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func (h *aliasHarness) listIDs(t *testing.T, principal, to string) []string {
	t.Helper()
	q := url.Values{"to": {to}, "as": {to}, "include_archived": {"true"}}
	code, raw := h.do(t, http.MethodGet, "/messages/list?"+q.Encode(), principal, "")
	if code != http.StatusOK {
		t.Fatalf("list %s: %d %s", to, code, raw)
	}
	var page struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(page.Messages))
	for _, m := range page.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAddressAliasManagementIsOperatorOnly(t *testing.T) {
	h := newAliasHarness(t, identity.Observe)
	body := `{"alias":"` + identity.OperatorID + `","canonical":"` + aliasCanonicalURN + `"}`
	// A session, a device and an unauthenticated caller are all refused, even in
	// observe mode: an alias is not an observation.
	for _, principal := range []string{"session", "agentsession", "sendas", "device", ""} {
		if code, raw := h.do(t, http.MethodPost, "/messages/aliases", principal, body); code != http.StatusForbidden {
			t.Fatalf("add as %q: %d %s", principal, code, raw)
		}
		if code, raw := h.do(t, http.MethodDelete, "/messages/aliases?alias="+url.QueryEscape(identity.OperatorID), principal, ""); code != http.StatusForbidden {
			t.Fatalf("remove as %q: %d %s", principal, code, raw)
		}
	}
	if rows, _ := h.db.ListAddressAliases(context.Background(), ""); len(rows) != 0 {
		t.Fatalf("a refused caller created rows: %+v", rows)
	}
	code, raw := h.do(t, http.MethodPost, "/messages/aliases", "operator", body)
	var added AddressAliasResponse
	if code != http.StatusOK || json.Unmarshal(raw, &added) != nil || added.Alias != identity.OperatorID || added.Canonical != aliasCanonicalURN || added.CreatedBy != identity.OperatorID {
		t.Fatalf("operator add: %d %s", code, raw)
	}
	// Anyone may list; a chain and a re-point are refused with a conflict.
	code, raw = h.do(t, http.MethodGet, "/messages/aliases?canonical="+url.QueryEscape(identity.OperatorID), "session", "")
	var listed struct {
		Aliases []store.AddressAlias `json:"aliases"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &listed) != nil || len(listed.Aliases) != 1 {
		t.Fatalf("list: %d %s", code, raw)
	}
	for _, bad := range []string{
		`{"alias":"` + h.agentURN + `","canonical":"` + identity.OperatorID + `"}`,
		`{"alias":"` + identity.OperatorID + `","canonical":"` + h.agentURN + `"}`,
	} {
		if code, raw := h.do(t, http.MethodPost, "/messages/aliases", "operator", bad); code != http.StatusConflict {
			t.Fatalf("refused add %s: %d %s", bad, code, raw)
		}
	}
	if code, raw := h.do(t, http.MethodDelete, "/messages/aliases?alias="+url.QueryEscape(identity.OperatorID), "operator", ""); code != http.StatusOK {
		t.Fatalf("operator remove: %d %s", code, raw)
	}
	if code, _ := h.do(t, http.MethodDelete, "/messages/aliases?alias="+url.QueryEscape(identity.OperatorID), "operator", ""); code != http.StatusNotFound {
		t.Fatalf("second remove: %d", code)
	}
}

// With identity checks off there is no credential to tell the operator by, so
// the local socket is trusted, as for every other route in that mode.
func TestAddressAliasManagementIdentityOff(t *testing.T) {
	h := newAliasHarness(t, identity.Off)
	body := `{"alias":"` + identity.OperatorID + `","canonical":"` + aliasCanonicalURN + `"}`
	if code, raw := h.do(t, http.MethodPost, "/messages/aliases", "", body); code != http.StatusOK {
		t.Fatalf("add: %d %s", code, raw)
	}
}

// Acceptance items 1-3: three sends, three lists, old mail with its state.
func TestAddressAliasSendAndListThroughEveryAddress(t *testing.T) {
	h := newAliasHarness(t, identity.Enforce)
	ctx := context.Background()
	// Mail from before the migration, some of it read and archived.
	old := []gomsg.Envelope{
		h.send(t, "session", "", identity.OperatorID),
		h.send(t, "session", "", identity.OperatorID),
		h.send(t, "session", "", h.agentURN),
	}
	opAddr, _ := gomsg.ParseURN(identity.OperatorID)
	if err := h.db.MessagingStore().MarkRead(ctx, old[0].ID, opAddr); err != nil {
		t.Fatal(err)
	}
	if err := h.db.MessagingStore().Archive(ctx, old[1].ID, opAddr); err != nil {
		t.Fatal(err)
	}

	h.migrate(t)

	sent := []gomsg.Envelope{
		h.send(t, "session", "", identity.OperatorID),
		h.send(t, "session", "", h.agentURN),
		h.send(t, "session", "", aliasCanonicalURN),
	}
	for _, env := range sent {
		if env.To.URN() != aliasCanonicalURN {
			t.Fatalf("message %s stored under %s", env.ID, env.To.URN())
		}
	}
	want := []string{sent[2].ID, sent[1].ID, sent[0].ID, old[2].ID, old[1].ID, old[0].ID}
	canonical := h.listIDs(t, "operator", aliasCanonicalURN)
	if !equalIDs(canonical, want) {
		t.Fatalf("canonical list = %v, want %v", canonical, want)
	}
	for _, addr := range []string{identity.OperatorID, h.agentURN} {
		if got := h.listIDs(t, "operator", addr); !equalIDs(got, canonical) {
			t.Fatalf("list via %s = %v, want %v", addr, got, canonical)
		}
	}
	// Old mail keeps its addresses and archive state. Listing as the mailbox
	// owner now also records a payload read, through every alias.
	page, err := h.db.MessagingStore().List(ctx, mustURN(t, aliasCanonicalURN), store.ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]store.Message{}
	for _, m := range page.Messages {
		state[m.ID] = m
	}
	if m := state[old[0].ID]; m.ReadAt == nil || m.ArchivedAt != nil || m.To.URN() != identity.OperatorID {
		t.Fatalf("read message = %+v", m)
	}
	if m := state[old[1].ID]; m.ArchivedAt == nil || m.ReadAt == nil || m.To.URN() != identity.OperatorID {
		t.Fatalf("archived message = %+v", m)
	}
	if m := state[old[2].ID]; m.ReadAt == nil || m.ArchivedAt != nil || m.To.URN() != h.agentURN {
		t.Fatalf("agent-row message = %+v", m)
	}

	// Get, thread, read and archive work through any address of the mailbox,
	// and still refuse an address that is not one.
	asOld := url.QueryEscape(identity.OperatorID)
	if code, raw := h.do(t, http.MethodGet, "/messages/"+old[2].ID+"?as="+asOld, "operator", ""); code != http.StatusOK {
		t.Fatalf("get via alias: %d %s", code, raw)
	}
	if code, raw := h.do(t, http.MethodPost, "/messages/"+sent[2].ID+"/read?as="+asOld, "operator", ""); code != http.StatusNoContent {
		t.Fatalf("read via alias: %d %s", code, raw)
	}
	if code, raw := h.do(t, http.MethodPost, "/messages/"+old[2].ID+"/archive?as="+url.QueryEscape(aliasCanonicalURN), "operator", ""); code != http.StatusNoContent {
		t.Fatalf("archive an alias-stored message as the canonical address: %d %s", code, raw)
	}
	if code, raw := h.do(t, http.MethodPost, "/messages/"+sent[0].ID+"/read?as="+url.QueryEscape(archieURN), "operator", ""); code != http.StatusConflict {
		t.Fatalf("read as a stranger: %d %s", code, raw)
	}
	// Enforce mode: a session that owns none of the forms cannot read the mailbox.
	q := url.Values{"to": {aliasCanonicalURN}, "as": {aliasCanonicalURN}}
	if code, raw := h.do(t, http.MethodGet, "/messages/list?"+q.Encode(), "session", ""); code != http.StatusForbidden {
		t.Fatalf("stranger list: %d %s", code, raw)
	}
}

// Acceptance item 5: the operator token's mail keeps the sender it was sent
// with (from_principal resolves to the canonical address through the alias),
// the operator may send as the canonical address, and a reply to either
// arrives in the canonical mailbox.
func TestAddressAliasOperatorSenderAndReply(t *testing.T) {
	h := newAliasHarness(t, identity.Enforce)
	h.migrate(t)

	stamped := h.send(t, "operator", "", archieURN)
	if stamped.From.URN() != identity.OperatorID || stamped.Metadata[MetaFromPrincipal] != identity.OperatorID {
		t.Fatalf("stamped send: from %s metadata %v", stamped.From.URN(), stamped.Metadata)
	}
	if c, err := h.db.CanonicalAddress(context.Background(), stamped.Metadata[MetaFromPrincipal]); err != nil || c != aliasCanonicalURN {
		t.Fatalf("from_principal resolves to %q (%v)", c, err)
	}
	explicit := h.send(t, "operator", aliasCanonicalURN, archieURN)
	if explicit.From.URN() != aliasCanonicalURN || explicit.Metadata[MetaFromVerification] != "verified" {
		t.Fatalf("explicit send: from %s metadata %v", explicit.From.URN(), explicit.Metadata)
	}
	// A reply addressed to whatever the recipient saw as the sender, or to the
	// old address it still has on file.
	for _, to := range []string{stamped.From.URN(), explicit.From.URN()} {
		body, _ := json.Marshal(map[string]any{"kind": "response", "to": to, "in_reply_to": stamped.ID, "payload": map[string]string{"body": "re"}})
		code, raw := h.do(t, http.MethodPost, "/messages", "session", string(body))
		var reply gomsg.Envelope
		if code != http.StatusCreated || json.Unmarshal(raw, &reply) != nil || reply.To.URN() != aliasCanonicalURN {
			t.Fatalf("reply to %s: %d %s", to, code, raw)
		}
	}
	if got := h.listIDs(t, "operator", aliasCanonicalURN); len(got) != 2 {
		t.Fatalf("canonical mailbox = %v, want the two replies", got)
	}
}

// A principal that owns one form may act as the others; one that owns none may not.
func TestAddressAliasPrincipalOwnsEveryForm(t *testing.T) {
	h := newAliasHarness(t, identity.Enforce)
	const builder, renamed = "msg://agent/agent-mux/builder", "msg://agent/agent-mux/agt_builder2"
	body, _ := json.Marshal(AddressAliasRequest{Alias: builder, Canonical: renamed})
	if code, raw := h.do(t, http.MethodPost, "/messages/aliases", "operator", string(body)); code != http.StatusOK {
		t.Fatalf("alias: %d %s", code, raw)
	}
	// agentsession owns the old address only.
	env := h.send(t, "agentsession", renamed, archieURN)
	if env.From.URN() != renamed || env.Metadata[MetaFromVerification] != "verified" {
		t.Fatalf("send as the new address: from %s metadata %v", env.From.URN(), env.Metadata)
	}
	h.send(t, "session", "", builder)
	if got := h.listIDs(t, "agentsession", renamed); len(got) != 1 {
		t.Fatalf("owner of the alias reading the canonical mailbox = %v", got)
	}
	q := url.Values{"to": {renamed}, "as": {renamed}}
	if code, raw := h.do(t, http.MethodGet, "/messages/list?"+q.Encode(), "session", ""); code != http.StatusForbidden {
		t.Fatalf("stranger: %d %s", code, raw)
	}
	bad, _ := json.Marshal(map[string]any{"kind": "notice", "from": renamed, "to": archieURN})
	if code, raw := h.do(t, http.MethodPost, "/messages", "session", string(bad)); code != http.StatusForbidden {
		t.Fatalf("stranger send as: %d %s", code, raw)
	}
}

// Acceptance item 4: a typed message to @chrispian and a group post mentioning
// @chrispian both reach the canonical mailbox.
func TestAddressAliasMentionsReachCanonicalMailbox(t *testing.T) {
	h := newAliasHarness(t, identity.Observe)
	h.migrate(t)
	ctx := context.Background()

	code, raw := h.do(t, http.MethodPost, "/messages/post?as="+url.QueryEscape(archieURN), "", `{"kind":"message","to":["@chrispian"],"body":"typed hello"}`)
	var post MessagePostResponse
	if code != http.StatusCreated || json.Unmarshal(raw, &post) != nil || len(post.IDs) != 1 {
		t.Fatalf("typed post: %d %s", code, raw)
	}
	typed, err := h.db.MessagingStore().Get(ctx, post.IDs[0])
	if err != nil || typed.To.URN() != aliasCanonicalURN {
		t.Fatalf("typed message stored under %s (%v)", typed.To.URN(), err)
	}
	// The operator still answers to @chrispian when it sends, and so does the
	// canonical address.
	code, raw = h.do(t, http.MethodPost, "/messages/post", "operator", `{"kind":"message","to":["@archie"],"body":"from me"}`)
	if code != http.StatusCreated || json.Unmarshal(raw, &post) != nil || post.Envelope.From != "@chrispian" {
		t.Fatalf("typed post from the operator: %d %s", code, raw)
	}
	if sent, err := h.db.MessagingStore().Get(ctx, post.IDs[0]); err != nil || sent.From.URN() != identity.OperatorID {
		t.Fatalf("typed message from %s (%v)", sent.From.URN(), err)
	}

	// A group post: @chrispian resolves by display name to the registry agent
	// row, an alias, and the notice lands in the canonical mailbox.
	author, err := h.reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "author", LastUpdatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	group, err := h.reg.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "room", LastUpdatedBy: author.URN})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.SendToGroup(ctx, group.URN, author.URN, "notice", "", "", json.RawMessage(`{"body":"@chrispian please look"}`)); err != nil {
		t.Fatal(err)
	}
	page, err := h.db.MessagingStore().List(ctx, mustURN(t, aliasCanonicalURN), store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var mention *store.Message
	for i, m := range page.Messages {
		if m.ID != typed.ID {
			mention = &page.Messages[i]
		}
	}
	if len(page.Messages) != 2 || mention == nil || mention.To.URN() != aliasCanonicalURN || mention.Metadata[store.MetaAliasTo] != h.agentURN {
		t.Fatalf("canonical mailbox after the group post = %+v", page.Messages)
	}
}

// Acceptance item 6: the registry answers for every address of the mailbox.
func TestAddressAliasRegistryLookup(t *testing.T) {
	h := newAliasHarness(t, identity.Observe)
	h.migrate(t)
	for _, urn := range []string{aliasCanonicalURN, identity.OperatorID, h.agentURN} {
		code, raw := h.do(t, http.MethodGet, "/registry/agents/"+url.PathEscape(urn), "", "")
		var out struct {
			URN          string   `json:"urn"`
			DisplayName  string   `json:"display_name"`
			CanonicalURN string   `json:"canonical_urn"`
			Aliases      []string `json:"aliases"`
		}
		if code != http.StatusOK || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("lookup %s: %d %s", urn, code, raw)
		}
		if out.DisplayName != "chrispian" || out.URN != h.agentURN || out.CanonicalURN != aliasCanonicalURN || len(out.Aliases) != 2 {
			t.Fatalf("lookup %s = %+v", urn, out)
		}
	}
	// An address with no alias rows answers exactly as before.
	code, raw := h.do(t, http.MethodGet, "/registry/agents/"+url.PathEscape("msg://agent/agent-mux/agt_nobody"), "", "")
	if code != http.StatusNotFound {
		t.Fatalf("unknown: %d %s", code, raw)
	}
	code, raw = h.do(t, http.MethodGet, "/whoami?as="+url.QueryEscape(identity.OperatorID), "", "")
	var who whoamiResponse
	if code != http.StatusOK || json.Unmarshal(raw, &who) != nil || who.CanonicalURN != aliasCanonicalURN || len(who.Aliases) != 2 {
		t.Fatalf("whoami: %d %s", code, raw)
	}
}

// A thread read as any form returns the turns stored under every form.
func TestAddressAliasThread(t *testing.T) {
	h := newAliasHarness(t, identity.Observe)
	before := h.send(t, "session", "", identity.OperatorID)
	h.migrate(t)
	body, _ := json.Marshal(map[string]any{"kind": "notice", "to": identity.OperatorID, "thread_id": "t1", "payload": map[string]string{"body": "a"}})
	if code, raw := h.do(t, http.MethodPost, "/messages", "session", string(body)); code != http.StatusCreated {
		t.Fatalf("threaded send: %d %s", code, raw)
	}
	if _, err := h.db.DB().Exec(`UPDATE messages SET thread_id='t1' WHERE id=?`, before.ID); err != nil {
		t.Fatal(err)
	}
	for _, as := range []string{aliasCanonicalURN, identity.OperatorID, h.agentURN} {
		code, raw := h.do(t, http.MethodGet, "/messages/thread/t1?as="+url.QueryEscape(as), "operator", "")
		var out struct {
			Messages []gomsg.Envelope `json:"messages"`
		}
		if code != http.StatusOK || json.Unmarshal(raw, &out) != nil || len(out.Messages) != 2 {
			t.Fatalf("thread as %s: %d %s", as, code, raw)
		}
	}
	code, raw := h.do(t, http.MethodGet, "/messages/thread/t1?as="+url.QueryEscape(archieURN), "operator", "")
	var out struct {
		Messages []gomsg.Envelope `json:"messages"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &out) != nil || len(out.Messages) != 0 {
		t.Fatalf("thread as a stranger: %d %s", code, raw)
	}
}

// A seat's logical name (msg://agent/agent-mux/<catalog id>) on either side of
// an alias means its catalog urn, the address its mail is stored under. Without
// that, mail to the alias was stored under the logical name, where nobody reads.
func TestAddressAliasSeatLogicalNames(t *testing.T) {
	h := newAliasHarness(t, identity.Observe)
	const (
		archieLogical = "msg://agent/agent-mux/archie"
		oldUser       = "msg://user/local/olduser"
		newHome       = "msg://agent/agent-mux/agt_newhome"
	)
	add := func(alias, canonical string) AddressAliasResponse {
		t.Helper()
		body, _ := json.Marshal(AddressAliasRequest{Alias: alias, Canonical: canonical})
		code, raw := h.do(t, http.MethodPost, "/messages/aliases", "operator", string(body))
		var out AddressAliasResponse
		if code != http.StatusOK || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("alias %s -> %s: %d %s", alias, canonical, code, raw)
		}
		return out
	}

	// The canonical address given as a logical name is stored as the catalog urn.
	if row := add(oldUser, archieLogical); row.Canonical != archieURN {
		t.Fatalf("canonical stored as %s, want %s", row.Canonical, archieURN)
	}
	sent := h.send(t, "session", "", oldUser)
	if sent.To.URN() != archieURN {
		t.Fatalf("mail to the alias stored under %s, want %s", sent.To.URN(), archieURN)
	}
	for _, as := range []string{archieURN, archieLogical, oldUser} {
		if got := h.listIDs(t, "operator", as); !equalIDs(got, []string{sent.ID}) {
			t.Fatalf("list as %s = %v, want [%s]", as, got, sent.ID)
		}
	}
	// An alias row that still names the logical form (made before the seat had
	// a catalog urn, or written to the table directly) resolves to the seat too.
	if _, _, err := h.db.AddAddressAlias(context.Background(), "msg://user/local/legacy", archieLogical, ""); err != nil {
		t.Fatal(err)
	}
	legacy := h.send(t, "session", "", "msg://user/local/legacy")
	if legacy.To.URN() != archieURN {
		t.Fatalf("mail through a logical-name alias row stored under %s, want %s", legacy.To.URN(), archieURN)
	}
	if err := h.db.RemoveAddressAlias(context.Background(), "msg://user/local/legacy"); err != nil {
		t.Fatal(err)
	}
	if code, raw := h.do(t, http.MethodDelete, "/messages/aliases?alias="+url.QueryEscape(oldUser), "operator", ""); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, raw)
	}

	// The alias given as a logical name is stored as the catalog urn, so a send
	// to either form of the seat goes to the canonical address.
	if row := add(archieLogical, newHome); row.Alias != archieURN {
		t.Fatalf("alias stored as %s, want %s", row.Alias, archieURN)
	}
	viaLogical := h.send(t, "session", "", archieLogical)
	viaURN := h.send(t, "session", "", archieURN)
	for _, env := range []gomsg.Envelope{viaLogical, viaURN} {
		if env.To.URN() != newHome {
			t.Fatalf("mail to the aliased seat stored under %s, want %s", env.To.URN(), newHome)
		}
	}
	// Newest first, then the seat's earlier mail, merged.
	want := []string{viaURN.ID, viaLogical.ID, legacy.ID, sent.ID}
	for _, as := range []string{newHome, archieURN, archieLogical} {
		if got := h.listIDs(t, "operator", as); !equalIDs(got, want) {
			t.Fatalf("list as %s = %v, want %v", as, got, want)
		}
	}
	// Removing by the logical name removes the row.
	if code, raw := h.do(t, http.MethodDelete, "/messages/aliases?alias="+url.QueryEscape(archieLogical), "operator", ""); code != http.StatusOK {
		t.Fatalf("remove by logical name: %d %s", code, raw)
	}
}

// @archie's ruling on #554: no agent row ever counts as Chrispian. With the
// team:chrispian tmux agent row aliased into his mailbox, a session that holds
// that agent address cannot send as his operator or user forms, cannot read or
// settle his mailbox through any form (its own included), does not render as
// @chrispian and cannot set his status. He still reads mail stored under the
// agent form.
func TestAddressAliasAgentRowNeverActsAsChrispian(t *testing.T) {
	const agentRow = "msg://agent/agent-mux/agt_rbcnlevo4w"
	h := newAliasHarness(t, identity.Enforce)
	h.alias(t, identity.OperatorID)
	h.alias(t, agentRow)
	h.principals["tmux"] = identity.Principal{ID: "msg://session/local/tmux", Kind: "session", Verified: true, Addresses: []string{agentRow}}

	before := h.send(t, "session", "", agentRow) // stored under the canonical address
	for _, from := range []string{identity.OperatorID, aliasCanonicalURN} {
		body, _ := json.Marshal(map[string]any{"kind": "notice", "from": from, "to": archieURN, "payload": map[string]string{"body": "x"}})
		if code, raw := h.do(t, http.MethodPost, "/messages", "tmux", string(body)); code != http.StatusForbidden {
			t.Fatalf("agent row sent as %s: %d %s", from, code, raw)
		}
	}
	for _, as := range []string{agentRow, identity.OperatorID, aliasCanonicalURN} {
		q := url.Values{"to": {as}, "as": {as}}
		if code, raw := h.do(t, http.MethodGet, "/messages/list?"+q.Encode(), "tmux", ""); code != http.StatusForbidden {
			t.Fatalf("agent row listed the mailbox as %s: %d %s", as, code, raw)
		}
		if code, raw := h.do(t, http.MethodGet, "/messages/"+before.ID+"?as="+url.QueryEscape(as), "tmux", ""); code != http.StatusForbidden {
			t.Fatalf("agent row read a message as %s: %d %s", as, code, raw)
		}
		if code, raw := h.do(t, http.MethodPost, "/messages/"+before.ID+"/read?as="+url.QueryEscape(as), "tmux", ""); code != http.StatusForbidden {
			t.Fatalf("agent row settled a message as %s: %d %s", as, code, raw)
		}
	}
	// Mail it sends under its own address is the agent's, not the operator's.
	own := h.send(t, "tmux", agentRow, archieURN)
	if own.From.URN() != agentRow || own.Metadata[MetaFromVerification] != "verified" {
		t.Fatalf("own send: from %s metadata %v", own.From.URN(), own.Metadata)
	}
	srv := &Server{Aliases: h.db}
	if handle := srv.handleFor(context.Background(), agentRow); handle == "@chrispian" {
		t.Fatal("the agent row renders as @chrispian")
	}
	if !srv.actsFor(context.Background(), identity.OperatorID, agentRow) || srv.actsFor(context.Background(), agentRow, aliasCanonicalURN) {
		t.Fatal("actsFor must be one way: his forms act for the agent alias, never the reverse")
	}
	status, _ := json.Marshal(StatusSetRequest{Target: "@chrispian", State: "blocked", Reason: "x"})
	if code, raw := h.do(t, http.MethodPost, "/status?as="+url.QueryEscape(agentRow), "tmux", string(status)); code != http.StatusForbidden {
		t.Fatalf("agent row set @chrispian's status: %d %s", code, raw)
	}
	// The operator still reads the mailbox through every form, and the mail stored under the agent form.
	if got := h.listIDs(t, "operator", aliasCanonicalURN); len(got) != 1 || got[0] != before.ID {
		t.Fatalf("operator list = %v", got)
	}
	if code, raw := h.do(t, http.MethodGet, "/messages/"+before.ID+"?as="+url.QueryEscape(identity.OperatorID), "operator", ""); code != http.StatusOK {
		t.Fatalf("operator read: %d %s", code, raw)
	}
}

// @archie's ruling on #554: Chrispian's own console (Tachyon's device, which he
// paired, holding read) reads his mailbox (list, item, thread), but never pulls
// his inbox (the destructive read), sends, marks read or archives as him. Agent principals, the agt_rbcnlevo4w row,
// a worker-only device, a device he did not pair and an issued token stay refused.
func TestAddressAliasOperatorPairedDeviceReadsOnly(t *testing.T) {
	const agentRow = "msg://agent/agent-mux/agt_rbcnlevo4w"
	h := newAliasHarness(t, identity.Enforce)
	h.alias(t, identity.OperatorID)
	h.alias(t, agentRow)
	device := func(id, by string, scopes ...string) identity.Principal {
		return identity.Principal{Verified: true, ID: "msg://device/" + id, Kind: "device", CreatedBy: by, Scopes: scopes}
	}
	h.principals["tachyon"] = device("tachyon", identity.OperatorID, "maintain", "operate", "read")
	h.principals["worker"] = device("worker", identity.OperatorID, "worker")
	h.principals["foreign"] = device("foreign", "msg://user/local/someone", "read")
	issued := device("tachyon", identity.OperatorID, "read")
	issued.TokenID = "jti-1"
	h.principals["issued"] = issued
	h.principals["tmux"] = identity.Principal{ID: "msg://session/local/tmux", Kind: "session", Verified: true, Addresses: []string{agentRow}}

	env := h.send(t, "session", "", identity.OperatorID)
	thread := env.ThreadID
	if thread == "" {
		thread = env.ID
	}
	reads := func(as string) []string {
		q := url.Values{"to": {as}, "as": {as}}.Encode()
		return []string{"/messages/list?" + q, "/messages/" + env.ID + "?as=" + url.QueryEscape(as), "/messages/thread/" + thread + "?as=" + url.QueryEscape(as)}
	}
	inbox := func(as string) string { return "/messages/inbox?" + url.Values{"to": {as}, "as": {as}}.Encode() }
	for _, as := range []string{identity.OperatorID, aliasCanonicalURN} {
		for _, path := range reads(as) {
			if code, raw := h.do(t, http.MethodGet, path, "tachyon", ""); code != http.StatusOK {
				t.Fatalf("operator-paired device GET %s: %d %s", path, code, raw)
			}
			for _, who := range []string{"tmux", "agentsession", "worker", "foreign", "issued"} {
				if code, raw := h.do(t, http.MethodGet, path, who, ""); code != http.StatusForbidden {
					t.Fatalf("%s GET %s: %d %s", who, path, code, raw)
				}
			}
		}
		// Not the inbox pull (it sets delivered_at and settles read mail), nor any GET outside the allowlist.
		q := url.Values{"to": {as}, "as": {as}}.Encode()
		for _, path := range []string{inbox(as), "/messages/subscribe?" + q, "/messages?" + q} {
			if code, raw := h.do(t, http.MethodGet, path, "tachyon", ""); code != http.StatusForbidden {
				t.Fatalf("operator-paired device GET %s: %d %s", path, code, raw)
			}
		}
		// Reads only: no read mark or archive as him.
		for _, op := range []string{"read", "archive"} {
			if code, raw := h.do(t, http.MethodPost, "/messages/"+env.ID+"/"+op+"?as="+url.QueryEscape(as), "tachyon", ""); code != http.StatusForbidden {
				t.Fatalf("operator-paired device %s as %s: %d %s", op, as, code, raw)
			}
		}
		// And no send as him.
		body, _ := json.Marshal(map[string]any{"kind": "notice", "from": as, "to": archieURN, "payload": map[string]string{"body": "x"}})
		if code, raw := h.do(t, http.MethodPost, "/messages", "tachyon", string(body)); code != http.StatusForbidden {
			t.Fatalf("operator-paired device sent as %s: %d %s", as, code, raw)
		}
	}
}

// The same boundary in observe mode (production's identity mode), where a claim
// that does not verify is recorded instead of refused: the mailbox refusal does
// not depend on the mode.
func TestAddressAliasOperatorPairedDeviceReadsOnlyObserve(t *testing.T) {
	const agentRow = "msg://agent/agent-mux/agt_rbcnlevo4w"
	h := newAliasHarness(t, identity.Observe)
	h.alias(t, identity.OperatorID)
	h.alias(t, agentRow)
	h.principals["tachyon"] = identity.Principal{Verified: true, ID: "msg://device/tachyon", Kind: "device", CreatedBy: identity.OperatorID, Scopes: []string{"read"}}
	h.principals["tmux"] = identity.Principal{ID: "msg://session/local/tmux", Kind: "session", Verified: true, Addresses: []string{agentRow}}
	env := h.send(t, "session", "", identity.OperatorID)
	list := "/messages/list?" + url.Values{"to": {identity.OperatorID}, "as": {identity.OperatorID}}.Encode()
	if code, raw := h.do(t, http.MethodGet, list, "tachyon", ""); code != http.StatusOK {
		t.Fatalf("device list: %d %s", code, raw)
	}
	for _, who := range []string{"tmux", "agentsession"} {
		if code, raw := h.do(t, http.MethodGet, list, who, ""); code != http.StatusForbidden {
			t.Fatalf("%s list: %d %s", who, code, raw)
		}
	}
	if code, raw := h.do(t, http.MethodPost, "/messages/"+env.ID+"/read?as="+url.QueryEscape(identity.OperatorID), "tachyon", ""); code != http.StatusForbidden {
		t.Fatalf("device read mark: %d %s", code, raw)
	}
	inbox := "/messages/inbox?" + url.Values{"to": {identity.OperatorID}, "as": {identity.OperatorID}}.Encode()
	if code, raw := h.do(t, http.MethodGet, inbox, "tachyon", ""); code != http.StatusForbidden {
		t.Fatalf("device inbox pull: %d %s", code, raw)
	}
}
