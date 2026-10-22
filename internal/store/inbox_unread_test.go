package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261021-0029: the pull of a live recipient session is unread-only. A message the
// recipient already read is settled (delivered, delivery closed as consumed), never
// handed back as new; unread messages are delivered exactly once; the plain pull is
// unchanged.

func inboxFixture(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	send := func() string {
		m, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{From: addr("alice"), To: addr("bob"), Kind: messaging.MsgKindNotice})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	read, unread := send(), send()
	// The read message predates the settle-on-read fix: read_at set, delivery still open.
	if _, err := db.DB().Exec(`UPDATE messages SET read_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), read); err != nil {
		t.Fatal(err)
	}
	return db, read, unread
}

func ids(envs []messaging.Envelope) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.ID)
	}
	return out
}

func TestUnreadOnlyInboxSettlesReadMessagesInsteadOfRepresentingThem(t *testing.T) {
	db, read, unread := inboxFixture(t)
	ctx := store.WithUnreadOnlyInbox(context.Background())
	got, err := db.MessagingStore().Inbox(ctx, addr("bob"), messaging.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); len(g) != 1 || g[0] != unread {
		t.Fatalf("pull returned %v, want only the unread message %s", g, unread)
	}
	// The read message was settled: delivered, and its delivery closed as consumed.
	var delivered *string
	if err := db.DB().QueryRow(`SELECT delivered_at FROM messages WHERE id=?`, read).Scan(&delivered); err != nil || delivered == nil {
		t.Fatalf("read message not marked delivered: %v %v", delivered, err)
	}
	did, ok, err := db.DeliveryIDForMessage(context.Background(), read)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	d, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(did))
	if err != nil || d.Status != delivery.DeliveryDelivered {
		t.Fatalf("delivery of the read message: %+v %v", d, err)
	}
	// And it never comes back: a second pull is empty, with or without the flag.
	for _, c := range []context.Context{ctx, context.Background()} {
		again, err := db.MessagingStore().Inbox(c, addr("bob"), messaging.Filter{})
		if err != nil || len(again) != 0 {
			t.Fatalf("second pull returned %v (%v), want nothing", ids(again), err)
		}
	}
}

func TestUnreadOnlyInboxStillDeliversUnreadOnce(t *testing.T) {
	db, _, unread := inboxFixture(t)
	ctx := store.WithUnreadOnlyInbox(context.Background())
	first, err := db.MessagingStore().Inbox(ctx, addr("bob"), messaging.Filter{})
	if err != nil || len(first) != 1 || first[0].ID != unread {
		t.Fatalf("first pull: %v %v", ids(first), err)
	}
	second, err := db.MessagingStore().Inbox(ctx, addr("bob"), messaging.Filter{})
	if err != nil || len(second) != 0 {
		t.Fatalf("second pull: %v %v, want nothing", ids(second), err)
	}
}

func TestPlainInboxPullIsUnchanged(t *testing.T) {
	db, read, unread := inboxFixture(t)
	got, err := db.MessagingStore().Inbox(context.Background(), addr("bob"), messaging.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	g := ids(got)
	if len(g) != 2 || g[0] != read || g[1] != unread {
		t.Fatalf("the plain (operator) pull returned %v, want both the read and the unread message in order", g)
	}
}

// The settle respects the filter: a kind or thread the caller did not ask for is left alone.
func TestUnreadOnlyInboxSettlesOnlyWhatTheFilterCovers(t *testing.T) {
	db, read, _ := inboxFixture(t)
	ctx := store.WithUnreadOnlyInbox(context.Background())
	if _, err := db.MessagingStore().Inbox(ctx, addr("bob"), messaging.Filter{Kind: []messaging.Kind{messaging.MsgKindRequest}}); err != nil {
		t.Fatal(err)
	}
	var delivered *string
	if err := db.DB().QueryRow(`SELECT delivered_at FROM messages WHERE id=?`, read).Scan(&delivered); err != nil || delivered != nil {
		t.Fatalf("a read notice was settled by a request-only pull: %v %v", delivered, err)
	}
}

// Another recipient's read messages are never touched.
func TestUnreadOnlyInboxIgnoresOtherMailboxes(t *testing.T) {
	db, read, _ := inboxFixture(t)
	ctx := store.WithUnreadOnlyInbox(context.Background())
	if _, err := db.MessagingStore().Inbox(ctx, addr("carol"), messaging.Filter{}); err != nil {
		t.Fatal(err)
	}
	var delivered *string
	if err := db.DB().QueryRow(`SELECT delivered_at FROM messages WHERE id=?`, read).Scan(&delivered); err != nil || delivered != nil {
		t.Fatalf("carol's pull settled bob's message: %v %v", delivered, err)
	}
}
