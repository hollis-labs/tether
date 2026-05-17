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

// mustPage runs List and fails the test on error, returning the page.
func mustPage(ctx context.Context, t *testing.T, ms store.InboxStore, to messaging.Address, f store.ListFilter) store.ListPage {
	t.Helper()
	page, err := ms.List(ctx, to, f)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return page
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
		page, err := ms.List(ctx, to, store.ListFilter{})
		if err != nil {
			t.Fatalf("list call %d: %v", call, err)
		}
		if len(page.Messages) != 3 {
			t.Fatalf("list call %d: got %d messages, want 3", call, len(page.Messages))
		}
		if page.Total != 3 {
			t.Errorf("list call %d: total = %d, want 3", call, page.Total)
		}
		for _, m := range page.Messages {
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
	page, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list after inbox: %v", err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("list after inbox: got %d, want 3", len(page.Messages))
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

	page, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ReadAt == nil {
		t.Fatalf("expected one message with read_at set, got %+v", page.Messages)
	}

	// Unread-only filter now excludes it.
	unread, err := ms.List(ctx, to, store.ListFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("list unread: %v", err)
	}
	if len(unread.Messages) != 0 {
		t.Errorf("unread_only list: got %d, want 0 after MarkRead", len(unread.Messages))
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
	if len(got.Messages) != 1 || got.Messages[0].ID != keep.ID {
		t.Fatalf("default list: got %+v, want only %s", got.Messages, keep.ID)
	}

	// include_archived surfaces both.
	all, err := ms.List(ctx, to, store.ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("list include_archived: %v", err)
	}
	if len(all.Messages) != 2 {
		t.Fatalf("include_archived list: got %d, want 2", len(all.Messages))
	}

	// Unarchive restores it to the default list.
	if err := ms.Unarchive(ctx, drop.ID, to); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	got, err = ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list after unarchive: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("list after unarchive: got %d, want 2", len(got.Messages))
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
			if len(got.Messages) != 1 {
				t.Fatalf("got %d messages, want 1", len(got.Messages))
			}
			if got.Messages[0].Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", got.Messages[0].Subject, tc.wantSubject)
			}
			if got.Messages[0].Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", got.Messages[0].Body, tc.wantBody)
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
	if len(byKind.Messages) != 1 || byKind.Messages[0].Kind != messaging.MsgKindRequest {
		t.Fatalf("kind filter: got %+v, want one request", byKind.Messages)
	}

	byThread, err := ms.List(ctx, to, store.ListFilter{ThreadID: "T2"})
	if err != nil {
		t.Fatalf("list by thread: %v", err)
	}
	if len(byThread.Messages) != 1 || byThread.Messages[0].ThreadID != "T2" {
		t.Fatalf("thread filter: got %+v, want one T2 message", byThread.Messages)
	}
}

// ─── Pagination: limit cap, offset paging, total count ──────────────────────

func TestList_PaginationAndCap(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()
	to := inboxAddr("paged")

	const n = 150
	for i := 0; i < n; i++ {
		sendTo(t, ms, to, "")
	}

	// Limit hard-caps at 100 even when a larger value is requested.
	capped, err := ms.List(ctx, to, store.ListFilter{Limit: 500})
	if err != nil {
		t.Fatalf("list limit=500: %v", err)
	}
	if len(capped.Messages) != 100 {
		t.Errorf("limit=500: got %d messages, want 100 (hard cap)", len(capped.Messages))
	}
	if capped.Limit != 100 {
		t.Errorf("limit=500: page.Limit = %d, want 100", capped.Limit)
	}
	if capped.Total != n {
		t.Errorf("limit=500: total = %d, want %d", capped.Total, n)
	}

	// Zero limit defaults to 100.
	def, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if def.Limit != 100 || len(def.Messages) != 100 {
		t.Errorf("default limit: page.Limit=%d len=%d, want 100/100", def.Limit, len(def.Messages))
	}

	// Offset paging: page 2 picks up the remaining 50 rows.
	page2, err := ms.List(ctx, to, store.ListFilter{Limit: 100, Offset: 100})
	if err != nil {
		t.Fatalf("list offset=100: %v", err)
	}
	if len(page2.Messages) != 50 {
		t.Errorf("offset=100: got %d messages, want 50", len(page2.Messages))
	}
	if page2.Offset != 100 || page2.Total != n {
		t.Errorf("offset=100: offset=%d total=%d, want 100/%d", page2.Offset, page2.Total, n)
	}

	// Negative offset floors at 0; limit below 1 defaults to 100.
	floored, err := ms.List(ctx, to, store.ListFilter{Limit: -5, Offset: -10})
	if err != nil {
		t.Fatalf("list negative args: %v", err)
	}
	if floored.Offset != 0 || floored.Limit != 100 {
		t.Errorf("negative args: offset=%d limit=%d, want 0/100", floored.Offset, floored.Limit)
	}

	// Offset + limit yield non-overlapping windows that cover every row.
	first := mustPage(ctx, t, ms, to, store.ListFilter{Limit: 75, Offset: 0})
	second := mustPage(ctx, t, ms, to, store.ListFilter{Limit: 75, Offset: 75})
	seen := map[string]bool{}
	for _, m := range append(first.Messages, second.Messages...) {
		if seen[m.ID] {
			t.Errorf("paging overlap: id %s appeared twice", m.ID)
		}
		seen[m.ID] = true
	}
	if len(seen) != n {
		t.Errorf("paging coverage: saw %d unique ids across two pages, want %d", len(seen), n)
	}
}

// ─── canceled_at is surfaced (not filtered) ─────────────────────────────────

func TestList_SurfacesCanceledAt(t *testing.T) {
	ms := openInboxDB(t)
	ctx := context.Background()
	to := inboxAddr("cancel-vis")

	live := sendTo(t, ms, to, "")
	canceled := sendTo(t, ms, to, "")

	if err := ms.Cancel(ctx, canceled.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	page, err := ms.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Canceled messages are NOT filtered out — both still listed.
	if len(page.Messages) != 2 {
		t.Fatalf("list after cancel: got %d, want 2 (canceled not filtered)", len(page.Messages))
	}
	byID := map[string]store.Message{}
	for _, m := range page.Messages {
		byID[m.ID] = m
	}
	if got := byID[canceled.ID]; got.CanceledAt == nil {
		t.Errorf("canceled message %s: CanceledAt = nil, want non-nil", canceled.ID)
	}
	if got := byID[live.ID]; got.CanceledAt != nil {
		t.Errorf("live message %s: CanceledAt = %v, want nil", live.ID, got.CanceledAt)
	}
}
