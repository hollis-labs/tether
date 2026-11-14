package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

const (
	aliasCanonical = "msg://user/local/chrispian"
	aliasOperator  = "msg://user/local/operator"
	aliasAgent     = "msg://agent/agent-mux/agt_rbcnlevo4w"
	aliasSender    = "msg://agent/agent-mux/agt_sender"
	aliasStranger  = "msg://agent/agent-mux/agt_stranger"
)

func openAliasStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "alias.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func aliasAddr(t *testing.T, urn string) messaging.Address {
	t.Helper()
	a, err := messaging.ParseURN(urn)
	if err != nil {
		t.Fatalf("parse %s: %v", urn, err)
	}
	return a
}

func aliasSend(t *testing.T, s *Store, from, to, body string) messaging.Envelope {
	t.Helper()
	env, err := s.MessagingStore().Send(context.Background(), messaging.Envelope{
		Kind: messaging.MsgKindNotice, From: aliasAddr(t, from), To: aliasAddr(t, to), Payload: []byte(`{"body":"` + body + `"}`),
	})
	if err != nil {
		t.Fatalf("send to %s: %v", to, err)
	}
	return env
}

func aliasListIDs(t *testing.T, s *Store, to string, f ListFilter) []string {
	t.Helper()
	page, err := s.MessagingStore().List(context.Background(), aliasAddr(t, to), f)
	if err != nil {
		t.Fatalf("list %s: %v", to, err)
	}
	ids := make([]string, 0, len(page.Messages))
	for _, m := range page.Messages {
		ids = append(ids, m.ID)
	}
	if page.Total != len(ids) {
		t.Fatalf("list %s: total %d but %d messages", to, page.Total, len(ids))
	}
	return ids
}

func sameIDs(a, b []string) bool {
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

func TestAddressAliasRules(t *testing.T) {
	s := openAliasStore(t)
	ctx := context.Background()
	if _, _, err := s.AddAddressAlias(ctx, aliasOperator, aliasCanonical, "tester"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Repeating the same alias is not an error and adds no row.
	if _, _, err := s.AddAddressAlias(ctx, aliasOperator, aliasCanonical, "tester"); err != nil {
		t.Fatalf("idempotent add: %v", err)
	}
	if rows, err := s.ListAddressAliases(ctx, ""); err != nil || len(rows) != 1 || rows[0].Alias != aliasOperator || rows[0].Canonical != aliasCanonical || rows[0].CreatedBy != "tester" {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	refused := map[string][2]string{
		"alias of itself":           {aliasCanonical, aliasCanonical},
		"alias already points away": {aliasOperator, aliasStranger},
		"canonical is an alias":     {aliasAgent, aliasOperator},
		"alias has aliases":         {aliasCanonical, aliasStranger},
		"session alias":             {"msg://session/local/abc", aliasCanonical},
		"group alias":               {"msg://group/agent-mux/grp_x", aliasCanonical},
		"channel canonical":         {aliasAgent, "msg://service/local/channel/general"},
		"not a urn":                 {"chrispian", aliasCanonical},
		"sub-id":                    {"msg://agent/agent-mux/a/b", aliasCanonical},
	}
	for name, pair := range refused {
		if _, _, err := s.AddAddressAlias(ctx, pair[0], pair[1], ""); !errors.Is(err, ErrAddressAliasInvalid) {
			t.Errorf("%s: err = %v, want ErrAddressAliasInvalid", name, err)
		}
	}
	if rows, _ := s.ListAddressAliases(ctx, ""); len(rows) != 1 {
		t.Fatalf("a refused add left rows: %+v", rows)
	}
	if forms, err := s.AddressForms(ctx, aliasOperator); err != nil || len(forms) != 2 || forms[0] != aliasCanonical || forms[1] != aliasOperator {
		t.Fatalf("forms = %v, %v", forms, err)
	}
	if c, err := s.CanonicalAddress(ctx, aliasStranger); err != nil || c != aliasStranger {
		t.Fatalf("unaliased canonical = %q, %v", c, err)
	}
	if err := s.RemoveAddressAlias(ctx, aliasOperator); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := s.RemoveAddressAlias(ctx, aliasOperator); !errors.Is(err, ErrAddressAliasNotFound) {
		t.Fatalf("second remove = %v, want ErrAddressAliasNotFound", err)
	}
	if c, _ := s.CanonicalAddress(ctx, aliasOperator); c != aliasOperator {
		t.Fatalf("after remove canonical = %q", c)
	}
}

// Mail stored under an address before it became an alias is listed in the
// canonical mailbox with its read and archive state, and no row is rewritten.
func TestAddressAliasMergesExistingMailWithoutRewriting(t *testing.T) {
	s := openAliasStore(t)
	ctx := context.Background()
	ms := s.MessagingStore()
	unread := aliasSend(t, s, aliasSender, aliasOperator, "unread")
	read := aliasSend(t, s, aliasSender, aliasOperator, "read")
	archived := aliasSend(t, s, aliasSender, aliasOperator, "archived")
	agentMail := aliasSend(t, s, aliasSender, aliasAgent, "to the agent row")
	if err := ms.MarkRead(ctx, read.ID, aliasAddr(t, aliasOperator)); err != nil {
		t.Fatal(err)
	}
	if err := ms.Archive(ctx, archived.ID, aliasAddr(t, aliasOperator)); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string][3]string {
		out := map[string][3]string{}
		rows, err := s.db.Query(`SELECT id, to_urn, coalesce(read_at,''), coalesce(archived_at,'') FROM messages`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, to, r, a string
			if err := rows.Scan(&id, &to, &r, &a); err != nil {
				t.Fatal(err)
			}
			out[id] = [3]string{to, r, a}
		}
		return out
	}
	before := snapshot()

	if got := aliasListIDs(t, s, aliasCanonical, ListFilter{IncludeArchived: true}); len(got) != 0 {
		t.Fatalf("canonical mailbox before aliasing = %v", got)
	}
	_, merged, err := s.AddAddressAlias(ctx, aliasOperator, aliasCanonical, "")
	if err != nil || merged != 3 {
		t.Fatalf("add operator alias: merged %d, %v", merged, err)
	}
	if _, merged, err = s.AddAddressAlias(ctx, aliasAgent, aliasCanonical, ""); err != nil || merged != 1 {
		t.Fatalf("add agent alias: merged %d, %v", merged, err)
	}

	all := aliasListIDs(t, s, aliasCanonical, ListFilter{IncludeArchived: true})
	if len(all) != 4 {
		t.Fatalf("canonical mailbox = %v, want all 4", all)
	}
	for _, addr := range []string{aliasOperator, aliasAgent} {
		if got := aliasListIDs(t, s, addr, ListFilter{IncludeArchived: true}); !sameIDs(got, all) {
			t.Fatalf("list via %s = %v, want %v", addr, got, all)
		}
	}
	if got := aliasListIDs(t, s, aliasCanonical, ListFilter{}); len(got) != 3 {
		t.Fatalf("active = %v, want 3 (archived stays archived)", got)
	}
	unreadIDs := aliasListIDs(t, s, aliasCanonical, ListFilter{UnreadOnly: true})
	if len(unreadIDs) != 2 || (unreadIDs[0] != agentMail.ID && unreadIDs[1] != agentMail.ID) || (unreadIDs[0] != unread.ID && unreadIDs[1] != unread.ID) {
		t.Fatalf("unread = %v, want %s and %s (read stays read)", unreadIDs, unread.ID, agentMail.ID)
	}
	after := snapshot()
	for id, was := range before {
		if after[id] != was {
			t.Fatalf("message %s changed by aliasing: %v -> %v", id, was, after[id])
		}
	}
}

func TestAddressAliasSendListAndActions(t *testing.T) {
	s := openAliasStore(t)
	ctx := context.Background()
	ms := s.MessagingStore()
	for _, a := range []string{aliasOperator, aliasAgent} {
		if _, _, err := s.AddAddressAlias(ctx, a, aliasCanonical, ""); err != nil {
			t.Fatal(err)
		}
	}
	viaOperator := aliasSend(t, s, aliasSender, aliasOperator, "one")
	viaAgent := aliasSend(t, s, aliasSender, aliasAgent, "two")
	direct := aliasSend(t, s, aliasSender, aliasCanonical, "three")
	for _, env := range []messaging.Envelope{viaOperator, viaAgent, direct} {
		if env.To.URN() != aliasCanonical {
			t.Fatalf("stored To = %s, want the canonical address", env.To.URN())
		}
	}
	if viaOperator.Metadata[MetaAliasTo] != aliasOperator || viaAgent.Metadata[MetaAliasTo] != aliasAgent || direct.Metadata[MetaAliasTo] != "" {
		t.Fatalf("alias_to metadata: %v / %v / %v", viaOperator.Metadata, viaAgent.Metadata, direct.Metadata)
	}
	want := []string{direct.ID, viaAgent.ID, viaOperator.ID} // newest first
	for _, addr := range []string{aliasCanonical, aliasOperator, aliasAgent} {
		if got := aliasListIDs(t, s, addr, ListFilter{}); !sameIDs(got, want) {
			t.Fatalf("list via %s = %v, want %v", addr, got, want)
		}
	}

	// A sender using an alias is stored as the canonical address.
	fromAlias := aliasSend(t, s, aliasOperator, aliasSender, "from the old address")
	if fromAlias.From.URN() != aliasCanonical || fromAlias.Metadata[MetaAliasFrom] != aliasOperator {
		t.Fatalf("from = %s, metadata %v", fromAlias.From.URN(), fromAlias.Metadata)
	}

	// Every form may act on the mailbox; another address may not.
	if err := ms.MarkRead(ctx, viaOperator.ID, aliasAddr(t, aliasStranger)); !errors.Is(err, ErrWrongRecipient) {
		t.Fatalf("stranger mark read = %v, want ErrWrongRecipient", err)
	}
	if err := ms.MarkRead(ctx, viaOperator.ID, aliasAddr(t, aliasAgent)); err != nil {
		t.Fatalf("mark read via alias: %v", err)
	}
	if err := ms.Archive(ctx, viaAgent.ID, aliasAddr(t, aliasOperator)); err != nil {
		t.Fatalf("archive via alias: %v", err)
	}
	if err := ms.Unarchive(ctx, viaAgent.ID, aliasAddr(t, aliasCanonical)); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if err := ms.Consume(ctx, direct.ID, aliasAddr(t, aliasStranger)); !errors.Is(err, ErrWrongRecipient) {
		t.Fatalf("stranger consume = %v, want ErrWrongRecipient", err)
	}
	if err := ms.Consume(ctx, direct.ID, aliasAddr(t, aliasOperator)); err != nil {
		t.Fatalf("consume via alias: %v", err)
	}
	if err := ms.MarkRead(ctx, "no-such-id", aliasAddr(t, aliasCanonical)); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("missing id = %v, want ErrNotFound", err)
	}
	if got := aliasListIDs(t, s, aliasOperator, ListFilter{UnreadOnly: true}); len(got) != 2 {
		t.Fatalf("unread via alias = %v, want 2", got)
	}

	// The destructive pull of any form drains the one mailbox.
	pulled, err := ms.Inbox(ctx, aliasAddr(t, aliasAgent), messaging.Filter{})
	if err != nil || len(pulled) != 3 {
		t.Fatalf("inbox via alias = %d, %v", len(pulled), err)
	}
	if again, _ := ms.Inbox(ctx, aliasAddr(t, aliasCanonical), messaging.Filter{}); len(again) != 0 {
		t.Fatalf("second pull = %d, want 0", len(again))
	}

	// The watch cursor read sees the same rows.
	win, err := ms.(MailboxWatcher).MailboxAfter(ctx, aliasAddr(t, aliasOperator), 0, ListFilter{}, 100)
	if err != nil || len(win.Entries) != 3 {
		t.Fatalf("mailbox after via alias = %d, %v", len(win.Entries), err)
	}
}

func TestAddressAliasSubscribeAndRemove(t *testing.T) {
	s := openAliasStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ms := s.MessagingStore()
	old := aliasSend(t, s, aliasSender, aliasOperator, "before")
	if _, _, err := s.AddAddressAlias(ctx, aliasOperator, aliasCanonical, ""); err != nil {
		t.Fatal(err)
	}
	ch, err := ms.Subscribe(ctx, aliasAddr(t, aliasOperator), messaging.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	during := aliasSend(t, s, aliasSender, aliasOperator, "during")
	select {
	case got := <-ch:
		if got.ID != during.ID {
			t.Fatalf("subscriber got %s, want %s", got.ID, during.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a subscriber on the alias did not receive mail stored under the canonical address")
	}

	if err := s.RemoveAddressAlias(ctx, aliasOperator); err != nil {
		t.Fatal(err)
	}
	// Removal splits the view again and rewrites nothing: mail sent to the alias
	// while it was one stays under the canonical address.
	if got := aliasListIDs(t, s, aliasCanonical, ListFilter{}); !sameIDs(got, []string{during.ID}) {
		t.Fatalf("canonical after remove = %v", got)
	}
	if got := aliasListIDs(t, s, aliasOperator, ListFilter{}); !sameIDs(got, []string{old.ID}) {
		t.Fatalf("alias after remove = %v", got)
	}
	after := aliasSend(t, s, aliasSender, aliasOperator, "after")
	if after.To.URN() != aliasOperator {
		t.Fatalf("send after remove stored under %s", after.To.URN())
	}
}

// A second process changes the table: this Store sees it within the TTL.
func TestAddressAliasSeesOutOfBandChange(t *testing.T) {
	s := openAliasStore(t)
	ctx := context.Background()
	if c, _ := s.CanonicalAddress(ctx, aliasOperator); c != aliasOperator {
		t.Fatalf("canonical = %q", c)
	}
	if _, err := s.db.Exec(`INSERT INTO address_aliases(alias_urn, canonical_urn, created_at) VALUES(?,?,?)`, aliasOperator, aliasCanonical, "2026-11-14T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	s.addrAliases.mu.Lock()
	s.addrAliases.loadedAt = time.Now().Add(-2 * addressAliasTTL)
	s.addrAliases.mu.Unlock()
	if c, _ := s.CanonicalAddress(ctx, aliasOperator); c != aliasCanonical {
		t.Fatalf("canonical after the TTL = %q", c)
	}
}
