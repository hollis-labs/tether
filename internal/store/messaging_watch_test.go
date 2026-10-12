package store_test

import (
	"context"
	"errors"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"

	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func mailboxWatcher(t *testing.T, db *store.Store) store.MailboxWatcher {
	t.Helper()
	w, ok := db.MessagingStore().(store.MailboxWatcher)
	if !ok {
		t.Fatal("messaging store does not implement MailboxWatcher")
	}
	return w
}

func sendToWorker(t *testing.T, db *store.Store, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, sendRetentionMessage(t, db))
	}
	return ids
}

func TestMailboxAfter_PageBoundaryResumesWithoutGapOrDuplicate(t *testing.T) {
	for _, total := range []int{100, 101} {
		db := openRetentionDB(t)
		w := mailboxWatcher(t, db)
		ids := sendToWorker(t, db, total)

		var got []string
		var after, lastHigh int64
		for pages := 0; pages < 3; pages++ {
			win, err := w.MailboxAfter(context.Background(), addr("worker"), after, store.ListFilter{}, 100)
			if err != nil {
				t.Fatalf("total=%d: MailboxAfter: %v", total, err)
			}
			lastHigh = win.HighWater
			for _, e := range win.Entries {
				if e.Seq <= after {
					t.Fatalf("total=%d: seq %d not after cursor %d", total, e.Seq, after)
				}
				got = append(got, e.ID)
				after = e.Seq
			}
			if pages == 0 {
				want := total
				if want > 100 {
					want = 100
				}
				if len(win.Entries) != want {
					t.Fatalf("total=%d: first page = %d, want %d", total, len(win.Entries), want)
				}
			}
			if len(win.Entries) == 0 {
				break
			}
		}
		if len(got) != total {
			t.Fatalf("total=%d: got %d messages", total, len(got))
		}
		for i := range ids {
			if got[i] != ids[i] {
				t.Fatalf("total=%d: message %d = %s, want %s (rowid order)", total, i, got[i], ids[i])
			}
		}
		if lastHigh != after {
			t.Fatalf("total=%d: HighWater = %d, want last seq %d", total, lastHigh, after)
		}
	}
}

func TestMailboxAfter_NeverReturnsStagedMessages(t *testing.T) {
	db := openRetentionDB(t)
	ids := sendToWorker(t, db, 2)
	if _, err := db.DB().Exec(`UPDATE messages SET routing_staged=1 WHERE id=?`, ids[0]); err != nil {
		t.Fatalf("stage: %v", err)
	}
	win, err := mailboxWatcher(t, db).MailboxAfter(context.Background(), addr("worker"), 0, store.ListFilter{}, 100)
	if err != nil {
		t.Fatalf("MailboxAfter: %v", err)
	}
	if len(win.Entries) != 1 || win.Entries[0].ID != ids[1] {
		t.Fatalf("entries = %+v, want only %s", win.Entries, ids[1])
	}
}

func TestMailboxAfter_RejectsChannelAddress(t *testing.T) {
	db := openRetentionDB(t)
	ch := messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "channel", SubID: "ops"}
	_, err := mailboxWatcher(t, db).MailboxAfter(context.Background(), ch, 0, store.ListFilter{}, 100)
	if !errors.Is(err, channels.ErrMailboxOperation) {
		t.Fatalf("err = %v, want channels.ErrMailboxOperation", err)
	}
}
