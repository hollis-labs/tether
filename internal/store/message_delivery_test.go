package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

func TestDeliveryHandoffFirstWinsAndReadImpliesDelivery(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	ms := st.MessagingStore()
	for _, path := range []string{"watch_stream", "list", "get", "inbox", "wake_hub", "wake_outpost", "read_implied"} {
		t.Run(path, func(t *testing.T) {
			env, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
			if err != nil {
				t.Fatal(err)
			}
			marker := ms.(DeliveryMarker)
			if err := marker.MarkDelivered(ctx, env.ID, from, "get", "sender"); !errors.Is(err, ErrWrongRecipient) {
				t.Fatalf("sender handoff: %v", err)
			}
			switch path {
			case "read_implied":
				if err := ms.MarkRead(ctx, env.ID, to); err != nil {
					t.Fatal(err)
				}
			case "inbox":
				if _, err := ms.Inbox(ctx, to, messaging.Filter{}); err != nil {
					t.Fatal(err)
				}
			default:
				var wg sync.WaitGroup
				for range 6 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if err := marker.MarkDelivered(ctx, env.ID, to, path, "s1"); err != nil {
							t.Error(err)
						}
					}()
				}
				wg.Wait()
			}
			if err := ms.MarkRead(ctx, env.ID, to); err != nil {
				t.Fatal(err)
			}
			if err := marker.MarkDelivered(ctx, env.ID, to, "get", "s2"); err != nil {
				t.Fatal(err)
			}
			var delivered, read, gotPath string
			if err := st.db.QueryRow(`SELECT delivered_at, read_at, delivery_path FROM messages WHERE id=?`, env.ID).Scan(&delivered, &read, &gotPath); err != nil {
				t.Fatal(err)
			}
			if parseMsgTime(delivered).After(parseMsgTime(read)) || gotPath != path {
				t.Fatalf("delivery=%s read=%s path=%s", delivered, read, gotPath)
			}
			var count int
			var eventPath string
			if err := st.db.QueryRow(`SELECT count(*), json_extract(payload_json,'$.delivery.path') FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.message_id')=?`, env.ID).Scan(&count, &eventPath); err != nil {
				t.Fatal(err)
			}
			if count != 1 || eventPath != path {
				t.Fatalf("events=%d path=%s", count, eventPath)
			}
		})
	}
}

func TestDeliveryMigrationBackfillsReadWithoutDeliveryRow(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()
	files, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	before := fstest.MapFS{}
	for _, m := range migrations {
		if m.version < 113 {
			before[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,created_at,read_at) VALUES ('old','notice','msg://agent/test/a','msg://agent/test/b','2026-01-01T00:00:00Z','2026-01-01T01:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := migrateFS(db, files); err != nil {
			t.Fatal(err)
		}
	}
	var delivered, read, path string
	if err := db.QueryRow(`SELECT delivered_at,read_at,delivery_path FROM messages WHERE id='old'`).Scan(&delivered, &read, &path); err != nil {
		t.Fatal(err)
	}
	if delivered != read || path != "read_implied" {
		t.Fatalf("%s %s %s", delivered, read, path)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM message_event_journal WHERE kind='message.delivered'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

// Queue ordering can differ from the order in which callers capture timestamps.
// A committed handoff must never be moved (or re-journaled) by a stale read.
func TestConcurrentMarkReadAndDelivery(t *testing.T) {
	for n := range 20 {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			st, err := Open(filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
			from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
			ms := st.MessagingStore()
			for range 10 {
				env, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
				if err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				results := make(chan error, 2)
				go func() { <-start; results <- ms.MarkRead(ctx, env.ID, to) }()
				go func() { <-start; results <- ms.(DeliveryMarker).MarkDelivered(ctx, env.ID, to, "watch_stream", "s") }()
				close(start)
				for range 2 {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				assertDeliveryReadEvent(t, st, env.ID)
			}
		})
	}
}

func assertDeliveryReadEvent(t *testing.T, st *Store, id string) {
	t.Helper()
	var delivered, read, eventAt, path, eventPath string
	var count int
	if err := st.db.QueryRow(`SELECT delivered_at,read_at,delivery_path FROM messages WHERE id=?`, id).Scan(&delivered, &read, &path); err != nil {
		t.Fatal(err)
	}
	d, err := time.Parse(time.RFC3339Nano, delivered)
	if err != nil {
		t.Fatal(err)
	}
	r, err := time.Parse(time.RFC3339Nano, read)
	if err != nil {
		t.Fatal(err)
	}
	if d.After(r) {
		t.Fatalf("delivered %s > read %s", delivered, read)
	}
	if err := st.db.QueryRow(`SELECT count(*),at,json_extract(payload_json,'$.delivery.path') FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.message_id')=?`, id).Scan(&count, &eventAt, &eventPath); err != nil {
		t.Fatal(err)
	}
	if count != 1 || eventAt != delivered || eventPath != path {
		t.Fatalf("events=%d eventAt=%s delivered=%s eventPath=%s path=%s", count, eventAt, delivered, eventPath, path)
	}
	var readEvents int
	var readEventAt string
	if err := st.db.QueryRow(`SELECT count(*),at FROM message_event_journal WHERE kind='message.read' AND json_extract(payload_json,'$.message_id')=?`, id).Scan(&readEvents, &readEventAt); err != nil {
		t.Fatal(err)
	}
	if readEvents != 1 || readEventAt != read {
		t.Fatalf("read events=%d eventAt=%s read=%s", readEvents, readEventAt, read)
	}
}

func TestMarkReadClampsToCommittedDelivery(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	for _, stamp := range []string{"2099-01-01T00:00:00Z", "2099-01-01T00:00:00.1Z", "2099-01-01T00:00:00.123456789Z"} {
		env, err := st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
		if err != nil {
			t.Fatal(err)
		}
		// Deterministically model a delivery committed with a timestamp newer
		// than MarkRead's candidate, without scheduler or clock dependencies.
		if _, err := st.db.Exec(`UPDATE messages SET delivered_at=?, delivery_path='watch_stream', delivery_session_id='s' WHERE id=?`, stamp, env.ID); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := st.MessagingStore().MarkRead(ctx, env.ID, to); err != nil {
				t.Fatal(err)
			}
		}
		assertDeliveryReadEvent(t, st, env.ID)
	}
}
