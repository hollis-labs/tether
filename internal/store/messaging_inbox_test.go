package store_test

// messaging_inbox_test.go — coverage for the non-destructive inbox surface
// added in CW-20260517-0003: repeatable List, idempotent MarkRead/Archive/
// Unarchive, archived exclusion, and the payload subject/body projection.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/store"
)

func openInboxDB(t *testing.T) store.InboxStore {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db.MessagingStore()
}

func inboxAddr(id string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: "inbox-test", ID: id}
}

// sendTo persists a notice envelope to `to` with the given payload.
func sendTo(t *testing.T, ms store.InboxStore, to messaging.Address, payload string) messaging.Envelope {
	t.Helper()
	env := messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: inboxAddr("sender"),
		To:   to,
	}
	if payload != "" {
		env.Payload = []byte(payload)
	}
	sent, err := ms.Send(context.Background(), env)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return sent
}

// ─── List is non-destructive and repeatable ─────────────────────────────────

func TestList_RepeatableNonDestructive(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()
	to := inboxAddr("alice")

	for i := 0; i < 3; i++ {
		sendTo(t, ms, to, "")
	}

	// List three times — the result set must be identical and stable.
	for call := 0; call < 3; call++ {
		got, err := ms.List(ctx, to, store.ListFilter{})
		if err != nil {
			t.Fatalf("list call %d: %v", call, err)
		}
		if len(got) != 3 {
			t.Fatalf("list call %d: got %d messages, want 3", call, len(got))
		}
		for _, m := range got {
			if m.DeliveredAt != nil {
				t.Errorf("list call %d: message %s has delivered_at set — List must not consume", call, m.ID)
			}
		}
	}

	// Inbox (the destructive pull) must still see all three — List did not
	// stamp delivered_at out from under it.
	pulled, err := ms.Inbox(ctx, to, messaging.Filter{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("inbox after List: got %d, want 3 (List must not consume)", len(pulled))
	}

	// After Inbox consumed them, List still returns them (delivered ≠ gone).
	got, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list after inbox: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("list after inbox: got %d, want 3", len(got))
	}
}

// ─── MarkRead ────────────────────────────────────────────────────────────────

func TestMarkRead_IdempotentAndScoped(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()
	to := inboxAddr("bob")
	sent := sendTo(t, ms, to, "")

	if err := ms.MarkRead(ctx, sent.ID, to); err != nil {
		t.Fatalf("first MarkRead: %v", err)
	}
	// Idempotent: a second call is a no-op success.
	if err := ms.MarkRead(ctx, sent.ID, to); err != nil {
		t.Fatalf("second MarkRead (idempotent): %v", err)
	}

	got, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ReadAt == nil {
		t.Fatalf("expected one message with read_at set, got %+v", got)
	}

	// Unread-only filter now excludes it.
	unread, err := ms.List(ctx, to, store.ListFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("list unread: %v", err)
	}
	if len(unread) != 0 {
		t.Errorf("unread_only list: got %d, want 0 after MarkRead", len(unread))
	}

	// Unknown id → ErrNotFound.
	if err := ms.MarkRead(ctx, "no-such-id", to); !errors.Is(err, messaging.ErrNotFound) {
		t.Errorf("MarkRead(missing) = %v, want ErrNotFound", err)
	}
	// Wrong recipient → ErrWrongRecipient.
	if err := ms.MarkRead(ctx, sent.ID, inboxAddr("eve")); !errors.Is(err, store.ErrWrongRecipient) {
		t.Errorf("MarkRead(wrong recipient) = %v, want ErrWrongRecipient", err)
	}
}

// ─── Archive / Unarchive ─────────────────────────────────────────────────────

func TestArchive_ExcludedFromDefaultListAndRestorable(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()
	to := inboxAddr("carol")
	keep := sendTo(t, ms, to, "")
	drop := sendTo(t, ms, to, "")

	if err := ms.Archive(ctx, drop.ID, to); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// Idempotent.
	if err := ms.Archive(ctx, drop.ID, to); err != nil {
		t.Fatalf("archive (idempotent): %v", err)
	}

	// Default list excludes the archived message.
	got, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != keep.ID {
		t.Fatalf("default list: got %+v, want only %s", got, keep.ID)
	}

	// include_archived surfaces both.
	all, err := ms.List(ctx, to, store.ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("list include_archived: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("include_archived list: got %d, want 2", len(all))
	}

	// Unarchive restores it to the default list.
	if err := ms.Unarchive(ctx, drop.ID, to); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	got, err = ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list after unarchive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list after unarchive: got %d, want 2", len(got))
	}

	// Recipient scoping holds for archive too.
	if err := ms.Archive(ctx, keep.ID, inboxAddr("eve")); !errors.Is(err, store.ErrWrongRecipient) {
		t.Errorf("Archive(wrong recipient) = %v, want ErrWrongRecipient", err)
	}
	if err := ms.Archive(ctx, "no-such-id", to); !errors.Is(err, messaging.ErrNotFound) {
		t.Errorf("Archive(missing) = %v, want ErrNotFound", err)
	}
}

// ─── Payload projection ──────────────────────────────────────────────────────

func TestList_PayloadProjection(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		payload     string
		wantSubject string
		wantBody    string
	}{
		{"structured object", `{"subject":"Deploy done","body":"shipped v2"}`, "Deploy done", "shipped v2"},
		{"title+summary aliases", `{"title":"Alert","summary":"disk low"}`, "Alert", "disk low"},
		{"json string", `"just a plain message"`, "", "just a plain message"},
		{"raw text", `plain text payload`, "", "plain text payload"},
		{"object without known keys", `{"foo":"bar"}`, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			to := inboxAddr("proj-" + tc.name)
			sendTo(t, ms, to, tc.payload)
			got, err := ms.List(ctx, to, store.ListFilter{})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d messages, want 1", len(got))
			}
			if got[0].Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", got[0].Subject, tc.wantSubject)
			}
			if got[0].Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", got[0].Body, tc.wantBody)
			}
		})
	}
}

// ─── Kind + thread filters on List ───────────────────────────────────────────

func TestList_KindAndThreadFilter(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()
	to := inboxAddr("dave")

	mkReq := messaging.Envelope{Kind: messaging.MsgKindRequest, From: inboxAddr("s"), To: to, ThreadID: "T1"}
	mkNotice := messaging.Envelope{Kind: messaging.MsgKindNotice, From: inboxAddr("s"), To: to, ThreadID: "T2"}
	if _, err := ms.Send(ctx, mkReq); err != nil {
		t.Fatalf("send req: %v", err)
	}
	if _, err := ms.Send(ctx, mkNotice); err != nil {
		t.Fatalf("send notice: %v", err)
	}

	byKind, err := ms.List(ctx, to, store.ListFilter{Kind: []messaging.Kind{messaging.MsgKindRequest}})
	if err != nil {
		t.Fatalf("list by kind: %v", err)
	}
	if len(byKind) != 1 || byKind[0].Kind != messaging.MsgKindRequest {
		t.Fatalf("kind filter: got %+v, want one request", byKind)
	}

	byThread, err := ms.List(ctx, to, store.ListFilter{ThreadID: "T2"})
	if err != nil {
		t.Fatalf("list by thread: %v", err)
	}
	if len(byThread) != 1 || byThread[0].ThreadID != "T2" {
		t.Fatalf("thread filter: got %+v, want one T2 message", byThread)
	}
}
